package compatibility_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func setup(t *testing.T) (*minio.Client, string) {
	t.Helper()
	u, err := url.Parse(os.Getenv("GATEWAY_ENDPOINT"))
	must(t, err)
	if u.Host == "" || os.Getenv("GATEWAY_ACCESS_KEY") == "" || os.Getenv("GATEWAY_SECRET_KEY") == "" {
		t.Fatal("S3 endpoint and credentials are required")
	}
	c, err := minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4(os.Getenv("GATEWAY_ACCESS_KEY"), os.Getenv("GATEWAY_SECRET_KEY"), ""), Secure: u.Scheme == "https", BucketLookup: minio.BucketLookupPath, TrailingHeaders: true})
	must(t, err)
	for attempt := 0; attempt < 30; attempt++ {
		_, err = c.ListBuckets(t.Context())
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	must(t, err)
	bucket := fmt.Sprintf("minio-test-%d", time.Now().UnixNano())
	must(t, c.MakeBucket(t.Context(), bucket, minio.MakeBucketOptions{Region: "us-east-1"}))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for obj := range c.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
			if obj.Err != nil {
				t.Error(obj.Err)
				return
			}
			if err := c.RemoveObject(ctx, bucket, obj.Key, minio.RemoveObjectOptions{}); err != nil {
				t.Error(err)
			}
		}
		if err := c.RemoveBucket(ctx, bucket); err != nil {
			t.Error(err)
		}
	})
	return c, bucket
}

func contents(t *testing.T, c *minio.Client, bucket, key string) []byte {
	t.Helper()
	obj, err := c.GetObject(t.Context(), bucket, key, minio.GetObjectOptions{})
	must(t, err)
	defer obj.Close()
	data, err := io.ReadAll(obj)
	must(t, err)
	return data
}

func TestObjectOperations(t *testing.T) {
	c, bucket := setup(t)
	exists, err := c.BucketExists(t.Context(), bucket)
	must(t, err)
	if !exists {
		t.Fatal("bucket is absent")
	}
	versioning, err := c.GetBucketVersioning(t.Context(), bucket)
	must(t, err)
	if versioning.Status != "" {
		t.Fatal("expected an unversioned bucket")
	}
	key, data := "table/a space+%雪.parquet", []byte("PAR1 parquet-like bytes PAR1")
	info, err := c.PutObject(t.Context(), bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: "application/parquet", UserMetadata: map[string]string{"table": "events"}})
	must(t, err)
	stat, err := c.StatObject(t.Context(), bucket, key, minio.StatObjectOptions{Checksum: true})
	must(t, err)
	if info.ETag != stat.ETag || stat.Size != int64(len(data)) || stat.UserMetadata["Table"] != "events" && stat.UserMetadata["table"] != "events" {
		t.Fatalf("stat differs: %#v", stat)
	}
	if !bytes.Equal(contents(t, c, bucket, key), data) {
		t.Fatal("object differs")
	}
	obj, err := c.GetObject(t.Context(), bucket, key, minio.GetObjectOptions{})
	must(t, err)
	tail := make([]byte, 4)
	_, err = obj.ReadAt(tail, int64(len(data)-4))
	must(t, err)
	must(t, obj.Close())
	if string(tail) != "PAR1" {
		t.Fatal("range differs")
	}
	_, err = c.CopyObject(t.Context(), minio.CopyDestOptions{Bucket: bucket, Object: "copy"}, minio.CopySrcOptions{Bucket: bucket, Object: key})
	must(t, err)
	if !bytes.Equal(contents(t, c, bucket, "copy"), data) {
		t.Fatal("copy differs")
	}
	conditional := minio.PutObjectOptions{}
	conditional.SetMatchETagExcept("*")
	_, err = c.PutObject(t.Context(), bucket, key, strings.NewReader("bad"), 3, conditional)
	if minio.ToErrorResponse(err).Code != "PreconditionFailed" {
		t.Fatalf("conditional overwrite: %v", err)
	}
	for _, v1 := range []bool{false, true} {
		count := 0
		for o := range c.ListObjects(t.Context(), bucket, minio.ListObjectsOptions{Recursive: true, UseV1: v1, MaxKeys: 1}) {
			must(t, o.Err)
			count++
		}
		if count != 2 {
			t.Fatalf("listing count %d", count)
		}
	}
	objects := make(chan minio.ObjectInfo, 2)
	objects <- minio.ObjectInfo{Key: key}
	objects <- minio.ObjectInfo{Key: "copy"}
	close(objects)
	for e := range c.RemoveObjects(t.Context(), bucket, objects, minio.RemoveObjectsOptions{}) {
		must(t, e.Err)
	}
}

