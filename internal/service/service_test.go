package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/8bitreid/simplerip/internal/config"
	"github.com/8bitreid/simplerip/internal/disc"
	"github.com/8bitreid/simplerip/internal/metadata"
	"github.com/8bitreid/simplerip/internal/ripper"
	"github.com/8bitreid/simplerip/internal/store"
)

type hostRewriteTransport struct {
	targets map[string]*url.URL
	base    http.RoundTripper
}

func (t hostRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	u := *req.URL
	if dst, ok := t.targets[req.URL.Host]; ok {
		u.Scheme = dst.Scheme
		u.Host = dst.Host
		clone.URL = &u
		clone.Host = dst.Host
	}
	return t.base.RoundTrip(clone)
}

func installHostRewrites(t *testing.T, targets map[string]string) {
	t.Helper()
	mapped := make(map[string]*url.URL, len(targets))
	for host, raw := range targets {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse rewrite URL %q: %v", raw, err)
		}
		mapped[host] = u
	}
	originalTMDBClientFactory := newTMDBClient
	originalOMDbClientFactory := newOMDbClient
	base := http.DefaultTransport
	newTMDBClient = func(apiKey string) *metadata.Client {
		return metadata.NewClientWithHTTPClient(apiKey, &http.Client{
			Transport: hostRewriteTransport{targets: mapped, base: base},
		})
	}
	newOMDbClient = func(apiKey string) *metadata.OMDbClient {
		return metadata.NewOMDbClientWithHTTPClient(apiKey, &http.Client{
			Transport: hostRewriteTransport{targets: mapped, base: base},
		})
	}
	t.Cleanup(func() {
		newTMDBClient = originalTMDBClientFactory
		newOMDbClient = originalOMDbClientFactory
	})
}

func installFakeMakeMKVCon(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	script := filepath.Join(binDir, "makemkvcon")
	content := `#!/bin/sh
# Handle scan/info command: makemkvcon [--cache=N] -r info dev:/dev/sr0
found_info=0
for arg in "$@"; do
	if [ "$arg" = "info" ]; then found_info=1; break; fi
done
if [ "$found_info" = "1" ]; then
	if [ "$INFO_MODE" = "short" ]; then
		cat <<'EOF'
CINFO:30,0,"TEST_DISC"
TCOUNT:1
TINFO:0,2,0,"Short"
TINFO:0,8,0,"1"
TINFO:0,9,0,"0:01:00"
EOF
		exit 0
	fi
	cat <<'EOF'
CINFO:30,0,"TEST_DISC"
TCOUNT:1
TINFO:0,2,0,"Main Feature"
TINFO:0,8,0,"10"
TINFO:0,9,0,"1:45:00"
SINFO:0,0,1,6202,"Audio"
EOF
	exit 0
fi

# Handle rip command: makemkvcon --cache=N --noscan -r --messages=-stdout --progress=-stdout mkv dev:/dev/sr0 <title> <outdir>
# Check if "mkv" appears anywhere in the arguments
found_mkv=0
for arg in "$@"; do
	if [ "$arg" = "mkv" ]; then
		found_mkv=1
		break
	fi
done

if [ "$found_mkv" = "1" ]; then
	for last; do :; done
	outdir="$last"
	printf 'PRGV:0,0,10000\n'
	printf 'PRGV:5000,5000,10000\n'
	printf 'PRGV:10000,10000,10000\n'
	touch "$outdir/title_t00.mkv"
	exit 0
fi

exit 1
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake makemkvcon: %v", err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
}

func installFakeMakeMKVConFailOnce(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "rip-once-marker")
	script := filepath.Join(binDir, "makemkvcon")
	content := `#!/bin/sh
found_info=0
for arg in "$@"; do
	if [ "$arg" = "info" ]; then found_info=1; break; fi
done
if [ "$found_info" = "1" ]; then
	cat <<'EOF'
CINFO:30,0,"TEST_DISC"
TCOUNT:1
TINFO:0,2,0,"Main Feature"
TINFO:0,8,0,"10"
TINFO:0,9,0,"1:45:00"
SINFO:0,0,1,6202,"Audio"
EOF
	exit 0
fi

found_mkv=0
for arg in "$@"; do
	if [ "$arg" = "mkv" ]; then
		found_mkv=1
		break
	fi
done

if [ "$found_mkv" = "1" ]; then
	if [ ! -f "` + marker + `" ]; then
		touch "` + marker + `"
		printf 'MSG:2003,0,3,"Read error"\n'
		exit 1
	fi
	for last; do :; done
	outdir="$last"
	printf 'PRGV:0,0,10000\n'
	printf 'PRGV:10000,10000,10000\n'
	touch "$outdir/title_t00.mkv"
	exit 0
fi

exit 1
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake makemkvcon fail-once: %v", err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
}

