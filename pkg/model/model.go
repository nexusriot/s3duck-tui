package model

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	s3m "github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	u "github.com/nexusriot/s3duck-tui/pkg/utils"
)

// ErrFileExists is returned (wrapped, with the path) when a download would
// overwrite an existing local file. Callers that want to replace the file —
// the overwrite prompt, sync — must remove it first; errors.Is identifies it.
var ErrFileExists = errors.New("file exists")

type optsFunc = func(*config.LoadOptions) error

type FType int8

const (
	File FType = iota
	Folder
	Bucket
)

type UploadTarget struct {
	LocalPath  string
	RemotePath string
	Size       int64
}

type Config struct {
	Url       string
	Region    *string
	AccessKey string
	SecretKey string
	// SessionToken is the STS session token for temporary credentials
	// (assume-role, SSO, MFA). Empty for long-lived key pairs.
	SessionToken   string
	SSl            bool
	MaxBytesPerSec int64 // 0 = unlimited
}

// rateLimiter is a token-bucket throttle shared across all transfer workers of
// a Model, so uploads and the parallel download pool honor one global cap. A
// nil *rateLimiter is unlimited (the zero-config case), making the call sites
// free when throttling is off.
type rateLimiter struct {
	mu     sync.Mutex
	rate   float64 // bytes/sec; <= 0 means unlimited
	tokens float64
	last   time.Time
}

func newRateLimiter(bytesPerSec int64) *rateLimiter {
	if bytesPerSec <= 0 {
		return nil
	}
	return &rateLimiter{
		rate:   float64(bytesPerSec),
		tokens: float64(bytesPerSec), // start with a 1s burst allowance
		last:   time.Now(),
	}
}

// throttleStep is the pure core of the token bucket: given the rate (bytes/sec),
// current token balance, time elapsed since the last refill, and the number of
// bytes about to move, it returns how long to sleep and the new token balance.
// A non-positive rate is unlimited. Tokens may go negative (debt paid off by
// the next refill), which is what bounds the long-run rate without over-
// crediting the time spent sleeping.
func throttleStep(rate, tokens float64, elapsed time.Duration, n int) (sleep time.Duration, newTokens float64) {
	if rate <= 0 {
		return 0, tokens
	}
	tokens += rate * elapsed.Seconds()
	if tokens > rate { // burst cap of one second's worth
		tokens = rate
	}
	tokens -= float64(n)
	if tokens < 0 {
		sleep = time.Duration((-tokens / rate) * float64(time.Second))
	}
	return sleep, tokens
}

// wait blocks until n bytes may be transferred under the configured rate. Safe
// for concurrent use; a nil limiter returns immediately (unlimited).
func (l *rateLimiter) wait(n int) {
	if l == nil || n <= 0 {
		return
	}
	l.mu.Lock()
	now := time.Now()
	elapsed := now.Sub(l.last)
	l.last = now
	sleep, tokens := throttleStep(l.rate, l.tokens, elapsed, n)
	l.tokens = tokens
	l.mu.Unlock()
	if sleep > 0 {
		time.Sleep(sleep)
	}
}

type DownloadTarget struct {
	Key  string
	Size int64
}

type Object struct {
	Key          *string
	Ot           FType
	Etag         *string
	Size         *int64
	StorageClass *string
	LastModified *time.Time
	FullPath     *string
}

type Model struct {
	Config     *aws.Config
	Client     *s3.Client
	Downloader *s3m.Downloader
	Cf         *Config
	Limiter    *rateLimiter
}

type progressReader struct {
	r       io.Reader
	written int64
	total   int64
	update  func(written int64, total int64)
	limiter *rateLimiter
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	pr.written += int64(n)
	pr.update(pr.written, pr.total)
	pr.limiter.wait(n)
	return n, err
}

type progressWriterAt struct {
	w          io.WriterAt
	written    int64
	total      int64
	updateFunc func(written int64, total int64)
	limiter    *rateLimiter
}

func (pwa *progressWriterAt) WriteAt(p []byte, off int64) (int, error) {
	n, err := pwa.w.WriteAt(p, off)
	w := atomic.AddInt64(&pwa.written, int64(n))
	if pwa.updateFunc != nil {
		pwa.updateFunc(w, pwa.total)
	}
	pwa.limiter.wait(n)
	return n, err
}

// uploadMaxAttempts is the total number of tries (1 initial + 2 retries) the
// uploader makes per part before giving up.
const uploadMaxAttempts = 3

