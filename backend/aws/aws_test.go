package aws

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixture(t *testing.T) (*Store, string) {
	t.Helper()
	endpoint := os.Getenv("GATEWAY_TEST_STORAGE_ENDPOINT")
	if endpoint == "" {
		t.Skip("set GATEWAY_TEST_STORAGE_ENDPOINT or run task test-s3 for the provider contract tests")
	}
	s, err := New(t.Context(), Options{Endpoint: endpoint, Region: "us-east-1", UsePathStyle: true})
	check(t, err)
	b := fmt.Sprintf("s3gw-backend-%d", time.Now().UnixNano())
	check(t, s.CreateBucket(t.Context(), b))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		pager := s3.NewListObjectVersionsPaginator(listingClient{s.client}, &s3.ListObjectVersionsInput{Bucket: &b})
		for pager.HasMorePages() {
			page, e := pager.NextPage(ctx)
			if e != nil {
				t.Error(e)
				return
			}
			for _, item := range page.Versions {
				_, e = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &b, Key: item.Key, VersionId: item.VersionId})
				if e != nil {
					t.Error(e)
				}
			}
			for _, item := range page.DeleteMarkers {
				_, e = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &b, Key: item.Key, VersionId: item.VersionId})
				if e != nil {
					t.Error(e)
				}
			}
		}
		check(t, s.DeleteBucket(ctx, b))
	})
	return s, b
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func wantError(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("want %v, got %v", want, err)
	}
}
func read(t *testing.T, s *Store, b, k string, r backend.ReadOptions) []byte {
	t.Helper()
	_, body, err := s.Get(t.Context(), b, k, r)
	check(t, err)
	defer body.Close()
	raw, err := io.ReadAll(body)
	check(t, err)
	return raw
}

func TestS3ObjectsAndMetadata(t *testing.T) {
	s, b := fixture(t)
	ctx := t.Context()
	k := "tables/a space+%/雪.parquet"
	data := "PAR1 original bytes PAR1"
	p := backend.Object{ETag: "frontend-multipart-2", ContentType: "application/parquet", CacheControl: "max-age=42", ContentDisposition: "inline", ContentEncoding: "identity", ContentLanguage: "en", WebsiteRedirectLocation: "/next", Expires: "Wed, 21 Oct 2037 07:28:00 GMT", Metadata: map[string]string{"table": "events", "gateway": "user value"}, Tags: []backend.Tag{{Key: "a", Value: "b"}}, Checksums: map[string]string{"SHA256": "checksum"}, Parts: []backend.ObjectPart{{Number: 1, Size: int64(len(data))}}}
	o, err := s.Put(ctx, b, k, strings.NewReader(data), int64(len(data)), backend.PutOptions{Object: p})
	check(t, err)
	if o.ETag != p.ETag || o.Revision == "" || o.Revision == o.ETag {
		t.Fatalf("logical/native identities: %+v", o)
	}
	head, err := s.Head(ctx, b, k)
	check(t, err)
	if !reflect.DeepEqual(head, o) {
		t.Fatalf("metadata round trip:\n%+v\n%+v", head, o)
	}
	if string(read(t, s, b, k, backend.ReadOptions{Offset: 5, Length: 8, Revision: o.Revision})) != "original" {
		t.Fatal("incorrect range")
	}
	if len(read(t, s, b, k, backend.ReadOptions{Length: 0})) != 0 {
		t.Fatal("nonempty zero length read")
	}
	native, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &b, Key: &k})
	check(t, err)
	raw, err := io.ReadAll(native.Body)
	native.Body.Close()
	check(t, err)
	// RustFS omits the native redirect property; the gateway round trip above
	// must still preserve it through the metadata envelope. Native request
	// headers are verified separately in TestNativeRedirectHeaders.
	if string(raw) != data || native.Metadata["table"] != "events" || aws.ToString(native.ContentType) != p.ContentType {
		t.Fatal("native representation differs")
	}
	o.Tags = []backend.Tag{{Key: "updated", Value: "yes"}}
	check(t, s.SetObjectMetadata(ctx, b, k, o))
	head, err = s.Head(ctx, b, k)
	check(t, err)
	if head.ETag != o.ETag || !head.Modified.Equal(o.Modified) || !reflect.DeepEqual(head.Tags, o.Tags) {
		t.Fatalf("metadata update changed identity: %+v", head)
	}
	_, err = s.Put(ctx, b, k, strings.NewReader("new"), 3, backend.PutOptions{Conditions: backend.Conditions{IfMatch: o.ETag}})
	check(t, err)
	_, body, err := s.Get(ctx, b, k, backend.ReadOptions{Length: -1, Revision: o.Revision})
	if body != nil {
		body.Close()
	}
	wantError(t, err, backend.ErrPrecondition)
	wantError(t, s.SetObjectMetadata(ctx, b, k, o), backend.ErrPrecondition)
	_, err = s.Put(ctx, b, k, strings.NewReader("short"), 10, backend.PutOptions{})
	wantError(t, err, io.ErrUnexpectedEOF)
	if string(read(t, s, b, k, backend.ReadOptions{Length: -1})) != "new" {
		t.Fatal("short write published")
	}
	_, err = s.Put(ctx, b, "empty", strings.NewReader(""), -1, backend.PutOptions{})
	check(t, err)
	if len(read(t, s, b, "empty", backend.ReadOptions{Length: -1})) != 0 {
		t.Fatal("empty object differs")
	}
}

