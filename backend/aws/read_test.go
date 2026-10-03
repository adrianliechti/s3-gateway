package aws

import (
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type trackedBody struct {
	io.Reader
	closed bool
}

func (b *trackedBody) Close() error { b.closed = true; return nil }

func TestGetUsesObjectAndMetadataRequests(t *testing.T) {
	for _, scenario := range []string{"full", "range", "data-only", "wrong-revision", "malformed-range", "missing-metadata", "corrupt-metadata"} {
		t.Run(scenario, func(t *testing.T) {
			want := backend.Object{Size: 7, ETag: "frontend-2", Modified: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC), VersionID: "logical-version", Metadata: map[string]string{"table": "events"}, Checksums: map[string]string{"CRC32": "AAAAAA==-2"}, ChecksumType: "COMPOSITE"}
			var metadata map[string]string
			var manifest string
			calls := 0
			var body *trackedBody
			tr := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if strings.HasPrefix(r.URL.Path, "/bucket/"+detailsPrefix) {
					status, data := 200, manifest
					if r.Method == "PUT" {
						raw, err := io.ReadAll(r.Body)
						check(t, err)
						manifest = string(raw)
						data = ""
					} else if r.Method != "GET" || r.Header.Get("If-Match") != "" || r.URL.Query().Has("versionId") {
						t.Fatalf("unexpected helper request: %s %s", r.Method, r.URL)
					} else if scenario == "missing-metadata" {
						status, data = 404, `<Error><Code>NoSuchKey</Code></Error>`
					} else if scenario == "corrupt-metadata" {
						data = "corrupt"
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(data)), Request: r}, nil
				}
				if r.Method != "GET" || r.Header.Get("If-Match") != `"native"` || r.URL.Query().Get("versionId") != "native-version" {
					t.Fatalf("unexpected native read: %s %s %v", r.Method, r.URL, r.Header)
				}
				h := http.Header{"Etag": {`"native"`}, "Content-Type": {"application/parquet"}, "Last-Modified": {want.Modified.Format(http.TimeFormat)}}
				for k, v := range metadata {
					h.Set("X-Amz-Meta-"+k, v)
				}
				data, status := "payload", 200
				if scenario == "range" || scenario == "malformed-range" {
					if r.Header.Get("Range") != "bytes=2-4" {
						t.Fatal("range not forwarded")
					}
					data, status = "ylo", 206
					h.Set("Content-Range", "bytes 2-4/7")
				}
				if scenario == "wrong-revision" {
					h.Set("ETag", `"changed"`)
				}
				if scenario == "malformed-range" {
					h.Set("Content-Range", "bytes 2-4/3")
				}
				h.Set("Content-Length", strconv.Itoa(len(data)))
				body = &trackedBody{Reader: strings.NewReader(data)}
				return &http.Response{StatusCode: status, Header: h, Body: body, Request: r}, nil
			})
			s := &Store{client: s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String("http://provider.test"), UsePathStyle: true, Credentials: aws.AnonymousCredentials{}, HTTPClient: &http.Client{Transport: tr}})}
			var err error
			metadata, err = s.encodeMetadata(t.Context(), "bucket", want)
			check(t, err)
			calls = 0
			options := backend.ReadOptions{Length: -1, Revision: `"native"`}
			options.DataOnly = scenario == "data-only"
			if scenario == "range" || scenario == "malformed-range" {
				options.Offset, options.Length = 2, 3
			}
			got, stream, err := s.Get(t.Context(), "bucket", backend.VersionReference("object", "native-version"), options)
			if scenario == "wrong-revision" || scenario == "malformed-range" || scenario == "missing-metadata" || scenario == "corrupt-metadata" {
				if err == nil || stream != nil || !body.closed {
					t.Fatalf("invalid response not rejected and closed: %v", err)
				}
				if scenario == "wrong-revision" && !errors.Is(err, backend.ErrPrecondition) {
					t.Fatalf("wrong error: %v", err)
				}
			} else {
				check(t, err)
				data, err := io.ReadAll(stream)
				stream.Close()
				check(t, err)
				expected := "payload"
				if scenario == "range" {
					expected = "ylo"
				}
				if scenario == "data-only" {
					want = backend.Object{ETag: "native", Metadata: metadata}
				}
				if string(data) != expected || got.Size != 7 || got.ETag != want.ETag || got.VersionID != want.VersionID || !reflect.DeepEqual(got.Metadata, want.Metadata) || !reflect.DeepEqual(got.Checksums, want.Checksums) {
					t.Fatalf("response metadata or bytes changed: %+v, %q", got, data)
				}
			}
			expectedCalls := 2
			if scenario == "data-only" || scenario == "wrong-revision" || scenario == "malformed-range" {
				expectedCalls = 1
			}
			if calls != expectedCalls {
				t.Fatalf("read used %d requests, want %d", calls, expectedCalls)
			}
		})
	}
}

func TestBucketSettingsErrorsNeedNoRedundantHead(t *testing.T) {
	for _, scenario := range []string{"present", "missing-settings", "missing-bucket", "ambiguous-404"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			tr := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				status, body := 200, `{"Versioning":"Enabled"}`
				if scenario != "present" {
					status = 404
					code := "NoSuchKey"
					if scenario == "missing-bucket" {
						code = "NoSuchBucket"
					}
					if scenario == "ambiguous-404" {
						code = "NotFound"
					}
					body = "<Error><Code>" + code + "</Code></Error>"
				}
				if r.Method == "HEAD" && scenario != "ambiguous-404" {
					t.Fatal("unnecessary bucket HEAD")
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			s := &Store{client: s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String("http://provider.test"), UsePathStyle: true, Credentials: aws.AnonymousCredentials{}, HTTPClient: &http.Client{Transport: tr}})}
			got, err := s.GetBucketProperties(t.Context(), "bucket")
			if scenario == "missing-bucket" || scenario == "ambiguous-404" {
				wantError(t, err, backend.ErrBucketNotFound)
			} else {
				check(t, err)
				if scenario == "present" && got.Versioning != "Enabled" {
					t.Fatal("settings lost")
				}
			}
			want := 1
			if scenario == "ambiguous-404" {
				want = 2
			}
			if calls != want {
				t.Fatalf("got %d requests, want %d", calls, want)
			}
		})
	}
}
