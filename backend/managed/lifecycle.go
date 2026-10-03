package managed

import (
	"context"
	"errors"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
)

// ExpireBucket rechecks the current policy and object under the same lock used
// by writes, tagging and version deletion. A scan never deletes a stale version
// merely because it was eligible before a concurrent mutation.
func (s *Store) ExpireBucket(ctx context.Context, b string, now time.Time, testDay time.Duration) error {
	versions, err := s.ListVersions(ctx, b, "")
	if err != nil {
		return err
	}
	previous := ""
	for _, v := range versions {
		if err = ctx.Err(); err != nil {
			return err
		}
		if v.Object.Key == previous {
			continue
		}
		previous = v.Object.Key
		if err = s.expireKey(ctx, b, v.Object.Key, now, testDay); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) expireKey(ctx context.Context, b, k string, now time.Time, testDay time.Duration) error {
	unlock := s.lockKey(b, k)
	defer unlock()
	p, err := s.settings(ctx, b)
	if err != nil {
		return err
	}
	if p.Lifecycle == nil {
		return nil
	}
	state, exists, err := s.readIndex(ctx, b, k)
	if err != nil {
		return err
	}
	if !exists {
		o, e := s.Backend.Head(ctx, b, k)
		if errors.Is(e, backend.ErrNotFound) {
			return nil
		}
		if e != nil {
			return e
		}
		due := false
		for _, rule := range p.Lifecycle.Rules {
			at := rule.Expiry(o, testDay)
			if !at.IsZero() && !now.Before(at) {
				due = true
				break
			}
		}
		if !due {
			return nil
		}
		if p.Versioning == "" {
			return s.Backend.Delete(ctx, b, k, backend.Conditions{IfMatch: o.ETag})
		}
		state, err = s.snapshot(ctx, b, k, state, false)
		if err != nil {
			return err
		}
	}
	if len(state.Versions) == 0 {
		return nil
	}
	next := index{Key: k}
	changed := false
	for i, v := range state.Versions {
		if i == 0 {
			next.Versions = append(next.Versions, v)
			continue
		}
		expired := false
		for _, rule := range p.Lifecycle.Rules {
			e := rule.NoncurrentExpiration
			if e == nil || !rule.Matches(v.Object) {
				continue
			}
			if e.NewerVersions != nil && i <= *e.NewerVersions {
				continue
			}
			if !now.Before(backend.LifecycleDeadline(v.NoncurrentSince, e.Days, testDay)) {
				expired = true
				break
			}
		}
		if expired {
			changed = true
		} else {
			next.Versions = append(next.Versions, v)
		}
	}
	current := next.Versions[0].Object
	if current.DeleteMarker {
		if len(next.Versions) == 1 {
			for _, rule := range p.Lifecycle.Rules {
				e := rule.Expiration
				if e == nil || !rule.Matches(current) {
					continue
				}
				// Days/Date expiration also removes orphaned delete markers. Tag/size
				// filtered rules cannot identify a deleted object's former payload.
				_, tags, gt, lt := rule.Selector()
				if len(tags) > 0 || gt != nil || lt != nil {
					continue
				}
				at := rule.Expiry(current, testDay)
				if e.DeleteMarker != nil && *e.DeleteMarker || !at.IsZero() && !now.Before(at) {
					next.Versions = nil
					changed = true
					break
				}
			}
		}
	} else {
		for _, rule := range p.Lifecycle.Rules {
			at := rule.Expiry(current, testDay)
			if at.IsZero() || now.Before(at) {
				continue
			}
			markerID := "null"
			if p.Versioning == "Enabled" {
				markerID = newID()
			}
			marker := Version{Object: backend.Object{Key: k, VersionID: markerID, DeleteMarker: true, Modified: now.UTC(), ACL: p.ACL}}
			retained := []Version{marker}
			for _, v := range next.Versions {
				if markerID != "null" || v.Object.VersionID != "null" {
					retained = append(retained, v)
				}
			}
			next.Versions = retained
			changed = true
			break
		}
	}
	if changed {
		return s.commit(ctx, b, state, next)
	}
	return nil
}