func installFakeFFProbeForClean(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	script := filepath.Join(binDir, "ffprobe")
	content := `#!/bin/sh
file=""
for arg in "$@"; do
	file="$arg"
done
base=$(basename "$file")
case "$base" in
	keeper.mkv)
		cat <<'EOF'
{"format":{"duration":"600","size":"20000000000"},"streams":[{"codec_type":"video","codec_name":"h264","width":1920,"height":1080},{"codec_type":"audio","codec_name":"truehd","channels":8,"channel_layout":"7.1","tags":{"language":"eng"}},{"codec_type":"subtitle","codec_name":"subrip"}]}
EOF
		;;
	dup.mkv)
		cat <<'EOF'
{"format":{"duration":"610","size":"10000000000"},"streams":[{"codec_type":"video","codec_name":"h264","width":1920,"height":1080},{"codec_type":"audio","codec_name":"ac3","channels":6,"channel_layout":"5.1","tags":{"language":"eng"}}]}
EOF
		;;
	*)
		exit 2
		;;
esac
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake ffprobe: %v", err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
}

func TestRipService_ScanDisc_Movie(t *testing.T) {
	cfg := config.Defaults()
	svc := New(cfg, nil)

	// Open the movie fixture.
	fixturePath := filepath.Join("..", "..", "testdata", "movie.txt")
	f, err := os.Open(fixturePath)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	result, err := svc.ScanInfoFromReader(f, "fixture-movie")
	if err != nil {
		t.Fatalf("ScanInfoFromReader: %v", err)
	}

	// Verify the classification pattern.
	if result.Pattern != ripper.DiscPatternMovie {
		t.Errorf("expected pattern Movie, got %s", result.Pattern)
	}

	// Verify we have main titles.
	if len(result.MainTitles) == 0 {
		t.Errorf("expected main titles, got none")
	}

	// Verify no missing metadata warning.
	if result.MissingMetadata {
		t.Errorf("expected metadata to be present, but got MissingMetadata=true")
	}
}

func TestRipService_ScanDisc_TV(t *testing.T) {
	cfg := config.Defaults()
	svc := New(cfg, nil)

	// Open the TV show fixture.
	fixturePath := filepath.Join("..", "..", "testdata", "tvshow.txt")
	f, err := os.Open(fixturePath)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	result, err := svc.ScanInfoFromReader(f, "fixture-tvshow")
	if err != nil {
		t.Fatalf("ScanInfoFromReader: %v", err)
	}

	// Verify the classification pattern.
	if result.Pattern != ripper.DiscPatternTV {
		t.Errorf("expected pattern TV, got %s", result.Pattern)
	}

	// TV mode should rip all main titles automatically.
	if len(result.MainTitles) < 3 {
		t.Errorf("expected at least 3 main titles for TV disc, got %d", len(result.MainTitles))
	}
}

func TestIdentifyTVUsesExplicitEpisodeMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/3/search/tv" || r.URL.Query().Get("query") != "the wire" {
			t.Errorf("unexpected TMDB TV request: %s", r.URL.String())
		}
		_, _ = fmt.Fprint(w, `{"results":[{"id":1438,"name":"The Wire","first_air_date":"2002-06-02"}]}`)
	}))
	defer server.Close()
	installHostRewrites(t, map[string]string{"api.themoviedb.org": server.URL})
	cfg := config.Defaults()
	cfg.Metadata.TMDBApiKey = "tmdb-key"
	svc := New(cfg, nil)

	result := svc.identifyTV(context.Background(), "The-Wire-DISC1", []disc.MKVTitle{
		{Index: 4, Name: "S02E03", Duration: 59 * time.Minute},
		{Index: 1, Name: "S02E01", Duration: 58 * time.Minute},
		{Index: 3, Name: "S02E02", Duration: 60 * time.Minute},
	})
	if !result.ShowCertain || result.Show == nil || result.Show.Title != "The Wire" {
		t.Fatalf("show match = %+v", result)
	}
	if result.Season != 2 || result.Episodes[4] != 3 || result.Episodes[1] != 1 || result.Episodes[3] != 2 {
		t.Fatalf("explicit season/episode mapping = %+v", result)
	}
	if result.LookupError != "" {
		t.Fatalf("unexpected lookup error: %s", result.LookupError)
	}
}

func TestIdentifyTVKeepsWeakCandidateAsSuggestion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"results":[{"id":42,"name":"The Office","first_air_date":"2001-01-01"}]}`)
	}))
	defer server.Close()
	installHostRewrites(t, map[string]string{"api.themoviedb.org": server.URL})
	cfg := config.Defaults()
	cfg.Metadata.TMDBApiKey = "tmdb-key"
	svc := New(cfg, nil)

	result := svc.identifyTV(context.Background(), "Completely Different Disc", []disc.MKVTitle{
		{Index: 0, Name: "Title 0", Duration: 22 * time.Minute},
		{Index: 1, Name: "Title 1", Duration: 22 * time.Minute},
		{Index: 2, Name: "Title 2", Duration: 22 * time.Minute},
	})
	if result.ShowCertain {
		t.Fatalf("weak match must not be selected: %+v", result)
	}
	if result.Show != nil {
		t.Fatalf("weak unrelated candidate should not be surfaced as a show suggestion: %+v", result.Show)
	}
}