func TestS3ConditionsAndErrors(t *testing.T) {
	s, b := fixture(t)
	ctx := t.Context()
	wantError(t, s.CreateBucket(ctx, b), backend.ErrBucketExists)
	wantError(t, s.HeadBucket(ctx, b+"-absent"), backend.ErrBucketNotFound)
	_, err := s.Head(ctx, b, "absent")
	wantError(t, err, backend.ErrNotFound)
	_, err = s.Head(ctx, b+"-absent", "key")
	wantError(t, err, backend.ErrBucketNotFound)
	_, err = s.Put(ctx, b, "absent", strings.NewReader("x"), 1, backend.PutOptions{Conditions: backend.Conditions{IfMatch: "*"}})
	wantError(t, err, backend.ErrNotFound)
	wantError(t, s.Delete(ctx, b, "absent", backend.Conditions{IfMatch: "*"}), backend.ErrNotFound)
	check(t, s.Delete(ctx, b, "absent", backend.Conditions{IfNoneMatch: "*"}))
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.Put(ctx, b, "race", strings.NewReader("winner"), 6, backend.PutOptions{Conditions: backend.Conditions{IfNoneMatch: "*"}})
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	wins, losses := 0, 0
	for e := range results {
		if e == nil {
			wins++
		} else if errors.Is(e, backend.ErrPrecondition) || errors.Is(e, backend.ErrConflict) {
			losses++
		} else {
			t.Fatal(e)
		}
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("conditional publication: %d wins, %d losses", wins, losses)
	}
	o, err := s.Head(ctx, b, "race")
	check(t, err)
	wantError(t, s.Delete(ctx, b, "race", backend.Conditions{IfMatch: "wrong"}), backend.ErrPrecondition)
	wantError(t, s.DeleteBucket(ctx, b), backend.ErrBucketNotEmpty)
	check(t, s.Delete(ctx, b, "race", backend.Conditions{IfMatch: o.ETag}))
}

func TestS3PaginationAndSettings(t *testing.T) {
	s, b := fixture(t)
	ctx := t.Context()
	for _, k := range []string{".gateway/a", ".gateway/b", ".gateway/c", "a", "dir/", "dir/x", "dir/y", "雪"} {
		data := k
		if strings.HasSuffix(k, "/") {
			data = ""
		}
		_, err := s.Put(ctx, b, k, strings.NewReader(data), -1, backend.PutOptions{})
		if err != nil {
			t.Fatalf("put %q: %v", k, err)
		}
	}
	var keys []string
	after := ""
	for {
		page, next, err := s.List(ctx, b, "", after, 2)
		check(t, err)
		for _, o := range page {
			keys = append(keys, o.Key)
		}
		if next == "" {
			break
		}
		if next <= after {
			t.Fatal("pagination did not advance")
		}
		after = next
	}
	if !reflect.DeepEqual(keys, []string{"a", "dir/", "dir/x", "dir/y", "雪"}) {
		t.Fatalf("keys: %q", keys)
	}
	page, _, err := s.List(ctx, b, backend.InternalPrefix, "", 10)
	check(t, err)
	var internal []string
	for _, o := range page {
		if !strings.HasPrefix(o.Key, detailsPrefix) {
			internal = append(internal, o.Key)
		}
	}
	if !reflect.DeepEqual(internal, []string{".gateway/a", ".gateway/b", ".gateway/c"}) {
		t.Fatalf("internal keys: %+v", page)
	}
	p := backend.BucketProperties{Versioning: "Enabled", Tags: []backend.Tag{{Key: "test", Value: "yes"}}}
	check(t, s.SetBucketProperties(ctx, b, p))
	got, err := s.GetBucketProperties(ctx, b)
	check(t, err)
	if !reflect.DeepEqual(got, p) {
		t.Fatalf("settings: %+v", got)
	}
}

