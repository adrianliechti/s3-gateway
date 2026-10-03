package gateway_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/s3-gateway/gateway"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Low-level AWS presigner allows deterministic dates and deliberately invalid
// expiry values without sleeps. The gateway never participates in signing.
func presigned(t *testing.T, method, target, expires string, at time.Time, headers http.Header) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), method, target, nil)
	must(t, err)
	r.Header = headers.Clone()
	if r.Header == nil {
		r.Header = http.Header{}
	}
	q := r.URL.Query()
	q.Set("X-Amz-Expires", expires)
	r.URL.RawQuery = q.Encode()
	u, h, err := v4.NewSigner().PresignHTTP(t.Context(), aws.Credentials{AccessKeyID: access, SecretAccessKey: secret}, r, "UNSIGNED-PAYLOAD", "s3", "us-east-1", at, func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true; o.DisableHeaderHoisting = true })
	must(t, err)
	out, err := http.NewRequestWithContext(t.Context(), method, u, nil)
	must(t, err)
	out.Header = h
	return out
}

func checkHTTP(t *testing.T, c s3.HTTPClient, r *http.Request, status int, code string) ([]byte, http.Header) {
	t.Helper()
	res, err := c.Do(r)
	must(t, err)
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	must(t, err)
	if res.StatusCode != status {
		t.Fatalf("HTTP %d, expected %d: %s", res.StatusCode, status, body)
	}
	if code != "" && r.Method != "HEAD" {
		var out struct{ Code string }
		must(t, xml.Unmarshal(body, &out))
		if out.Code != code {
			t.Fatalf("error %s, expected %s", out.Code, code)
		}
	}
	return body, res.Header
}

func TestSDKCompatibilityPresignedRequests(t *testing.T) {
	checkPresignedRequests(t, versionSetup(t))
}

func checkPresignedRequests(t *testing.T, f versionFixture) {
	ctx := t.Context()
	key := "table/a space+%雪?#.parquet"
	value := "PAR1 presigned data PAR1"
	digest := sha256.Sum256([]byte(value))
	checksum := base64.StdEncoding.EncodeToString(digest[:])
	p := s3.NewPresignClient(f.c)
	put, err := p.PresignPutObject(ctx, &s3.PutObjectInput{Bucket: &f.bucket, Key: &key, ContentType: aws.String("application/vnd.apache.parquet"), Metadata: map[string]string{"table": "events"}, ChecksumSHA256: &checksum})
	must(t, err)
	r, err := http.NewRequestWithContext(ctx, "PUT", put.URL, strings.NewReader(value))
	must(t, err)
	r.Header = put.SignedHeader.Clone()
	checkHTTP(t, f.c.Options().HTTPClient, r, 200, "")
	head, err := f.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &f.bucket, Key: &key, ChecksumMode: types.ChecksumModeEnabled})
	must(t, err)
	if head.Metadata["table"] != "events" || aws.ToString(head.ChecksumSHA256) != checksum {
		t.Fatal("signed metadata/checksum was ignored")
	}
	r, err = http.NewRequestWithContext(ctx, "PUT", put.URL, strings.NewReader("tampered bytes"))
	must(t, err)
	r.Header = put.SignedHeader.Clone()
	checkHTTP(t, f.c.Options().HTTPClient, r, 400, "BadDigest")
	if f.read(t, key, "") != value {
		t.Fatal("failed presigned write changed object")
	}
	get, err := p.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &f.bucket, Key: &key, Range: aws.String("bytes=-4"), ResponseContentDisposition: aws.String(`attachment; filename="a +%.parquet"`)})
	must(t, err)
	r, err = http.NewRequestWithContext(ctx, "GET", get.URL, nil)
	must(t, err)
	r.Header = get.SignedHeader.Clone()
	body, headers := checkHTTP(t, f.c.Options().HTTPClient, r, 206, "")
	if string(body) != "PAR1" || headers.Get("Content-Disposition") != `attachment; filename="a +%.parquet"` {
		t.Fatal("presigned range or response override changed")
	}
	r, err = http.NewRequestWithContext(ctx, "GET", get.URL, nil)
	must(t, err)
	r.Header = get.SignedHeader.Clone()
	r.Header.Set("Range", "bytes=0-2")
	checkHTTP(t, f.c.Options().HTTPClient, r, 403, "SignatureDoesNotMatch")
	headerURL, err := p.PresignHeadObject(ctx, &s3.HeadObjectInput{Bucket: &f.bucket, Key: &key})
	must(t, err)
	r, err = http.NewRequestWithContext(ctx, "HEAD", headerURL.URL, nil)
	must(t, err)
	r.Header = headerURL.SignedHeader.Clone()
	body, headers = checkHTTP(t, f.c.Options().HTTPClient, r, 200, "")
	if len(body) != 0 || headers.Get("ETag") != aws.ToString(head.ETag) {
		t.Fatal("presigned HEAD differs")
	}
	// Real HTTP parsing is exercised too, rather than just the in-process SDK transport.
	g, err := gateway.New(f.be, gateway.Options{AccessKey: access, SecretKey: secret})
	must(t, err)
	srv := httptest.NewServer(g)
	defer srv.Close()
	r = presigned(t, "GET", srv.URL+"/"+f.bucket+"/"+url.PathEscape(key), "60", time.Now(), nil)
	body, _ = checkHTTP(t, srv.Client(), r, 200, "")
	if string(body) != value {
		t.Fatal("real HTTP presigning failed")
	}
}

