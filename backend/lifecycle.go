package backend

import (
	"strings"
	"time"
)

func (r LifecycleRule) Matches(o Object) bool {
	if r.Status != "Enabled" || strings.HasPrefix(o.Key, InternalPrefix) {
		return false
	}
	prefix, tags, gt, lt := r.Selector()
	if !strings.HasPrefix(o.Key, prefix) || gt != nil && o.Size <= *gt || lt != nil && o.Size >= *lt {
		return false
	}
	for _, tag := range tags {
		found := false
		for _, v := range o.Tags {
			if tag == v {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func (r LifecycleRule) Selector() (string, []Tag, *int64, *int64) {
	prefix := ""
	if r.Prefix != nil {
		prefix = *r.Prefix
	}
	var tags []Tag
	var gt, lt *int64
	if f := r.Filter; f != nil {
		if f.Prefix != nil {
			prefix = *f.Prefix
		}
		if f.Tag != nil {
			tags = append(tags, *f.Tag)
		}
		gt, lt = f.GreaterThan, f.LessThan
		if a := f.And; a != nil {
			if a.Prefix != nil {
				prefix = *a.Prefix
			}
			tags = a.Tags
			gt, lt = a.GreaterThan, a.LessThan
		}
	}
	return prefix, tags, gt, lt
}

// LifecycleDeadline rounds up to midnight UTC for production S3 days. A
// nonzero testDay is an explicitly selected accelerated fixture clock only.
func LifecycleDeadline(since time.Time, days int, testDay time.Duration) time.Time {
	if testDay > 0 {
		// Split seconds/nanoseconds so a valid long retention period cannot
		// overflow time.Duration and accidentally become immediately eligible.
		seconds := int64(days) * int64(testDay/time.Second)
		nanos := int64(days) * int64(testDay%time.Second)
		return time.Unix(since.Unix()+seconds, int64(since.Nanosecond())+nanos).In(since.Location())
	}
	target := since.UTC().AddDate(0, 0, days)
	midnight := time.Date(target.Year(), target.Month(), target.Day(), 0, 0, 0, 0, time.UTC)
	if target.After(midnight) {
		midnight = midnight.AddDate(0, 0, 1)
	}
	return midnight
}
func (r LifecycleRule) Expiry(o Object, testDay time.Duration) time.Time {
	if r.Expiration == nil || !r.Matches(o) {
		return time.Time{}
	}
	e := r.Expiration
	if e.Days != nil {
		return LifecycleDeadline(o.Modified, *e.Days, testDay)
	}
	if e.Date != "" {
		t, _ := time.Parse(time.RFC3339Nano, e.Date)
		return t
	}
	return time.Time{}
}
