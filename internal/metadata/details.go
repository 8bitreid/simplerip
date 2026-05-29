package metadata

import (
	"context"
	"fmt"
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
