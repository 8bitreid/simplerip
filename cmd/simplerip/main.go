package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/8bitreid/simplerip/internal/config"
	"github.com/8bitreid/simplerip/internal/disc"
	"github.com/8bitreid/simplerip/internal/inspect"
	"github.com/8bitreid/simplerip/internal/metadata"
	"github.com/8bitreid/simplerip/internal/output"
	"github.com/8bitreid/simplerip/internal/ripper"
	"github.com/8bitreid/simplerip/internal/server"
	"github.com/8bitreid/simplerip/internal/service"
	"github.com/8bitreid/simplerip/internal/store"
)

const version = "0.1.0-dev"

// cfgPath holds the value of the --config persistent flag.
var cfgPath string

func main() {
	configureLogger()
	if err := rootCmd.Execute(); err != nil {
		slog.Error("command failed", "error", err)
		os.Exit(1)
	}
}

func configureLogger() {
	level := slog.LevelInfo
	if raw := strings.TrimSpace(os.Getenv("LOG_LEVEL")); raw != "" {
		switch strings.ToUpper(raw) {
		case "DEBUG":
			level = slog.LevelDebug
		case "INFO":
			level = slog.LevelInfo
		case "WARN":
			level = slog.LevelWarn
		case "ERROR":
			level = slog.LevelError
		}
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgPath, "config", "",
		"config file path (overrides CONFIG_PATH env var, default /config/config.yaml)")
	rootCmd.Flags().String("device", "", "optical device path (e.g. /dev/sr0)")

	scanCmd.Flags().String("device", "", "optical device path (e.g. /dev/sr0)")
	scanCmd.Flags().String("fixture", "",
		"parse a captured makemkvcon output file instead of running makemkvcon")

	ripCmd.Flags().String("device", "", "optical device path (e.g. /dev/sr0)")
	ripCmd.Flags().Int("title", 0, "title index to rip (from scan output)")
	ripCmd.Flags().String("output", "", "directory to write .mkv files into")

	organizeCmd.Flags().String("dir", "", "directory containing MKV files to organize")
	organizeCmd.Flags().String("query", "", "TMDB search query (defaults to directory name)")
	organizeCmd.Flags().String("edition", "", `edition label for alternate cuts (e.g. "Director's Cut")`)
	organizeCmd.Flags().Bool("yes", false, "skip confirmation prompts (TMDB selection only)")
	organizeCmd.Flags().Bool("dry-run", false,
		"show what would happen without moving or renaming anything")

	rootCmd.AddCommand(scanCmd, ripCmd, organizeCmd, serveCmd, versionCmd)
}

// ── root (full rip pipeline) ──────────────────────────────────────────────

var rootCmd = &cobra.Command{
	Use:   "simplerip",
	Short: "Minimal Blu-ray/DVD ripper",
	Long: `simplerip — minimal Blu-ray/DVD ripper

Wraps makemkvcon + ffprobe + rsync for automated disc ripping.
Run without a subcommand to start the full pipeline (scan → rip → deliver → notify).

Environment variables:
  CONFIG_PATH   path to config.yaml (default /config/config.yaml)
  MAKEMKV_KEY   MakeMKV licence key (always overrides config file)`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		device, _ := cmd.Flags().GetString("device")
		if device == "" {
			return cmd.Help()
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		svc := service.New(cfg, nil)
		slog.Info("starting automated rip pipeline", "device", device)
		if err := svc.RipDisc(context.Background(), device); err != nil {
			return fmt.Errorf("rip pipeline: %w", err)
		}
		slog.Info("rip pipeline completed successfully", "device", device)
		return nil
	},
}

// ── scan ──────────────────────────────────────────────────────────────────

