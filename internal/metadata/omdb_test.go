package metadata

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newOMDbTestClient(t *testing.T, server *httptest.Server) *OMDbClient {
	t.Helper()
	c := NewOMDbClient("omdb-key")
	base := mustParseURL(t, server.URL)
	c.httpClient = &http.Client{
		Transport: rewriteTransport{
			base:       base,
			targetHost: "www.omdbapi.com",
			rt:         server.Client().Transport,
		},
	}
	return c
}

func TestOMDbResultRuntimeMinutes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{name: "valid", in: "109 min", want: 109},
		{name: "whitespace", in: " 88 min ", want: 88},
		{name: "invalid", in: "n/a", want: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := OMDbResult{Runtime: tc.in}.RuntimeMinutes()
			if got != tc.want {
				t.Fatalf("RuntimeMinutes() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestOMDbResultRottenTomatoes(t *testing.T) {
	tests := []struct {
		name    string
		ratings []OMDbRating
		want    string
	}{
		{
			name: "has rotten tomatoes rating",
			ratings: []OMDbRating{
				{Source: "Internet Movie Database", Value: "6.8/10"},
				{Source: "Rotten Tomatoes", Value: "58%"},
			},
			want: "58%",
		},
		{
			name: "no rotten tomatoes rating",
			ratings: []OMDbRating{
				{Source: "Internet Movie Database", Value: "6.8/10"},
			},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := OMDbResult{Ratings: tc.ratings}
			if got := o.RottenTomatoes(); got != tc.want {
				t.Fatalf("RottenTomatoes() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOMDbClientGetByIMDbIDTable(t *testing.T) {
	tests := []struct {
		name        string
		imdbID      string
		response    string
		wantTitle   string
		wantErrPart string
	}{
		{
			name:      "success",
			imdbID:    "tt0134847",
			response:  `{"Title":"Pitch Black","Runtime":"109 min","Response":"True"}`,
			wantTitle: "Pitch Black",
		},
		{
			name:        "false response error",
			imdbID:      "tt0000000",
			response:    `{"Response":"False","Error":"Movie not found!"}`,
			wantErrPart: "omdb: Movie not found!",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.URL.Query().Get("apikey"); got != "omdb-key" {
					t.Fatalf("apikey query = %q, want %q", got, "omdb-key")
				}
				if got := r.URL.Query().Get("i"); got != tc.imdbID {
					t.Fatalf("imdb id query = %q, want %q", got, tc.imdbID)
				}
				if got := r.URL.Query().Get("plot"); got != "short" {
					t.Fatalf("plot query = %q, want %q", got, "short")
				}
				_, _ = fmt.Fprint(w, tc.response)
			}))
			defer server.Close()

			c := newOMDbTestClient(t, server)

			got, err := c.GetByIMDbID(context.Background(), tc.imdbID)
			if tc.wantErrPart != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Fatalf("GetByIMDbID() error = %v, want substring %q", err, tc.wantErrPart)
				}
				return
			}

			if err != nil {
				t.Fatalf("GetByIMDbID() error = %v", err)
			}
			if got.Title != tc.wantTitle {
				t.Fatalf("Title = %q, want %q", got.Title, tc.wantTitle)
			}
		})
	}
}
