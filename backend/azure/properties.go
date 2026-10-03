package azure

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/adrianliechti/s3-gateway/backend"
)

func (s *Store) GetBucketProperties(ctx context.Context, b string) (backend.BucketProperties, error) {
	var out backend.BucketProperties
	p, err := s.cc(b).GetProperties(ctx, nil)
	if err != nil {
		return out, translate(err)
	}
	encoded := ""
	for key, value := range p.Metadata {
		if strings.EqualFold(key, "s3gw_settings") {
			encoded = val(value)
		}
	}
	if encoded == "" {
		return out, nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}
func (s *Store) SetBucketProperties(ctx context.Context, b string, p backend.BucketProperties) error {
	current, err := s.cc(b).GetProperties(ctx, nil)
	if err != nil {
		return translate(err)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	metadata := map[string]*string{}
	for key, value := range current.Metadata {
		metadata[strings.ToLower(key)] = value
	}
	metadata["s3gw_settings"] = ptr(base64.StdEncoding.EncodeToString(raw))
	_, err = s.cc(b).SetMetadata(ctx, &container.SetMetadataOptions{Metadata: metadata, ModifiedAccessConditions: &container.ModifiedAccessConditions{IfMatch: current.ETag}})
	return translate(err)
}
func (s *Store) SetObjectMetadata(ctx context.Context, b, k string, o backend.Object) error {
	current, err := s.bc(b, k).GetProperties(ctx, nil)
	if err != nil {
		return translate(err)
	}
	if o.Revision != "" && o.Revision != string(val(current.ETag)) {
		return backend.ErrPrecondition
	}
	digest := hex.EncodeToString(current.ContentMD5)
	if digest == "" {
		// Without a native content digest, SetMetadata changes the only content
		// identity (the Azure ETag). Republish unchanged bytes + digest + ACL in
		// one block commit so future metadata can be bound to stable content.
		_, body, e := s.Get(ctx, b, k, backend.ReadOptions{Length: -1, Revision: o.Revision})
		if e != nil {
			return e
		}
		defer body.Close()
		_, e = s.put(ctx, b, k, body, o.Size, backend.PutOptions{Object: o, Conditions: backend.Conditions{IfMatch: o.ETag}}, true)
		return e
	}
	m, err := s.encodeMetadata(ctx, b, o, digest, true)
	if err != nil {
		return err
	}
	_, err = s.bc(b, k).SetMetadata(ctx, m, &blob.SetMetadataOptions{AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: ptr(azcore.ETag(val(current.ETag)))}}})
	return translate(err)
}
func preserveTime(m map[string]*string, modified time.Time) error {
	raw, err := base64.StdEncoding.DecodeString(val(m["s3gw"]))
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
	m["s3gw"] = ptr(base64.StdEncoding.EncodeToString(raw))
	return nil
}
