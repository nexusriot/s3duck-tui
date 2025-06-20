package model

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	s3m "github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3t "github.com/aws/aws-sdk-go-v2/service/s3/types"

	u "github.com/nexusriot/s3duck-tui/pkg/utils"
)

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
	pwa.written += int64(n)
	pwa.updateFunc(pwa.written, pwa.total)
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

		// TODO: optionally disable ssl

		if cf.Region != nil {
			endpoint = aws.Endpoint{
				//PartitionID:   "aws",
				URL:               cf.Url,
				SigningRegion:     *cf.Region,
				HostnameImmutable: true,
			}
		} else {
			endpoint = aws.Endpoint{
				//PartitionID: "aws",
				URL:               cf.Url,
				HostnameImmutable: true,
			}
		}
		return endpoint, nil
	})

	staticProvider := credentials.NewStaticCredentialsProvider(
		cf.AccessKey,
		cf.SecretKey,
		"",
	)

	var opts []optsFunc
	// TODO: check usage
	if update && strings.Contains(cf.Url, "amazon") {
		opts = []optsFunc{
			config.WithRegion(*cf.Region),
		}
	} else {
		opts = []optsFunc{
			config.WithEndpointResolverWithOptions(customResolver),
		}
	}

	opts = append(opts, config.WithCredentialsProvider(staticProvider))
	if !cf.SSl {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		client := &http.Client{Transport: tr}
		opts = append(opts, config.WithHTTPClient(client))
	}

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
func (m *Model) ListObjects(key string, bucket *Object) []s3t.Object {
	var objects []s3t.Object

	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(*bucket.Key),
		Prefix: aws.String(key),
	}

	paginator := s3.NewListObjectsV2Paginator(m.Client, input)
	for paginator.HasMorePages() {
		output, err := paginator.NextPage(context.TODO())
		if err != nil {
			panic(err)
		}

		objects = append(objects, output.Contents...)
	}
	return objects
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
		return 0, fmt.Errorf("file exists: %s", downloadPath)
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
			panic(err)
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
				//o.StorageClass,
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
			// Add other fields if necessary
		}
		objs = append(objs, ko)
	}
	return objs, nil
}

func (m *Model) Delete(key *string, bucket *Object) error {

	var objectIds []s3t.ObjectIdentifier

	if strings.HasSuffix(*key, "/") {
		ks := m.ListObjects(*key, bucket)
		for _, o := range ks {
			objectIds = append(objectIds, s3t.ObjectIdentifier{Key: aws.String(*o.Key)})
		}
	} else {
		objectIds = append(objectIds, s3t.ObjectIdentifier{Key: aws.String(*key)})
	}
	_, err := m.Client.DeleteObjects(context.TODO(), &s3.DeleteObjectsInput{
		Bucket: aws.String(*bucket.Key),
		Delete: &s3t.Delete{Objects: objectIds},
	})
	return err
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
	var totalSize int64

	if isDir {
		err := filepath.Walk(localPath, func(path string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() {
				return nil
			}
			files = append(files, path)
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

	uploader := s3m.NewUploader(m.Client)
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

		baseFolder := filepath.Base(localPath)
		relPath := fpath
		if isDir {
			relPath, _ = filepath.Rel(localPath, fpath)
		}
		relPath = filepath.ToSlash(relPath)
		s3Key := path.Join(s3Prefix, baseFolder, relPath)

		pr := &progressReader{
			r:     fp,
			total: stat.Size(),
			update: func(written, _ int64) {
				if progressCb != nil {
					progressCb(uploadedTotal+written, totalSize, i+1, len(files), fpath, s3Key)
				}
			},
		}

		_, err = uploader.Upload(ctx, &s3.PutObjectInput{
			Bucket: aws.String(*bucket.Key),
			Key:    aws.String(s3Key),
			Body:   pr,
		})

		fp.Close()

		if err != nil {
			return fmt.Errorf("upload failed for %s: %w", fpath, err)
		}

		uploadedTotal += stat.Size()
	}

	return nil
}

// PrepareUpload returns list of files to upload with remote keys and total size.
func (m *Model) PrepareUpload(localPath string, currentPath string, bucket *Object) ([]UploadTarget, int64, error) {
	var targets []UploadTarget
	var totalSize int64

	err := filepath.Walk(localPath, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err // skip unreadable file
		}
		if info.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(localPath, p)
		if err != nil {
			return err
		}

		// S3 expects forward slashes
		remotePath := filepath.ToSlash(filepath.Join(currentPath, relPath))

		targets = append(targets, UploadTarget{
			LocalPath:  p,
			RemotePath: remotePath,
			Size:       info.Size(),
		})
		totalSize += info.Size()
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
			"Effect":"Allow",
			 "Principal": {
			 "AWS": "*"
			 },
			"Action":"s3:GetObject",
			"Resource":["arn:aws:s3:::%s"],
			"Condition": {}
		}]
	}`, bucketName)

	_, err := m.Client.PutBucketPolicy(context.TODO(), &s3.PutBucketPolicyInput{
		Bucket: aws.String(bucketName),
		Policy: aws.String(policy),
	})
	return err
}
