package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
)

// RunLifecycleOnce executes a policy scan. Embedders that use New instead of
// Run can schedule it themselves; Run starts and stops the worker with HTTP.
func (g *Gateway) RunLifecycleOnce(ctx context.Context, now time.Time) error {
	if g.opts.ReadOnly {
		return nil
	}
	buckets, err := g.be.ListBuckets(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, b := range buckets {
		if err = ctx.Err(); err != nil {
			return err
		}
		p, e := g.be.GetBucketProperties(ctx, b.Name)
		if errors.Is(e, backend.ErrBucketNotFound) {
			continue
		}
		if e != nil {
			failures = append(failures, e)
			continue
		}
		if p.Lifecycle == nil {
			continue
		}
		if e = g.be.ExpireBucket(ctx, b.Name, now, g.opts.LifecycleTestDay); e != nil && !errors.Is(e, backend.ErrBucketNotFound) {
			failures = append(failures, fmt.Errorf("expire bucket %s: %w", b.Name, e))
		}
		if e = g.expireUploads(ctx, b.Name, now); e != nil && !errors.Is(e, backend.ErrBucketNotFound) {
			failures = append(failures, fmt.Errorf("expire uploads %s: %w", b.Name, e))
		}
	}
	return errors.Join(failures...)
}
func (g *Gateway) runLifecycle(ctx context.Context) {
	if g.opts.ReadOnly {
		return
	}
	ticker := time.NewTicker(g.opts.LifecycleInterval)
	defer ticker.Stop()
	for {
		if err := g.RunNotificationsOnce(ctx, time.Now()); err != nil && ctx.Err() == nil {
			slog.Error("SNS notification retry failed", "error", err)
		}
		if err := g.RunMaintenanceOnce(ctx, time.Now()); err != nil && ctx.Err() == nil {
			slog.Error("recovery and cleanup scan failed", "error", err)
		}
		if err := g.RunLifecycleOnce(ctx, time.Now()); err != nil && ctx.Err() == nil {
			slog.Error("lifecycle scan failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (g *Gateway) expireUploads(ctx context.Context, b string, now time.Time) error {
	p, err := g.be.GetBucketProperties(ctx, b)
	if err != nil {
		return err
	}
	enabled := false
	if p.Lifecycle != nil {
		for _, r := range p.Lifecycle.Rules {
			if r.Status == "Enabled" && r.AbortMultipart != nil {
				enabled = true
			}
		}
	}
	if !enabled {
		return nil
	}
	after := ""
	for {
		objects, next, err := g.be.List(ctx, b, backend.InternalPrefix, after, 1000)
		if err != nil {
			return err
		}
		for _, o := range objects {
			parts := strings.Split(strings.TrimPrefix(o.Key, backend.InternalPrefix), "/")
			if len(parts) != 2 || parts[1] != "manifest" || len(parts[0]) != 32 {
				continue
			}
			if err = g.expireUpload(ctx, b, parts[0], now); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		after = next
	}
}
func (g *Gateway) expireUpload(ctx context.Context, b, uid string, now time.Time) error {
	unlock := g.lockUpload(uid)
	defer unlock()
	_, body, err := g.be.Get(ctx, b, uploadBase(uid)+"manifest", backend.ReadOptions{Length: -1})
	if errors.Is(err, backend.ErrNotFound) {
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
		return nil
	}
	p, err := g.be.GetBucketProperties(ctx, b)
	if err != nil {
		return err
	}
	if p.Lifecycle == nil {
		return nil
	}
	for _, r := range p.Lifecycle.Rules {
		if r.AbortMultipart == nil || !r.Matches(backend.Object{Key: u.Key}) {
			continue
		}
		if !now.Before(backend.LifecycleDeadline(u.Initiated, r.AbortMultipart.Days, g.opts.LifecycleTestDay)) {
			return g.cleanupUpload(ctx, b, uid, true)
		}
	}
	return nil
}
func (g *Gateway) expirationHeader(w http.ResponseWriter, r *http.Request, b string, o backend.Object) {
	if o.DeleteMarker || r.URL.Query().Has("versionId") {
		return
	}
	p, err := g.be.GetBucketProperties(r.Context(), b)
	if err != nil || p.Lifecycle == nil {
		return
	}
	var expiry time.Time
	ruleID := ""
	for _, rule := range p.Lifecycle.Rules {
		// HTTP dates always describe production S3 days, even in accelerated fixtures.
		at := rule.Expiry(o, 0)
		if !at.IsZero() && (expiry.IsZero() || at.Before(expiry)) {
			expiry, ruleID = at, rule.ID
		}
	}
	if !expiry.IsZero() {
		w.Header().Set("X-Amz-Expiration", fmt.Sprintf("expiry-date=%q, rule-id=%q", expiry.UTC().Format(http.TimeFormat), awsEncode(ruleID, false)))
	}
}
