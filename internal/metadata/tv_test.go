package metadata

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/8bitreid/simplerip/internal/disc"
)

func TestNormalizeTVQuery(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"The Office-DISC1", "the office"},
		{"The Office Volume 2", "the office"},
		{"Breakng-Bad_DVD-1", "breaking bad"},
		{"The Simpons (Disc 1)", "the simpsons"},
		{"Friends (2000)", "friends"},
	}
	for _, test := range tests {
		t.Run(test.in, func(t *testing.T) {
			if got := NormalizeTVQuery(test.in); got != test.want {
				t.Fatalf("NormalizeTVQuery(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
	queries := TVSearchQueries("Breakng Bad Disc 1")
	if len(queries) != 2 || queries[0] != "breaking bad" || queries[1] != "breakng bad" {
		t.Fatalf("TVSearchQueries() = %v", queries)
	}
}

func TestRankTVCandidatesAndConfidence(t *testing.T) {
	results := []MediaSearchResult{
		{ID: 1, Title: "The Office", MediaType: "tv"},
		{ID: 2, Title: "The Office", MediaType: "tv"},
		{ID: 3, Title: "The Office Movie", MediaType: "movie"},
	}
	ranked := RankTVCandidates("The Office Disc 1", results)
	if len(ranked) != 2 || ranked[0].Media.ID != 1 {
		t.Fatalf("ranked candidates = %+v", ranked)
	}
	if ranked[0].MatchedQuery != "the office" {
		t.Fatalf("matched query = %q, want normalized series label", ranked[0].MatchedQuery)
	}
	if !ConfidentTVMatch(ranked[:1]) {
		t.Fatalf("exact single candidate should be confident: %+v", ranked[:1])
	}
	if ConfidentTVMatch(ranked) {
		t.Fatalf("near-tied candidates must remain ambiguous: %+v", ranked)
	}
	if ConfidentTVMatch(RankTVCandidates("Unrelated mystery", results)) {
		t.Fatal("unrelated candidate must not be selected")
	}
}

func TestMatchSeasonByRuntime(t *testing.T) {
	titles := []disc.MKVTitle{
		{Index: 2, Duration: 20*time.Minute + 4*time.Second},
		{Index: 4, Duration: 22*time.Minute + 32*time.Second},
		{Index: 7, Duration: 24*time.Minute + 1*time.Second},
	}
	seasons := []TVSeason{
		{SeasonNumber: 1, Episodes: []TVEpisode{
			{EpisodeNumber: 1, Runtime: 20}, {EpisodeNumber: 2, Runtime: 22}, {EpisodeNumber: 3, Runtime: 24},
		}},
		{SeasonNumber: 2, Episodes: []TVEpisode{
			{EpisodeNumber: 1, Runtime: 42}, {EpisodeNumber: 2, Runtime: 43}, {EpisodeNumber: 3, Runtime: 42},
		}},
	}
	season, ok, mapping := MatchSeasonByRuntime(titles, seasons)
	if !ok || season != 1 {
		t.Fatalf("MatchSeasonByRuntime() = (%d, %v, %v), want season 1", season, ok, mapping)
	}
	if len(mapping) != 3 || mapping[2] != 1 || mapping[4] != 2 || mapping[7] != 3 {
		t.Fatalf("episode mapping = %v", mapping)
	}

	ambiguous := []TVSeason{seasons[0], {SeasonNumber: 3, Episodes: append([]TVEpisode(nil), seasons[0].Episodes...)}}
	if _, ok, _ := MatchSeasonByRuntime(titles, ambiguous); ok {
		t.Fatal("identical runtimes across seasons must be ambiguous")
	}
	if _, ok, _ := MatchSeasonByRuntime(titles[:2], seasons); ok {
		t.Fatal("fewer than three titles must not infer a season")
	}
	if _, ok, _ := MatchSeasonByRuntime(titles, seasons[:1]); ok {
		t.Fatal("one available season cannot distinguish it from other seasons")
	}

	repeatedDurations := []disc.MKVTitle{
		{Index: 1, Duration: 20 * time.Minute},
		{Index: 2, Duration: 20 * time.Minute},
		{Index: 3, Duration: 20 * time.Minute},
	}
	repeatedSeasons := []TVSeason{
		{SeasonNumber: 1, Episodes: []TVEpisode{
			{EpisodeNumber: 1, Runtime: 20}, {EpisodeNumber: 2, Runtime: 42}, {EpisodeNumber: 3, Runtime: 42},
		}},
		{SeasonNumber: 2, Episodes: []TVEpisode{
			{EpisodeNumber: 1, Runtime: 21}, {EpisodeNumber: 2, Runtime: 21}, {EpisodeNumber: 3, Runtime: 21},
		}},
	}
	if season, ok, _ := MatchSeasonByRuntime(repeatedDurations, repeatedSeasons); !ok || season != 2 {
		t.Fatalf("runtime distribution should distinguish season 2, got season=%d matched=%v", season, ok)
	}
}

func TestParseTVEpisodeReference(t *testing.T) {
	for _, title := range []string{"S02E11 - Episode", "s2e3", "2x03"} {
		season, episode, ok := ParseTVEpisodeReference(title)
		if !ok || season != 2 || episode == 0 {
			t.Errorf("ParseTVEpisodeReference(%q) = (%d, %d, %v)", title, season, episode, ok)
		}
	}
	if _, _, ok := ParseTVEpisodeReference("Title 12 - Main Feature"); ok {
		t.Fatal("a bare title index/number is not episode evidence")
	}
	if got := TVQueryFromTitleName("The Wire - S02E03"); got != "the wire" {
		t.Fatalf("TVQueryFromTitleName() = %q, want the wire", got)
	}
}

func TestTMDBTVRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/search/tv":
			if r.URL.Query().Get("query") != "friends" || r.URL.Query().Get("api_key") != "tmdb-key" {
				t.Errorf("unexpected search request: %s", r.URL.String())
			}
			_, _ = fmt.Fprint(w, `{"results":[{"id":1668,"name":"Friends","first_air_date":"1994-09-22"}]}`)
		case "/3/tv/1668":
			_, _ = fmt.Fprint(w, `{"id":1668,"name":"Friends","first_air_date":"1994-09-22","seasons":[{"season_number":1}]}`)
		case "/3/tv/1668/season/1":
			_, _ = fmt.Fprint(w, `{"season_number":1,"episodes":[{"episode_number":1,"name":"Pilot","runtime":22}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newTMDBTestClient(t, server)
	results, err := client.SearchTV(context.Background(), "friends")
	if err != nil || len(results) != 1 || results[0].Year != "1994" {
		t.Fatalf("SearchTV() = %+v, %v", results, err)
	}
	detail, err := client.GetTV(context.Background(), results[0].ID)
	if err != nil || detail.Name != "Friends" || len(detail.Seasons) != 1 {
		t.Fatalf("GetTV() = %+v, %v", detail, err)
	}
	season, err := client.GetTVSeason(context.Background(), results[0].ID, 1)
	if err != nil || len(season.Episodes) != 1 || season.Episodes[0].Runtime != 22 {
		t.Fatalf("GetTVSeason() = %+v, %v", season, err)
	}
}
