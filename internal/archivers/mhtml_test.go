package archivers

import "testing"

func TestParseMHTMLSnapshot(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		got, err := parseMHTMLSnapshot(map[string]interface{}{"data": "MIME-body"})
		if err != nil || got != "MIME-body" {
			t.Fatalf("got %q, err %v", got, err)
		}
	})
	t.Run("nil result", func(t *testing.T) {
		if _, err := parseMHTMLSnapshot(nil); err == nil {
			t.Fatal("expected error for nil result")
		}
	})
	t.Run("wrong outer type", func(t *testing.T) {
		if _, err := parseMHTMLSnapshot("not a map"); err == nil {
			t.Fatal("expected error for non-map result")
		}
	})
	t.Run("missing data field", func(t *testing.T) {
		if _, err := parseMHTMLSnapshot(map[string]interface{}{"other": 1}); err == nil {
			t.Fatal("expected error for missing data field")
		}
	})
	t.Run("data not a string", func(t *testing.T) {
		if _, err := parseMHTMLSnapshot(map[string]interface{}{"data": 123}); err == nil {
			t.Fatal("expected error for non-string data")
		}
	})
}
