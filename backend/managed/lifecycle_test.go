package managed

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
	"github.com/adrianliechti/s3-gateway/backend/disk"
)

func TestLifecyclePublicationRecovery(t *testing.T) {
	ctx := t.Context()
	raw, err := disk.New(disk.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	broken := &interrupted{Backend: raw}
	s := New(broken)
	if err = s.CreateBucket(ctx, "bucket"); err != nil {
		t.Fatal(err)
	}
	prefix := ""
	days := 1
	if err = s.UpdateBucketProperties(ctx, "bucket", func(p *backend.BucketProperties) error {
		p.Versioning = "Enabled"
		p.Lifecycle = &backend.LifecycleConfiguration{Rules: []backend.LifecycleRule{{ID: "expire", Status: "Enabled", Prefix: &prefix, Expiration: &backend.LifecycleExpiration{Days: &days}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	old, err := s.Put(ctx, "bucket", "key", strings.NewReader("history"), 7, backend.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	broken.fail = true
	if err = s.ExpireBucket(ctx, "bucket", time.Now().Add(72*time.Hour), 0); err == nil {
		t.Fatal("fault injection did not fail")
	}
	s = New(&interrupted{Backend: raw})
	marker, err := s.Head(ctx, "bucket", "key")
	if !errors.Is(err, backend.ErrNotFound) || !marker.DeleteMarker {
		t.Fatalf("expiration journal not recovered: %+v %v", marker, err)
	}
	if _, err = s.HeadVersion(ctx, "bucket", "key", old.VersionID); err != nil {
		t.Fatal("expiration lost immutable history", err)
	}
}

func TestLifecycleNoncurrentAgeSurvivesDeletionAndRestart(t *testing.T) {
	ctx := t.Context()
	raw, err := disk.New(disk.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	s := New(raw)
	if err = s.CreateBucket(ctx, "bucket"); err != nil {
		t.Fatal(err)
	}
	prefix := ""
	days := 1
	if err = s.UpdateBucketProperties(ctx, "bucket", func(p *backend.BucketProperties) error {
		p.Versioning = "Enabled"
		p.Lifecycle = &backend.LifecycleConfiguration{Rules: []backend.LifecycleRule{{ID: "history", Status: "Enabled", Prefix: &prefix, NoncurrentExpiration: &backend.NoncurrentExpiration{Days: days}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err = s.Put(ctx, "bucket", "key", strings.NewReader("data"), 4, backend.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	state, _, err := s.readIndex(ctx, "bucket", "key")
	if err != nil {
		t.Fatal(err)
	}
	// The oldest became noncurrent five days ago; the middle one only today.
	now := time.Now().UTC()
	state.Versions[2].NoncurrentSince = now.Add(-5 * 24 * time.Hour)
	state.Versions[1].NoncurrentSince = now
	if err = s.json(ctx, "bucket", base("key")+"index", state); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DeleteVersion(ctx, "bucket", "key", state.Versions[1].Object.VersionID, backend.Conditions{}); err != nil {
		t.Fatal(err)
	}
	s = New(raw)
	if err = s.ExpireBucket(ctx, "bucket", now, 0); err != nil {
		t.Fatal(err)
	}
	versions, err := s.ListVersions(ctx, "bucket", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 || versions[0].Object.VersionID != state.Versions[0].Object.VersionID {
		t.Fatal("deleting an intermediate version changed the oldest version's age")
	}
}

func TestLifecycleRechecksChanges(t *testing.T) {
	ctx := t.Context()
	raw, err := disk.New(disk.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	s := New(raw)
	if err = s.CreateBucket(ctx, "bucket"); err != nil {
		t.Fatal(err)
	}
	prefix := ""
	days := 1
	policy := &backend.LifecycleConfiguration{Rules: []backend.LifecycleRule{{ID: "expire", Status: "Enabled", Prefix: &prefix, Expiration: &backend.LifecycleExpiration{Days: &days}}}}
	if err = s.UpdateBucketProperties(ctx, "bucket", func(p *backend.BucketProperties) error { p.Lifecycle = policy; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Put(ctx, "bucket", "key", strings.NewReader("old"), 3, backend.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	// Reproduce a replacement after key enumeration but before its expiration.
	before := time.Now()
	if _, err = s.Put(ctx, "bucket", "key", strings.NewReader("new"), 3, backend.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err = s.expireKey(ctx, "bucket", "key", before, time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Head(ctx, "bucket", "key"); err != nil {
		t.Fatal("scan deleted a newer replacement", err)
	}
	if err = s.UpdateBucketProperties(ctx, "bucket", func(p *backend.BucketProperties) error { p.Lifecycle = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	if err = s.expireKey(ctx, "bucket", "key", before.Add(72*time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Head(ctx, "bucket", "key"); err != nil {
		t.Fatal("scan used a removed lifecycle policy", err)
	}
}
