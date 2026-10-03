// Package managed adds durable S3 version history to native-path backends.
// It serializes S3 operations per bucket in this process. Native readers see
// the current object; native writers and multiple gateway processes must not
// mutate versioned buckets. Immutable history and a replayable publication
// journal keep interrupted publications recoverable on the next S3 access.
package managed

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
)

const historyPrefix = backend.InternalPrefix + "versions/"

type Store struct {
	backend.Backend
	properties backend.Properties
	locks      [256]sync.Mutex
}
type Version struct {
	Object   backend.Object
	Data     string
	IsLatest bool
}
type index struct {
	Key      string
	Versions []Version
}

func New(be backend.Backend) *Store {
	p, _ := be.(backend.Properties)
	return &Store{Backend: be, properties: p}
}
func (s *Store) lock(b string) func() {
	h := sha256.Sum256([]byte(b))
	s.locks[h[0]].Lock()
	return s.locks[h[0]].Unlock
}
func newID() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func base(k string) string {
	h := sha256.Sum256([]byte(k))
	return historyPrefix + hex.EncodeToString(h[:]) + "/"
}
func (s *Store) json(ctx context.Context, b, k string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.Backend.Put(ctx, b, k, bytes.NewReader(raw), int64(len(raw)), backend.PutOptions{})
	return err
}
func (s *Store) readJSON(ctx context.Context, b, k string, v any) error {
	_, body, err := s.Backend.Get(ctx, b, k, backend.ReadOptions{Length: -1})
	if err != nil {
		return err
	}
	defer body.Close()
	return json.NewDecoder(body).Decode(v)
}
func (s *Store) settings(ctx context.Context, b string) (backend.BucketProperties, error) {
	if s.properties != nil {
		return s.properties.GetBucketProperties(ctx, b)
	}
	var p backend.BucketProperties
	if err := s.HeadBucket(ctx, b); err != nil {
		return p, err
	}
	err := s.readJSON(ctx, b, backend.InternalPrefix+"settings", &p)
	if errors.Is(err, backend.ErrNotFound) {
		err = nil
	}
	return p, err
}
func (s *Store) GetBucketProperties(ctx context.Context, b string) (backend.BucketProperties, error) {
	u := s.lock(b)
	defer u()
	return s.settings(ctx, b)
}
func (s *Store) UpdateBucketProperties(ctx context.Context, b string, change func(*backend.BucketProperties) error) error {
	u := s.lock(b)
	defer u()
	p, err := s.settings(ctx, b)
	if err != nil {
		return err
	}
	if err = change(&p); err != nil {
		return err
	}
	if s.properties != nil {
		return s.properties.SetBucketProperties(ctx, b, p)
	}
	return s.json(ctx, b, backend.InternalPrefix+"settings", p)
}
func (s *Store) readIndex(ctx context.Context, b, k string) (index, bool, error) {
	var next index
	err := s.readJSON(ctx, b, base(k)+"pending", &next)
	if err == nil {
		if err = s.apply(ctx, b, next); err != nil {
			return next, false, err
		}
	} else if !errors.Is(err, backend.ErrNotFound) {
		return next, false, err
	}
	var state index
	err = s.readJSON(ctx, b, base(k)+"index", &state)
	if errors.Is(err, backend.ErrNotFound) {
		return index{Key: k}, false, nil
	}
	if err == nil && state.Key != k {
		return state, false, fmt.Errorf("version index key mismatch")
	}
	return state, err == nil, err
}

