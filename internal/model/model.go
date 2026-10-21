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
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	s3m "github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"

	u "github.com/nexusriot/s3duck-tui/internal/utils"
)

// ErrFileExists is returned (wrapped, with the path) when a download would
// overwrite an existing local file. Callers that want to replace the file —
// the overwrite prompt, sync — must remove it first; errors.Is identifies it.
var ErrFileExists = errors.New("file exists")

// ErrTruncatedListing reports a listing page that said more results follow but
// carried no new continuation token. The SDK's paginator simply stops there,
// which handed callers a partial listing with a nil error — the shape a
// delete-extraneous sync, a bucket empty and a delete-size scan all take as
// the whole truth. Some S3-compatible backends produce it.
var ErrTruncatedListing = errors.New("the server reported more results but sent no new continuation token")

// truncatedWithoutToken reports a page the paginator would silently stop
// after: truncated, with a next token that is missing or repeats the previous
// page's (prevNext).
func truncatedWithoutToken(out *s3.ListObjectsV2Output, prevNext string) bool {
	if out == nil || !out.IsTruncated {
		return false
	}
	next := aws.ToString(out.NextContinuationToken)
	return next == "" || next == prevNext
}

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
	// AWSProfile delegates credentials to the AWS SDK's own resolution for
	// that shared-config profile instead of using the static keys above. Empty
	// means static credentials. See GetConfig.
	AWSProfile string
	// Write holds the per-profile decisions for every object this app creates
	// (content type, encryption, checksums). See WriteOptions.
	Write WriteOptions
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

// wait blocks until n bytes may be transferred under the configured rate, or
// until ctx is done, whose error it then returns. Safe for concurrent use; a
// nil limiter returns immediately (unlimited), as does a nil ctx's sleep end.
//
// The sleep has to answer ctx. A downloader writes each 5 MiB chunk in one
// WriteAt and an uploader reads a whole part in one Read, so with a low cap a
// single call owes tens of seconds — and with four workers times the SDK's own
// part concurrency, a Cancel used to wait out minutes of sleeps, holding the
// transfer slot all the while.
func (l *rateLimiter) wait(ctx context.Context, n int) error {
	if l == nil || n <= 0 {
		return nil
	}
	l.mu.Lock()
	now := time.Now()
	elapsed := now.Sub(l.last)
	l.last = now
	sleep, tokens := throttleStep(l.rate, l.tokens, elapsed, n)
	l.tokens = tokens
	l.mu.Unlock()
	if sleep <= 0 {
		return nil
	}
	if ctx == nil {
		time.Sleep(sleep)
		return nil
	}
	t := time.NewTimer(sleep)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
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
	// ctx is the transfer's context: a cancel interrupts the throttle's
	// sleep, and the read reports it (see rateLimiter.wait).
	ctx context.Context
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	pr.written += int64(n)
	pr.update(pr.written, pr.total)
	if werr := pr.limiter.wait(pr.ctx, n); werr != nil && err == nil {
		err = werr
	}
	return n, err
}

type progressWriterAt struct {
	w          io.WriterAt
	written    int64
	total      int64
	updateFunc func(written int64, total int64)
	limiter    *rateLimiter
	// ctx is the transfer's context; see progressReader.
	ctx context.Context
}

func (pwa *progressWriterAt) WriteAt(p []byte, off int64) (int, error) {
	n, err := pwa.w.WriteAt(p, off)
	w := atomic.AddInt64(&pwa.written, int64(n))
	if pwa.updateFunc != nil {
		pwa.updateFunc(w, pwa.total)
	}
	if werr := pwa.limiter.wait(pwa.ctx, n); werr != nil && err == nil {
		err = werr
	}
	return n, err
}

// uploadMaxAttempts is the total number of tries (1 initial + 2 retries) the
// uploader makes per part before giving up.
const uploadMaxAttempts = 3

