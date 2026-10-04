package ripper

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/8bitreid/simplerip/internal/disc"
)

// TitleScore rates one candidate title from scan data alone. Higher is better.
// It mirrors the post-rip scoring in internal/inspect so that the title chosen
// before ripping is the one that would later be kept as the best duplicate.
type TitleScore struct {
	Total        int
	Disqualified bool // audio languages are known and none is English
	Audio        int  // codec rank of the best English audio track
	Channels     int
	Subs         int
	Video        int
	Size         int
	BestAudio    string
}

// Label is a short human-readable breakdown for job history.
func (s TitleScore) Label() string {
	if s.Disqualified {
		return "no English audio"
	}
	subs := "no eng subs"
	if s.Subs > 0 {
		subs = "eng subs"
	}
	return fmt.Sprintf("score %d (%s; %s)", s.Total, s.BestAudio, subs)
}

func isEnglish(lang string) bool {
	l := strings.ToLower(lang)
	return l == "eng" || l == "en" || l == "english"
}

// audioCodecRank ranks a track's codec: lossless > DTS > Dolby > the rest.
func audioCodecRank(t disc.Track) int {
	tag := strings.ToUpper(t.CodecID)
	desc := strings.ToUpper(t.CodecLong + " " + t.CodecDesc)
	switch {
	case strings.Contains(tag, "TRUEHD"):
		return 100
	case strings.Contains(tag, "DTS"):
		switch {
		case strings.Contains(desc, "MASTER") || strings.Contains(desc, " MA"):
			return 90
		case strings.Contains(desc, "HIGH RES") || strings.Contains(desc, "HRA"):
			return 80
		}
		return 60
	case strings.Contains(tag, "FLAC"), strings.Contains(tag, "PCM"):
		return 85
	case strings.Contains(tag, "EAC3"), strings.Contains(desc, "DIGITAL PLUS"):
		return 50
	case strings.Contains(tag, "AC3"):
		return 40
	case strings.Contains(tag, "AAC"):
		return 30
	case strings.Contains(tag, "MP3"):
		return 20
	}
	return 10
}

func channelRank(n int) int {
	switch {
	case n >= 8:
		return 40
	case n == 7:
		return 35
	case n >= 6:
		return 30
	case n == 2:
		return 10
	case n == 0:
		return 0
	}
	return 5
}

// videoRank gives a small bonus for higher resolution (e.g. "1920x1080").
func videoRank(res string) int {
	_, h, ok := strings.Cut(strings.ToLower(res), "x")
	if !ok {
		return 0
	}
	height, err := strconv.Atoi(strings.TrimSpace(h))
	switch {
	case err != nil:
		return 0
	case height >= 2000:
		return 30
	case height >= 1000:
		return 20
	case height >= 700:
		return 10
	}
	return 0
}

// ScoreTitle scores a title from its scanned tracks. When no audio track
// reports a language the English requirement is not enforced, since the
// scan simply lacked the data.
func ScoreTitle(t disc.MKVTitle) TitleScore {
	var s TitleScore
	anyLang, bestCodec := false, -1
	bestCh := 0
	for _, tr := range t.Tracks {
		switch tr.Type {
		case "Audio":
			if tr.Language != "" {
				anyLang = true
			}
			if tr.Language != "" && !isEnglish(tr.Language) {
				continue
			}
			c := audioCodecRank(tr)
			ch := channelRank(tr.Channels)
			if c+ch > bestCodec+bestCh {
				bestCodec, bestCh = c, ch
				s.BestAudio = strings.TrimSpace(strings.Join(strings.Fields(tr.CodecDesc+" "+tr.AudioLayout), " "))
				if s.BestAudio == "" {
					s.BestAudio = tr.CodecID
				}
			}
		case "Subtitles":
			if isEnglish(tr.Language) {
				s.Subs = 10
			}
		case "Video":
			if v := videoRank(tr.Resolution); v > s.Video {
				s.Video = v
			}
		}
	}
	if bestCodec < 0 {
		if anyLang {
			s.Disqualified = true
			return s
		}
		bestCodec, bestCh = 0, 0
	}
	s.Audio, s.Channels = bestCodec, bestCh
	// Size is a tiebreak only: one point per 3 GB, capped at 5.
	s.Size = min(int(t.SizeGB/3), 5)
	s.Total = s.Audio + s.Channels + s.Subs + s.Video + s.Size
	return s
}
