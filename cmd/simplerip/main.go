package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/8bitreid/simplerip/internal/config"
	"github.com/8bitreid/simplerip/internal/disc"
	"github.com/8bitreid/simplerip/internal/inspect"
	"github.com/8bitreid/simplerip/internal/metadata"
	"github.com/8bitreid/simplerip/internal/output"
	"github.com/8bitreid/simplerip/internal/ripper"
)

const version = "0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "scan":
		runScan(os.Args[2:])
	case "rip":
		runRip(os.Args[2:])
	case "clean":
		runClean(os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "simplerip: unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `simplerip - minimal Blu-ray/DVD ripper

Usage:
  simplerip scan    -device /dev/sr0
  simplerip scan    -fixture ./testdata/movie.txt
  simplerip rip     -device /dev/sr0 -title 0 -output /tmp/rip
  simplerip clean   -dir /path/to/mkv/folder [-query "search title"] [-yes]
  simplerip version

Environment:
  CONFIG_PATH   path to config.yaml (default /config/config.yaml)
  MAKEMKV_KEY   MakeMKV licence key (always overrides config file)
`)
}

// ── scan ──────────────────────────────────────────────────────────────────

func runScan(args []string) {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	device  := fs.String("device", "", "optical device path (e.g. /dev/sr0)")
	fixture := fs.String("fixture", "", "parse a captured makemkvcon output file instead of running makemkvcon")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: simplerip scan -device /dev/sr0")
		fmt.Fprintln(os.Stderr, "       simplerip scan -fixture ./testdata/movie.txt")
		fs.PrintDefaults()
	}
	fs.Parse(args) //nolint:errcheck // ExitOnError never returns an error

	if *fixture == "" && *device == "" {
		fmt.Fprintln(os.Stderr, "scan: -device or -fixture is required")
		fs.Usage()
		os.Exit(2)
	}

	cfg := loadConfig()

	var result *disc.ClassifiedDisc

	if *fixture != "" {
		// Use the fixture filename as the device label when -device is omitted.
		deviceLabel := *device
		if deviceLabel == "" {
			deviceLabel = *fixture
		}
		f, err := os.Open(*fixture)
		if err != nil {
			fatal("fixture:", err)
		}
		defer f.Close()
		var serr error
		result, serr = ripper.ScanInfoFromReader(f, deviceLabel)
		if serr != nil {
			fatal("scan:", serr)
		}
	} else {
		var serr error
		result, serr = ripper.ScanInfo(context.Background(), "makemkvcon", *device, cfg.MakeMKV.Key)
		if serr != nil {
			fatal("scan:", serr)
		}
	}

	cl := ripper.ClassifyTitles(result.Titles, cfg.Detection)

	// Warn on stderr if metadata is missing (happens with encrypted/unreadable discs).
	if cl.MissingMetadata {
		fmt.Fprintln(os.Stderr, "WARNING: Disc metadata is missing or incomplete.")
		fmt.Fprintln(os.Stderr, "         Most titles have zero duration/size/chapters.")
		fmt.Fprintln(os.Stderr, "         This can happen with:")
		fmt.Fprintln(os.Stderr, "         - Heavily encrypted discs")
		fmt.Fprintln(os.Stderr, "         - Drive read errors")
		fmt.Fprintln(os.Stderr, "         - Unsupported disc structures")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "         Try ripping title 0 or 1 manually:")
		fmt.Fprintln(os.Stderr, "         simplerip rip -device", result.Device, "-title 0 -output /tmp/test")
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
		Pattern:         strings.ToLower(cl.Pattern.String()),
		Main:            toTitleJSON(cl.MainTitles),
		Extras:          toTitleJSON(cl.ExtraTitles),
		Junk:            toTitleJSON(cl.JunkTitles),
		MissingMetadata: cl.MissingMetadata,
		AllTitles:       toTitleJSON(cl.AllTitles),
		MultiAngle:      cl.MultiAngle,
		AngleCount:      cl.AngleCount,
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fatal("json encode:", err)
	}
}

// ── rip ───────────────────────────────────────────────────────────────────

func runRip(args []string) {
	fs := flag.NewFlagSet("rip", flag.ExitOnError)
	device    := fs.String("device", "", "optical device path (e.g. /dev/sr0)")
	titleIdx  := fs.Int("title", 0, "title index to rip (from scan output)")
	outputDir := fs.String("output", "", "directory to write .mkv files into")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: simplerip rip -device /dev/sr0 -title 0 -output /tmp/rip")
		fs.PrintDefaults()
	}
	fs.Parse(args) //nolint:errcheck

	if *device == "" || *outputDir == "" {
		fmt.Fprintln(os.Stderr, "rip: -device and -output are required")
		fs.Usage()
		os.Exit(2)
	}

	cfg := loadConfig()
	title := disc.MKVTitle{Index: *titleIdx}

	files, err := ripper.RipTitle(
		context.Background(),
		*device,
		title,
		*outputDir,
		cfg.MakeMKV.Key,
		cfg.MakeMKV.TimeoutMinutes,
	)
	if err != nil {
		if errors.Is(err, ripper.ErrRipTimeout) {
			fmt.Fprintf(os.Stderr, "rip: %v\n", err)
			os.Exit(3) // distinct exit code so callers can retry
		}
		fatal("rip:", err)
	}

	for _, f := range files {
		fmt.Println(f)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────

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

// loadConfig reads CONFIG_PATH (default /config/config.yaml).
// Missing file → defaults. Invalid YAML → fatal.
func loadConfig() *config.Config {
	path := os.Getenv("CONFIG_PATH")
	if path == "" {
		path = "/config/config.yaml"
	}
	cfg, err := config.Load(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return config.Defaults()
		}
		fatal("config:", err)
	}
	return cfg
}

// ── clean ─────────────────────────────────────────────────────────────────

func runClean(args []string) {
	fs := flag.NewFlagSet("clean", flag.ExitOnError)
	dir     := fs.String("dir", "", "directory containing MKV files to clean")
	query   := fs.String("query", "", "TMDB search query (defaults to directory name)")
	edition := fs.String("edition", "", "edition label for alternate cuts e.g. \"Director's Cut\"")
	yes     := fs.Bool("yes", false, "skip confirmation prompts (TMDB selection only)")
	dryRun  := fs.Bool("dry-run", false, "show what would happen without moving or renaming anything")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: simplerip clean -dir /path/to/folder [-query \"title\"] [-edition \"Director's Cut\"] [-dry-run] [-yes]")
		fs.PrintDefaults()
	}
	fs.Parse(args) //nolint:errcheck

	if *dir == "" {
		fmt.Fprintln(os.Stderr, "clean: -dir is required")
		fs.Usage()
		os.Exit(2)
	}

	absDir, err := filepath.Abs(*dir)
	if err != nil {
		fatal("clean:", err)
	}

	cfg := loadConfig()
	if cfg.Metadata.TMDBApiKey == "" {
		fatal("clean:", fmt.Errorf("metadata.tmdb_api_key not set in config"))
	}

	ctx := context.Background()
	stdin := bufio.NewScanner(os.Stdin)

	// ── Step 1: flatten extras/ into parent dir ─────────────────────────
	if moved, err := output.FlattenSubdirs(absDir); err != nil {
		fatal("clean:", err)
	} else if len(moved) > 0 {
		fmt.Fprintf(os.Stderr, "Flattened %d file(s) from extras/\n", len(moved))
		for _, f := range moved {
			fmt.Fprintf(os.Stderr, "  %s\n", filepath.Base(f))
		}
	}

	// ── Step 2: analyse (no files touched) ─────────────────────────────
	fmt.Fprintf(os.Stderr, "Scanning %s...\n", absDir)
	analyses, err := output.AnalyzeDir(ctx, absDir)
	if err != nil {
		fatal("clean:", err)
	}

	var keptFile string
	if len(analyses) == 0 {
		// No duplicates — find the single MKV.
		matches, _ := filepath.Glob(filepath.Join(absDir, "*.mkv"))
		if len(matches) == 0 {
			fatal("clean:", fmt.Errorf("no MKV files found in %q", absDir))
		}
		keptFile = matches[0]
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

		if *dryRun {
			fmt.Fprintln(os.Stderr, "[dry-run] No files were moved.")
			return
		}

		// Confirm before touching anything.
		fmt.Fprintf(os.Stderr, "Move %d duplicate(s) to _duplicates/? [y/N]: ", len(a.Duplicates))
		stdin.Scan()
		if strings.ToLower(strings.TrimSpace(stdin.Text())) != "y" {
			fmt.Fprintln(os.Stderr, "Aborted.")
			return
		}

		results, err := output.ExecuteDedupe(absDir, analyses)
		if err != nil {
			fatal("clean:", err)
		}
		for _, dup := range results[0].Duplicates {
			fmt.Fprintf(os.Stderr, "Moved: %s → _duplicates/\n", filepath.Base(dup))
		}
		keptFile = results[0].Kept
	}

	// ── Step 2: collect all keeper files and probe their durations ─────────
	allMKVs, _ := filepath.Glob(filepath.Join(absDir, "*.mkv"))
	type mkvFile struct {
		path string
		dur  time.Duration
	}
	// Build set of files to keep: the dedupe winner + any distinct-duration files.
	keepPaths := []string{keptFile}
	if len(analyses) > 0 {
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

	// Probe all keepers now so both dry-run and execution use real durations.
	fmt.Fprintln(os.Stderr, "\nProbing files...")
	var keepers []mkvFile
	for _, p := range keepPaths {
		info, err := inspect.Probe(ctx, p)
		if err != nil {
			fatal("clean:", fmt.Errorf("probe %q: %w", filepath.Base(p), err))
		}
		keepers = append(keepers, mkvFile{path: p, dur: info.Duration})
		fmt.Fprintf(os.Stderr, "  %s  %d:%02d:%02d\n",
			filepath.Base(p),
			int(info.Duration.Hours()),
			int(info.Duration.Minutes())%60,
			int(info.Duration.Seconds())%60,
		)
	}

	// ── Step 3: TMDB + OMDb lookup ──────────────────────────────────────
	searchQuery := *query
	if searchQuery == "" {
		searchQuery = queryFromMKVPath(keptFile)
	}

	tmdbClient := metadata.NewClient(cfg.Metadata.TMDBApiKey)

	// Try progressively shorter queries by dropping the last word until we get results.
	words := strings.Fields(searchQuery)
	var movies []metadata.MovieResult
	var usedQuery string
	for len(words) > 0 {
		q := strings.Join(words, " ")
		fmt.Fprintf(os.Stderr, "\nSearching TMDB for %q...\n", q)
		results, err := tmdbClient.SearchMovie(ctx, q)
		if err != nil {
			fatal("clean:", err)
		}
		if len(results) > 0 {
			movies = results
			usedQuery = q
			break
		}
		fmt.Fprintf(os.Stderr, "  no results — retrying without %q\n", words[len(words)-1])
		words = words[:len(words)-1]
	}
	if len(movies) == 0 {
		fatal("clean:", fmt.Errorf("no TMDB results for %q", usedQuery))
	}
	_ = usedQuery

	fmt.Fprintln(os.Stderr, "")
	for i, m := range movies {
		fmt.Fprintf(os.Stderr, "  [%d] %s (%s)  — id:%d\n", i+1, m.Title, m.Year(), m.ID)
	}

	var chosen metadata.MovieResult
	if *yes {
		chosen = movies[0]
		fmt.Fprintf(os.Stderr, "\nAuto-selecting [1] %s (%s)\n", chosen.Title, chosen.Year())
	} else {
		fmt.Fprint(os.Stderr, "\nSelect [1]: ")
		stdin.Scan()
		input := strings.TrimSpace(stdin.Text())
		idx := 1
		if input != "" {
			if _, err := fmt.Sscanf(input, "%d", &idx); err != nil || idx < 1 || idx > len(movies) {
				fatal("clean:", fmt.Errorf("invalid selection %q", input))
			}
		}
		chosen = movies[idx-1]
	}

	// Enrich with full TMDB detail + OMDb cross-reference.
	var omdbClient *metadata.OMDbClient
	if cfg.Metadata.OMDbApiKey != "" {
		omdbClient = metadata.NewOMDbClient(cfg.Metadata.OMDbApiKey)
	}
	fmt.Fprintln(os.Stderr, "\nFetching metadata...")
	details, err := metadata.Enrich(ctx, tmdbClient, omdbClient, chosen)
	if err != nil {
		fatal("clean:", err)
	}

	// Print runtime report.
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

	if *dryRun {
		ref := time.Duration(details.RuntimeMinutes) * time.Minute
		dryClosestIdx, dryClosestDiff := 0, time.Duration(1<<62)
		for i, k := range keepers {
			d := k.dur - ref
			if d < 0 {
				d = -d
			}
			if d < dryClosestDiff {
				dryClosestDiff, dryClosestIdx = d, i
			}
		}
		altCount := len(keepers) - 1
		fmt.Fprintln(os.Stderr, "\n[dry-run] Would rename:")
		for i, k := range keepers {
			var label string
			if i != dryClosestIdx {
				label = "Alternate"
				if *edition != "" {
					label = *edition
				}
				if altCount > 1 || *edition == "" {
					label = fmt.Sprintf("%s (%dmin)", label, int(k.dur.Minutes()))
				}
			}
			name := details.FolderName()
			if label != "" {
				name += " - " + label
			}
			fmt.Fprintf(os.Stderr, "  %s → %s/%s.mkv\n", filepath.Base(k.path), details.FolderName(), name)
		}
		fmt.Fprintln(os.Stderr, "[dry-run] No files were renamed.")
		return
	}

	// ── Step 4: rename with edition labels ──────────────────────────────
	// Use NASPath if configured, otherwise stay inside the staging dir.
	// Never use filepath.Dir(absDir) — if absDir is /staging that resolves to /
	// and cross-device renames fail when staging and output are separate mounts.
	baseDir := cfg.Output.NASPath
	if baseDir == "" {
		baseDir = absDir
	}
	folderDir := filepath.Join(baseDir, details.FolderName())
	if err := os.MkdirAll(folderDir, 0o755); err != nil {
		fatal("clean:", err)
	}

	// Find the single keeper closest to theatrical runtime — it gets the clean name.
	// All others get an Alternate label to prevent collisions.
	theatricalRef := time.Duration(details.RuntimeMinutes) * time.Minute
	closestIdx, closestDiff := 0, time.Duration(1<<62)
	for i, k := range keepers {
		d := k.dur - theatricalRef
		if d < 0 {
			d = -d
		}
		if d < closestDiff {
			closestDiff, closestIdx = d, i
		}
	}

	// Count alternates for duration-suffix logic.
	alternateCount := 0
	for i, k := range keepers {
		if i == closestIdx {
			continue
		}
		if details.EditionLabel(k.dur) != "" || len(keepers) > 1 {
			alternateCount++
		}
	}

	for i, k := range keepers {
		var label string
		if i != closestIdx {
			// Non-closest files always get an alternate label.
			label = "Alternate"
			if *edition != "" {
				label = *edition
			}
			if alternateCount > 1 || *edition == "" {
				label = fmt.Sprintf("%s (%dmin)", label, int(k.dur.Minutes()))
			}
		}
		baseName := details.FolderName()
		if label != "" {
			baseName += " - " + label
		}
		newFile := filepath.Join(folderDir, baseName+".mkv")
		// Never silently overwrite — fail loudly.
		if _, err := os.Stat(newFile); err == nil {
			fatal("clean:", fmt.Errorf("destination already exists: %s", newFile))
		}
		if err := moveFile(k.path, newFile); err != nil {
			fatal("clean:", err)
		}
		fmt.Printf("%s\n", newFile)
	}

	if err := os.RemoveAll(absDir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove old directory: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "Removed: %s\n", absDir)
	}
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

func fatal(prefix string, err error) {
	fmt.Fprintf(os.Stderr, "simplerip: %s %v\n", prefix, err)
	os.Exit(1)
}

// moveFile moves src to dst, falling back to copy+delete when src and dst are
// on different filesystems (os.Rename returns EXDEV in that case).
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !isExdev(err) {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
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

func isExdev(err error) bool {
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err == syscall.EXDEV
	}
	return false
}

// queryFromMKVPath derives a TMDB search query from a MKV file path.
// makemkvcon names output files like "Revenge of the Sith_t00.mkv"; this
// strips the _tNN suffix and extension before passing through QueryFromDirName.
func queryFromMKVPath(path string) string {
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
