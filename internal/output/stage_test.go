package output_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/8bitreid/simplerip/internal/output"
	"github.com/8bitreid/simplerip/internal/tools"
)

func installFakeRsync(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	script := filepath.Join(binDir, "rsync")
	content := `#!/bin/sh
if [ "$RSYNC_FAIL" = "1" ]; then
  exit 2
fi
dest=""
for arg in "$@"; do
  dest="$arg"
done
dest="${dest%/}"
mkdir -p "$dest"
for arg in "$@"; do
  case "$arg" in
    --*) continue ;;
  esac
  if [ "$arg" = "$dest" ] || [ "$arg" = "$dest/" ]; then
    continue
  fi
  cp "$arg" "$dest/"
done
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake rsync: %v", err)
	}
	tools.UseDirForTest(t, binDir)
}

func TestDeliver(t *testing.T) {
	installFakeRsync(t)

	staging := t.TempDir()
	mkv1 := filepath.Join(staging, "title_t00.mkv")
	mkv2 := filepath.Join(staging, "title_t01.mkv")
	if err := os.WriteFile(mkv1, []byte("fake mkv content 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mkv2, []byte("fake mkv content 2"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()

	result, err := output.Deliver(
		context.Background(),
		[]string{mkv1, mkv2},
		staging, dest, "Oppenheimer (2023)",
		"Oppenheimer", "OPPENHEIMER",
	)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	if len(result.Files) != 2 {
		t.Fatalf("want 2 dest files, got %d", len(result.Files))
	}

	for _, f := range result.Files {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("dest file missing: %v", err)
		}
	}

	// rip.json must be present and parseable.
	logPath := filepath.Join(result.DestDir, "rip.json")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("rip.json missing: %v", err)
	}
	var log output.RipLog
	if err := json.Unmarshal(data, &log); err != nil {
		t.Fatalf("rip.json invalid JSON: %v", err)
	}
	if log.Title != "Oppenheimer" {
		t.Errorf("log.Title = %q, want %q", log.Title, "Oppenheimer")
	}
	if len(log.Files) != 2 {
		t.Errorf("log.Files len = %d, want 2", len(log.Files))
	}
}

func TestDeliverNestedDirectory(t *testing.T) {
	installFakeRsync(t)
	staging := t.TempDir()
	source := filepath.Join(staging, "episode-01.mkv")
	if err := os.WriteFile(source, []byte("episode"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	result, err := output.Deliver(context.Background(), []string{source}, staging, dest,
		filepath.Join("The Wire (2002)", "season-1"), "The Wire (2002)", "DISC")
	if err != nil {
		t.Fatalf("Deliver(): %v", err)
	}
	want := filepath.Join(dest, "The Wire (2002)", "season-1", "episode-01.mkv")
	if len(result.Files) != 1 || result.Files[0] != want {
		t.Fatalf("delivered files = %v, want %q", result.Files, want)
	}
	firstLog := filepath.Join(result.DestDir, "rip.json")
	if _, err := os.Stat(firstLog); err != nil {
		t.Fatalf("first rip log missing: %v", err)
	}

	secondSource := filepath.Join(staging, "episode-02.mkv")
	if err := os.WriteFile(secondSource, []byte("second episode"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := output.Deliver(context.Background(), []string{secondSource}, staging, dest,
		filepath.Join("The Wire (2002)", "season-1"), "The Wire (2002)", "DISC 2")
	if err != nil {
		t.Fatalf("second Deliver(): %v", err)
	}
	if _, err := os.Stat(firstLog); err != nil {
		t.Fatalf("first rip log was overwritten: %v", err)
	}
	data, err := os.ReadFile(firstLog)
	if err != nil {
		t.Fatal(err)
	}
	var first output.RipLog
	if err := json.Unmarshal(data, &first); err != nil {
		t.Fatal(err)
	}
	if first.DiscName != "DISC" {
		t.Fatalf("first rip log was overwritten with disc %q", first.DiscName)
	}
	logs, err := filepath.Glob(filepath.Join(second.DestDir, "rip-*.json"))
	if err != nil || len(logs) != 1 {
		t.Fatalf("additional rip log files = %v, err=%v; want one", logs, err)
	}
}

func TestDeliverRsyncFailure(t *testing.T) {
	installFakeRsync(t)
	t.Setenv("RSYNC_FAIL", "1")

	staging := t.TempDir()
	file := filepath.Join(staging, "title_t00.mkv")
	if err := os.WriteFile(file, []byte("fake mkv content"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := output.Deliver(
		context.Background(),
		[]string{file},
		staging,
		t.TempDir(),
		"Movie (2024)",
		"Movie",
		"MOVIE",
	)
	if err == nil || !strings.Contains(err.Error(), "rsync") {
		t.Fatalf("Deliver() error = %v, want rsync error", err)
	}
}

func TestRenameForDelivery(t *testing.T) {
	dir := t.TempDir()
	mk := func(n string) string {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	one, err := output.RenameForDelivery([]string{mk("title_t00.mkv")}, "Real Steel (2011)")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "Real Steel (2011).mkv"); one[0] != want {
		t.Fatalf("got %q, want %q", one[0], want)
	}
	if _, err := os.Stat(one[0]); err != nil {
		t.Fatalf("renamed file missing: %v", err)
	}

	dir2 := t.TempDir()
	a, b := filepath.Join(dir2, "a.mkv"), filepath.Join(dir2, "b.mkv")
	os.WriteFile(a, []byte("x"), 0o644)
	os.WriteFile(b, []byte("x"), 0o644)
	two, err := output.RenameForDelivery([]string{a, b}, "Movie (2000)")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(two[0]) != "Movie (2000) - part1.mkv" || filepath.Base(two[1]) != "Movie (2000) - part2.mkv" {
		t.Fatalf("unexpected names: %v", two)
	}
}

func TestRenameForDeliverySanitizesReservedChars(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "t.mkv")
	os.WriteFile(src, []byte("x"), 0o644)
	got, err := output.RenameForDelivery([]string{src}, `LeapFrog: Letter Factory? (2003)`)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got[0]) != "LeapFrog - Letter Factory (2003).mkv" {
		t.Fatalf("got %q", got[0])
	}
}

func TestRenameForDeliveryPattern_TV(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "t00.mkv"), filepath.Join(dir, "t01.mkv")
	os.WriteFile(a, []byte("x"), 0o644)
	os.WriteFile(b, []byte("x"), 0o644)
	renamed, err := output.RenameForDeliveryPattern([]string{a, b}, "Breaking Bad S01", true)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(renamed[0]) != "Breaking Bad S01 - E01.mkv" || filepath.Base(renamed[1]) != "Breaking Bad S01 - E02.mkv" {
		t.Fatalf("unexpected TV episode names: %v", renamed)
	}
}

func TestTVEpisodeNamingAndNextNumber(t *testing.T) {
	dest := t.TempDir()
	seasonDir := filepath.Join(dest, "The Wire (2002)", "season-1")
	if err := os.MkdirAll(seasonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"episode-01.mkv", "episode-02.mkv"} {
		if err := os.WriteFile(filepath.Join(seasonDir, name), []byte("existing"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	start, err := output.TVEpisodeStart(dest, "The Wire (2002)", 1, 0, 2)
	if err != nil {
		t.Fatalf("TVEpisodeStart(): %v", err)
	}
	if start != 3 {
		t.Fatalf("next episode = %d, want 3", start)
	}
	if _, err := output.TVEpisodeStart(dest, "The Wire (2002)", 1, 2, 1); err == nil {
		t.Fatal("expected collision on an existing requested episode")
	}

	staging := t.TempDir()
	a := filepath.Join(staging, "title_t00.mkv")
	b := filepath.Join(staging, "title_t01.mkv")
	for _, path := range []string{a, b} {
		if err := os.WriteFile(path, []byte("episode"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	renamed, err := output.RenameForDeliveryEpisodes([]string{a, b}, start)
	if err != nil {
		t.Fatalf("RenameForDeliveryEpisodes(): %v", err)
	}
	if filepath.Base(renamed[0]) != "episode-03.mkv" || filepath.Base(renamed[1]) != "episode-04.mkv" {
		t.Fatalf("episode paths = %v", renamed)
	}
}

func TestRenameForDeliveryEpisodeNumbers(t *testing.T) {
	staging := t.TempDir()
	files := []string{
		filepath.Join(staging, "title_t04.mkv"),
		filepath.Join(staging, "title_t01.mkv"),
	}
	for _, file := range files {
		if err := os.WriteFile(file, []byte("episode"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	renamed, err := output.RenameForDeliveryEpisodeNumbers(files, []int{4, 1}, []int{9, 2})
	if err != nil {
		t.Fatalf("RenameForDeliveryEpisodeNumbers(): %v", err)
	}
	if filepath.Base(renamed[0]) != "episode-09.mkv" || filepath.Base(renamed[1]) != "episode-02.mkv" {
		t.Fatalf("mapped episode paths = %v", renamed)
	}

	dest := t.TempDir()
	if err := output.ValidateTVEpisodeNumbers(dest, "Show (2000)", 1, []int{9, 2}); err != nil {
		t.Fatalf("ValidateTVEpisodeNumbers() = %v", err)
	}
	seasonDir := filepath.Join(dest, output.TVSeasonDirectory("Show (2000)", 1))
	if err := os.MkdirAll(seasonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seasonDir, "episode-09.mkv"), []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := output.ValidateTVEpisodeNumbers(dest, "Show (2000)", 1, []int{9}); err == nil {
		t.Fatal("expected an existing mapped episode to be rejected")
	}
}