// apply is idempotent: pending is removed only after native publication and
// the authoritative version index are both durable. Unacknowledged mutations
// may complete during recovery, as with an S3 request whose response is lost.
func (s *Store) apply(ctx context.Context, b string, next index) error {
	if len(next.Versions) == 0 || next.Versions[0].Object.DeleteMarker {
		if err := s.Backend.Delete(ctx, b, next.Key, backend.Conditions{}); err != nil {
			return err
		}
	} else {
		v := next.Versions[0]
		_, body, err := s.Backend.Get(ctx, b, v.Data, backend.ReadOptions{Length: -1})
		if err != nil {
			return err
		}
		_, err = s.Backend.Put(ctx, b, next.Key, body, v.Object.Size, backend.PutOptions{Object: v.Object})
		body.Close()
		if err != nil {
			return err
		}
	}
	if err := s.json(ctx, b, base(next.Key)+"index", next); err != nil {
		return err
	}
	return s.Backend.Delete(ctx, b, base(next.Key)+"pending", backend.Conditions{})
}
func (s *Store) commit(ctx context.Context, b string, old, next index) error {
	if err := s.json(ctx, b, base(next.Key)+"pending", next); err != nil {
		return err
	}
	if err := s.apply(ctx, b, next); err != nil {
		return err
	}
	// Garbage collection cannot invalidate a successfully committed operation.
	live := map[string]bool{}
	for _, v := range next.Versions {
		live[v.Data] = true
	}
	for _, v := range old.Versions {
		if v.Data != "" && !live[v.Data] {
			_ = s.Backend.Delete(ctx, b, v.Data, backend.Conditions{})
		}
	}
	return nil
}
func (s *Store) snapshot(ctx context.Context, b, k string, state index, exists bool) (index, error) {
	if exists {
		return state, nil
	}
	o, body, err := s.Backend.Get(ctx, b, k, backend.ReadOptions{Length: -1})
	if errors.Is(err, backend.ErrNotFound) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	defer body.Close()
	o.VersionID = "null"
	data := base(k) + "data/" + newID()
	if _, err = s.Backend.Put(ctx, b, data, body, o.Size, backend.PutOptions{Object: o}); err != nil {
		return state, err
	}
	state.Versions = []Version{{Object: o, Data: data}}
	return state, nil
}
func selectVersion(state index, id string) (Version, error) {
	for i, v := range state.Versions {
		if id == "" && i == 0 || id != "" && v.Object.VersionID == id {
			v.IsLatest = i == 0
			if v.Object.DeleteMarker {
				if id != "" {
					return v, backend.ErrDeleteMarker
				}
				return v, backend.ErrNotFound
			}
			return v, nil
		}
	}
	if id != "" {
		return Version{}, backend.ErrVersionNotFound
	}
	return Version{}, backend.ErrNotFound
}
func (s *Store) resolve(ctx context.Context, b, k, id string) (Version, error) {
	state, exists, err := s.readIndex(ctx, b, k)
	if err != nil {
		return Version{}, err
	}
	if exists {
		return selectVersion(state, id)
	}
	if id != "" && id != "null" {
		return Version{}, backend.ErrVersionNotFound
	}
	o, err := s.Backend.Head(ctx, b, k)
	if errors.Is(err, backend.ErrNotFound) && id != "" {
		err = backend.ErrVersionNotFound
	}
	if err != nil {
		return Version{}, err
	}
	p, err := s.settings(ctx, b)
	if err != nil {
		return Version{}, err
	}
	if p.Versioning != "" || id != "" {
		o.VersionID = "null"
	}
	return Version{Object: o, Data: k, IsLatest: true}, nil
}
func (s *Store) head(ctx context.Context, b, k, id string) (backend.Object, error) {
	v, err := s.resolve(ctx, b, k, id)
	if err != nil {
		return v.Object, err
	}
	actual, err := s.Backend.Head(ctx, b, v.Data)
	v.Object.Revision = actual.Revision
	return v.Object, err
}
func (s *Store) Head(ctx context.Context, b, k string) (backend.Object, error) {
	return s.HeadVersion(ctx, b, k, "")
}
func (s *Store) HeadVersion(ctx context.Context, b, k, id string) (backend.Object, error) {
	if strings.HasPrefix(k, backend.InternalPrefix) {
		return s.Backend.Head(ctx, b, k)
	}
	u := s.lock(b)
	defer u()
	return s.head(ctx, b, k, id)
}
func (s *Store) Get(ctx context.Context, b, k string, r backend.ReadOptions) (backend.Object, io.ReadCloser, error) {
	return s.GetVersion(ctx, b, k, "", r)
}
func (s *Store) GetVersion(ctx context.Context, b, k, id string, r backend.ReadOptions) (backend.Object, io.ReadCloser, error) {
	if strings.HasPrefix(k, backend.InternalPrefix) {
		return s.Backend.Get(ctx, b, k, r)
	}
	u := s.lock(b)
	defer u()
	v, err := s.resolve(ctx, b, k, id)
	if err != nil {
		return v.Object, nil, err
	}
	actual, body, err := s.Backend.Get(ctx, b, v.Data, r)
	v.Object.Revision = actual.Revision
	return v.Object, body, err
}
func (s *Store) Put(ctx context.Context, b, k string, body io.Reader, size int64, p backend.PutOptions) (backend.Object, error) {
	if strings.HasPrefix(k, backend.InternalPrefix) {
		return s.Backend.Put(ctx, b, k, body, size, p)
	}
	u := s.lock(b)
	defer u()
	if validator, ok := s.Backend.(interface{ ValidateKey(string, string) error }); ok {
		if err := validator.ValidateKey(b, k); err != nil {
			return backend.Object{}, err
		}
	}
	settings, err := s.settings(ctx, b)
	if err != nil {
		return backend.Object{}, err
	}
	if settings.Versioning == "" {
		p.Object.VersionID = ""
		return s.Backend.Put(ctx, b, k, body, size, p)
	}
	state, exists, err := s.readIndex(ctx, b, k)
	if err != nil {
		return backend.Object{}, err
	}
	current, err := s.head(ctx, b, k, "")
	if err != nil && !errors.Is(err, backend.ErrNotFound) {
		return current, err
	}
	if e := backend.CheckWriteConditions(current, err == nil, p.Conditions); e != nil {
		return backend.Object{}, e
	}
	state, err = s.snapshot(ctx, b, k, state, exists)
	if err != nil {
		return backend.Object{}, err
	}
	id := "null"
	if settings.Versioning == "Enabled" {
		id = newID()
	}
	p.Object.VersionID = id
	p.Object.DeleteMarker = false
	p.Conditions = backend.Conditions{}
	data := base(k) + "data/" + newID()
	o, err := s.Backend.Put(ctx, b, data, body, size, p)
	if err != nil {
		return o, err
	}
	o.Key = k
	next := index{Key: k, Versions: []Version{{Object: o, Data: data}}}
	for _, v := range state.Versions {
		if v.Object.VersionID != id {
			next.Versions = append(next.Versions, v)
		}
	}
	if err = s.commit(ctx, b, state, next); err != nil {
		return o, err
	}
	return o, nil
}
func (s *Store) Delete(ctx context.Context, b, k string, c backend.Conditions) error {
	_, err := s.DeleteVersion(ctx, b, k, "", c)
	return err
}
func (s *Store) DeleteVersion(ctx context.Context, b, k, id string, c backend.Conditions) (backend.Object, error) {
	if strings.HasPrefix(k, backend.InternalPrefix) {
		return backend.Object{}, s.Backend.Delete(ctx, b, k, c)
	}
	u := s.lock(b)
	defer u()
	if validator, ok := s.Backend.(interface{ ValidateKey(string, string) error }); ok {
		if err := validator.ValidateKey(b, k); err != nil {
			return backend.Object{}, err
		}
	}
	settings, err := s.settings(ctx, b)
	if err != nil {
		return backend.Object{}, err
	}
	state, exists, err := s.readIndex(ctx, b, k)
	if err != nil {
		return backend.Object{}, err
	}
	if settings.Versioning == "" && id == "" {
		return backend.Object{}, s.Backend.Delete(ctx, b, k, c)
	}
	if !exists && id != "" {
		if id != "null" {
			return backend.Object{VersionID: id}, nil
		}
		if _, e := s.Backend.Head(ctx, b, k); errors.Is(e, backend.ErrNotFound) {
			return backend.Object{VersionID: id}, nil
		} else if e != nil {
			return backend.Object{}, e
		}
		return backend.Object{VersionID: id}, s.Backend.Delete(ctx, b, k, c)
	}
	current, err := s.head(ctx, b, k, id)
	if id != "" && errors.Is(err, backend.ErrVersionNotFound) {
		return backend.Object{VersionID: id}, nil
	}
	if id != "" && errors.Is(err, backend.ErrDeleteMarker) {
		err = nil
	}
	if err != nil && !errors.Is(err, backend.ErrNotFound) {
		return current, err
	}
	if e := backend.CheckDeleteConditions(current, err == nil, c); e != nil {
		return backend.Object{}, e
	}
	state, err = s.snapshot(ctx, b, k, state, exists)
	if err != nil {
		return backend.Object{}, err
	}
	next := index{Key: k}
	result := backend.Object{VersionID: id}
	if id == "" {
		markerID := "null"
		if settings.Versioning == "Enabled" {
			markerID = newID()
		}
		result = backend.Object{Key: k, VersionID: markerID, DeleteMarker: true, Modified: time.Now().UTC(), ACL: settings.ACL}
		next.Versions = append(next.Versions, Version{Object: result})
		for _, v := range state.Versions {
			if markerID != "null" || v.Object.VersionID != "null" {
				next.Versions = append(next.Versions, v)
			}
		}
	} else {
		found := false
		for _, v := range state.Versions {
			if v.Object.VersionID == id {
				result = v.Object
				found = true
			} else {
				next.Versions = append(next.Versions, v)
			}
		}
		if !found {
			return result, nil
		}
	}
	err = s.commit(ctx, b, state, next)
	return result, err
}
func (s *Store) recoverAll(ctx context.Context, b string) error {
	after := ""
	for {
		objects, next, err := s.Backend.List(ctx, b, historyPrefix, after, 1000)
		if err != nil {
			return err
		}
		for _, o := range objects {
			if strings.HasSuffix(o.Key, "/pending") {
				var state index
				if err = s.readJSON(ctx, b, o.Key, &state); err != nil {
					return err
				}
				if err = s.apply(ctx, b, state); err != nil {
					return err
				}
			}
		}
		if next == "" {
			return nil
		}
		after = next
	}
}
func (s *Store) versions(ctx context.Context, b, prefix string) ([]Version, error) {
	if err := s.recoverAll(ctx, b); err != nil {
		return nil, err
	}
	out := []Version{}
	seen := map[string]bool{}
	after := ""
	for {
		objects, next, err := s.Backend.List(ctx, b, historyPrefix, after, 1000)
		if err != nil {
			return nil, err
		}
		for _, o := range objects {
			if !strings.HasSuffix(o.Key, "/index") {
				continue
			}
			var state index
			if err = s.readJSON(ctx, b, o.Key, &state); err != nil {
				return nil, err
			}
			seen[state.Key] = true
			if strings.HasPrefix(state.Key, prefix) {
				for i, v := range state.Versions {
					v.IsLatest = i == 0
					out = append(out, v)
				}
			}
		}
		if next == "" {
			break
		}
		after = next
	}
	after = ""
	for {
		objects, next, err := s.Backend.List(ctx, b, prefix, after, 1000)
		if err != nil {
			return nil, err
		}
		for _, o := range objects {
			if !seen[o.Key] {
				o.VersionID = "null"
				out = append(out, Version{Object: o, Data: o.Key, IsLatest: true})
			}
		}
		if next == "" {
			break
		}
		after = next
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Object.Key < out[j].Object.Key })
	return out, nil
}
func (s *Store) ListVersions(ctx context.Context, b, prefix string) ([]Version, error) {
	u := s.lock(b)
	defer u()
	if strings.HasPrefix(prefix, backend.InternalPrefix) {
		return []Version{}, s.Backend.HeadBucket(ctx, b)
	}
	return s.versions(ctx, b, prefix)
}
func (s *Store) List(ctx context.Context, b, prefix, after string, n int) ([]backend.Object, string, error) {
	if strings.HasPrefix(prefix, backend.InternalPrefix) {
		return s.Backend.List(ctx, b, prefix, after, n)
	}
	u := s.lock(b)
	defer u()
	if err := s.recoverAll(ctx, b); err != nil {
		return nil, "", err
	}
	objects, next, err := s.Backend.List(ctx, b, prefix, after, n)
	if err != nil {
		return nil, "", err
	}
	for i, o := range objects {
		if o.VersionID != "" {
			state, exists, e := s.readIndex(ctx, b, o.Key)
			if e != nil {
				return nil, "", e
			}
			if exists && len(state.Versions) > 0 {
				objects[i] = state.Versions[0].Object
			}
		}
	}
	return objects, next, nil
}
func (s *Store) DeleteBucket(ctx context.Context, b string) error {
	u := s.lock(b)
	defer u()
	versions, err := s.versions(ctx, b, "")
	if err != nil {
		return err
	}
	if len(versions) > 0 {
		return backend.ErrBucketNotEmpty
	}
	return s.Backend.DeleteBucket(ctx, b)
}
func (s *Store) SetACL(ctx context.Context, b, k, id string, acl backend.ACL) error {
	_, err := s.updateObjectMetadata(ctx, b, k, id, func(o *backend.Object) { o.ACL = acl })
	return err
}
func (s *Store) SetTags(ctx context.Context, b, k, id string, tags []backend.Tag) (backend.Object, error) {
	return s.updateObjectMetadata(ctx, b, k, id, func(o *backend.Object) { o.Tags = tags })
}
func (s *Store) updateObjectMetadata(ctx context.Context, b, k, id string, update func(*backend.Object)) (backend.Object, error) {
	u := s.lock(b)
	defer u()
	state, exists, err := s.readIndex(ctx, b, k)
	if err != nil {
		return backend.Object{}, err
	}
	v, err := s.resolve(ctx, b, k, id)
	if err != nil {
		return v.Object, err
	}
	update(&v.Object)
	if !exists {
		if s.properties == nil {
			return v.Object, fmt.Errorf("backend does not support metadata updates")
		}
		return v.Object, s.properties.SetObjectMetadata(ctx, b, k, v.Object)
	}
	for i := range state.Versions {
		if state.Versions[i].Object.VersionID == v.Object.VersionID {
			update(&state.Versions[i].Object)
		}
	}
	if v.IsLatest && s.properties != nil {
		current, err := s.Backend.Head(ctx, b, k)
		if err != nil {
			return v.Object, err
		}
		update(&current)
		if err = s.properties.SetObjectMetadata(ctx, b, k, current); err != nil {
			return v.Object, err
		}
	}
	return v.Object, s.json(ctx, b, base(k)+"index", state)
}
