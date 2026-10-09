package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/8bitreid/simplerip/internal/disc"
)

var (
	tvEpisodePattern = regexp.MustCompile(`(?i)\bS(\d{1,2})\s*E(\d{1,3})\b|\b(\d{1,2})x(\d{1,3})\b`)
	tvSuffixPattern  = regexp.MustCompile(`(?i)^(disc|disk|dvd|volume|vol|bd)[ ._-]*\d+$`)
)

var commonTVMisspellings = map[string]string{
	"breakng bad":          "breaking bad",
	"the simpons":          "the simpsons",
	"strnger things":       "stranger things",
	"frends":               "friends",
	"the big bang theorey": "the big bang theory",
	"law and order svu":    "law and order special victims unit",
}

// NormalizeTVQuery removes a trailing disc/volume marker and normalizes a
// small set of common series-name misspellings without guessing arbitrary words.
func NormalizeTVQuery(label string) string {
	query := normalizeTVLabel(label)
	if corrected, ok := commonTVMisspellings[query]; ok {
		return corrected
	}
	return query
}

func normalizeTVLabel(label string) string {
	query := strings.ToLower(QueryFromDirName(label))
	fields := strings.Fields(query)
	for len(fields) > 0 {
		last := strings.Trim(fields[len(fields)-1], ".,;:()[]{}")
		if tvSuffixPattern.MatchString(last) {
			fields = fields[:len(fields)-1]
			continue
		}
		// Handle separators that QueryFromDirName turns into separate tokens.
		if len(fields) >= 2 && (fields[len(fields)-2] == "disc" || fields[len(fields)-2] == "disk" ||
			fields[len(fields)-2] == "dvd" || fields[len(fields)-2] == "volume" || fields[len(fields)-2] == "vol") {
			if _, err := fmt.Sscanf(last, "%d", new(int)); err == nil {
				fields = fields[:len(fields)-2]
				continue
			}
		}
		break
	}
	return strings.Join(fields, " ")
}

// TVSearchQueries returns the normalized query followed by a known spelling
// correction when the original normalized phrase is a listed common typo.
func TVSearchQueries(label string) []string {
	normalized := normalizeTVLabel(label)
	query := NormalizeTVQuery(label)
	if query == "" {
		return nil
	}
	if query != normalized {
		return []string{query, normalized}
	}
	return []string{query}
}

// TVQueryFromTitleName extracts a likely series-name prefix/suffix when the
// MakeMKV title name contains an explicit episode marker.
func TVQueryFromTitleName(title string) string {
	match := tvEpisodePattern.FindStringIndex(title)
	if match == nil {
		return ""
	}
	before := strings.Trim(title[:match[0]], " -_:.,")
	after := strings.Trim(title[match[1]:], " -_:.,")
	query := before
	if len(strings.Fields(after)) > len(strings.Fields(before)) {
		query = after
	}
	return NormalizeTVQuery(query)
}

// RankTVCandidates orders TMDB TV results by normalized title similarity.
// Similarity is an explainable string-match score, not a probability.
func RankTVCandidates(query string, results []MediaSearchResult) []TVCandidate {
	query = NormalizeTVQuery(query)
	ranked := make([]TVCandidate, 0, len(results))
	seen := make(map[int]bool)
	for _, result := range results {
		if result.MediaType != "tv" || seen[result.ID] {
			continue
		}
		seen[result.ID] = true
		score := titleSimilarity(query, result.Title)
		prefix := false
		if leading := leadingWordsSimilarity(query, result.Title); leading > score {
			score, prefix = leading, true
		}
		ranked = append(ranked, TVCandidate{Media: result, Similarity: score, MatchedQuery: query, PrefixOnly: prefix})
	}
	SortTVCandidates(ranked)
	return ranked
}

// SortTVCandidates orders candidates by similarity, then TMDB vote count, then title.
func SortTVCandidates(ranked []TVCandidate) {
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Similarity != ranked[j].Similarity {
			return ranked[i].Similarity > ranked[j].Similarity
		}
		if ranked[i].Media.VoteCount != ranked[j].Media.VoteCount {
			return ranked[i].Media.VoteCount > ranked[j].Media.VoteCount
		}
		return ranked[i].Media.Title < ranked[j].Media.Title
	})
}