func TestMultipartChecksumsAndUnknownSize(t *testing.T) {
	c, bucket := setup(t)
	data := bytes.Repeat([]byte("0123456789abcdef"), (6<<20)/16)
	for _, unknown := range []bool{false, true} {
		key := fmt.Sprintf("multipart-%v", unknown)
		size := int64(len(data))
		if unknown {
			size = -1
		}
		info, err := c.PutObject(t.Context(), bucket, key, bytes.NewReader(data), size, minio.PutObjectOptions{PartSize: 5 << 20, Checksum: minio.ChecksumSHA256})
		must(t, err)
		if !strings.HasSuffix(info.ETag, "-2") || !strings.HasSuffix(info.ChecksumSHA256, "-2") {
			t.Fatalf("multipart response omitted checksum: %#v", info)
		}
		if !bytes.Equal(contents(t, c, bucket, key), data) {
			t.Fatal("multipart bytes differ")
		}
		stat, err := c.StatObject(t.Context(), bucket, key, minio.StatObjectOptions{Checksum: true})
		must(t, err)
		if stat.ChecksumSHA256 != info.ChecksumSHA256 {
			t.Fatal("persisted checksum differs")
		}
	}
}

func TestIncompleteUploadsAndCompose(t *testing.T) {
	c, bucket := setup(t)
	core := minio.Core{Client: c}
	for _, key := range []string{"dir/a", "dir/b", "root"} {
		_, err := core.NewMultipartUpload(t.Context(), bucket, key, minio.PutObjectOptions{})
		must(t, err)
	}
	count := 0
	for upload := range c.ListIncompleteUploads(t.Context(), bucket, "", false) {
		must(t, upload.Err)
		count++
	}
	if count != 2 {
		t.Fatalf("expected grouped prefix and root upload, got %d", count)
	}
	must(t, c.RemoveIncompleteUpload(t.Context(), bucket, "dir/a"))
	for _, key := range []string{"source1", "source2"} {
		_, err := c.PutObject(t.Context(), bucket, key, bytes.NewReader(bytes.Repeat([]byte(key[len(key)-1:]), 5<<20)), 5<<20, minio.PutObjectOptions{})
		must(t, err)
	}
	result, err := c.ComposeObject(t.Context(), minio.CopyDestOptions{Bucket: bucket, Object: "composed", ChecksumType: minio.ChecksumSHA256}, minio.CopySrcOptions{Bucket: bucket, Object: "source1"}, minio.CopySrcOptions{Bucket: bucket, Object: "source2"})
	must(t, err)
	data := append(bytes.Repeat([]byte("1"), 5<<20), bytes.Repeat([]byte("2"), 5<<20)...)
	if !bytes.Equal(contents(t, c, bucket, "composed"), data) || !strings.HasSuffix(result.ChecksumSHA256, "-2") {
		t.Fatal("composition bytes or checksum differ")
	}
}

func TestPresignedRequests(t *testing.T) {
	c, bucket := setup(t)
	key := "signed space+雪"
	putURL, err := c.PresignedPutObject(t.Context(), bucket, key, time.Minute)
	must(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, putURL.String(), strings.NewReader("signed data"))
	must(t, err)
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("presigned put status: %d", resp.StatusCode)
	}
	getURL, err := c.PresignedGetObject(t.Context(), bucket, key, time.Minute, nil)
	must(t, err)
	resp, err = http.Get(getURL.String())
	must(t, err)
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	must(t, err)
	if resp.StatusCode != 200 || string(data) != "signed data" {
		t.Fatalf("presigned get status: %d", resp.StatusCode)
	}
	headURL, err := c.PresignedHeadObject(t.Context(), bucket, key, time.Minute, nil)
	must(t, err)
	resp, err = http.Head(headURL.String())
	must(t, err)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.ContentLength != 11 {
		t.Fatal("presigned head differs")
	}
}

func TestCopyPartChecksumResponse(t *testing.T) {
	c, bucket := setup(t)
	const data = "copy checksum"
	_, err := c.PutObject(t.Context(), bucket, "source", strings.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
	must(t, err)
	core := minio.Core{Client: c}
	uid, err := core.NewMultipartUpload(t.Context(), bucket, "dest", minio.PutObjectOptions{UserMetadata: map[string]string{"x-amz-checksum-algorithm": "SHA256"}})
	must(t, err)
	p, err := core.CopyObjectPart(t.Context(), bucket, "source", bucket, "dest", uid, 1, 0, int64(len(data)), nil)
	must(t, err)
	digest := sha256.Sum256([]byte(data))
	if p.ChecksumSHA256 != base64.StdEncoding.EncodeToString(digest[:]) {
		t.Fatal("copy part checksum missing")
	}
	parts, err := core.ListObjectParts(t.Context(), bucket, "dest", uid, 0, 1000)
	must(t, err)
	if len(parts.ObjectParts) != 1 || parts.ObjectParts[0].ChecksumSHA256 != p.ChecksumSHA256 {
		t.Fatal("list parts checksum missing")
	}
	_, err = core.CompleteMultipartUpload(t.Context(), bucket, "dest", uid, []minio.CompletePart{p}, minio.PutObjectOptions{})
	must(t, err)
	if string(contents(t, c, bucket, "dest")) != data {
		t.Fatal("copy differs")
	}
}
