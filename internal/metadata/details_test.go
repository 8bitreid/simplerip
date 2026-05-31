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

func TestParseDurationMinutes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{name: "standard format", input: "1:33:14", want: 93},
		{name: "zero hours", input: "0:93:00", want: 93},
		{name: "long runtime", input: "2:20:05", want: 140},
		{name: "round up seconds", input: "1:30:45", want: 91},
		{name: "round down seconds", input: "1:30:20", want: 90},
		{name: "invalid format - missing parts", input: "1:30", want: 0},
		{name: "invalid format - extra parts", input: "1:30:00:00", want: 0},
		{name: "invalid format - non-numeric", input: "a:b:c", want: 0},
		{name: "empty string", input: "", want: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseDurationMinutes(tc.input)
			if got != tc.want {
				t.Errorf("parseDurationMinutes(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

func TestDurationToMinutes(t *testing.T) {
	tests := []struct {
		name string
		dur  time.Duration
		want int
	}{
		{name: "93 minutes", dur: 93 * time.Minute, want: 93},
		{name: "1h33m14s", dur: 1*time.Hour + 33*time.Minute + 14*time.Second, want: 93},
		{name: "round up 45s", dur: 90*time.Minute + 45*time.Second, want: 91},
		{name: "round down 20s", dur: 90*time.Minute + 20*time.Second, want: 90},
		{name: "zero duration", dur: 0, want: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := durationToMinutes(tc.dur)
			if got != tc.want {
				t.Errorf("durationToMinutes(%v) = %d, want %d", tc.dur, got, tc.want)
			}
		})
	}
}

func TestScoreResult(t *testing.T) {
	tests := []struct {
		name         string
		result       MovieResult
		discDuration time.Duration
		tmdbRuntime  int
		wantScore    int
		description  string
	}{
		{
			name:         "exact match 1000+10",
			result:       MovieResult{Popularity: 50},
			discDuration: 93 * time.Minute,
			tmdbRuntime:  93,
			wantScore:    1010, // 1000 (exact) + 10 (popularity)
			description:  "0 minute difference",
		},
		{
			name:         "1 minute diff",
			result:       MovieResult{Popularity: 50},
			discDuration: 93 * time.Minute,
			tmdbRuntime:  94,
			wantScore:    1000, // 990 (1 min) + 10 (popularity)
			description:  "1 minute difference",
		},
		{
			name:         "8 minute diff",
			result:       MovieResult{Popularity: 50},
			discDuration: 93 * time.Minute,
			tmdbRuntime:  101,
			wantScore:    930, // 920 (8 min) + 10 (popularity)
			description:  "8 minute difference",
		},
		{
			name:         "10 minute diff",
			result:       MovieResult{Popularity: 50},
			discDuration: 93 * time.Minute,
			tmdbRuntime:  103,
			wantScore:    910, // 900 (10 min) + 10 (popularity)
			description:  "10 minute difference",
		},
		{
			name:         "20 minute diff",
			result:       MovieResult{Popularity: 50},
			discDuration: 93 * time.Minute,
			tmdbRuntime:  113,
			wantScore:    810, // 800 (20 min) + 10 (popularity)
			description:  "20 minute difference",
		},
		{
			name:         "large diff",
			result:       MovieResult{Popularity: 50},
			discDuration: 93 * time.Minute,
			tmdbRuntime:  150,
			wantScore:    440, // 430 (57 min) + 10 (popularity)
			description:  "57 minute difference",
		},
		{
			name:         "very large diff floors at 0",
			result:       MovieResult{Popularity: 50},
			discDuration: 93 * time.Minute,
			tmdbRuntime:  250,
			wantScore:    10, // 0 (157 min > 100) + 10 (popularity)
			description:  "difference exceeds 100 minutes",
		},
		{
			name:         "no tmdb runtime",
			result:       MovieResult{Popularity: 50},
			discDuration: 93 * time.Minute,
			tmdbRuntime:  0,
			wantScore:    10, // 0 (no runtime) + 10 (popularity)
			description:  "TMDB runtime unavailable",
		},
		{
			name:         "no disc duration",
			result:       MovieResult{Popularity: 50},
			discDuration: 0,
			tmdbRuntime:  93,
			wantScore:    10, // 0 (no duration) + 10 (popularity)
			description:  "disc duration unavailable",
		},
		{
			name:         "high popularity caps at 20",
			result:       MovieResult{Popularity: 200},
			discDuration: 0,
			tmdbRuntime:  0,
			wantScore:    20, // popularity capped at 20
			description:  "popularity > 100 normalizes to 20",
		},
		{
			name:         "low popularity",
			result:       MovieResult{Popularity: 5},
			discDuration: 0,
			tmdbRuntime:  0,
			wantScore:    1, // 5/5 = 1
			description:  "low popularity scores low",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreResult(tc.result, tc.discDuration, tc.tmdbRuntime)
			if got != tc.wantScore {
				t.Errorf("ScoreResult() = %d, want %d (%s)", got, tc.wantScore, tc.description)
			}
		})
	}
}

func TestBestMatch(t *testing.T) {
	tests := []struct {
		name              string
		results           []MovieResult
		discDuration      time.Duration
		tmdbResponses     []string // JSON responses for each result's GetMovie call
		wantTitle         string
		wantRuntimeWinner bool
		wantLogSubstring  string
		wantErr           bool
	}{
		{
			name: "runtime match wins over popularity",
			results: []MovieResult{
				{ID: 1, Title: "Popular Movie", ReleaseDate: "2020-01-01", Popularity: 100},
				{ID: 2, Title: "Runtime Match", ReleaseDate: "2015-01-01", Popularity: 50},
			},
			discDuration: 93 * time.Minute,
			tmdbResponses: []string{
				`{"id":1,"title":"Popular Movie","release_date":"2020-01-01","runtime":140,"imdb_id":"tt0000001"}`,
				`{"id":2,"title":"Runtime Match","release_date":"2015-01-01","runtime":93,"imdb_id":"tt0000002"}`,
			},
			wantTitle:         "Runtime Match",
			wantRuntimeWinner: true,
			wantLogSubstring:  "runtime match: Runtime Match (2015) 93min — overrides top result: Popular Movie (2020) 140min",
		},
		{
			name: "no runtime falls back to popularity",
			results: []MovieResult{
				{ID: 1, Title: "Most Popular", ReleaseDate: "2020-01-01", Popularity: 100},
				{ID: 2, Title: "Less Popular", ReleaseDate: "2015-01-01", Popularity: 50},
			},
			discDuration: 0, // no duration
			tmdbResponses: []string{
				`{"id":1,"title":"Most Popular","release_date":"2020-01-01","runtime":120,"imdb_id":"tt0000001"}`,
				`{"id":2,"title":"Less Popular","release_date":"2015-01-01","runtime":93,"imdb_id":"tt0000002"}`,
			},
			wantTitle:         "Most Popular",
			wantRuntimeWinner: false,
			wantLogSubstring:  "",
		},
		{
			name: "popularity winner also has best runtime",
			results: []MovieResult{
				{ID: 1, Title: "Best Match", ReleaseDate: "2020-01-01", Popularity: 100},
				{ID: 2, Title: "Worse Match", ReleaseDate: "2015-01-01", Popularity: 50},
			},
			discDuration: 93 * time.Minute,
			tmdbResponses: []string{
				`{"id":1,"title":"Best Match","release_date":"2020-01-01","runtime":93,"imdb_id":"tt0000001"}`,
				`{"id":2,"title":"Worse Match","release_date":"2015-01-01","runtime":150,"imdb_id":"tt0000002"}`,
			},
			wantTitle:         "Best Match",
			wantRuntimeWinner: false, // no override because first result wins
			wantLogSubstring:  "",
		},
		{
			name: "TMNT: exact match beats close match despite higher popularity",
			results: []MovieResult{
				{ID: 1, Title: "Teenage Mutant Ninja Turtles", ReleaseDate: "2014-08-08", Popularity: 100},
				{ID: 2, Title: "Teenage Mutant Ninja Turtles", ReleaseDate: "1990-03-30", Popularity: 50},
			},
			discDuration: 93 * time.Minute,
			tmdbResponses: []string{
				`{"id":1,"title":"Teenage Mutant Ninja Turtles","release_date":"2014-08-08","runtime":101,"imdb_id":"tt1291150"}`,
				`{"id":2,"title":"Teenage Mutant Ninja Turtles","release_date":"1990-03-30","runtime":93,"imdb_id":"tt0100758"}`,
			},
			wantTitle:         "Teenage Mutant Ninja Turtles",
			wantRuntimeWinner: true,
			wantLogSubstring:  "1990) 93min — overrides top result",
		},
		{
			name:             "no results returns error",
			results:          []MovieResult{},
			discDuration:     93 * time.Minute,
			tmdbResponses:    []string{},
			wantErr:          true,
			wantLogSubstring: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create a test server that returns different responses based on ID
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Extract movie ID from URL
				path := r.URL.Path
				if strings.HasPrefix(path, "/3/movie/") {
					idStr := strings.TrimPrefix(path, "/3/movie/")
					idStr = strings.Split(idStr, "?")[0]
					for i, result := range tc.results {
						if fmt.Sprintf("%d", result.ID) == idStr && i < len(tc.tmdbResponses) {
							w.Header().Set("Content-Type", "application/json")
							fmt.Fprint(w, tc.tmdbResponses[i])
							return
						}
					}
				}
				http.NotFound(w, r)
			}))
			defer server.Close()

			client := NewClient("test-key")
			client.httpClient = &http.Client{
				Transport: rewriteTransport{
					base:       mustParseURL(t, server.URL),
					targetHost: "api.themoviedb.org",
					rt:         server.Client().Transport,
				},
			}

			chosen, runtimeWinner, logMsg, err := BestMatch(context.Background(), client, tc.results, tc.discDuration)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("BestMatch() error = %v", err)
			}

			if chosen.Title != tc.wantTitle {
				t.Errorf("BestMatch() chose %q, want %q", chosen.Title, tc.wantTitle)
			}
			if runtimeWinner != tc.wantRuntimeWinner {
				t.Errorf("BestMatch() runtimeWinner = %v, want %v", runtimeWinner, tc.wantRuntimeWinner)
			}
			if tc.wantLogSubstring != "" && !strings.Contains(logMsg, tc.wantLogSubstring) {
				t.Errorf("BestMatch() logMsg = %q, want substring %q", logMsg, tc.wantLogSubstring)
			}
			if tc.wantLogSubstring == "" && logMsg != "" {
				t.Errorf("BestMatch() logMsg = %q, want empty", logMsg)
			}
		})
	}
}

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
