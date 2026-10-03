package gateway_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
	storageaws "github.com/adrianliechti/s3-gateway/backend/aws"
	"github.com/adrianliechti/s3-gateway/backend/azure"
	"github.com/adrianliechti/s3-gateway/backend/disk"
	"github.com/adrianliechti/s3-gateway/gateway"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Use the real cloud adapters against a local HTTP provider. This exercises
// SDK parsing and error handling without live credentials or cloud resources.
type upstreamFixture struct {
	mu                   sync.Mutex
	requests             []string
	objects              int
	manifest             []byte
	metadataRef          string
	metadataFailure      bool
	forwardedCredentials bool
}

func (f *upstreamFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	f.forwardedCredentials = f.forwardedCredentials || strings.Contains(r.Header.Get("Authorization"), access) || r.URL.Query().Has("X-Amz-Credential") || r.URL.Query().Has("X-Amz-Signature")
	f.mu.Unlock()
	w.Header().Set("Server", "private-provider")
	w.Header().Set("X-Amz-Request-Id", "private-request")
	w.Header().Set("X-Amz-Id-2", "private-host-id")
	w.Header().Set("X-Amz-Bucket-Region", "private-region")
	w.Header().Set("X-Ms-Request-Id", "private-azure-request")
	w.Header().Set("Location", "https://private-provider.invalid/warehouse/key?sig=private-token")
	w.Header().Set("Content-Location", "https://private-provider.invalid/warehouse/key")
	key := strings.TrimPrefix(r.URL.Path, "/warehouse")
	key = strings.TrimPrefix(key, "/")
	if key == "provider-error" || key == "provider-body-error" && r.Method == http.MethodGet {
		w.Header().Set("X-Ms-Error-Code", "AuthorizationFailure")
		w.WriteHeader(http.StatusForbidden)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>https://private-provider.invalid/?sig=private-token</Message></Error>`)
		}
		return
	}
	if f.metadataFailure && r.Method == http.MethodPut && strings.HasPrefix(key, backend.InternalPrefix+"metadata/") {
		w.Header().Set("X-Ms-Error-Code", "AuthorizationFailure")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code></Error>`)
		return
	}
	if f.metadataRef != "" && key == f.metadataRef && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(f.manifest)
		return
	}
	if r.Method == http.MethodPut {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("ETag", `"native-etag"`)
		w.Header().Set("Last-Modified", "Sun, 04 Oct 2026 12:00:00 GMT")
		if r.Header.Get("X-Ms-Version") != "" { // Azure blob/block staging and publication.
			w.WriteHeader(http.StatusCreated)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		return
	}
	if r.URL.Query().Get("list-type") == "2" {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated>`)
		if !strings.HasPrefix(r.URL.Query().Get("prefix"), backend.InternalPrefix) {
			for i := range f.objects {
				_, _ = fmt.Fprintf(w, `<Contents><Key>object-%04d</Key><Size>7</Size><ETag>"native-etag"</ETag><LastModified>2026-10-04T12:00:00Z</LastModified></Contents>`, i)
			}
		}
		_, _ = io.WriteString(w, `</ListBucketResult>`)
		return
	}
	if strings.HasPrefix(key, backend.InternalPrefix) {
		w.Header().Set("X-Ms-Error-Code", "BlobNotFound")
		w.WriteHeader(http.StatusNotFound)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
		}
		return
	}
	if key == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if match := r.Header.Get("If-Match"); match != "" && match != `"native-etag"` {
		w.Header().Set("X-Ms-Error-Code", "ConditionNotMet")
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = io.WriteString(w, `<Error><Code>PreconditionFailed</Code></Error>`)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", `"native-etag"`)
	w.Header().Set("Last-Modified", "Sun, 04 Oct 2026 12:00:00 GMT")
	if f.metadataRef != "" {
		raw, _ := json.Marshal(map[string]any{"v": 1, "ref": f.metadataRef})
		value := base64.StdEncoding.EncodeToString(raw)
		w.Header().Set("X-Amz-Meta-Gateway", value)
		w.Header().Set("X-Ms-Meta-Gateway", value)
	}
	body := "payload"
	span := r.Header.Get("Range")
	if span == "" {
		span = r.Header.Get("X-Ms-Range")
	}
	if span != "" {
		var first, last int
		if _, err := fmt.Sscanf(span, "bytes=%d-%d", &first, &last); err != nil || first < 0 || first >= len(body) || last < first {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		last = min(last, len(body)-1)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, len(body)))
		body = body[first : last+1]
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, body)
	}
}

