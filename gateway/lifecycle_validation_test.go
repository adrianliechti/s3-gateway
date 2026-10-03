package gateway

import (
	"encoding/xml"
	"errors"
	"testing"

	"github.com/adrianliechti/s3-gateway/backend"
)

func TestLifecycleValidation(t *testing.T) {
	for _, tc := range []struct{ name, rule, code string }{
		{"zero days", "<Prefix/><Expiration><Days>0</Days></Expiration>", "InvalidArgument"},
		{"mixed filter", "<Prefix/><Filter/><Expiration><Days>1</Days></Expiration>", "InvalidArgument"},
		{"missing action", "<Filter/>", "MalformedXML"},
		{"invalid UTC time", "<Filter/><Expiration><Date>2026-10-03T01:00:00Z</Date></Expiration>", "InvalidArgument"},
		{"mixed expiration", "<Filter/><Expiration><Days>1</Days><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration>", "MalformedXML"},
		{"unimplemented transition", "<Filter/><Transition><Days>1</Days><StorageClass>GLACIER</StorageClass></Transition>", "NotImplemented"},
		{"unknown action", "<Filter/><Expiration><Days>1</Days></Expiration><Typo>value</Typo>", "MalformedXML"},
		{"tag abort", "<Filter><Tag><Key>type</Key><Value>temp</Value></Tag></Filter><AbortIncompleteMultipartUpload><DaysAfterInitiation>1</DaysAfterInitiation></AbortIncompleteMultipartUpload>", "InvalidArgument"},
		{"retention without Filter", "<Prefix/><NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays><NewerNoncurrentVersions>2</NewerNoncurrentVersions></NoncurrentVersionExpiration>", "InvalidArgument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c backend.LifecycleConfiguration
			if err := xml.Unmarshal([]byte("<LifecycleConfiguration><Rule><Status>Enabled</Status>"+tc.rule+"</Rule></LifecycleConfiguration>"), &c); err != nil {
				t.Fatal(err)
			}
			err := validateLifecycle(&c)
			var api *s3Error
			if !errors.As(err, &api) || api.Code != tc.code {
				t.Fatalf("got %v want %s", err, tc.code)
			}
		})
	}
}
