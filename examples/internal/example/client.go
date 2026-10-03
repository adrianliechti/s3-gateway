// Package example shares connection and cleanup code between runnable demos.
package example

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type Options struct {
	Endpoint, Region string
	Timeout          time.Duration
}

func Flags() *Options {
	o := &Options{}
	flag.StringVar(&o.Endpoint, "endpoint", Env("GATEWAY_ENDPOINT", "http://127.0.0.1:9000"), "S3 gateway endpoint")
	flag.StringVar(&o.Region, "region", Env("GATEWAY_REGION", "us-east-1"), "gateway signing region")
	flag.DurationVar(&o.Timeout, "timeout", 2*time.Minute, "overall example timeout")
	return o
}

func Env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func Endpoint(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("endpoint must be an HTTP(S) URL without credentials, query or fragment")
	}
	return nil
}

func (o Options) Client() (*s3.Client, error) {
	if err := Endpoint(o.Endpoint); err != nil {
		return nil, err
	}
	access, secret := os.Getenv("GATEWAY_ACCESS_KEY"), os.Getenv("GATEWAY_SECRET_KEY")
	if access == "" || secret == "" {
		return nil, fmt.Errorf("set GATEWAY_ACCESS_KEY and GATEWAY_SECRET_KEY to the gateway's client credentials")
	}
	// Do not use AWS_* here: those may select the gateway's upstream storage.
	return s3.New(s3.Options{Region: o.Region, BaseEndpoint: &o.Endpoint, UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(access, secret, ""),
		HTTPClient:  &http.Client{Timeout: o.Timeout}}), nil
}

func Main(o *Options, run func(context.Context) error) {
	flag.Parse()
	if o.Timeout <= 0 {
		log.Fatal("timeout must be positive")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func Name(prefix string) string {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		panic(err)
	}
	return prefix + "-" + hex.EncodeToString(nonce[:])
}

// NewBucket owns a randomly named demo bucket, never an existing user bucket.
// Cleanup removes versions and delete markers as well as ordinary objects.
func NewBucket(ctx context.Context, c *s3.Client, prefix string) (string, func(), error) {
	b := Name(prefix)
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &b}); err != nil {
		return "", nil, err
	}
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for {
			page, err := c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: &b})
			if err != nil {
				log.Printf("Cleanup %s: %v", b, err)
				return
			}
			var objects []types.ObjectIdentifier
			for _, v := range page.Versions {
				objects = append(objects, types.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
			}
			for _, v := range page.DeleteMarkers {
				objects = append(objects, types.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
			}
			if len(objects) == 0 {
				break
			}
			out, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &b, Delete: &types.Delete{Objects: objects}})
			if err != nil {
				log.Printf("Cleanup %s: %v", b, err)
				return
			}
			if len(out.Errors) > 0 {
				log.Printf("Cleanup %s: %s", b, aws.ToString(out.Errors[0].Message))
				return
			}
		}
		if _, err := c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &b}); err != nil {
			log.Printf("Cleanup %s: %v", b, err)
		}
	}
	return b, cleanup, nil
}
