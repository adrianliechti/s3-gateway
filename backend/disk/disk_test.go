package disk

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/s3-gateway/backend"
)

func must(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}
func TestMetadataIsolationAndRestart(t *testing.T) {
	root := t.TempDir()
	s, e := New(Options{Root: root})
	must(t, e)
	ctx := t.Context()
	must(t, s.CreateBucket(ctx, "warehouse"))
	original, e := s.Put(ctx, "warehouse", "a/b", strings.NewReader("content"), 7, backend.PutOptions{Object: backend.Object{Metadata: map[string]string{"hello": "world"}}})
	must(t, e)
	must(t, s.Close())
	s, e = New(Options{Root: root})
	must(t, e)
	defer s.Close()
	o, e := s.Head(ctx, "warehouse", "a/b")
	must(t, e)
	if o.Metadata["hello"] != "world" || o.ETag != original.ETag {
		t.Fatal("metadata did not survive restart")
	}
	buckets, e := s.ListBuckets(ctx)
	must(t, e)
	if len(buckets) != 1 || buckets[0].Name != "warehouse" {
		t.Fatalf("private state exposed: %v", buckets)
	}
	if _, e = os.Stat(filepath.Join(root, ".system", "objects", "warehouse")); e != nil {
		t.Fatal(e)
	}
	// Out-of-band replacement invalidates saved S3 metadata.
	must(t, os.WriteFile(filepath.Join(root, "warehouse", "a", "b"), []byte("external replacement"), 0600))
	o, e = s.Head(ctx, "warehouse", "a/b")
	must(t, e)
	if o.Size != 20 || len(o.Metadata) != 0 || o.ETag == original.ETag {
		t.Fatalf("stale native metadata: %+v", o)
	}
}
func TestRootExclusivityAndTraversal(t *testing.T) {
	root := t.TempDir()
	s, e := New(Options{Root: root})
	must(t, e)
	defer s.Close()
	if other, e := New(Options{Root: root}); e == nil {
		other.Close()
		t.Fatal("second writer accepted")
	}
	must(t, s.CreateBucket(t.Context(), "warehouse"))
	outside := t.TempDir()
	must(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("hidden"), 0600))
	must(t, os.Symlink(outside, filepath.Join(root, "warehouse", "escape")))
	for _, key := range []string{"../.system/LOCK", "a/../b", "/absolute", "escape/secret", "a//b"} {
		_, _, e = s.Get(t.Context(), "warehouse", key, backend.ReadOptions{Length: -1})
		if e == nil {
			t.Fatalf("unsafe key accepted: %s", key)
		}
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestFailedPutKeepsCommittedObject(t *testing.T) {
	s, e := New(Options{Root: t.TempDir()})
	must(t, e)
	defer s.Close()
	ctx := t.Context()
	must(t, s.CreateBucket(ctx, "warehouse"))
	before, e := s.Put(ctx, "warehouse", "key", strings.NewReader("old"), 3, backend.PutOptions{})
	must(t, e)
	_, e = s.Put(ctx, "warehouse", "key", io.MultiReader(strings.NewReader("partial"), failingReader{}), 10, backend.PutOptions{})
	if e == nil {
		t.Fatal("partial upload accepted")
	}
	after, e := s.Head(ctx, "warehouse", "key")
	must(t, e)
	if before.ETag != after.ETag {
		t.Fatal("partial upload published")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, e = s.Put(canceled, "warehouse", "key", strings.NewReader("new"), 3, backend.PutOptions{})
	if !errors.Is(e, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", e)
	}
}