// uploadRetryer is the retry policy for uploads. Uploads previously ran with
// aws.NopRetryer{}, so a single transient network error failed the whole
// transfer — painful for a multi-file upload and worse for a sync run pushing
// hundreds of objects. The SDK's standard retryer backs off on throttling and
// transient 5xx/connection errors; retries are safe here because the uploader
// buffers each part of a non-seekable body before sending it, so a retry
// resends the buffered part rather than re-reading a consumed stream.
func uploadRetryer() aws.Retryer {
	return retry.NewStandard(func(o *retry.StandardOptions) {
		o.MaxAttempts = uploadMaxAttempts
	})
}

// newUploader builds the shared multipart uploader used by every upload path
// (UploadFile, and through it UploadTargets; CrossCopy), so they can't drift
// on part size or retry policy. size is the body's length, or <= 0 if unknown.
func newUploader(client *s3.Client, size int64) *s3m.Uploader {
	return s3m.NewUploader(client, func(u *s3m.Uploader) {
		u.PartSize = uploadPartSize(size)
		u.LeavePartsOnError = false
		u.ClientOptions = append(u.ClientOptions, func(o *s3.Options) {
			o.Retryer = uploadRetryer()
		})
	})
}

// uploadPartSize picks a part size that fits size into S3's 10 000-part limit.
//
// The SDK grows the part size itself only for a seekable body, and every body
// here is a progressReader, which is not one. At a fixed 5 MiB, anything over
// 10 000 × 5 MiB (48.8 GiB) ran to the last part and then failed with
// "exceeded total allowed configured MaxUploadParts", throwing the whole
// transfer away.
func uploadPartSize(size int64) int64 {
	const mib = 1 << 20
	ps := s3m.MinUploadPartSize
	if size <= 0 {
		return ps
	}
	if need := (size + int64(s3m.MaxUploadParts) - 1) / int64(s3m.MaxUploadParts); need > ps {
		ps = (need + mib - 1) / mib * mib
	}
	return ps
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
		Write:          DefaultWriteOptions(),
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

	var opts []optsFunc
	if update && strings.Contains(cf.Url, "amazonaws.com") {
		opts = []optsFunc{config.WithRegion(*cf.Region)}
	} else {
		opts = []optsFunc{config.WithEndpointResolverWithOptions(customResolver)}
	}

	// Static keys suit a long-lived MinIO key; anything temporary wants the
	// SDK's own resolution chain, which runs the SSO flow, assumes the role or
	// invokes credential_process — and re-resolves when what it handed out
	// expires. Naming a shared-config profile stores no key material here.
	if cf.AWSProfile != "" {
		opts = append(opts, config.WithSharedConfigProfile(cf.AWSProfile))
	} else {
		opts = append(opts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cf.AccessKey, cf.SecretKey, cf.SessionToken)))
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
	opts = append(opts, config.WithHTTPClient(timeoutClient))

	cfg, err := config.LoadDefaultConfig(context.TODO(), opts...)
	if err != nil {
		return cfg, err
	}
	// A delegating profile that resolves to nothing would be worth catching
	// here, while the message can still name the profile. This check is only
	// a backstop, though: the pinned SDK (config v1.18.8) does not leave
	// Credentials nil for such a profile — it falls back to its default chain,
	// ending at EC2 IMDS — so the usual symptom is still an opaque credential
	// error on the first request. Catching it properly means retrieving once
	// at open, which would run SSO / credential_process / STS right there.
	if cf.AWSProfile != "" && cfg.Credentials == nil {
		return cfg, fmt.Errorf("aws profile %q resolved no credentials", cf.AWSProfile)
	}
	return cfg, nil
}

// CredentialSource describes where this connection's credentials come from,
// for the profile details pane.
func (c Config) CredentialSource() string {
	if c.AWSProfile != "" {
		return "aws profile " + c.AWSProfile + " (SDK-resolved, auto-refreshing)"
	}
	if c.SessionToken != "" {
		return "static keys + session token (expires, no refresh)"
	}
	return "static keys"
}

func NewModel(cf Config) (*Model, error) {
	cfg, err := GetConfig(cf, false)
	if err != nil {
		// Swallowing this used to hand back a zero-value client whose every
		// call failed with unrelated-looking auth errors; a malformed
		// ~/.aws/config is enough to get here.
		return nil, fmt.Errorf("building AWS config: %w", err)
	}

	client := s3.NewFromConfig(cfg)

	m := Model{
		Config:     &cfg,
		Client:     client,
		Downloader: GetDownloader(client),
		Cf:         &cf,
		Limiter:    newRateLimiter(cf.MaxBytesPerSec),
	}
	return &m, nil
}

