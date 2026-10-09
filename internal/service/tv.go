package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/8bitreid/simplerip/internal/disc"
	"github.com/8bitreid/simplerip/internal/metadata"
	"github.com/8bitreid/simplerip/internal/store"
)

const (
	// tvEvidenceCandidates is how many top-ranked shows get their episode data
	// fetched; each costs one or two TMDB requests.
	tvEvidenceCandidates = 5
	// tvEvidenceMinSimilarity skips fetching shows whose names clearly differ.
	tvEvidenceMinSimilarity = 0.35
	tvSuggestMinSimilarity  = 0.65
	tvCertainMinSimilarity  = 0.82
	tvCertainMinLead        = 0.10
	// identityHistoryLimit bounds how many past identity decisions are scanned.
	identityHistoryLimit = 500
)

// tvSibling is a show identity confirmed for an earlier disc whose label has
// the same stem as this one (e.g. SPONGEBOB_DISC1 for SPONGEBOB_DISC2).
type tvSibling struct {
	TMDBID int
	Title  string
	Year   string
	Season int
	Label  string
}

// tvCandidateEvidence is everything known about one candidate series.
type tvCandidateEvidence struct {
	metadata.TVCandidate
	Fit        metadata.RuntimeFit
	FitSeason  int
	FitCost    float64
	Data       *metadata.TVShowData // nil when not fetched
	FetchError string
	Sibling    bool
}

type tvDiscIdentification struct {
	Query           string
	Candidates      []tvCandidateEvidence
	Show            *metadata.MediaSearchResult
	ShowCertain     bool
	ShowWhy         string
	Sibling         *tvSibling
	Season          int
	SeasonWhy       string
	SuggestedSeason int
	Episodes        map[int]int
	EpisodeWhy      string
	LookupError     string
}

// tvSiblingFor looks up an earlier confirmed identity for a disc of the same set.
func (s *RipService) tvSiblingFor(ctx context.Context, jobID, discName string) (*tvSibling, error) {
	if s.store == nil || metadata.TVLabelStem(discName) == "" {
		return nil, nil
	}
	records, err := s.store.IdentityHistory(ctx, jobID, identityHistoryLimit)
	if err != nil {
		return nil, err
	}
	return siblingTVIdentity(discName, records), nil
}

// siblingTVIdentity finds the show confirmed for earlier discs whose label has
// the same stem. Each job counts once, by its latest decision. Disagreeing jobs
// cancel the evidence, since the stem then does not identify one show.
func siblingTVIdentity(label string, records []store.IdentityRecord) *tvSibling {
	stem := metadata.TVLabelStem(label)
	if stem == "" {
		return nil
	}
	seen := make(map[string]bool)
	var matches []tvSibling
	for _, record := range records { // newest first
		if seen[record.JobID] || metadata.TVLabelStem(record.DiscLabel) != stem {
			continue
		}
		seen[record.JobID] = true
		var payload map[string]any
		if err := json.Unmarshal(record.Data, &payload); err != nil {
			continue
		}
		var match tvSibling
		if corrected, _ := payload["correction"].(bool); corrected {
			if mediaType, _ := payload["media_type"].(string); mediaType != "tv" {
				continue
			}
			match.TMDBID = intFromPayload(payload["tmdb_id"])
			match.Title, _ = payload["title"].(string)
			match.Year = yearFromPayload(payload["year"])
		} else {
			match.TMDBID = intFromPayload(payload["suggested_tmdb_id"])
			match.Title, _ = payload["suggested_title"].(string)
			match.Year = yearFromPayload(payload["suggested_year"])
		}
		match.Season = intFromPayload(payload["season"])
		match.Label = record.DiscLabel
		if match.TMDBID <= 0 || strings.TrimSpace(match.Title) == "" {
			continue
		}
		matches = append(matches, match)
	}
	if len(matches) == 0 {
		return nil
	}
	sibling := matches[0]
	for _, match := range matches[1:] {
		if match.TMDBID != sibling.TMDBID {
			return nil
		}
		if sibling.Season == 0 {
			sibling.Season = match.Season
		}
	}
	return &sibling
}

