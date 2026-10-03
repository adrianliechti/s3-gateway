package gateway_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianliechti/s3-gateway/backend/disk"
	"github.com/adrianliechti/s3-gateway/gateway"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

const access = "gateway-test-access"
const secret = "gateway-test-secret"

type transport struct {
	handler http.Handler
	close   func() error
}

func (t *transport) RoundTrip(r *http.Request) (*http.Response, error) {
	req := r.Clone(r.Context())
	req.RequestURI = req.URL.RequestURI()
	if req.Host == "" {
		req.Host = req.URL.Host
	}
	if req.Body == nil {
		req.Body = http.NoBody
	}
	w := httptest.NewRecorder()
	t.handler.ServeHTTP(w, req)
	res := w.Result()
	res.Request = r
	return res, nil
}
func client(tr http.RoundTripper) *s3.Client {
	return s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String("http://gateway.test"), UsePathStyle: true, HTTPClient: &http.Client{Transport: tr}, RetryMaxAttempts: 1, Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: access, SecretAccessKey: secret}, nil
	})})
}
func setup(t *testing.T) (*s3.Client, *transport, string) {
	t.Helper()
	root := t.TempDir()
	be, err := disk.New(disk.Options{Root: root})
	must(t, err)
	t.Cleanup(func() { be.Close() })
	g, err := gateway.New(be, gateway.Options{AccessKey: access, SecretKey: secret})
	must(t, err)
	tr := &transport{handler: g, close: be.Close}
	c := client(tr)
	_, err = c.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String("warehouse")})
	must(t, err)
	return c, tr, root
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func code(t *testing.T, err error, want string) {
	t.Helper()
	var e smithy.APIError
	if !errors.As(err, &e) || e.ErrorCode() != want {
		t.Fatalf("want %s, got %v", want, err)
	}
}
func put(t *testing.T, c *s3.Client, k, stringBody string) *s3.PutObjectOutput {
	t.Helper()
	out, e := c.PutObject(t.Context(), &s3.PutObjectInput{Bucket: aws.String("warehouse"), Key: &k, Body: strings.NewReader(stringBody)})
	must(t, e)
	return out
}
func get(t *testing.T, c *s3.Client, k string) string {
	t.Helper()
	o, e := c.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String("warehouse"), Key: &k})
	must(t, e)
	defer o.Body.Close()
	v, e := io.ReadAll(o.Body)
	must(t, e)
	return string(v)
}

