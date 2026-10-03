package aws

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/adrianliechti/s3-gateway/backend"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

var _ backend.NativeHistory = (*Store)(nil)

func (s *Store) NativeVersion(ctx context.Context, b, k string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &b, Key: aws.String(k)})
	if err != nil {
		return "", s.objectError(ctx, b, err)
	}
	if aws.ToString(p.VersionId) == "" {
		return "", nil
	}
	return backend.VersionReference(k, aws.ToString(p.VersionId)), nil
}

// Gateway helpers do not need their own version history. Remove their native
// versions as well as delete markers when retiring a helper in a versioned bucket.
func (s *Store) purgeKey(ctx context.Context, b, k string) error {
	pager := s3.NewListObjectVersionsPaginator(listingClient{s.client}, &s3.ListObjectVersionsInput{Bucket: &b, Prefix: aws.String(k)})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return translate(err)
		}
		for _, v := range page.Versions {
			if aws.ToString(v.Key) == k {
				if _, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &b, Key: aws.String(k), VersionId: v.VersionId}); err != nil {
					return translate(err)
				}
			}
		}
		for _, v := range page.DeleteMarkers {
			if aws.ToString(v.Key) == k {
				if _, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &b, Key: aws.String(k), VersionId: v.VersionId}); err != nil {
					return translate(err)
				}
			}
		}
	}
	return nil
}

func (s *Store) purgeEmptyBucket(ctx context.Context, b string) error {
	// Shared indexes identify keys whose logical versions were removed. Do not
	// erase unrelated, pre-existing native history merely because no current
	// objects remain in the bucket.
	known := make(map[string]bool)
	indexes := s3.NewListObjectsV2Paginator(listingClient{s.client}, &s3.ListObjectsV2Input{Bucket: &b, Prefix: aws.String(backend.InternalPrefix + "versions/")})
	for indexes.HasMorePages() {
		page, err := indexes.NextPage(ctx)
		if err != nil {
			return translate(err)
		}
		for _, item := range page.Contents {
			key := aws.ToString(item.Key)
			if !strings.HasSuffix(key, "/index") {
				continue
			}
			raw, err := s.readRaw(ctx, b, key)
			if err != nil {
				return err
			}
			var state struct{ Key string }
			if err = json.Unmarshal(raw, &state); err != nil {
				return err
			}
			h := sha256.Sum256([]byte(state.Key))
			if key == backend.InternalPrefix+"versions/"+hex.EncodeToString(h[:])+"/index" {
				known[state.Key] = true
			}
		}
	}
	keys := make(map[string]bool)
	owned, foreign := make(map[string]bool), make(map[string]bool)
	versions := s3.NewListObjectVersionsPaginator(listingClient{s.client}, &s3.ListObjectVersionsInput{Bucket: &b})
	for versions.HasMorePages() {
		page, err := versions.NextPage(ctx)
		if err != nil {
			return translate(err)
		}
		for _, v := range page.Versions {
			key := aws.ToString(v.Key)
			keys[key] = true
			if !strings.HasPrefix(key, backend.InternalPrefix) && !known[key] {
				p, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &b, Key: v.Key, VersionId: v.VersionId})
				if err != nil {
					return translate(err)
				}
				if _, ok := envelope(p.Metadata); ok {
					owned[key] = true
				} else {
					foreign[key] = true
				}
			}
		}
		for _, v := range page.DeleteMarkers {
			keys[aws.ToString(v.Key)] = true
		}
	}
	for key := range keys {
		if !strings.HasPrefix(key, backend.InternalPrefix) && !known[key] && !(owned[key] && !foreign[key]) {
			return backend.ErrBucketNotEmpty
		}
	}
	for key := range keys {
		if err := s.purgeKey(ctx, b, key); err != nil {
			return err
		}
	}
	return nil
}
