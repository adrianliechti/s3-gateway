package aws

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/adrianliechti/s3-gateway/backend"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

func TestProviderListingsDecodeKeysAndPaginate(t *testing.T) {
	const encoded = "dir%2Fctrl%01%2B%252F%20%E9%9B%AA"
	const key = "dir/ctrl\x01+%2F 雪"
	for _, versions := range []bool{false, true} {
		t.Run(fmt.Sprint("versions=", versions), func(t *testing.T) {
			requests := 0
			client := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String("http://provider.test"), UsePathStyle: true,
				Credentials: aws.AnonymousCredentials{}, HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					requests++
					q := r.URL.Query()
					if q.Get("encoding-type") != "url" {
						t.Error("listing omitted encoding-type=url")
					}
					if requests == 2 {
						if versions && (q.Get("key-marker") != key || q.Get("version-id-marker") != "opaque%2B+") {
							t.Errorf("version markers were not preserved: %v", q)
						}
						if !versions && q.Get("continuation-token") != "opaque%2B+" {
							t.Errorf("continuation token was decoded: %v", q)
						}
					}
					body := fmt.Sprintf(`<ListBucketResult><EncodingType>url</EncodingType><IsTruncated>%t</IsTruncated><NextContinuationToken>opaque%%2B+</NextContinuationToken><Contents><Key>%s</Key></Contents><CommonPrefixes><Prefix>%s</Prefix></CommonPrefixes></ListBucketResult>`, requests == 1, encoded, encoded)
					if versions {
						body = fmt.Sprintf(`<ListVersionsResult><EncodingType>url</EncodingType><IsTruncated>%t</IsTruncated><NextKeyMarker>%s</NextKeyMarker><NextVersionIdMarker>opaque%%2B+</NextVersionIdMarker><Version><Key>%s</Key><VersionId>opaque%%2B+</VersionId></Version><DeleteMarker><Key>%s</Key></DeleteMarker><CommonPrefixes><Prefix>%s</Prefix></CommonPrefixes></ListVersionsResult>`, requests == 1, encoded, encoded, encoded, encoded)
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/xml"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}})
			if versions {
				pager := s3.NewListObjectVersionsPaginator(listingClient{client}, &s3.ListObjectVersionsInput{Bucket: aws.String("bucket")})
				for pager.HasMorePages() {
					page, err := pager.NextPage(t.Context())
					check(t, err)
					if aws.ToString(page.Versions[0].Key) != key || aws.ToString(page.DeleteMarkers[0].Key) != key || aws.ToString(page.CommonPrefixes[0].Prefix) != key || aws.ToString(page.Versions[0].VersionId) != "opaque%2B+" {
						t.Fatalf("version listing changed keys or IDs: %+v", page)
					}
				}
			} else {
				pager := s3.NewListObjectsV2Paginator(listingClient{client}, &s3.ListObjectsV2Input{Bucket: aws.String("bucket")})
				for pager.HasMorePages() {
					page, err := pager.NextPage(t.Context())
					check(t, err)
					if aws.ToString(page.Contents[0].Key) != key || aws.ToString(page.CommonPrefixes[0].Prefix) != key {
						t.Fatalf("object listing changed keys: %+v", page)
					}
				}
			}
			if requests != 2 {
				t.Fatalf("expected two pages, got %d", requests)
			}
		})
	}
}

func TestTranslateProviderValidationErrors(t *testing.T) {
	for code, want := range map[string]error{"InvalidArgument": backend.ErrInvalidKey, "InvalidRequest": backend.ErrInvalidRequest} {
		err := fmt.Errorf("provider: %w", &smithy.GenericAPIError{Code: code, Fault: smithy.FaultClient})
		wantError(t, translate(err), want)
	}
	for _, code := range []string{"InternalError", "AccessDenied", "SlowDown"} {
		err := &smithy.GenericAPIError{Code: code}
		if translate(err) != err {
			t.Fatalf("provider failure %s was misclassified as a client error", code)
		}
	}
}
