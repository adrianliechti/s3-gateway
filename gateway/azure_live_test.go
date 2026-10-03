package gateway_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
	"github.com/adrianliechti/s3-gateway/backend/azure"
	"github.com/adrianliechti/s3-gateway/gateway"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Opt-in: creates and deletes exactly one uniquely named Azure container.
func TestAzureLive(t *testing.T) {
	if os.Getenv("S3_TEST_AZURE") != "1" {
		t.Skip("set S3_TEST_AZURE=1 with Azure credentials to run the live test")
	}
	be, err := azure.New(azure.Options{Account: os.Getenv("AZURE_STORAGE_ACCOUNT"), AccountKey: os.Getenv("AZURE_STORAGE_KEY"), ServiceURL: os.Getenv("AZURE_STORAGE_SERVICE_URL"), SASToken: os.Getenv("AZURE_STORAGE_SAS_TOKEN")})
	must(t, err)
	defer be.Close()
	g, err := gateway.New(be, gateway.Options{AccessKey: access, SecretKey: secret})
	must(t, err)
	c := client(&transport{handler: g})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	bucket := fmt.Sprintf("s3gw-test-%d", time.Now().UnixNano())
	_, err = c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bucket})
	must(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		pages := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{Bucket: &bucket})
		for pages.HasMorePages() {
			p, e := pages.NextPage(ctx)
			if e != nil {
				t.Error(e)
				return
			}
			var objects []types.ObjectIdentifier
			for _, o := range p.Contents {
				objects = append(objects, types.ObjectIdentifier{Key: o.Key})
			}
			if len(objects) > 0 {
				_, e = c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &bucket, Delete: &types.Delete{Objects: objects}})
				if e != nil {
					t.Error(e)
					return
				}
			}
		}
		_, e := c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &bucket})
		if e != nil {
			t.Error(e)
		}
	})
	key := "table/a space+雪.parquet"
	value := "PAR1 native azure bytes PAR1"
	result, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: &bucket, Key: &key, Body: strings.NewReader(value), Metadata: map[string]string{"format": "parquet"}})
	must(t, err)
	native, body, err := be.Get(ctx, bucket, key, backend.ReadOptions{Length: -1})
	must(t, err)
	content, err := io.ReadAll(body)
	body.Close()
	must(t, err)
	if string(content) != value || native.Metadata["format"] != "parquet" {
		t.Fatal("native blob representation differs")
	}
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: &bucket, Key: &key, Body: strings.NewReader("replace"), IfNoneMatch: aws.String("*")})
	code(t, err, "PreconditionFailed")
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: &bucket, Key: &key, Body: strings.NewReader(value), IfMatch: result.ETag})
	must(t, err)
	output, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &key, Range: aws.String("bytes=-4")})
	must(t, err)
	tail, err := io.ReadAll(output.Body)
	output.Body.Close()
	must(t, err)
	if string(tail) != "PAR1" {
		t.Fatal("Azure range read differs")
	}
}
