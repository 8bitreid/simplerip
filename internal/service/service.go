// Package service implements the core business logic for SimpleRip.
// It orchestrates disc scanning, ripping, cleaning, and notification workflows
// without containing any CLI-specific code.
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/8bitreid/simplerip/internal/config"
	"github.com/8bitreid/simplerip/internal/inspect"
	"github.com/8bitreid/simplerip/internal/metadata"
	"github.com/8bitreid/simplerip/internal/notify"
	"github.com/8bitreid/simplerip/internal/output"
	"github.com/8bitreid/simplerip/internal/ripper"
)

// RipService orchestrates disc scanning, ripping, and delivery workflows.
// It is stateless and contains no CLI dependencies — all I/O is explicit.
type RipService struct {
	cfg    *config.Config
	notify *notify.Client
}

var (
	newTMDBClient = metadata.NewClient
	newOMDbClient = metadata.NewOMDbClient
)

// New creates a RipService with the given configuration.
// The notification client is initialized from cfg.Notification.WebhookURL.
func New(cfg *config.Config) *RipService {
	return &RipService{
		cfg:    cfg,
		notify: notify.NewClient(cfg.Notification.WebhookURL),
	}
}

// ScanDisc scans a physical disc device and classifies its titles.
// Returns the classification result suitable for decision-making.
// device is the optical drive path (e.g. /dev/sr0).
func (s *RipService) ScanDisc(device string) (*ripper.ClassificationResult, error) {
	ctx := context.Background()

	// Scan the disc with makemkvcon.
	scanned, err := ripper.ScanInfo(ctx, "makemkvcon", device, s.cfg.MakeMKV.Key)
	if err != nil {
		return nil, fmt.Errorf("scan device %q: %w", device, err)
	}

	// Classify titles according to detection rules.
	result := ripper.ClassifyTitles(scanned.Titles, s.cfg.Detection)
	return &result, nil
}

// RipDisc executes the full automated pipeline:
//  1. Scan the disc and classify titles
//  2. Rip main titles + extras (if configured to auto-rip)
//  3. Deliver files to NAS via rsync
//  4. Send Discord notification on completion
//
// This method is intended for unattended daemon mode.
// Returns an error if scan/rip/delivery steps fail. Notification is best-effort.
func (s *RipService) RipDisc(ctx context.Context, device string) error {
	// Step 1: Scan and classify.
	scanned, err := ripper.ScanInfo(ctx, "makemkvcon", device, s.cfg.MakeMKV.Key)
	if err != nil {
		return fmt.Errorf("scan device %q: %w", device, err)
	}
	result := ripper.ClassifyTitles(scanned.Titles, s.cfg.Detection)

	// Step 2: Determine which titles to rip.
	// In daemon mode, we always rip MainTitles immediately.
	// For now, skip extras — future enhancement will integrate Discord callbacks.
	if len(result.MainTitles) == 0 {
		return fmt.Errorf("no main titles found on disc %q", device)
	}

	// Step 3: Rip each main title to staging.
	stagingDir := s.cfg.Output.StagingDir
	if stagingDir == "" {
		stagingDir = "/tmp/simplerip-staging"
	}
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}

	jobID := fmt.Sprintf("rip-%d", time.Now().UnixNano())
	ripOutputDir := filepath.Join(stagingDir, jobID)
	if err := os.MkdirAll(ripOutputDir, 0o755); err != nil {
		return fmt.Errorf("create rip output dir: %w", err)
	}

	var rippedFiles []string
	for _, title := range result.MainTitles {
		files, err := ripper.RipTitle(
			ctx,
			device,
			title,
			ripOutputDir,
			s.cfg.MakeMKV.Key,
			s.cfg.MakeMKV.TimeoutMinutes,
		)
		if err != nil {
			return fmt.Errorf("rip title %d: %w", title.Index, err)
		}
		rippedFiles = append(rippedFiles, files...)
	}

	if len(rippedFiles) == 0 {
		return fmt.Errorf("no files ripped from disc %q", device)
	}

	// Step 4: Deliver to NAS if configured.
	destDir := s.cfg.Output.NASPath
	if destDir != "" {
		// Deliver files to NAS using rsync.
		// Use job ID as subdirectory name (future enhancement: TMDB-enriched names).
		deliverResult, err := output.Deliver(
			ctx,
			rippedFiles,
			ripOutputDir,
			destDir,
			jobID,
			"Ripped Disc", // title placeholder
			device,        // disc name is device path for now
		)
		if err != nil {
			return fmt.Errorf("deliver to NAS: %w", err)
		}
		// Update rippedFiles to point to destination paths for notification.
		rippedFiles = deliverResult.Files
	}

	// Step 5: Send completion notification.
	// Build minimal metadata for Discord.
	var media []notify.MKVMeta
	for _, file := range rippedFiles {
		info, err := inspect.Probe(ctx, file)
		if err != nil {
			// Log warning but continue — notification is best-effort.
			continue
		}
		// Convert AudioTrack slice to string slice.
		var audioTracks []string
		for _, track := range info.Audio {
			audioTracks = append(audioTracks, track.String())
		}
		media = append(media, notify.MKVMeta{
			File:        filepath.Base(file),
			VideoCodec:  info.VideoCodec,
			Resolution:  info.Resolution,
			AudioTracks: audioTracks,
			SizeBytes:   info.SizeBytes,
		})
	}

	payload := notify.RipCompletePayload(
		jobID,
		"Ripped Disc", // title placeholder — future enhancement can use TMDB lookup
		device,
		destDir,
		rippedFiles,
		media,
	)

	if err := s.notify.Send(ctx, payload); err != nil {
		// Notification failure is not fatal — the rip succeeded.
		return nil
	}

	return nil
}

