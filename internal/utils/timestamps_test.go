package utils

import (
	"encoding/json"
	"testing"
)

func TestNormalizeTimestamp(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"rfc3339 passes through", "2026-04-16T12:57:30Z", "2026-04-16T12:57:30Z"},
		{"offset becomes utc", "2026-08-28T01:00:42+02:00", "2026-08-27T23:00:42Z"},
		{"milliseconds are dropped", "2024-04-18T02:28:21.000Z", "2024-04-18T02:28:21Z"},
		{"bare datetime is utc", "2026-08-28 01:00:42", "2026-08-28T01:00:42Z"},
		{"ruby date is x's createdAt", "Sat Jun 06 07:21:57 +0000 2026", "2026-06-06T07:21:57Z"},
		{"rfc1123z is pinterest's", "Wed, 26 Aug 2026 02:43:18 +0000", "2026-08-26T02:43:18Z"},
		// Instagram's taken_at, the shape that reached consumers as an integer.
		{"epoch seconds as a number", float64(1776344250), "2026-04-16T12:57:30Z"},
		{"epoch seconds as an int", 1776344250, "2026-04-16T12:57:30Z"},
		{"epoch seconds as a string", "1776344250", "2026-04-16T12:57:30Z"},
		{"epoch milliseconds", float64(1776344250000), "2026-04-16T12:57:30Z"},
		{"epoch microseconds", float64(1776344250000000), "2026-04-16T12:57:30Z"},
		{"json number", json.Number("1776344250"), "2026-04-16T12:57:30Z"},
		// An unparsed date is still worth more than none.
		{"unknown layout is kept", "sometime last April", "sometime last April"},
		{"blank is dropped", "   ", ""},
		{"zero epoch is dropped", float64(0), ""},
		{"negative epoch is dropped", float64(-1), ""},
		{"nil is dropped", nil, ""},
		{"object is dropped", map[string]any{"text": "x"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeTimestamp(tc.value); got != tc.want {
				t.Errorf("NormalizeTimestamp(%#v) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}
