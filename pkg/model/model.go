package model

import (
	"context"
	"fmt"
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
}

func GetDownloader(client *s3.Client) *s3m.Downloader {
	d := s3m.NewDownloader(client, func(d *s3m.Downloader) {
		d.BufferProvider = s3m.NewPooledBufferedWriterReadFromProvider(5 * 1024 * 1024)
	})
	return d
}

func NewModel() *Model {
	//conf := aws.NewConfig().
	//	WithRegion("eu-central-1").
	//	//WithEndpoint("http://127.0.0.1:4566").
	//	WithS3ForcePathStyle(true)

	opts := []optsFunc{
		config.WithRegion("eu-central-1"),
	}

	cfg, err := config.LoadDefaultConfig(context.TODO(), opts...)
	if err != nil {
		panic(err)
	}

	client := s3.NewFromConfig(cfg)

	m := Model{
		Config:     &cfg,
		Client:     client,
		Downloader: GetDownloader(client),
	}
	return &m
}

func (m *Model) RefreshClient(bucket *string) {
	// TODO: handle err
	region, _ := m.GetBucketLocation(bucket)
	//endpoint := m.Client.ClientInfo.Endpoint
	opts := []optsFunc{
		config.WithRegion(*region),
	}
	cfg, err := config.LoadDefaultConfig(context.TODO(), opts...)
	if err != nil {
		panic(err)
	}
	//conf := aws.NewConfig().
	//	WithRegion(*region).
	//WithEndpoint(endpoint).
	// TODO: ???
	//WithS3ForcePathStyle(true)

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
