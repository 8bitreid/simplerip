package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/8bitreid/simplerip/internal/config"
	"github.com/8bitreid/simplerip/internal/disc"
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
		result, serr = ripper.ScanInfo(context.Background(), "makemkvcon", *device)
		if serr != nil {
			fatal("scan:", serr)
		}
	}

	cl := ripper.ClassifyTitles(result.Titles, cfg.Detection)

	out := struct {
		Pattern string      `json:"pattern"`
		Main    []titleJSON `json:"main"`
		Extras  []titleJSON `json:"extras"`
		Junk    []titleJSON `json:"junk"`
	}{
		Pattern: strings.ToLower(cl.Pattern.String()),
		Main:    toTitleJSON(cl.MainTitles),
		Extras:  toTitleJSON(cl.ExtraTitles),
		Junk:    toTitleJSON(cl.JunkTitles),
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

func fatal(prefix string, err error) {
	fmt.Fprintf(os.Stderr, "simplerip: %s %v\n", prefix, err)
	os.Exit(1)
}
