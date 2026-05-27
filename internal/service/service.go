// Package service implements the core business logic for SimpleRip.
// It orchestrates disc scanning, ripping, cleaning, and notification workflows
// without containing any CLI-specific code.
package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/8bitreid/simplerip/internal/config"
	"github.com/8bitreid/simplerip/internal/inspect"
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
//   1. Scan the disc and classify titles
//   2. Rip main titles + extras (if configured to auto-rip)
//   3. Deliver files to NAS via rsync
//   4. Send Discord notification on completion
//
// This method is intended for unattended daemon mode.
// Returns an error if any step fails.
func (s *RipService) RipDisc(ctx context.Context, device string) error {
	// Step 1: Scan and classify.
	result, err := s.ScanDisc(device)
	if err != nil {
		return err
	}

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

	jobID := fmt.Sprintf("rip-%d", time.Now().Unix())
	ripOutputDir := filepath.Join(stagingDir, jobID)

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
		result, err := output.Deliver(
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
		rippedFiles = result.Files
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
		return fmt.Errorf("send notification (non-fatal): %w", err)
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
	// This is a simplified version — full implementation would require
	// interactive TMDB selection and runtime reconciliation.
	// For now, we just verify the config is usable.
	if s.cfg.Metadata.TMDBApiKey == "" {
		return fmt.Errorf("metadata.tmdb_api_key not configured")
	}

	// Future enhancement: add TMDB search, OMDb cross-reference, and file renaming.
	// Current implementation stops after deduplication to avoid breaking existing CLI.

	return nil
}

// ScanInfoFromReader scans a disc from a captured makemkvcon output fixture.
// This is a testing-only helper that wraps ripper.ScanInfoFromReader.
func (s *RipService) ScanInfoFromReader(r *os.File, deviceLabel string) (*ripper.ClassificationResult, error) {
	scanned, err := ripper.ScanInfoFromReader(r, deviceLabel)
	if err != nil {
		return nil, fmt.Errorf("scan fixture: %w", err)
	}

	result := ripper.ClassifyTitles(scanned.Titles, s.cfg.Detection)
	return &result, nil
}
