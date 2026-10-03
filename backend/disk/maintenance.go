package disk

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"path"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
)

// CollectGarbage drops obsolete metadata generations under the same lock as
// publication. Current native files, versions and upload parts keep their
// matching generation. Unknown or corrupt records stop the scan.
func (s *Store) CollectGarbage(ctx context.Context, b string, cutoff time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.HeadBucket(ctx, b); err != nil {
		return err
	}
	dir := path.Join(b, backend.InternalPrefix, "metadata")
	if err := s.noSymlinks(dir); err != nil {
		return err
	}
	var garbage []string
	err := fs.WalkDir(s.root.FS(), dir, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && p == dir {
			return nil
		}
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		id, err := hex.DecodeString(d.Name())
		if err != nil || len(id) != 32 {
			return nil
		}
		if err := s.noSymlinks(p); err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			return err
		}
		raw, err := s.root.ReadFile(p)
		if err != nil {
			return err
		}
		var o backend.Object
		if err = json.Unmarshal(raw, &o); err != nil {
			return err
		}
		f, err := s.open(ctx, b, o.Key)
		if errors.Is(err, backend.ErrNotFound) {
			garbage = append(garbage, p)
			return nil
		}
		if err != nil {
			return err
		}
		current, err := f.Stat()
		f.Close()
		if err != nil {
			return err
		}
		if p != metaPath(b, o.Key, fingerprint(current)) {
			garbage = append(garbage, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, p := range garbage {
		if err = s.root.Remove(p); err != nil {
			return err
		}
	}
	return nil
}