func TestIdentifyTVInfersDistinctiveSeasonRuntimes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/search/tv":
			_, _ = fmt.Fprint(w, `{"results":[{"id":51,"name":"Blue Show","first_air_date":"2010-01-01"}]}`)
		case "/3/tv/51":
			_, _ = fmt.Fprint(w, `{"id":51,"name":"Blue Show","seasons":[{"season_number":1},{"season_number":2}]}`)
		case "/3/tv/51/season/1":
			_, _ = fmt.Fprint(w, `{"season_number":1,"episodes":[{"episode_number":1,"runtime":20},{"episode_number":2,"runtime":22},{"episode_number":3,"runtime":24}]}`)
		case "/3/tv/51/season/2":
			_, _ = fmt.Fprint(w, `{"season_number":2,"episodes":[{"episode_number":1,"runtime":42},{"episode_number":2,"runtime":43},{"episode_number":3,"runtime":42}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	installHostRewrites(t, map[string]string{"api.themoviedb.org": server.URL})
	cfg := config.Defaults()
	cfg.Metadata.TMDBApiKey = "tmdb-key"
	svc := New(cfg, nil)

	result := svc.identifyTV(context.Background(), "Blue-Show-DISC2", []disc.MKVTitle{
		{Index: 7, Duration: 24*time.Minute + 5*time.Second},
		{Index: 2, Duration: 20*time.Minute + 3*time.Second},
		{Index: 4, Duration: 22*time.Minute + 2*time.Second},
	})
	if !result.ShowCertain || result.Show == nil || result.Season != 1 {
		t.Fatalf("runtime season match = %+v", result)
	}
	if result.Episodes[7] != 3 || result.Episodes[2] != 1 || result.Episodes[4] != 2 {
		t.Fatalf("runtime episode mapping = %v", result.Episodes)
	}
}

func TestFindManualSelectionTakesPrecedenceOverAutomaticTVEvidence(t *testing.T) {
	now := time.Now()
	events := []store.JobEvent{
		{Stage: "identify", CreatedAt: now, Data: json.RawMessage(`{"action":"tv_identification","suggested_title":"Automatic"}`)},
		{Stage: "identify", CreatedAt: now.Add(time.Second), Data: json.RawMessage(`{"correction":true,"title":"Chosen Show","year":2004,"media_type":"tv","season":3,"episode_start":5}`)},
	}
	selection, ok := findManualSelection(events, now.Add(-time.Minute))
	if !ok || selection.Title != "Chosen Show" || selection.Season != 3 || selection.EpisodeStart != 5 {
		t.Fatalf("manual selection = %+v, ok=%v", selection, ok)
	}
}

func TestRipService_ScanDisc_MissingMetadata(t *testing.T) {
	cfg := config.Defaults()
	svc := New(cfg, nil)

	// Open the missing metadata fixture.
	fixturePath := filepath.Join("..", "..", "testdata", "missing-metadata.txt")
	f, err := os.Open(fixturePath)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	result, err := svc.ScanInfoFromReader(f, "fixture-missing")
	if err != nil {
		t.Fatalf("ScanInfoFromReader: %v", err)
	}

	// Verify missing metadata flag is set.
	if !result.MissingMetadata {
		t.Errorf("expected MissingMetadata=true for missing-metadata fixture")
	}

	// Pattern should be Ambiguous when metadata is missing.
	if result.Pattern != ripper.DiscPatternAmbiguous {
		t.Errorf("expected pattern Ambiguous for missing metadata, got %s", result.Pattern)
	}
}

func TestMoveFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	dst := filepath.Join(dir, "dst.mkv")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MoveFile(src, dst); err != nil {
		t.Fatalf("MoveFile: %v", err)
	}
	assertMovedFile(t, src, dst)
}

func TestMoveFile_DestExists(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	dst := filepath.Join(dir, "dst.mkv")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := MoveFile(src, dst); err == nil {
		t.Fatal("expected error when destination exists")
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("src should remain on failure: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "existing" {
		t.Fatalf("dst content changed: got %q, want %q", got, "existing")
	}
}

func TestCopyAndRemove(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	dst := filepath.Join(dir, "dst.mkv")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyAndRemove(src, dst); err != nil {
		t.Fatalf("copyAndRemove: %v", err)
	}
	assertMovedFile(t, src, dst)

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o644); got != want {
		t.Fatalf("dst mode = %v, want %v", got, want)
	}
}

func TestCopyAndRemove_DestExists(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	dst := filepath.Join(dir, "dst.mkv")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := copyAndRemove(src, dst); err == nil {
		t.Fatal("expected error when destination exists")
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("src should remain on failure: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "existing" {
		t.Fatalf("dst content changed: got %q, want %q", got, "existing")
	}
}

func TestMoveFileCrossDeviceFallback(t *testing.T) {
	srcDir := t.TempDir()
	dstRoot, ok := findCrossDeviceDir(t, srcDir)
	if !ok {
		t.Skip("no writable directory on a different device available")
	}

	dstDir := filepath.Join(dstRoot, "simplerip-test-cross-device")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatalf("mkdir dst dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dstDir) })

	src := filepath.Join(srcDir, "src.mkv")
	dst := filepath.Join(dstDir, "dst.mkv")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	srcDev, _ := deviceID(srcDir)
	dstDev, _ := deviceID(dstDir)
	if srcDev == dstDev {
		t.Skip("source and destination are on the same device")
	}

	if err := MoveFile(src, dst); err != nil {
		t.Fatalf("MoveFile cross-device fallback: %v", err)
	}
	assertMovedFile(t, src, dst)
}

// ── helpers ───────────────────────────────────────────────────────────────

func assertMovedFile(t *testing.T, src, dst string) {
	t.Helper()
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("src should be removed")
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "data" {
		t.Errorf("dst content: got %q, want %q", got, "data")
	}
}

func deviceID(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, syscall.EINVAL
	}
	return uint64(st.Dev), nil
}

func findCrossDeviceDir(t *testing.T, srcDir string) (string, bool) {
	t.Helper()
	srcDev, err := deviceID(srcDir)
	if err != nil {
		t.Fatalf("stat source device: %v", err)
	}
	for _, candidate := range []string{"/dev/shm", "/var/tmp", "/tmp"} {
		info, err := os.Stat(candidate)
		if err != nil || !info.IsDir() {
			continue
		}
		dev, err := deviceID(candidate)
		if err != nil {
			continue
		}
		if dev != srcDev {
			return candidate, true
		}
	}
	return "", false
}

func TestEnrichMovie_NoAPIKey(t *testing.T) {
	cfg := config.Defaults() // TMDBApiKey is empty
	svc := New(cfg, nil)

	_, err := svc.EnrichMovie(context.Background(), metadata.MovieResult{ID: 1, Title: "Oppenheimer"})
	if err == nil {
		t.Fatal("expected error when TMDB API key is not configured, got nil")
	}
}

func TestSearchMovie_NoAPIKey(t *testing.T) {
	cfg := config.Defaults() // TMDBApiKey is empty
	svc := New(cfg, nil)

	_, err := svc.SearchMovie(context.Background(), "Oppenheimer")
	if err == nil {
		t.Fatal("expected error when TMDB API key is not configured, got nil")
	}
}

func TestScanDiscWithFakeMakeMKV(t *testing.T) {
	installFakeMakeMKVCon(t)
	t.Setenv("HOME", t.TempDir())

	cfg := config.Defaults()
	svc := New(cfg, nil)

	result, err := svc.ScanDisc("/dev/sr0")
	if err != nil {
		t.Fatalf("ScanDisc() error = %v", err)
	}
	if result.Pattern != ripper.DiscPatternMovie {
		t.Fatalf("Pattern = %v, want %v", result.Pattern, ripper.DiscPatternMovie)
	}
	if len(result.MainTitles) != 1 {
		t.Fatalf("MainTitles len = %d, want 1", len(result.MainTitles))
	}
}

func TestRipDiscWithFakeMakeMKV(t *testing.T) {
	installFakeMakeMKVCon(t)
	t.Setenv("HOME", t.TempDir())

	cfg := config.Defaults()
	cfg.Output.StagingDir = t.TempDir()
	cfg.Output.NASPath = ""

	svc := New(cfg, nil)
	if err := svc.RipDisc(context.Background(), "/dev/sr0"); err != nil {
		t.Fatalf("RipDisc() error = %v", err)
	}

	matches, _ := filepath.Glob(filepath.Join(cfg.Output.StagingDir, "*", "*.mkv"))
	if len(matches) == 0 {
		t.Fatal("expected at least one ripped mkv file in staging dir")
	}
}

func TestCancelRipEmitsCancelledDriveState(t *testing.T) {
	binDir := t.TempDir()
	script := filepath.Join(binDir, "makemkvcon")
	content := `#!/bin/sh
found_info=0
for arg in "$@"; do
	if [ "$arg" = "info" ]; then found_info=1; break; fi
done
if [ "$found_info" = "1" ]; then
	cat <<'EOF'
CINFO:1,0,"DVD"
CINFO:30,0,"TEST_DISC"
TCOUNT:1
TINFO:0,2,0,"Main Feature"
TINFO:0,8,0,"10"
TINFO:0,9,0,"1:45:00"
SINFO:0,0,1,6202,"Audio"
EOF
	exit 0
fi
exec sleep 60
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake makemkvcon: %v", err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())

	cfg := config.Defaults()
	cfg.Output.StagingDir = t.TempDir()
	svc := New(cfg, nil)
	subID, events := svc.EventBus().Subscribe()
	defer svc.EventBus().Unsubscribe(subID)

	done := make(chan error, 1)
	go func() {
		done <- svc.RipDisc(context.Background(), "/dev/sr0")
	}()

	deadline := time.After(5 * time.Second)
	cancelRequested := false
	for {
		select {
		case ev := <-events:
			if ev.Stage == "analyzing" && !cancelRequested {
				cancelRequested = true
				if !svc.CancelRip("/dev/sr0") {
					t.Fatal("CancelRip returned false for active rip")
				}
			}
			if ev.Stage == "cancelled" {
				if ev.Message != "Rip canceled" {
					t.Fatalf("cancelled event message = %q, want %q", ev.Message, "Rip canceled")
				}
				if !cancelRequested {
					t.Fatal("rip failed before cancellation was requested")
				}
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("RipDisc() error = %v, want context.Canceled", err)
				}
				return
			}
		case err := <-done:
			t.Fatalf("RipDisc() returned before failure event: %v", err)
		case <-deadline:
			t.Fatal("timed out waiting for canceled rip failure event")
		}
	}
}

