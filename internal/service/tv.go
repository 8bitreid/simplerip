package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/8bitreid/simplerip/internal/disc"
	"github.com/8bitreid/simplerip/internal/metadata"
)

type tvDiscIdentification struct {
	Query       string
	Candidates  []metadata.TVCandidate
	Show        *metadata.MediaSearchResult
	ShowCertain bool
	Season      int
	SeasonWhy   string
	Episodes    map[int]int
	EpisodeWhy  string
	LookupError string
}

func (s *RipService) identifyTV(ctx context.Context, discName string, titles []disc.MKVTitle) tvDiscIdentification {
	result := tvDiscIdentification{Query: metadata.NormalizeTVQuery(discName)}
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
	result.Candidates = rankTVCandidates(queries, allResults)
	if len(result.Candidates) == 0 {
		if len(searchErrors) > 0 {
			result.LookupError = strings.Join(searchErrors, "; ")
		} else {
			result.LookupError = "TMDB returned no TV series candidates"
		}
		return result
	}

	best := result.Candidates[0]
	if best.Similarity >= 0.65 {
		candidate := best.Media
		result.Show = &candidate
	}
	result.ShowCertain = metadata.ConfidentTVMatch(result.Candidates)
	if !result.ShowCertain {
		result.SeasonWhy = "show identity is not sufficiently distinct"
		return result
	}
	if result.Show == nil {
		result.SeasonWhy = "disc label is not similar enough to a series title"
		return result
	}

	allExplicit := len(titles) > 0
	explicitSeason := 0
	explicitEpisodes := make(map[int]int, len(titles))
	for _, title := range titles {
		season, episode, ok := metadata.ParseTVEpisodeReference(title.Name)
		if !ok {
			season, episode, ok = metadata.ParseTVEpisodeReference(title.SourceFileName)
		}
		if !ok || (explicitSeason != 0 && explicitSeason != season) {
			allExplicit = false
			break
		}
		if _, exists := explicitEpisodes[title.Index]; exists {
			allExplicit = false
			break
		}
		explicitSeason = season
		explicitEpisodes[title.Index] = episode
	}
	if allExplicit {
		seen := make(map[int]bool, len(explicitEpisodes))
		for _, episode := range explicitEpisodes {
			if seen[episode] {
				allExplicit = false
				break
			}
			seen[episode] = true
		}
	}
	if allExplicit {
		result.Season = explicitSeason
		result.SeasonWhy = "all main-title names contain the same explicit SxxEyy season marker"
		result.Episodes = explicitEpisodes
		result.EpisodeWhy = "episode numbers read directly from title names"
		return result
	}

	lookupCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	detail, err := client.GetTV(lookupCtx, result.Show.ID)
	if err != nil {
		result.LookupError = fmt.Sprintf("show details unavailable: %v", err)
		result.SeasonWhy = "season episode runtimes could not be checked"
		return result
	}

	type seasonResponse struct {
		season metadata.TVSeason
		err    error
	}
	var summaries []metadata.TVSeasonBrief
	for _, season := range detail.Seasons {
		if season.SeasonNumber > 0 {
			summaries = append(summaries, season)
		}
	}
	if len(summaries) > 20 {
		result.SeasonWhy = "the series has too many seasons to compare within the bounded metadata lookup"
		return result
	}
	responses := make(chan seasonResponse, len(summaries))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(4, len(summaries)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for number := range jobs {
				season, err := client.GetTVSeason(lookupCtx, result.Show.ID, number)
				if err != nil {
					responses <- seasonResponse{err: fmt.Errorf("season %d: %w", number, err)}
					continue
				}
				responses <- seasonResponse{season: *season}
			}
		}()
	}
	go func() {
		for _, summary := range summaries {
			jobs <- summary.SeasonNumber
		}
		close(jobs)
	}()
	go func() {
		wg.Wait()
		close(responses)
	}()
	var seasons []metadata.TVSeason
	complete := 0
	for response := range responses {
		if response.err != nil {
			searchErrors = append(searchErrors, response.err.Error())
			continue
		}
		seasons = append(seasons, response.season)
		complete++
	}
	sort.Slice(seasons, func(i, j int) bool { return seasons[i].SeasonNumber < seasons[j].SeasonNumber })
	if len(seasons) == 0 || complete != len(summaries) {
		result.LookupError = strings.Join(searchErrors, "; ")
		result.SeasonWhy = "not all season runtime data was available for a safe comparison"
		return result
	}
	season, matched, mapping := metadata.MatchSeasonByRuntime(titles, seasons)
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
	if len(searchErrors) > 0 {
		result.LookupError = strings.Join(searchErrors, "; ")
	}
	return result
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
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Similarity == ranked[j].Similarity {
			return ranked[i].Media.Title < ranked[j].Media.Title
		}
		return ranked[i].Similarity > ranked[j].Similarity
	})
	return ranked
}

func tvIdentificationEventData(result tvDiscIdentification) map[string]any {
	candidates := make([]map[string]any, 0, min(5, len(result.Candidates)))
	for _, candidate := range result.Candidates[:min(5, len(result.Candidates))] {
		candidates = append(candidates, map[string]any{
			"tmdb_id": candidate.Media.ID, "title": candidate.Media.Title,
			"year": candidate.Media.Year, "title_similarity": candidate.Similarity,
			"matched_query": candidate.MatchedQuery,
		})
	}
	data := map[string]any{
		"action": "tv_identification", "query": result.Query,
		"show_confidence": "low", "title_similarity_is_probability": false,
		"candidates": candidates, "season_confidence": "unresolved",
		"episode_confidence": "unresolved", "season_reason": result.SeasonWhy,
	}
	if result.ShowCertain {
		data["show_confidence"] = "high"
	} else if result.Show != nil {
		data["show_confidence"] = "suggestion"
		data["show_reason"] = "candidate similarity or lead over alternatives is insufficient for automatic selection"
	}
	if result.Show != nil {
		data["suggested_tmdb_id"] = result.Show.ID
		data["suggested_title"] = result.Show.Title
		data["suggested_year"] = result.Show.Year
	}
	if result.Season > 0 {
		data["season"] = result.Season
		data["season_confidence"] = "high"
		data["season_reason"] = result.SeasonWhy
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
