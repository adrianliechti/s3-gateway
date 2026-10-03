package aws

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const detailsPrefix = backend.InternalPrefix + "metadata/"
const settingsKey = backend.InternalPrefix + "settings"
const maxDetailsSize = 16 << 20

// Bytes, HTTP properties and compatible user metadata stay native. The reserved
// gateway field only references a manifest, published before the object.
type metadata struct {
	Version   int             `json:"v"`
	Reference string          `json:"ref,omitempty"`
	Object    *backend.Object `json:"object,omitempty"`
}

func metadataSize(m map[string]string) int {
	n := 0
	for k, v := range m {
		n += len(k) + len(v)
	}
	return n
}

func nativeMetadata(k, v string) bool {
	if k == "" || strings.EqualFold(k, "gateway") {
		return false
	}
	for _, c := range k {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
			return false
		}
	}
	for _, c := range v {
		if c < 32 || c > 126 {
			return false
		}
	}
	return true
}

func (s *Store) encodeMetadata(ctx context.Context, b string, o backend.Object) (map[string]string, error) {
	o.Key, o.Revision = "", ""
	raw, err := json.Marshal(metadata{Version: 1, Object: &o})
	if err != nil {
		return nil, err
	}
	if len(raw) > maxDetailsSize {
		return nil, backend.ErrMetadataTooLarge
	}
	m := make(map[string]string)
	for k, v := range o.Metadata {
		if nativeMetadata(k, v) {
			m[strings.ToLower(k)] = v
		}
	}
	sum := sha256.Sum256(raw)
	key := detailsPrefix + hex.EncodeToString(sum[:])
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &b, Key: aws.String(key), Body: bytes.NewReader(raw), ContentType: aws.String("application/json"), IfNoneMatch: aws.String("*")})
	if err != nil && !errors.Is(translate(err), backend.ErrPrecondition) {
		return nil, translate(err)
	}
	ref, _ := json.Marshal(metadata{Version: 1, Reference: key})
	m["gateway"] = base64.StdEncoding.EncodeToString(ref)
	if metadataSize(m) > 2048 {
		// The manifest already holds all user metadata; retain it there when
		// native metadata plus the reserved pointer would exceed S3's limit.
		m = map[string]string{"gateway": m["gateway"]}
	}
	return m, nil
}

func envelope(m map[string]string) (metadata, bool) {
	var out metadata
	raw, err := base64.StdEncoding.DecodeString(m["gateway"])
	ok := err == nil && json.Unmarshal(raw, &out) == nil && (out.Version == 1 || out.Version == 2)
	return out, ok
}

func (s *Store) readRaw(ctx context.Context, b, k string) ([]byte, error) {
	p, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &b, Key: aws.String(k)})
	if err != nil {
		return nil, s.objectError(ctx, b, err)
	}
	defer p.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(p.Body, maxDetailsSize+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxDetailsSize {
		return nil, backend.ErrMetadataTooLarge
	}
	return raw, nil
}

func (s *Store) decodeMetadata(ctx context.Context, b string, o backend.Object) (backend.Object, error) {
	saved, ok := envelope(o.Metadata)
	if !ok {
		return o, nil
	}
	if saved.Reference != "" {
		hash := strings.TrimPrefix(saved.Reference, detailsPrefix)
		if len(hash) != 64 || saved.Reference != detailsPrefix+hash {
			return o, fmt.Errorf("invalid S3 metadata reference")
		}
		if _, err := hex.DecodeString(hash); err != nil {
			return o, fmt.Errorf("invalid S3 metadata reference")
		}
		raw, err := s.readRaw(ctx, b, saved.Reference)
		if errors.Is(err, backend.ErrNotFound) {
			return o, fmt.Errorf("S3 metadata manifest is missing")
		}
		if err != nil {
			return o, fmt.Errorf("read S3 metadata manifest: %w", err)
		}
		sum := sha256.Sum256(raw)
		if saved.Version == 1 && hex.EncodeToString(sum[:]) != hash {
			return o, fmt.Errorf("S3 metadata manifest integrity failure")
		}
		if err = json.Unmarshal(raw, &saved); err != nil {
			return o, err
		}
	}
	if saved.Version != 1 || saved.Object == nil {
		return o, fmt.Errorf("invalid S3 metadata manifest")
	}
	delete(o.Metadata, "gateway")
	if saved.Object.Size != o.Size {
		return o, nil
	}
	// Upstream native writers must discard reserved gateway metadata when
	// replacing content, as they do for any other content-specific metadata.
	v := saved.Object
	o.ETag, o.Modified, o.Expires = v.ETag, v.Modified, v.Expires
	// Some S3 providers drop the native redirect property during multipart
	// completion. Preserve the value recorded before publishing the object.
	o.WebsiteRedirectLocation = v.WebsiteRedirectLocation
	o.Metadata, o.Checksums, o.ChecksumType = v.Metadata, v.Checksums, v.ChecksumType
	o.VersionID, o.ACL, o.Tags, o.Parts = v.VersionID, v.ACL, v.Tags, v.Parts
	return o, nil
}