// CleanDir runs the post-rip deduplication and TMDB enrichment workflow.
// It wraps output.Clean logic but remains agnostic to CLI concerns.
//
// Parameters:
//   - dir: absolute path to the directory containing MKV files
//
// Returns an error if the directory cannot be cleaned.
// For interactive prompts (TMDB selection, confirmation), the caller must
// handle user input — this method is designed for programmatic use.
func (s *RipService) CleanDir(dir string) error {
	ctx := context.Background()

	// Step 1: Flatten extras/ subdirectories.
	_, err := output.FlattenSubdirs(dir)
	if err != nil {
		return fmt.Errorf("flatten subdirs: %w", err)
	}

	// Step 2: Analyze for duplicates.
	analyses, err := output.AnalyzeDir(ctx, dir)
	if err != nil {
		return fmt.Errorf("analyze dir: %w", err)
	}

	// Step 3: Execute deduplication if needed.
	if len(analyses) > 0 {
		_, err := output.ExecuteDedupe(dir, analyses)
		if err != nil {
			return fmt.Errorf("dedupe: %w", err)
		}
	}

	// Step 4: TMDB enrichment and renaming.
	// Future enhancement: add TMDB search, OMDb cross-reference, and file renaming.
	// Current implementation intentionally stops after deduplication.

	return nil
}

// RenameEntry is an MKV file paired with its probed duration.
type RenameEntry struct {
	Path string
	Dur  time.Duration
}

// RenamePlan describes a single file rename operation.
type RenamePlan struct {
	Src    string // full source path
	Folder string // destination subfolder name, e.g. "Oppenheimer (2023)"
	Base   string // destination base name without extension
}

// PlanRename computes a rename plan for the given keepers. The keeper whose
// duration is closest to the theatrical reference (details.RuntimeMinutes)
// receives the clean folder name. All others receive an "Alternate" label
// (or the edition name if provided), with a duration suffix appended unless
// exactly one alternate exists and a specific edition name is given.
func PlanRename(keepers []RenameEntry, details *metadata.MovieDetails, edition string) []RenamePlan {
	if details == nil {
		return []RenamePlan{}
	}

	ref := time.Duration(details.RuntimeMinutes) * time.Minute
	closestIdx, closestDiff := 0, time.Duration(1<<62)
	for i, k := range keepers {
		d := k.Dur - ref
		if d < 0 {
			d = -d
		}
		if d < closestDiff {
			closestDiff, closestIdx = d, i
		}
	}

	alternateCount := len(keepers) - 1
	folder := details.FolderName()

	plans := make([]RenamePlan, len(keepers))
	for i, k := range keepers {
		var label string
		if i != closestIdx {
			label = "Alternate"
			if edition != "" {
				label = edition
			}
			if alternateCount > 1 || edition == "" {
				label = fmt.Sprintf("%s (%dmin)", label, int(k.Dur.Minutes()))
			}
		}
		base := folder
		if label != "" {
			base += " - " + label
		}
		plans[i] = RenamePlan{
			Src:    k.Path,
			Folder: folder,
			Base:   base,
		}
	}
	return plans
}