// RefreshClient re-pins the client to the bucket's actual region. It runs on
// every endpoint: for an amazonaws.com URL the rebuilt client uses the SDK's
// regional endpoint, and for a custom endpoint the URL stays but the signing
// region becomes the one GetBucketLocation reported. On any failure it
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

// ListObjectsStream lists every object under key recursively (no delimiter),
// handing each page to onPage along with the running total. onPage returns
// false to stop early, which is how the callers that cap their result set
// avoid paying for pages they will throw away.
//
// Cancellation is checked before every page as well as being passed into the
// request, so a cancelled scan stops at the next page boundary at the latest.
func (m *Model) ListObjectsStream(ctx context.Context, key string, bucket *Object, onPage func(page []s3t.Object, total int) bool) error {
	if bucket == nil || bucket.Key == nil {
		return fmt.Errorf("bucket is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(*bucket.Key),
		Prefix: aws.String(key),
	}

	total := 0
	prevNext := ""
	paginator := s3.NewListObjectsV2Paginator(m.Client, input)
	for paginator.HasMorePages() {
		if err := ctx.Err(); err != nil {
			return err
		}
		output, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}
		// Refused before the page is handed on: every caller of the recursive
		// listing either deletes or decides what to delete from it.
		if truncatedWithoutToken(output, prevNext) {
			return fmt.Errorf("listing %s/%s: %w", *bucket.Key, key, ErrTruncatedListing)
		}
		prevNext = aws.ToString(output.NextContinuationToken)
		total += len(output.Contents)
		if onPage != nil && !onPage(output.Contents, total) {
			return nil
		}
	}
	return nil
}

