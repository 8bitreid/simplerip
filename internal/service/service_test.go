package service

import (
	"context"
	"fmt"
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
	"github.com/8bitreid/simplerip/internal/metadata"
	"github.com/8bitreid/simplerip/internal/ripper"
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
# Handle scan/info command: makemkvcon -r info dev:/dev/sr0
if [ "$1" = "-r" ] && [ "$2" = "info" ]; then
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
