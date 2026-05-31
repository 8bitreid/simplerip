// Package metadata provides TMDB title lookups.
package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const tmdbBase = "https://api.themoviedb.org/3"

// MovieResult is a single TMDB search result.
type MovieResult struct {
	ID          int     `json:"id"`
	Title       string  `json:"title"`
	ReleaseDate string  `json:"release_date"` // "YYYY-MM-DD"
	Overview    string  `json:"overview"`
	Popularity  float64 `json:"popularity"`
}

// Year returns the four-digit release year, or "" if unavailable.
func (m MovieResult) Year() string {
	if len(m.ReleaseDate) >= 4 {
		return m.ReleaseDate[:4]
	}
	return ""
}

// FolderName returns "Title (Year)" for use as a directory name.
func (m MovieResult) FolderName() string {
	year := m.Year()
	if year == "" {
		return sanitize(m.Title)
	}
	return fmt.Sprintf("%s (%s)", sanitize(m.Title), year)
}

// Client performs TMDB API v3 requests.
type Client struct {
	apiKey     string
	httpClient *http.Client
}

// NewClient returns a Client using apiKey.
func NewClient(apiKey string) *Client {
	return NewClientWithHTTPClient(apiKey, &http.Client{Timeout: 10 * time.Second})
}

// NewClientWithHTTPClient returns a Client using apiKey and httpClient.
func NewClientWithHTTPClient(apiKey string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{
		apiKey:     apiKey,
		httpClient: httpClient,
	}
}

// SearchMovie searches TMDB for movies matching query.
// Returns up to 5 results ordered by TMDB relevance.
func (c *Client) SearchMovie(ctx context.Context, query string) ([]MovieResult, error) {
	u, _ := url.Parse(tmdbBase + "/search/movie")
	q := u.Query()
	q.Set("api_key", c.apiKey)
	q.Set("query", query)
	q.Set("language", "en-US")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tmdb search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tmdb returned %d", resp.StatusCode)
	}

	var body struct {
		Results []MovieResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode tmdb response: %w", err)
	}

	if len(body.Results) > 5 {
		return body.Results[:5], nil
	}
	return body.Results, nil
}

// TMDbMovieDetail is the full movie record from /movie/{id}.
type TMDbMovieDetail struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	ReleaseDate string `json:"release_date"`
	Runtime     int    `json:"runtime"` // minutes
	ImdbID      string `json:"imdb_id"`
	Overview    string `json:"overview"`
}

// GetMovie fetches full details for a TMDB movie ID.
func (c *Client) GetMovie(ctx context.Context, id int) (*TMDbMovieDetail, error) {
	u := fmt.Sprintf("%s/movie/%d?api_key=%s", tmdbBase, id, c.apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tmdb get movie: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tmdb returned %d", resp.StatusCode)
	}
	var detail TMDbMovieDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return nil, fmt.Errorf("decode tmdb movie: %w", err)
	}
	return &detail, nil
}

// QueryFromDirName converts a raw directory name like
// "Star-Wars--Episode-I---The-Phantom-Menace", "Pitch-Black (2000)",
// or "HIGH_SCHOOL_MUSICAL2" into a clean TMDB search query.
func QueryFromDirName(dir string) string {
	// Strip parenthesized year suffix e.g. " (2000)".
	if idx := strings.LastIndex(dir, " ("); idx != -1 {
		dir = dir[:idx]
	}
	// Replace dashes and underscores with spaces.
	r := strings.NewReplacer("-", " ", "_", " ")
	s := r.Replace(dir)
	// Insert a space between a letter and a digit boundary: "MUSICAL2" → "MUSICAL 2".
	var out strings.Builder
	runes := []rune(s)
	for i, ch := range runes {
		if i > 0 {
			prev := runes[i-1]
			letterToDigit := isLetter(prev) && isDigit(ch)
			digitToLetter := isDigit(prev) && isLetter(ch)
			if letterToDigit || digitToLetter {
				out.WriteRune(' ')
			}
		}
		out.WriteRune(ch)
	}
	fields := strings.Fields(out.String())
	return strings.Join(fields, " ")
}

func isLetter(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') }
func isDigit(r rune) bool  { return r >= '0' && r <= '9' }

// sanitize strips characters that are illegal in Linux/macOS filenames.
func sanitize(s string) string {
	r := strings.NewReplacer(
		"/", "-",
		":", " -",
		"\\", "-",
		"?", "",
		"*", "",
		"\"", "",
		"<", "",
		">", "",
		"|", "",
	)
	return strings.TrimSpace(r.Replace(s))
}