func TestSDKObjectsAndNativeFiles(t *testing.T) {
	c, _, root := setup(t)
	ctx := t.Context()
	key := "tables/a space+percent%/雪.parquet"
	body := "PAR1 some parquet-like bytes PAR1"
	result, e := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("warehouse"), Key: &key, Body: strings.NewReader(body), ContentType: aws.String("application/octet-stream"), Metadata: map[string]string{"table": "events"}, CacheControl: aws.String("max-age=42")})
	must(t, e)
	if get(t, c, key) != body {
		t.Fatal("round trip differs")
	}
	native, e := os.ReadFile(filepath.Join(root, "warehouse", key))
	must(t, e)
	if string(native) != body {
		t.Fatal("native content differs")
	}
	h, e := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("warehouse"), Key: &key})
	must(t, e)
	if h.Metadata["table"] != "events" || aws.ToInt64(h.ContentLength) != int64(len(body)) || aws.ToString(h.ETag) != aws.ToString(result.ETag) {
		t.Fatalf("bad head: %+v", h)
	}
	for _, rng := range []string{"bytes=0-3", "bytes=-4"} {
		o, e := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("warehouse"), Key: &key, Range: &rng})
		must(t, e)
		v, e := io.ReadAll(o.Body)
		o.Body.Close()
		must(t, e)
		if string(v) != "PAR1" {
			t.Fatalf("range %s: %q", rng, v)
		}
	}
	_, e = c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("warehouse"), Key: &key, Range: aws.String("bytes=1000-")})
	code(t, e, "InvalidRange")
	_, e = c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("warehouse"), Key: &key, Body: strings.NewReader("bad"), IfNoneMatch: aws.String("*")})
	code(t, e, "PreconditionFailed")
	_, e = c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("warehouse"), Key: &key, IfMatch: aws.String(`"wrong"`)})
	code(t, e, "PreconditionFailed")
	src := url.PathEscape("warehouse/" + key)
	_, e = c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("warehouse"), Key: aws.String("copy"), CopySource: &src})
	must(t, e)
	if get(t, c, "copy") != body {
		t.Fatal("copy differs")
	}
	must(t, os.WriteFile(filepath.Join(root, "warehouse", "native.txt"), []byte("native file"), 0600))
	if get(t, c, "native.txt") != "native file" {
		t.Fatal("native files not visible")
	}
	put(t, c, "zero", "")
	if get(t, c, "zero") != "" {
		t.Fatal("empty object differs")
	}
	_, e = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("warehouse")})
	code(t, e, "BucketNotEmpty")
}
func TestSDKPaginationAndDelete(t *testing.T) {
	c, _, _ := setup(t)
	for _, k := range []string{"dir/a", "dir/b", "dir2/a", "root"} {
		put(t, c, k, k)
	}
	p := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{Bucket: aws.String("warehouse"), Delimiter: aws.String("/"), MaxKeys: aws.Int32(1)})
	var keys []string
	for p.HasMorePages() {
		o, e := p.NextPage(t.Context())
		must(t, e)
		for _, v := range o.CommonPrefixes {
			keys = append(keys, aws.ToString(v.Prefix))
		}
		for _, v := range o.Contents {
			keys = append(keys, aws.ToString(v.Key))
		}
	}
	if strings.Join(keys, ",") != "dir/,dir2/,root" {
		t.Fatalf("pagination: %v", keys)
	}
	p2 := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{Bucket: aws.String("warehouse"), MaxKeys: aws.Int32(2)})
	var objects []types.ObjectIdentifier
	for p2.HasMorePages() {
		o, e := p2.NextPage(t.Context())
		must(t, e)
		for _, v := range o.Contents {
			objects = append(objects, types.ObjectIdentifier{Key: v.Key})
		}
	}
	o, e := c.DeleteObjects(t.Context(), &s3.DeleteObjectsInput{Bucket: aws.String("warehouse"), Delete: &types.Delete{Objects: objects}})
	must(t, e)
	if len(o.Errors) > 0 || len(o.Deleted) != 4 {
		t.Fatalf("delete result: %+v", o)
	}
	_, e = c.DeleteBucket(t.Context(), &s3.DeleteBucketInput{Bucket: aws.String("warehouse")})
	must(t, e)
}
func TestSDKMultipartRestartAndAbort(t *testing.T) {
	c, tr, root := setup(t)
	ctx := t.Context()
	key := "table/data/file.parquet"
	init, e := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("warehouse"), Key: &key, Metadata: map[string]string{"format": "parquet"}})
	must(t, e)
	if aws.ToString(init.UploadId) == "" {
		t.Fatal("missing upload id")
	}
	body1 := bytes.Repeat([]byte("a"), 5<<20)
	body2 := []byte("tail")
	var parts []types.CompletedPart
	for i, b := range [][]byte{body1, body2} {
		n := int32(i + 1)
		o, e := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String("warehouse"), Key: &key, UploadId: init.UploadId, PartNumber: &n, Body: bytes.NewReader(b)})
		must(t, e)
		parts = append(parts, types.CompletedPart{PartNumber: &n, ETag: o.ETag})
	}
	lp, e := c.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String("warehouse"), Key: &key, UploadId: init.UploadId})
	must(t, e)
	if len(lp.Parts) != 2 {
		t.Fatalf("parts: %+v", lp)
	}
	listing, e := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("warehouse")})
	must(t, e)
	if len(listing.Contents) != 0 {
		t.Fatal("multipart helpers visible")
	}
	// Open a fresh backend/handler to demonstrate that the upload is persisted.
	must(t, tr.close())
	be, e := disk.New(disk.Options{Root: root})
	must(t, e)
	defer be.Close()
	g, e := gateway.New(be, gateway.Options{AccessKey: access, SecretKey: secret})
	must(t, e)
	tr.handler = g
	request := &s3.CompleteMultipartUploadInput{Bucket: aws.String("warehouse"), Key: &key, UploadId: init.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}}
	result, e := c.CompleteMultipartUpload(ctx, request)
	must(t, e)
	if !strings.HasSuffix(aws.ToString(result.ETag), `-2"`) {
		t.Fatalf("multipart etag: %v", result.ETag)
	}
	if actual := get(t, c, key); actual != string(body1)+string(body2) {
		t.Fatal("multipart content differs")
	}
	_, e = c.CompleteMultipartUpload(ctx, request)
	must(t, e)
	abort, e := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("warehouse"), Key: &key})
	must(t, e)
	_, e = c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String("warehouse"), Key: &key, UploadId: abort.UploadId})
	must(t, e)
	if !strings.HasSuffix(get(t, c, key), "tail") {
		t.Fatal("abort removed committed data")
	}
}
func TestSDKPresignAndBadCredentials(t *testing.T) {
	c, tr, _ := setup(t)
	put(t, c, "signed +%雪", "content")
	p := s3.NewPresignClient(c)
	url, e := p.PresignGetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String("warehouse"), Key: aws.String("signed +%雪")})
	must(t, e)
	req, e := http.NewRequest("GET", url.URL, nil)
	must(t, e)
	res, e := (&http.Client{Transport: tr}).Do(req)
	must(t, e)
	defer res.Body.Close()
	v, e := io.ReadAll(res.Body)
	must(t, e)
	if res.StatusCode != 200 || string(v) != "content" {
		t.Fatalf("presign: %d %s", res.StatusCode, v)
	}
	bad := s3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: access, SecretAccessKey: "wrong"}, nil
	}), HTTPClient: &http.Client{Transport: tr}}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String("http://gateway.test")
		o.UsePathStyle = true
		o.RetryMaxAttempts = 1
	})
	_, e = bad.ListBuckets(t.Context(), &s3.ListBucketsInput{})
	code(t, e, "SignatureDoesNotMatch")
}
func TestSDKConcurrentConditionalCreate(t *testing.T) {
	c, _, _ := setup(t)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := c.PutObject(t.Context(), &s3.PutObjectInput{Bucket: aws.String("warehouse"), Key: aws.String("metadata/current.json"), Body: strings.NewReader("{}"), IfNoneMatch: aws.String("*")})
			if e == nil {
				wins.Add(1)
			} else {
				code(t, e, "PreconditionFailed")
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("expected one atomic winner, got %d", wins.Load())
	}
}
func TestIntegrityRejectsCorruptWrites(t *testing.T) {
	c, tr, _ := setup(t)
	put(t, c, "protected", "old")
	_, e := c.PutObject(t.Context(), &s3.PutObjectInput{Bucket: aws.String("warehouse"), Key: aws.String("protected"), Body: strings.NewReader("bad"), ContentMD5: aws.String(base64.StdEncoding.EncodeToString(make([]byte, 16)))})
	code(t, e, "BadDigest")
	if get(t, c, "protected") != "old" {
		t.Fatal("corrupt write was published")
	}
	req, _ := http.NewRequest("PUT", "http://gateway.test/warehouse/protected", strings.NewReader("tampered"))
	sum := sha256.Sum256([]byte("original"))
	digest := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Content-Sha256", digest)
	must(t, v4.NewSigner().SignHTTP(t.Context(), aws.Credentials{AccessKeyID: access, SecretAccessKey: secret}, req, digest, "s3", "us-east-1", time.Now(), func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true }))
	res, e := tr.RoundTrip(req)
	must(t, e)
	defer res.Body.Close()
	if res.StatusCode != 400 {
		v, _ := io.ReadAll(res.Body)
		t.Fatalf("tamper response: %d %s", res.StatusCode, v)
	}
	if get(t, c, "protected") != "old" {
		t.Fatal("tampered write replaced data")
	}
}