func (s *Store) GetBucketProperties(ctx context.Context, b string) (backend.BucketProperties, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var p backend.BucketProperties
	raw, err := s.readRaw(ctx, b, settingsKey)
	if errors.Is(err, backend.ErrNotFound) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(raw, &p)
	return p, err
}

func (s *Store) SetBucketProperties(ctx context.Context, b string, p backend.BucketProperties) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p.Versioning != "" {
		// Logical suspension is maintained by the shared index. Native history
		// stays enabled so retained null versions remain immutable as well.
		_, err := s.client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: &b, VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}})
		if err != nil {
			return translate(err)
		}
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if len(raw) > maxDetailsSize {
		return backend.ErrMetadataTooLarge
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &b, Key: aws.String(settingsKey), Body: bytes.NewReader(raw), ContentType: aws.String("application/json")})
	return translate(err)
}

func (s *Store) SetObjectMetadata(ctx context.Context, b, k string, o backend.Object) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	current, body, err := s.get(ctx, b, k, backend.ReadOptions{Length: -1, Revision: o.Revision})
	if err != nil {
		return err
	}
	defer body.Close()
	// Conditional republishing works on providers without destination-conditional
	// CopyObject support, and also supports objects larger than CopyObject's limit.
	_, err = s.put(ctx, b, k, body, current.Size, o, &current.Revision, nil, true)
	return err
}

// CollectGarbage retains manifests referenced by current objects, multipart
// helpers and immutable version data. One gateway writer per bucket is required.
func (s *Store) CollectGarbage(ctx context.Context, b string, cutoff time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	live, references, owned, remaining := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	var candidates []typesCandidate
	pager := s3.NewListObjectsV2Paginator(listingClient{s.client}, &s3.ListObjectsV2Input{Bucket: &b})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return translate(err)
		}
		for _, item := range page.Contents {
			key := aws.ToString(item.Key)
			if strings.HasPrefix(key, detailsPrefix) {
				if aws.ToTime(item.LastModified).Before(cutoff) {
					candidates = append(candidates, typesCandidate{key, aws.ToString(item.ETag)})
				}
				continue
			}
			if strings.HasPrefix(key, backend.InternalPrefix+"versions/") && (strings.HasSuffix(key, "/index") || strings.HasSuffix(key, "/pending")) {
				raw, err := s.readRaw(ctx, b, key)
				if err != nil {
					return err
				}
				name, refs, err := backend.HistoryReferences(raw)
				if err != nil {
					return err
				}
				owned[name] = true
				for _, ref := range refs {
					references[ref] = true
				}
			}
			p, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &b, Key: aws.String(key)})
			if err != nil {
				return translate(err)
			}
			if saved, ok := envelope(p.Metadata); ok && saved.Reference != "" {
				live[saved.Reference] = true
			}
		}
	}
	// Trace retained native history and collect superseded gateway generations.
	// Native objects without gateway metadata are never collected here.
	var stale []types.ObjectVersion
	var markers []types.DeleteMarkerEntry
	versions := s3.NewListObjectVersionsPaginator(listingClient{s.client}, &s3.ListObjectVersionsInput{Bucket: &b})
	for versions.HasMorePages() {
		page, err := versions.NextPage(ctx)
		if err != nil {
			return translate(err)
		}
		markers = append(markers, page.DeleteMarkers...)
		for _, item := range page.Versions {
			key := aws.ToString(item.Key)
			if strings.HasPrefix(key, detailsPrefix) {
				continue
			}
			p, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &b, Key: item.Key, VersionId: item.VersionId})
			if errors.Is(translate(err), backend.ErrNotFound) && !references[backend.VersionReference(key, aws.ToString(item.VersionId))] {
				continue
			}
			if err != nil {
				return fmt.Errorf("inspect native version of %s: %w", key, translate(err))
			}
			saved, gatewayOwned := envelope(p.Metadata)
			// Bucket settings are raw JSON without an object envelope. The
			// reserved namespace also owns those superseded native generations.
			gatewayOwned = gatewayOwned || strings.HasPrefix(key, backend.InternalPrefix)
			if gatewayOwned && !strings.HasPrefix(key, backend.InternalPrefix) {
				owned[key] = true
			}
			if !aws.ToBool(item.IsLatest) && gatewayOwned && !references[backend.VersionReference(key, aws.ToString(item.VersionId))] && aws.ToTime(item.LastModified).Before(cutoff) {
				stale = append(stale, item)
				continue
			}
			remaining[key] = true
			if gatewayOwned && saved.Reference != "" {
				live[saved.Reference] = true
			}
		}
	}
	// No mutation until the complete reference scan succeeds.
	for _, item := range stale {
		if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &b, Key: item.Key, VersionId: item.VersionId}); err != nil {
			return translate(err)
		}
	}
	for _, item := range markers {
		key := aws.ToString(item.Key)
		if owned[key] && !remaining[key] && aws.ToTime(item.LastModified).Before(cutoff) {
			if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &b, Key: item.Key, VersionId: item.VersionId}); err != nil {
				return translate(err)
			}
		}
	}
	for _, item := range candidates {
		if live[item.key] {
			continue
		}
		if err := s.purgeKey(ctx, b, item.key); err != nil {
			return err
		}
	}
	return nil
}

type typesCandidate struct{ key, revision string }