func cloudReadFixture(tb testing.TB, provider string, objects int) (*s3.Client, *transport, *upstreamFixture) {
	tb.Helper()
	f := &upstreamFixture{objects: objects}
	server := httptest.NewServer(f)
	tb.Cleanup(server.Close)
	var be backend.Backend
	var err error
	switch provider {
	case "s3":
		tb.Setenv("AWS_ACCESS_KEY_ID", "private-upstream-access")
		tb.Setenv("AWS_SECRET_ACCESS_KEY", "private-upstream-secret")
		tb.Setenv("AWS_SESSION_TOKEN", "")
		tb.Setenv("AWS_MAX_ATTEMPTS", "1")
		be, err = storageaws.New(tb.Context(), storageaws.Options{Endpoint: server.URL, Region: "us-east-1", UsePathStyle: true})
	case "azure":
		be, err = azure.New(azure.Options{ServiceURL: server.URL, SASToken: "sig=private-token"})
	default:
		tb.Fatalf("unknown provider %q", provider)
	}
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = be.Close() })
	g, err := gateway.New(be, gateway.Options{AccessKey: access, SecretKey: secret})
	if err != nil {
		tb.Fatal(err)
	}
	tr := &transport{handler: g}
	return client(tr), tr, f
}

func TestPresignedCloudResponsesHideUpstream(t *testing.T) {
	for _, provider := range []string{"s3", "azure"} {
		t.Run(provider, func(t *testing.T) {
			_, tr, upstream := cloudReadFixture(t, provider, 0)
			for _, tc := range []struct {
				method, key string
				status      int
			}{{"GET", "object", 200}, {"HEAD", "object", 200}, {"PUT", "object", 200}, {"GET", "provider-error", 500}, {"HEAD", "provider-error", 500}, {"GET", "provider-body-error", 500}} {
				t.Run(tc.method+"/"+tc.key, func(t *testing.T) {
					r := presigned(t, tc.method, "http://gateway.test/warehouse/"+tc.key, "60", time.Now(), nil)
					if tc.method == http.MethodPut {
						r.Body = io.NopCloser(strings.NewReader("payload"))
						r.ContentLength = 7
					}
					if r.URL.Host != "gateway.test" || strings.Contains(r.URL.String(), "private-") {
						t.Fatalf("presigned URL exposed provider: %s", r.URL)
					}
					body, headers := checkHTTP(t, &http.Client{Transport: tr}, r, tc.status, "")
					if strings.Contains(string(body)+fmt.Sprint(headers), "private-") {
						t.Fatalf("provider details reached the client: headers=%v body=%s", headers, body)
					}
					if headers.Get("Location") != "" || headers.Get("Content-Location") != "" {
						t.Fatalf("provider location reached the client: %v", headers)
					}
					if tc.method == "GET" && tc.status == 200 && string(body) != "payload" {
						t.Fatalf("object bytes changed: %q", body)
					}
				})
			}
			upstream.mu.Lock()
			defer upstream.mu.Unlock()
			if upstream.forwardedCredentials {
				t.Fatal("client authorization was forwarded to the provider")
			}
		})
	}
}

func TestCloudErrorsRetainProviderDiagnostics(t *testing.T) {
	for _, provider := range []string{"s3", "azure"} {
		t.Run(provider, func(t *testing.T) {
			c, _, _ := cloudReadFixture(t, provider, 0)
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			_, err := c.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String("warehouse"), Key: aws.String("provider-body-error")})
			code(t, err, "InternalError")
			if !strings.Contains(logs.String(), "private-provider.invalid") {
				t.Fatal("gateway logs lost upstream diagnostics")
			}
			if strings.Contains(err.Error(), "private-") {
				t.Fatal("upstream diagnostics reached the client error")
			}
		})
	}
}

func TestCloudMetadataFailureDoesNotPublishObject(t *testing.T) {
	for _, provider := range []string{"s3", "azure"} {
		t.Run(provider, func(t *testing.T) {
			c, _, upstream := cloudReadFixture(t, provider, 0)
			upstream.metadataFailure = true
			_, err := c.PutObject(t.Context(), &s3.PutObjectInput{Bucket: aws.String("warehouse"), Key: aws.String("object"), Body: strings.NewReader("replacement")})
			code(t, err, "InternalError")
			upstream.mu.Lock()
			defer upstream.mu.Unlock()
			attempted := false
			for _, request := range upstream.requests {
				method, path, _ := strings.Cut(request, " ")
				u, err := url.Parse(path)
				must(t, err)
				attempted = attempted || method == "PUT" && strings.HasPrefix(u.Path, "/warehouse/.gateway/metadata/")
				if method == "PUT" && u.Path == "/warehouse/object" && u.Query().Get("comp") != "block" {
					t.Fatalf("object published before its metadata was durable: %s", request)
				}
			}
			if !attempted {
				t.Fatal("metadata publication failure was not exercised")
			}
		})
	}
}