func TestSDKCompatibilityPresignedValidation(t *testing.T) {
	f := versionSetup(t)
	f.put(t, "key", "content")
	target := "http://gateway.test/" + f.bucket + "/key"
	for _, tc := range []struct {
		name, expires string
		at            time.Time
		status        int
		code          string
	}{
		{"valid", "60", time.Now(), 200, ""},
		{"seven-days", "604800", time.Now(), 200, ""},
		{"expired", "60", time.Now().Add(-2 * time.Minute), 403, "AccessDenied"},
		{"future", "60", time.Now().Add(time.Hour), 403, "AccessDenied"},
		{"zero", "0", time.Now(), 400, "AuthorizationQueryParametersError"},
		{"negative", "-1", time.Now(), 400, "AuthorizationQueryParametersError"},
		{"too-long", "604801", time.Now(), 400, "AuthorizationQueryParametersError"},
		{"malformed", "no", time.Now(), 400, "AuthorizationQueryParametersError"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkHTTP(t, f.c.Options().HTTPClient, presigned(t, "GET", target, tc.expires, tc.at, nil), tc.status, tc.code)
		})
	}
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
		status int
		code   string
	}{
		{"path", func(r *http.Request) { r.URL.Path += "changed" }, 403, "SignatureDoesNotMatch"},
		{"host", func(r *http.Request) { r.Host = "other.test" }, 403, "SignatureDoesNotMatch"},
		{"method", func(r *http.Request) { r.Method = "DELETE" }, 403, "SignatureDoesNotMatch"},
		{"query", func(r *http.Request) { r.URL.RawQuery += "&response-content-type=text/plain" }, 403, "SignatureDoesNotMatch"},
		{"duplicate", func(r *http.Request) { r.URL.RawQuery += "&X-Amz-Expires=60" }, 400, "InvalidArgument"},
		{"missing-header", func(r *http.Request) { r.Header.Del("Range") }, 403, "SignatureDoesNotMatch"},
		{"unsigned-if-range", func(r *http.Request) { r.Header.Set("If-Range", `"other-etag"`) }, 403, "AccessDenied"},
		{"unsigned-metadata", func(r *http.Request) { r.Header.Set("X-Amz-Meta-Injected", "value") }, 403, "AccessDenied"},
		{"mixed-auth", func(r *http.Request) { r.Header.Set("Authorization", "AWS4-HMAC-SHA256 invalid") }, 400, "InvalidArgument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := presigned(t, "GET", target, "60", time.Now(), http.Header{"Range": {"bytes=0-2"}})
			tc.change(r)
			checkHTTP(t, f.c.Options().HTTPClient, r, tc.status, tc.code)
		})
	}
	if f.read(t, "key", "") != "content" {
		t.Fatal("tampered URL mutated object")
	}
	checksum := sha256.Sum256([]byte("checked"))
	signedChecksum := base64.StdEncoding.EncodeToString(checksum[:])
	for _, value := range []string{"checked", "changed"} {
		r := presigned(t, "PUT", target+"?"+url.Values{"x-amz-checksum-sha256": {signedChecksum}}.Encode(), "60", time.Now(), nil)
		r.Body = io.NopCloser(strings.NewReader(value))
		r.ContentLength = int64(len(value))
		status, code := 200, ""
		if value != "checked" {
			status, code = 400, "BadDigest"
		}
		checkHTTP(t, f.c.Options().HTTPClient, r, status, code)
	}
	// Query values must reach the ordinary feature validators and metadata handler.
	for _, tc := range []struct {
		name, value, code string
		status            int
	}{
		{"x-amz-server-side-encryption", "AES256", "NotImplemented", 501},
		{"x-amz-acl", "public-read", "AccessControlListNotSupported", 400},
		{"x-amz-bypass-governance-retention", "true", "NotImplemented", 501},
		{"x-amz-storage-class", "GLACIER", "InvalidStorageClass", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := presigned(t, "PUT", target+"?"+url.Values{tc.name: {tc.value}}.Encode(), "60", time.Now(), nil)
			checkHTTP(t, f.c.Options().HTTPClient, r, tc.status, tc.code)
		})
	}
	r := presigned(t, "PUT", target+"?x-amz-meta-table=events", "60", time.Now(), http.Header{"X-Amz-Meta-Table": {"different"}})
	checkHTTP(t, f.c.Options().HTTPClient, r, 400, "InvalidRequest")
	r = presigned(t, "PUT", target+"?"+url.Values{"x-amz-meta-table": {"two  spaces"}, "x-amz-acl": {"private"}}.Encode(), "60", time.Now(), http.Header{"X-Amz-Meta-Table": {"two  spaces"}})
	checkHTTP(t, f.c.Options().HTTPClient, r, 200, "")
	head, err := f.c.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: &f.bucket, Key: aws.String("key")})
	must(t, err)
	if head.Metadata["table"] != "two  spaces" {
		t.Fatal("query metadata was discarded or normalized")
	}
}