var scanCmd = &cobra.Command{
	Use:   "scan",
	Short: "Scan a disc and print title classification as JSON",
	Long: `Scans a disc with makemkvcon, classifies titles (movie/TV/ambiguous),
and prints structured JSON to stdout.

Use --fixture to replay captured makemkvcon output without a physical disc.`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		device, _ := cmd.Flags().GetString("device")
		fixture, _ := cmd.Flags().GetString("fixture")

		if fixture == "" && device == "" {
			return fmt.Errorf("scan: --device or --fixture is required")
		}

		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		svc := service.New(cfg, nil)

		var result *ripper.ClassificationResult
		if fixture != "" {
			deviceLabel := device
			if deviceLabel == "" {
				deviceLabel = fixture
			}
			f, ferr := os.Open(fixture)
			if ferr != nil {
				return fmt.Errorf("scan: fixture: %w", ferr)
			}
			defer f.Close()
			result, err = svc.ScanInfoFromReader(f, deviceLabel)
		} else {
			result, err = svc.ScanDisc(device)
		}
		if err != nil {
			return fmt.Errorf("scan: %w", err)
		}

		if result.MissingMetadata {
			fmt.Fprintln(os.Stderr, "WARNING: Disc metadata is missing or incomplete.")
			fmt.Fprintln(os.Stderr, "         Most titles have zero duration/size/chapters.")
			fmt.Fprintln(os.Stderr, "         This can happen with:")
			fmt.Fprintln(os.Stderr, "         - Heavily encrypted discs")
			fmt.Fprintln(os.Stderr, "         - Drive read errors")
			fmt.Fprintln(os.Stderr, "         - Unsupported disc structures")
			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintln(os.Stderr, "         Try ripping title 0 or 1 manually:")
			if device != "" {
				fmt.Fprintln(os.Stderr, "         simplerip rip --device", device, "--title 0 --output /tmp/test")
			}
			fmt.Fprintln(os.Stderr, "")
		}

		out := struct {
			Pattern         string      `json:"pattern"`
			Main            []titleJSON `json:"main"`
			Extras          []titleJSON `json:"extras"`
			Junk            []titleJSON `json:"junk"`
			MissingMetadata bool        `json:"missing_metadata,omitempty"`
			AllTitles       []titleJSON `json:"all_titles,omitempty"`
			MultiAngle      bool        `json:"multi_angle,omitempty"`
			AngleCount      int         `json:"angle_count,omitempty"`
		}{
			Pattern:         strings.ToLower(result.Pattern.String()),
			Main:            toTitleJSON(result.MainTitles),
			Extras:          toTitleJSON(result.ExtraTitles),
			Junk:            toTitleJSON(result.JunkTitles),
			MissingMetadata: result.MissingMetadata,
			AllTitles:       toTitleJSON(result.AllTitles),
			MultiAngle:      result.MultiAngle,
			AngleCount:      result.AngleCount,
		}

		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	},
}

// ── rip ───────────────────────────────────────────────────────────────────

var ripCmd = &cobra.Command{
	Use:   "rip",
	Short: "Rip a single title from a disc (debug/manual use)",
	Long: `Rips a specific title index from a disc using makemkvcon.

Intended for debugging or manual overrides; the full pipeline runs automatically
via the root command (simplerip --device /dev/sr0).

Exits with code 3 on timeout (distinct from other errors).`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		device, _ := cmd.Flags().GetString("device")
		titleIdx, _ := cmd.Flags().GetInt("title")
		outputDir, _ := cmd.Flags().GetString("output")

		if device == "" || outputDir == "" {
			return fmt.Errorf("rip: --device and --output are required")
		}

		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		title := disc.MKVTitle{Index: titleIdx}
		files, err := ripper.RipTitle(
			context.Background(),
			device,
			title,
			outputDir,
			cfg.MakeMKV.Key,
			cfg.MakeMKV.TimeoutMinutes,
			cfg.MakeMKV.CacheMB,
			cfg.MakeMKV.ReadErrorLimit,
			cfg.MakeMKV.NoProgressMin,
			nil, // No progress callback in CLI mode
		)
		if err != nil {
			if errors.Is(err, ripper.ErrRipTimeout) {
				fmt.Fprintf(os.Stderr, "rip: %v\n", err)
				os.Exit(3) // distinct exit code so callers can retry
			}
			return fmt.Errorf("rip: %w", err)
		}
		for _, f := range files {
			fmt.Println(f)
		}
		return nil
	},
}

