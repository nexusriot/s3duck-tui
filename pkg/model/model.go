package model

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"

	u "github.com/nexusriot/s3duck-tui/pkg/utils"
)

type FType int8

const (
	File FType = iota
	Folder
	Bucket
)

type Config struct {
	Endpoint  *string
	ForcePath bool
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
	Config  *Config
	Session *session.Session
	Client  *s3.S3
}

func NewModel(config *Config) *Model {
	conf := aws.NewConfig().
		WithRegion("eu-central-1").
		//WithEndpoint("http://127.0.0.1:4566").
		WithS3ForcePathStyle(true)

	sess, err := session.NewSession(conf)
	if err != nil {
		log.Fatalf("failed to create a new aws session: %v", sess)
	}
	m := Model{
		Config:  config,
		Session: sess,
		Client:  s3.New(sess),
	}
	return &m
}

func (m *Model) RefreshClient(bucket *string) {
	// TODO: handle err
	region, _ := m.GetBucketLocation(bucket)
	//endpoint := m.Client.ClientInfo.Endpoint
	conf := aws.NewConfig().
		WithRegion(*region).
		//WithEndpoint(endpoint).
		// TODO: ???
		WithS3ForcePathStyle(true)
	sess, _ := session.NewSession(conf)
	m.Session = sess
	m.Client = s3.New(m.Session)
}

func (m *Model) GetBucketLocation(name *string) (*string, error) {
	bl, err := m.Client.GetBucketLocation(
		&s3.GetBucketLocationInput{
			Bucket: name,
		},
	)
	location := bl.LocationConstraint
	if location == nil {
		loc := "us-east-1"
		location = &loc
	}
	return location, err
}

func (m *Model) List(path string, bucket *Object) ([]*Object, error) {
	var appKey string
	objs := make([]*Object, 0)
	ctx := context.Background()
	// list files under `blog` directory in `work-with-s3` bucket.
	if err := m.Client.ListObjectsPagesWithContext(ctx, &s3.ListObjectsInput{
		Bucket:    aws.String(*bucket.Key),
		Prefix:    aws.String(path), // list files in the directory.
		Delimiter: aws.String("/"),
	}, func(o *s3.ListObjectsOutput, b bool) bool { // callback func to enable paging.
		for _, p := range o.CommonPrefixes {
			if p.Prefix == nil {
				continue
			}
			fields := strings.FieldsFunc(strings.TrimSpace(*p.Prefix), u.SplitFunc)
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
		for _, o := range o.Contents {
			if *o.Key == path {
				continue
			}
			fields := strings.FieldsFunc(strings.TrimSpace(*o.Key), u.SplitFunc)
			appKey := fields[len(fields)-1]
			ko := &Object{
				&appKey,
				File,
				o.ETag,
				o.Size,
				o.StorageClass,
				o.LastModified,
				o.Key,
			}
			objs = append(objs, ko)
		}
		return true
	}); err != nil {

		log.Fatalf("failed to list items in s3 directory: %v", err)
	}
	return objs, nil
}

func (m *Model) ListBuckets() ([]*Object, error) {
	objs := make([]*Object, 0)
	result, err := m.Client.ListBuckets(nil)
	for _, b := range result.Buckets {
		sv := aws.StringValue(b.Name)
		td := aws.TimeValue(b.CreationDate)
		ko := &Object{
			&sv,
			Bucket,
			nil,
			nil,
			nil,
			&td,
			nil,
		}
		objs = append(objs, ko)
	}
	return objs, err
}
