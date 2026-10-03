package disk

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
)

func TestCollectionRetainsCurrentMetadata(t *testing.T) {
	s, err := New(Options{Root: t.TempDir()})
	must(t, err)
	defer s.Close()
	ctx := t.Context()
	must(t, s.CreateBucket(ctx, "bucket"))
	old, err := s.Put(ctx, "bucket", "key", strings.NewReader("old"), 3, backend.PutOptions{})
	must(t, err)
	current, err := s.Put(ctx, "bucket", "key", strings.NewReader("new content"), 11, backend.PutOptions{Object: backend.Object{Metadata: map[string]string{"kept": "value"}}})
	must(t, err)
	removed, err := s.Put(ctx, "bucket", "removed", strings.NewReader("removed"), 7, backend.PutOptions{})
	must(t, err)
	must(t, s.Delete(ctx, "bucket", "removed", backend.Conditions{}))
	must(t, s.CollectGarbage(ctx, "bucket", time.Now()))
	for _, o := range []backend.Object{old, removed} {
		if _, err = s.root.Stat(metaPath("bucket", o.Key, o.Revision)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("obsolete generation retained: %v", err)
		}
	}
	_, err = s.root.Stat(metaPath("bucket", current.Key, current.Revision))
	must(t, err)
	o, err := s.Head(ctx, "bucket", "key")
	must(t, err)
	if o.Metadata["kept"] != "value" {
		t.Fatal("current metadata lost")
	}
}

func TestRestartClearsOnlyOwnedStaging(t *testing.T) {
	root := t.TempDir()
	s, err := New(Options{Root: root})
	must(t, err)
	must(t, os.MkdirAll(filepath.Join(root, ".gateway", "staging"), 0700))
	staging := filepath.Join(root, ".gateway", "staging", "interrupted")
	must(t, os.WriteFile(staging, []byte("staged body"), 0600))
	other, err := New(Options{Root: root})
	if err == nil {
		other.Close()
		t.Fatal("second writer accepted")
	}
	_, err = os.Stat(staging)
	must(t, err) // A failed competing open must not remove live staging.
	must(t, s.Close())
	s, err = New(Options{Root: root})
	must(t, err)
	defer s.Close()
	if _, err = os.Stat(staging); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("abandoned staging retained: %v", err)
	}
}
