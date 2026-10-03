package gateway

import (
	"time"

	"github.com/adrianliechti/s3-gateway/backend"
)

func validateLifecycle(c *backend.LifecycleConfiguration) error {
	invalid := func(message string) error { return apiError("InvalidArgument", 400, message) }
	malformed := func() error { return apiError("MalformedXML", 400, "Invalid lifecycle configuration") }
	if len(c.Rules) == 0 || len(c.Rules) > 1000 || len(c.Unknown) > 0 {
		return malformed()
	}
	ids := map[string]bool{}
	for i := range c.Rules {
		r := &c.Rules[i]
		if len(r.ID) > 255 || r.ID != "" && ids[r.ID] {
			return invalid("Lifecycle rule IDs must be unique and at most 255 bytes")
		}
		if r.ID == "" {
			r.ID = id()
		}
		ids[r.ID] = true
		if r.Status != "Enabled" && r.Status != "Disabled" || len(r.Unknown) > 0 {
			return malformed()
		}
		if r.Prefix != nil && r.Filter != nil {
			return invalid("Prefix and Filter cannot be combined")
		}
		if r.Prefix == nil && r.Filter == nil {
			return malformed()
		}
		if f := r.Filter; f != nil {
			n := 0
			for _, yes := range []bool{f.Prefix != nil, f.Tag != nil, f.And != nil, f.GreaterThan != nil, f.LessThan != nil} {
				if yes {
					n++
				}
			}
			if n > 1 || len(f.Unknown) > 0 {
				return malformed()
			}
			if a := f.And; a != nil {
				n = len(a.Tags)
				for _, yes := range []bool{a.Prefix != nil, a.GreaterThan != nil, a.LessThan != nil} {
					if yes {
						n++
					}
				}
				if n < 2 || len(a.Unknown) > 0 {
					return malformed()
				}
			}
		}
		prefix, tags, gt, lt := r.Selector()
		if len(prefix) > 1024 {
			return invalid("Lifecycle prefix is too long")
		}
		if err := validateTags(tags); err != nil {
			return err
		}
		if gt != nil && *gt < 0 || lt != nil && *lt < 0 || gt != nil && lt != nil && *gt >= *lt {
			return invalid("Invalid object size filter")
		}
		if e := r.Expiration; e != nil {
			n := 0
			for _, yes := range []bool{e.Days != nil, e.Date != "", e.DeleteMarker != nil} {
				if yes {
					n++
				}
			}
			if n != 1 || len(e.Unknown) > 0 {
				return malformed()
			}
			if e.Days != nil && (*e.Days < 1 || *e.Days > 365000) {
				return invalid("Expiration days must be positive and within 1000 years")
			}
			if e.Date != "" && !lifecycleDate(e.Date) {
				return invalid("Expiration date must be midnight UTC in ISO 8601 format")
			}
			if e.DeleteMarker != nil && len(tags) > 0 {
				return invalid("Delete-marker expiration cannot use tag filters")
			}
		}
		if e := r.NoncurrentExpiration; e != nil {
			if e.Days < 1 || e.Days > 365000 || len(e.Unknown) > 0 {
				return invalid("Invalid noncurrent expiration")
			}
			if e.NewerVersions != nil && (*e.NewerVersions < 1 || *e.NewerVersions > 100 || r.Filter == nil) {
				return invalid("Retaining newer noncurrent versions requires a Filter and a count from 1 to 100")
			}
		}
		if a := r.AbortMultipart; a != nil {
			if a.Days < 1 || a.Days > 365000 || len(a.Unknown) > 0 {
				return invalid("Invalid multipart expiration")
			}
			if len(tags) > 0 || gt != nil || lt != nil {
				return invalid("Multipart expiration only supports prefix filters")
			}
		}
		for _, t := range r.Transitions {
			if t.Date != "" && !lifecycleDate(t.Date) {
				return invalid("Invalid transition date")
			}
		}
		if len(r.Transitions) > 0 || len(r.NoncurrentTransitions) > 0 {
			return apiError("NotImplemented", 501, "Lifecycle storage-class transitions are not implemented")
		}
		if r.Expiration == nil && r.NoncurrentExpiration == nil && r.AbortMultipart == nil {
			return malformed()
		}
	}
	return nil
}
func lifecycleDate(s string) bool {
	t, err := time.Parse(time.RFC3339Nano, s)
	_, offset := t.Zone()
	return err == nil && offset == 0 && t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0
}
