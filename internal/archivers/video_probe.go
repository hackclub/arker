package archivers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// VideoProbe contains intrinsic facts read from the archived media itself.
// Valid probe values are authoritative because they describe the exact bytes
// Arker stored; provider values remain fallbacks when a fact cannot be probed.
type VideoProbe struct {
	DurationSeconds *float64
	Width           *int64
	Height          *int64
	FPS             *float64
	VideoCodec      string
	AudioCodec      string
	BitrateKbps     *float64
}

type ffprobeOutput struct {
	Streams []struct {
		CodecName    string `json:"codec_name"`
		CodecType    string `json:"codec_type"`
		Width        int64  `json:"width"`
		Height       int64  `json:"height"`
		Duration     string `json:"duration"`
		AvgFrameRate string `json:"avg_frame_rate"`
		RFrameRate   string `json:"r_frame_rate"`
		BitRate      string `json:"bit_rate"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
		BitRate  string `json:"bit_rate"`
	} `json:"format"`
}

// ProbeVideo asks ffprobe to inspect the bytes supplied on stdin. Supplying a
// reader instead of a path lets the worker inspect the object it just stored
// through the same Storage interface used by filesystem and S3 backends.
//
// A pipe cannot seek, so a container whose moov atom trails the media data may
// not be probeable this way; use ProbeVideoFile when the bytes are on disk.
func ProbeVideo(ctx context.Context, media io.Reader) (VideoProbe, error) {
	if media == nil {
		return VideoProbe{}, fmt.Errorf("video reader is nil")
	}
	return runVideoProbe(ctx, media, "pipe:0")
}

// ProbeVideoFile inspects a media file on disk. ffprobe can seek in a real
// file, so this also reads containers whose index sits at the end — which is
// how ffmpeg-muxed downloads (gallery-dl's ytdl route, for one) come out.
func ProbeVideoFile(ctx context.Context, path string) (VideoProbe, error) {
	if path == "" {
		return VideoProbe{}, fmt.Errorf("video path is empty")
	}
	return runVideoProbe(ctx, nil, path)
}

func runVideoProbe(ctx context.Context, stdin io.Reader, input string) (VideoProbe, error) {
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-show_entries", "format=duration,bit_rate:stream=codec_type,codec_name,width,height,duration,avg_frame_rate,r_frame_rate,bit_rate",
		"-of", "json",
		input,
	)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			return VideoProbe{}, fmt.Errorf("ffprobe: %w", err)
		}
		return VideoProbe{}, fmt.Errorf("ffprobe: %w: %s", err, detail)
	}

	var raw ffprobeOutput
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		return VideoProbe{}, fmt.Errorf("decode ffprobe output: %w", err)
	}

	probe := VideoProbe{
		DurationSeconds: positiveProbeFloat(raw.Format.Duration),
		BitrateKbps:     probeBitrateKbps(raw.Format.BitRate),
	}
	for _, stream := range raw.Streams {
		switch stream.CodecType {
		case "video":
			if probe.VideoCodec != "" {
				continue
			}
			probe.VideoCodec = strings.TrimSpace(stream.CodecName)
			if stream.Width > 0 {
				width := stream.Width
				probe.Width = &width
			}
			if stream.Height > 0 {
				height := stream.Height
				probe.Height = &height
			}
			probe.FPS = probeFrameRate(stream.AvgFrameRate, stream.RFrameRate)
			if probe.BitrateKbps == nil {
				probe.BitrateKbps = probeBitrateKbps(stream.BitRate)
			}
			if probe.DurationSeconds == nil {
				probe.DurationSeconds = positiveProbeFloat(stream.Duration)
			}
		case "audio":
			if probe.AudioCodec == "" {
				probe.AudioCodec = strings.TrimSpace(stream.CodecName)
			}
		}
	}
	return probe, nil
}

// BackfillVideoMetadata reconciles normalized intrinsic media facts with the
// stored artifact. Valid probe values are authoritative; existing provider
// values survive only where ffprobe did not return a usable value.
func BackfillVideoMetadata(metadataJSON []byte, probe VideoProbe) ([]byte, error) {
	var metadata VideoMetadata
	if err := json.Unmarshal(metadataJSON, &metadata); err != nil {
		return nil, fmt.Errorf("decode normalized video metadata: %w", err)
	}
	if metadata.SchemaVersion == "" {
		return metadataJSON, nil
	}

	metadata.DurationSeconds = firstVideoFloat(probe.DurationSeconds, metadata.DurationSeconds)
	metadata.Media.Width = firstVideoInt(probe.Width, metadata.Media.Width)
	metadata.Media.Height = firstVideoInt(probe.Height, metadata.Media.Height)
	metadata.Media.FPS = firstVideoFloat(probe.FPS, metadata.Media.FPS)
	metadata.Media.VideoCodec = firstVideoString(probe.VideoCodec, metadata.Media.VideoCodec)
	metadata.Media.AudioCodec = firstVideoString(probe.AudioCodec, metadata.Media.AudioCodec)
	metadata.Media.BitrateKbps = firstVideoFloat(probe.BitrateKbps, metadata.Media.BitrateKbps)
	return MarshalVideoMetadata(&metadata)
}

// ProbeGalleryVideoFiles reads intrinsic facts (duration, dimensions) off the
// video files of a gallery bundle before it is zipped, writing them into the
// matching GalleryFile entries in place.
//
// This is the only moment those facts are knowable for a gallery: provider
// records rarely carry a per-slide duration, and once the bundle is a stored
// ZIP the bytes are behind an immutable object. Without it, a single-video
// post archived through the gallery flow serves a video manifest with no
// duration_seconds — an honest absence, but an avoidable one, and one that
// blocks any consumer integrating over playback time. Probe values win over
// provider values where both exist, matching BackfillVideoMetadata: the probe
// describes the exact bytes stored. A probe failure only logs; intrinsic
// metadata is an enrichment, never an archive-success boundary.
func ProbeGalleryVideoFiles(ctx context.Context, dir string, files []GalleryFile, logWriter io.Writer) {
	for i := range files {
		file := &files[i]
		if !file.IsVideo {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		probe, err := ProbeVideoFile(probeCtx, filepath.Join(dir, file.Name))
		cancel()
		if err != nil {
			if logWriter != nil {
				fmt.Fprintf(logWriter, "Warning: could not probe %s for intrinsic video metadata: %v\n", file.Name, err)
			}
			continue
		}
		if probe.DurationSeconds != nil {
			file.DurationSeconds = probe.DurationSeconds
		}
		if probe.Width != nil {
			file.Width = int(*probe.Width)
		}
		if probe.Height != nil {
			file.Height = int(*probe.Height)
		}
	}
}

func positiveProbeFloat(value string) *float64 {
	number, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || number <= 0 {
		return nil
	}
	return &number
}

func probeBitrateKbps(value string) *float64 {
	bitsPerSecond := positiveProbeFloat(value)
	if bitsPerSecond == nil {
		return nil
	}
	kbps := *bitsPerSecond / 1000
	return &kbps
}

func probeFrameRate(values ...string) *float64 {
	for _, value := range values {
		parts := strings.Split(strings.TrimSpace(value), "/")
		if len(parts) != 2 {
			if fps := positiveProbeFloat(value); fps != nil {
				return fps
			}
			continue
		}
		numerator, err1 := strconv.ParseFloat(parts[0], 64)
		denominator, err2 := strconv.ParseFloat(parts[1], 64)
		if err1 == nil && err2 == nil && numerator > 0 && denominator > 0 {
			fps := numerator / denominator
			return &fps
		}
	}
	return nil
}
