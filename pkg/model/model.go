package model

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
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

type Config struct {
	Url       string
	Region    *string
	AccessKey string
	SecretKey string
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

func GetDownloader(client *s3.Client) *s3m.Downloader {
	d := s3m.NewDownloader(client, func(d *s3m.Downloader) {
		d.BufferProvider = s3m.NewPooledBufferedWriterReadFromProvider(5 * 1024 * 1024)
	})
	return d
}

func NewConfig(url string, region *string, accKey string, secKey string) Config {
	return Config{
		url,
		region,
		accKey,
		secKey,
	}
}

// GetConfig  ...
func GetConfig(cf Config, update bool) (aws.Config, error) {
	customResolver := aws.EndpointResolverWithOptionsFunc(func(service, region string, options ...interface{}) (aws.Endpoint, error) {
		var endpoint aws.Endpoint

		// TODO: optionally disable ssl

		if cf.Region != nil {
			endpoint = aws.Endpoint{
				// TODO: check usage
				//PartitionID:   "aws",
				URL:               cf.Url,
				SigningRegion:     *cf.Region,
				HostnameImmutable: true,
			}
		} else {
			endpoint = aws.Endpoint{
				// TODO: check usage
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
	//opts := []optsFunc{
	//	config.WithEndpointResolverWithOptions(customResolver),
	//	config.WithCredentialsProvider(staticProvider),
	//}

	var opts []optsFunc
	// TODO: check usage
	if update && strings.Contains(cf.Url, "amazon") {
		opts = []optsFunc{
			config.WithRegion(*cf.Region),
			config.WithCredentialsProvider(staticProvider),
		}
	} else {
		opts = []optsFunc{
			config.WithEndpointResolverWithOptions(customResolver),
			config.WithCredentialsProvider(staticProvider),
		}
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

func (m *Model) Download(object s3t.Object, currentPath string, destPath string, bucket *string) (n int64, err error) {
	if err = os.MkdirAll(filepath.Dir(destPath), 0700); err != nil {
		return 0, err
	}

	p, _ := filepath.Rel(currentPath, *object.Key)
	dp := path.Join(destPath, p)

	if strings.HasSuffix(*object.Key, "/") {
		err = os.MkdirAll(dp, 0760)
		return 0, nil
	}
	dir := filepath.Dir(dp)
	err = os.MkdirAll(dir, 0760)

	_, err = os.Stat(dp)
	if err == nil {
		return 0, fmt.Errorf("exists")
	}

	fp, err := os.Create(dp)
	if err != nil {
		return 0, err
	}

	defer func() {
		if err := fp.Close(); err != nil {
			panic(err)
		}
	}()

	return m.Downloader.Download(
		context.TODO(),
		fp,
		&s3.GetObjectInput{
			Bucket: aws.String(*bucket),
			Key:    object.Key,
		},
	)
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
	objs := make([]*Object, 0)
	result, err := m.Client.ListBuckets(context.TODO(), &s3.ListBucketsInput{})
	if err != nil {
		return nil, err
	}
	for _, b := range result.Buckets {
		sv := aws.String(*b.Name)
		td := aws.Time(*b.CreationDate)
		ko := &Object{
			sv,
			Bucket,
			nil,
			nil,
			nil,
			td,
			nil,
		}
		objs = append(objs, ko)
	}
	return objs, err
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