// ── version ───────────────────────────────────────────────────────────────

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the HTTP server with WebSocket progress streaming",
	Long: `Starts the HTTP server on the configured port (default 8080).

The server provides:
  - Web UI at /
  - WebSocket progress stream at /ws/progress
  - JSON status endpoint at /api/status

The server will stream real-time progress updates for any active rip jobs.

If optical devices are configured in makemkv.devices, the server will
automatically poll for disc insertion and start ripping when a disc is detected.`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}

		var st *store.Store
		if cfg.Database.URL != "" {
			var stErr error
			st, stErr = store.New(cmd.Context(), cfg.Database.URL)
			if stErr != nil {
				slog.Warn("database unavailable; running without persistence", "error", stErr)
			}
		}

		svc := service.New(cfg, st)
		srv := server.New(svc, st, cfg.MakeMKV.Devices)

		port := cfg.Server.Port
		if port == 0 {
			port = 8080
		}

		// Start disc polling if devices are configured
		if len(cfg.MakeMKV.Devices) > 0 {
			// Use command context so polling stops on cancellation
			ctx := cmd.Context()
			busyDevices := &disc.BusyDeviceTracker{}
			discCh := disc.PollEventsWithBusy(ctx, cfg.MakeMKV.Devices, 5*time.Second, busyDevices.IsBusy)

			// Handle disc insertion/removal events in background.
			go func() {
				for ev := range discCh {
					if !ev.Present {
						// Ignore removals for a device that's mid-rip — the disc
						// hasn't really left; the busy drive just failed a probe.
						if busyDevices.IsBusy(ev.Device) {
							slog.Debug("ignoring disc removal while rip in progress", "device", ev.Device)
							continue
						}
						// Disc removed — reset the drive card to idle.
						slog.Info("disc removed", "device", ev.Device)
						svc.MarkDeviceIdle(ev.Device)
						continue
					}

					// Don't start a second rip on a device that's already ripping.
					if busyDevices.IsBusy(ev.Device) {
						slog.Debug("ignoring disc detection because rip already in progress", "device", ev.Device)
						continue
					}
					busyDevices.MarkBusy(ev.Device)

					slog.Info("disc detected; starting rip", "device", ev.Device)
					go func(dev string) {
						defer busyDevices.MarkIdle(dev)

						// Apply timeout from config to rip job
						timeout := time.Duration(cfg.MakeMKV.TimeoutMinutes) * time.Minute
						if timeout == 0 {
							timeout = 120 * time.Minute // Default 2 hours
						}
						ripCtx, cancel := context.WithTimeout(ctx, timeout)
						defer cancel()

						if err := svc.RipDisc(ripCtx, dev); err != nil {
							slog.Error("rip failed", "device", dev, "error", err)
						}
					}(ev.Device)
				}
			}()

			slog.Info("starting disc polling", "devices", cfg.MakeMKV.Devices, "interval", 5*time.Second)
		}

		slog.Info("starting http server", "port", port)
		slog.Info("web ui ready", "url", fmt.Sprintf("http://localhost:%d/", port))
		slog.Info("websocket ready", "url", fmt.Sprintf("ws://localhost:%d/ws/progress", port))

		if err := srv.Start(port); err != nil {
			return fmt.Errorf("serve: %w", err)
		}

		return nil
	},
}

// ── version ───────────────────────────────────────────────────────────────

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the simplerip version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(version)
	},
}

// ── helpers ───────────────────────────────────────────────────────────────
// loadConfig reads the config file.
// Priority: --config flag > CONFIG_PATH env > /config/config.yaml.
// A missing file returns defaults. Invalid YAML is a fatal error.
func loadConfig() (*config.Config, error) {
	path := cfgPath
	if path == "" {
		path = os.Getenv("CONFIG_PATH")
	}
	if path == "" {
		path = "/config/config.yaml"
	}
	cfg, err := config.Load(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return config.Defaults(), nil
		}
		return nil, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

// titleJSON is the JSON shape for a single disc title.
type titleJSON struct {
	Index        int     `json:"index"`
	Name         string  `json:"name"`
	Duration     string  `json:"duration"`
	ChapterCount int     `json:"chapters"`
	AudioTracks  int     `json:"audio_tracks"`
	SizeGB       float64 `json:"size_gb"`
	SourceFile   string  `json:"source_file,omitempty"`
}

// toTitleJSON converts a slice of disc.MKVTitle to the JSON-friendly form.
// An empty or nil input always marshals as [] rather than null.
func toTitleJSON(titles []disc.MKVTitle) []titleJSON {
	out := make([]titleJSON, len(titles))
	for i, t := range titles {
		out[i] = titleJSON{
			Index:        t.Index,
			Name:         t.Name,
			Duration:     fmtDuration(t.Duration),
			ChapterCount: t.ChapterCount,
			AudioTracks:  t.AudioTrackCount,
			SizeGB:       roundGB(t.SizeGB),
			SourceFile:   t.SourceFileName,
		}
	}
	return out
}

// fmtDuration formats a time.Duration as "H:MM:SS" matching makemkvcon output.
func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%d:%02d:%02d", h, m, s)
}