// uploadRetryer is the retry policy for uploads. Uploads previously ran with
// aws.NopRetryer{}, so a single transient network error failed the whole
// transfer — painful for a multi-file upload and worse for a sync run pushing
// hundreds of objects. The SDK's standard retryer backs off on throttling and
// transient 5xx/connection errors; retries are safe here because every part is
// re-read from the file, not from a consumed buffer.
func uploadRetryer() aws.Retryer {
	return retry.NewStandard(func(o *retry.StandardOptions) {
		o.MaxAttempts = uploadMaxAttempts
	})
}

// newUploader builds the shared multipart uploader used by both Upload and
// UploadFile, so the two paths can't drift on part size or retry policy.
func newUploader(client *s3.Client) *s3m.Uploader {
	return s3m.NewUploader(client, func(u *s3m.Uploader) {
		u.PartSize = 5 * 1024 * 1024
		u.LeavePartsOnError = false
		u.ClientOptions = append(u.ClientOptions, func(o *s3.Options) {
			o.Retryer = uploadRetryer()
		})
	})
}

func GetDownloader(client *s3.Client) *s3m.Downloader {
	d := s3m.NewDownloader(client, func(d *s3m.Downloader) {
		d.BufferProvider = s3m.NewPooledBufferedWriterReadFromProvider(5 * 1024 * 1024)
	})
	return d
}

func NewConfig(url string, region *string, accKey string, secKey string, sessionToken string, ssl bool, maxBytesPerSec int64) Config {
	return Config{
		Url:            url,
		Region:         region,
		AccessKey:      accKey,
		SecretKey:      secKey,
		SessionToken:   sessionToken,
		SSl:            ssl,
		MaxBytesPerSec: maxBytesPerSec,
	}
}

// GetConfig  ...
func GetConfig(cf Config, update bool) (aws.Config, error) {
	customResolver := aws.EndpointResolverWithOptionsFunc(func(service, region string, options ...interface{}) (aws.Endpoint, error) {
		var endpoint aws.Endpoint
		if cf.Region != nil {
			endpoint = aws.Endpoint{
				URL:               cf.Url,
				SigningRegion:     *cf.Region,
				HostnameImmutable: true,
			}
		} else {
			endpoint = aws.Endpoint{
				URL:               cf.Url,
				HostnameImmutable: true,
			}
		}
		return endpoint, nil
	})

	staticProvider := credentials.NewStaticCredentialsProvider(cf.AccessKey, cf.SecretKey, cf.SessionToken)

	var opts []optsFunc
	if update && strings.Contains(cf.Url, "amazon") {
		opts = []optsFunc{config.WithRegion(*cf.Region)}
	} else {
		opts = []optsFunc{config.WithEndpointResolverWithOptions(customResolver)}
	}

	// Timeouts are per phase, NOT http.Client.Timeout: that one spans the whole
	// exchange including the body, so a 5 MiB part on a link slower than
	// ~1.4 Mbit/s would be killed mid-transfer (and the bandwidth throttle could
	// trigger it on any link). Hung connections are still bounded — dial, TLS
	// and first-response-byte each get their own deadline — while a healthy
	// transfer may take as long as it takes.
	timeoutClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: !cf.SSl},
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}
	opts = append(opts, config.WithCredentialsProvider(staticProvider), config.WithHTTPClient(timeoutClient))

	cfg, err := config.LoadDefaultConfig(context.TODO(), opts...)
	return cfg, err
}

func NewModel(cf Config) *Model {

	cfg, _ := GetConfig(cf, false)

	client := s3.NewFromConfig(cfg)

	m := Model{
		Config:     &cfg,
		Client:     client,
		Downloader: GetDownloader(client),
		Cf:         &cf,
		Limiter:    newRateLimiter(cf.MaxBytesPerSec),
	}
	return &m
}

