package azure

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/adrianliechti/s3-gateway/backend"
)

var _ backend.NativeHistory = (*Store)(nil)

func (s *Store) versionClient(b, k string) (*blockblob.Client, error) {
	key, version, reference, err := backend.ParseVersionReference(k)
	if err != nil {
		return nil, err
	}
	if reference {
		return s.bc(b, key).WithVersionID(version)
	}
	return s.bc(b, k), nil
}

// Azure account-level versioning is configured by the operator. Do not change
// account settings in response to an S3 per-bucket versioning request.
func (s *Store) NativeVersion(ctx context.Context, b, k string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, err := s.bc(b, k).GetProperties(ctx, nil)
	if err != nil {
		return "", translate(err)
	}
	if val(p.VersionID) == "" {
		return "", nil
	}
	return backend.VersionReference(k, val(p.VersionID)), nil
}
