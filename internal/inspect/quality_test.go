package inspect

import (
	"testing"
)

func TestAudioCodecScore(t *testing.T) {
	tests := []struct {
		name string
		in   AudioTrack
		want int
	}{
		{name: "truehd", in: AudioTrack{Codec: "truehd"}, want: 100},
		{name: "dts ma", in: AudioTrack{Codec: "dts", Profile: "DTS-HD MA"}, want: 90},
		{name: "dts hra", in: AudioTrack{Codec: "dts", Profile: "DTS-HD HRA"}, want: 80},
		{name: "dts core", in: AudioTrack{Codec: "dts", Profile: "DTS"}, want: 60},
		{name: "flac", in: AudioTrack{Codec: "flac"}, want: 85},
		{name: "pcm", in: AudioTrack{Codec: "pcm_bluray"}, want: 85},
		{name: "eac3", in: AudioTrack{Codec: "eac3"}, want: 50},
		{name: "ac3", in: AudioTrack{Codec: "ac3"}, want: 40},
		{name: "aac", in: AudioTrack{Codec: "aac"}, want: 30},
		{name: "mp3", in: AudioTrack{Codec: "mp3"}, want: 20},
		{name: "unknown", in: AudioTrack{Codec: "opus"}, want: 10},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := audioCodecScore(tc.in); got != tc.want {
				t.Fatalf("audioCodecScore() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestChannelScore(t *testing.T) {
	tests := []struct {
		channels int
		want     int
	}{
		{channels: 8, want: 40},
		{channels: 7, want: 35},
		{channels: 6, want: 30},
		{channels: 2, want: 10},
		{channels: 1, want: 5},
	}

	for _, tc := range tests {
		if got := channelScore(tc.channels); got != tc.want {
			t.Fatalf("channelScore(%d) = %d, want %d", tc.channels, got, tc.want)
		}
	}
}

func TestIsEnglish(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{in: "eng", want: true},
		{in: "EN", want: true},
		{in: "English", want: true},
		{in: "fra", want: false},
	}

	for _, tc := range tests {
		if got := isEnglish(tc.in); got != tc.want {
			t.Fatalf("isEnglish(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestScoreAndLabel(t *testing.T) {
	tests := []struct {
		name      string
		info      *FileInfo
		sizeBytes int64
		want      QualityScore
		wantLabel string
	}{
		{
			name: "no english audio disqualified",
			info: &FileInfo{Audio: []AudioTrack{{Codec: "ac3", Channels: 6, Layout: "5.1", Language: "fra"}}},
			want: QualityScore{},
			wantLabel: "DISQUALIFIED (no English audio)",
		},
		{
			name: "best english selected with subs and size cap",
			info: &FileInfo{
				Audio: []AudioTrack{
					{Codec: "ac3", Channels: 6, Layout: "5.1", Language: "eng"},
					{Codec: "truehd", Channels: 8, Layout: "7.1", Language: "eng"},
					{Codec: "dts", Profile: "DTS-HD MA", Channels: 6, Layout: "5.1", Language: "fra"},
				},
				Subtitles: 1,
			},
			sizeBytes: 70 * 1024 * 1024 * 1024,
			want: QualityScore{
				Total:          155,
				HasEnglish:     true,
				HasEnglishSubs: true,
				BestAudio:      AudioTrack{Codec: "truehd", Channels: 8, Layout: "7.1", Language: "eng"},
				BestCodecScore: 100,
				BestChanScore:  40,
				SubScore:       10,
				SizeScore:      5,
			},
			wantLabel: "TrueHD · 7.1 · eng subs",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Score(tc.info, tc.sizeBytes)
			if got.Total != tc.want.Total || got.HasEnglish != tc.want.HasEnglish || got.HasEnglishSubs != tc.want.HasEnglishSubs || got.BestCodecScore != tc.want.BestCodecScore || got.BestChanScore != tc.want.BestChanScore || got.SubScore != tc.want.SubScore || got.SizeScore != tc.want.SizeScore {
				t.Fatalf("Score() = %+v, want %+v", got, tc.want)
			}
			if got.BestAudio != tc.want.BestAudio {
				t.Fatalf("Score() BestAudio = %+v, want %+v", got.BestAudio, tc.want.BestAudio)
			}
			if gotLabel := got.ScoreLabel(); gotLabel != tc.wantLabel {
				t.Fatalf("ScoreLabel() = %q, want %q", gotLabel, tc.wantLabel)
			}
		})
	}
}