// RefreshClient re-pins the client to the bucket's actual region (AWS only —
// GetConfig ignores the region rebuild for custom endpoints). On any failure it
// leaves the existing client and region untouched and reports the error:
// mutating them on a failed lookup used to pin every later call — and even
// CreateBucket — to a wrong us-east-1 default, turning one denied
// s3:GetBucketLocation into a stream of baffling redirect errors.
func (m *Model) RefreshClient(bucket *string) error {
	region, err := m.GetBucketLocation(bucket)
	if err != nil {
		return fmt.Errorf("resolving region of %s: %w", aws.ToString(bucket), err)
	}

	cf := *m.Cf
	cf.Region = region
	cfg, err := GetConfig(cf, true)
	if err != nil {
		return fmt.Errorf("rebuilding client for region %s: %w", aws.ToString(region), err)
	}

	m.Cf.Region = region
	m.Client = s3.NewFromConfig(cfg)
	m.Downloader = GetDownloader(m.Client)
	return nil
}
func (m *Model) ListObjects(key string, bucket *Object) ([]s3t.Object, error) {
	if bucket == nil || bucket.Key == nil {
		return nil, fmt.Errorf("bucket is nil")
	}

	var objects []s3t.Object
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(*bucket.Key),
		Prefix: aws.String(key),
	}

	paginator := s3.NewListObjectsV2Paginator(m.Client, input)
	for paginator.HasMorePages() {
		output, err := paginator.NextPage(context.TODO())
		if err != nil {
			return nil, err
		}
		objects = append(objects, output.Contents...)
	}
	return objects, nil
}

func (m *Model) GetBucketLocation(name *string) (*string, error) {
	bl, err := m.Client.GetBucketLocation(
		context.TODO(),
		&s3.GetBucketLocationInput{
			Bucket: name,
		},
	)
	if err != nil || bl == nil {
		// No plausible default here: guessing us-east-1 on a failed lookup is
		// what used to poison the client region. Callers decide what to do.
		return nil, err
	}
	loc := normalizeBucketLocation(string(bl.LocationConstraint))
	return &loc, nil
}

// normalizeBucketLocation maps GetBucketLocation's constraint values onto real
// region names: an empty constraint is us-east-1, and buckets created before
// 2009 in Ireland report the legacy alias "EU" rather than eu-west-1.
func normalizeBucketLocation(constraint string) string {
	switch constraint {
	case "":
		return "us-east-1"
	case "EU":
		return "eu-west-1"
	default:
		return constraint
	}
}

func (m *Model) List(path string, bucket *Object) ([]*Object, error) {
	if bucket == nil || bucket.Key == nil {
		return nil, fmt.Errorf("bucket is nil")
	}
	objs := make([]*Object, 0)
	input := &s3.ListObjectsV2Input{
		Bucket:    aws.String(*bucket.Key),
		Delimiter: aws.String("/"),
		Prefix:    aws.String(path),
	}

	paginator := s3.NewListObjectsV2Paginator(m.Client, input)

	for paginator.HasMorePages() {
		output, err := paginator.NextPage(context.TODO())
		if err != nil {
			return nil, err
		}

		for _, p := range output.CommonPrefixes {
			if p.Prefix == nil {
				continue
			}
			fields := strings.FieldsFunc(strings.TrimSpace(*p.Prefix), u.SplitFunc)
			var appKey string
			if len(fields) != 0 {
				appKey = fields[len(fields)-1]
			} else {
				appKey = "/"
			}

			ko := &Object{
				&appKey,
				Folder,
				nil,
				nil,
				nil,
				nil,
				p.Prefix,
			}
			objs = append(objs, ko)
		}
		for _, o := range output.Contents {
			if o.Key == nil || *o.Key == path {
				continue
			}
			fields := strings.FieldsFunc(strings.TrimSpace(*o.Key), u.SplitFunc)
			if len(fields) == 0 {
				continue
			}
			appKey := fields[len(fields)-1]
			ts := strings.Trim(aws.ToString(o.ETag), "\"")
			size := o.Size

			var sc *string
			if o.StorageClass != "" {
				s := string(o.StorageClass)
				sc = &s
			}
			ko := &Object{
				&appKey,
				File,
				&ts,
				&size,
				sc,
				o.LastModified,
				o.Key,
			}
			objs = append(objs, ko)
		}
	}
	return objs, nil
}

func (m *Model) ListBuckets() ([]*Object, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	objs := make([]*Object, 0)

	result, err := m.Client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return nil, err
	}

	for _, b := range result.Buckets {
		// S3-compatible endpoints may omit Name/CreationDate; avoid deref panics.
		name := aws.ToString(b.Name)
		ko := &Object{
			Key:          &name,
			Ot:           Bucket,
			Etag:         nil,
			Size:         nil,
			StorageClass: nil,
			LastModified: b.CreationDate,
		}
		objs = append(objs, ko)
	}
	return objs, nil
}

