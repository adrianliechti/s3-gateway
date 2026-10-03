package azure

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/adrianliechti/s3-gateway/backend"
)

const settingsKey = backend.InternalPrefix + "settings"

func (s *Store) GetBucketProperties(ctx context.Context, b string) (backend.BucketProperties, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out backend.BucketProperties
	res, err := s.bc(b, settingsKey).DownloadStream(ctx, nil)
	if errors.Is(translate(err), backend.ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return out, translate(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxDetailsSize+1))
	if err != nil {
		return out, err
	}
	if len(raw) > maxDetailsSize {
		return out, backend.ErrMetadataTooLarge
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}

func (s *Store) SetBucketProperties(ctx context.Context, b string, p backend.BucketProperties) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if len(raw) > maxDetailsSize {
		return backend.ErrMetadataTooLarge
	}
	_, err = s.bc(b, settingsKey).Upload(ctx, streaming.NopCloser(bytes.NewReader(raw)), &blockblob.UploadOptions{HTTPHeaders: &blob.HTTPHeaders{BlobContentType: ptr("application/json")}})
	return translate(err)
}
func (s *Store) SetObjectMetadata(ctx context.Context, b, k string, o backend.Object) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	current, err := s.bc(b, k).GetProperties(ctx, nil)
	if err != nil {
		return translate(err)
	}
	if o.Revision != "" && o.Revision != string(val(current.ETag)) {
		return backend.ErrPrecondition
	}
	digest := hex.EncodeToString(current.ContentMD5)
	m, err := s.encodeMetadata(ctx, b, o, digest, true)
	if err != nil {
		return err
	}
	_, err = s.bc(b, k).SetMetadata(ctx, m, &blob.SetMetadataOptions{AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: ptr(azcore.ETag(val(current.ETag)))}}})
	return translate(err)
}
func preserveTime(m map[string]*string, modified time.Time) error {
	raw, err := base64.StdEncoding.DecodeString(val(m["gateway"]))
	if err != nil {
		return err
	}
	var saved metadata
	if err = json.Unmarshal(raw, &saved); err != nil {
		return err
	}
	saved.Modified = modified
	raw, err = json.Marshal(saved)
	if err != nil {
		return err
	}
	m["gateway"] = ptr(base64.StdEncoding.EncodeToString(raw))
	return nil
}
