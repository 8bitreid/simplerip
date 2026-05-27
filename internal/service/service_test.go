package service

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/8bitreid/simplerip/internal/config"
	"github.com/8bitreid/simplerip/internal/metadata"
	"github.com/8bitreid/simplerip/internal/ripper"
)

func TestRipService_ScanDisc_Movie(t *testing.T) {
	cfg := config.Defaults()
	svc := New(cfg)

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
	svc := New(cfg)

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
	svc := New(cfg)

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
	svc := New(cfg)

	_, err := svc.EnrichMovie(context.Background(), metadata.MovieResult{ID: 1, Title: "Oppenheimer"})
	if err == nil {
		t.Fatal("expected error when TMDB API key is not configured, got nil")
	}
}

func TestSearchMovie_NoAPIKey(t *testing.T) {
	cfg := config.Defaults() // TMDBApiKey is empty
	svc := New(cfg)

	_, err := svc.SearchMovie(context.Background(), "Oppenheimer")
	if err == nil {
		t.Fatal("expected error when TMDB API key is not configured, got nil")
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
	svc := New(cfg)

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
