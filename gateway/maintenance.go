package gateway

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
)

// RunMaintenanceOnce recovers accepted writes and cleans unreachable helper
// data. The 24-hour grace period is independent of the lifecycle test clock.
// Active uploads and completed receipts have no automatic retention limit.
func (g *Gateway) RunMaintenanceOnce(ctx context.Context, now time.Time) error {
	if g.opts.ReadOnly {
		return nil
	}
	buckets, err := g.be.ListBuckets(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, b := range buckets {
		cutoff := now.Add(-24 * time.Hour)
		if err = g.be.MaintainBucket(ctx, b.Name, cutoff); err == nil {
			err = g.cleanUploads(ctx, b.Name, cutoff)
		}
		if err != nil && !errors.Is(err, backend.ErrBucketNotFound) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (g *Gateway) cleanUploads(ctx context.Context, b string, cutoff time.Time) error {
	after, previousID := "", ""
	for {
		objects, next, err := g.be.List(ctx, b, backend.InternalPrefix, after, 1000)
		if err != nil {
			return err
		}
		for _, o := range objects {
			parts := strings.Split(strings.TrimPrefix(o.Key, backend.InternalPrefix), "/")
			if len(parts) < 2 || len(parts[0]) != 32 || parts[0] == previousID {
				continue
			}
			if _, e := hex.DecodeString(parts[0]); e != nil {
				continue
			}
			previousID = parts[0]
			if err = g.cleanUpload(ctx, b, parts[0], o.Modified.Before(cutoff)); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		after = next
	}
}

func (g *Gateway) cleanUpload(ctx context.Context, b, uid string, old bool) error {
	unlock := g.lockUpload(uid)
	defer unlock()
	_, body, err := g.be.Get(ctx, b, uploadBase(uid)+"manifest", backend.ReadOptions{Length: -1})
	if errors.Is(err, backend.ErrNotFound) {
		if old {
			return g.cleanupUpload(ctx, b, uid, false)
		}
		return nil
	}
	if err != nil {
		return err
	}
	var u upload
	err = json.NewDecoder(io.LimitReader(body, maxUploadState)).Decode(&u)
	body.Close()
	if err != nil {
		return err
	}
	u, err = g.loadUpload(ctx, b, u.Key, uid)
	if err != nil {
		return err
	}
	if u.Completed != nil {
		return g.cleanupUpload(ctx, b, uid, false)
	}
	return nil
}
