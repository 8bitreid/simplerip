package ripper

import (
	"sort"
	"time"

	"github.com/8bitreid/simplerip/internal/config"
	"github.com/8bitreid/simplerip/internal/disc"
)

type DiscPattern int

const (
	DiscPatternAmbiguous DiscPattern = iota
	DiscPatternTV
	DiscPatternMovie
	DiscPatternDouble
)

func (p DiscPattern) String() string {
	switch p {
	case DiscPatternTV:
		return "TV"
	case DiscPatternMovie:
		return "Movie"
	case DiscPatternDouble:
		return "Double"
	default:
		return "Ambiguous"
	}
}

type ClassificationResult struct {
	Pattern     DiscPattern
	MainTitles  []disc.MKVTitle // rip immediately without asking
	ExtraTitles []disc.MKVTitle // ask user before ripping
	JunkTitles  []disc.MKVTitle // under min duration, silently ignored
}

// ClassifyTitles applies disc-pattern rules to a flat list of titles and
// returns how to handle each one. The caller should rip MainTitles immediately
// and ask via Discord before ripping ExtraTitles.
//
// cfg is the Detection block from config.yaml; thresholds map directly with no
// translation layer needed.
//
// Rule priority (first match wins):
//  1. 3+ titles within DurationTolerance of each other → TV (rip all)
//  2. Exactly 2 feature-length titles within DurationTolerance → Double (ask)
//  3. Exactly 1 feature-length title → Movie (rip main, ask about rest)
//  4. Everything else → Ambiguous (ask about all)
func ClassifyTitles(titles []disc.MKVTitle, cfg config.DetectionConfig) ClassificationResult {
	minExtra := time.Duration(cfg.MinExtraMinutes) * time.Minute
	minFeature := time.Duration(cfg.MinFeatureMinutes) * time.Minute
	tolerance := time.Duration(cfg.DurationToleranceSec) * time.Second

	var junk, candidates []disc.MKVTitle
	for _, t := range titles {
		if t.Duration < minExtra {
			junk = append(junk, t)
		} else {
			candidates = append(candidates, t)
		}
	}

	if len(candidates) == 0 {
		return ClassificationResult{Pattern: DiscPatternAmbiguous, JunkTitles: junk}
	}

	sorted := make([]disc.MKVTitle, len(candidates))
	copy(sorted, candidates)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Duration < sorted[j].Duration
	})

	largest := largestCluster(buildClusters(sorted, tolerance))

	// Rule 1: TV
	if len(largest) >= cfg.TVThreshold {
		return ClassificationResult{
			Pattern:    DiscPatternTV,
			MainTitles: candidates,
			JunkTitles: junk,
		}
	}

	// Rule 2: Double feature — two same-duration titles, both feature-length.
	if len(largest) == 2 && largest[0].Duration >= minFeature && largest[1].Duration >= minFeature {
		return ClassificationResult{
			Pattern:     DiscPatternDouble,
			ExtraTitles: candidates,
			JunkTitles:  junk,
		}
	}

	// Rule 3: Single movie.
	var features, extras []disc.MKVTitle
	for _, t := range candidates {
		if t.Duration >= minFeature {
			features = append(features, t)
		} else {
			extras = append(extras, t)
		}
	}
	if len(features) == 1 {
		return ClassificationResult{
			Pattern:     DiscPatternMovie,
			MainTitles:  features,
			ExtraTitles: extras,
			JunkTitles:  junk,
		}
	}

	// Rule 4: Ambiguous.
	return ClassificationResult{
		Pattern:     DiscPatternAmbiguous,
		ExtraTitles: candidates,
		JunkTitles:  junk,
	}
}

// buildClusters groups a duration-sorted slice into consecutive runs where
// the spread (last.Duration − first.Duration) stays within tolerance.
func buildClusters(sorted []disc.MKVTitle, tolerance time.Duration) [][]disc.MKVTitle {
	if len(sorted) == 0 {
		return nil
	}
	var clusters [][]disc.MKVTitle
	start := 0
	for i := 1; i <= len(sorted); i++ {
		if i == len(sorted) || sorted[i].Duration-sorted[start].Duration > tolerance {
			clusters = append(clusters, sorted[start:i])
			start = i
		}
	}
	return clusters
}

// largestCluster returns the cluster with the most entries.
// On a tie the first (lowest-duration) cluster wins.
func largestCluster(clusters [][]disc.MKVTitle) []disc.MKVTitle {
	var largest []disc.MKVTitle
	for _, c := range clusters {
		if len(c) > len(largest) {
			largest = c
		}
	}
	return largest
}
