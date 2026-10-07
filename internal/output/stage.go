// Package output handles moving ripped MKV files to their final destination
// and writing the rip.json audit log.
package output

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// RipLog is the data written to rip.json in the destination directory.
type RipLog struct {
	Title      string    `json:"title"`
	DiscName   string    `json:"disc_name"`
	RippedAt   time.Time `json:"ripped_at"`
	Files      []string  `json:"files"`
	StagingDir string    `json:"staging_dir"`
	DestDir    string    `json:"dest_dir"`
}

// DeliverResult is returned by Deliver after a successful rsync.
type DeliverResult struct {
	DestDir string
	Files   []string // absolute paths at destination
}

// Deliver rsyncs srcFiles from stagingDir into destDir/subdir, verifies the
// files arrived, then writes rip.json. Returns the destination paths.
//
// srcFiles must be absolute paths inside stagingDir.
// subdir is a relative directory inside destDir (e.g. "Oppenheimer (2023)/season-1").
// title and discName are stored in the log only.
func Deliver(
	ctx context.Context,
	srcFiles []string,
	stagingDir, destDir, subdir string,
	title, discName string,
) (*DeliverResult, error) {
	relativeDir, err := sanitizeRelativePath(subdir)
	if err != nil {
		return nil, fmt.Errorf("invalid delivery directory: %w", err)
	}
	target := filepath.Join(destDir, relativeDir)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %q: %w", target, err)
	}

	if err := rsync(ctx, srcFiles, target); err != nil {
		return nil, err
	}

	// Verify every source file arrived at the destination.
	var destFiles []string
	for _, src := range srcFiles {
		dst := filepath.Join(target, filepath.Base(src))
		if err := verifyFile(src, dst); err != nil {
			return nil, fmt.Errorf("verify %q: %w", filepath.Base(src), err)
		}
		destFiles = append(destFiles, dst)
	}

	log := RipLog{
		Title:      title,
		DiscName:   discName,
		RippedAt:   time.Now().UTC(),
		Files:      destFiles,
		StagingDir: stagingDir,
		DestDir:    target,
	}
	if err := writeLog(target, log); err != nil {
		return nil, err
	}

	return &DeliverResult{DestDir: target, Files: destFiles}, nil
}

// RenameForDelivery renames ripped files in place (within their staging
// directory) to the Jellyfin-style base name: "<name>.mkv" for a single file,
// or "<name> - part1.mkv", "<name> - part2.mkv" ... when there are several.
// It returns the new paths in the same order. If any rename fails, files already
// renamed are restored and an error is returned.
func RenameForDelivery(files []string, name string) ([]string, error) {
	return RenameForDeliveryPattern(files, name, false)
}

// RenameForDeliveryPattern renames ripped files in place. If isTV is true,
// multi-file episodes are formatted as "<name> - E01.mkv", "<name> - E02.mkv"
// so media servers (Jellyfin/Plex) index them as distinct episodes rather than
// stacking them as parts of a single movie.
func RenameForDeliveryPattern(files []string, name string, isTV bool) ([]string, error) {
	name = sanitizeFileName(name)
	if name == "" || len(files) == 0 {
		return files, nil
	}
	out := make([]string, 0, len(files))
	for i, src := range files {
		base := name
		if len(files) > 1 {
			if isTV {
				base = fmt.Sprintf("%s - E%02d", name, i+1)
			} else {
				base = fmt.Sprintf("%s - part%d", name, i+1)
			}
		}
		dst := filepath.Join(filepath.Dir(src), base+filepath.Ext(src))
		if dst != src {
			if _, err := os.Stat(dst); err == nil {
				err = fmt.Errorf("destination %q already exists", dst)
				rollbackRenames(files, out)
				return nil, err
			}
			if err := os.Rename(src, dst); err != nil {
				rollbackRenames(files, out)
				return nil, fmt.Errorf("rename %q: %w", filepath.Base(src), err)
			}
		}
		out = append(out, dst)
	}
	return out, nil
}

