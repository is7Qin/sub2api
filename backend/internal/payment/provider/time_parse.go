package provider

import (
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func parseProviderTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05-0700", "2006-01-02T15:04:05.000-0700"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), true
		}
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", raw, timezone.Location()); err == nil {
		return t.UTC(), true
	}
	if ts, err := strconv.ParseInt(raw, 10, 64); err == nil && ts > 0 {
		if ts > 1_000_000_000_000 {
			return time.UnixMilli(ts).UTC(), true
		}
		return time.Unix(ts, 0).UTC(), true
	}
	return time.Time{}, false
}
