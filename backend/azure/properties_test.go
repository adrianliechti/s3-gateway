package azure

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
)

func TestBucketSettingsUseSharedObject(t *testing.T) {
	var mu sync.Mutex
	var saved []byte
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.URL.Path != "/bucket/.gateway/settings" {
			t.Errorf("unexpected settings location: %s", r.URL)
			w.WriteHeader(400)
			return
		}
		switch r.Method {
		case "PUT":
			var err error
			saved, err = io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			w.WriteHeader(201)
		case "GET":
			if saved == nil {
				w.Header().Set("X-Ms-Error-Code", "BlobNotFound")
				w.WriteHeader(404)
				return
			}
			_, _ = w.Write(saved)
		default:
			t.Errorf("unexpected settings operation: %s", r.Method)
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	s, err := New(Options{ServiceURL: server.URL, SASToken: "sig=test"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.GetBucketProperties(t.Context(), "bucket")
	if err != nil || !reflect.DeepEqual(got, backend.BucketProperties{}) {
		t.Fatalf("native bucket did not get defaults: %+v, %v", got, err)
	}
	// Exceed container metadata capacity: settings must still use a single
	// JSON object and one native PUT/GET, with no overflow helper or pointer.
	want := backend.BucketProperties{Versioning: "Enabled", ABAC: "Enabled", Tags: []backend.Tag{{Key: "large", Value: strings.Repeat("x", 10000)}}}
	if err = s.SetBucketProperties(t.Context(), "bucket", want); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetBucketProperties(t.Context(), "bucket")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("settings changed: %+v, %v", got, err)
	}
	mu.Lock()
	defer mu.Unlock()
	raw, _ := json.Marshal(want)
	if string(saved) != string(raw) {
		t.Fatal("settings are not the shared JSON format")
	}
	if !reflect.DeepEqual(calls, []string{"GET /bucket/.gateway/settings", "PUT /bucket/.gateway/settings", "GET /bucket/.gateway/settings"}) {
		t.Fatalf("unexpected round trips: %v", calls)
	}
}

func TestBucketSettingsErrorsDoNotBecomeDefaults(t *testing.T) {
	for _, scenario := range []string{"missing-bucket", "corrupt"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scenario == "missing-bucket" {
					w.Header().Set("X-Ms-Error-Code", "ContainerNotFound")
					w.WriteHeader(404)
				} else {
					_, _ = io.WriteString(w, "invalid JSON")
				}
			}))
			defer server.Close()
			s, err := New(Options{ServiceURL: server.URL, SASToken: "sig=test"})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			_, err = s.GetBucketProperties(t.Context(), "bucket")
			if err == nil || scenario == "missing-bucket" && !errors.Is(err, backend.ErrBucketNotFound) {
				t.Fatalf("wrong settings error: %v", err)
			}
		})
	}
}

func TestSettingsHistoryCollection(t *testing.T) {
	var mu sync.Mutex
	var removed []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			mu.Lock()
			removed = append(removed, r.URL.Path+"?versionid="+r.URL.Query().Get("versionid"))
			mu.Unlock()
			w.WriteHeader(202)
			return
		}
		if r.Method != "GET" || r.URL.Query().Get("comp") != "list" {
			t.Errorf("unexpected collection request: %s %s", r.Method, r.URL)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<EnumerationResults><Blobs>`)
		for _, item := range []struct {
			key, version, modified string
			current                bool
		}{
			{settingsKey, "old", "Thu, 01 Oct 2026 12:00:00 GMT", false},
			{settingsKey, "young", "Sun, 04 Oct 2026 12:00:00 GMT", false},
			{settingsKey, "current", "Thu, 01 Oct 2026 12:00:00 GMT", true},
			{"native-object", "native-old", "Thu, 01 Oct 2026 12:00:00 GMT", false},
		} {
			_, _ = fmt.Fprintf(w, `<Blob><Name>%s</Name><VersionId>%s</VersionId><IsCurrentVersion>%t</IsCurrentVersion><Properties><Last-Modified>%s</Last-Modified><Etag>"revision"</Etag><Content-Length>2</Content-Length><BlobType>BlockBlob</BlobType></Properties></Blob>`, item.key, item.version, item.current, item.modified)
		}
		_, _ = io.WriteString(w, `</Blobs><NextMarker/></EnumerationResults>`)
	}))
	defer server.Close()
	s, err := New(Options{ServiceURL: server.URL, SASToken: "sig=test"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CollectGarbage(t.Context(), "bucket", time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(removed, []string{"/bucket/.gateway/settings?versionid=old"}) {
		t.Fatalf("collection must preserve current settings, young versions and native history: %v", removed)
	}
}