func (m *Model) Delete(key *string, bucket *Object) error {
	if bucket == nil || bucket.Key == nil {
		return fmt.Errorf("bucket is nil")
	}
	if key == nil || *key == "" {
		return fmt.Errorf("key is empty")
	}

	var objectIds []s3t.ObjectIdentifier

	if strings.HasSuffix(*key, "/") {
		ks, err := m.ListObjects(*key, bucket)

		if err != nil {
			return err
		}
		for _, o := range ks {
			if o.Key == nil {
				continue
			}
			objectIds = append(objectIds, s3t.ObjectIdentifier{Key: aws.String(*o.Key)})
		}
	} else {
		objectIds = append(objectIds, s3t.ObjectIdentifier{Key: aws.String(*key)})
	}

	return m.deleteObjectIDs(bucket, objectIds)
}

// deleteObjectIDs removes the given objects in DeleteObjects batches of 1000
// (the S3 API maximum), failing on the first per-key error the service reports.
func (m *Model) deleteObjectIDs(bucket *Object, objectIds []s3t.ObjectIdentifier) error {
	if len(objectIds) == 0 {
		return nil
	}

	const maxDelete = 1000
	ctx := context.TODO()

	for i := 0; i < len(objectIds); i += maxDelete {
		end := i + maxDelete
		if end > len(objectIds) {
			end = len(objectIds)
		}

		out, err := m.Client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(*bucket.Key),
			Delete: &s3t.Delete{
				Objects: objectIds[i:end],
				Quiet:   true,
			},
		})
		if err != nil {
			return err
		}
		if out != nil && len(out.Errors) > 0 {
			e := out.Errors[0]
			return fmt.Errorf("delete failed for %s: %s", aws.ToString(e.Key), aws.ToString(e.Message))
		}
	}

	return nil
}

// EmptyBucket removes every current object in the bucket, so DeleteBucket can
// succeed afterwards — S3 only deletes empty buckets, and the delete confirm
// has already promised the user exactly this removal. On a versioned bucket
// this writes delete markers only: old versions survive, and the subsequent
// DeleteBucket still fails with BucketNotEmpty (see DESIGN.md).
func (m *Model) EmptyBucket(bucket *Object) error {
	if bucket == nil || bucket.Key == nil {
		return fmt.Errorf("bucket is nil")
	}
	objs, err := m.ListObjects("", bucket)
	if err != nil {
		return err
	}
	ids := make([]s3t.ObjectIdentifier, 0, len(objs))
	for _, o := range objs {
		if o.Key != nil {
			ids = append(ids, s3t.ObjectIdentifier{Key: o.Key})
		}
	}
	return m.deleteObjectIDs(bucket, ids)
}

func (m *Model) DeleteBucket(name *string) error {
	// No logging here: stdout/stderr writes corrupt the tview display. The
	// error is returned and surfaced by the controller's delete report.
	_, err := m.Client.DeleteBucket(context.TODO(), &s3.DeleteBucketInput{
		Bucket: aws.String(*name)})
	return err
}

// MultipartUpload is one in-progress (incomplete) multipart upload.
type MultipartUpload struct {
	Key       string
	UploadID  string
	Initiated *time.Time
}

// ListMultipartUploads returns the bucket's in-progress multipart uploads —
// orphaned parts from interrupted uploads that keep costing storage until
// aborted. Returns up to the first page (1000) of uploads, which is ample for
// cleaning up orphans.
func (m *Model) ListMultipartUploads(bucket *Object) ([]MultipartUpload, error) {
	if bucket == nil || bucket.Key == nil {
		return nil, fmt.Errorf("bucket is nil")
	}
	out, err := m.Client.ListMultipartUploads(context.TODO(), &s3.ListMultipartUploadsInput{
		Bucket: aws.String(*bucket.Key),
	})
	if err != nil {
		return nil, err
	}
	var ups []MultipartUpload
	for _, u := range out.Uploads {
		ups = append(ups, MultipartUpload{
			Key:       aws.ToString(u.Key),
			UploadID:  aws.ToString(u.UploadId),
			Initiated: u.Initiated,
		})
	}
	return ups, nil
}

// BucketConfigInfo is a read-only snapshot of a bucket's configuration.
type BucketConfigInfo struct {
	Region     string
	Versioning string // "Enabled" / "Suspended" / "off"
	Encryption string // e.g. "AES256" / "aws:kms" / "none"
	ObjectLock string // "on" / "off"
}

