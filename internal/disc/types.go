package disc

import "time"

type DiscType int

const (
	DiscTypeUnknown DiscType = iota
	DiscTypeBluRay
	DiscTypeDVD
	DiscTypeCD
)

func (d DiscType) String() string {
	switch d {
	case DiscTypeBluRay:
		return "bluray"
	case DiscTypeDVD:
		return "dvd"
	case DiscTypeCD:
		return "cd"
	default:
		return "unknown"
	}
}

// MKVTitle is a single title as reported by `makemkvcon -r info`.
type MKVTitle struct {
	Index           int
	Name            string
	Duration        time.Duration
	ChapterCount    int
	AudioTrackCount int
	Tracks          []Track
	// SizeGB is the estimated output size in gigabytes; 0 means not reported.
	SizeGB float64
	// SourceFileName is the source segment map reported by makemkvcon.
	SourceFileName string
	// AngleNumber is the camera angle number for multi-angle discs; 0 means not an angle.
	AngleNumber int
}

// Track is a single stream track as reported by SINFO lines.
type Track struct {
	Index       int
	Type        string // From SINFO attribute 1
	CodecID     string // From SINFO attribute 5
	CodecLong   string // From SINFO attribute 6
	AudioLayout string // From SINFO attribute 2 (e.g. "Surround 5.1")
	Language    string // From SINFO attribute 3 (ISO code)
	CodecDesc   string // From SINFO attribute 7 (e.g. "DTS-HD Master Audio")
	Channels    int    // From SINFO attribute 14 (audio only)
	Resolution  string // From SINFO attribute 19 (video only, e.g. "1920x1080")
}

// ClassifiedDisc is the result of scanning + classifying titles on a disc.
type ClassifiedDisc struct {
	Device   string
	Type     DiscType
	Titles   []MKVTitle
	DiscName string
	// Warnings collects MSG lines from makemkvcon that may indicate problems.
	Warnings []string
	// DriveName is makemkvcon's name for the scanned drive, e.g.
	// "BD-RE ASUS BW-16D1HT 3.10 KLIO4993744" (type, model, firmware, serial).
	DriveName string
	// LibreDrive is makemkvcon's LibreDrive status for the scanned drive,
	// e.g. "Using LibreDrive mode (v06.3 id=0FA242DD4D0B)"; empty if not reported.
	LibreDrive string
}
