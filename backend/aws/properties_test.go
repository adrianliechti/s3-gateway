package aws

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/adrianliechti/s3-gateway/backend"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestMetadataPreservesRedirectWhenProviderOmitsIt(t *testing.T) {
	var manifest string
	tr := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := manifest
		if r.Method == "PUT" {
			raw, err := io.ReadAll(r.Body)
			check(t, err)
			manifest, body = string(raw), ""
		} else if r.Method != "GET" {
			t.Fatalf("unexpected request: %s", r.Method)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	s := &Store{client: s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String("http://provider.test"), UsePathStyle: true, Credentials: aws.AnonymousCredentials{}, HTTPClient: &http.Client{Transport: tr}})}
	want := backend.Object{Size: 3, WebsiteRedirectLocation: "/destination"}
	m, err := s.encodeMetadata(t.Context(), "bucket", want)
	check(t, err)
	ref, err := base64.StdEncoding.DecodeString(m["gateway"])
	check(t, err)
	var fields map[string]any
	check(t, json.Unmarshal(ref, &fields))
	if len(fields) != 2 || fields["v"] != float64(1) || !strings.HasPrefix(fields["ref"].(string), detailsPrefix) {
		t.Fatalf("native metadata contains more than a reference: %s", ref)
	}
	got, err := s.decodeMetadata(t.Context(), "bucket", backend.Object{Size: 3, Metadata: m})
	check(t, err)
	if got.WebsiteRedirectLocation != want.WebsiteRedirectLocation {
		t.Fatalf("redirect lost in provider round trip: %+v", got)
	}
}

func TestNativeRedirectHeaders(t *testing.T) {
	for _, operation := range []string{"put", "multipart"} {
		t.Run(operation, func(t *testing.T) {
			requests := 0
			tr := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasPrefix(r.URL.Path, "/bucket/"+detailsPrefix) {
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
				}
				requests++
				if got := r.Header.Get("X-Amz-Website-Redirect-Location"); got != "/destination" {
					t.Errorf("native redirect header: got %q", got)
				}
				body := ""
				if operation == "multipart" {
					if r.Method != "POST" || !r.URL.Query().Has("uploads") {
						t.Errorf("expected multipart initiation, got %s %s", r.Method, r.URL)
					}
					body = `<InitiateMultipartUploadResult><UploadId>upload</UploadId></InitiateMultipartUploadResult>`
				} else if r.Method != "PUT" {
					t.Errorf("expected PUT, got %s", r.Method)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"ETag": {`"native"`}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			s := &Store{client: s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String("http://provider.test"), UsePathStyle: true, Credentials: aws.AnonymousCredentials{}, HTTPClient: &http.Client{Transport: tr}})}
			o := backend.Object{WebsiteRedirectLocation: "/destination"}
			if operation == "multipart" {
				_, err := s.startUpload(t.Context(), "bucket", "key", o, nil)
				check(t, err)
			} else {
				_, err := s.put(t.Context(), "bucket", "key", strings.NewReader("abc"), 3, o, nil, nil, false)
				check(t, err)
			}
			if requests != 1 {
				t.Fatalf("native request count: %d", requests)
			}
		})
	}
}
