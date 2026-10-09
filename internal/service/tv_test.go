package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/8bitreid/simplerip/internal/config"
	"github.com/8bitreid/simplerip/internal/disc"
	"github.com/8bitreid/simplerip/internal/metadata"
	"github.com/8bitreid/simplerip/internal/store"
)

// fakeTVShow is one series served by fakeTMDBTV: its search entry and the
// runtimes of each numbered season.
type fakeTVShow struct {
	id        int
	name      string
	firstAir  string
	votes     int
	seasons   [][]int // seasons[i] holds season i+1's episode runtimes
	inSearch  bool
	failFetch bool
}

// spongebobShows is TMDB's real search for "spongebob" (Oct 2026), trimmed
// to the first seasons of each show.
func spongebobShows() []*fakeTVShow {
	squarePants := make([][]int, 3)
	for i := range squarePants {
		squarePants[i] = append([]int{9, 3}, repeatMinutes(11, 37)...)
		squarePants[i] = append(squarePants[i], 12, 12)
	}
	return []*fakeTVShow{
		{id: 121021, name: "SpongeBob DocuPants", firstAir: "2020-01-01", votes: 26, inSearch: true,
			seasons: [][]int{{14, 13, 12, 18, 14, 14, 11, 12}}},
		{id: 238145, name: "SpongeBob: Reimagined", firstAir: "2021-01-01", votes: 2, inSearch: true,
			seasons: [][]int{{3, 5, 5, 5, 5, 5, 4, 5, 3, 5, 4, 4, 3, 3}, {2}}},
		{id: 387, name: "SpongeBob SquarePants", firstAir: "1999-05-01", votes: 3281, inSearch: true,
			seasons: squarePants},
		{id: 205996, name: "SpongeBob As Told By", firstAir: "2020-01-01", votes: 0, inSearch: true,
			seasons: [][]int{{6, 6, 7, 6, 7, 6, 6, 6}}},
		{id: 99588, name: "Kamp Koral: SpongeBob's Under Years", firstAir: "2021-03-04", votes: 523, inSearch: true,
			seasons: [][]int{repeatMinutes(11, 50), repeatMinutes(11, 25)}},
	}
}

func repeatMinutes(runtime, count int) []int {
	runtimes := make([]int, count)
	for i := range runtimes {
		runtimes[i] = runtime
	}
	return runtimes
}

