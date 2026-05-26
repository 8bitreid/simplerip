// Package inspect uses ffprobe to read metadata from finished MKV files.
package inspect

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// FileInfo is the metadata ffprobe extracts from a single MKV file.
type FileInfo struct {
	Path       string
	Duration   time.Duration
	SizeBytes  int64
	VideoCodec string
	Resolution string // e.g. "1920x1080"
	Audio      []AudioTrack
	Subtitles  int
}

// AudioTrack describes one audio stream.
type AudioTrack struct {
	Codec    string // e.g. "dts", "ac3", "truehd"
	Profile  string // e.g. "DTS-HD MA", "DTS", "" for non-DTS
	Channels int
	Layout   string // e.g. "7.1", "5.1(side)", "stereo"
	Language string // e.g. "eng", "fra"
}

// DisplayCodec returns a human-readable codec name including profile where relevant.
func (a AudioTrack) DisplayCodec() string {
	if a.Codec == "dts" && a.Profile != "" {
		return a.Profile // "DTS-HD MA", "DTS-HD HRA", "DTS"
	}
	switch a.Codec {
	case "truehd":
		return "TrueHD"
	case "eac3":
		return "EAC3"
	case "ac3":
		return "AC3"
	case "flac":
		return "FLAC"
	case "pcm_bluray", "pcm_s16le", "pcm_s24le":
		return "PCM"
	default:
		return strings.ToUpper(a.Codec)
	}
}

// String formats the track for display: "TrueHD 7.1 (eng)"
func (a AudioTrack) String() string {
	lang := a.Language
	if lang == "" {
		lang = "und"
	}
	return fmt.Sprintf("%s %s (%s)", a.DisplayCodec(), a.Layout, lang)
}

type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat  `json:"format"`
}

type ffprobeStream struct {
	CodecType     string            `json:"codec_type"`
	CodecName     string            `json:"codec_name"`
	Profile       string            `json:"profile"`
	Width         int               `json:"width"`
	Height        int               `json:"height"`
	Channels      int               `json:"channels"`
	ChannelLayout string            `json:"channel_layout"`
	Tags          map[string]string `json:"tags"`
}

type ffprobeFormat struct {
	Duration string `json:"duration"`
	Size     string `json:"size"`
}

// Probe runs ffprobe on path and returns structured metadata.
func Probe(ctx context.Context, path string) (*FileInfo, error) {
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe %q: %w", path, err)
	}

	var raw ffprobeOutput
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse ffprobe output: %w", err)
	}

	info := &FileInfo{Path: path}

	// Format-level fields.
	if raw.Format.Duration != "" {
		if secs, err := strconv.ParseFloat(raw.Format.Duration, 64); err == nil {
			info.Duration = time.Duration(secs * float64(time.Second))
		}
	}
	if raw.Format.Size != "" {
		if b, err := strconv.ParseInt(raw.Format.Size, 10, 64); err == nil {
			info.SizeBytes = b
		}
	}

	// Per-stream fields.
	for _, s := range raw.Streams {
		switch s.CodecType {
		case "video":
			if info.VideoCodec == "" {
				info.VideoCodec = s.CodecName
				if s.Width > 0 && s.Height > 0 {
					info.Resolution = fmt.Sprintf("%dx%d", s.Width, s.Height)
				}
			}
		case "audio":
			lang := ""
			if s.Tags != nil {
				lang = s.Tags["language"]
			}
			info.Audio = append(info.Audio, AudioTrack{
				Codec:    s.CodecName,
				Profile:  s.Profile,
				Channels: s.Channels,
				Layout:   s.ChannelLayout,
				Language: lang,
			})
		case "subtitle":
			info.Subtitles++
		}
	}

	return info, nil
}

// DurationWithin returns true if a and b are within tolerance of each other.
func DurationWithin(a, b, tolerance time.Duration) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= tolerance
}