func yearFromPayload(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case float64:
		if v > 0 {
			return strconv.Itoa(int(v))
		}
	}
	return ""
}

func (s *RipService) identifyTV(ctx context.Context, discName string, titles []disc.MKVTitle, sibling *tvSibling) tvDiscIdentification {
	result := tvDiscIdentification{Query: metadata.NormalizeTVQuery(discName), Sibling: sibling}
	if !s.tmdbConfigured() {
		result.LookupError = "TMDB credentials are not configured"
		return result
	}
	if result.Query == "" {
		result.LookupError = "disc label did not contain a searchable series name"
		return result
	}

	client := s.tmdbClient()
	allResults := make([]metadata.MediaSearchResult, 0)
	var searchErrors []string
	queries := metadata.TVSearchQueries(discName)
	seenQueries := make(map[string]bool)
	for _, title := range titles {
		for _, name := range []string{title.Name, title.SourceFileName} {
			query := metadata.TVQueryFromTitleName(name)
			if query != "" && !seenQueries[query] && len(queries) < 4 {
				queries = append(queries, query)
				seenQueries[query] = true
			}
		}
	}
	for _, query := range queries {
		found, err := client.SearchTV(ctx, query)
		if err != nil {
			searchErrors = append(searchErrors, err.Error())
			continue
		}
		allResults = append(allResults, found...)
	}
	ranked := rankTVCandidates(queries, allResults)
	if sibling != nil && !containsTVCandidate(ranked, sibling.TMDBID) {
		// The earlier disc's show is a candidate even when this search missed it.
		media := metadata.MediaSearchResult{ID: sibling.TMDBID, Title: sibling.Title, Year: sibling.Year, MediaType: "tv"}
		ranked = append(ranked, rankTVCandidates(queries, []metadata.MediaSearchResult{media})...)
		metadata.SortTVCandidates(ranked)
	}
	if len(ranked) == 0 {
		if len(searchErrors) > 0 {
			result.LookupError = strings.Join(searchErrors, "; ")
		} else {
			result.LookupError = "TMDB returned no TV series candidates"
		}
		return result
	}

	result.Candidates = gatherTVEvidence(ctx, client, ranked, titles, sibling)
	chosen, certain, why := chooseTVShow(result.Candidates)
	result.ShowWhy = why
	if chosen < 0 {
		result.SeasonWhy = why
		return result
	}
	best := result.Candidates[chosen]
	if certain || best.Sibling || best.Similarity >= tvSuggestMinSimilarity {
		candidate := best.Media
		result.Show = &candidate
	}
	result.ShowCertain = certain
	if !certain {
		result.SeasonWhy = "show identity is not sufficiently distinct"
		return result
	}

	if season, episodes, ok := explicitEpisodeNumbers(titles); ok {
		result.Season = season
		result.SeasonWhy = "all main-title names contain the same explicit SxxEyy season marker"
		result.Episodes = episodes
		result.EpisodeWhy = "episode numbers read directly from title names"
		return result
	}

	switch {
	case best.Data == nil:
		result.LookupError = "show details unavailable"
		if best.FetchError != "" {
			result.LookupError += ": " + best.FetchError
		}
		result.SeasonWhy = "season episode runtimes could not be checked"
	case !best.Data.Complete:
		result.SeasonWhy = "not all season runtime data was available for a safe comparison"
	default:
		season, matched, mapping := metadata.MatchSeasonByRuntime(titles, best.Data.Seasons)
		if matched {
			result.Season = season
			result.SeasonWhy = "disc title runtimes uniquely match this season's episode-runtime distribution"
			if len(mapping) == len(titles) {
				result.Episodes = mapping
				result.EpisodeWhy = "each title has a unique matching episode runtime; MakeMKV title indexes were not used as order"
			} else {
				result.EpisodeWhy = "season matched, but episode runtimes are not distinctive enough to map titles"
			}
		} else {
			result.SeasonWhy = "season runtimes do not distinguish one season from the others"
		}
	}
	if result.Season == 0 && sibling != nil && sibling.TMDBID == best.Media.ID && sibling.Season > 0 {
		result.SuggestedSeason = sibling.Season
		result.SeasonWhy += fmt.Sprintf("; earlier disc %s was season %d (suggested, not applied)", sibling.Label, sibling.Season)
	}
	if len(searchErrors) > 0 && result.LookupError == "" {
		result.LookupError = strings.Join(searchErrors, "; ")
	}
	return result
}

