package azure

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/adrianliechti/s3-gateway/backend"
)

// CollectGarbage traces native metadata on every blob, including version data
// and multipart parts. The exclusive lock excludes local readers/publishers
// until collection ends. Multiple gateway writers remain unsupported.
func (s *Store) CollectGarbage(ctx context.Context, b string, cutoff time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	live := make(map[string]bool)
	mark := func(m map[string]*string) error {
		for name, value := range m {
			name = strings.ToLower(name)
			if name != "gateway" {
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(val(value))
			if err != nil {
				return err
			}
			var ref struct {
				Object string `json:"ref"`
			}
			if err = json.Unmarshal(raw, &ref); err != nil {
				return err
			}
			live[ref.Object] = true
		}
		return nil
	}
	type candidate struct {
		key, version string
		revision     azcore.ETag
		current      bool
		modified     time.Time
		metadata     map[string]*string
	}
	var items []candidate
	references := map[string]bool{}
	pager := s.cc(b).NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Include: container.ListBlobsInclude{Metadata: true, Versions: true}})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return translate(err)
		}
		for _, item := range page.Segment.BlobItems {
			current := val(item.VersionID) == "" || val(item.IsCurrentVersion)
			key := val(item.Name)
			items = append(items, candidate{key, val(item.VersionID), val(item.Properties.ETag), current, val(item.Properties.LastModified), item.Metadata})
			if current && strings.HasPrefix(key, backend.InternalPrefix+"versions/") && (strings.HasSuffix(key, "/index") || strings.HasSuffix(key, "/pending")) {
				res, err := s.bc(b, key).DownloadStream(ctx, nil)
				if err != nil {
					return translate(err)
				}
				raw, err := io.ReadAll(io.LimitReader(res.Body, maxDetailsSize+1))
				res.Body.Close()
				if err != nil {
					return err
				}
				if len(raw) > maxDetailsSize {
					return backend.ErrMetadataTooLarge
				}
				_, refs, err := backend.HistoryReferences(raw)
				if err != nil {
					return err
				}
				for _, ref := range refs {
					references[ref] = true
				}
			}
		}
	}
	var stale []candidate
	var helpers []candidate
	for _, item := range items {
		if strings.HasPrefix(item.key, detailsPrefix) {
			helpers = append(helpers, item)
			continue
		}
		// Settings are raw JSON without an envelope. Only superseded gateway
		// generations may be collected; native blobs retain their own history.
		owned := item.key == settingsKey
		for name, value := range item.metadata {
			if strings.EqualFold(name, "gateway") {
				raw, err := base64.StdEncoding.DecodeString(val(value))
				if err != nil {
					return err
				}
				var saved metadata
				if err = json.Unmarshal(raw, &saved); err != nil {
					return err
				}
				owned = saved.Version == 1 || saved.Version == 2
			}
		}
		if !item.current && owned && item.modified.Before(cutoff) && !references[backend.VersionReference(item.key, item.version)] {
			stale = append(stale, item)
			continue
		}
		if err := mark(item.metadata); err != nil {
			return err
		}
	}
	// Finish the complete reference scan before deleting anything.
	remove := func(item candidate) error {
		client := s.bc(b, item.key)
		if item.version != "" {
			var err error
			client, err = client.WithVersionID(item.version)
			if err != nil {
				return err
			}
		}
		_, err := client.Delete(ctx, &blob.DeleteOptions{AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: &item.revision}}})
		if errors.Is(translate(err), backend.ErrNotFound) {
			return nil
		}
		return translate(err)
	}
	for _, item := range stale {
		if err := remove(item); err != nil {
			return err
		}
	}
	// Remove current helper names first, then their historical native versions.
	for _, item := range helpers {
		if item.current && !live[item.key] && item.modified.Before(cutoff) {
			v := item
			v.version = ""
			if err := remove(v); err != nil {
				return err
			}
		}
	}
	for _, item := range helpers {
		if item.version != "" && !live[item.key] && item.modified.Before(cutoff) {
			if err := remove(item); err != nil {
				return err
			}
		}
	}
	return nil
}
