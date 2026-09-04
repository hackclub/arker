package utils

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// NormalizeTimestamp turns the timestamp shapes sources publish — RFC 3339 and
// the other layouts platforms favour, epoch seconds/milliseconds/microseconds,
// written as strings or as numbers — into one RFC 3339 UTC string.
//
// It exists because "when was this posted" reaches Arker in a different shape
// from every extractor and every paid actor, while consumers of a normalized
// field are entitled to exactly one type and one format. A string in a layout
// this does not recognize is returned unchanged, since an unparsed date still
// carries more information than none; anything else yields "".
func NormalizeTimestamp(value any) string {
	switch typed := value.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return ""
		}
		// RubyDate is X's createdAt ("Sat Jun 06 07:21:57 +0000 2026");
		// RFC1123Z is Pinterest's ("Wed, 26 Aug 2026 02:43:18 +0000").
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05", time.RubyDate, time.RFC1123Z, time.RFC1123} {
			if t, err := time.Parse(layout, trimmed); err == nil {
				return t.UTC().Format(time.RFC3339)
			}
		}
		if epoch, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			return NormalizeTimestamp(float64(epoch))
		}
		return trimmed
	case float64:
		if typed <= 0 {
			return ""
		}
		seconds := int64(typed)
		// Epoch values arrive in seconds, milliseconds (Bright Data, TikTok)
		// and occasionally microseconds (Instagram's device_timestamp).
		// 1e12 seconds is the year 33658, so anything past it is a finer unit
		// rather than a real date.
		for seconds > 1e12 {
			seconds /= 1000
		}
		return time.Unix(seconds, 0).UTC().Format(time.RFC3339)
	case int:
		return NormalizeTimestamp(float64(typed))
	case int64:
		return NormalizeTimestamp(float64(typed))
	case json.Number:
		if v, err := typed.Float64(); err == nil {
			return NormalizeTimestamp(v)
		}
	}
	return ""
}
