package metadata

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMovieDetailsFolderName(t *testing.T) {
	tests := []struct {
		name string
		d    MovieDetails
		want string
	}{
		{name: "with year", d: MovieDetails{Title: "A/B: C", Year: "2001"}, want: "A-B - C (2001)"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.FolderName(); got != tc.want {
				t.Fatalf("FolderName() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMovieDetailsEditionLabel(t *testing.T) {
	tests := []struct {
		name string
		d    MovieDetails
		dur  time.Duration
		want string
	}{
		{
			name: "no runtime baseline",
			d:    MovieDetails{RuntimeMinutes: 0},
			dur:  100 * time.Minute,
			want: "",
		},
		{
			name: "within tolerance theatrical",
			d:    MovieDetails{RuntimeMinutes: 100},
			dur:  102 * time.Minute,
			want: "",
		},
		{
			name: "longer alternate",
			d:    MovieDetails{RuntimeMinutes: 100},
			dur:  106 * time.Minute,
			want: "Alternate",
		},
		{
			name: "shorter abridged",
			d:    MovieDetails{RuntimeMinutes: 100},
			dur:  94 * time.Minute,
			want: "Abridged",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.EditionLabel(tc.dur); got != tc.want {
				t.Fatalf("EditionLabel() = %q, want %q", got, tc.want)
			}
		})
	}
}

func newEnrichTestClients(t *testing.T, tmdbStatus int, tmdbBody string, omdbStatus int, omdbBody string, useOMDb bool) (*Client, *OMDbClient) {
	t.Helper()
	tmdbServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tmdbStatus != 0 {
			w.WriteHeader(tmdbStatus)
			return
		}
		_, _ = fmt.Fprint(w, tmdbBody)
	}))
	t.Cleanup(tmdbServer.Close)

	tmdbClient := NewClient("tmdb-key")
	tmdbClient.httpClient = &http.Client{
		Transport: rewriteTransport{
			base:       mustParseURL(t, tmdbServer.URL),
			targetHost: "api.themoviedb.org",
			rt:         tmdbServer.Client().Transport,
		},
	}

	if !useOMDb {
		return tmdbClient, nil
	}

	omdbServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if omdbStatus != 0 {
			w.WriteHeader(omdbStatus)
			return
		}
		_, _ = fmt.Fprint(w, omdbBody)
	}))
	t.Cleanup(omdbServer.Close)

	omdbClient := NewOMDbClient("omdb-key")
	omdbClient.httpClient = &http.Client{
		Transport: rewriteTransport{
			base:       mustParseURL(t, omdbServer.URL),
			targetHost: "www.omdbapi.com",
			rt:         omdbServer.Client().Transport,
		},
	}

	return tmdbClient, omdbClient
}

func TestEnrichTable(t *testing.T) {
	tests := []struct {
		name            string
		tmdbStatus      int
		tmdbBody        string
		useOMDb         bool
		omdbStatus      int
		omdbBody        string
		wantErrPart     string
		wantRuntime     int
		wantTMDbRuntime int
		wantYear        string
		wantConflict    bool
		wantDirector    string
		wantRTRating    string
	}{
		{
			name:            "tmdb only",
			tmdbBody:        `{"id":7,"title":"Pitch Black","runtime":109,"imdb_id":"tt0134847"}`,
			useOMDb:         false,
			wantRuntime:     109,
			wantTMDbRuntime: 109,
			wantYear:        "2000",
			wantConflict:    false,
		},
		{
			name:            "omdb runtime averages",
			tmdbBody:        `{"id":7,"title":"Pitch Black","runtime":100,"imdb_id":"tt0134847"}`,
			useOMDb:         true,
			omdbBody:        `{"Director":"David Twohy","Actors":"Radha Mitchell","Genre":"Sci-Fi","imdbRating":"7.0","Runtime":"102 min","Ratings":[{"Source":"Rotten Tomatoes","Value":"59%"}],"Response":"True"}`,
			wantRuntime:     101,
			wantTMDbRuntime: 100,
			wantYear:        "2000",
			wantConflict:    false,
			wantDirector:    "David Twohy",
			wantRTRating:    "59%",
		},
		{
			name:            "omdb conflict prefers longer",
			tmdbBody:        `{"id":7,"title":"Pitch Black","runtime":100,"imdb_id":"tt0134847"}`,
			useOMDb:         true,
			omdbBody:        `{"Runtime":"110 min","Response":"True"}`,
			wantRuntime:     110,
			wantTMDbRuntime: 100,
			wantYear:        "2000",
			wantConflict:    true,
		},
		{
			name:            "omdb failure non fatal",
			tmdbBody:        `{"id":7,"title":"Pitch Black","runtime":109,"imdb_id":"tt0134847"}`,
			useOMDb:         true,
			omdbBody:        `{"Response":"False","Error":"Movie not found"}`,
			wantRuntime:     109,
			wantTMDbRuntime: 109,
			wantYear:        "2000",
			wantConflict:    false,
		},
		{
			name:        "tmdb error",
			tmdbStatus:  http.StatusBadGateway,
			useOMDb:     false,
			wantErrPart: "tmdb detail",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmdbClient, omdbClient := newEnrichTestClients(t, tc.tmdbStatus, tc.tmdbBody, tc.omdbStatus, tc.omdbBody, tc.useOMDb)
			got, err := Enrich(context.Background(), tmdbClient, omdbClient, MovieResult{ID: 7, ReleaseDate: "2000-02-18"})
			if tc.wantErrPart != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Fatalf("Enrich() error = %v, want substring %q", err, tc.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("Enrich() error = %v", err)
			}
			if got.RuntimeMinutes != tc.wantRuntime {
				t.Fatalf("RuntimeMinutes = %d, want %d", got.RuntimeMinutes, tc.wantRuntime)
			}
			if got.TMDbRuntime != tc.wantTMDbRuntime {
				t.Fatalf("TMDbRuntime = %d, want %d", got.TMDbRuntime, tc.wantTMDbRuntime)
			}
			if got.Year != tc.wantYear {
				t.Fatalf("Year = %q, want %q", got.Year, tc.wantYear)
			}
			if got.RuntimeConflict != tc.wantConflict {
				t.Fatalf("RuntimeConflict = %v, want %v", got.RuntimeConflict, tc.wantConflict)
			}
			if got.Director != tc.wantDirector {
				t.Fatalf("Director = %q, want %q", got.Director, tc.wantDirector)
			}
			if got.RTRating != tc.wantRTRating {
				t.Fatalf("RTRating = %q, want %q", got.RTRating, tc.wantRTRating)
			}
		})
	}
}
