package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const omdbBase = "http://www.omdbapi.com/"

// OMDbResult is the response from OMDb for a single movie.
type OMDbResult struct {
	Title      string       `json:"Title"`
	Year       string       `json:"Year"`
	Runtime    string       `json:"Runtime"` // e.g. "109 min"
	Director   string       `json:"Director"`
	Actors     string       `json:"Actors"`
	Genre      string       `json:"Genre"`
	ImdbID     string       `json:"imdbID"`
	ImdbRating string       `json:"imdbRating"`
	Ratings    []OMDbRating `json:"Ratings"`
	Response   string       `json:"Response"`
	Error      string       `json:"Error"`
}

// OMDbRating is one entry from the Ratings array.
type OMDbRating struct {
	Source string `json:"Source"`
	Value  string `json:"Value"`
}

// RuntimeMinutes parses the "109 min" string into an integer.
// Returns 0 if unparseable.
func (o OMDbResult) RuntimeMinutes() int {
	s := strings.TrimSuffix(strings.TrimSpace(o.Runtime), " min")
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// RottenTomatoes returns the RT score string (e.g. "58%") or "".
func (o OMDbResult) RottenTomatoes() string {
	for _, r := range o.Ratings {
		if r.Source == "Rotten Tomatoes" {
			return r.Value
		}
	}
	return ""
}

// OMDbClient performs OMDb API requests.
type OMDbClient struct {
	apiKey     string
	httpClient *http.Client
}

// NewOMDbClient returns a client using apiKey.
func NewOMDbClient(apiKey string) *OMDbClient {
	return NewOMDbClientWithHTTPClient(apiKey, &http.Client{Timeout: 10 * time.Second})
}

// NewOMDbClientWithHTTPClient returns a client using apiKey and httpClient.
func NewOMDbClientWithHTTPClient(apiKey string, httpClient *http.Client) *OMDbClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &OMDbClient{
		apiKey:     apiKey,
		httpClient: httpClient,
	}
}

// GetByIMDbID fetches full movie details using an IMDb ID (e.g. "tt0134847").
// This is preferred over title search — unambiguous.
func (c *OMDbClient) GetByIMDbID(ctx context.Context, imdbID string) (*OMDbResult, error) {
	u, _ := url.Parse(omdbBase)
	q := u.Query()
	q.Set("apikey", c.apiKey)
	q.Set("i", imdbID)
	q.Set("plot", "short")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("omdb request: %w", err)
	}
	defer resp.Body.Close()

	var result OMDbResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode omdb response: %w", err)
	}
	if result.Response == "False" {
		return nil, fmt.Errorf("omdb: %s", result.Error)
	}
	return &result, nil
}
