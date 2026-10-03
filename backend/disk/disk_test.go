package disk

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if _, e = os.Stat(filepath.Join(root, "warehouse", ".gateway", "metadata")); e != nil {
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

func TestNativeReplacementWithPreservedSizeAndTime(t *testing.T) {
	root := t.TempDir()
	s, err := New(Options{Root: root})
	must(t, err)
	defer s.Close()
	ctx := t.Context()
	must(t, s.CreateBucket(ctx, "bucket"))
	old, err := s.Put(ctx, "bucket", "key", strings.NewReader("old"), 3, backend.PutOptions{Object: backend.Object{Metadata: map[string]string{"old": "metadata"}}})
	must(t, err)
	target := filepath.Join(root, "bucket", "key")
	st, err := os.Stat(target)
	must(t, err)
	replacement := filepath.Join(root, "replacement")
	must(t, os.WriteFile(replacement, []byte("new"), 0600))
	must(t, os.Chtimes(replacement, st.ModTime(), st.ModTime()))
	must(t, os.Rename(replacement, target))
	current, body, err := s.Get(ctx, "bucket", "key", backend.ReadOptions{Length: -1})
	must(t, err)
	data, err := io.ReadAll(body)
	must(t, err)
	must(t, body.Close())
	digest := md5.Sum([]byte("new"))
	if string(data) != "new" || current.Revision == old.Revision || len(current.Metadata) != 0 || current.ETag != hex.EncodeToString(digest[:]) {
		t.Fatalf("replacement reused stale metadata: %+v, %q", current, data)
	}
	for _, dataOnly := range []bool{false, true} {
		_, body, err := s.Get(ctx, "bucket", "key", backend.ReadOptions{Length: -1, Revision: old.Revision, DataOnly: dataOnly})
		if body != nil {
			body.Close()
		}
		if !errors.Is(err, backend.ErrPrecondition) {
			t.Fatalf("stale revision accepted (DataOnly=%v): %v", dataOnly, err)
		}
	}
	// Reading a native object does not manufacture gateway metadata.
	if _, err := s.root.Stat(metaPath("bucket", "key", current.Revision)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read created metadata: %v", err)
	}
}

func TestDataOnlyReadAndUnconditionalReplacement(t *testing.T) {
	s, err := New(Options{Root: t.TempDir()})
	must(t, err)
	defer s.Close()
	ctx := t.Context()
	must(t, s.CreateBucket(ctx, "bucket"))
	o, err := s.Put(ctx, "bucket", "key", strings.NewReader("content"), 7, backend.PutOptions{})
	must(t, err)
	// A caller that has already resolved metadata must not resolve it a second
	// time to read a guarded body range. Full reads still reject corruption.
	must(t, s.root.WriteFile(metaPath("bucket", "key", o.Revision), []byte("broken JSON"), 0600))
	_, err = s.Head(ctx, "bucket", "key")
	if err == nil {
		t.Fatal("corrupt metadata accepted")
	}
	_, body, err := s.Get(ctx, "bucket", "key", backend.ReadOptions{Offset: 1, Length: 3, Revision: o.Revision, DataOnly: true})
	must(t, err)
	data, err := io.ReadAll(body)
	must(t, err)
	must(t, body.Close())
	if string(data) != "ont" {
		t.Fatalf("wrong range: %q", data)
	}
	_, err = s.Put(ctx, "bucket", "key", strings.NewReader("new"), 3, backend.PutOptions{})
	must(t, err)
	current, err := s.Head(ctx, "bucket", "key")
	must(t, err)
	if current.Size != 3 || current.Revision == o.Revision {
		t.Fatal("unconditional replacement depended on old metadata")
	}
}

func TestPrivateLayoutAndBucketRecreation(t *testing.T) {
	s, err := New(Options{Root: t.TempDir()})
	must(t, err)
	defer s.Close()
	ctx := t.Context()
	must(t, s.CreateBucket(ctx, "bucket"))
	must(t, s.SetBucketProperties(ctx, "bucket", backend.BucketProperties{Versioning: "Enabled"}))
	_, err = s.Put(ctx, "bucket", "public", strings.NewReader("bytes"), 5, backend.PutOptions{})
	must(t, err)
	for _, key := range []string{".gateway/upload/manifest", ".gateway/versions/object/data/id"} {
		_, err = s.Put(ctx, "bucket", key, strings.NewReader("private"), 7, backend.PutOptions{})
		must(t, err)
		_, err = s.root.Stat("bucket/" + key)
		must(t, err)
	}
	raw, err := s.root.ReadFile("bucket/.gateway/settings")
	must(t, err)
	if !strings.Contains(string(raw), "Enabled") {
		t.Fatalf("missing settings: %s", raw)
	}
	objects, _, err := s.List(ctx, "bucket", "", "", 100)
	must(t, err)
	if len(objects) != 1 || objects[0].Key != "public" {
		t.Fatalf("private helpers exposed: %+v", objects)
	}
	internal, _, err := s.List(ctx, "bucket", ".gateway/versions/", "", 100)
	must(t, err)
	if len(internal) != 1 || internal[0].Key != ".gateway/versions/object/data/id" {
		t.Fatalf("wrong internal listing: %+v", internal)
	}
	if err := s.DeleteBucket(ctx, "bucket"); !errors.Is(err, backend.ErrBucketNotEmpty) {
		t.Fatalf("nonempty bucket deleted: %v", err)
	}
	p, err := s.GetBucketProperties(ctx, "bucket")
	must(t, err)
	if p.Versioning != "Enabled" {
		t.Fatal("failed deletion removed settings")
	}
	must(t, s.Delete(ctx, "bucket", "public", backend.Conditions{}))
	must(t, s.DeleteBucket(ctx, "bucket"))
	must(t, s.CreateBucket(ctx, "bucket"))
	p, err = s.GetBucketProperties(ctx, "bucket")
	must(t, err)
	internal, _, err = s.List(ctx, "bucket", ".gateway/", "", 100)
	must(t, err)
	if p.Versioning != "" || len(internal) != 0 {
		t.Fatalf("private state survived recreation: %+v, %+v", p, internal)
	}
}

func TestPrivateSymlinksRejected(t *testing.T) {
	for _, dir := range []string{".gateway", ".gateway/metadata"} {
		t.Run(dir, func(t *testing.T) {
			root := t.TempDir()
			s, err := New(Options{Root: root})
			must(t, err)
			defer s.Close()
			ctx := t.Context()
			must(t, s.CreateBucket(ctx, "bucket"))
			must(t, s.CreateBucket(ctx, "other"))
			must(t, s.root.WriteFile("bucket/key", []byte("native"), 0600))
			target := filepath.Join(root, "bucket", dir)
			must(t, os.MkdirAll(filepath.Dir(target), 0700))
			must(t, os.Symlink(filepath.Join(root, "other"), target))
			_, err = s.Head(ctx, "bucket", "key")
			if !errors.Is(err, backend.ErrInvalidKey) {
				t.Fatalf("metadata symlink followed: %v", err)
			}
			_, err = s.Put(ctx, "bucket", "key", strings.NewReader("replacement"), 11, backend.PutOptions{})
			if !errors.Is(err, backend.ErrInvalidKey) {
				t.Fatalf("metadata written through symlink: %v", err)
			}
			if err = s.CollectGarbage(ctx, "bucket", time.Now()); !errors.Is(err, backend.ErrInvalidKey) {
				t.Fatalf("collector followed symlink: %v", err)
			}
			raw, err := s.root.ReadFile("bucket/key")
			must(t, err)
			if string(raw) != "native" {
				t.Fatal("metadata failure published replacement bytes")
			}
			if dir == ".gateway" {
				if err = s.SetBucketProperties(ctx, "bucket", backend.BucketProperties{}); !errors.Is(err, backend.ErrInvalidKey) {
					t.Fatalf("settings written through symlink: %v", err)
				}
				if _, err = s.GetBucketProperties(ctx, "bucket"); !errors.Is(err, backend.ErrInvalidKey) {
					t.Fatalf("settings read through symlink: %v", err)
				}
			}
		})
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
	for _, key := range []string{"../.gateway/LOCK", "a/../b", "/absolute", "escape/secret", "a//b"} {
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
