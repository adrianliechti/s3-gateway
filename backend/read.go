package backend

import (
	"fmt"
	"strconv"
	"strings"
)

// ReadObjectSize recovers the full object size from a ranged GET response.
// Content-Length alone describes only the returned bytes, which must not be
// used to validate an object's persisted metadata envelope.
func ReadObjectSize(length int64, contentRange string) (int64, error) {
	if contentRange == "" {
		return length, nil
	}
	span, total, ok := strings.Cut(strings.TrimPrefix(contentRange, "bytes "), "/")
	start, end, valid := strings.Cut(span, "-")
	first, e1 := strconv.ParseInt(start, 10, 64)
	last, e2 := strconv.ParseInt(end, 10, 64)
	size, e3 := strconv.ParseInt(total, 10, 64)
	if !strings.HasPrefix(contentRange, "bytes ") || !ok || !valid || e1 != nil || e2 != nil || e3 != nil || first < 0 || last < first || size <= last || last-first+1 != length {
		return 0, fmt.Errorf("invalid provider content range")
	}
	return size, nil
}
