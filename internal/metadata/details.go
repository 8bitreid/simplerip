package metadata

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// MovieDetails is the merged result from TMDB + OMDb.
type MovieDetails struct {
	TMDbID     int
	ImdbID     string
	Title      string
	Year       string
	Director   string
	Actors     string
	Genre      string
	ImdbRating string
	RTRating   string

	// Runtime from each source and the reconciled value used for edition matching.
	TMDbRuntime     int  // minutes
	OMDbRuntime     int  // minutes
	RuntimeMinutes  int  // reconciled: average if sources agree, flagged if they diverge
	RuntimeConflict bool // true if TMDB and OMDb differ by more than 3 min
}

// FolderName returns "Title (Year)" safe for use as a directory name.
func (d MovieDetails) FolderName() string {
	r := MovieResult{Title: d.Title, ReleaseDate: d.Year + "-01-01"}
	return r.FolderName()
}

// EditionLabel returns a label for an MKV whose duration differs from the
// reconciled theatrical runtime. Returns "" if it matches (i.e. theatrical cut).
//
// Tolerance is ±5 minutes to account for credits, trailers embedded in the
// stream, and minor runtime discrepancies between sources.
func (d MovieDetails) EditionLabel(dur time.Duration) string {
	if d.RuntimeMinutes == 0 {
		return ""
	}
	theatrical := time.Duration(d.RuntimeMinutes) * time.Minute
	diff := dur - theatrical
	if diff < 0 {
		diff = -diff
	}
	if diff <= 3*time.Minute {
		return "" // matches theatrical
	}
	if dur > theatrical {
		return "Alternate" // longer — director's/extended/unrated, we can't know which
	}
	return "Abridged" // shorter — unusual but possible
}

// Enrich fetches TMDB detail + OMDb data for the given TMDB search result.
// omdbClient may be nil; in that case only TMDB data is used.
func Enrich(ctx context.Context, tmdbClient *Client, omdbClient *OMDbClient, result MovieResult) (*MovieDetails, error) {
	detail, err := tmdbClient.GetMovie(ctx, result.ID)
	if err != nil {
		return nil, fmt.Errorf("tmdb detail: %w", err)
	}

	d := &MovieDetails{
		TMDbID:         detail.ID,
		ImdbID:         detail.ImdbID,
		Title:          detail.Title,
		Year:           result.Year(),
		TMDbRuntime:    detail.Runtime,
		RuntimeMinutes: detail.Runtime,
	}

	if omdbClient != nil && detail.ImdbID != "" {
		omdb, err := omdbClient.GetByIMDbID(ctx, detail.ImdbID)
		if err == nil {
			d.Director = omdb.Director
			d.Actors = omdb.Actors
			d.Genre = omdb.Genre
			d.ImdbRating = omdb.ImdbRating
			d.RTRating = omdb.RottenTomatoes()
			d.OMDbRuntime = omdb.RuntimeMinutes()

			// Reconcile runtimes.
			if d.OMDbRuntime > 0 {
				diff := d.TMDbRuntime - d.OMDbRuntime
				if diff < 0 {
					diff = -diff
				}
				if diff > 3 {
					d.RuntimeConflict = true
					// Use the longer one as the safer theatrical upper bound.
					if d.OMDbRuntime > d.TMDbRuntime {
						d.RuntimeMinutes = d.OMDbRuntime
					}
				} else {
					// Average when sources agree.
					d.RuntimeMinutes = (d.TMDbRuntime + d.OMDbRuntime) / 2
				}
			}
		}
		// OMDb failure is non-fatal — we continue with TMDB data only.
	}

	return d, nil
}

// parseDurationMinutes converts a duration string like "1:33:14" to minutes.
// Supports formats: "H:MM:SS" or "0:MM:SS" where H can be any number of hours.
// Returns 0 for invalid input.
func parseDurationMinutes(d string) int {
	parts := strings.Split(d, ":")
	if len(parts) != 3 {
		return 0
	}
	hours, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0
	}
	minutes, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0
	}
	seconds, err := strconv.Atoi(parts[2])
	if err != nil {
		return 0
	}
	return hours*60 + minutes + (seconds+30)/60 // round seconds to nearest minute
}

