package backend

import (
	"testing"
	"time"
)

func TestLifecycleUTCDeadline(t *testing.T) {
	for _, tc := range []struct {
		since, want string
		days        int
	}{
		{"2026-10-03T14:05:00+02:00", "2026-10-05T00:00:00Z", 1},
		{"2026-10-03T00:00:00Z", "2026-10-04T00:00:00Z", 1},
		{"2024-02-28T23:59:59Z", "2024-03-01T00:00:00Z", 1},
	} {
		since, _ := time.Parse(time.RFC3339, tc.since)
		if got := LifecycleDeadline(since, tc.days, 0).Format(time.RFC3339); got != tc.want {
			t.Fatalf("%s: got %s want %s", tc.since, got, tc.want)
		}
	}
}
