// Command s3dev is the S3 client for local development and the end-to-end
// test, against the local RustFS. It is not part of the builder image.
//
//	s3dev ensure-bucket   create S3_BUCKET unless it exists, waiting for the store
//	s3dev put KEY BODY    store BODY at KEY
//	s3dev keys            print every key in the bucket, one per line
//
// A real deployment provisions its bucket out of band, with a lifecycle
// policy and access controls this tool has no business inventing.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const (
	// RustFS has just been started by compose, so expect to lose the first
	// few attempts.
	attempts = 30
	delay    = 2 * time.Second
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	bucket := os.Getenv("S3_BUCKET")
	if bucket == "" {
		return errors.New("S3_BUCKET is not set")
	}
	client := newClient()

	switch {
	case len(args) == 1 && args[0] == "ensure-bucket":
		return ensureBucket(ctx, client, bucket)
	case len(args) == 3 && args[0] == "put":
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(args[1]),
			Body:   strings.NewReader(args[2]),
		})
		return err
	case len(args) == 1 && args[0] == "keys":
		paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
		for paginator.HasMorePages() {
			page, err := paginator.NextPage(ctx)
			if err != nil {
				return err
			}
			for _, object := range page.Contents {
				fmt.Println(aws.ToString(object.Key))
			}
		}
		return nil
	default:
		return errors.New("usage: s3dev ensure-bucket | put KEY BODY | keys")
	}
}

// newClient reads S3_ENDPOINT, S3_REGION, S3_ACCESS_KEY and S3_SECRET_KEY,
// the variables Caddy's storage is configured with.
func newClient() *s3.Client {
	region := os.Getenv("S3_REGION")
	if region == "" {
		region = "us-east-1"
	}
	options := s3.Options{
		Region:       region,
		UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(
			os.Getenv("S3_ACCESS_KEY"), os.Getenv("S3_SECRET_KEY"), ""),
	}
	if endpoint := os.Getenv("S3_ENDPOINT"); endpoint != "" {
		options.BaseEndpoint = aws.String(endpoint)
	}
	return s3.New(options)
}

func ensureBucket(ctx context.Context, client *s3.Client, bucket string) error {
	for attempt := 1; attempt <= attempts; attempt++ {
		_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})

		var owned *types.BucketAlreadyOwnedByYou
		var exists *types.BucketAlreadyExists
		switch {
		case err == nil:
			fmt.Printf("created bucket %s\n", bucket)
			return nil
		case errors.As(err, &owned), errors.As(err, &exists):
			fmt.Printf("bucket %s already exists\n", bucket)
			return nil
		}

		fmt.Fprintf(os.Stderr, "attempt %d/%d: %v\n", attempt, attempts, err)
		time.Sleep(delay)
	}
	return fmt.Errorf("gave up creating bucket %s", bucket)
}
