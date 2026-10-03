// Demonstrate versions, conditional writes, metadata, tags and presigned HTTP.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/adrianliechti/s3-gateway/examples/internal/example"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func main() {
	o := example.Flags()
	example.Main(o, func(ctx context.Context) error { return run(ctx, *o) })
}

func run(ctx context.Context, o example.Options) error {
	c, err := o.Client()
	if err != nil {
		return err
	}
	b, cleanup, err := example.NewBucket(ctx, c, "example-objects")
	if err != nil {
		return err
	}
	defer cleanup()
	_, err = c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: &b, VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}})
	if err != nil {
		return err
	}
	key := "reports/a space+plus.json"
	first, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: &b, Key: &key, Body: strings.NewReader(`{"revision":1}`), ContentType: aws.String("application/json"), Metadata: map[string]string{"author": "example"}, Tagging: aws.String("team=analytics"), IfNoneMatch: aws.String("*"), ChecksumAlgorithm: types.ChecksumAlgorithmSha256})
	if err != nil {
		return err
	}
	second, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: &b, Key: &key, Body: strings.NewReader(`{"revision":2}`), ContentType: aws.String("application/json"), IfMatch: first.ETag, ChecksumAlgorithm: types.ChecksumAlgorithmSha256})
	if err != nil {
		return err
	}
	old, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: &b, Key: &key, VersionId: first.VersionId, ChecksumMode: types.ChecksumModeEnabled})
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(old.Body)
	old.Body.Close()
	if err != nil {
		return err
	}
	if string(raw) != `{"revision":1}` {
		return fmt.Errorf("historical contents differ")
	}
	fmt.Printf("Created versions %s and %s; first version still reads %s\n", aws.ToString(first.VersionId), aws.ToString(second.VersionId), raw)

	presign := s3.NewPresignClient(c)
	get, err := presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &b, Key: &key}, s3.WithPresignExpires(5*time.Minute))
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, get.Method, get.URL, nil)
	if err != nil {
		return err
	}
	request.Header = get.SignedHeader.Clone()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	raw, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return err
	}
	if response.StatusCode != 200 || string(raw) != `{"revision":2}` {
		return fmt.Errorf("presigned GET failed: HTTP %d", response.StatusCode)
	}
	fmt.Printf("Presigned GET returned %s using an ordinary HTTP client\n", raw)

	// A browser or another client only needs this URL and its signed headers.
	put, err := presign.PresignPutObject(ctx, &s3.PutObjectInput{Bucket: &b, Key: aws.String("direct-upload.txt"), ContentType: aws.String("text/plain")}, s3.WithPresignExpires(5*time.Minute))
	if err != nil {
		return err
	}
	request, err = http.NewRequestWithContext(ctx, put.Method, put.URL, strings.NewReader("uploaded with a presigned URL"))
	if err != nil {
		return err
	}
	request.Header = put.SignedHeader.Clone()
	request.Header.Set("Content-Type", "text/plain")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("presigned PUT failed: HTTP %d", response.StatusCode)
	}
	fmt.Println("Presigned PUT succeeded; cleaning up demo objects and versions")
	return nil
}
