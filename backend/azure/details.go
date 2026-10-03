package azure

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

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/adrianliechti/s3-gateway/backend"
)

const detailsPrefix = backend.InternalPrefix + "metadata/"
const maxDetailsSize = 16 << 20

// Keep compatible user metadata native and gateway state in an immutable,
// content-addressed helper blob, published before its pointer.
// The backend maintenance lock protects helper creation, pointer publication,
// metadata reads and reachability-based garbage collection.
func (s *Store) encodeMetadata(ctx context.Context, b string, o backend.Object, digest string, preserveModified bool) (map[string]*string, error) {
	m, err := encode(o, digest)
	if err != nil && !errors.Is(err, backend.ErrMetadataTooLarge) {
		return nil, err
	}
	if preserveModified {
		if err = preserveTime(m, o.Modified); err != nil {
			return nil, err
		}
	}
	raw, err := base64.StdEncoding.DecodeString(val(m["gateway"]))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxDetailsSize {
		return nil, backend.ErrMetadataTooLarge
	}
	sum := sha256.Sum256(raw)
	key := detailsPrefix + hex.EncodeToString(sum[:])
	_, err = s.bc(b, key).Upload(ctx, streaming.NopCloser(bytes.NewReader(raw)), &blockblob.UploadOptions{
		AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: ptr(azcore.ETag("*"))}},
		HTTPHeaders:      &blob.HTTPHeaders{BlobContentType: ptr("application/json")},
	})
	// Identical immutable metadata may already exist from a previous attempt.
	if err != nil && !helperAlreadyExists(err) {
		return nil, translate(err)
	}
	ref := struct {
		Version   int    `json:"v"`
		Reference string `json:"ref"`
	}{Version: 1, Reference: key}
	data, err := json.Marshal(ref)
	if err != nil {
		return nil, err
	}
	m["gateway"] = ptr(base64.StdEncoding.EncodeToString(data))
	if metadataSize(m) > 8000 {
		return nil, backend.ErrMetadataTooLarge
	}
	return m, nil
}

func helperAlreadyExists(err error) bool {
	return errors.Is(translate(err), backend.ErrPrecondition) || bloberror.HasCode(err, bloberror.BlobAlreadyExists)
}
func metadataSize(m map[string]*string) int {
	n := 0
	for k, v := range m {
		n += len(k) + len(val(v)) + len("x-ms-meta-: \r\n")
	}
	return n
}
func (s *Store) decodeMetadata(ctx context.Context, b string, o backend.Object, m map[string]*string) (backend.Object, error) {
	var encoded string
	for k, v := range m {
		if strings.EqualFold(k, "gateway") {
			encoded = val(v)
		}
	}
	raw, _ := base64.StdEncoding.DecodeString(encoded)
	var saved metadata
	if json.Unmarshal(raw, &saved) != nil || (saved.Version != 1 && saved.Version != 2) || saved.Reference == "" {
		return decode(o, m)
	}
	hash := strings.TrimPrefix(saved.Reference, detailsPrefix)
	if len(hash) != 64 || saved.Reference != detailsPrefix+hash {
		return o, fmt.Errorf("invalid Azure metadata reference")
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return o, fmt.Errorf("invalid Azure metadata reference")
	}
	res, err := s.bc(b, saved.Reference).DownloadStream(ctx, nil)
	if err != nil {
		return o, fmt.Errorf("read Azure metadata manifest: %w", err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxDetailsSize+1))
	if err != nil {
		return o, err
	}
	sum := sha256.Sum256(data)
	if len(data) > maxDetailsSize || hex.EncodeToString(sum[:]) != hash {
		return o, fmt.Errorf("Azure metadata manifest integrity failure")
	}
	expanded := make(map[string]*string, len(m))
	for k, v := range m {
		if !strings.EqualFold(k, "gateway") {
			expanded[k] = v
		}
	}
	expanded["gateway"] = ptr(base64.StdEncoding.EncodeToString(data))
	return decode(o, expanded)
}