func TestS3LargeUploadAndManifests(t *testing.T) {
	s, b := fixture(t)
	ctx := t.Context()
	data := bytes.Repeat([]byte("abcdefgh"), int(uploadPartSize/8+100))
	p := backend.Object{Metadata: map[string]string{"large": strings.Repeat("v", 2000)}}
	for i := 1; i <= 1000; i++ {
		p.Parts = append(p.Parts, backend.ObjectPart{Number: i, Size: 8, Checksums: map[string]string{"SHA256": "part-checksum"}})
	}
	o, err := s.Put(ctx, b, "large", bytes.NewReader(data), int64(len(data)), backend.PutOptions{Object: p})
	check(t, err)
	if !strings.Contains(o.Revision, "-2") {
		t.Fatalf("expected native multipart ETag: %s", o.Revision)
	}
	if !bytes.Equal(read(t, s, b, "large", backend.ReadOptions{Length: -1}), data) {
		t.Fatal("multipart bytes differ")
	}
	head, err := s.Head(ctx, b, "large")
	check(t, err)
	if !reflect.DeepEqual(head.Parts, p.Parts) || !reflect.DeepEqual(head.Metadata, p.Metadata) {
		t.Fatal("manifest metadata differs")
	}
	check(t, s.CollectGarbage(ctx, b, time.Now().Add(time.Hour)))
	_, err = s.Head(ctx, b, "large")
	check(t, err)
	check(t, s.Delete(ctx, b, "large", backend.Conditions{}))
	check(t, s.CollectGarbage(ctx, b, time.Now().Add(time.Hour)))
	page, _, err := s.List(ctx, b, detailsPrefix, "", 100)
	check(t, err)
	if len(page) != 0 {
		t.Fatal("orphan manifests retained")
	}
}

func TestS3SDKConfiguration(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/config")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/credentials")
	t.Setenv("AWS_IGNORE_CONFIGURED_ENDPOINT_URLS", "false")
	t.Setenv("AWS_ENDPOINT_URL", "http://global.test")
	t.Setenv("AWS_ENDPOINT_URL_S3", "http://rustfs.test:9000")
	t.Setenv("AWS_REGION", "eu-west-1")
	for _, tc := range []struct {
		name, endpoint, region, wantEndpoint, wantRegion string
	}{
		{name: "SDK environment", wantEndpoint: "http://rustfs.test:9000", wantRegion: "eu-west-1"},
		{name: "explicit override", endpoint: "https://override.test", region: "us-west-2", wantEndpoint: "https://override.test", wantRegion: "us-west-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := New(t.Context(), Options{Endpoint: tc.endpoint, Region: tc.region, UsePathStyle: true})
			check(t, err)
			t.Cleanup(func() { check(t, s.Close()) })
			req, err := s3.NewPresignClient(s.client).PresignGetObject(t.Context(), &s3.GetObjectInput{
				Bucket: aws.String("bucket"), Key: aws.String("key"),
			})
			check(t, err)
			if !strings.HasPrefix(req.URL, tc.wantEndpoint+"/bucket/key?") {
				t.Fatalf("expected endpoint %q, got %q", tc.wantEndpoint, req.URL)
			}
			if !strings.Contains(req.URL, "%2F"+tc.wantRegion+"%2Fs3%2Faws4_request") {
				t.Fatalf("expected signing region %q, got %q", tc.wantRegion, req.URL)
			}
		})
	}
}