// ListObjects lists every object under key recursively (no delimiter). The
// context is honoured between pages as well as inside them: a recursive
// listing of a large prefix is dozens of round trips, and cancelling a
// copy/sync/delete used to keep paginating to the end because this ran on
// context.TODO().
func (m *Model) ListObjects(ctx context.Context, key string, bucket *Object) ([]s3t.Object, error) {
	var objects []s3t.Object
	err := m.ListObjectsStream(ctx, key, bucket, func(page []s3t.Object, _ int) bool {
		objects = append(objects, page...)
		return true
	})
	if err != nil {
		return nil, err
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

// List is the delimited listing behind ordinary navigation: one prefix level,
// folders (common prefixes) and files. It accumulates every page; ListStream
// is the incremental form the browser uses so a prefix holding a hundred
// thousand keys can be shown as it arrives instead of after the last page.
func (m *Model) List(ctx context.Context, path string, bucket *Object) ([]*Object, error) {
	objs := make([]*Object, 0)
	err := m.ListStream(ctx, path, bucket, func(page []*Object, _ int) bool {
		objs = append(objs, page...)
		return true
	})
	if err != nil {
		return nil, err
	}
	return objs, nil
}

// ListStream is List, page by page. onPage receives the objects decoded from
// one ListObjectsV2 page and the running total, and returns false to stop —
// which is how the browser enforces its listing cap without fetching the rest
// of a huge prefix.
//
// Before this took a context, navigating into a prefix with a hundred
// thousand keys at one level meant a hundred blocking round trips that
// nothing could interrupt, and the whole set landed in the list widget at
// once. The context is honoured between pages as well as within them.
func (m *Model) ListStream(ctx context.Context, path string, bucket *Object, onPage func(page []*Object, total int) bool) error {
	if bucket == nil || bucket.Key == nil {
		return fmt.Errorf("bucket is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	input := &s3.ListObjectsV2Input{
		Bucket:    aws.String(*bucket.Key),
		Delimiter: aws.String("/"),
		Prefix:    aws.String(path),
	}

	total := 0
	prevNext := ""
	paginator := s3.NewListObjectsV2Paginator(m.Client, input)

	for paginator.HasMorePages() {
		if err := ctx.Err(); err != nil {
			return err
		}
		output, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}
		// Unlike the recursive listing, this page is still worth showing —
		// the browser marks the pane partial on ErrTruncatedListing — so the
		// check reports after onPage below rather than before it.
		truncated := truncatedWithoutToken(output, prevNext)
		prevNext = aws.ToString(output.NextContinuationToken)
		objs := make([]*Object, 0, len(output.CommonPrefixes)+len(output.Contents))

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
		total += len(objs)
		if onPage != nil && !onPage(objs, total) {
			return nil
		}
		if truncated {
			return fmt.Errorf("listing %s/%s: %w", *bucket.Key, path, ErrTruncatedListing)
		}
	}
	return nil
}

// ListBuckets returns the endpoint's buckets. ctx may be nil, in which case a
// generous whole-call bound applies.
func (m *Model) ListBuckets(ctx context.Context) ([]*Object, error) {
	// A generous whole-call bound only: hung connections are already cut by the
	// per-phase transport timeouts, and 5s here used to make profiles unusable
	// over slow links (high-latency VPNs, huge bucket lists).
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
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

// Delete removes a single key, or every object under a folder prefix. A
// prefix delete lists recursively first, so it takes the caller's context: a
// cancelled delete stops rather than paginating a huge prefix to the end.
func (m *Model) Delete(ctx context.Context, key *string, bucket *Object) error {
	if bucket == nil || bucket.Key == nil {
		return fmt.Errorf("bucket is nil")
	}
	if key == nil || *key == "" {
		return fmt.Errorf("key is empty")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var objectIds []s3t.ObjectIdentifier

	if strings.HasSuffix(*key, "/") {
		ks, err := m.ListObjects(ctx, *key, bucket)

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

	return m.deleteObjectIDs(ctx, bucket, objectIds)
}

// deleteObjectIDs removes the given objects in DeleteObjects batches of 1000
// (the S3 API maximum), failing on the first per-key error the service reports.
func (m *Model) deleteObjectIDs(ctx context.Context, bucket *Object, objectIds []s3t.ObjectIdentifier) error {
	if len(objectIds) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	const maxDelete = 1000

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
func (m *Model) EmptyBucket(ctx context.Context, bucket *Object) error {
	if bucket == nil || bucket.Key == nil {
		return fmt.Errorf("bucket is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	objs, err := m.ListObjects(ctx, "", bucket)
	if err != nil {
		return err
	}
	ids := make([]s3t.ObjectIdentifier, 0, len(objs))
	for _, o := range objs {
		if o.Key != nil {
			ids = append(ids, s3t.ObjectIdentifier{Key: o.Key})
		}
	}
	return m.deleteObjectIDs(ctx, bucket, ids)
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
	in := &s3.PutObjectInput{
		Bucket: aws.String(*bucket.Key),
		Key:    aws.String(*name),
	}
	// No name to derive a type from — a folder marker is not a file — but a
	// profile that encrypts everything must encrypt this too, or a bucket
	// policy requiring SSE rejects the marker and folder creation fails.
	m.writeOpts().applyPut(in, "")
	_, err := m.Client.PutObject(context.TODO(), in)
	return err
}

// walkFollowingRoot walks localPath like filepath.Walk, but follows localPath
// itself when it is (or traverses) a symlink. filepath.Walk lstats its root,
// so a symlinked directory would otherwise "walk" to a single skipped
// symlink entry and report an empty tree with a nil error — an upload that
// silently transfers nothing, or a sync listing that plans deleting the whole
// destination. Visited paths are reported under localPath, not the resolved
// target, so key derivation keeps the user-visible name.
func walkFollowingRoot(localPath string, fn filepath.WalkFunc) error {
	resolved, err := filepath.EvalSymlinks(localPath)
	if err != nil {
		return err
	}
	if resolved == localPath {
		return filepath.Walk(localPath, fn)
	}
	return filepath.Walk(resolved, func(p string, fi os.FileInfo, err error) error {
		return fn(localPath+strings.TrimPrefix(p, resolved), fi, err)
	})
}

// followFileLink resolves a symbolic link met BELOW a walked root when it
// points at a regular file, returning the target's info so the file is sent
// with its real size and mtime. filepath.Walk lstats every entry, so such a
// link used to read as "not a regular file" and vanish from uploads and sync
// listings without a word. Anything else — a link to a directory (which may
// loop, or lead anywhere on the filesystem), a dangling link — is returned
// unchanged and stays unfollowed; WalkLocalTree reports those to sync.
func followFileLink(p string, fi os.FileInfo) os.FileInfo {
	if fi == nil || fi.Mode()&os.ModeSymlink == 0 {
		return fi
	}
	if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
		return st
	}
	return fi
}

// Upload sends a local file or directory tree to s3Prefix.
//
// It is PrepareUpload followed by UploadTargets, so the keys it writes are by
// construction the ones the preview and the overwrite scan were shown. (It
// used to re-derive them during a walk of its own, with path.Join, which
// cleans "//" and ".." segments that PrepareUpload keeps: under a prefix like
// "logs//2024/" the preview promised one key and the upload wrote another —
// past the overwrite check, and with the user's skip answers matching
// nothing.)
//
// skip, when non-nil, names remote keys the user chose to keep: those targets
// are never sent, nor counted in the progress totals.
func (m *Model) Upload(
	ctx context.Context,
	localPath, s3Prefix string,
	bucket *Object,
	skip map[string]bool,
	progressCb func(current, total int64, done, count int, local, remote string),
) error {
	targets, _, err := m.PrepareUpload(localPath, s3Prefix, bucket)
	if err != nil {
		return err
	}
	if len(skip) > 0 {
		kept := targets[:0:0]
		for _, t := range targets {
			if !skip[t.RemotePath] {
				kept = append(kept, t)
			}
		}
		targets = kept
	}
	return m.UploadTargets(ctx, bucket, targets, progressCb)
}

// UploadTargets sends exactly targets: no walk, no key derivation. A target
// whose RemotePath ends in "/" is a folder marker for an empty directory and
// is written as a zero-byte object.
//
// progressCb reports the bytes sent so far against the targets' total, and
// how many targets have completed out of how many.
func (m *Model) UploadTargets(
	ctx context.Context,
	bucket *Object,
	targets []UploadTarget,
	progressCb func(current, total int64, done, count int, local, remote string),
) error {
	if bucket == nil || bucket.Key == nil {
		return fmt.Errorf("bucket is nil")
	}
	var total int64
	for _, t := range targets {
		total += t.Size
	}

	var sent int64
	for i, t := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		if strings.HasSuffix(t.RemotePath, "/") {
			if err := m.putMarker(ctx, bucket, t.RemotePath); err != nil {
				return fmt.Errorf("failed to create folder marker %s: %w", t.RemotePath, err)
			}
		} else {
			var fileSent int64
			err := m.UploadFile(ctx, t.LocalPath, t.RemotePath, bucket, func(written, _ int64) {
				fileSent = written
				if progressCb != nil {
					progressCb(sent+written, total, i, len(targets), t.LocalPath, t.RemotePath)
				}
			})
			if err != nil {
				return err
			}
			sent += fileSent
		}
		if progressCb != nil {
			progressCb(sent, total, i+1, len(targets), t.LocalPath, t.RemotePath)
		}
	}
	return nil
}

// putMarker writes the zero-byte object that represents an empty folder.
// A marker is not a file, so there is no name to derive a type from — but a
// profile that encrypts everything must encrypt this too, or a bucket policy
// requiring SSE rejects the marker and the whole directory upload fails.
// CreateFolder is the other way a marker gets written.
func (m *Model) putMarker(ctx context.Context, bucket *Object, key string) error {
	in := &s3.PutObjectInput{
		Bucket: aws.String(*bucket.Key),
		Key:    aws.String(key),
		Body:   strings.NewReader(""),
	}
	m.writeOpts().applyPut(in, "")
	_, err := m.Client.PutObject(ctx, in)
	return err
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
	// its top-level directory name. These targets are exactly what Upload
	// sends: the preview and the transfer share one derivation.
	base := filepath.Dir(localPath)
	err = walkFollowingRoot(localPath, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		fi = followFileLink(p, fi)
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			// S3 has no real folders: an empty directory exists only as a
			// key ending in "/". A non-empty one needs nothing of its own.
			// (Same convention as LocalTargets.)
			if isEmptyDir(p) {
				targets = append(targets, UploadTarget{LocalPath: p, RemotePath: prefix + filepath.ToSlash(rel) + "/"})
			}
			return nil
		}
		// Sockets, FIFOs and devices are skipped: opening a FIFO with no
		// writer blocks forever and the open syscall ignores ctx cancel, so
		// one stray pipe would hang the whole upload.
		if !fi.Mode().IsRegular() {
			return nil
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
func (m *Model) ResolveDownloadObjects(ctx context.Context, key string, isFolder bool, size *int64, bucket *Object) ([]DownloadTarget, int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if isFolder {
		if !strings.HasSuffix(key, "/") {
			key += "/"
		}
		objs, err := m.ListObjects(ctx, key, bucket)
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

// SafeLocalPath maps an object key onto a local path under destPath, stripping
// the currentPath prefix. S3 keys are opaque and may legally contain ".."
// segments; filepath.Join cleans them, so an unchecked join lets a hostile or
// accidental key name a file OUTSIDE the chosen download directory (zip-slip).
// Any key whose cleaned path escapes destPath is rejected loudly.
func SafeLocalPath(destPath, currentPath, s3Key string) (string, error) {
	key := filepath.ToSlash(s3Key)
	prefix := NormalizePrefix(currentPath)
	relativeKey := key
	if strings.HasPrefix(key, prefix) {
		relativeKey = strings.TrimPrefix(key, prefix)
	}

	p := filepath.Join(destPath, filepath.FromSlash(relativeKey))
	// Containment is decided by Rel, not by a string prefix: Join cleans "./"
	// away and a root already ends in a separator, so a prefix test against
	// "."+sep or "/"+sep rejected every key when downloading into the working
	// directory or a filesystem root.
	rel, err := filepath.Rel(filepath.Clean(destPath), p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("object key %q escapes the download directory", s3Key)
	}
	return p, nil
}

func (m *Model) DownloadTarget(
	ctx context.Context,
	t DownloadTarget,
	currentPath,
	destPath string,
	bucket *string,
	overwrite bool,
	progressCb func(written int64, total int64, key string),
) (int64, error) {
	if err := os.MkdirAll(destPath, 0700); err != nil {
		return 0, err
	}

	downloadPath, err := SafeLocalPath(destPath, currentPath, t.Key)
	if err != nil {
		return 0, err
	}

	if strings.HasSuffix(t.Key, "/") {
		return 0, os.MkdirAll(downloadPath, 0760)
	}
	if err := os.MkdirAll(filepath.Dir(downloadPath), 0760); err != nil {
		return 0, err
	}
	if !overwrite {
		if _, err := os.Stat(downloadPath); err == nil {
			return 0, fmt.Errorf("%w: %s", ErrFileExists, downloadPath)
		}
	}

	// Download into a sibling temp file and rename onto the target only after
	// the body is fully on disk: a failed or canceled transfer must never leave
	// the target missing or truncated, and an overwrite must not destroy the
	// existing file before its replacement exists.
	//
	// The temp name is unique per transfer. Two keys can land on one local
	// path ("a//b" and "a/b", or "Report.pdf" and "report.pdf" on a
	// case-insensitive filesystem); with a fixed name, two workers truncated
	// and wrote into the same temp file and the result was an interleaving of
	// both objects, reported as a success.
	fp, err := os.CreateTemp(filepath.Dir(downloadPath), tempStem(filepath.Base(downloadPath))+".*.s3duck-part")
	if err != nil {
		return 0, err
	}
	tmpPath := fp.Name()
	// CreateTemp makes the file 0600; a download keeps the mode it always had.
	_ = fp.Chmod(0644)

	writerAt := &progressWriterAt{
		w:     fp,
		total: t.Size,
		updateFunc: func(written int64, total int64) {
			if progressCb != nil {
				progressCb(written, total, t.Key)
			}
		},
		limiter: m.Limiter,
		ctx:     ctx,
	}

	n, err := m.Downloader.Download(ctx, writerAt, &s3.GetObjectInput{
		Bucket: bucket,
		Key:    aws.String(t.Key),
	})
	if ctx.Err() != nil {
		fp.Close()
		_ = os.Remove(tmpPath)
		return 0, ctx.Err()
	}
	if err != nil {
		fp.Close()
		_ = os.Remove(tmpPath)
		return n, err
	}
	if err := fp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return n, err
	}
	if err := os.Rename(tmpPath, downloadPath); err != nil {
		_ = os.Remove(tmpPath)
		return n, err
	}
	return n, nil
}

// tempStemMax bounds the part of a download's temp name taken from the target.
// The suffix adds about 23 bytes (".<random>.s3duck-part"), and filesystems cap
// a name at 255: copied whole, any valid target name past ~232 bytes — about 78
// CJK characters — failed with "file name too long" and could not be
// downloaded at all.
const tempStemMax = 120

// tempStem is the target's base name shortened for a temp name, cut on a rune
// boundary so the result stays valid UTF-8.
func tempStem(base string) string {
	if len(base) <= tempStemMax {
		return base
	}
	cut := tempStemMax
	for cut > 0 && !utf8.RuneStart(base[cut]) {
		cut--
	}
	return base[:cut]
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

// checkSelfOverlap refuses a same-bucket folder copy whose destination keys
// land on the source's own objects. planFolderCopy already refuses a
// destination inside the source; the other way round — the destination an
// ancestor of the source, as in moving "a/a/" up to "a/" — is legitimate
// unless the source holds the sub-path that maps onto itself. Then the copy
// overwrites a source object before reading it ("a/a/a/f" lists first and
// lands on "a/a/f"), and a move deletes both: the original content is gone
// without having been written anywhere.
func checkSelfOverlap(sameBucket bool, src, dst string, objs []s3t.Object) error {
	if !sameBucket || !strings.HasPrefix(src, dst) {
		return nil
	}
	keys := make(map[string]bool, len(objs))
	for _, o := range objs {
		if o.Key != nil {
			keys[*o.Key] = true
		}
	}
	for _, o := range objs {
		if o.Key == nil {
			continue
		}
		if k := remapKey(src, dst, *o.Key); keys[k] {
			return fmt.Errorf("destination overlaps the source: %s would land on %s, which is itself being copied", *o.Key, k)
		}
	}
	return nil
}

// CopyObject performs a single server-side object copy. Source and destination
// may live in different buckets; the copy is issued against the destination
// bucket with the source expressed as CopySource. (Both must be reachable
// through the same endpoint — see CopyKeys.)
// CopyObject issues one server-side copy. It takes a context so a cancelled
// copy/move/sync stops issuing requests instead of running to completion.
// CopyObject copies one object server-side. The source size is unknown here,
// so it costs one HeadObject to decide between a single-request copy and a
// multipart part-copy; callers that already know the size (a folder listing, a
// sync plan) should use CopyObjectSized and skip it.
func (m *Model) CopyObject(ctx context.Context, srcBucket, dstBucket *Object, srcKey, dstKey string) error {
	return m.CopyObjectSized(ctx, srcBucket, dstBucket, srcKey, dstKey, SizeUnknown)
}

// CopyObjectSized is CopyObject with the source's size already known. Sizes
// above MultipartCopyThreshold are copied part by part: a single-request
// CopyObject is capped at 5 GiB by S3.
func (m *Model) CopyObjectSized(ctx context.Context, srcBucket, dstBucket *Object, srcKey, dstKey string, size int64) error {
	if srcBucket == nil || srcBucket.Key == nil || dstBucket == nil || dstBucket.Key == nil {
		return fmt.Errorf("bucket is nil")
	}
	if *srcBucket.Key == *dstBucket.Key && srcKey == dstKey {
		return fmt.Errorf("source and destination are the same")
	}
	return m.runCopy(ctx, copySpec{
		srcBucket: *srcBucket.Key,
		srcKey:    srcKey,
		dstBucket: *dstBucket.Key,
		dstKey:    dstKey,
		srcSize:   size,
	})
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
// skip, when non-nil, names destination keys the user chose to keep: those
// objects are not written. A skipped destination also leaves its source in
// place on a move, because copyKeysTracked reports only what it actually
// copied and MoveKeys deletes exactly that.
func (m *Model) CopyKeys(
	ctx context.Context,
	srcBucket, dstBucket *Object,
	srcKey, dstKey string,
	isFolder bool,
	skip map[string]bool,
	progressCb func(done, total int, key string),
) (int, error) {
	copied, err := m.copyKeysTracked(ctx, srcBucket, dstBucket, srcKey, dstKey, isFolder, skip, progressCb)
	return len(copied), err
}

// copyKeysTracked is CopyKeys returning the exact source keys that were
// copied. MoveKeys deletes precisely this set — never a fresh listing of the
// source prefix, which would sweep up objects written after the copy's
// snapshot and destroy them without a copy existing anywhere.
func (m *Model) copyKeysTracked(
	ctx context.Context,
	srcBucket, dstBucket *Object,
	srcKey, dstKey string,
	isFolder bool,
	skip map[string]bool,
	progressCb func(done, total int, key string),
) ([]string, error) {
	if srcBucket == nil || srcBucket.Key == nil || dstBucket == nil || dstBucket.Key == nil {
		return nil, fmt.Errorf("bucket is nil")
	}

	if !isFolder {
		if skip[dstKey] {
			return nil, nil
		}
		if err := m.CopyObject(ctx, srcBucket, dstBucket, srcKey, dstKey); err != nil {
			return nil, err
		}
		if progressCb != nil {
			progressCb(1, 1, dstKey)
		}
		return []string{srcKey}, nil
	}

	sameBucket := *srcBucket.Key == *dstBucket.Key
	src, dst, err := planFolderCopy(sameBucket, srcKey, dstKey)
	if err != nil {
		return nil, err
	}

	objs, err := m.ListObjects(ctx, src, srcBucket)
	if err != nil {
		return nil, err
	}
	if err := checkSelfOverlap(sameBucket, src, dst, objs); err != nil {
		return nil, err
	}

	total := len(objs)
	var copied []string
	for _, o := range objs {
		if o.Key == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return copied, ctx.Err()
		default:
		}
		newKey := remapKey(src, dst, *o.Key)
		if skip[newKey] {
			continue
		}
		// The listing already carries the size, so the copy never has to
		// HeadObject to find out whether it must go multipart.
		if err := m.CopyObjectSized(ctx, srcBucket, dstBucket, *o.Key, newKey, o.Size); err != nil {
			return copied, fmt.Errorf("copy %s: %w", *o.Key, err)
		}
		copied = append(copied, *o.Key)
		if progressCb != nil {
			progressCb(len(copied), total, newKey)
		}
	}
	return copied, nil
}

// MoveKeys copies srcKey (in srcBucket) to dstKey (in dstBucket) and then
// deletes the source. Rename is a same-bucket move whose destination shares the
// source prefix.
func (m *Model) MoveKeys(
	ctx context.Context,
	srcBucket, dstBucket *Object,
	srcKey, dstKey string,
	isFolder bool,
	skip map[string]bool,
	progressCb func(done, total int, key string),
) (int, error) {
	copied, err := m.copyKeysTracked(ctx, srcBucket, dstBucket, srcKey, dstKey, isFolder, skip, progressCb)
	if err != nil {
		return len(copied), err
	}

	// Delete exactly the keys that were copied. Re-listing the source prefix
	// here would also catch objects written between the copy's snapshot and
	// now (a backgrounded upload, an external producer) and delete them
	// without them ever having been copied.
	ids := make([]s3t.ObjectIdentifier, 0, len(copied))
	for _, k := range copied {
		ids = append(ids, s3t.ObjectIdentifier{Key: aws.String(k)})
	}
	if err := m.deleteObjectIDs(ctx, srcBucket, ids); err != nil {
		return len(copied), fmt.Errorf("copied ok, but failed to delete source: %w", err)
	}
	return len(copied), nil
}
