package metadata

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTMDBTestClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	c := NewClient("tmdb-key")
	base := mustParseURL(t, server.URL)
	c.httpClient = &http.Client{
		Transport: rewriteTransport{
			base:       base,
			targetHost: "api.themoviedb.org",
			rt:         server.Client().Transport,
		},
	}
	return c
}

func TestMovieResultYearAndFolderName(t *testing.T) {
	tests := []struct {
		name       string
		movie      MovieResult
		wantYear   string
		wantFolder string
	}{
		{
			name:       "has release date",
			movie:      MovieResult{Title: "A/B: C", ReleaseDate: "2001-10-09"},
			wantYear:   "2001",
			wantFolder: "A-B - C (2001)",
		},
		{
			name:       "missing release date",
			movie:      MovieResult{Title: "A/B: C"},
			wantYear:   "",
			wantFolder: "A-B - C",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.movie.Year(); got != tc.wantYear {
				t.Fatalf("Year() = %q, want %q", got, tc.wantYear)
			}
			if got := tc.movie.FolderName(); got != tc.wantFolder {
				t.Fatalf("FolderName() = %q, want %q", got, tc.wantFolder)
			}
		})
	}
}

func TestQueryFromDirName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "multiple separators", in: "Star-Wars--Episode-I---The-Phantom-Menace", want: "Star Wars Episode I The Phantom Menace"},
		{name: "strips year suffix", in: "Pitch-Black (2000)", want: "Pitch Black"},
		{name: "splits letter digit boundary", in: "HIGH_SCHOOL_MUSICAL2", want: "HIGH SCHOOL MUSICAL 2"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := QueryFromDirName(tc.in); got != tc.want {
				t.Fatalf("QueryFromDirName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestClientSearchMovieTable(t *testing.T) {
	tests := []struct {
		name        string
		query       string
		statusCode  int
		response    string
		wantLen     int
		wantErrPart string
	}{
		{
			name:  "success truncates to five",
			query: "pitch black",
			response: `{"results":[
				{"id":1,"title":"A","release_date":"2000-01-01"},
				{"id":2,"title":"B","release_date":"2001-01-01"},
				{"id":3,"title":"C","release_date":"2002-01-01"},
				{"id":4,"title":"D","release_date":"2003-01-01"},
				{"id":5,"title":"E","release_date":"2004-01-01"},
				{"id":6,"title":"F","release_date":"2005-01-01"}
			]}`,
			wantLen: 5,
		},
		{
			name:        "non-200 status",
			query:       "x",
			statusCode:  http.StatusBadGateway,
			wantErrPart: "tmdb returned 502",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/3/search/movie" {
					t.Fatalf("path = %q, want %q", r.URL.Path, "/3/search/movie")
				}
				if got := r.URL.Query().Get("api_key"); got != "tmdb-key" {
					t.Fatalf("api_key = %q, want %q", got, "tmdb-key")
				}
				if got := r.URL.Query().Get("query"); got != tc.query {
					t.Fatalf("query = %q, want %q", got, tc.query)
				}
				if got := r.URL.Query().Get("language"); got != "en-US" {
					t.Fatalf("language = %q, want %q", got, "en-US")
				}

				if tc.statusCode != 0 {
					w.WriteHeader(tc.statusCode)
					return
				}
				_, _ = fmt.Fprint(w, tc.response)
			}))
			defer server.Close()

			c := newTMDBTestClient(t, server)

			results, err := c.SearchMovie(context.Background(), tc.query)
			if tc.wantErrPart != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Fatalf("SearchMovie() error = %v, want substring %q", err, tc.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("SearchMovie() error = %v", err)
			}
			if len(results) != tc.wantLen {
				t.Fatalf("SearchMovie() len = %d, want %d", len(results), tc.wantLen)
			}
		})
	}
}

func TestClientGetMovieTable(t *testing.T) {
	tests := []struct {
		name        string
		id          int
		statusCode  int
		response    string
		want        *TMDbMovieDetail
		wantErrPart string
	}{
		{
			name:     "success",
			id:       42,
			response: `{"id":42,"title":"Pitch Black","runtime":109,"imdb_id":"tt0134847"}`,
			want:     &TMDbMovieDetail{ID: 42, Title: "Pitch Black", Runtime: 109, ImdbID: "tt0134847"},
		},
		{
			name:        "non-200 status",
			id:          99,
			statusCode:  http.StatusBadGateway,
			wantErrPart: "tmdb returned 502",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wantPath := fmt.Sprintf("/3/movie/%d", tc.id)
				if r.URL.Path != wantPath {
					t.Fatalf("path = %q, want %q", r.URL.Path, wantPath)
				}
				if got := r.URL.Query().Get("api_key"); got != "tmdb-key" {
					t.Fatalf("api_key = %q, want %q", got, "tmdb-key")
				}

				if tc.statusCode != 0 {
					w.WriteHeader(tc.statusCode)
					return
				}
				_, _ = fmt.Fprint(w, tc.response)
			}))
			defer server.Close()

			c := newTMDBTestClient(t, server)

			got, err := c.GetMovie(context.Background(), tc.id)
			if tc.wantErrPart != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Fatalf("GetMovie() error = %v, want substring %q", err, tc.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetMovie() error = %v", err)
			}
			if got.ID != tc.want.ID || got.Runtime != tc.want.Runtime || got.ImdbID != tc.want.ImdbID || got.Title != tc.want.Title {
				t.Fatalf("GetMovie() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