// BucketConfig gathers a bucket's versioning, default encryption, object-lock,
// and region. Optional features that the endpoint doesn't support (common on
// MinIO/Ceph) error out and are reported as their "off"/"none" default rather
// than failing the whole call.
func (m *Model) BucketConfig(bucket *Object) (BucketConfigInfo, error) {
	if bucket == nil || bucket.Key == nil {
		return BucketConfigInfo{}, fmt.Errorf("bucket is nil")
	}
	name := aws.String(*bucket.Key)
	ctx := context.TODO()
	info := BucketConfigInfo{Region: "us-east-1", Versioning: "off", Encryption: "none", ObjectLock: "off"}

	if loc, err := m.GetBucketLocation(bucket.Key); err == nil && loc != nil && *loc != "" {
		info.Region = *loc
	}
	if v, err := m.Client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: name}); err == nil {
		if s := string(v.Status); s != "" {
			info.Versioning = s
		}
	}
	if e, err := m.Client.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: name}); err == nil {
		if e.ServerSideEncryptionConfiguration != nil && len(e.ServerSideEncryptionConfiguration.Rules) > 0 {
			if d := e.ServerSideEncryptionConfiguration.Rules[0].ApplyServerSideEncryptionByDefault; d != nil {
				info.Encryption = string(d.SSEAlgorithm)
			}
		}
	}
	if l, err := m.Client.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: name}); err == nil {
		if l.ObjectLockConfiguration != nil && string(l.ObjectLockConfiguration.ObjectLockEnabled) == "Enabled" {
			info.ObjectLock = "on"
		}
	}
	return info, nil
}

// AbortMultipartUpload discards one incomplete multipart upload and its parts.
func (m *Model) AbortMultipartUpload(bucket *Object, key, uploadID string) error {
	if bucket == nil || bucket.Key == nil {
		return fmt.Errorf("bucket is nil")
	}
	_, err := m.Client.AbortMultipartUpload(context.TODO(), &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(*bucket.Key),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
	return err
}

func (m *Model) CreateBucket(name *string, public bool) error {
	region := aws.ToString(m.Cf.Region)

	input := &s3.CreateBucketInput{
		Bucket: aws.String(*name),
		ACL:    s3t.BucketCannedACLPrivate,
	}

	// us-east-1 does NOT accept a LocationConstraint
	if region != "" && region != "us-east-1" {
		input.CreateBucketConfiguration = &s3t.CreateBucketConfiguration{
			LocationConstraint: s3t.BucketLocationConstraint(region),
		}
	}

	_, err := m.Client.CreateBucket(context.TODO(), input)
	if err != nil {
		return fmt.Errorf("failed to create bucket: %w", err)
	}
	if public {
		err = m.MakeBucketPublic(*name)
		if err != nil {
			return fmt.Errorf("bucket created, but failed to make it public: %v", err)
		}
	}
	return nil
}

func (m *Model) CreateFolder(name *string, bucket *Object) error {
	_, err := m.Client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket: aws.String(*bucket.Key),
		Key:    aws.String(*name),
	})
	return err
}

