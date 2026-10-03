package managed

import (
	"context"
	"encoding/hex"
	"strings"

	"github.com/adrianliechti/s3-gateway/backend"
)

func (s *Store) copyData(ctx context.Context, b, k, source string, o backend.Object) error {
	if composer, ok := s.Backend.(backend.Composer); ok {
		actual, err := s.Backend.Head(ctx, b, source)
		if err != nil {
			return err
		}
		_, err = composer.Compose(ctx, b, k, []backend.ComposeSource{{Key: source, Size: actual.Size, Revision: actual.Revision}}, backend.PutOptions{Object: o})
		return err
	}
	_, body, err := s.Backend.Get(ctx, b, source, backend.ReadOptions{Length: -1})
	if err != nil {
		return err
	}
	defer body.Close()
	_, err = s.Backend.Put(ctx, b, k, body, o.Size, backend.PutOptions{Object: o})
	return err
}

func (s *Store) CompleteComposed(ctx context.Context, b, k, id string, sources []backend.ComposeSource, size int64, p backend.PutOptions, manifest func(backend.Object) ([]byte, error)) (backend.Object, error) {
	if len(id) != 32 || manifest == nil || strings.HasPrefix(k, backend.InternalPrefix) {
		return backend.Object{}, backend.ErrInvalidKey
	}
	if _, err := hex.DecodeString(id); err != nil {
		return backend.Object{}, backend.ErrInvalidKey
	}
	unlock := s.lockKey(b, k)
	defer unlock()
	return s.put(ctx, b, k, nil, size, p, id, manifest, sources)
}
