package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// spongebobDiscTitles mirrors SPONGEBOB_DISC2: 15 segments of 11:02–11:34.
func spongebobDiscTitles() []disc.MKVTitle {
	seconds := []int{11*60 + 44, 11*60 + 3, 11*60 + 2, 11*60 + 3, 11*60 + 3, 11*60 + 3, 11*60 + 2, 11*60 + 3,
		11*60 + 2, 11*60 + 2, 11*60 + 2, 11*60 + 32, 11*60 + 13, 11*60 + 34, 11*60 + 4}
	titles := make([]disc.MKVTitle, len(seconds))
	for i, s := range seconds {
		titles[i] = disc.MKVTitle{Index: i, Duration: time.Duration(s) * time.Second}
	}
	return titles
}

func runtimeSeason(number int, runtimes ...int) TVSeason {
	season := TVSeason{SeasonNumber: number}
	for i, runtime := range runtimes {
		season.Episodes = append(season.Episodes, TVEpisode{EpisodeNumber: i + 1, Runtime: runtime})
	}
	return season
}

func repeatRuntime(runtime, count int) []int {
	runtimes := make([]int, count)
	for i := range runtimes {
		runtimes[i] = runtime
	}
	return runtimes
}

func TestLeadingWordsSimilarity(t *testing.T) {
	results := []MediaSearchResult{
		{ID: 121021, Title: "SpongeBob DocuPants", MediaType: "tv", VoteCount: 26},
		{ID: 387, Title: "SpongeBob SquarePants", MediaType: "tv", VoteCount: 3281},
		{ID: 99588, Title: "Kamp Koral: SpongeBob's Under Years", MediaType: "tv", VoteCount: 523},
	}
	ranked := RankTVCandidates("SPONGEBOB_DISC2", results)
	if ranked[0].Media.ID != 387 || ranked[1].Media.ID != 121021 {
		t.Fatalf("equal leading-word matches should rank by vote count: %+v", ranked)
	}
	if !ranked[0].PrefixOnly || ranked[0].Similarity < 0.82 {
		t.Fatalf("label abbreviating the title should be a strong prefix-only match: %+v", ranked[0])
	}
	if ranked[2].Media.ID != 99588 || ranked[2].PrefixOnly || ranked[2].Similarity > 0.5 {
		t.Fatalf("a title that only contains the label later on is not a leading-word match: %+v", ranked[2])
	}
	if got := leadingWordsSimilarity("the office", "The Office Specials"); got < 0.85 {
		t.Fatalf("leading article should not prevent a prefix match, got %.2f", got)
	}
	if got := leadingWordsSimilarity("abc", "ABC Mystery Hour"); got != 0 {
		t.Fatalf("very short labels must not earn a prefix match, got %.2f", got)
	}
	if got := leadingWordsSimilarity("friends", "Friends"); got != 0 {
		t.Fatalf("a whole-title match is scored by titleSimilarity, got %.2f", got)
	}
}

func TestSeasonRuntimeFit(t *testing.T) {
	titles := spongebobDiscTitles()
	squarePants := []TVSeason{
		runtimeSeason(1, append([]int{9, 3}, repeatRuntime(11, 39)...)...),
		runtimeSeason(2, repeatRuntime(12, 36)...),
	}
	if fit, season, cost := SeasonRuntimeFit(titles, squarePants); fit != RuntimeFitMatch || season == 0 || cost > 1 {
		t.Fatalf("SquarePants fit = %v season %d cost %.2f, want fits", fit, season, cost)
	}
	docuPants := []TVSeason{runtimeSeason(1, 14, 13, 12, 18, 14, 14, 11, 12)}
	if fit, _, _ := SeasonRuntimeFit(titles, docuPants); fit != RuntimeFitMismatch {
		t.Fatalf("an 8-episode season cannot hold 15 titles, got %v", fit)
	}
	reimagined := []TVSeason{runtimeSeason(1, repeatRuntime(4, 20)...)}
	if fit, _, _ := SeasonRuntimeFit(titles, reimagined); fit != RuntimeFitMismatch {
		t.Fatalf("4-minute episodes cannot produce 11-minute titles, got %v", fit)
	}
	unknown := []TVSeason{runtimeSeason(1, repeatRuntime(0, 20)...)}
	if fit, _, _ := SeasonRuntimeFit(titles, unknown); fit != RuntimeFitUnknown {
		t.Fatalf("missing runtimes must not rule a show out, got %v", fit)
	}
	if fit, _, _ := SeasonRuntimeFit(titles, nil); fit != RuntimeFitUnknown {
		t.Fatalf("no season data is unknown, got %v", fit)
	}
}

func TestTVLabelStem(t *testing.T) {
	tests := map[string]string{
		"SPONGEBOB_DISC2":     "spongebob",
		"SPONGEBOB_DISK2":     "spongebob",
		"SpongeBob Disc 1":    "spongebob",
		"The-Office-Volume-3": "the office",
		"DVD_VIDEO":           "",
		"DISC1":               "",
		"BDMV":                "",
	}
	for label, want := range tests {
		if got := TVLabelStem(label); got != want {
			t.Errorf("TVLabelStem(%q) = %q, want %q", label, got, want)
		}
	}
}

func TestGetTVWithSeasonsBundlesRequests(t *testing.T) {
	var bundled, single int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/3/tv/9" && r.URL.Query().Get("append_to_response") == "":
			seasons := make([]map[string]int, 0, 26)
			for n := 0; n <= 25; n++ {
				seasons = append(seasons, map[string]int{"season_number": n})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9, "name": "Long Show", "seasons": seasons})
		case r.URL.Path == "/3/tv/9":
			bundled++
			keys := strings.Split(r.URL.Query().Get("append_to_response"), ",")
			if len(keys) > 20 {
				t.Errorf("bundle of %d seasons exceeds TMDB's limit of 20", len(keys))
			}
			body := map[string]any{"id": 9}
			for _, key := range keys {
				var n int
				_, _ = fmt.Sscanf(key, "season/%d", &n)
				if n == 7 {
					continue // force the single-season fallback
				}
				body[key] = runtimeSeason(n, 22, 23)
			}
			_ = json.NewEncoder(w).Encode(body)
		case r.URL.Path == "/3/tv/9/season/7":
			single++
			_ = json.NewEncoder(w).Encode(runtimeSeason(7, 22, 23))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newTMDBTestClient(t, server)
	show, err := client.GetTVWithSeasons(context.Background(), 9)
	if err != nil {
		t.Fatalf("GetTVWithSeasons() error = %v", err)
	}
	if !show.Complete || len(show.Seasons) != 25 || show.Seasons[0].SeasonNumber != 1 || show.Seasons[24].SeasonNumber != 25 {
		t.Fatalf("seasons = %d complete=%v", len(show.Seasons), show.Complete)
	}
	if bundled != 2 || single != 1 {
		t.Fatalf("requests: %d bundled, %d single; want 2 bundled and 1 fallback", bundled, single)
	}
}