// ExecuteRename creates destination directories and moves each source file to
// baseDir/plan.Folder/plan.Base+".mkv". It returns the destination paths of
// all successfully moved files. If a destination already exists or a move
// fails, execution stops immediately and the error is returned.
func ExecuteRename(plans []RenamePlan, baseDir string) ([]string, error) {
	var renamed []string
	for _, p := range plans {
		destDir := filepath.Join(baseDir, p.Folder)
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return renamed, err
		}
		dst := filepath.Join(destDir, p.Base+".mkv")
		if _, err := os.Stat(dst); err == nil {
			return renamed, fmt.Errorf("destination already exists: %s", dst)
		} else if !os.IsNotExist(err) {
			return renamed, fmt.Errorf("stat destination %s: %w", dst, err)
		}
		if err := MoveFile(p.Src, dst); err != nil {
			return renamed, err
		}
		renamed = append(renamed, dst)
	}
	return renamed, nil
}

// EnrichMovie fetches full TMDB detail + OMDb metadata for the chosen movie
// result. If an OMDb API key is configured, runtimes are cross-referenced and
// reconciled. Returns an error if the TMDB API key is not configured.
func (s *RipService) EnrichMovie(ctx context.Context, chosen metadata.MovieResult) (*metadata.MovieDetails, error) {
	if s.cfg.Metadata.TMDBApiKey == "" {
		return nil, fmt.Errorf("metadata.tmdb_api_key not configured")
	}

	tmdbClient := newTMDBClient(s.cfg.Metadata.TMDBApiKey)

	var omdbClient *metadata.OMDbClient
	if s.cfg.Metadata.OMDbApiKey != "" {
		omdbClient = newOMDbClient(s.cfg.Metadata.OMDbApiKey)
	}

	return metadata.Enrich(ctx, tmdbClient, omdbClient, chosen)
}

// SearchMovie searches TMDB for movies matching query. When the initial query
// returns no results, it retries with progressively shorter queries by dropping
// the last word until results are found or all words are exhausted.
// Returns an error if the TMDB API key is not configured or no results are found.
func (s *RipService) SearchMovie(ctx context.Context, query string) ([]metadata.MovieResult, error) {
	if s.cfg.Metadata.TMDBApiKey == "" {
		return nil, fmt.Errorf("metadata.tmdb_api_key not configured")
	}

	client := newTMDBClient(s.cfg.Metadata.TMDBApiKey)

	words := strings.Fields(query)
	for len(words) > 0 {
		q := strings.Join(words, " ")
		results, err := client.SearchMovie(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("tmdb search %q: %w", q, err)
		}
		if len(results) > 0 {
			return results, nil
		}
		words = words[:len(words)-1]
	}

	return nil, fmt.Errorf("no TMDB results for %q", query)
}

// MoveFile moves src to dst, falling back to a copy+delete when src and dst
// are on different devices (EXDEV). This is necessary when staging and NAS
// are separate mounts.
func MoveFile(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("destination already exists: %s", dst)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat destination %s: %w", dst, err)
	}

	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}

	var linkErr *os.LinkError
	if errors.As(err, &linkErr) && errors.Is(linkErr.Err, syscall.EXDEV) {
		return copyAndRemove(src, dst)
	}

	return err
}

func copyAndRemove(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	return os.Remove(src)
}

// QueryFromMKVPath derives a TMDB search query from a MKV file path.
// makemkvcon names output files like "Revenge of the Sith_t00.mkv"; this
// strips the _tNN suffix and extension before passing through metadata.QueryFromDirName.
func QueryFromMKVPath(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), ".mkv")
	if i := strings.LastIndex(name, "_t"); i != -1 && isAllDigits(name[i+2:]) {
		name = name[:i]
	}
	return metadata.QueryFromDirName(name)
}

func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

// ScanInfoFromReader scans a disc from a captured makemkvcon output fixture.
// It wraps ripper.ScanInfoFromReader and is useful for tests and for --fixture CLI mode.
func (s *RipService) ScanInfoFromReader(r io.Reader, deviceLabel string) (*ripper.ClassificationResult, error) {
	scanned, err := ripper.ScanInfoFromReader(r, deviceLabel)
	if err != nil {
		return nil, fmt.Errorf("scan fixture: %w", err)
	}

	result := ripper.ClassifyTitles(scanned.Titles, s.cfg.Detection)
	return &result, nil
}