// durationToMinutes converts a time.Duration to minutes, rounding to nearest minute.
func durationToMinutes(d time.Duration) int {
	return int((d + 30*time.Second).Minutes())
}

// ScoredResult pairs a MovieResult with its match score and runtime for logging.
type ScoredResult struct {
	Result      MovieResult
	Score       int
	TMDBRuntime int // minutes from TMDB detail fetch
}

// ScoreResult calculates a match score for a TMDB result based on runtime proximity
// and popularity. Higher scores indicate better matches.
//
// Scoring:
//   - Runtime proximity: 1000 - (diff_minutes * 10), capped at 0
//     This ensures closer matches always win (10 points per minute difference)
//   - Popularity bonus: +0 to +20 (normalized, much smaller than runtime component)
//   - No runtime from TMDB: popularity only
//
// Examples (assuming max popularity = 20):
//   - Exact match (0 min diff):   1000 + 20 = 1020
//   - Close match (5 min diff):   950 + 20 = 970
//   - Moderate diff (10 min diff): 900 + 20 = 920
//   - Far match (50 min diff):    500 + 20 = 520
//   - Very far (100+ min):        0 + 20 = 20
func ScoreResult(result MovieResult, discDuration time.Duration, tmdbRuntime int) int {
	if discDuration == 0 {
		// No duration to compare, fall back to popularity only
		return normalizePopularity(result.Popularity)
	}

	discMinutes := durationToMinutes(discDuration)
	if discMinutes == 0 || tmdbRuntime == 0 {
		// Invalid duration or no TMDB runtime, fall back to popularity
		return normalizePopularity(result.Popularity)
	}

	diff := discMinutes - tmdbRuntime
	if diff < 0 {
		diff = -diff
	}

	// Distance-based scoring: subtract 10 points per minute of difference.
	// This ensures that runtime proximity always dominates over popularity.
	runtimeScore := 1000 - (diff * 10)
	if runtimeScore < 0 {
		runtimeScore = 0
	}

	popularityScore := normalizePopularity(result.Popularity)
	return runtimeScore + popularityScore
}

// normalizePopularity converts TMDB popularity (typically 0-100+) to a 0-20 score.
func normalizePopularity(pop float64) int {
	if pop <= 0 {
		return 0
	}
	// Cap at 100 and scale to 0-20
	normalized := math.Min(pop, 100.0) / 5.0
	return int(normalized)
}

// BestMatch finds the highest-scoring TMDB result based on runtime proximity
// and popularity. Returns the result, whether a runtime-based winner was
// selected over the popularity winner, and log details for the decision.
func BestMatch(ctx context.Context, tmdbClient *Client, results []MovieResult, discDuration time.Duration) (MovieResult, bool, string, error) {
	if len(results) == 0 {
		return MovieResult{}, false, "", fmt.Errorf("no results to score")
	}

	// Fetch runtime for each result and score it
	var scored []ScoredResult
	for _, r := range results {
		detail, err := tmdbClient.GetMovie(ctx, r.ID)
		if err != nil {
			// Skip results we can't fetch details for
			continue
		}
		score := ScoreResult(r, discDuration, detail.Runtime)
		scored = append(scored, ScoredResult{
			Result:      r,
			Score:       score,
			TMDBRuntime: detail.Runtime,
		})
	}

	if len(scored) == 0 {
		return MovieResult{}, false, "", fmt.Errorf("could not fetch details for any result")
	}

	// Find the highest score
	best := scored[0]
	for _, s := range scored[1:] {
		if s.Score > best.Score {
			best = s
		}
	}

	// Check if runtime winner differs from popularity winner (first result)
	runtimeWinner := best.Result.ID != scored[0].Result.ID

	// Build log message if runtime winner differs from popularity winner
	logMsg := ""
	if runtimeWinner {
		logMsg = fmt.Sprintf("runtime match: %s (%s) %dmin — overrides top result: %s (%s) %dmin",
			best.Result.Title, best.Result.Year(), best.TMDBRuntime,
			scored[0].Result.Title, scored[0].Result.Year(), scored[0].TMDBRuntime)
	}

	return best.Result, runtimeWinner, logMsg, nil
}
