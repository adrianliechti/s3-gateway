package azure

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/adrianliechti/s3-gateway/backend"
)

func TestGetUsesObjectAndMetadataRequests(t *testing.T) {
	for _, scenario := range []string{"full", "range", "data-only", "wrong-revision", "malformed-range", "missing-metadata", "corrupt-metadata"} {
		t.Run(scenario, func(t *testing.T) {
			want := backend.Object{Size: 7, ETag: "frontend-2", VersionID: "logical-version", Metadata: map[string]string{"table": "events"}, Checksums: map[string]string{"CRC32": "AAAAAA==-2"}, ChecksumType: "COMPOSITE"}
			digest := md5.Sum([]byte("payload"))
			var metadata map[string]*string
			var manifest []byte
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if strings.HasPrefix(r.URL.Path, "/bucket/"+detailsPrefix) {
					if r.Method == "PUT" {
						var err error
						manifest, err = io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
						}
						w.WriteHeader(201)
						return
					}
					if r.Method != "GET" || r.Header.Get("If-Match") != "" || r.URL.Query().Has("versionid") {
						t.Errorf("unexpected helper request: %s %s", r.Method, r.URL)
					}
					if scenario == "missing-metadata" {
						w.Header().Set("X-Ms-Error-Code", "BlobNotFound")
						w.WriteHeader(404)
					} else if scenario == "corrupt-metadata" {
						_, _ = io.WriteString(w, "corrupt")
					} else {
						_, _ = w.Write(manifest)
					}
					return
				}
				if r.Method != "GET" || r.Header.Get("If-Match") != `"native"` || r.URL.Query().Get("versionid") != "native-version" {
					t.Errorf("unexpected native read: %s %s %v", r.Method, r.URL, r.Header)
				}
				w.Header().Set("ETag", `"native"`)
				w.Header().Set("Content-Type", "application/parquet")
				for k, v := range metadata {
					w.Header().Set("X-Ms-Meta-"+k, val(v))
				}
				data, status := "payload", 200
				if scenario == "range" || scenario == "malformed-range" {
					if r.Header.Get("X-Ms-Range") != "bytes=2-4" {
						t.Error("range not forwarded")
					}
					data, status = "ylo", 206
					w.Header().Set("Content-Range", "bytes 2-4/7")
					// Azure can supply a range digest separately from the blob's
					// full-object MD5. Only the latter validates the envelope.
					partDigest := md5.Sum([]byte(data))
					w.Header().Set("Content-MD5", base64.StdEncoding.EncodeToString(partDigest[:]))
					w.Header().Set("X-Ms-Blob-Content-MD5", base64.StdEncoding.EncodeToString(digest[:]))
				} else {
					w.Header().Set("Content-MD5", base64.StdEncoding.EncodeToString(digest[:]))
				}
				if scenario == "wrong-revision" {
					w.Header().Set("ETag", `"changed"`)
				}
				if scenario == "malformed-range" {
					w.Header().Set("Content-Range", "bytes 2-4/3")
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, data)
			}))
			defer server.Close()
			s, err := New(Options{ServiceURL: server.URL, SASToken: "sig=test"})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			metadata, err = s.encodeMetadata(t.Context(), "bucket", want, hex.EncodeToString(digest[:]), false)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := base64.StdEncoding.DecodeString(val(metadata["gateway"]))
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if len(fields) != 2 || fields["v"] != float64(1) || !strings.HasPrefix(fmt.Sprint(fields["ref"]), detailsPrefix) {
				t.Fatalf("native metadata contains more than a reference: %s", raw)
			}
			calls.Store(0)
			options := backend.ReadOptions{Length: -1, Revision: `"native"`}
			options.DataOnly = scenario == "data-only"
			if scenario == "range" || scenario == "malformed-range" {
				options.Offset, options.Length = 2, 3
			}
			got, body, err := s.Get(t.Context(), "bucket", backend.VersionReference("object", "native-version"), options)
			if scenario == "wrong-revision" || scenario == "malformed-range" || scenario == "missing-metadata" || scenario == "corrupt-metadata" {
				if err == nil || body != nil {
					t.Fatalf("invalid response accepted: %v", err)
				}
				if scenario == "wrong-revision" && !errors.Is(err, backend.ErrPrecondition) {
					t.Fatalf("wrong error: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(body)
				body.Close()
				if err != nil {
					t.Fatal(err)
				}
				expected := "payload"
				if scenario == "range" {
					expected = "ylo"
				}
				if scenario == "data-only" {
					want = backend.Object{ETag: hex.EncodeToString(digest[:])}
				}
				if string(data) != expected || got.Size != 7 || got.ETag != want.ETag || got.VersionID != want.VersionID || !reflect.DeepEqual(got.Metadata, want.Metadata) || !reflect.DeepEqual(got.Checksums, want.Checksums) {
					t.Fatalf("response metadata or bytes changed: %+v, %q", got, data)
				}
			}
			expectedCalls := int32(2)
			if scenario == "data-only" || scenario == "wrong-revision" || scenario == "malformed-range" {
				expectedCalls = 1
			}
			if calls.Load() != expectedCalls {
				t.Fatalf("read used %d requests, want %d", calls.Load(), expectedCalls)
			}
		})
	}
}
