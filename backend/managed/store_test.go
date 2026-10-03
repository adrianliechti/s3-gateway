package managed

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/adrianliechti/s3-gateway/backend"
	"github.com/adrianliechti/s3-gateway/backend/disk"
)

// Simulate loss of publication at the durable index boundary, after native
// content may already have changed. The next handler must replay the journal.
type interrupted struct {
	backend.Backend
	fail bool
}

func (s *interrupted) Put(ctx context.Context, b, k string, body io.Reader, n int64, p backend.PutOptions) (backend.Object, error) {
	if s.fail && strings.HasPrefix(k, historyPrefix) && strings.HasSuffix(k, "/index") {
		s.fail = false
		return backend.Object{}, errors.New("injected index publication failure")
	}
	return s.Backend.Put(ctx, b, k, body, n, p)
}
func TestPublicationRecovery(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	raw, err := disk.New(disk.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	broken := &interrupted{Backend: raw}
	store := New(broken)
	if err = store.CreateBucket(ctx, "bucket"); err != nil {
		t.Fatal(err)
	}
	if err = store.UpdateBucketProperties(ctx, "bucket", func(p *backend.BucketProperties) error { p.Versioning = "Enabled"; return nil }); err != nil {
		t.Fatal(err)
	}
	old, err := store.Put(ctx, "bucket", "key", strings.NewReader("first"), 5, backend.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	broken.fail = true
	if _, err = store.Put(ctx, "bucket", "key", strings.NewReader("second"), 6, backend.PutOptions{}); err == nil {
		t.Fatal("fault injection did not fail")
	}
	// Retain the adapter's fallback settings interface across the restart.
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err = disk.New(disk.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	store = New(&interrupted{Backend: raw})
	o, body, err := store.Get(ctx, "bucket", "key", backend.ReadOptions{Length: -1})
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "second" || o.VersionID == old.VersionID {
		t.Fatal("pending write was not replayed")
	}
	versions, err := store.ListVersions(ctx, "bucket", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 {
		t.Fatalf("history lost during recovery: %d", len(versions))
	}
	internal, _, err := raw.List(ctx, "bucket", backend.InternalPrefix, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	foundHistory := false
	for _, o := range internal {
		if strings.HasPrefix(o.Key, historyPrefix) {
			foundHistory = true
		}
	}
	if !foundHistory {
		t.Fatal("backend internal prefix omitted version storage")
	}
	_, body, err = store.GetVersion(ctx, "bucket", "key", old.VersionID, backend.ReadOptions{Length: -1})
	if err != nil {
		t.Fatal(err)
	}
	content, err = io.ReadAll(body)
	body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "first" {
		t.Fatal("prior version corrupted")
	}
	failed := store.Backend.(*interrupted)
	failed.fail = true
	if _, err = store.DeleteVersion(ctx, "bucket", "key", "", backend.Conditions{}); err == nil {
		t.Fatal("delete publication fault did not fail")
	}
	store = New(&interrupted{Backend: raw})
	marker, err := store.Head(ctx, "bucket", "key")
	if !errors.Is(err, backend.ErrNotFound) || !marker.DeleteMarker {
		t.Fatalf("delete recovery failed: %+v, %v", marker, err)
	}
	if _, err = raw.Head(ctx, "bucket", "key"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatal("native deleted path survived recovery")
	}
	versions, err = store.ListVersions(ctx, "bucket", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 3 {
		t.Fatal("delete recovery lost history")
	}
}

func TestVersionedConditionalWriters(t *testing.T) {
	raw, err := disk.New(disk.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	store := New(raw)
	ctx := t.Context()
	if err = store.CreateBucket(ctx, "bucket"); err != nil {
		t.Fatal(err)
	}
	if err = store.UpdateBucketProperties(ctx, "bucket", func(p *backend.BucketProperties) error { p.Versioning = "Enabled"; return nil }); err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			_, e := store.Put(ctx, "bucket", "key", strings.NewReader("value"), 5, backend.PutOptions{Conditions: backend.Conditions{IfNoneMatch: "*"}})
			if e == nil {
				successes.Add(1)
			} else if !errors.Is(e, backend.ErrPrecondition) {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("conditional create successes: %d", successes.Load())
	}
	versions, err := store.ListVersions(ctx, "bucket", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 {
		t.Fatal("failed conditional writes added versions")
	}
}