func TestRipDiscNoMainTitles(t *testing.T) {
	installFakeMakeMKVCon(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("INFO_MODE", "short")

	cfg := config.Defaults()
	cfg.Output.StagingDir = t.TempDir()

	svc := New(cfg, nil)
	err := svc.RipDisc(context.Background(), "/dev/sr0")
	if err == nil || !strings.Contains(err.Error(), "no main titles found") {
		t.Fatalf("RipDisc() error = %v, want no-main-titles error", err)
	}
}

func TestTVDiscSendsInputNeededNotification(t *testing.T) {
	bodies := make(chan []byte, 1)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read webhook body: %v", err)
		}
		bodies <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hook.Close()

	cfg := config.Defaults()
	cfg.Output.StagingDir = t.TempDir()
	cfg.Notification.DiscordWebhookURL = hook.URL
	cfg.Notification.UIURL = "http://ui.test:8080"
	svc := New(cfg, nil)
	defer svc.notifier.Wait(context.Background())
	svc.notifyTVSelection("job-123", "TEST_TV_DISC", "/dev/sr0", "TEST_TV_DISC")

	select {
	case body := <-bodies:
		var payload struct {
			Embeds []struct {
				Title       string `json:"title"`
				Description string `json:"description"`
				URL         string `json:"url"`
			} `json:"embeds"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode notification: %v", err)
		}
		if len(payload.Embeds) != 1 {
			t.Fatalf("embeds = %d, want 1", len(payload.Embeds))
		}
		embed := payload.Embeds[0]
		if embed.Title != "Input needed" || !strings.Contains(embed.Description, "choose a season if known") || embed.URL != cfg.Notification.UIURL {
			t.Fatalf("unexpected TV selection notification: %+v", embed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no TV selection notification received")
	}
}

func TestRipDiscRetriesThenSucceeds(t *testing.T) {
	installFakeMakeMKVConFailOnce(t)

	cfg := config.Defaults()
	cfg.Output.StagingDir = t.TempDir()
	cfg.Output.NASPath = ""
	cfg.MakeMKV.MaxRipRetries = 1

	svc := New(cfg, nil)
	if err := svc.RipDisc(context.Background(), "/dev/sr0"); err != nil {
		t.Fatalf("RipDisc() error = %v", err)
	}

	matches, _ := filepath.Glob(filepath.Join(cfg.Output.StagingDir, "*", "*.mkv"))
	if len(matches) == 0 {
		t.Fatal("expected a ripped mkv file after retry")
	}
}

func TestRipDiscRetryDisabledFails(t *testing.T) {
	installFakeMakeMKVConFailOnce(t)

	cfg := config.Defaults()
	cfg.Output.StagingDir = t.TempDir()
	cfg.Output.NASPath = ""
	cfg.MakeMKV.MaxRipRetries = 0

	svc := New(cfg, nil)
	err := svc.RipDisc(context.Background(), "/dev/sr0")
	if err == nil {
		t.Fatal("expected rip to fail with retries disabled")
	}
}

func TestFindManualCorrection(t *testing.T) {
	now := time.Now().UTC()
	manualPayload, _ := json.Marshal(map[string]any{
		"correction":    true,
		"title":         "Anne of Green Gables",
		"year":          1985,
		"media_type":    "tv",
		"season":        1,
		"episode_start": 4,
	})

	events := []store.JobEvent{
		{Stage: "scan", CreatedAt: now.Add(-2 * time.Minute)},
		{Stage: "identify", CreatedAt: now.Add(-1 * time.Minute), Data: manualPayload},
	}

	title, year, ok := findManualCorrection(events, now.Add(-90*time.Second))
	if !ok {
		t.Fatal("expected manual correction to be detected")
	}
	if title != "Anne of Green Gables" {
		t.Fatalf("title = %q, want %q", title, "Anne of Green Gables")
	}
	if year != 1985 {
		t.Fatalf("year = %d, want %d", year, 1985)
	}
	selection, ok := findManualSelection(events, now.Add(-90*time.Second))
	if !ok || selection.MediaType != "tv" || selection.Season != 1 || selection.EpisodeStart != 4 {
		t.Fatalf("TV selection = %+v, ok=%v", selection, ok)
	}
}

func TestPickLongest(t *testing.T) {
	titles := []disc.MKVTitle{
		{Index: 0, Duration: 95 * time.Minute},
		{Index: 1, Duration: 98 * time.Minute},
		{Index: 2, Duration: 96 * time.Minute},
	}

	got, ok := pickLongest(titles)
	if !ok {
		t.Fatal("expected a title")
	}
	if got.Index != 1 {
		t.Fatalf("picked index = %d, want %d", got.Index, 1)
	}
}

func TestCleanDirNoMKVFiles(t *testing.T) {
	cfg := config.Defaults()
	svc := New(cfg, nil)

	err := svc.CleanDir(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "analyze dir") {
		t.Fatalf("CleanDir() error = %v, want analyze dir error", err)
	}
}

func TestCleanDirSuccess(t *testing.T) {
	installFakeFFProbeForClean(t)

	dir := t.TempDir()
	keeper := filepath.Join(dir, "keeper.mkv")
	dup := filepath.Join(dir, "dup.mkv")
	if err := os.WriteFile(keeper, []byte("keeper"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dup, []byte("dup"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := New(config.Defaults(), nil)
	if err := svc.CleanDir(dir); err != nil {
		t.Fatalf("CleanDir() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "_duplicates", "dup.mkv")); err != nil {
		t.Fatalf("expected duplicate moved to _duplicates: %v", err)
	}
}

func TestSearchMovie_RetryTable(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		responses   map[string]string
		statusByQ   map[string]int
		wantQueries []string
		wantCount   int
		wantErr     string
	}{
		{
			name:  "returns first query results",
			input: "pitch black",
			responses: map[string]string{
				"pitch black": `{"results":[{"id":7,"title":"Pitch Black","release_date":"2000-02-18"}]}`,
			},
			wantQueries: []string{"pitch black"},
			wantCount:   1,
		},
		{
			name:  "drops last word until results",
			input: "the lord rings",
			responses: map[string]string{
				"the": `{"results":[{"id":11,"title":"The","release_date":"2017-01-01"}]}`,
			},
			wantQueries: []string{"the lord rings", "the lord", "the"},
			wantCount:   1,
		},
		{
			name:        "no results after all retries",
			input:       "this matches nothing",
			responses:   map[string]string{},
			wantQueries: []string{"this matches nothing", "this matches", "this"},
			wantErr:     "no TMDB results",
		},
		{
			name:        "returns tmdb status error",
			input:       "broken lookup",
			responses:   map[string]string{},
			statusByQ:   map[string]int{"broken lookup": http.StatusBadGateway},
			wantQueries: []string{"broken lookup"},
			wantErr:     "tmdb search \"broken lookup\"",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			queries := []string{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query().Get("query")
				queries = append(queries, q)
				if got := r.URL.Query().Get("api_key"); got != "tmdb-key" {
					t.Fatalf("api_key = %q, want %q", got, "tmdb-key")
				}
				if got := r.URL.Query().Get("language"); got != "en-US" {
					t.Fatalf("language = %q, want %q", got, "en-US")
				}

				if code, ok := tc.statusByQ[q]; ok {
					w.WriteHeader(code)
					return
				}

				body, ok := tc.responses[q]
				if !ok {
					body = `{"results":[]}`
				}
				_, _ = fmt.Fprint(w, body)
			}))
			defer server.Close()

			installHostRewrites(t, map[string]string{"api.themoviedb.org": server.URL})

			cfg := config.Defaults()
			cfg.Metadata.TMDBApiKey = "tmdb-key"
			svc := New(cfg, nil)

			got, err := svc.SearchMovie(context.Background(), tc.input)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("SearchMovie() error = %v, want substring %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("SearchMovie() error = %v", err)
			}

			if !reflect.DeepEqual(queries, tc.wantQueries) {
				t.Fatalf("queries = %v, want %v", queries, tc.wantQueries)
			}
			if tc.wantErr == "" && len(got) != tc.wantCount {
				t.Fatalf("result count = %d, want %d", len(got), tc.wantCount)
			}
		})
	}
}

func TestSearchMedia_UsesBearerAndRetriesShorterQuery(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/3/search/multi" {
			t.Fatalf("path = %q, want /3/search/multi", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer read-token" {
			t.Fatalf("Authorization = %q, want Bearer read-token", got)
		}
		queries = append(queries, r.URL.Query().Get("query"))
		if len(queries) == 1 {
			_, _ = fmt.Fprint(w, `{"results":[]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"results":[{"id":387,"name":"SpongeBob SquarePants","first_air_date":"1999-05-01","media_type":"tv"}]}`)
	}))
	defer server.Close()
	installHostRewrites(t, map[string]string{"api.themoviedb.org": server.URL})

	cfg := config.Defaults()
	cfg.Metadata.TMDBAccessToken = "read-token"
	svc := New(cfg, nil)
	got, err := svc.SearchMedia(context.Background(), "spongebob squarepants")
	if err != nil {
		t.Fatalf("SearchMedia() error = %v", err)
	}
	if !reflect.DeepEqual(queries, []string{"spongebob squarepants", "spongebob"}) {
		t.Fatalf("queries = %v", queries)
	}
	if len(got) != 1 || got[0].MediaType != "tv" || got[0].Year != "1999" {
		t.Fatalf("SearchMedia() = %+v", got)
	}
}

func TestEnrichMovie_Table(t *testing.T) {
	tests := []struct {
		name            string
		tmdbBody        string
		tmdbStatus      int
		omdbBody        string
		omdbStatus      int
		omdbKey         string
		wantErr         string
		wantRuntime     int
		wantConflict    bool
		wantDirectorSet bool
	}{
		{
			name:            "tmdb only no omdb key",
			tmdbBody:        `{"id":7,"title":"Pitch Black","runtime":109,"imdb_id":"tt0134847"}`,
			wantRuntime:     109,
			wantConflict:    false,
			wantDirectorSet: false,
		},
		{
			name:            "omdb success averages runtime",
			tmdbBody:        `{"id":7,"title":"Pitch Black","runtime":100,"imdb_id":"tt0134847"}`,
			omdbBody:        `{"Director":"David Twohy","Runtime":"102 min","Response":"True"}`,
			omdbKey:         "omdb-key",
			wantRuntime:     101,
			wantConflict:    false,
			wantDirectorSet: true,
		},
		{
			name:            "omdb false response non fatal",
			tmdbBody:        `{"id":7,"title":"Pitch Black","runtime":109,"imdb_id":"tt0134847"}`,
			omdbBody:        `{"Response":"False","Error":"Movie not found"}`,
			omdbKey:         "omdb-key",
			wantRuntime:     109,
			wantConflict:    false,
			wantDirectorSet: false,
		},
		{
			name:         "tmdb status error bubbles up",
			tmdbStatus:   http.StatusBadGateway,
			wantErr:      "tmdb detail",
			wantRuntime:  0,
			wantConflict: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmdbServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.tmdbStatus != 0 {
					w.WriteHeader(tc.tmdbStatus)
					return
				}
				_, _ = fmt.Fprint(w, tc.tmdbBody)
			}))
			defer tmdbServer.Close()

			omdbServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.omdbStatus != 0 {
					w.WriteHeader(tc.omdbStatus)
					return
				}
				if tc.omdbBody == "" {
					_, _ = fmt.Fprint(w, `{"Response":"False","Error":"disabled"}`)
					return
				}
				_, _ = fmt.Fprint(w, tc.omdbBody)
			}))
			defer omdbServer.Close()

			installHostRewrites(t, map[string]string{
				"api.themoviedb.org": tmdbServer.URL,
				"www.omdbapi.com":    omdbServer.URL,
			})

			cfg := config.Defaults()
			cfg.Metadata.TMDBApiKey = "tmdb-key"
			cfg.Metadata.OMDbApiKey = tc.omdbKey
			svc := New(cfg, nil)

			got, err := svc.EnrichMovie(context.Background(), metadata.MovieResult{ID: 7, ReleaseDate: "2000-02-18"})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("EnrichMovie() error = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("EnrichMovie() error = %v", err)
			}
			if got.RuntimeMinutes != tc.wantRuntime {
				t.Fatalf("RuntimeMinutes = %d, want %d", got.RuntimeMinutes, tc.wantRuntime)
			}
			if got.RuntimeConflict != tc.wantConflict {
				t.Fatalf("RuntimeConflict = %v, want %v", got.RuntimeConflict, tc.wantConflict)
			}
			hasDirector := got.Director != ""
			if hasDirector != tc.wantDirectorSet {
				t.Fatalf("Director set = %v, want %v", hasDirector, tc.wantDirectorSet)
			}
		})
	}
}

func TestPlanRename_Single(t *testing.T) {
	details := &metadata.MovieDetails{
		Title:          "Oppenheimer",
		Year:           "2023",
		RuntimeMinutes: 180,
	}
	keepers := []RenameEntry{
		{Path: "/staging/oppenheimer.mkv", Dur: 180 * time.Minute},
	}
	plans := PlanRename(keepers, details, "")
	if len(plans) != 1 {
		t.Fatalf("expected 1 plan, got %d", len(plans))
	}
	if plans[0].Base != "Oppenheimer (2023)" {
		t.Errorf("single keeper: Base = %q, want %q", plans[0].Base, "Oppenheimer (2023)")
	}
	if plans[0].Folder != "Oppenheimer (2023)" {
		t.Errorf("single keeper: Folder = %q, want %q", plans[0].Folder, "Oppenheimer (2023)")
	}
}

func TestPlanRename_NilDetails(t *testing.T) {
	keepers := []RenameEntry{
		{Path: "/staging/oppenheimer.mkv", Dur: 180 * time.Minute},
	}
	plans := PlanRename(keepers, nil, "")
	if len(plans) != 0 {
		t.Fatalf("expected 0 plans for nil details, got %d", len(plans))
	}
}

func TestPlanRename_TwoKeepers_NoEdition(t *testing.T) {
	details := &metadata.MovieDetails{
		Title:          "Blade Runner",
		Year:           "1982",
		RuntimeMinutes: 117,
	}
	keepers := []RenameEntry{
		{Path: "/staging/theatrical.mkv", Dur: 117 * time.Minute},
		{Path: "/staging/directors.mkv", Dur: 134 * time.Minute},
	}
	plans := PlanRename(keepers, details, "")
	if len(plans) != 2 {
		t.Fatalf("expected 2 plans, got %d", len(plans))
	}
	// First is closest to theatrical — clean name.
	if plans[0].Base != "Blade Runner (1982)" {
		t.Errorf("theatrical: Base = %q, want %q", plans[0].Base, "Blade Runner (1982)")
	}
	// Second is alternate — must have duration suffix (only 1 alternate, but no edition given).
	wantAlt := "Blade Runner (1982) - Alternate (134min)"
	if plans[1].Base != wantAlt {
		t.Errorf("alternate: Base = %q, want %q", plans[1].Base, wantAlt)
	}
}

func TestPlanRename_TwoKeepers_WithEdition(t *testing.T) {
	details := &metadata.MovieDetails{
		Title:          "Blade Runner",
		Year:           "1982",
		RuntimeMinutes: 117,
	}
	keepers := []RenameEntry{
		{Path: "/staging/theatrical.mkv", Dur: 117 * time.Minute},
		{Path: "/staging/directors.mkv", Dur: 134 * time.Minute},
	}
	// Single alternate + edition name → no duration suffix.
	plans := PlanRename(keepers, details, "Director's Cut")
	wantAlt := "Blade Runner (1982) - Director's Cut"
	if plans[1].Base != wantAlt {
		t.Errorf("alternate with edition: Base = %q, want %q", plans[1].Base, wantAlt)
	}
}

func TestPlanRename_ThreeKeepers_WithEdition(t *testing.T) {
	details := &metadata.MovieDetails{
		Title:          "Blade Runner",
		Year:           "1982",
		RuntimeMinutes: 117,
	}
	keepers := []RenameEntry{
		{Path: "/staging/theatrical.mkv", Dur: 117 * time.Minute},
		{Path: "/staging/directors.mkv", Dur: 134 * time.Minute},
		{Path: "/staging/final.mkv", Dur: 116 * time.Minute},
	}
	// Multiple alternates + edition → duration suffix still appended.
	plans := PlanRename(keepers, details, "Director's Cut")
	if plans[0].Base != "Blade Runner (1982)" {
		t.Errorf("theatrical: Base = %q, want %q", plans[0].Base, "Blade Runner (1982)")
	}
	wantAlt1 := "Blade Runner (1982) - Director's Cut (134min)"
	if plans[1].Base != wantAlt1 {
		t.Errorf("alternate 1: Base = %q, want %q", plans[1].Base, wantAlt1)
	}
	wantAlt2 := "Blade Runner (1982) - Director's Cut (116min)"
	if plans[2].Base != wantAlt2 {
		t.Errorf("alternate 2: Base = %q, want %q", plans[2].Base, wantAlt2)
	}
}

func TestExecuteRename(t *testing.T) {
	dir := t.TempDir()

	// Create two source files.
	src1 := filepath.Join(dir, "theatrical.mkv")
	src2 := filepath.Join(dir, "directors.mkv")
	if err := os.WriteFile(src1, []byte("theatrical"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src2, []byte("directors"), 0o644); err != nil {
		t.Fatal(err)
	}

	plans := []RenamePlan{
		{Src: src1, Folder: "Blade Runner (1982)", Base: "Blade Runner (1982)"},
		{Src: src2, Folder: "Blade Runner (1982)", Base: "Blade Runner (1982) - Alternate (134min)"},
	}

	renamed, err := ExecuteRename(plans, dir)
	if err != nil {
		t.Fatalf("ExecuteRename: %v", err)
	}
	if len(renamed) != 2 {
		t.Fatalf("expected 2 renamed paths, got %d", len(renamed))
	}

	// Verify sources are gone and destinations exist with correct content.
	for i, want := range []string{"theatrical", "directors"} {
		got, err := os.ReadFile(renamed[i])
		if err != nil {
			t.Errorf("read renamed[%d]: %v", i, err)
			continue
		}
		if string(got) != want {
			t.Errorf("renamed[%d] content = %q, want %q", i, got, want)
		}
	}
	if _, err := os.Stat(src1); !os.IsNotExist(err) {
		t.Error("src1 should have been removed")
	}
	if _, err := os.Stat(src2); !os.IsNotExist(err) {
		t.Error("src2 should have been removed")
	}
}

func TestExecuteRename_DestExists(t *testing.T) {
	dir := t.TempDir()

	src := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pre-create the destination.
	destDir := filepath.Join(dir, "Movie (2024)")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(destDir, "Movie (2024).mkv")
	if err := os.WriteFile(dst, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}

	plans := []RenamePlan{
		{Src: src, Folder: "Movie (2024)", Base: "Movie (2024)"},
	}
	_, err := ExecuteRename(plans, dir)
	if err == nil {
		t.Fatal("expected error for pre-existing destination, got nil")
	}
}

func TestExecuteRename_DestStatError(t *testing.T) {
	dir := t.TempDir()

	src := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	plans := []RenamePlan{
		{Src: src, Folder: "Movie (2024)", Base: "Movie\x00(2024)"},
	}
	_, err := ExecuteRename(plans, dir)
	if err == nil {
		t.Fatal("expected stat error for invalid destination path, got nil")
	}
}

func TestQueryFromMKVPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// makemkvcon _tNN suffix stripped, underscores → spaces
		{"Revenge_of_the_Sith_t00.mkv", "Revenge of the Sith"},
		{"The_Dark_Knight_t01.mkv", "The Dark Knight"},
		// no suffix — still cleans the name
		{"Star-Wars--Episode-I.mkv", "Star Wars Episode I"},
		// already clean title with extension
		{"Oppenheimer.mkv", "Oppenheimer"},
		// full path, not just filename
		{"/staging/rip-123/Inception_t02.mkv", "Inception"},
	}

	for _, tc := range cases {
		got := QueryFromMKVPath(tc.in)
		if got != tc.want {
			t.Errorf("QueryFromMKVPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNew(t *testing.T) {
	cfg := config.Defaults()
	svc := New(cfg, nil)

	if svc == nil {
		t.Fatal("New returned nil")
	}
	if svc.cfg == nil {
		t.Error("service cfg is nil")
	}
	if svc.notify == nil {
		t.Error("service notify client is nil")
	}
}

func TestDurationMismatch(t *testing.T) {
	cases := []struct {
		name    string
		actual  time.Duration
		runtime int
		want    bool
	}{
		{"within tolerance", 89 * time.Minute, 87, false},
		{"exactly tolerance", 90 * time.Minute, 87, false},
		{"longer cut", 134 * time.Minute, 117, true},
		{"shorter", 60 * time.Minute, 90, true},
		{"unknown runtime", 90 * time.Minute, 0, false},
		{"unknown duration", 0, 90, false},
	}
	for _, tc := range cases {
		if got := durationMismatch(tc.actual, tc.runtime); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPickByRuntime(t *testing.T) {
	titles := []disc.MKVTitle{
		{Index: 0, Duration: 44 * time.Minute},
		{Index: 1, Duration: 88 * time.Minute},
		{Index: 2, Duration: 91 * time.Minute},
	}
	if got, ok := pickByRuntime(titles, 89); !ok || got.Index != 1 {
		t.Fatalf("89 min: got %+v ok=%v, want title 1", got, ok)
	}
	if got, ok := pickByRuntime(titles, 92); !ok || got.Index != 2 {
		t.Fatalf("92 min: got %+v ok=%v, want title 2", got, ok)
	}
	if _, ok := pickByRuntime(titles, 120); ok {
		t.Fatal("120 min: nothing within tolerance, want no match")
	}
	if _, ok := pickByRuntime(titles, 0); ok {
		t.Fatal("unknown runtime must not match")
	}
}

func TestCanReselect(t *testing.T) {
	if !canReselect(ripper.ClassificationResult{Pattern: ripper.DiscPatternMovie}) {
		t.Error("movie should allow reselect")
	}
	if !canReselect(ripper.ClassificationResult{Pattern: ripper.DiscPatternAmbiguous}) {
		t.Error("ambiguous should allow reselect")
	}
	if canReselect(ripper.ClassificationResult{Pattern: ripper.DiscPatternTV}) {
		t.Error("TV must not reselect")
	}
	if canReselect(ripper.ClassificationResult{Pattern: ripper.DiscPatternMovie, MultiAngle: true}) {
		t.Error("multi-angle must not reselect")
	}
	if canReselect(ripper.ClassificationResult{Pattern: ripper.DiscPatternMovie, MissingMetadata: true}) {
		t.Error("missing metadata must not reselect")
	}
}

func installFakeMakeMKVConTwoTitles(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	content := `#!/bin/sh
for arg in "$@"; do
	if [ "$arg" = "info" ]; then
		cat <<'EOF2'
CINFO:30,0,"TEST_DISC"
TCOUNT:2
TINFO:0,8,0,"10"
TINFO:0,9,0,"1:45:00"
TINFO:1,8,0,"4"
TINFO:1,9,0,"0:20:00"
EOF2
		exit 0
	fi
done
# rip: mkv dev:/dev/sr0 <title> <outdir>
for last; do :; done
outdir="$last"
set -- "$@"
shift $(($# - 2))
title="$1"
if [ "$title" = "0" ]; then
	exec sleep 60
fi
printf 'PRGV:10000,10000,10000\n'
touch "$outdir/title_t0$title.mkv"
`
	if err := os.WriteFile(filepath.Join(binDir, "makemkvcon"), []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
}

func TestEditTitleMidRipRestartsWithDifferentTitle(t *testing.T) {
	installFakeMakeMKVConTwoTitles(t)
	t.Setenv("HOME", t.TempDir())

	cfg := config.Defaults()
	cfg.Output.StagingDir = t.TempDir()
	cfg.Output.NASPath = ""
	svc := New(cfg, nil)

	const dev = "/dev/sr0"
	done := make(chan error, 1)
	go func() { done <- svc.RipDisc(context.Background(), dev) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		svc.ripMu.Lock()
		_, ok := svc.restarts[dev]
		svc.ripMu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rip never reached the ripping stage")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !svc.ReidentifyRip(dev, "Short Film (2000)") {
		t.Fatal("re-identify rejected")
	}
	svc.SetRipRuntime(dev, 20)
	if !svc.RestartIfNeeded(dev) {
		t.Fatal("expected a restart: 20 min matches title 1, not the title being ripped")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RipDisc() error = %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("RipDisc did not finish after restart")
	}

	matches, _ := filepath.Glob(filepath.Join(cfg.Output.StagingDir, "*", "*.mkv"))
	if len(matches) != 1 || filepath.Base(matches[0]) != "title_t01.mkv" {
		t.Fatalf("staged files = %v, want only title_t01.mkv", matches)
	}
	dirs, _ := filepath.Glob(filepath.Join(cfg.Output.StagingDir, "rip-*"))
	if len(dirs) != 1 {
		t.Fatalf("staging dirs = %v, want the first attempt's dir removed", dirs)
	}
}

func TestRestartIfNeededRules(t *testing.T) {
	titles := []disc.MKVTitle{
		{Index: 0, Duration: 105 * time.Minute},
		{Index: 1, Duration: 20 * time.Minute},
	}
	setup := func(reselect bool) (*RipService, *int) {
		s := New(&config.Config{}, nil)
		s.beginRipTitle("/dev/sr0", "Disc")
		cancels := 0
		s.restarts["/dev/sr0"] = &restartState{
			cancel:   func(error) { cancels++ },
			all:      titles,
			selected: titles[:1],
			reselect: reselect,
		}
		return s, &cancels
	}

	s, cancels := setup(true)
	s.SetRipRuntime("/dev/sr0", 105)
	if s.RestartIfNeeded("/dev/sr0") || *cancels != 0 {
		t.Fatal("same title chosen: must not restart")
	}
	s.SetRipRuntime("/dev/sr0", 20)
	if !s.RestartIfNeeded("/dev/sr0") || *cancels != 1 {
		t.Fatal("different title: must restart")
	}
	if s.RestartIfNeeded("/dev/sr0") || *cancels != 1 {
		t.Fatal("second edit must not cancel twice")
	}
	if s.claimDelivery("/dev/sr0") {
		t.Fatal("delivery must not proceed once a restart is requested")
	}

	s, _ = setup(false)
	s.SetRipRuntime("/dev/sr0", 20)
	if s.RestartIfNeeded("/dev/sr0") {
		t.Fatal("non-reselectable disc (TV/multi-angle) must not restart")
	}

	s, _ = setup(true)
	s.SetRipRuntime("/dev/sr0", 20)
	s.freezeTitle("/dev/sr0", "x")
	if s.RestartIfNeeded("/dev/sr0") {
		t.Fatal("must not restart after delivery has started")
	}

	s, _ = setup(true)
	if !s.claimDelivery("/dev/sr0") {
		t.Fatal("claimDelivery should succeed with no restart pending")
	}
	s.SetRipRuntime("/dev/sr0", 20)
	if s.RestartIfNeeded("/dev/sr0") {
		t.Fatal("must not restart once delivery is claimed")
	}
}

// TestMain strips notification env vars so no test can reach a real webhook.
func TestMain(m *testing.M) {
	os.Unsetenv("DISCORD_WEBHOOK_URL")
	os.Unsetenv("SIMPLERIP_UI_URL")
	os.Exit(m.Run())
}

func TestRipCompleteNotificationBody(t *testing.T) {
	installFakeMakeMKVCon(t)
	t.Setenv("HOME", t.TempDir())

	bodies := make(chan []byte, 4)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- b
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hook.Close()

	cfg := config.Defaults()
	cfg.Output.StagingDir = t.TempDir()
	cfg.Output.NASPath = t.TempDir()
	cfg.Notification.DiscordWebhookURL = hook.URL
	cfg.Notification.UIURL = "http://ui.test:8080"

	svc := New(cfg, nil)
	if err := svc.RipDisc(context.Background(), "/dev/sr0"); err != nil {
		t.Fatalf("RipDisc() error = %v", err)
	}

	select {
	case b := <-bodies:
		var p struct {
			Embeds []struct {
				Title, URL string
				Fields     []struct{ Name, Value string }
			}
		}
		if err := json.Unmarshal(b, &p); err != nil || len(p.Embeds) != 1 {
			t.Fatalf("bad payload %s: %v", b, err)
		}
		e := p.Embeds[0]
		if e.Title != "Rip complete" || e.URL != "http://ui.test:8080" {
			t.Errorf("embed = %+v", e)
		}
		got := map[string]string{}
		for _, f := range e.Fields {
			got[f.Name] = f.Value
		}
		if !strings.Contains(got["Disc"], "TEST_DISC") || !strings.Contains(got["Device"], "/dev/sr0") {
			t.Errorf("fields = %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notification received")
	}
}

func TestEditMessage(t *testing.T) {
	titles := []disc.MKVTitle{{Index: 11, Duration: 107 * time.Minute}, {Index: 3, Duration: 20 * time.Minute}}
	s := New(&config.Config{}, nil)
	s.beginRipTitle("/dev/sr0", "Disc")
	if got := s.EditMessage("/dev/sr0", false); !strings.Contains(got, "Saved") {
		t.Errorf("not ripping: %q", got)
	}
	s.restarts["/dev/sr0"] = &restartState{cancel: func(error) {}, all: titles, selected: titles[:1], reselect: true}
	s.SetRipRuntime("/dev/sr0", 96)
	if got := s.EditMessage("/dev/sr0", false); !strings.Contains(got, "no title is within 3 min") || !strings.Contains(got, "title 11") {
		t.Errorf("no match: %q", got)
	}
	s.SetRipRuntime("/dev/sr0", 107)
	if got := s.EditMessage("/dev/sr0", false); !strings.Contains(got, "already matches") {
		t.Errorf("same: %q", got)
	}
	s.SetRipRuntime("/dev/sr0", 20)
	if !s.RestartIfNeeded("/dev/sr0") {
		t.Fatal("want restart")
	}
	if got := s.EditMessage("/dev/sr0", true); !strings.Contains(got, "Restarting: ripping title 3") {
		t.Errorf("restart: %q", got)
	}
}

func tracks(audio ...disc.Track) []disc.Track {
	return append([]disc.Track{{Type: "Video", Resolution: "1920x1080"}}, audio...)
}

func TestPickBestNearScoresFormat(t *testing.T) {
	ac3 := disc.Track{Type: "Audio", CodecID: "A_AC3", Channels: 6, Language: "eng"}
	dtsma := disc.Track{Type: "Audio", CodecID: "A_DTS", CodecDesc: "DTS-HD Master Audio", Channels: 8, Language: "eng"}
	french := disc.Track{Type: "Audio", CodecID: "A_TRUEHD", Channels: 8, Language: "fra"}
	titles := []disc.MKVTitle{
		{Index: 0, Duration: 107 * time.Minute, Tracks: tracks(ac3, ac3, ac3, ac3), AudioTrackCount: 4, SizeGB: 5},
		{Index: 1, Duration: 106 * time.Minute, Tracks: tracks(dtsma), AudioTrackCount: 1, SizeGB: 5},
		{Index: 2, Duration: 107 * time.Minute, Tracks: tracks(french), AudioTrackCount: 1, SizeGB: 9},
		{Index: 3, Duration: 140 * time.Minute, Tracks: tracks(dtsma), AudioTrackCount: 1, SizeGB: 9},
	}
	got, ok := pickBestNear(titles, 107*time.Minute)
	if !ok || got.Index != 1 {
		t.Fatalf("got %+v ok=%v, want title 1 (one DTS-HD MA track beats four AC3; French-only disqualified; 140m out of range)", got, ok)
	}
}

func TestPickBestNearSubtitlesBreakTie(t *testing.T) {
	a := disc.Track{Type: "Audio", CodecID: "A_AC3", Channels: 6, Language: "eng"}
	sub := disc.Track{Type: "Subtitles", Language: "eng"}
	titles := []disc.MKVTitle{
		{Index: 0, Duration: 100 * time.Minute, Tracks: tracks(a)},
		{Index: 1, Duration: 101 * time.Minute, Tracks: tracks(a, sub)},
	}
	if got, _ := pickBestNear(titles, 100*time.Minute); got.Index != 1 {
		t.Fatalf("got title %d, want 1 (English subtitles)", got.Index)
	}
}

func TestFindAlternates(t *testing.T) {
	m := func(i, min int) disc.MKVTitle {
		return disc.MKVTitle{Index: i, Duration: time.Duration(min) * time.Minute}
	}
	main := m(11, 107)
	all := []disc.MKVTitle{main, m(1, 108), m(2, 120), m(3, 95), m(4, 45), m(5, 200), m(6, 10)}
	got := findAlternates(all, main, 40*time.Minute)
	var idx []int
	for _, g := range got {
		idx = append(idx, g.Index)
	}
	// 108m is the same cut, 45m and 200m are outside the plausible range, 10m is an extra.
	if !reflect.DeepEqual(idx, []int{3, 2}) {
		t.Fatalf("alternates = %v, want [3 2]", idx)
	}
}

func TestStartAlternateRipGuards(t *testing.T) {
	cfg := config.Defaults()
	cfg.Output.NASPath = t.TempDir()
	s := New(cfg, nil)
	if err := s.StartAlternateRip("nope", 1); !errors.Is(err, ErrNoAlternate) {
		t.Fatalf("unknown job: %v", err)
	}
	s.alternates["j"] = &altState{device: "/dev/sr0", title: "X (2000)", cands: []disc.MKVTitle{{Index: 2}}}
	if err := s.StartAlternateRip("j", 9); !errors.Is(err, ErrNoAlternate) {
		t.Fatalf("unknown index: %v", err)
	}
	s.beginRipTitle("/dev/sr0", "X")
	if err := s.StartAlternateRip("j", 2); !errors.Is(err, ErrDeviceBusy) {
		t.Fatalf("busy drive: %v", err)
	}
}

func TestAlternateRipEndToEnd(t *testing.T) {
	installFakeMakeMKVConTwoTitles(t)
	t.Setenv("HOME", t.TempDir())
	bodies := make(chan string, 8)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hook.Close()

	cfg := config.Defaults()
	cfg.Output.StagingDir = t.TempDir()
	cfg.Output.NASPath = t.TempDir()
	cfg.Notification.DiscordWebhookURL = hook.URL
	s := New(cfg, nil)
	s.alternates["j"] = &altState{
		device: "/dev/sr0", title: "Film (2000)",
		disc:  &disc.ClassifiedDisc{DiscName: "DISC"},
		cands: []disc.MKVTitle{{Index: 1, Duration: 20 * time.Minute}},
	}
	if err := s.StartAlternateRip("j", 1); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	want := filepath.Join(cfg.Output.NASPath, "Film (2000)", "Film (2000) - Alternate (20min).mkv")
	for {
		if _, err := os.Stat(want); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("alternate not delivered to %s", want)
		}
		time.Sleep(50 * time.Millisecond)
	}
	select {
	case b := <-bodies:
		if !strings.Contains(b, "Alternate (20min)") {
			t.Errorf("notification body = %s", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notification")
	}
}

func TestNeedsConfirmation(t *testing.T) {
	main := []disc.MKVTitle{{Duration: 107 * time.Minute}}
	cases := []struct {
		name      string
		main      []disc.MKVTitle
		files     int
		locked    bool
		confirmed bool
		enabled   bool
		runtime   int
		want      bool
	}{
		{"auto match far off", main, 1, false, true, true, 140, true},
		{"auto match close", main, 1, false, true, true, 105, false},
		{"user chose it", main, 1, true, false, true, 140, false},
		{"unconfirmed match with tmdb enabled", main, 1, false, false, true, 0, true},
		{"unconfirmed match with tmdb disabled", main, 1, false, false, false, 0, false},
		{"multi-title disc", append(main, main...), 2, false, false, true, 140, false},
	}
	for _, c := range cases {
		if got := needsConfirmation(c.main, c.files, c.locked, c.confirmed, c.enabled, c.runtime); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestIsConfidentMovieMatch(t *testing.T) {
	d := func(min int) time.Duration { return time.Duration(min) * time.Minute }
	cases := []struct {
		name    string
		runtime int
		longest time.Duration
		want    bool
	}{
		{"exact match", 107, d(107), true},
		{"close theatrical match (within 20m)", 90, d(105), true},
		{"extended cut within 1.6x", 178, d(228), true},
		{"director cut within 1.6x", 144, d(194), true},
		{"shorter cut within 0.70x", 120, d(90), true},
		{"unrelated movie on cryptic disc label (70m vs 180m)", 70, d(180), false},
		{"short film on cryptic label (35m vs 120m)", 35, d(120), false},
		{"tv episode length (< 60m must not confirm as movie)", 50, d(50), false},
		{"tv episode vs full movie", 120, d(45), false},
		{"zero runtime", 0, d(100), false},
		{"zero longest title", 100, 0, false},
	}
	for _, c := range cases {
		if got := isConfidentMovieMatch(c.runtime, c.longest); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
