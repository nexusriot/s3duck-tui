package model

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	s3m "github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	u "github.com/nexusriot/s3duck-tui/pkg/utils"
)

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
	SSl       bool
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
}

type progressReader struct {
	r       io.Reader
	written int64
	total   int64
	update  func(written int64, total int64)
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	pr.written += int64(n)
	pr.update(pr.written, pr.total)
	return n, err
}

type progressWriterAt struct {
	w          io.WriterAt
	written    int64
	total      int64
	updateFunc func(written int64, total int64)
}

func (pwa *progressWriterAt) WriteAt(p []byte, off int64) (int, error) {
	n, err := pwa.w.WriteAt(p, off)
	w := atomic.AddInt64(&pwa.written, int64(n))
	if pwa.updateFunc != nil {
		pwa.updateFunc(w, pwa.total)
	}
	return n, err
}

func GetDownloader(client *s3.Client) *s3m.Downloader {
	d := s3m.NewDownloader(client, func(d *s3m.Downloader) {
		d.BufferProvider = s3m.NewPooledBufferedWriterReadFromProvider(5 * 1024 * 1024)
	})
	return d
}

func NewConfig(url string, region *string, accKey string, secKey string, ssl bool) Config {
	return Config{
		url,
		region,
		accKey,
		secKey,
		ssl,
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

	staticProvider := credentials.NewStaticCredentialsProvider(cf.AccessKey, cf.SecretKey, "")

	var opts []optsFunc
	if update && strings.Contains(cf.Url, "amazon") {
		opts = []optsFunc{config.WithRegion(*cf.Region)}
	} else {
		opts = []optsFunc{config.WithEndpointResolverWithOptions(customResolver)}
	}

	timeoutClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: !cf.SSl},
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
	}
	return &m
}

func (m *Model) RefreshClient(bucket *string) {

	// TODO: handle err
	region, _ := m.GetBucketLocation(bucket)
	m.Cf.Region = region
	cfg, _ := GetConfig(*m.Cf, true)

	m.Client = s3.NewFromConfig(cfg)
	m.Downloader = GetDownloader(m.Client)
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

func (m *Model) Download(
	ctx context.Context,
	object s3t.Object,
	currentPath, destPath string,
	bucket *string,
	totalSize int64,
	progressCb func(written int64, total int64, key string),
) (int64, error) {
	if err := os.MkdirAll(destPath, 0700); err != nil {
		return 0, err
	}

	key := filepath.ToSlash(*object.Key)
	prefix := filepath.ToSlash(currentPath)

	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	relativeKey := key
	if strings.HasPrefix(key, prefix) {
		relativeKey = strings.TrimPrefix(key, prefix)
	}

	downloadPath := filepath.Join(destPath, relativeKey)

	if strings.HasSuffix(*object.Key, "/") {
		return 0, os.MkdirAll(downloadPath, 0760)
	}
	dir := filepath.Dir(downloadPath)
	if err := os.MkdirAll(dir, 0760); err != nil {
		return 0, err
	}

	if _, err := os.Stat(downloadPath); err == nil {
		return 0, ErrFileExists
	}

	fp, err := os.Create(downloadPath)
	if err != nil {
		return 0, err
	}
	defer func() {
		if cerr := fp.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	writerAt := &progressWriterAt{
		w:     fp,
		total: object.Size,
		updateFunc: func(written int64, total int64) {
			if progressCb != nil {
				progressCb(written, total, *object.Key)
			}
		},
	}

	n, err := m.Downloader.Download(ctx, writerAt, &s3.GetObjectInput{
		Bucket: bucket,
		Key:    object.Key,
	})

	if ctx.Err() != nil {
		os.Remove(downloadPath)
		return 0, ctx.Err()
	}

	return n, err
}

func (m *Model) GetBucketLocation(name *string) (*string, error) {
	bl, err := m.Client.GetBucketLocation(
		context.TODO(),
		&s3.GetBucketLocationInput{
			Bucket: name,
		},
	)
	location := string(bl.LocationConstraint)
	if location == "" {
		loc := "us-east-1"
		location = loc
	}
	return &location, err
}

func (m *Model) List(path string, bucket *Object) ([]*Object, error) {
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
			if *o.Key == path {
				continue
			}
			fields := strings.FieldsFunc(strings.TrimSpace(*o.Key), u.SplitFunc)
			appKey := fields[len(fields)-1]
			ts := strings.Trim(*o.ETag, "\"")
			size := o.Size

			ko := &Object{
				&appKey,
				File,
				&ts,
				&size,
				nil,
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
		sv := aws.String(*b.Name)
		td := aws.Time(*b.CreationDate)
		ko := &Object{
			Key:          sv,
			Ot:           Bucket,
			Etag:         nil,
			Size:         nil,
			StorageClass: nil,
			LastModified: td,
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

func (m *Model) DeleteBucket(name *string) error {
	_, err := m.Client.DeleteBucket(context.TODO(), &s3.DeleteBucketInput{
		Bucket: aws.String(*name)})
	if err != nil {
		log.Printf("Couldn't delete bucket %v because of %v\n", *name, err)
	}
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

	uploader := s3m.NewUploader(m.Client, func(u *s3m.Uploader) {
		u.PartSize = 5 * 1024 * 1024
		u.LeavePartsOnError = false
		u.ClientOptions = append(u.ClientOptions, func(o *s3.Options) {
			o.Retryer = aws.NopRetryer{}
		})
	})

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
		}

		uploadCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		_, err = uploader.Upload(uploadCtx, &s3.PutObjectInput{
			Bucket: aws.String(*bucket.Key),
			Key:    aws.String(s3Key),
			Body:   reader,
		})
		fp.Close()

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

	// Normalize currentPath to S3 prefix style
	prefix := filepath.ToSlash(currentPath)
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	if !info.IsDir() {
		remote := prefix + filepath.ToSlash(filepath.Base(localPath))
		targets = append(targets, UploadTarget{
			LocalPath:  localPath,
			RemotePath: remote,
			Size:       info.Size(),
		})
		return targets, info.Size(), nil
	}

	err = filepath.Walk(localPath, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(localPath, p)
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
	prefix := filepath.ToSlash(currentPath)
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
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
		return 0, fmt.Errorf("file exists: %s", downloadPath)
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
	}

	n, err := m.Downloader.Download(ctx, writerAt, &s3.GetObjectInput{
		Bucket: bucket,
		Key:    aws.String(t.Key),
	})
	if ctx.Err() != nil {
		_ = os.Remove(downloadPath)
		return 0, ctx.Err()
	}
	return n, err
}