// RenameForDeliveryEpisodes assigns sequential episode-NN filenames.
func RenameForDeliveryEpisodes(files []string, firstEpisode int) ([]string, error) {
	if firstEpisode < 1 {
		return nil, fmt.Errorf("first episode number must be at least 1")
	}
	out := make([]string, 0, len(files))
	for i, src := range files {
		dst := filepath.Join(filepath.Dir(src), fmt.Sprintf("episode-%02d%s", firstEpisode+i, filepath.Ext(src)))
		if dst != src {
			if _, err := os.Stat(dst); err == nil {
				rollbackRenames(files, out)
				return nil, fmt.Errorf("destination %q already exists", filepath.Base(dst))
			} else if !os.IsNotExist(err) {
				rollbackRenames(files, out)
				return nil, fmt.Errorf("check destination %q: %w", filepath.Base(dst), err)
			}
			if err := os.Rename(src, dst); err != nil {
				rollbackRenames(files, out)
				return nil, fmt.Errorf("rename %q: %w", filepath.Base(src), err)
			}
		}
		out = append(out, dst)
	}
	return out, nil
}

// RenameForDeliveryEpisodeNumbers renames titles using explicitly identified
// episode numbers keyed by their MakeMKV title index.
func RenameForDeliveryEpisodeNumbers(files []string, titleIndices, episodeNumbers []int) ([]string, error) {
	if len(files) == 0 || len(files) != len(titleIndices) || len(files) != len(episodeNumbers) {
		return nil, fmt.Errorf("files, title indexes, and episode numbers must have the same non-zero length")
	}
	seen := make(map[int]bool, len(episodeNumbers))
	out := make([]string, 0, len(files))
	for i, src := range files {
		episode := episodeNumbers[i]
		if episode < 1 || seen[episode] {
			rollbackRenames(files, out)
			return nil, fmt.Errorf("invalid or duplicate episode number %d", episode)
		}
		seen[episode] = true
		dst := filepath.Join(filepath.Dir(src), fmt.Sprintf("episode-%02d%s", episode, filepath.Ext(src)))
		if dst != src {
			if _, err := os.Stat(dst); err == nil {
				rollbackRenames(files, out)
				return nil, fmt.Errorf("destination %q already exists", filepath.Base(dst))
			} else if !os.IsNotExist(err) {
				rollbackRenames(files, out)
				return nil, fmt.Errorf("check destination %q: %w", filepath.Base(dst), err)
			}
			if err := os.Rename(src, dst); err != nil {
				rollbackRenames(files, out)
				return nil, fmt.Errorf("rename title %d: %w", titleIndices[i], err)
			}
		}
		out = append(out, dst)
	}
	return out, nil
}

// TVEpisodeStart selects a non-conflicting episode range in the show's season
// directory. requested=0 starts after the highest existing episode number.
func TVEpisodeStart(destDir, showTitle string, season, requested, count int) (int, error) {
	if season < 1 {
		return 0, fmt.Errorf("season number must be at least 1")
	}
	if requested < 0 {
		return 0, fmt.Errorf("first episode number cannot be negative")
	}
	if count < 1 {
		return 0, fmt.Errorf("episode count must be at least 1")
	}
	showDir := sanitizeFileName(showTitle)
	if showDir == "" {
		return 0, fmt.Errorf("show title is empty")
	}
	seasonDir := filepath.Join(destDir, TVSeasonDirectory(showTitle, season))
	entries, err := os.ReadDir(seasonDir)
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("read season directory: %w", err)
	}
	used := make(map[int]bool)
	maxEpisode := 0
	for _, entry := range entries {
		name := strings.ToLower(entry.Name())
		if !strings.HasPrefix(name, "episode-") || !strings.HasSuffix(name, ".mkv") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "episode-"), ".mkv"))
		if err == nil && n > 0 {
			used[n] = true
			if n > maxEpisode {
				maxEpisode = n
			}
		}
	}
	if requested == 0 {
		requested = maxEpisode + 1
	}
	for episode := requested; episode < requested+count; episode++ {
		if used[episode] {
			return 0, fmt.Errorf("episode-%02d.mkv already exists in season %d; choose another starting episode", episode, season)
		}
	}
	return requested, nil
}