// roundGB rounds SizeGB to two decimal places for clean JSON output.
func roundGB(gb float64) float64 {
	return float64(int(gb*100+0.5)) / 100
}

// ── organize ──────────────────────────────────────────────────────────────

const organizeErrFmt = "organize: %w"

var organizeCmd = &cobra.Command{
	Use:   "organize",
	Short: "Identify, deduplicate, and rename MKVs to Title (Year) format",
	Long: `Reads an already-ripped directory, removes duplicate MKVs (keeping the
highest audio quality), looks up TMDB/OMDb metadata, and renames files to
"Title (Year)/Title (Year).mkv".`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, _ := cmd.Flags().GetString("dir")
		query, _ := cmd.Flags().GetString("query")
		edition, _ := cmd.Flags().GetString("edition")
		yes, _ := cmd.Flags().GetBool("yes")
		dryRun, _ := cmd.Flags().GetBool("dry-run")

		if dir == "" {
			return fmt.Errorf("organize: --dir is required")
		}

		absDir, err := filepath.Abs(dir)
		if err != nil {
			return fmt.Errorf(organizeErrFmt, err)
		}

		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		if cfg.Metadata.TMDBApiKey == "" {
			return fmt.Errorf("organize: metadata.tmdb_api_key not set in config")
		}
		svc := service.New(cfg, nil)
		ctx := context.Background()
		stdin := bufio.NewScanner(os.Stdin)

		// ── Step 1: flatten extras/ into parent dir ─────────────────────────
		// In dry-run, FlattenSubdirs is read-only: it reports the files it
		// *would* move (in their current subdir locations) so the preview stays
		// faithful without mutating the disk.
		flattened, err := output.FlattenSubdirs(absDir, dryRun)
		if err != nil {
			return fmt.Errorf(organizeErrFmt, err)
		}
		if len(flattened) > 0 {
			verb := "Flattened"
			if dryRun {
				verb = "[dry-run] Would flatten"
			}
			fmt.Fprintf(os.Stderr, "%s %d file(s) from extras/\n", verb, len(flattened))
			for _, f := range flattened {
				fmt.Fprintf(os.Stderr, "  %s\n", filepath.Base(f))
			}
		}

		// ── Step 2: deduplicate ──────────────────────────────────────────────
		// Effective file set = top-level MKVs plus, in dry-run, the extras files
		// that would have been flattened (analysed in place, since they were not
		// actually moved).
		fmt.Fprintf(os.Stderr, "Scanning %s...\n", absDir)
		effectiveMKVs, _ := filepath.Glob(filepath.Join(absDir, "*.mkv"))
		if dryRun {
			effectiveMKVs = append(effectiveMKVs, flattened...)
		}
		analyses, err := output.AnalyzeFiles(ctx, effectiveMKVs)
		if err != nil {
			return fmt.Errorf(organizeErrFmt, err)
		}

		var keptFile string
		if len(analyses) == 0 {
			if len(effectiveMKVs) == 0 {
				return fmt.Errorf("organize: no MKV files found in %q", absDir)
			}
			keptFile = effectiveMKVs[0]
			fmt.Fprintf(os.Stderr, "No duplicates found. File: %s\n", filepath.Base(keptFile))
		} else {
			a := analyses[0]
			fmt.Fprintln(os.Stderr, "\n── Duplicate analysis ───────────────────────────────")
			printFileReport(os.Stderr, "KEEP", a.Keeper)
			for _, dr := range a.Duplicates {
				printFileReport(os.Stderr, "DUPE", dr)
			}
			fmt.Fprintln(os.Stderr, "─────────────────────────────────────────────────────")
			fmt.Fprintf(os.Stderr, "Reason: largest file by size (%s vs", formatBytes(a.Keeper.SizeBytes))
			for i, dr := range a.Duplicates {
				if i > 0 {
					fmt.Fprint(os.Stderr, ",")
				}
				fmt.Fprintf(os.Stderr, " %s", formatBytes(dr.SizeBytes))
			}
			fmt.Fprintln(os.Stderr, ") — all share the same duration")

			if dryRun {
				fmt.Fprintln(os.Stderr, "[dry-run] No files were moved.")
				return nil
			}

			fmt.Fprintf(os.Stderr, "Move %d duplicate(s) to _duplicates/? [y/N]: ", len(a.Duplicates))
			stdin.Scan()
			if strings.ToLower(strings.TrimSpace(stdin.Text())) != "y" {
				fmt.Fprintln(os.Stderr, "Aborted.")
				return nil
			}

			results, err := output.ExecuteDedupe(absDir, analyses)
			if err != nil {
				return fmt.Errorf(organizeErrFmt, err)
			}
			for _, dup := range results[0].Duplicates {
				fmt.Fprintf(os.Stderr, "Moved: %s → _duplicates/\n", filepath.Base(dup))
			}
			keptFile = results[0].Kept
		}

		allMKVs := effectiveMKVs
		keepPaths := []string{keptFile}
		if len(analyses) == 0 {
			keepPaths = append([]string{}, allMKVs...)
		} else {
			deduped := map[string]bool{keptFile: true}
			for _, dr := range analyses[0].Duplicates {
				deduped[dr.Path] = true
			}
			for _, mkv := range allMKVs {
				if !deduped[mkv] {
					keepPaths = append(keepPaths, mkv)
				}
			}
		}

		fmt.Fprintln(os.Stderr, "\nProbing files...")
		var keepers []service.RenameEntry
		for _, p := range keepPaths {
			info, err := inspect.Probe(ctx, p)
			if err != nil {
				return fmt.Errorf("organize: probe %q: %w", filepath.Base(p), err)
			}
			keepers = append(keepers, service.RenameEntry{Path: p, Dur: info.Duration})
			fmt.Fprintf(os.Stderr, "  %s  %d:%02d:%02d\n",
				filepath.Base(p),
				int(info.Duration.Hours()),
				int(info.Duration.Minutes())%60,
				int(info.Duration.Seconds())%60,
			)
		}

		// ── Step 3: TMDB + OMDb lookup ──────────────────────────────────────
		searchQuery := query
		if searchQuery == "" {
			searchQuery = service.QueryFromMKVPath(keptFile)
		}
		fmt.Fprintf(os.Stderr, "\nSearching TMDB for %q...\n", searchQuery)
		movies, err := svc.SearchMovie(ctx, searchQuery)
		if err != nil {
			return fmt.Errorf(organizeErrFmt, err)
		}

		fmt.Fprintln(os.Stderr, "")
		for i, m := range movies {
			fmt.Fprintf(os.Stderr, "  [%d] %s (%s)  — id:%d\n", i+1, m.Title, m.Year(), m.ID)
		}

		var chosen metadata.MovieResult
		var runtimeOverride bool
		var logMsg string
		if yes {
			// Use runtime-based matching with the kept file's duration
			tmdbClient := metadata.NewClient(cfg.Metadata.TMDBApiKey)
			var mainDuration time.Duration
			if len(keepers) > 0 {
				mainDuration = keepers[0].Dur
			}
			chosen, runtimeOverride, logMsg, err = metadata.BestMatch(ctx, tmdbClient, movies, mainDuration)
			if err != nil {
				// Fall back to first result
				chosen = movies[0]
			} else if runtimeOverride && logMsg != "" {
				fmt.Fprintf(os.Stderr, "\n%s\n", logMsg)
			}
			fmt.Fprintf(os.Stderr, "\nAuto-selecting: %s (%s)\n", chosen.Title, chosen.Year())
		} else {
			fmt.Fprint(os.Stderr, "\nSelect [1]: ")
			stdin.Scan()
			input := strings.TrimSpace(stdin.Text())
			idx := 1
			if input != "" {
				if _, err := fmt.Sscanf(input, "%d", &idx); err != nil || idx < 1 || idx > len(movies) {
					return fmt.Errorf("organize: invalid selection %q", input)
				}
			}
			chosen = movies[idx-1]
		}

		fmt.Fprintln(os.Stderr, "\nFetching metadata...")
		details, err := svc.EnrichMovie(ctx, chosen)
		if err != nil {
			return fmt.Errorf(organizeErrFmt, err)
		}

		fmt.Fprintf(os.Stderr, "\n── Runtime cross-reference ──────────────────────────\n")
		fmt.Fprintf(os.Stderr, "  TMDB:       %d min\n", details.TMDbRuntime)
		if details.OMDbRuntime > 0 {
			fmt.Fprintf(os.Stderr, "  OMDb:       %d min\n", details.OMDbRuntime)
		}
		if details.RuntimeConflict {
			fmt.Fprintf(os.Stderr, "  WARNING: sources disagree by >3 min — using longer value\n")
		}
		fmt.Fprintf(os.Stderr, "  Reconciled: %d min (theatrical reference)\n", details.RuntimeMinutes)
		if details.Director != "" {
			fmt.Fprintf(os.Stderr, "  Director:   %s\n", details.Director)
		}
		if details.ImdbRating != "" {
			fmt.Fprintf(os.Stderr, "  IMDb:       %s/10", details.ImdbRating)
			if details.RTRating != "" {
				fmt.Fprintf(os.Stderr, "   RT: %s", details.RTRating)
			}
			fmt.Fprintln(os.Stderr, "")
		}
		fmt.Fprintln(os.Stderr, "─────────────────────────────────────────────────────")

		if dryRun {
			plans := service.PlanRename(keepers, details, edition)
			fmt.Fprintln(os.Stderr, "\n[dry-run] Would rename:")
			for _, p := range plans {
				fmt.Fprintf(os.Stderr, "  %s → %s/%s.mkv\n", filepath.Base(p.Src), p.Folder, p.Base)
			}
			fmt.Fprintln(os.Stderr, "[dry-run] No files were renamed.")
			return nil
		}

		// ── Step 4: rename ──────────────────────────────────────────────────
		// Use NASPath if configured, otherwise stay inside the staging dir.
		// Never use filepath.Dir(absDir) — if absDir is /staging that resolves to /
		// and cross-device renames fail when staging and output are separate mounts.
		baseDir := cfg.Output.NASPath
		if baseDir == "" {
			baseDir = absDir
		}
		plans := service.PlanRename(keepers, details, edition)
		renamed, err := service.ExecuteRename(plans, baseDir)
		if err != nil {
			return fmt.Errorf(organizeErrFmt, err)
		}
		for _, f := range renamed {
			fmt.Printf("%s\n", f)
		}
		return nil
	},
}