func TestMultipartChecksums(t *testing.T) {
	c, _, _ := setup(t)
	ctx := t.Context()
	key := "checksummed"
	init, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("warehouse"), Key: &key, ChecksumAlgorithm: types.ChecksumAlgorithmCrc32})
	must(t, err)
	p, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String("warehouse"), Key: &key, UploadId: init.UploadId, PartNumber: aws.Int32(1), Body: strings.NewReader("checksum payload"), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32})
	must(t, err)
	_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("warehouse"), Key: &key, UploadId: init.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{PartNumber: aws.Int32(1), ETag: p.ETag, ChecksumCRC32: p.ChecksumCRC32}}}})
	must(t, err)
	o, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("warehouse"), Key: &key, ChecksumMode: types.ChecksumModeEnabled})
	must(t, err)
	defer o.Body.Close()
	v, err := io.ReadAll(o.Body)
	must(t, err)
	if string(v) != "checksum payload" {
		t.Fatal("checksummed payload differs")
	}
	if o.ChecksumType != types.ChecksumTypeComposite {
		t.Fatalf("missing composite checksum type: %s", o.ChecksumType)
	}
}
func TestAWSChunkedTrailer(t *testing.T) {
	c, tr, _ := setup(t)
	data := "streamed body"
	sum := sha256.Sum256([]byte(data))
	checksum := base64.StdEncoding.EncodeToString(sum[:])
	for _, bad := range []bool{false, true} {
		trailer := checksum
		if bad {
			trailer = base64.StdEncoding.EncodeToString(make([]byte, 32))
		}
		payload := fmt.Sprintf("%x\r\n%s\r\n0\r\nx-amz-checksum-sha256:%s\r\n\r\n", len(data), data, trailer)
		req, _ := http.NewRequest("PUT", "http://gateway.test/warehouse/streamed", strings.NewReader(payload))
		req.Header.Set("Content-Encoding", "aws-chunked")
		req.Header.Set("X-Amz-Decoded-Content-Length", strconv.Itoa(len(data)))
		req.Header.Set("X-Amz-Trailer", "x-amz-checksum-sha256")
		req.Header.Set("X-Amz-Content-Sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
		must(t, v4.NewSigner().SignHTTP(t.Context(), aws.Credentials{AccessKeyID: access, SecretAccessKey: secret}, req, "STREAMING-UNSIGNED-PAYLOAD-TRAILER", "s3", "us-east-1", time.Now(), func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true }))
		res, err := tr.RoundTrip(req)
		must(t, err)
		content, _ := io.ReadAll(res.Body)
		res.Body.Close()
		want := 200
		if bad {
			want = 400
		}
		if res.StatusCode != want {
			t.Fatalf("streaming response: %d %s", res.StatusCode, content)
		}
	}
	if get(t, c, "streamed") != data {
		t.Fatal("invalid trailer replaced the object")
	}
}

