package azure

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/adrianliechti/s3-gateway/backend"
)

func TestDownloadsPreserveStoredEncoding(t *testing.T) {
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	_, _ = w.Write([]byte("compressed object bytes"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for _, auth := range []string{"key", "sas"} {
		for _, data := range [][]byte{compressed.Bytes(), []byte("opaque bytes labeled gzip")} {
			t.Run(auth+"/"+strconv.Itoa(len(data)), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Accept-Encoding") != "" {
						t.Error("blob transport requested response compression")
					}
					w.Header().Set("Content-Encoding", "gzip")
					w.Header().Set("Content-Type", "application/octet-stream")
					w.Header().Set("Content-Length", strconv.Itoa(len(data)))
					w.Header().Set("ETag", `"native-etag"`)
					w.Header().Set("Last-Modified", "Sun, 04 Oct 2026 12:00:00 GMT")
					if r.Method != http.MethodHead {
						_, _ = w.Write(data)
					}
				}))
				defer server.Close()
				o := Options{Account: "gateway", ServiceURL: server.URL}
				if auth == "key" {
					o.AccountKey = "Y29tcGF0aWJpbGl0eS10ZXN0LWtleS1vbmx5"
				} else {
					o.SASToken = "sig=test"
				}
				s, err := New(o)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				object, body, err := s.Get(t.Context(), "bucket", "object", backend.ReadOptions{Length: -1})
				if err != nil {
					t.Fatal(err)
				}
				defer body.Close()
				got, err := io.ReadAll(body)
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("download altered bytes: length=%d, error=%v", len(got), err)
				}
				if object.ContentEncoding != "gzip" || object.Size != int64(len(data)) {
					t.Fatalf("download altered metadata: %+v", object)
				}
			})
		}
	}
}
