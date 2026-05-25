package output

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/8bitreid/simplerip/internal/inspect"
)

const dupTolerance = 5 * time.Second

// FileReport holds ffprobe data for one file in a duplicate group.
type FileReport struct {
	Path      string
	SizeBytes int64
	Duration  time.Duration
	Info      *inspect.FileInfo
}

// CleanAnalysis is the result of analysing a directory — no files are moved.
type CleanAnalysis struct {
	Keeper     FileReport
	Duplicates []FileReport // files that would be moved to _duplicates/
}

// CleanResult is returned after deduplication has actually been executed.
type CleanResult struct {
	Kept       string       // absolute path of the kept file
	KeptReport FileReport
	Duplicates []string     // destination paths inside _duplicates/
	DupReports []FileReport
}

// AnalyzeDir probes all MKV files in dir, groups them by duration, and
// returns what would be kept vs removed — without touching any files.
func AnalyzeDir(ctx context.Context, dir string) ([]CleanAnalysis, error) {
	entries, err := filepath.Glob(filepath.Join(dir, "*.mkv"))
	if err != nil || len(entries) == 0 {
		return nil, fmt.Errorf("no MKV files found in %q", dir)
	}

	var infos []*inspect.FileInfo
	for _, path := range entries {
		info, err := inspect.Probe(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("probe %q: %w", filepath.Base(path), err)
		}
		infos = append(infos, info)
	}

	groups := groupByDuration(infos, dupTolerance)

	var analyses []CleanAnalysis
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
		sort.Slice(group, func(i, j int) bool {
			return group[i].SizeBytes > group[j].SizeBytes
		})
		keeper := toReport(group[0])
		var dups []FileReport
		for _, d := range group[1:] {
			dups = append(dups, toReport(d))
		}
		analyses = append(analyses, CleanAnalysis{Keeper: keeper, Duplicates: dups})
	}
	return analyses, nil
}

// ExecuteDedupe moves the duplicates identified by analyses into <dir>/_duplicates/.
// Call AnalyzeDir first, show the user the plan, then call this.
func ExecuteDedupe(dir string, analyses []CleanAnalysis) ([]CleanResult, error) {
	dupDir := filepath.Join(dir, "_duplicates")
	var results []CleanResult

	for _, a := range analyses {
		if err := os.MkdirAll(dupDir, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir _duplicates: %w", err)
		}

		var moved []string
		var dupReports []FileReport
		for _, dup := range a.Duplicates {
			dst := filepath.Join(dupDir, filepath.Base(dup.Path))
			if err := os.Rename(dup.Path, dst); err != nil {
				return nil, fmt.Errorf("move %q: %w", filepath.Base(dup.Path), err)
			}
			moved = append(moved, dst)
			dr := dup
			dr.Path = dst
			dupReports = append(dupReports, dr)
		}

		results = append(results, CleanResult{
			Kept:       a.Keeper.Path,
			KeptReport: a.Keeper,
			Duplicates: moved,
			DupReports: dupReports,
		})
	}
	return results, nil
}

// RenameToTitle moves keptFile into baseDir/folderName/folderName.mkv.
func RenameToTitle(keptFile, baseDir, folderName string) (string, error) {
	newDir := filepath.Join(baseDir, folderName)
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %q: %w", newDir, err)
	}
	newFile := filepath.Join(newDir, folderName+".mkv")
	if err := os.Rename(keptFile, newFile); err != nil {
		return "", fmt.Errorf("rename file: %w", err)
	}
	return newFile, nil
}

func toReport(info *inspect.FileInfo) FileReport {
	return FileReport{
		Path:      info.Path,
		SizeBytes: info.SizeBytes,
		Duration:  info.Duration,
		Info:      info,
	}
}

func groupByDuration(infos []*inspect.FileInfo, tolerance time.Duration) [][]*inspect.FileInfo {
	used := make([]bool, len(infos))
	var groups [][]*inspect.FileInfo

	for i, a := range infos {
		if used[i] {
			continue
		}
		group := []*inspect.FileInfo{a}
		used[i] = true
		for j := i + 1; j < len(infos); j++ {
			if !used[j] && inspect.DurationWithin(a.Duration, infos[j].Duration, tolerance) {
				group = append(group, infos[j])
				used[j] = true
			}
		}
		groups = append(groups, group)
	}
	return groups
}
