package model

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
)

type FType int8

const (
	File FType = iota
	Folder
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
}

type Model struct {
	Path    *string
	Bucket  *string
	Config  *Config
	Objects []*Object
	Current *s3.Object
	Session *session.Session
	Client  *s3.S3
}

func splitFunc(r rune) bool {
	return r == '/'
}

func NewModel(config *Config) *Model {
	sess, _ := session.NewSession(&aws.Config{
		Region: aws.String("us-west-2")},
	)

	conf := aws.NewConfig().
		WithRegion("eu-central-1").
		//WithEndpoint("http://127.0.0.1:4566").
		WithS3ForcePathStyle(true)

	sess, err := session.NewSession(conf)
	if err != nil {
		log.Fatalf("failed to create a new aws session: %v", sess)
	}
	path := ""
	m := Model{
		Path:    &path,
		Bucket:  nil,
		Config:  config,
		Objects: make([]*Object, 0),
		Current: nil,
		Session: sess,
		Client:  s3.New(sess),
	}
	return &m
}

func (m *Model) List(path string) ([]*Object, error) {
	objs := make([]*Object, 0)
	ctx := context.Background()
	// list files under `blog` directory in `work-with-s3` bucket.
	if err := m.Client.ListObjectsPagesWithContext(ctx, &s3.ListObjectsInput{
		Bucket:    aws.String(*m.Bucket),
		Prefix:    aws.String(path), // list files in the directory.
		Delimiter: aws.String("/"),
	}, func(o *s3.ListObjectsOutput, b bool) bool { // callback func to enable paging.
		for _, p := range o.CommonPrefixes {
			if p.Prefix == nil {
				continue
			}
			name := strings.TrimSuffix(*p.Prefix, "/")
			ko := &Object{
				&name,
				Folder,
				nil,
				nil,
				nil,
				nil,
			}
			objs = append(objs, ko)
		}
		for _, o := range o.Contents {
			if *o.Key == path {
				m.Current = o
				continue
			}
			fields := strings.FieldsFunc(strings.TrimSpace(*o.Key), splitFunc)
			fmt.Println(fields)
			ko := &Object{
				&fields[1],
				File,
				o.ETag,
				o.Size,
				o.StorageClass,
				o.LastModified,
			}
			objs = append(objs, ko)
		}
		return true
	}); err != nil {

		log.Fatalf("failed to list items in s3 directory: %v", err)
	}
	return objs, nil
}

//func main() {
// Initialize a session in us-west-2 that the SDK will use to load
// credentials from the shared credentials file ~/.aws/credentials.
//sess, _ := session.NewSession(&aws.Config{
//	Region: aws.String("us-west-2")},
//)

// Create S3 service client
//svc := s3.New(sess)

//result, err := svc.ListBuckets(nil)
//if err != nil {
//	exitErrorf("Unable to list buckets, %v", err)
//}
//
//fmt.Println("Buckets:")
//
//for _, b := range result.Buckets {
//	fmt.Printf("* %s created on %s\n",
//		aws.StringValue(b.Name), aws.TimeValue(b.CreationDate))
//}
//params := &s3.ListObjectsInput{
//	Bucket: aws.String("vlad-bucket-1"),
//	//Prefix: aws.String(""),
//}
//	params := &s3.ListObjectsInput{
//		Bucket: aws.String("vlad-bucket-1"),
//	}
//
//	resp, _ := svc.ListObjects(params)
//	for _, key := range resp.Contents {
//		fmt.Println(*key.Key)
//	}
//
//}
//
//func exitErrorf(msg string, args ...interface{}) {
//	fmt.Fprintf(os.Stderr, msg+"\n", args...)
//	os.Exit(1)
//}

//	ctx := context.Background()
//	bucket := "vlad-bucket-1"
//
//	conf := aws.NewConfig().
//		WithRegion("eu-central-1").
//		//WithEndpoint("http://127.0.0.1:4566").
//		WithS3ForcePathStyle(true)
//
//	sess, err := session.NewSession(conf)
//	if err != nil {
//		log.Fatalf("failed to create a new aws session: %v", sess)
//	}
//	s3client := s3.New(sess)
//
//	objs := make([]*Object, 0)
//
//	path := "temp/"
//	st := Model{}
//
//	// list files under `blog` directory in `work-with-s3` bucket.
//	if err := s3client.ListObjectsPagesWithContext(ctx, &s3.ListObjectsInput{
//		Bucket:    aws.String(bucket),
//		Prefix:    aws.String(path), // list files in the directory.
//		Delimiter: aws.String("/"),
//	}, func(o *s3.ListObjectsOutput, b bool) bool { // callback func to enable paging.
//		for _, p := range o.CommonPrefixes {
//			if p.Prefix == nil {
//				continue
//			}
//			name := strings.TrimSuffix(*p.Prefix, "/")
//			ko := &Object{
//				&name,
//				Folder,
//				nil,
//				nil,
//				nil,
//				nil,
//			}
//			st.Objects = append(st.Objects, ko)
//			objs = append(objs, ko)
//		}
//		for _, o := range o.Contents {
//			if *o.Key == path {
//				st.Current = o
//				continue
//			}
//			fields := strings.FieldsFunc(strings.TrimSpace(*o.Key), splitFunc)
//			fmt.Println(fields)
//			ko := &Object{
//				&fields[1],
//				File,
//				o.ETag,
//				o.Size,
//				o.StorageClass,
//				o.LastModified,
//			}
//			st.Objects = append(st.Objects, ko)
//			objs = append(objs, ko)
//		}
//		return true
//	}); err != nil {
//		log.Fatalf("failed to list items in s3 directory: %v", err)
//	}
//
//	for _, k := range objs {
//		log.Printf("file: %s", *k.Key)
//	}
//}