func containsTVCandidate(ranked []metadata.TVCandidate, id int) bool {
	for _, candidate := range ranked {
		if candidate.Media.ID == id {
			return true
		}
	}
	return false
}

// gatherTVEvidence fetches episode data for the top-ranked candidates (and an
// earlier disc's show) and checks whether their runtimes could produce the disc.
func gatherTVEvidence(ctx context.Context, client *metadata.Client, ranked []metadata.TVCandidate, titles []disc.MKVTitle, sibling *tvSibling) []tvCandidateEvidence {
	evidence := make([]tvCandidateEvidence, len(ranked))
	var wg sync.WaitGroup
	for i, candidate := range ranked {
		evidence[i] = tvCandidateEvidence{TVCandidate: candidate}
		isSibling := sibling != nil && candidate.Media.ID == sibling.TMDBID
		evidence[i].Sibling = isSibling
		if !isSibling && (i >= tvEvidenceCandidates || candidate.Similarity < tvEvidenceMinSimilarity) {
			continue
		}
		wg.Add(1)
		go func(e *tvCandidateEvidence) {
			defer wg.Done()
			data, err := client.GetTVWithSeasons(ctx, e.Media.ID)
			if err != nil {
				e.FetchError = err.Error()
				return
			}
			e.Data = data
			e.Fit, e.FitSeason, e.FitCost = metadata.SeasonRuntimeFit(titles, data.Seasons)
			if !data.Complete && e.Fit == metadata.RuntimeFitMismatch {
				// Missing seasons might have fit; don't rule the show out.
				e.Fit = metadata.RuntimeFitUnknown
			}
		}(&evidence[i])
	}
	wg.Wait()
	return evidence
}

// chooseTVShow picks the best candidate from ranked evidence and says whether
// it is certain enough to apply automatically. It returns -1 when no
// candidate is plausible.
//
//   - A show whose episode runtimes cannot produce the disc is ruled out.
//   - A show confirmed for an earlier disc of the same set wins.
//   - Otherwise the top remaining title must be similar enough, lead the next
//     remaining candidate clearly, and, when the label only matches the
//     title's leading words, be corroborated by fitting episode runtimes.
func chooseTVShow(candidates []tvCandidateEvidence) (int, bool, string) {
	var plausible []int
	for i, candidate := range candidates {
		if candidate.Fit != metadata.RuntimeFitMismatch {
			plausible = append(plausible, i)
		}
	}
	if len(plausible) == 0 {
		return -1, false, "no candidate's episode runtimes fit the disc titles"
	}
	for _, i := range plausible {
		if candidates[i].Sibling {
			return i, true, "an earlier disc with the same label stem was confirmed as this show"
		}
	}
	top := plausible[0] // candidates are already ranked
	best := candidates[top]
	switch {
	case best.Similarity < tvCertainMinSimilarity:
		return top, false, "best title similarity is below the automatic-match threshold"
	case len(plausible) > 1 && best.Similarity-candidates[plausible[1]].Similarity < tvCertainMinLead:
		return top, false, "another candidate whose runtimes also fit has a similar title"
	case best.PrefixOnly && best.Fit != metadata.RuntimeFitMatch:
		return top, false, "disc label matches only the title's leading words and episode runtimes could not confirm it"
	}
	ruledOut := len(candidates) - len(plausible)
	why := "title match with a clear lead"
	if best.Fit == metadata.RuntimeFitMatch {
		why += "; episode runtimes fit the disc"
	}
	if ruledOut > 0 {
		why += fmt.Sprintf("; %d candidate(s) ruled out by episode runtimes", ruledOut)
	}
	return top, true, why
}