// Report provider calls per client operation instead of relying on local
// network timings as a proxy for production cloud latency.
func BenchmarkCloudReadRequests(b *testing.B) {
	for _, provider := range []string{"s3", "azure"} {
		b.Run(provider, func(b *testing.B) {
			for _, operation := range []string{"GET", "HEAD", "LIST-100", "LIST-1000"} {
				if provider == "azure" && strings.HasPrefix(operation, "LIST-") {
					continue
				}
				b.Run(operation, func(b *testing.B) {
					objects := 0
					if strings.HasPrefix(operation, "LIST-") {
						objects, _ = strconv.Atoi(strings.TrimPrefix(operation, "LIST-"))
					}
					c, _, upstream := cloudReadFixture(b, provider, objects)
					b.ResetTimer()
					for range b.N {
						var err error
						switch operation {
						case "GET":
							var out *s3.GetObjectOutput
							out, err = c.GetObject(b.Context(), &s3.GetObjectInput{Bucket: aws.String("warehouse"), Key: aws.String("object")})
							if err == nil {
								_, err = io.Copy(io.Discard, out.Body)
								out.Body.Close()
							}
						case "HEAD":
							_, err = c.HeadObject(b.Context(), &s3.HeadObjectInput{Bucket: aws.String("warehouse"), Key: aws.String("object")})
						default:
							_, err = c.ListObjectsV2(b.Context(), &s3.ListObjectsV2Input{Bucket: aws.String("warehouse"), MaxKeys: aws.Int32(1000)})
						}
						if err != nil {
							b.Fatal(err)
						}
					}
					b.StopTimer()
					upstream.mu.Lock()
					defer upstream.mu.Unlock()
					b.ReportMetric(float64(len(upstream.requests))/float64(b.N), "upstream-requests/op")
					if operation == "GET" {
						counts := make(map[string]int)
						for _, request := range upstream.requests {
							counts[request]++
						}
						var requests []string
						for request := range counts {
							requests = append(requests, request)
						}
						sort.Strings(requests)
						for _, request := range requests {
							b.Logf("%.0f x %s", float64(counts[request])/float64(b.N), request)
						}
					}
				})
			}
		})
	}
}

func TestCloudReadRoundTrips(t *testing.T) {
	for _, provider := range []string{"s3", "azure"} {
		t.Run(provider, func(t *testing.T) {
			for _, referenced := range []bool{false, true} {
				t.Run(fmt.Sprintf("reference=%t", referenced), func(t *testing.T) {
					_, tr, upstream := cloudReadFixture(t, provider, 0)
					if referenced {
						if provider == "s3" {
							upstream.manifest = []byte(`{"v":1,"object":{"Size":7,"ETag":"native-etag","Modified":"2026-10-04T12:00:00Z"}}`)
						} else {
							upstream.manifest = []byte(`{"v":2,"size":7,"etag":"native-etag"}`)
						}
						sum := sha256.Sum256(upstream.manifest)
						upstream.metadataRef = backend.InternalPrefix + "metadata/" + hex.EncodeToString(sum[:])
					}
					for _, tc := range []struct {
						name, method  string
						headers       http.Header
						status, calls int
						body          string
					}{
						{"get", "GET", nil, 200, 3, "payload"},
						{"head", "HEAD", nil, 200, 3, ""},
						{"range", "GET", http.Header{"Range": {"bytes=2-4"}}, 206, 4, "ylo"},
						{"not-modified", "GET", http.Header{"If-None-Match": {`"native-etag"`}}, 304, 3, ""},
						{"failed-condition", "GET", http.Header{"If-Match": {`"different"`}}, 412, 3, ""},
					} {
						t.Run(tc.name, func(t *testing.T) {
							upstream.mu.Lock()
							upstream.requests = nil
							upstream.mu.Unlock()
							r := presigned(t, tc.method, "http://gateway.test/warehouse/object", "60", time.Now(), tc.headers)
							body, headers := checkHTTP(t, &http.Client{Transport: tr}, r, tc.status, "")
							if strings.Contains(fmt.Sprint(headers), ".gateway/metadata/") || headers.Get("X-Amz-Meta-Gateway") != "" {
								t.Fatal("metadata reference reached the client")
							}
							if tc.status < 400 && string(body) != tc.body {
								t.Fatalf("body = %q, want %q", body, tc.body)
							}
							upstream.mu.Lock()
							defer upstream.mu.Unlock()
							expected := tc.calls
							if referenced {
								expected++
							}
							if len(upstream.requests) != expected {
								t.Fatalf("got %d provider calls, want %d: %v", len(upstream.requests), expected, upstream.requests)
							}
						})
					}
				})
			}
		})
	}
}