// ── helpers ───────────────────────────────────────────────────────────────

func printFileReport(w *os.File, label string, r output.FileReport) {
	h := int(r.Duration.Hours())
	m := int(r.Duration.Minutes()) % 60
	s := int(r.Duration.Seconds()) % 60
	dur := fmt.Sprintf("%d:%02d:%02d", h, m, s)

	fmt.Fprintf(w, "\n  [%s] %s\n", label, filepath.Base(r.Path))
	fmt.Fprintf(w, "        Size:     %s\n", formatBytes(r.SizeBytes))
	fmt.Fprintf(w, "        Duration: %s\n", dur)
	if r.Info != nil {
		fmt.Fprintf(w, "        Video:    %s %s\n", r.Info.VideoCodec, r.Info.Resolution)
		for _, a := range r.Info.Audio {
			fmt.Fprintf(w, "        Audio:    %s\n", a)
		}
		fmt.Fprintf(w, "        Subs:     %d tracks\n", r.Info.Subtitles)
	}
	// Quality score breakdown
	sc := r.Score
	if sc.HasEnglish {
		fmt.Fprintf(w, "        Quality:  score=%d  codec=%d  channels=%d  subs=%d  size=%d\n",
			sc.Total, sc.BestCodecScore, sc.BestChanScore, sc.SubScore, sc.SizeScore)
		fmt.Fprintf(w, "        Decision: %s\n", sc.ScoreLabel())
	} else {
		fmt.Fprintf(w, "        Quality:  DISQUALIFIED (no English audio)\n")
	}
}

func formatBytes(b int64) string {
	const gb = 1024 * 1024 * 1024
	return fmt.Sprintf("%.2f GB", float64(b)/float64(gb))
}