// explicitEpisodeNumbers reads SxxEyy markers when every title carries one for
// the same season and no episode repeats.
func explicitEpisodeNumbers(titles []disc.MKVTitle) (int, map[int]int, bool) {
	if len(titles) == 0 {
		return 0, nil, false
	}
	season := 0
	episodes := make(map[int]int, len(titles))
	seen := make(map[int]bool, len(titles))
	for _, title := range titles {
		s, episode, ok := metadata.ParseTVEpisodeReference(title.Name)
		if !ok {
			s, episode, ok = metadata.ParseTVEpisodeReference(title.SourceFileName)
		}
		if !ok || (season != 0 && season != s) || seen[episode] {
			return 0, nil, false
		}
		if _, exists := episodes[title.Index]; exists {
			return 0, nil, false
		}
		season = s
		episodes[title.Index] = episode
		seen[episode] = true
	}
	return season, episodes, true
}

func rankTVCandidates(queries []string, results []metadata.MediaSearchResult) []metadata.TVCandidate {
	best := make(map[int]metadata.TVCandidate)
	for _, query := range queries {
		for _, candidate := range metadata.RankTVCandidates(query, results) {
			current, ok := best[candidate.Media.ID]
			if !ok || candidate.Similarity > current.Similarity {
				best[candidate.Media.ID] = candidate
			}
		}
	}
	ranked := make([]metadata.TVCandidate, 0, len(best))
	for _, candidate := range best {
		ranked = append(ranked, candidate)
	}
	metadata.SortTVCandidates(ranked)
	return ranked
}

func tvIdentificationEventData(result tvDiscIdentification) map[string]any {
	candidates := make([]map[string]any, 0, min(5, len(result.Candidates)))
	for _, candidate := range result.Candidates[:min(5, len(result.Candidates))] {
		entry := map[string]any{
			"tmdb_id": candidate.Media.ID, "title": candidate.Media.Title,
			"year": candidate.Media.Year, "title_similarity": candidate.Similarity,
			"matched_query": candidate.MatchedQuery, "vote_count": candidate.Media.VoteCount,
			"runtime_fit": candidate.Fit.String(),
		}
		if candidate.PrefixOnly {
			entry["leading_words_match"] = true
		}
		if candidate.FitSeason > 0 {
			entry["best_fit_season"] = candidate.FitSeason
			entry["runtime_cost_minutes"] = candidate.FitCost
		}
		if candidate.Sibling {
			entry["earlier_disc"] = true
		}
		if candidate.FetchError != "" {
			entry["fetch_error"] = candidate.FetchError
		}
		candidates = append(candidates, entry)
	}
	data := map[string]any{
		"action": "tv_identification", "query": result.Query,
		"show_confidence": "low", "title_similarity_is_probability": false,
		"candidates": candidates, "season_confidence": "unresolved",
		"episode_confidence": "unresolved", "season_reason": result.SeasonWhy,
	}
	if result.ShowWhy != "" {
		data["show_reason"] = result.ShowWhy
	}
	if result.ShowCertain {
		data["show_confidence"] = "high"
	} else if result.Show != nil {
		data["show_confidence"] = "suggestion"
	}
	if result.Show != nil {
		data["suggested_tmdb_id"] = result.Show.ID
		data["suggested_title"] = result.Show.Title
		data["suggested_year"] = result.Show.Year
	}
	if result.Sibling != nil {
		data["earlier_disc_label"] = result.Sibling.Label
		data["earlier_disc_tmdb_id"] = result.Sibling.TMDBID
	}
	if result.Season > 0 {
		data["season"] = result.Season
		data["season_confidence"] = "high"
		data["season_reason"] = result.SeasonWhy
	} else if result.SuggestedSeason > 0 {
		data["suggested_season"] = result.SuggestedSeason
		data["season_confidence"] = "suggestion"
	}
	if len(result.Episodes) > 0 {
		data["episode_confidence"] = "high"
		data["episode_reason"] = result.EpisodeWhy
		data["episode_numbers_by_title_index"] = result.Episodes
	} else {
		data["episode_reason"] = result.EpisodeWhy
	}
	if result.LookupError != "" {
		data["lookup_error"] = result.LookupError
	}
	return data
}
