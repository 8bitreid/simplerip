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
	Index          int
	Name           string
	Duration       time.Duration
	ChapterCount   int
	AudioTrackCount int
	// SizeGB is the estimated output size in gigabytes; 0 means not reported.
	SizeGB         float64
	// SourceFileName is the source segment map reported by makemkvcon.
	SourceFileName string
}

// ClassifiedDisc is the result of scanning + classifying titles on a disc.
type ClassifiedDisc struct {
	Device   string
	Type     DiscType
	Titles   []MKVTitle
	DiscName string
	// Warnings collects MSG lines from makemkvcon that may indicate problems.
	Warnings []string
}