// TVCandidate includes a normalized string similarity score (0–1); it is not
// a probability or a measure of episode/season certainty.
type TVCandidate struct {
	Media        MediaSearchResult
	Similarity   float64
	MatchedQuery string
	// PrefixOnly marks a score earned because the query is the title's leading
	// words ("spongebob" for "SpongeBob SquarePants") rather than the whole
	// title. Such a match needs corroborating evidence before it is trusted.
	PrefixOnly bool
}

// leadingWordsSimilarity scores a disc label that abbreviates a series title
// to its leading words. Labels are often truncated, so this is strong but not
// exact evidence. It returns 0 unless every query word matches, in order, the
// start of the title.
func leadingWordsSimilarity(query, title string) float64 {
	q := strings.Fields(normalizedTitle(query))
	t := strings.Fields(normalizedTitle(title))
	if len(q) > 0 && len(t) > 0 && q[0] == "the" && t[0] != "the" {
		q = q[1:]
	}
	if len(t) > 0 && t[0] == "the" && (len(q) == 0 || q[0] != "the") {
		t = t[1:]
	}
	if len(q) == 0 || len(q) >= len(t) || len(strings.Join(q, "")) < 4 {
		return 0
	}
	for i, word := range q {
		if t[i] != word {
			return 0
		}
	}
	return 0.85 + 0.15*float64(len(q))/float64(len(t))
}

// ConfidentTVMatch accepts only a high-similarity result with a clear lead.
func ConfidentTVMatch(ranked []TVCandidate) bool {
	return len(ranked) > 0 && ranked[0].Similarity >= 0.82 &&
		(len(ranked) == 1 || ranked[0].Similarity-ranked[1].Similarity >= 0.10)
}

type TMDbTVDetail struct {
	ID           int             `json:"id"`
	Name         string          `json:"name"`
	FirstAirDate string          `json:"first_air_date"`
	Seasons      []TVSeasonBrief `json:"seasons"`
}

type TVSeasonBrief struct {
	SeasonNumber int `json:"season_number"`
}

type TVEpisode struct {
	EpisodeNumber int    `json:"episode_number"`
	Name          string `json:"name"`
	Runtime       int    `json:"runtime"`
}

type TVSeason struct {
	SeasonNumber int         `json:"season_number"`
	Episodes     []TVEpisode `json:"episodes"`
}

func (c *Client) tvRequest(ctx context.Context, endpoint string, params url.Values, target any) error {
	u, err := url.Parse(tmdbBase + endpoint)
	if err != nil {
		return fmt.Errorf("build TMDB TV URL: %w", err)
	}
	q := u.Query()
	for key, values := range params {
		q[key] = values
	}
	c.setAPIKey(q)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("build TMDB TV request: %w", err)
	}
	c.authorize(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("TMDB TV request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("TMDB TV request returned %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return fmt.Errorf("decode TMDB TV response: %w", err)
	}
	return nil
}