// fakeTMDBTV serves search, details and bundled seasons for the given shows
// and records which shows had details fetched.
func fakeTMDBTV(t *testing.T, shows []*fakeTVShow) *sync.Map {
	t.Helper()
	fetched := &sync.Map{}
	byID := make(map[int]*fakeTVShow, len(shows))
	for _, show := range shows {
		byID[show.id] = show
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/3/search/tv" {
			var results []map[string]any
			for _, show := range shows {
				if show.inSearch {
					results = append(results, map[string]any{
						"id": show.id, "name": show.name, "first_air_date": show.firstAir, "vote_count": show.votes,
					})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
			return
		}
		var id int
		if _, err := fmt.Sscanf(r.URL.Path, "/3/tv/%d", &id); err != nil || byID[id] == nil {
			http.NotFound(w, r)
			return
		}
		show := byID[id]
		if show.failFetch {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		fetched.Store(id, true)
		seasonList := make([]map[string]int, 0, len(show.seasons))
		for n := range show.seasons {
			seasonList = append(seasonList, map[string]int{"season_number": n + 1})
		}
		body := map[string]any{"id": show.id, "name": show.name, "first_air_date": show.firstAir, "seasons": seasonList}
		if appended := r.URL.Query().Get("append_to_response"); appended != "" {
			for _, key := range strings.Split(appended, ",") {
				var n int
				_, _ = fmt.Sscanf(key, "season/%d", &n)
				if n < 1 || n > len(show.seasons) {
					continue
				}
				episodes := make([]map[string]int, len(show.seasons[n-1]))
				for i, runtime := range show.seasons[n-1] {
					episodes[i] = map[string]int{"episode_number": i + 1, "runtime": runtime}
				}
				body[key] = map[string]any{"season_number": n, "episodes": episodes}
			}
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	installHostRewrites(t, map[string]string{"api.themoviedb.org": server.URL})
	return fetched
}

func spongebobDisc() []disc.MKVTitle {
	seconds := []int{704, 663, 662, 663, 663, 663, 662, 663, 662, 662, 662, 692, 673, 694, 664}
	titles := make([]disc.MKVTitle, len(seconds))
	for i, s := range seconds {
		titles[i] = disc.MKVTitle{Index: i, Duration: time.Duration(s) * time.Second}
	}
	return titles
}

func tmdbService() *RipService {
	cfg := config.Defaults()
	cfg.Metadata.TMDBApiKey = "tmdb-key"
	return New(cfg, nil)
}

func TestIdentifyTVSpongeBobMatchesShowFromRuntimeEvidence(t *testing.T) {
	fetched := fakeTMDBTV(t, spongebobShows())

	result := tmdbService().identifyTV(context.Background(), "SPONGEBOB_DISC2", spongebobDisc(), nil)
	if !result.ShowCertain || result.Show == nil || result.Show.ID != 387 {
		t.Fatalf("show = %+v certain=%v why=%q", result.Show, result.ShowCertain, result.ShowWhy)
	}
	if !strings.Contains(result.ShowWhy, "ruled out by episode runtimes") {
		t.Fatalf("reason should record the eliminated candidates: %q", result.ShowWhy)
	}
	fits := map[int]metadata.RuntimeFit{}
	for _, candidate := range result.Candidates {
		fits[candidate.Media.ID] = candidate.Fit
	}
	if fits[387] != metadata.RuntimeFitMatch || fits[121021] != metadata.RuntimeFitMismatch || fits[238145] != metadata.RuntimeFitMismatch {
		t.Fatalf("runtime fits = %v", fits)
	}
	if _, ok := fetched.Load(99588); ok {
		t.Fatal("a candidate with an unrelated name should not have its episodes fetched")
	}
	// Every SquarePants season is 11–12 minute segments, so runtimes cannot
	// pick the season or the episode order.
	if result.Season != 0 || len(result.Episodes) != 0 || result.SuggestedSeason != 0 {
		t.Fatalf("season/episodes must stay unresolved: %+v", result)
	}
}

func TestIdentifyTVEarlierDiscSuggestsSeason(t *testing.T) {
	fakeTMDBTV(t, spongebobShows())
	sibling := &tvSibling{TMDBID: 387, Title: "SpongeBob SquarePants", Year: "1999", Season: 1, Label: "SPONGEBOB_DISC1"}

	result := tmdbService().identifyTV(context.Background(), "SPONGEBOB_DISC2", spongebobDisc(), sibling)
	if !result.ShowCertain || result.Show == nil || result.Show.ID != 387 {
		t.Fatalf("show = %+v certain=%v", result.Show, result.ShowCertain)
	}
	if result.Season != 0 || result.SuggestedSeason != 1 {
		t.Fatalf("season = %d suggested = %d; the earlier disc's season is only a suggestion", result.Season, result.SuggestedSeason)
	}
	data := tvIdentificationEventData(result)
	if data["suggested_season"] != 1 || data["season_confidence"] != "suggestion" || data["earlier_disc_label"] != "SPONGEBOB_DISC1" {
		t.Fatalf("event data = %v", data)
	}
}

func TestIdentifyTVEarlierDiscShowMissingFromSearch(t *testing.T) {
	shows := spongebobShows()
	for _, show := range shows {
		show.inSearch = show.id != 387
	}
	fetched := fakeTMDBTV(t, shows)
	sibling := &tvSibling{TMDBID: 387, Title: "SpongeBob SquarePants", Year: "1999", Label: "SPONGEBOB_DISC1"}

	result := tmdbService().identifyTV(context.Background(), "SPONGEBOB_DISC2", spongebobDisc(), sibling)
	if !result.ShowCertain || result.Show == nil || result.Show.ID != 387 {
		t.Fatalf("show = %+v certain=%v", result.Show, result.ShowCertain)
	}
	if _, ok := fetched.Load(387); !ok {
		t.Fatal("the earlier disc's show should have its episodes checked")
	}
}

func TestIdentifyTVEarlierDiscIgnoredWhenRuntimesContradict(t *testing.T) {
	fakeTMDBTV(t, spongebobShows())
	// DocuPants has 8 episodes; it cannot be this 15-title disc.
	sibling := &tvSibling{TMDBID: 121021, Title: "SpongeBob DocuPants", Label: "SPONGEBOB_DISC1"}

	result := tmdbService().identifyTV(context.Background(), "SPONGEBOB_DISC2", spongebobDisc(), sibling)
	if result.Show == nil || result.Show.ID != 387 {
		t.Fatalf("show = %+v; a contradicted earlier identity must not win", result.Show)
	}
}

func TestIdentifyTVPrefixMatchNeedsRuntimeConfirmation(t *testing.T) {
	shows := []*fakeTVShow{{id: 387, name: "SpongeBob SquarePants", firstAir: "1999-05-01", votes: 3281, inSearch: true,
		seasons: [][]int{repeatMinutes(0, 40)}}}
	fakeTMDBTV(t, shows)

	result := tmdbService().identifyTV(context.Background(), "SPONGEBOB_DISC2", spongebobDisc(), nil)
	if result.ShowCertain {
		t.Fatalf("a label matching only the title's first word needs runtime support: %+v", result)
	}
	if result.Show == nil || result.Show.ID != 387 {
		t.Fatalf("the candidate should still be offered as a suggestion: %+v", result.Show)
	}
}

func TestIdentifyTVExactNameStillMatchesWithoutRuntimes(t *testing.T) {
	shows := []*fakeTVShow{{id: 1668, name: "Friends", firstAir: "1994-09-22", votes: 9000, inSearch: true, failFetch: true}}
	fakeTMDBTV(t, shows)

	result := tmdbService().identifyTV(context.Background(), "FRIENDS_DISC1", spongebobDisc()[:3], nil)
	if !result.ShowCertain || result.Show == nil || result.Show.ID != 1668 {
		t.Fatalf("exact title match = %+v certain=%v", result.Show, result.ShowCertain)
	}
	if result.LookupError == "" || result.Season != 0 {
		t.Fatalf("failed detail fetch should leave season unresolved with an error: %+v", result)
	}
}

func identityRecord(jobID, label string, data map[string]any) store.IdentityRecord {
	raw, _ := json.Marshal(data)
	return store.IdentityRecord{JobID: jobID, DiscLabel: label, Data: raw}
}

func TestSiblingTVIdentity(t *testing.T) {
	correction := map[string]any{"correction": true, "media_type": "tv", "tmdb_id": 387,
		"title": "SpongeBob SquarePants", "year": 1999, "season": 1, "episode_start": 0}
	auto := map[string]any{"action": "tv_identification", "show_confidence": "high",
		"suggested_tmdb_id": 387, "suggested_title": "SpongeBob SquarePants", "suggested_year": "1999"}

	got := siblingTVIdentity("SPONGEBOB_DISK2", []store.IdentityRecord{
		identityRecord("job-b", "SPONGEBOB_DISC3", auto),
		identityRecord("job-a", "SPONGEBOB_DISC1", correction),
		identityRecord("job-x", "FLIPPER", map[string]any{"correction": true, "media_type": "movie", "tmdb_id": 5, "title": "Flipper"}),
	})
	if got == nil || got.TMDBID != 387 || got.Year != "1999" || got.Season != 1 {
		t.Fatalf("sibling = %+v", got)
	}

	// The latest decision per job wins: a later correction to a movie means
	// that job no longer counts as TV evidence.
	got = siblingTVIdentity("SPONGEBOB_DISC2", []store.IdentityRecord{
		identityRecord("job-a", "SPONGEBOB_DISC1", map[string]any{"correction": true, "media_type": "movie", "tmdb_id": 9, "title": "The Movie"}),
		identityRecord("job-a", "SPONGEBOB_DISC1", correction),
	})
	if got != nil {
		t.Fatalf("superseded TV correction should not count: %+v", got)
	}

	conflicting := map[string]any{"correction": true, "media_type": "tv", "tmdb_id": 99588, "title": "Kamp Koral"}
	if got := siblingTVIdentity("SPONGEBOB_DISC2", []store.IdentityRecord{
		identityRecord("job-a", "SPONGEBOB_DISC1", correction),
		identityRecord("job-c", "SPONGEBOB_DISC4", conflicting),
	}); got != nil {
		t.Fatalf("disagreeing earlier discs must cancel out: %+v", got)
	}

	if got := siblingTVIdentity("DVD_VIDEO", []store.IdentityRecord{identityRecord("job-a", "DVD_VIDEO", correction)}); got != nil {
		t.Fatalf("generic labels must not link discs: %+v", got)
	}
}