func (m *Model) Upload(
	ctx context.Context,
	localPath, s3Prefix string,
	bucket *Object,
	progressCb func(current, total int64, i, count int, local, remote string),
) error {
	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}

	isDir := info.IsDir()
	var files []string
	var dirs []string
	var totalSize int64

	if isDir {
		err := filepath.Walk(localPath, func(p string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if fi.IsDir() {
				dirs = append(dirs, p)
				return nil
			}
			// Sockets, FIFOs and devices are skipped: opening a FIFO with no
			// writer blocks forever and the open syscall ignores ctx cancel,
			// so one stray pipe would hang the whole upload.
			if !fi.Mode().IsRegular() {
				return nil
			}
			files = append(files, p)
			totalSize += fi.Size()
			return nil
		})
		if err != nil {
			return err
		}
	} else {
		files = []string{localPath}
		totalSize = info.Size()
	}

	// If uploading a directory, create S3 "folder marker" objects for empty dirs.
	// S3 has no real folders; empty folders exist only if there's a key ending with "/".
	if isDir {
		// Mark all directories that have at least one file somewhere under them.
		nonEmpty := make(map[string]bool, len(dirs))
		for _, f := range files {
			d := filepath.Dir(f)
			for {
				nonEmpty[d] = true
				if d == localPath {
					break
				}
				parent := filepath.Dir(d)
				if parent == d {
					break
				}
				d = parent
			}
		}
		// Create markers for dirs that have no file descendants.
		// Also supports the case when the root folder itself is empty
		for _, d := range dirs {
			if nonEmpty[d] {
				continue
			}

			parent := filepath.Dir(localPath)
			relPath, err := filepath.Rel(parent, d)
			if err != nil {
				return err
			}

			s3Key := filepath.ToSlash(path.Join(s3Prefix, relPath))
			if !strings.HasSuffix(s3Key, "/") {
				s3Key += "/"
			}

			_, err = m.Client.PutObject(ctx, &s3.PutObjectInput{
				Bucket: aws.String(*bucket.Key),
				Key:    aws.String(s3Key),
				Body:   strings.NewReader(""),
			})
			if err != nil {
				return fmt.Errorf("failed to create folder marker %s: %w", s3Key, err)
			}
		}
	}

	uploader := newUploader(m.Client)

	var uploadedTotal int64

	for i, fpath := range files {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		stat, err := os.Stat(fpath)
		if err != nil {
			return fmt.Errorf("stat failed for %s: %w", fpath, err)
		}

		fp, err := os.Open(fpath)
		if err != nil {
			return fmt.Errorf("failed to open file %s: %w", fpath, err)
		}

		var s3Key string
		if isDir {
			parent := filepath.Dir(localPath)
			relPath, _ := filepath.Rel(parent, fpath)
			s3Key = filepath.ToSlash(path.Join(s3Prefix, relPath))
		} else {
			s3Key = path.Join(s3Prefix, filepath.Base(fpath))
		}

		reader := &progressReader{
			r:     fp,
			total: stat.Size(),
			update: func(written, _ int64) {
				if progressCb != nil {
					progressCb(uploadedTotal+written, totalSize, i+1, len(files), fpath, s3Key)
				}
			},
			limiter: m.Limiter,
		}

		uploadCtx, cancel := context.WithCancel(ctx)

		_, err = uploader.Upload(uploadCtx, &s3.PutObjectInput{
			Bucket: aws.String(*bucket.Key),
			Key:    aws.String(s3Key),
			Body:   reader,
		})
		fp.Close()
		cancel() // release per-file context immediately, not at Upload() return

		if err != nil {
			if errors.Is(err, context.Canceled) {
				return fmt.Errorf("upload canceled for %s", fpath)
			}
			var apiErr smithy.APIError
			if errors.As(err, &apiErr) {
				return fmt.Errorf("upload failed for %s: %s - %s", fpath, apiErr.ErrorCode(), apiErr.ErrorMessage())
			}
			return fmt.Errorf("upload failed for %s: %w", fpath, err)
		}

		uploadedTotal += stat.Size()
	}

	return nil
}

// PrepareUpload returns list of files to upload with remote keys and total size.
func (m *Model) PrepareUpload(localPath string, currentPath string, bucket *Object) ([]UploadTarget, int64, error) {
	info, err := os.Stat(localPath)
	if err != nil {
		return nil, 0, err
	}

	var targets []UploadTarget
	var totalSize int64

	prefix := NormalizePrefix(currentPath)

	if !info.IsDir() {
		remote := prefix + filepath.ToSlash(filepath.Base(localPath))
		targets = append(targets, UploadTarget{
			LocalPath:  localPath,
			RemotePath: remote,
			Size:       info.Size(),
		})
		return targets, info.Size(), nil
	}

	// Keys are relative to the PARENT of localPath, so the uploaded tree keeps
	// its top-level directory name — this must match Upload's key derivation
	// exactly, or the preview promises destinations the objects never land on.
	base := filepath.Dir(localPath)
	err = filepath.Walk(localPath, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Mirror Upload's walk: directories contribute nothing here and
		// non-regular files would hang the transfer (see Upload).
		if fi.IsDir() || !fi.Mode().IsRegular() {
			return nil
		}

		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		remote := prefix + filepath.ToSlash(rel)

		targets = append(targets, UploadTarget{
			LocalPath:  p,
			RemotePath: remote,
			Size:       fi.Size(),
		})
		totalSize += fi.Size()
		return nil
	})

	if err != nil {
		return nil, 0, err
	}
	return targets, totalSize, nil
}

func (m *Model) MakeBucketPublic(bucketName string) error {
	policy := fmt.Sprintf(`{
		"Version":"2012-10-17",
		"Statement":[{
			"Sid":"PublicReadGetObject",
			"Effect":"Allow",
			"Principal":"*",
			"Action":"s3:GetObject",
			"Resource":"arn:aws:s3:::%s/*"
		}]
	}`, bucketName)

	_, err := m.Client.PutBucketPolicy(context.TODO(), &s3.PutBucketPolicyInput{
		Bucket: aws.String(bucketName),
		Policy: aws.String(policy),
	})
	return err
}

