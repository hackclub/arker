package archivers

import (
	"encoding/json"
	"testing"
	"time"
)

func TestBuildArchivedProbeVideoArtifacts(t *testing.T) {
	logs := "earlier output\nVideo info:\nWARNING: [youtube] a harmless extractor warning\nCaptured title\n47.555\nCaptured channel\n\nStarting yt-dlp download process...\n"
	metadataSidecar, rawSidecar, err := BuildArchivedProbeVideoArtifacts(logs, "https://youtube.com/shorts/example", VideoMedia{Extension: ".mp4", SizeBytes: 123}, time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if rawSidecar == nil || !json.Valid(rawSidecar.Data) {
		t.Fatalf("raw sidecar = %#v", rawSidecar)
	}
	var metadata VideoMetadata
	if err := json.Unmarshal(metadataSidecar.Data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Title != "Captured title" || metadata.Channel != "Captured channel" || metadata.DurationSeconds == nil || *metadata.DurationSeconds != 47.555 {
		t.Fatalf("metadata = %+v", metadata)
	}
	if metadata.Provider != "captured_probe_log" || metadata.PublicationTimestamp != "" {
		t.Errorf("provider/date = %q/%q", metadata.Provider, metadata.PublicationTimestamp)
	}
}

func TestBuildArchivedProbeVideoArtifactsRejectsDiagnostics(t *testing.T) {
	logs := "Video info:\n\nStarting yt-dlp download process...\ninnocent later label\n30\nnot probe metadata\n"
	if _, _, err := BuildArchivedProbeVideoArtifacts(logs, "https://example.com", VideoMedia{}, time.Now()); err == nil {
		t.Fatal("expected sparse diagnostic log to be rejected")
	}
}
