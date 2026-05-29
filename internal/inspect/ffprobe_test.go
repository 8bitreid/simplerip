package inspect

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func installFakeFFProbe(t *testing.T, jsonOut string, fail bool) {
	t.Helper()
	binDir := t.TempDir()
	script := filepath.Join(binDir, "ffprobe")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+
		"if [ \"$FFPROBE_FAIL\" = \"1\" ]; then exit 2; fi\n"+
		"printf '%s' \"$FFPROBE_JSON\"\n"), 0o755); err != nil {
		t.Fatalf("write fake ffprobe: %v", err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("FFPROBE_JSON", jsonOut)
	if fail {
		t.Setenv("FFPROBE_FAIL", "1")
	}
}

func TestAudioTrackDisplayAndString(t *testing.T) {
	tests := []struct {
		name        string
		track       AudioTrack
		wantDisplay string
		wantString  string
	}{
		{name: "dts profile", track: AudioTrack{Codec: "dts", Profile: "DTS-HD MA", Layout: "5.1", Language: "eng"}, wantDisplay: "DTS-HD MA", wantString: "DTS-HD MA 5.1 (eng)"},
		{name: "truehd", track: AudioTrack{Codec: "truehd", Layout: "7.1", Language: "eng"}, wantDisplay: "TrueHD", wantString: "TrueHD 7.1 (eng)"},
		{name: "pcm", track: AudioTrack{Codec: "pcm_s24le", Layout: "stereo"}, wantDisplay: "PCM", wantString: "PCM stereo (und)"},
		{name: "unknown upper", track: AudioTrack{Codec: "opus", Layout: "stereo", Language: "eng"}, wantDisplay: "OPUS", wantString: "OPUS stereo (eng)"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.track.DisplayCodec(); got != tc.wantDisplay {
				t.Fatalf("DisplayCodec() = %q, want %q", got, tc.wantDisplay)
			}
			if got := tc.track.String(); got != tc.wantString {
				t.Fatalf("String() = %q, want %q", got, tc.wantString)
			}
		})
	}
}

func TestProbe(t *testing.T) {
	tests := []struct {
		name        string
		jsonOut     string
		fail        bool
		wantErrPart string
		assert      func(t *testing.T, got *FileInfo)
	}{
		{
			name: "parses video audio subtitle and format",
			jsonOut: `{
				"format": {"duration":"120.5","size":"10737418240"},
				"streams": [
					{"codec_type":"video","codec_name":"h264","width":1920,"height":1080},
					{"codec_type":"audio","codec_name":"truehd","channels":8,"channel_layout":"7.1","tags":{"language":"eng"}},
					{"codec_type":"subtitle","codec_name":"hdmv_pgs_subtitle"}
				]
			}`,
			assert: func(t *testing.T, got *FileInfo) {
				t.Helper()
				if got.VideoCodec != "h264" || got.Resolution != "1920x1080" {
					t.Fatalf("video fields = %+v", got)
				}
				if got.Subtitles != 1 || len(got.Audio) != 1 {
					t.Fatalf("audio/subtitles fields = %+v", got)
				}
				if got.Audio[0].Language != "eng" || got.Audio[0].Layout != "7.1" {
					t.Fatalf("audio track = %+v", got.Audio[0])
				}
				if got.Duration != 120*time.Second+500*time.Millisecond {
					t.Fatalf("Duration = %v", got.Duration)
				}
				if got.SizeBytes != 10737418240 {
					t.Fatalf("SizeBytes = %d", got.SizeBytes)
				}
			},
		},
		{
			name:        "command failure",
			jsonOut:     `{}`,
			fail:        true,
			wantErrPart: "ffprobe",
		},
		{
			name:        "invalid json",
			jsonOut:     `not-json`,
			wantErrPart: "parse ffprobe output",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			installFakeFFProbe(t, tc.jsonOut, tc.fail)
			got, err := Probe(context.Background(), "/tmp/file.mkv")
			if tc.wantErrPart != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Fatalf("Probe() error = %v, want substring %q", err, tc.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("Probe() error = %v", err)
			}
			if tc.assert != nil {
				tc.assert(t, got)
			}
		})
	}
}

func TestDurationWithin(t *testing.T) {
	tests := []struct {
		name      string
		a         time.Duration
		b         time.Duration
		tolerance time.Duration
		want      bool
	}{
		{name: "within", a: 100 * time.Second, b: 120 * time.Second, tolerance: 20 * time.Second, want: true},
		{name: "outside", a: 100 * time.Second, b: 121 * time.Second, tolerance: 20 * time.Second, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DurationWithin(tc.a, tc.b, tc.tolerance); got != tc.want {
				t.Fatalf("DurationWithin() = %v, want %v", got, tc.want)
			}
		})
	}
}