// ValidateTVEpisodeNumbers rejects episode names that already exist in a
// show's season directory before an inferred mapping is renamed or delivered.
func ValidateTVEpisodeNumbers(destDir, showTitle string, season int, episodes []int) error {
	if season < 1 || len(episodes) == 0 {
		return fmt.Errorf("season and episode numbers are required")
	}
	seasonDir := filepath.Join(destDir, TVSeasonDirectory(showTitle, season))
	entries, err := os.ReadDir(seasonDir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read season directory: %w", err)
	}
	used := make(map[int]bool, len(entries)+len(episodes))
	for _, entry := range entries {
		name := strings.ToLower(entry.Name())
		if !strings.HasPrefix(name, "episode-") || !strings.HasSuffix(name, ".mkv") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "episode-"), ".mkv"))
		if err == nil && n > 0 {
			used[n] = true
		}
	}
	for _, episode := range episodes {
		if episode < 1 || used[episode] {
			return fmt.Errorf("episode-%02d.mkv already exists or is invalid in season %d", episode, season)
		}
		used[episode] = true
	}
	return nil
}

// TVSeasonDirectory returns the sanitized relative directory for a show season.
func TVSeasonDirectory(showTitle string, season int) string {
	return filepath.Join(sanitizeFileName(showTitle), fmt.Sprintf("season-%d", season))
}

func sanitizeRelativePath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("path must be relative")
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path must stay inside the destination")
	}
	parts := strings.Split(clean, "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("invalid path component")
		}
		parts[i] = sanitizeFileName(part)
		if parts[i] == "" {
			return "", fmt.Errorf("empty path component")
		}
	}
	return filepath.Join(parts...), nil
}

func rollbackRenames(orig, renamed []string) {
	for i := range renamed {
		_ = os.Rename(renamed[i], orig[i])
	}
}

// sanitizeFileName strips characters Jellyfin reserves (< > : " / \ | ? *), so
// the folder and file names always match and never trip up SMB shares.
// A colon becomes " -" ("Star Wars: A New Hope" -> "Star Wars - A New Hope").
func sanitizeFileName(s string) string {
	r := strings.NewReplacer(
		"/", "-", "\\", "-", ":", " -",
		"?", "", "*", "", "\"", "", "<", "", ">", "", "|", "", "\x00", "",
	)
	return strings.Join(strings.Fields(r.Replace(s)), " ")
}

// rsync calls the system rsync to copy srcFiles into destDir.
// Uses --checksum so correctness doesn't depend on timestamps.
func rsync(ctx context.Context, srcFiles []string, destDir string) error {
	args := []string{
		"--archive",
		"--checksum",
		"--no-inc-recursive",
	}
	args = append(args, srcFiles...)
	args = append(args, destDir+"/")

	cmd := exec.CommandContext(ctx, "rsync", args...)
	cmd.Stdout = os.Stderr // rsync progress/stats → stderr so stdout stays clean
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("rsync timed out: %w", ctx.Err())
		}
		return fmt.Errorf("rsync: %w", err)
	}
	return nil
}

// verifyFile checks that dst exists and matches src in size.
// A full checksum is too slow for large MKVs; size parity catches truncation.
func verifyFile(src, dst string) error {
	si, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat src: %w", err)
	}
	di, err := os.Stat(dst)
	if err != nil {
		return fmt.Errorf("stat dst: %w", err)
	}
	if si.Size() != di.Size() {
		return fmt.Errorf("size mismatch: src=%d dst=%d", si.Size(), di.Size())
	}
	return nil
}

// writeLog writes rip.json into dir.
func writeLog(dir string, log RipLog) error {
	var f *os.File
	for i := 0; i < 100; i++ {
		name := "rip.json"
		if i > 0 {
			name = fmt.Sprintf("rip-%s-%d.json", log.RippedAt.Format("20060102T150405.000000000Z"), i)
		}
		var err error
		f, err = os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			break
		}
		if !os.IsExist(err) {
			return fmt.Errorf("create %s: %w", name, err)
		}
	}
	if f == nil {
		return fmt.Errorf("create rip log: too many logs already exist in %q", dir)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(log); err != nil {
		return fmt.Errorf("write rip.json: %w", err)
	}
	return nil
}
