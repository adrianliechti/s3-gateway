package conformance_test

import (
	"reflect"
	"testing"
)

func TestCleanupTracksDeleteMarkersAndBatchKeys(t *testing.T) {
	h := &H{}
	h.track(request{method: "DELETE", bucket: "bucket", key: "missing"})
	h.track(request{method: "POST", bucket: "bucket", query: q("delete", ""), body: []byte(`<Delete><Object><Key>a&amp;b</Key></Object><Object><Key>missing</Key></Object></Delete>`)})
	h.track(request{method: "PUT", bucket: "bucket", key: "uploaded"})
	h.track(request{method: "GET", bucket: "bucket", key: "unrelated"})
	h.track(request{method: "HEAD", bucket: "bucket", key: "unrelated"})
	want := map[string]map[string]bool{"bucket": {"missing": true, "a&b": true, "uploaded": true}}
	if !reflect.DeepEqual(h.keys, want) {
		t.Fatalf("cleanup ownership: got %v, want %v", h.keys, want)
	}
}