func TestSDKCompatibilityUnsupportedProtection(t *testing.T) {
	f := versionSetup(t)
	bucket := f.bucket + "-locked"
	_, err := f.c.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: &bucket, ObjectLockEnabledForBucket: aws.Bool(true)})
	code(t, err, "NotImplemented")
	if err = f.be.HeadBucket(t.Context(), bucket); err == nil {
		_ = f.be.DeleteBucket(t.Context(), bucket)
		t.Fatal("unsupported lock request created bucket")
	}
	f.put(t, "key", "keep")
	_, err = f.c.DeleteObject(t.Context(), &s3.DeleteObjectInput{Bucket: &f.bucket, Key: aws.String("key"), BypassGovernanceRetention: aws.Bool(true)})
	code(t, err, "NotImplemented")
	if f.read(t, "key", "") != "keep" {
		t.Fatal("unsupported retention bypass deleted object")
	}
	_, err = f.c.CopyObject(t.Context(), &s3.CopyObjectInput{Bucket: &f.bucket, Key: aws.String("copied"), CopySource: aws.String(f.bucket + "/key"), CopySourceSSECustomerAlgorithm: aws.String("AES256")})
	code(t, err, "NotImplemented")
	_, err = f.c.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: &f.bucket, Key: aws.String("copied")})
	code(t, err, "NotFound")

}

func TestSDKCompatibilityPresignedVersionsAndMultipart(t *testing.T) {
	checkPresignedVersionsAndMultipart(t, versionSetup(t))
}

func checkPresignedVersionsAndMultipart(t *testing.T, f versionFixture) {
	ctx := t.Context()
	f.state(t, types.BucketVersioningStatusEnabled)
	key := "presigned/versioned"
	first := f.put(t, key, "old")
	f.put(t, key, "new")
	p := s3.NewPresignClient(f.c)
	signed, err := p.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &f.bucket, Key: &key, VersionId: first.VersionId})
	must(t, err)
	r, err := http.NewRequestWithContext(ctx, "GET", signed.URL, nil)
	must(t, err)
	r.Header = signed.SignedHeader.Clone()
	body, _ := checkHTTP(t, f.c.Options().HTTPClient, r, 200, "")
	if string(body) != "old" {
		t.Fatal("presigned URL read wrong version")
	}
	r.URL.RawQuery = strings.ReplaceAll(r.URL.RawQuery, aws.ToString(first.VersionId), "null")
	checkHTTP(t, f.c.Options().HTTPClient, r, 403, "SignatureDoesNotMatch")

	put, err := p.PresignPutObject(ctx, &s3.PutObjectInput{Bucket: &f.bucket, Key: &key, IfNoneMatch: aws.String("*")})
	must(t, err)
	r, err = http.NewRequestWithContext(ctx, "PUT", put.URL, strings.NewReader("should not replace"))
	must(t, err)
	r.Header = put.SignedHeader.Clone()
	checkHTTP(t, f.c.Options().HTTPClient, r, 412, "PreconditionFailed")
	if f.read(t, key, "") != "new" {
		t.Fatal("presigned condition was ignored")
	}

	key = "presigned/multipart"
	upload, err := f.c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &f.bucket, Key: &key})
	must(t, err)
	t.Cleanup(func() {
		_, _ = f.c.AbortMultipartUpload(context.Background(), &s3.AbortMultipartUploadInput{Bucket: &f.bucket, Key: &key, UploadId: upload.UploadId})
	})
	part, err := p.PresignUploadPart(ctx, &s3.UploadPartInput{Bucket: &f.bucket, Key: &key, UploadId: upload.UploadId, PartNumber: aws.Int32(1)})
	must(t, err)
	r, err = http.NewRequestWithContext(ctx, "PUT", part.URL, strings.NewReader("multipart data"))
	must(t, err)
	r.Header = part.SignedHeader.Clone()
	_, headers := checkHTTP(t, f.c.Options().HTTPClient, r, 200, "")
	completed, err := f.c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: &f.bucket, Key: &key, UploadId: upload.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{PartNumber: aws.Int32(1), ETag: aws.String(headers.Get("ETag"))}}}})
	must(t, err)
	// A new handler has no in-memory upload or version state.
	restarted, err := gateway.New(f.be, gateway.Options{AccessKey: access, SecretKey: secret, Domain: "gateway.test"})
	must(t, err)
	fresh := client(&transport{handler: restarted})
	virtualTarget := "http://" + f.bucket + ".gateway.test/" + key + "?" + url.Values{"versionId": {aws.ToString(completed.VersionId)}}.Encode()
	r = presigned(t, "GET", virtualTarget, "60", time.Now(), nil)
	body, _ = checkHTTP(t, fresh.Options().HTTPClient, r, 200, "")
	if string(body) != "multipart data" {
		t.Fatal("presigned multipart version did not survive handler restart")
	}
}
