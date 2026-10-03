package managed

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
)

// MaintainBucket replays accepted publications, then removes old immutable
// data that no index references. The bucket lock covers staging, publication
// and this scan, so an in-flight writer cannot lose its unpublished data.
func (s *Store) MaintainBucket(ctx context.Context, b string, cutoff time.Time) error {
	u := s.lock(b)
	defer u()
	if err := s.recoverAll(ctx, b); err != nil {
		return err
	}
	live := make(map[string]bool)
	var candidates []backend.Object
	after := ""
	for {
		objects, next, err := s.Backend.List(ctx, b, historyPrefix, after, 1000)
		if err != nil {
			return err
		}
		for _, o := range objects {
			if strings.HasSuffix(o.Key, "/index") {
				var state index
				if err := s.readJSON(ctx, b, o.Key, &state); err != nil {
					return err
				}
				if o.Key != base(state.Key)+"index" {
					return fmt.Errorf("version index key mismatch")
				}
				for _, v := range state.Versions {
					live[v.Data] = true
				}
			} else if strings.Contains(o.Key, "/data/") && o.Modified.Before(cutoff) {
				candidates = append(candidates, o)
			}
		}
		if next == "" {
			break
		}
		after = next
	}
	for _, o := range candidates {
		if !live[o.Key] {
			if err := s.Backend.Delete(ctx, b, o.Key, backend.Conditions{}); err != nil {
				return err
			}
		}
	}
	after = ""
	for {
		objects, next, err := s.Backend.List(ctx, b, backend.InternalPrefix+"incoming/", after, 1000)
		if err != nil {
			return err
		}
		for _, o := range objects {
			if o.Modified.Before(cutoff) {
				if err = s.Backend.Delete(ctx, b, o.Key, backend.Conditions{}); err != nil {
					return err
				}
			}
		}
		if next == "" {
			break
		}
		after = next
	}
	if collector, ok := s.Backend.(interface {
		CollectGarbage(context.Context, string, time.Time) error
	}); ok {
		return collector.CollectGarbage(ctx, b, cutoff)
	}
	return nil
}