func (c *Client) SearchTV(ctx context.Context, query string) ([]MediaSearchResult, error) {
	u, _ := url.Parse(tmdbBase + "/search/tv")
	q := u.Query()
	c.setAPIKey(q)
	q.Set("query", query)
	q.Set("language", "en-US")
	q.Set("page", "1")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build TMDB TV search request: %w", err)
	}
	c.authorize(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("TMDB TV search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("TMDB TV search returned %d", resp.StatusCode)
	}
	var body struct {
		Results []struct {
			ID           int    `json:"id"`
			Name         string `json:"name"`
			FirstAirDate string `json:"first_air_date"`
			VoteCount    int    `json:"vote_count"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode TMDB TV search response: %w", err)
	}
	results := make([]MediaSearchResult, 0, min(len(body.Results), 10))
	for _, item := range body.Results {
		if item.ID <= 0 || strings.TrimSpace(item.Name) == "" {
			continue
		}
		year := ""
		if len(item.FirstAirDate) >= 4 {
			year = item.FirstAirDate[:4]
		}
		results = append(results, MediaSearchResult{ID: item.ID, Title: item.Name, Year: year, MediaType: "tv", VoteCount: item.VoteCount})
		if len(results) == 10 {
			break
		}
	}
	return results, nil
}

func (c *Client) GetTV(ctx context.Context, id int) (*TMDbTVDetail, error) {
	if id <= 0 {
		return nil, fmt.Errorf("invalid TMDB TV ID %d", id)
	}
	var detail TMDbTVDetail
	if err := c.tvRequest(ctx, fmt.Sprintf("/tv/%d", id), nil, &detail); err != nil {
		return nil, err
	}
	return &detail, nil
}

// maxAppendedSeasons is TMDB's limit on append_to_response items per request.
const maxAppendedSeasons = 20

// maxFetchedSeasons bounds how many seasons GetTVWithSeasons will load.
const maxFetchedSeasons = 60

// TVShowData is a series with every numbered season's episode list.
type TVShowData struct {
	Detail  TMDbTVDetail
	Seasons []TVSeason // numbered seasons (no specials), ascending
	// Complete is false when some numbered season could not be loaded.
	Complete bool
}

// GetTVWithSeasons loads a series and all of its numbered seasons, bundling up
// to 20 seasons per request with append_to_response. A season missing from a
// bundled response is fetched on its own.
func (c *Client) GetTVWithSeasons(ctx context.Context, id int) (*TVShowData, error) {
	detail, err := c.GetTV(ctx, id)
	if err != nil {
		return nil, err
	}
	show := &TVShowData{Detail: *detail, Complete: true}
	var numbers []int
	for _, season := range detail.Seasons {
		if season.SeasonNumber > 0 {
			numbers = append(numbers, season.SeasonNumber)
		}
	}
	if len(numbers) > maxFetchedSeasons {
		show.Complete = false
		return show, nil
	}
	for start := 0; start < len(numbers); start += maxAppendedSeasons {
		chunk := numbers[start:min(start+maxAppendedSeasons, len(numbers))]
		keys := make([]string, len(chunk))
		for i, number := range chunk {
			keys[i] = fmt.Sprintf("season/%d", number)
		}
		var bundle map[string]json.RawMessage
		params := url.Values{"append_to_response": {strings.Join(keys, ",")}}
		if err := c.tvRequest(ctx, fmt.Sprintf("/tv/%d", id), params, &bundle); err != nil {
			return nil, err
		}
		for i, number := range chunk {
			var season TVSeason
			raw, ok := bundle[keys[i]]
			if !ok || json.Unmarshal(raw, &season) != nil || season.SeasonNumber != number {
				fetched, err := c.GetTVSeason(ctx, id, number)
				if err != nil {
					show.Complete = false
					continue
				}
				season = *fetched
			}
			show.Seasons = append(show.Seasons, season)
		}
	}
	sort.Slice(show.Seasons, func(i, j int) bool { return show.Seasons[i].SeasonNumber < show.Seasons[j].SeasonNumber })
	return show, nil
}

func (c *Client) GetTVSeason(ctx context.Context, id, season int) (*TVSeason, error) {
	if id <= 0 || season < 0 {
		return nil, fmt.Errorf("invalid TMDB TV season %d for show %d", season, id)
	}
	var detail TVSeason
	if err := c.tvRequest(ctx, fmt.Sprintf("/tv/%d/season/%d", id, season), nil, &detail); err != nil {
		return nil, err
	}
	return &detail, nil
}

// ParseTVEpisodeReference reads explicit SxxEyy or x-style title metadata.
func ParseTVEpisodeReference(title string) (season, episode int, ok bool) {
	match := tvEpisodePattern.FindStringSubmatch(title)
	if match == nil {
		return 0, 0, false
	}
	if match[1] != "" {
		_, _ = fmt.Sscanf(match[1], "%d", &season)
		_, _ = fmt.Sscanf(match[2], "%d", &episode)
	} else {
		_, _ = fmt.Sscanf(match[3], "%d", &season)
		_, _ = fmt.Sscanf(match[4], "%d", &episode)
	}
	return season, episode, season > 0 && episode > 0
}

// MatchSeasonByRuntime selects a season only when at least three disc titles
// and a clear runtime-distance margin distinguish it from other seasons.
func MatchSeasonByRuntime(titles []disc.MKVTitle, seasons []TVSeason) (int, bool, map[int]int) {
	if len(titles) < 3 {
		return 0, false, nil
	}
	type seasonScore struct {
		number int
		cost   float64
	}
	var scores []seasonScore
	for _, season := range seasons {
		runtimes := make([]TVEpisode, 0, len(season.Episodes))
		for _, episode := range season.Episodes {
			if episode.Runtime > 0 {
				runtimes = append(runtimes, episode)
			}
		}
		if len(runtimes) < len(titles) {
			continue
		}
		_, cost, ok := matchRuntimeDistribution(titles, runtimes)
		if !ok {
			continue
		}
		scores = append(scores, seasonScore{number: season.SeasonNumber, cost: cost})
	}
	sort.Slice(scores, func(i, j int) bool { return scores[i].cost < scores[j].cost })
	if len(scores) < 2 || scores[0].cost > 2 || scores[1].cost-scores[0].cost < 1.5 {
		return 0, false, nil
	}

	var chosen *TVSeason
	for i := range seasons {
		if seasons[i].SeasonNumber == scores[0].number {
			chosen = &seasons[i]
			break
		}
	}
	if chosen == nil {
		return 0, false, nil
	}
	episodes := make(map[int]int, len(titles))
	used := make(map[int]bool)
	for _, title := range titles {
		minutes := title.Duration.Minutes()
		bestDiff, secondDiff, bestEpisode := math.Inf(1), math.Inf(1), 0
		for _, episode := range chosen.Episodes {
			if episode.Runtime <= 0 {
				continue
			}
			delta := math.Abs(minutes - float64(episode.Runtime))
			if delta < bestDiff {
				secondDiff, bestDiff, bestEpisode = bestDiff, delta, episode.EpisodeNumber
			} else if delta < secondDiff {
				secondDiff = delta
			}
		}
		if bestEpisode == 0 || bestDiff > 1.5 || secondDiff-bestDiff < 0.75 || used[bestEpisode] {
			return scores[0].number, true, nil
		}
		episodes[title.Index] = bestEpisode
		used[bestEpisode] = true
	}
	return scores[0].number, true, episodes
}

// RuntimeFit says whether a series' episode runtimes could produce a disc's titles.
type RuntimeFit int

const (
	// RuntimeFitUnknown means TMDB lacks the runtimes to say either way.
	RuntimeFitUnknown RuntimeFit = iota
	// RuntimeFitMatch means some season has enough episodes whose runtimes
	// closely match the disc's title durations.
	RuntimeFitMatch
	// RuntimeFitMismatch means no season has enough episodes, or the runtimes
	// that are known are clearly different from the disc's titles.
	RuntimeFitMismatch
)

func (f RuntimeFit) String() string {
	switch f {
	case RuntimeFitMatch:
		return "fits"
	case RuntimeFitMismatch:
		return "mismatch"
	default:
		return "unknown"
	}
}

const (
	// Mean minutes between disc titles and episode runtimes for a season to fit.
	runtimeFitMaxCost = 2.0
	// Beyond this mean distance, known runtimes rule the season out.
	runtimeMismatchMinCost = 3.0
)

// SeasonRuntimeFit checks the disc's titles against every numbered season and
// reports the best-fitting season and its mean per-title runtime distance.
func SeasonRuntimeFit(titles []disc.MKVTitle, seasons []TVSeason) (fit RuntimeFit, season int, cost float64) {
	if len(titles) == 0 {
		return RuntimeFitUnknown, 0, 0
	}
	bestCost := math.Inf(1)
	bigEnough, missingRuntimes, numbered := false, false, false
	for _, s := range seasons {
		if s.SeasonNumber < 1 {
			continue
		}
		numbered = true
		if len(s.Episodes) < len(titles) {
			continue
		}
		bigEnough = true
		known := make([]TVEpisode, 0, len(s.Episodes))
		for _, episode := range s.Episodes {
			if episode.Runtime > 0 {
				known = append(known, episode)
			}
		}
		if len(known) < len(titles) {
			missingRuntimes = true
			continue
		}
		if _, c, ok := matchRuntimeDistribution(titles, known); ok && c < bestCost {
			bestCost, season = c, s.SeasonNumber
		}
	}
	switch {
	case !numbered:
		return RuntimeFitUnknown, 0, 0
	case bestCost <= runtimeFitMaxCost:
		return RuntimeFitMatch, season, bestCost
	case !bigEnough:
		return RuntimeFitMismatch, 0, 0
	case missingRuntimes || bestCost <= runtimeMismatchMinCost:
		if math.IsInf(bestCost, 1) {
			return RuntimeFitUnknown, 0, 0
		}
		return RuntimeFitUnknown, season, bestCost
	default:
		return RuntimeFitMismatch, season, bestCost
	}
}

// genericLabelStems are disc labels that say nothing about the content, so
// two discs sharing one are not evidence of the same series.
var genericLabelStems = map[string]bool{
	"dvd": true, "dvd video": true, "dvdvideo": true, "video ts": true, "bluray": true,
	"blu ray": true, "bdmv": true, "bd rom": true, "disc": true, "disk": true,
	"video": true, "volume": true, "untitled": true, "no label": true, "new volume": true,
}

// TVLabelStem returns a disc label with its trailing disc/volume number
// removed ("SPONGEBOB_DISC2" → "spongebob"), so discs from one set share a
// stem. It returns "" for labels too generic to identify a series.
func TVLabelStem(label string) string {
	stem := normalizedTitle(normalizeTVLabel(label))
	letters := 0
	for _, r := range stem {
		if r >= 'a' && r <= 'z' {
			letters++
		}
	}
	if letters < 3 || genericLabelStems[stem] {
		return ""
	}
	return stem
}

func matchRuntimeDistribution(titles []disc.MKVTitle, episodes []TVEpisode) ([]TVEpisode, float64, bool) {
	orderedTitles := append([]disc.MKVTitle(nil), titles...)
	orderedEpisodes := append([]TVEpisode(nil), episodes...)
	sort.Slice(orderedTitles, func(i, j int) bool { return orderedTitles[i].Duration < orderedTitles[j].Duration })
	sort.Slice(orderedEpisodes, func(i, j int) bool {
		if orderedEpisodes[i].Runtime == orderedEpisodes[j].Runtime {
			return orderedEpisodes[i].EpisodeNumber < orderedEpisodes[j].EpisodeNumber
		}
		return orderedEpisodes[i].Runtime < orderedEpisodes[j].Runtime
	})
	n, m := len(orderedTitles), len(orderedEpisodes)
	dp := make([][]float64, n+1)
	take := make([][]bool, n+1)
	for i := range dp {
		dp[i] = make([]float64, m+1)
		take[i] = make([]bool, m+1)
		for j := range dp[i] {
			dp[i][j] = math.Inf(1)
		}
	}
	for j := range dp[0] {
		dp[0][j] = 0
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			dp[i][j] = dp[i][j-1]
			delta := math.Abs(orderedTitles[i-1].Duration.Minutes() - float64(orderedEpisodes[j-1].Runtime))
			matched := dp[i-1][j-1] + delta
			if matched < dp[i][j] {
				dp[i][j] = matched
				take[i][j] = true
			}
		}
	}
	if math.IsInf(dp[n][m], 1) {
		return nil, 0, false
	}
	assignment := make([]TVEpisode, n)
	i, j := n, m
	for i > 0 && j > 0 {
		if take[i][j] {
			assignment[i-1] = orderedEpisodes[j-1]
			i--
			j--
		} else {
			j--
		}
	}
	if i != 0 {
		return nil, 0, false
	}
	return assignment, dp[n][m] / float64(n), true
}

func titleSimilarity(a, b string) float64 {
	a, b = normalizedTitle(a), normalizedTitle(b)
	if a == "" || b == "" {
		return 0
	}
	char := 1 - float64(editDistance(a, b))/float64(max(len([]rune(a)), len([]rune(b))))
	at, bt := strings.Fields(a), strings.Fields(b)
	aset, bset := make(map[string]bool), make(map[string]bool)
	for _, token := range at {
		aset[token] = true
	}
	for _, token := range bt {
		bset[token] = true
	}
	common := 0
	for token := range aset {
		if bset[token] {
			common++
		}
	}
	dice := 2 * float64(common) / float64(len(aset)+len(bset))
	return 0.7*char + 0.3*dice
}

func normalizedTitle(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	previous := make([]int, len(br)+1)
	for j := range previous {
		previous[j] = j
	}
	for i, x := range ar {
		current := make([]int, len(br)+1)
		current[0] = i + 1
		for j, y := range br {
			cost := 0
			if x != y {
				cost = 1
			}
			current[j+1] = min(current[j]+1, previous[j+1]+1, previous[j]+cost)
		}
		previous = current
	}
	return previous[len(br)]
}