func TestSDKMultipartCopyChecksumsAndListing(t *testing.T) {
	c, _, _ := setup(t)
	ctx := t.Context()
	bucket := aws.String("warehouse")
	data := "copy with checksum"
	put(t, c, "source", data)
	init, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: bucket, Key: aws.String("dir/a"), ChecksumAlgorithm: types.ChecksumAlgorithmSha256})
	must(t, err)
	p, err := c.UploadPartCopy(ctx, &s3.UploadPartCopyInput{Bucket: bucket, Key: aws.String("dir/a"), UploadId: init.UploadId, PartNumber: aws.Int32(1), CopySource: aws.String("warehouse/source")})
	must(t, err)
	digest := sha256.Sum256([]byte(data))
	want := base64.StdEncoding.EncodeToString(digest[:])
	if p.CopyPartResult == nil || aws.ToString(p.CopyPartResult.ChecksumSHA256) != want {
		t.Fatal("copy response checksum missing")
	}
	parts, err := c.ListParts(ctx, &s3.ListPartsInput{Bucket: bucket, Key: aws.String("dir/a"), UploadId: init.UploadId})
	must(t, err)
	if len(parts.Parts) != 1 || aws.ToString(parts.Parts[0].ChecksumSHA256) != want {
		t.Fatal("list part checksum missing")
	}
	for _, key := range []string{"dir/b", "root", "root"} {
		_, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: bucket, Key: &key})
		must(t, err)
	}
	page, err := c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: bucket, Delimiter: aws.String("/"), MaxUploads: aws.Int32(1)})
	must(t, err)
	if len(page.CommonPrefixes) != 1 || aws.ToString(page.CommonPrefixes[0].Prefix) != "dir/" || !aws.ToBool(page.IsTruncated) {
		t.Fatalf("bad multipart grouping: %#v", page)
	}
	page, err = c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: bucket, Delimiter: aws.String("/"), MaxUploads: aws.Int32(1), KeyMarker: page.NextKeyMarker, UploadIdMarker: page.NextUploadIdMarker})
	must(t, err)
	if len(page.Uploads) != 1 || aws.ToString(page.Uploads[0].Key) != "root" || !aws.ToBool(page.IsTruncated) {
		t.Fatal("second multipart page differs")
	}
	firstID := aws.ToString(page.Uploads[0].UploadId)
	page, err = c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: bucket, Delimiter: aws.String("/"), MaxUploads: aws.Int32(1), KeyMarker: page.NextKeyMarker, UploadIdMarker: page.NextUploadIdMarker})
	must(t, err)
	if len(page.Uploads) != 1 || aws.ToString(page.Uploads[0].UploadId) == firstID || aws.ToBool(page.IsTruncated) {
		t.Fatal("same-key multipart pagination differs")
	}
	result, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: bucket, Key: aws.String("dir/a"), UploadId: init.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{PartNumber: aws.Int32(1), ETag: p.CopyPartResult.ETag, ChecksumSHA256: p.CopyPartResult.ChecksumSHA256}}}})
	must(t, err)
	composite := sha256.Sum256(digest[:])
	if aws.ToString(result.ChecksumSHA256) != base64.StdEncoding.EncodeToString(composite[:])+"-1" {
		t.Fatal("completion XML checksum differs")
	}
}

func TestUnsupportedOperationsDoNotMutateObjects(t *testing.T) {
	c, tr, _ := setup(t)
	put(t, c, "original", "keep me")
	for _, tc := range []struct{ query, header string }{{"annotation=", ""}, {"renameObject=", ""}, {"", "X-Amz-Write-Offset-Bytes"}} {
		req, err := http.NewRequest("PUT", "http://gateway.test/warehouse/original?"+tc.query, strings.NewReader("replace"))
		must(t, err)
		if tc.header != "" {
			req.Header.Set(tc.header, "0")
		}
		sum := sha256.Sum256([]byte("replace"))
		hash := hex.EncodeToString(sum[:])
		req.Header.Set("X-Amz-Content-Sha256", hash)
		must(t, v4.NewSigner().SignHTTP(t.Context(), aws.Credentials{AccessKeyID: access, SecretAccessKey: secret}, req, hash, "s3", "us-east-1", time.Now()))
		res, err := tr.RoundTrip(req)
		must(t, err)
		res.Body.Close()
		if res.StatusCode != 501 {
			t.Fatalf("unsupported request returned %d", res.StatusCode)
		}
	}
	if get(t, c, "original") != "keep me" {
		t.Fatal("unsupported operation overwrote data")
	}
}