// ResolveDownloadObjects resolves the objects to be downloaded.
// If `isFolder` is true, it performs a prefix-based list.
// If false, returns a single exact match using the key and size.
func (m *Model) ResolveDownloadObjects(key string, isFolder bool, size *int64, bucket *Object) ([]DownloadTarget, int64, error) {
	if isFolder {
		if !strings.HasSuffix(key, "/") {
			key += "/"
		}
		objs, err := m.ListObjects(key, bucket)
		if err != nil {
			return nil, 0, err
		}

		var out []DownloadTarget
		var total int64
		for _, obj := range objs {
			if obj.Key == nil {
				continue
			}
			out = append(out, DownloadTarget{
				Key:  *obj.Key,
				Size: obj.Size,
			})
			total += obj.Size
		}
		return out, total, nil
	}

	if size == nil {
		return nil, 0, fmt.Errorf("file size is nil for object %s", key)
	}
	return []DownloadTarget{{Key: key, Size: *size}}, *size, nil
}
func (m *Model) DownloadTarget(
	ctx context.Context,
	t DownloadTarget,
	currentPath,
	destPath string,
	bucket *string,
	progressCb func(written int64, total int64, key string),
) (int64, error) {
	// build local path based on key (same logic as your Download)
	if err := os.MkdirAll(destPath, 0700); err != nil {
		return 0, err
	}

	key := filepath.ToSlash(t.Key)
	prefix := NormalizePrefix(currentPath)
	relativeKey := key
	if strings.HasPrefix(key, prefix) {
		relativeKey = strings.TrimPrefix(key, prefix)
	}
	downloadPath := filepath.Join(destPath, relativeKey)

	if strings.HasSuffix(t.Key, "/") {
		return 0, os.MkdirAll(downloadPath, 0760)
	}
	if err := os.MkdirAll(filepath.Dir(downloadPath), 0760); err != nil {
		return 0, err
	}
	if _, err := os.Stat(downloadPath); err == nil {
		return 0, fmt.Errorf("%w: %s", ErrFileExists, downloadPath)
	}

	fp, err := os.Create(downloadPath)
	if err != nil {
		return 0, err
	}
	defer fp.Close()

	writerAt := &progressWriterAt{
		w:     fp,
		total: t.Size,
		updateFunc: func(written int64, total int64) {
			if progressCb != nil {
				progressCb(written, total, t.Key)
			}
		},
		limiter: m.Limiter,
	}

	n, err := m.Downloader.Download(ctx, writerAt, &s3.GetObjectInput{
		Bucket: bucket,
		Key:    aws.String(t.Key),
	})
	if ctx.Err() != nil {
		_ = os.Remove(downloadPath)
		return 0, ctx.Err()
	}
	if err != nil {
		// Drop the partial/empty file so a retry isn't blocked by the
		// "file exists" stat guard above.
		_ = os.Remove(downloadPath)
		return n, err
	}
	return n, err
}

// PresignMaxTTL is the maximum lifetime AWS SigV4 allows for a presigned URL.
const PresignMaxTTL = 7 * 24 * time.Hour

// PresignGetURL returns a time-limited presigned GET URL for an object, so a
// private object can be shared without making the bucket public. ttl is capped
// at PresignMaxTTL (the SigV4 ceiling).
func (m *Model) PresignGetURL(bucket *Object, key string, ttl time.Duration) (string, error) {
	if bucket == nil || bucket.Key == nil {
		return "", fmt.Errorf("bucket is nil")
	}
	if key == "" {
		return "", fmt.Errorf("key is empty")
	}
	if ttl <= 0 {
		return "", fmt.Errorf("ttl must be positive")
	}
	if ttl > PresignMaxTTL {
		ttl = PresignMaxTTL
	}

	ps := s3.NewPresignClient(m.Client)
	req, err := ps.PresignGetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(*bucket.Key),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

// copySource builds a URL-encoded CopySource value ("bucket/key"); the AWS
// SDK does not encode it for us, so keys with spaces/unicode would otherwise
// break.
func copySource(bucket, key string) string {
	u := &url.URL{Path: "/" + bucket + "/" + key}
	src := strings.TrimPrefix(u.EscapedPath(), "/")
	// EscapedPath leaves "+" literal (legal in an RFC 3986 path), but S3
	// decodes x-amz-copy-source query-style, where "+" means a space — so a
	// key like "report+final.pdf" would be looked up as "report final.pdf".
	return strings.ReplaceAll(src, "+", "%2B")
}

// planFolderCopy normalizes source/destination folder prefixes (each terminated
// with "/") and validates them. The self-overlap guards — copying a folder onto
// itself or into its own subtree — only make sense within a single bucket, so
// sameBucket=false relaxes them (the same prefix in another bucket is fine).
func planFolderCopy(sameBucket bool, srcKey, dstKey string) (src, dst string, err error) {
	src = srcKey
	if !strings.HasSuffix(src, "/") {
		src += "/"
	}
	dst = dstKey
	if !strings.HasSuffix(dst, "/") {
		dst += "/"
	}
	if sameBucket {
		if dst == src {
			return "", "", fmt.Errorf("source and destination are the same")
		}
		if strings.HasPrefix(dst, src) {
			return "", "", fmt.Errorf("cannot copy/move a folder into itself")
		}
	}
	return src, dst, nil
}

// remapKey maps a source object key living under prefix src to its destination
// key under prefix dst (both prefixes end with "/").
func remapKey(src, dst, key string) string {
	return dst + strings.TrimPrefix(key, src)
}

// CopyObject performs a single server-side object copy. Source and destination
// may live in different buckets; the copy is issued against the destination
// bucket with the source expressed as CopySource. (Both must be reachable
// through the same endpoint — see CopyKeys.)
// CopyObject issues one server-side copy. It takes a context so a cancelled
// copy/move/sync stops issuing requests instead of running to completion.
func (m *Model) CopyObject(ctx context.Context, srcBucket, dstBucket *Object, srcKey, dstKey string) error {
	if srcBucket == nil || srcBucket.Key == nil || dstBucket == nil || dstBucket.Key == nil {
		return fmt.Errorf("bucket is nil")
	}
	if *srcBucket.Key == *dstBucket.Key && srcKey == dstKey {
		return fmt.Errorf("source and destination are the same")
	}
	_, err := m.Client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(*dstBucket.Key),
		CopySource: aws.String(copySource(*srcBucket.Key, srcKey)),
		Key:        aws.String(dstKey),
	})
	return err
}

