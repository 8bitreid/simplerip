package inspect

import "strings"

// QualityScore rates a FileInfo for keeper selection. Higher is better.
// A file with no English audio scores 0 and should never be chosen.
type QualityScore struct {
	Total          int
	HasEnglish     bool
	HasEnglishSubs bool
	BestAudio      AudioTrack
	BestCodecScore int
	BestChanScore  int
	SubScore       int
	SizeScore      int // tiebreaker only
}

// audioCodecScore returns the codec quality rank (higher = better lossless/lossy).
func audioCodecScore(t AudioTrack) int {
	switch t.Codec {
	case "truehd":
		return 100 // TrueHD / Atmos (lossless)
	case "dts":
		p := strings.ToUpper(t.Profile)
		switch {
		case strings.Contains(p, "MA"):
			return 90 // DTS-HD Master Audio (lossless)
		case strings.Contains(p, "HRA"):
			return 80 // DTS-HD High Resolution (lossy but high bitrate)
		default:
			return 60 // DTS core (lossy)
		}
	case "flac":
		return 85 // FLAC lossless
	case "pcm_bluray", "pcm_s16le", "pcm_s24le", "pcm_s32le":
		return 85 // Uncompressed PCM (lossless)
	case "eac3":
		return 50 // Dolby Digital Plus (lossy)
	case "ac3":
		return 40 // Dolby Digital (lossy)
	case "aac":
		return 30
	case "mp3":
		return 20
	default:
		return 10
	}
}

// channelScore maps channel count to a score.
func channelScore(channels int) int {
	switch {
	case channels >= 8:
		return 40 // 7.1
	case channels == 7:
		return 35 // 6.1
	case channels >= 6:
		return 30 // 5.1
	case channels == 2:
		return 10 // stereo
	default:
		return 5 // mono
	}
}

// isEnglish returns true for common English language tags.
func isEnglish(lang string) bool {
	l := strings.ToLower(lang)
	return l == "eng" || l == "en" || l == "english"
}

// Score computes a QualityScore for info.
// sizeBytes is the file size used only as a final tiebreaker.
func Score(info *FileInfo, sizeBytes int64) QualityScore {
	s := QualityScore{}

	// Find best English audio track.
	bestCodec := -1
	for _, a := range info.Audio {
		if !isEnglish(a.Language) {
			continue
		}
		s.HasEnglish = true
		cs := audioCodecScore(a)
		if cs > bestCodec {
			bestCodec = cs
			s.BestAudio = a
			s.BestCodecScore = cs
			s.BestChanScore = channelScore(a.Channels)
		}
	}

	if !s.HasEnglish {
		// No English audio — disqualified.
		return s
	}

	// English subtitle presence.
	// We don't have per-subtitle language in FileInfo yet; use Subtitles > 0 as proxy.
	if info.Subtitles > 0 {
		s.HasEnglishSubs = true
		s.SubScore = 10
	}

	// Size tiebreaker — normalise to 0–5 range (avoids swamping codec score).
	// We use GB units: every 10 GB = 1 point, capped at 5.
	sizePoints := int(sizeBytes/(10*1024*1024*1024))
	if sizePoints > 5 {
		sizePoints = 5
	}
	s.SizeScore = sizePoints

	s.Total = s.BestCodecScore + s.BestChanScore + s.SubScore + s.SizeScore
	return s
}

// ScoreLabel returns a human-readable breakdown for reports.
func (s QualityScore) ScoreLabel() string {
	if !s.HasEnglish {
		return "DISQUALIFIED (no English audio)"
	}
	subs := "no eng subs"
	if s.HasEnglishSubs {
		subs = "eng subs"
	}
	return strings.Join([]string{
		s.BestAudio.DisplayCodec(),
		s.BestAudio.Layout,
		subs,
	}, " · ")
}