func TestMultipartLocationResolvesToUploadedObject(t *testing.T) {
	for _, virtual := range []bool{false, true} {
		t.Run(fmt.Sprintf("virtual=%t", virtual), func(t *testing.T) {
			c, tr, root := setup(t)
			must(t, tr.close())
			be, err := disk.New(disk.Options{Root: root})
			must(t, err)
			t.Cleanup(func() { _ = be.Close() })
			g, err := gateway.New(be, gateway.Options{AccessKey: access, SecretKey: secret, Domain: "gateway.test"})
			must(t, err)
			tr.handler = g
			if virtual {
				options := c.Options()
				options.UsePathStyle = false
				c = s3.New(options)
			}
			bucket, key := "warehouse", "folder/a space+%雪"
			created, err := c.CreateMultipartUpload(t.Context(), &s3.CreateMultipartUploadInput{Bucket: &bucket, Key: &key})
			must(t, err)
			part, err := c.UploadPart(t.Context(), &s3.UploadPartInput{Bucket: &bucket, Key: &key, UploadId: created.UploadId, PartNumber: aws.Int32(1), Body: strings.NewReader("payload")})
			must(t, err)
			completed, err := c.CompleteMultipartUpload(t.Context(), &s3.CompleteMultipartUploadInput{Bucket: &bucket, Key: &key, UploadId: created.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{PartNumber: aws.Int32(1), ETag: part.ETag}}}})
			must(t, err)
			base, err := url.Parse("http://gateway.test/warehouse/" + url.PathEscape(key))
			must(t, err)
			if virtual {
				base.Host = "warehouse.gateway.test"
				base.Path, base.RawPath = "/"+key, ""
			}
			location, err := url.Parse(aws.ToString(completed.Location))
			must(t, err)
			target := base.ResolveReference(location)
			r := presigned(t, "GET", target.String(), "60", time.Now(), nil)
			body, _ := checkHTTP(t, c.Options().HTTPClient, r, http.StatusOK, "")
			if string(body) != "payload" {
				t.Fatalf("completion location addresses different bytes: %s", target)
			}
		})
	}
}

// A streaming backend holds this upload before reading its body, just as a
// slow client or upstream provider could. Reads of other keys must progress.
type stalledPutBackend struct {
	backend.Backend
	backend.Properties
	started chan struct{}
	release chan struct{}
}

func (b *stalledPutBackend) Put(ctx context.Context, bucket, key string, body io.Reader, size int64, options backend.PutOptions) (backend.Object, error) {
	if key == "slow-upload" {
		close(b.started)
		select {
		case <-b.release:
		case <-ctx.Done():
			return backend.Object{}, ctx.Err()
		}
	}
	return b.Backend.Put(ctx, bucket, key, body, size, options)
}

func (b *stalledPutBackend) Compose(context.Context, string, string, []backend.ComposeSource, backend.PutOptions) (backend.Object, error) {
	return backend.Object{}, fmt.Errorf("unexpected composition")
}

func TestStreamingPutAllowsUnrelatedHead(t *testing.T) {
	be, err := disk.New(disk.Options{Root: t.TempDir()})
	must(t, err)
	t.Cleanup(func() { _ = be.Close() })
	must(t, be.CreateBucket(t.Context(), "warehouse"))
	_, err = be.Put(t.Context(), "warehouse", "ready", strings.NewReader("ready"), 5, backend.PutOptions{})
	must(t, err)
	probe := &stalledPutBackend{Backend: be, Properties: be, started: make(chan struct{}), release: make(chan struct{})}
	g, err := gateway.New(probe, gateway.Options{AccessKey: access, SecretKey: secret})
	must(t, err)
	c := client(&transport{handler: g})
	putDone := make(chan error, 1)
	go func() {
		_, err := c.PutObject(t.Context(), &s3.PutObjectInput{Bucket: aws.String("warehouse"), Key: aws.String("slow-upload"), Body: strings.NewReader("payload")})
		putDone <- err
	}()
	select {
	case <-probe.started:
	case err := <-putDone:
		t.Fatalf("upload never reached backend: %v", err)
	case <-time.After(5 * time.Second):
		close(probe.release)
		t.Fatal("upload never reached backend")
	}
	headDone := make(chan error, 1)
	go func() {
		_, err := c.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String("warehouse"), Key: aws.String("ready")})
		headDone <- err
	}()
	blocked := false
	select {
	case err := <-headDone:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		blocked = true
	}
	close(probe.release)
	must(t, <-putDone)
	if blocked {
		must(t, <-headDone)
		t.Fatal("HEAD of an unrelated object waited for a stalled upload in the same bucket")
	}
}
