package managed

import (
	"context"
	"io"
	"sync"

	"github.com/adrianliechti/s3-gateway/backend"
)

type readContextKey struct{}
type readContext struct {
	store    *Store
	mu       sync.Mutex
	settings map[string]backend.BucketProperties
}

// ReadContext shares bucket configuration within one read-only request. Each
// request gets a fresh snapshot so ABAC, CORS and lifecycle changes remain
// visible to subsequent requests. Mutation paths must use the original context.
func (s *Store) ReadContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, readContextKey{}, &readContext{store: s, settings: make(map[string]backend.BucketProperties)})
}

func (s *Store) cachedSettings(ctx context.Context) *readContext {
	c, _ := ctx.Value(readContextKey{}).(*readContext)
	if c != nil && c.store == s {
		return c
	}
	return nil
}

func (s *Store) readState(ctx context.Context, b, k, id string) (index, bool, error) {
	if s.cachedSettings(ctx) != nil && (id == "" || id == "null") {
		p, err := s.settings(ctx, b)
		if err != nil {
			return index{}, false, err
		}
		if p.Versioning == "" {
			// Versioning cannot be reset to its virgin state through the API.
			// These buckets have no history index. A pending multipart commit
			// must still recover before reading the native object.
			return index{Key: k}, false, s.recoverPending(ctx, b, k)
		}
	}
	return s.readIndex(ctx, b, k)
}

func (s *Store) resolveHead(ctx context.Context, b, k, id string) (Version, error) {
	v, err := s.resolve(ctx, b, k, id)
	if err != nil || v.Data == k {
		// Native-path resolution has already fetched the object's metadata.
		return v, err
	}
	actual, err := s.Backend.Head(ctx, b, v.Data)
	v.Object.Revision = actual.Revision
	return v, err
}

// ReadVersion resolves and recovers the requested generation once, checks the
// client's conditions/range, and opens that generation with its native revision.
// prepare must not call back into the store; it runs under the bucket lock.
func (s *Store) ReadVersion(ctx context.Context, b, k, id string, prepare func(backend.Object) (backend.ReadOptions, error)) (backend.Object, io.ReadCloser, error) {
	unlock := s.lockKey(b, k)
	defer unlock()
	v, err := s.resolveHead(ctx, b, k, id)
	if err != nil {
		return v.Object, nil, err
	}
	options, err := prepare(v.Object)
	if err != nil {
		return v.Object, nil, err
	}
	if options.Length == 0 {
		return v.Object, nil, nil
	}
	options.Revision = v.Object.Revision
	options.DataOnly = true
	_, body, err := s.Backend.Get(ctx, b, v.Data, options)
	return v.Object, body, err
}