func TestS3EndpointValidation(t *testing.T) {
	for _, endpoint := range []string{"localhost:9000", "ftp://host", "https://user:secret@host", "https://host?secret=value", "https://host#fragment"} {
		if _, err := New(t.Context(), Options{Endpoint: endpoint}); err == nil {
			t.Fatalf("accepted endpoint %q", endpoint)
		}
	}
}

// Model a concurrent native overwrite after HEAD. Both the logical frontend
// condition and the native condition on the actual SDK mutation must hold.
func TestS3NativeConditionsOnWire(t *testing.T) {
	for _, operation := range []string{"put", "multipart", "get", "delete", "metadata"} {
		t.Run(operation, func(t *testing.T) {
			var mutated, aborted bool
			raw, err := json.Marshal(metadata{Version: 1, Object: &backend.Object{Size: 3, ETag: "frontend-2", Modified: time.Now().UTC().Truncate(time.Second)}})
			check(t, err)
			tr := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				h := make(http.Header)
				status := 200
				body := ""
				switch {
				case r.Method == "PUT" && strings.Contains(r.URL.Path, "/"+detailsPrefix):
					if r.Header.Get("If-None-Match") != "*" {
						t.Fatal("stream metadata must be immutable")
					}
				case r.Method == "HEAD":
					h.Set("ETag", `"native"`)
					h.Set("Content-Length", "3")
					h.Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
					h.Set("X-Amz-Meta-Gateway", base64.StdEncoding.EncodeToString(raw))
				case r.Method == "POST" && r.URL.Query().Has("uploads"):
					body = `<InitiateMultipartUploadResult><UploadId>upload</UploadId></InitiateMultipartUploadResult>`
				case r.Method == "PUT" && r.URL.Query().Has("partNumber"):
					h.Set("ETag", `"part"`)
				case r.Method == "DELETE" && r.URL.Query().Has("uploadId"):
					aborted = true
				default:
					if r.Header.Get("If-Match") != `"native"` {
						t.Fatalf("%s lost native precondition: %v", r.Method, r.Header)
					}
					if operation == "metadata" && r.Method == "GET" {
						body = "old"
						h.Set("ETag", `"native"`)
						h.Set("Content-Length", "3")
						break
					}
					mutated = true
					status = 412
					body = `<Error><Code>PreconditionFailed</Code></Error>`
				}
				return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			s := &Store{client: s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String("http://upstream.test"), UsePathStyle: true, RetryMaxAttempts: 1, RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired, HTTPClient: &http.Client{Transport: tr}, Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test-secret"}, nil
			})})}
			ctx := t.Context()
			switch operation {
			case "put", "multipart":
				data := []byte("new")
				if operation == "multipart" {
					data = bytes.Repeat([]byte("x"), int(uploadPartSize+1))
				}
				_, err = s.Put(ctx, "bucket", "key", bytes.NewReader(data), int64(len(data)), backend.PutOptions{Conditions: backend.Conditions{IfMatch: "frontend-2"}})
			case "get":
				_, _, err = s.Get(ctx, "bucket", "key", backend.ReadOptions{Length: -1, Revision: `"native"`})
			case "delete":
				err = s.Delete(ctx, "bucket", "key", backend.Conditions{IfMatch: "frontend-2"})
			case "metadata":
				err = s.SetObjectMetadata(ctx, "bucket", "key", backend.Object{Size: 3, ETag: "frontend-2", Revision: `"native"`})
			}
			wantError(t, err, backend.ErrPrecondition)
			if !mutated {
				t.Fatal("conditional request not sent")
			}
			if operation == "multipart" && !aborted {
				t.Fatal("failed multipart upload not aborted")
			}
		})
	}
}

func TestS3RetainsForeignNativeHistory(t *testing.T) {
	s, b := fixture(t)
	ctx := t.Context()
	_, err := s.client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: &b, VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}})
	check(t, err)
	key := "foreign"
	created, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &b, Key: &key, Body: strings.NewReader("native history")})
	check(t, err)
	_, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &b, Key: &key})
	check(t, err)
	wantError(t, s.DeleteBucket(ctx, b), backend.ErrBucketNotEmpty)
	_, err = s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &b, Key: &key, VersionId: created.VersionId})
	check(t, err)
}
