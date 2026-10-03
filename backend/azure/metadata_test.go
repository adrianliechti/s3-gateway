package azure

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/adrianliechti/s3-gateway/backend"
)

func TestNativeMetadataRoundTripAndExternalOverwrite(t *testing.T) {
	o := backend.Object{Size: 42, ETag: "multipart-2", ContentType: "application/parquet", Expires: "tomorrow", Metadata: map[string]string{"format": "parquet", "with-hyphen": "value", "s3gw": "user value", "unicode": "雪"}, Checksums: map[string]string{"CRC32": "AAAAAA==-2"}, ChecksumType: "COMPOSITE"}
	m, err := encode(o, "native-md5")
	if err != nil {
		t.Fatal(err)
	}
	if val(m["format"]) != "parquet" || m["with-hyphen"] != nil || m["unicode"] != nil {
		t.Fatal("native metadata mapping differs")
	}
	native := backend.Object{Size: 42, ETag: "native-md5", ContentType: "application/parquet"}
	got, err := decode(native, m)
	if err != nil {
		t.Fatal(err)
	}
	if got.ETag != o.ETag || !reflect.DeepEqual(got.Metadata, o.Metadata) || !reflect.DeepEqual(got.Checksums, o.Checksums) || got.ContentType != o.ContentType {
		t.Fatalf("round trip differs: %#v", got)
	}
	m["format"] = ptr("native-edit")
	got, _ = decode(native, m)
	if got.Metadata["format"] != "native-edit" {
		t.Fatal("native metadata edits were hidden")
	}
	native.ETag = "replaced-native-content"
	got, _ = decode(native, m)
	if got.ETag != native.ETag || len(got.Checksums) != 0 || got.Metadata["s3gw"] != "" || got.Metadata["format"] != "native-edit" {
		t.Fatalf("stale metadata reused: %#v", got)
	}
	got, _ = decode(native, map[string]*string{"s3gw": ptr("unrelated native field")})
	if got.Metadata["s3gw"] != "unrelated native field" {
		t.Fatal("native field lost")
	}
}

func TestAzureMetadataCapacity(t *testing.T) {
	_, err := encode(backend.Object{Metadata: map[string]string{"large": strings.Repeat("x", 8192)}}, "md5")
	if !errors.Is(err, backend.ErrMetadataTooLarge) {
		t.Fatalf("expected metadata limit, got %v", err)
	}
}
