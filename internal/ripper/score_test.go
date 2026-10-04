package ripper

import (
	"testing"

	"github.com/8bitreid/simplerip/internal/disc"
)

func TestScoreTitle(t *testing.T) {
	eng := func(codec, desc string, ch int) disc.Track {
		return disc.Track{Type: "Audio", CodecID: codec, CodecDesc: desc, Channels: ch, Language: "eng"}
	}
	cases := []struct {
		name  string
		title disc.MKVTitle
		want  int
	}{
		{"truehd 7.1", disc.MKVTitle{Tracks: []disc.Track{eng("A_TRUEHD", "", 8)}}, 140},
		{"dts-hd ma 5.1 + eng subs", disc.MKVTitle{Tracks: []disc.Track{eng("A_DTS", "DTS-HD Master Audio", 6), {Type: "Subtitles", Language: "eng"}}}, 130},
		{"ac3 5.1 hd video", disc.MKVTitle{Tracks: []disc.Track{{Type: "Video", Resolution: "1920x1080"}, eng("A_AC3", "", 6)}}, 90},
		{"dts core stereo", disc.MKVTitle{Tracks: []disc.Track{eng("A_DTS", "DTS", 2)}}, 70},
	}
	for _, c := range cases {
		if got := ScoreTitle(c.title); got.Total != c.want || got.Disqualified {
			t.Errorf("%s: total=%d disq=%v, want %d", c.name, got.Total, got.Disqualified, c.want)
		}
	}
	nonEng := disc.MKVTitle{Tracks: []disc.Track{{Type: "Audio", CodecID: "A_AC3", Channels: 6, Language: "fra"}}}
	if !ScoreTitle(nonEng).Disqualified {
		t.Error("French-only title must be disqualified")
	}
	noLang := disc.MKVTitle{Tracks: []disc.Track{{Type: "Audio", CodecID: "A_AC3", Channels: 6}}}
	if ScoreTitle(noLang).Disqualified {
		t.Error("missing language data must not disqualify")
	}
}

func TestParseStreamInfoFields(t *testing.T) {
	m := map[int]*disc.MKVTitle{}
	for _, l := range []string{
		`11,0,1,6201,"Video"`, `11,0,19,0,"720x480"`,
		`11,2,1,6202,"Audio"`, `11,2,3,0,"eng"`, `11,2,5,0,"A_DTS"`, `11,2,14,0,"6"`, `11,2,2,0,"Surround 5.1"`,
	} {
		parseStreamInfo(l, m)
	}
	a := m[11].Tracks[2]
	if a.Language != "eng" || a.Channels != 6 || a.CodecID != "A_DTS" || a.AudioLayout != "Surround 5.1" {
		t.Errorf("audio track = %+v", a)
	}
	if m[11].Tracks[0].Resolution != "720x480" {
		t.Errorf("video track = %+v", m[11].Tracks[0])
	}
}
