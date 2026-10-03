package disk

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"

	"github.com/adrianliechti/s3-gateway/backend"
)

func (s *Store) ValidateKey(b, k string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, err := location(b, k)
	if err != nil {
		return err
	}
	if err = s.noSymlinks(p); err != nil {
		return err
	}
	st, err := s.root.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return backend.ErrInvalidKey
	}
	return nil
}

func (s *Store) GetBucketProperties(ctx context.Context, b string) (backend.BucketProperties, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var p backend.BucketProperties
	if err := s.HeadBucket(ctx, b); err != nil {
		return p, err
	}
	raw, err := s.root.ReadFile(path.Join(".system/buckets", b, "settings.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(raw, &p)
	return p, err
}
func (s *Store) SetBucketProperties(ctx context.Context, b string, p backend.BucketProperties) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.HeadBucket(ctx, b); err != nil {
		return err
	}
	return s.writeJSON(path.Join(".system/buckets", b, "settings.json"), p)
}
func (s *Store) writeJSON(target string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err = s.root.MkdirAll(path.Dir(target), 0700); err != nil {
		return err
	}
	tmp := target + "." + randomID() + ".tmp"
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() { f.Close(); s.root.Remove(tmp) }()
	if _, err = f.Write(raw); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = s.root.Rename(tmp, target); err != nil {
		return err
	}
	return syncDir(s.root, path.Dir(target))
}
func (s *Store) SetObjectMetadata(ctx context.Context, b, k string, o backend.Object) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.head(ctx, b, k)
	if err != nil {
		return err
	}
	if o.Revision != "" && o.Revision != current.Revision {
		return backend.ErrPrecondition
	}
	current.ACL = o.ACL
	current.Tags = o.Tags
	return s.writeJSON(metaPath(b, k, current.Revision), current)
}