// CopyKeys copies srcKey (in srcBucket) to dstKey (in dstBucket). When isFolder
// is true it recursively copies every object under srcKey/ into dstKey/. Returns
// the number of objects copied.
//
// Cross-bucket copy uses the server-side CopyObject and therefore assumes both
// buckets are reachable through the currently configured endpoint (always true
// for a single S3-compatible endpoint such as MinIO/Ceph, and for same-region
// AWS buckets). Copying between AWS buckets in different regions is not handled
// here.
func (m *Model) CopyKeys(
	ctx context.Context,
	srcBucket, dstBucket *Object,
	srcKey, dstKey string,
	isFolder bool,
	progressCb func(done, total int, key string),
) (int, error) {
	if srcBucket == nil || srcBucket.Key == nil || dstBucket == nil || dstBucket.Key == nil {
		return 0, fmt.Errorf("bucket is nil")
	}

	if !isFolder {
		if err := m.CopyObject(ctx, srcBucket, dstBucket, srcKey, dstKey); err != nil {
			return 0, err
		}
		if progressCb != nil {
			progressCb(1, 1, dstKey)
		}
		return 1, nil
	}

	sameBucket := *srcBucket.Key == *dstBucket.Key
	src, dst, err := planFolderCopy(sameBucket, srcKey, dstKey)
	if err != nil {
		return 0, err
	}

	objs, err := m.ListObjects(src, srcBucket)
	if err != nil {
		return 0, err
	}

	total := len(objs)
	done := 0
	for _, o := range objs {
		if o.Key == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return done, ctx.Err()
		default:
		}
		newKey := remapKey(src, dst, *o.Key)
		if err := m.CopyObject(ctx, srcBucket, dstBucket, *o.Key, newKey); err != nil {
			return done, fmt.Errorf("copy %s: %w", *o.Key, err)
		}
		done++
		if progressCb != nil {
			progressCb(done, total, newKey)
		}
	}
	return done, nil
}

// MoveKeys copies srcKey (in srcBucket) to dstKey (in dstBucket) and then
// deletes the source. Rename is a same-bucket move whose destination shares the
// source prefix.
func (m *Model) MoveKeys(
	ctx context.Context,
	srcBucket, dstBucket *Object,
	srcKey, dstKey string,
	isFolder bool,
	progressCb func(done, total int, key string),
) (int, error) {
	n, err := m.CopyKeys(ctx, srcBucket, dstBucket, srcKey, dstKey, isFolder, progressCb)
	if err != nil {
		return n, err
	}

	delKey := srcKey
	if isFolder && !strings.HasSuffix(delKey, "/") {
		delKey += "/"
	}
	if err := m.Delete(&delKey, srcBucket); err != nil {
		return n, fmt.Errorf("copied ok, but failed to delete source: %w", err)
	}
	return n, nil
}
