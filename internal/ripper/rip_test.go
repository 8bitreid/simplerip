package ripper

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/8bitreid/simplerip/internal/disc"
	"github.com/8bitreid/simplerip/internal/tools"
)

// TestRipTitleSuccess injects a fake makemkvcon via PATH, verifies that PRGV
// lines are consumed without error, and that the returned paths point to the
// .mkv files the script created.
func TestRipTitleSuccess(t *testing.T) {
	outDir := t.TempDir()

	// The real command is: makemkvcon mkv --noscan -r --messages=-stdout
	//                       --progress=-stdout dev:X <idx> <outdir>
	// We grab the last positional argument (outdir) with the POSIX idiom
	// "for last; do :; done" which works in any /bin/sh implementation.
	scriptBody := `#!/bin/sh
for last; do :; done
OUTDIR="$last"
printf 'MSG:1005,0,1,"MakeMKV v1.18.3 linux(x86_64-release)"\n'
printf 'PRGV:0,0,65536\n'
printf 'PRGV:16384,16384,65536\n'
printf 'PRGV:32768,32768,65536\n'
printf 'PRGV:49152,49152,65536\n'
printf 'PRGV:65536,65536,65536\n'
printf 'MSG:5010,0,1,"Operation successfully completed"\n'
touch "$OUTDIR/Title_t00.mkv"
touch "$OUTDIR/Title_t01.mkv"
exit 0
`
	fakeMakeMKV(t, scriptBody)

	t.Logf("outDir: %s", outDir)

	title := disc.MKVTitle{Index: 0, Name: "Inception"}
	files, err := RipTitle(context.Background(), "/dev/sr0", title, outDir, RipOptions{Key: "test-key", TimeoutMinutes: 2, CacheMB: 256, ReadErrorLimit: 100, NoProgressMinutes: 15})
	if err != nil {
		t.Fatalf("RipTitle returned error: %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("expected 2 .mkv files, got %d: %v", len(files), files)
	}
	for _, f := range files {
		if !strings.HasSuffix(f, ".mkv") {
			t.Errorf("unexpected file in result: %q", f)
		}
		if _, err := os.Stat(f); err != nil {
			t.Errorf("returned path does not exist: %q", f)
		}
	}
}

func TestRipTitleFailure(t *testing.T) {
	outDir := t.TempDir()
	scriptBody := `#!/bin/sh
exit 3
`
	fakeMakeMKV(t, scriptBody)

	_, err := RipTitle(context.Background(), "/dev/sr0", disc.MKVTitle{Index: 1}, outDir, RipOptions{Key: "test-key", TimeoutMinutes: 1, CacheMB: 256, ReadErrorLimit: 100, NoProgressMinutes: 15})
	if err == nil {
		t.Fatal("expected RipTitle to fail")
	}
	if errors.Is(err, ErrRipTimeout) {
		t.Fatalf("expected non-timeout error, got %v", err)
	}
}

func TestNewMKVFilesCutoff(t *testing.T) {
	dir := t.TempDir()
	oldFile := filepath.Join(dir, "old.mkv")
	newFile := filepath.Join(dir, "new.mkv")

	if err := os.WriteFile(oldFile, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-2 * time.Second)
	if err := os.Chtimes(oldFile, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now()
	if err := os.WriteFile(newFile, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := newMKVFiles(dir, cutoff)
	if err != nil {
		t.Fatalf("newMKVFiles() error = %v", err)
	}
	if len(files) != 1 || files[0] != newFile {
		t.Fatalf("newMKVFiles() = %v, want [%s]", files, newFile)
	}
}

func TestRipTitleReadErrorLimit(t *testing.T) {
	outDir := t.TempDir()
	scriptBody := `#!/bin/sh
printf 'MSG:2003,0,3,"Read error one"\n'
printf 'MSG:2003,0,3,"Read error two"\n'
printf 'MSG:2003,0,3,"Read error three"\n'
sleep 1
exit 1
`
	fakeMakeMKV(t, scriptBody)

	_, err := RipTitle(context.Background(), "/dev/sr0", disc.MKVTitle{Index: 2}, outDir, RipOptions{Key: "test-key", TimeoutMinutes: 2, CacheMB: 256, ReadErrorLimit: 3, NoProgressMinutes: 0})
	if err == nil {
		t.Fatal("expected read error limit failure")
	}
	if !errors.Is(err, ErrRipReadErrorLimit) {
		t.Fatalf("expected ErrRipReadErrorLimit, got %v", err)
	}
}

func TestRipTitleReportsAnalyzeThenSavePhases(t *testing.T) {
	outDir := t.TempDir()
	body := `#!/bin/sh
for last; do :; done
printf 'PRGV:0,0,65536\n'
printf 'PRGV:32768,32768,65536\n'
printf 'MSG:5011,0,0,"Operation successfully completed"\n'
printf 'MSG:5014,131072,2,"Saving 1 titles into directory x"\n'
printf 'PRGV:0,0,65536\n'
printf 'PRGV:16384,16384,65536\n'
touch "$last/Title_t00.mkv"
exit 0
`
	fakeMakeMKV(t, body)

	type step struct {
		pct   int
		phase RipPhase
	}
	var got []step
	cb := func(_ int, pct int, phase RipPhase) { got = append(got, step{pct, phase}) }
	if _, err := RipTitle(context.Background(), "/dev/sr0", disc.MKVTitle{Index: 0}, outDir, RipOptions{Key: "k", TimeoutMinutes: 2, CacheMB: 256, ReadErrorLimit: 100, NoProgressMinutes: 15, Progress: cb}); err != nil {
		t.Fatal(err)
	}
	want := []step{{0, PhaseAnalyze}, {50, PhaseAnalyze}, {0, PhaseSave}, {25, PhaseSave}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("step %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

func TestRipTitleSaveFailedReportsLocation(t *testing.T) {
	outDir := t.TempDir()
	body := `#!/bin/sh
printf 'MSG:2003,0,3,"Error x","fmt","Scsi error","/VIDEO_TS/VTS_01_1.VOB","601227264"\n'
printf 'MSG:5003,0,2,"Failed to save title 0","fmt","0","f.mkv"\n'
printf 'PRGV:100,100,100\n'
exit 0
`
	fakeMakeMKV(t, body)

	_, err := RipTitle(context.Background(), "/dev/sr0", disc.MKVTitle{Index: 0}, outDir, RipOptions{Key: "k", TimeoutMinutes: 2, CacheMB: 256, ReadErrorLimit: 0, NoProgressMinutes: 0})
	var re *ReadError
	if !errors.Is(err, ErrRipSaveFailed) || !errors.As(err, &re) {
		t.Fatalf("expected ReadError(ErrRipSaveFailed), got %v", err)
	}
	if re.Offset != 601227264 || re.File != "/VIDEO_TS/VTS_01_1.VOB" {
		t.Fatalf("bad location: %+v", re)
	}
}

func TestCalculateBatchTimeoutMinutes(t *testing.T) {
	got, err := CalculateBatchTimeoutMinutes(45, 10, 14)
	if err != nil {
		t.Fatalf("CalculateBatchTimeoutMinutes() error = %v", err)
	}
	if got != 185 {
		t.Fatalf("CalculateBatchTimeoutMinutes() = %d, want 185", got)
	}

	for _, tc := range []struct {
		analyze, save, titles int
	}{
		{0, 10, 2},
		{45, 0, 2},
		{45, 10, 0},
		{-1, 10, 2},
	} {
		if _, err := CalculateBatchTimeoutMinutes(tc.analyze, tc.save, tc.titles); err == nil {
			t.Errorf("CalculateBatchTimeoutMinutes(%d, %d, %d) expected error",
				tc.analyze, tc.save, tc.titles)
		}
	}
	maxInt := int(^uint(0) >> 1)
	if _, err := CalculateBatchTimeoutMinutes(1, maxInt, 2); err == nil {
		t.Fatal("CalculateBatchTimeoutMinutes() accepted an overflowing timeout")
	}
}

func TestMapTitleFilesUsesZeroBasedSuffix(t *testing.T) {
	titles := []disc.MKVTitle{{Index: 0}, {Index: 2}, {Index: 4}}
	files := []string{
		"/staging/D3_t04.mkv",
		"/staging/D3_t02.mkv",
		"/staging/D3_t01.mkv", // unselected extra
		"/staging/not-a-title.mkv",
	}
	got, err := MapTitleFiles(titles, files)
	if err != nil {
		t.Fatalf("MapTitleFiles() error = %v", err)
	}
	if len(got) != 2 || got[2] != files[1] || got[4] != files[0] {
		t.Fatalf("MapTitleFiles() = %v, want title 2 -> %s, title 4 -> %s",
			got, files[1], files[0])
	}
	if _, ok := got[3]; ok {
		t.Fatal("title 2's t02 suffix was incorrectly mapped to title index 3")
	}
}

func TestMapTitleFilesRejectsDuplicateIndex(t *testing.T) {
	_, err := MapTitleFiles(
		[]disc.MKVTitle{{Index: 2}},
		[]string{"/staging/Disc_t02.mkv", "/staging/Other_t02.mkv"},
	)
	if err == nil {
		t.Fatal("MapTitleFiles() accepted multiple files for the same title")
	}
}

func TestRipTitlesRunsOneBatchAndTracksTitleByFile(t *testing.T) {
	outDir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("ARGS_FILE", argsFile)
	// No PRGC/PRGT lines: the active title must come from the output files.
	fakeMakeMKV(t, `#!/bin/sh
printf '%s\n' "$@" > "$ARGS_FILE"
for last; do :; done
printf 'PRGV:0,0,65536\n'
printf 'MSG:5014,131072,2,"Saving 2 titles into directory x"\n'
touch "$last/Disc_t00.mkv"
printf 'PRGV:32768,16384,65536\n'
sleep 1.1
touch "$last/Disc_t02.mkv"
printf 'PRGV:16384,40960,65536\n'
printf '%070000d\n' 0
exit 0
`)

	titles := []disc.MKVTitle{
		{Index: 0, Name: "Episode One", Duration: 11*time.Minute + 3*time.Second},
		{Index: 2, Name: "Episode Two", Duration: 10 * time.Minute},
	}
	type progress struct {
		index int
		pct   int
		phase RipPhase
	}
	var got []progress
	files, err := RipTitles(
		context.Background(),
		"/dev/sr0",
		titles,
		outDir,
		RipOptions{
			Key:               "test-key",
			TimeoutMinutes:    10,
			CacheMB:           256,
			ReadErrorLimit:    100,
			NoProgressMinutes: 15,
			Progress:          func(index, pct int, phase RipPhase) { got = append(got, progress{index, pct, phase}) },
		},
	)
	if err != nil {
		t.Fatalf("RipTitles() error = %v", err)
	}
	if len(files) != 2 || filepath.Base(files[0]) != "Disc_t00.mkv" || filepath.Base(files[1]) != "Disc_t02.mkv" {
		t.Fatalf("RipTitles() files = %v, want Disc_t00.mkv and Disc_t02.mkv", files)
	}
	want := []progress{{0, 0, PhaseAnalyze}, {0, 50, PhaseSave}, {2, 25, PhaseSave}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("progress callbacks = %+v, want %+v", got, want)
	}

	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	argList := strings.Fields(string(args))
	for _, want := range []string{"all", "--minlength=599", "dev:/dev/sr0"} {
		if !containsString(argList, want) {
			t.Fatalf("MakeMKV args = %q, missing %q", argList, want)
		}
	}
}

func TestBatchProgressPercentUsesTotalForAnalyzeAndCurrentForSave(t *testing.T) {
	// Shaped like makemkvcon robot output: max is fixed at 65536, total climbs
	// across the whole operation, current restarts for each saved title.
	steps := []struct {
		line  string
		phase RipPhase
		want  int
	}{
		{"0,0,65536", PhaseAnalyze, 0},
		{"65536,13108,65536", PhaseAnalyze, 20}, // a finished sub-step must not read as 100%
		{"6553,52429,65536", PhaseAnalyze, 80},
		{"0,0,65536", PhaseSave, 0},
		{"32768,8192,65536", PhaseSave, 50}, // title 1 of 4 half done
		{"65536,16384,65536", PhaseSave, 100},
		{"0,16384,65536", PhaseSave, 0}, // title 2 starts over
		{"49152,28672,65536", PhaseSave, 75},
		{"70000,70000,65536", PhaseSave, 100},
		{"5,5,0", PhaseSave, 0},
	}
	for _, step := range steps {
		prog, ok := parsePRGV(step.line)
		if !ok {
			t.Fatalf("parsePRGV(%q) failed", step.line)
		}
		if got := batchProgressPercent(prog, step.phase); got != step.want {
			t.Errorf("batchProgressPercent(PRGV:%s, phase %d) = %d, want %d", step.line, step.phase, got, step.want)
		}
	}
}

func TestRipTitlesFailureKeepsCompletedTitlesOnly(t *testing.T) {
	outDir := t.TempDir()
	fakeMakeMKV(t, `#!/bin/sh
for last; do :; done
printf 'MSG:5014,131072,2,"Saving 3 titles into directory x"\n'
touch "$last/Disc_t00.mkv"
printf 'PRGV:65536,21845,65536\n'
sleep 0.3
touch "$last/Disc_t02.mkv"
printf 'PRGV:100,21900,65536\n'
sleep 0.3
exit 1
`)

	files, err := RipTitles(
		context.Background(),
		"/dev/sr0",
		[]disc.MKVTitle{{Index: 0, Duration: 5 * time.Minute}, {Index: 2, Duration: 5 * time.Minute},
			{Index: 4, Duration: 5 * time.Minute}},
		outDir,
		RipOptions{
			Key:               "test-key",
			TimeoutMinutes:    2,
			CacheMB:           256,
			ReadErrorLimit:    100,
			NoProgressMinutes: 0,
		},
	)
	if err == nil {
		t.Fatal("RipTitles() succeeded after makemkvcon exited 1")
	}
	if len(files) != 1 || filepath.Base(files[0]) != "Disc_t00.mkv" {
		t.Fatalf("RipTitles() files = %v, want only finished Disc_t00.mkv", files)
	}
	if _, statErr := os.Stat(filepath.Join(outDir, "Disc_t00.mkv")); statErr != nil {
		t.Fatalf("finished title was removed: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(outDir, "Disc_t02.mkv")); !os.IsNotExist(statErr) {
		t.Fatalf("unfinished title output still exists or could not be checked: %v", statErr)
	}
}

func TestRipTitlesReadErrorLimitDropsUnfinishedTitle(t *testing.T) {
	outDir := t.TempDir()
	fakeMakeMKV(t, `#!/bin/sh
for last; do :; done
printf 'MSG:5014,131072,2,"Saving 2 titles into directory x"\n'
touch "$last/Disc_t00.mkv"
sleep 0.3
printf 'MSG:2003,0,3,"Read error"\n'
sleep 1
exit 1
`)

	files, err := RipTitles(
		context.Background(),
		"/dev/sr0",
		[]disc.MKVTitle{{Index: 0, Duration: 5 * time.Minute}, {Index: 1, Duration: 5 * time.Minute}},
		outDir,
		RipOptions{
			Key:               "test-key",
			TimeoutMinutes:    2,
			CacheMB:           256,
			ReadErrorLimit:    1,
			NoProgressMinutes: 0,
		},
	)
	if !errors.Is(err, ErrRipReadErrorLimit) {
		t.Fatalf("RipTitles() error = %v, want ErrRipReadErrorLimit", err)
	}
	if len(files) != 0 {
		t.Fatalf("RipTitles() files = %v, want unfinished title excluded", files)
	}
	if _, statErr := os.Stat(filepath.Join(outDir, "Disc_t00.mkv")); !os.IsNotExist(statErr) {
		t.Fatalf("unfinished title output still exists or could not be checked: %v", statErr)
	}
}

func TestRipTitlesSaveFailureForNonActiveTitle(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failed    string
		wantFiles []string
		wantErr   bool
	}{
		// An unselected title failing must not cost the selected ones.
		{"unselected extra", "Disc_t01.mkv", []string{"Disc_t00.mkv", "Disc_t02.mkv"}, false},
		// A finished title failing is dropped even though Disc_t02 is newest.
		{"finished selected title", "Disc_t00.mkv", []string{"Disc_t02.mkv"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outDir := t.TempDir()
			t.Setenv("FAILED_FILE", tc.failed)
			fakeMakeMKV(t, `#!/bin/sh
for last; do :; done
printf 'MSG:5014,131072,2,"Saving 3 titles into directory x"\n'
touch "$last/Disc_t00.mkv"
printf 'PRGV:0,0,65536\n'
sleep 0.2
touch "$last/Disc_t01.mkv"
sleep 0.2
touch "$last/Disc_t02.mkv"
printf 'PRGV:0,0,65536\n'
printf 'MSG:5003,0,2,"Failed to save title to file file://%s/%s","Failed to save title %%1 to file %%2","1","file://%s/%s"\n' "$last" "$FAILED_FILE" "$last" "$FAILED_FILE"
exit 0
`)

			files, err := RipTitles(
				context.Background(),
				"/dev/sr0",
				[]disc.MKVTitle{{Index: 0, Duration: 5 * time.Minute}, {Index: 2, Duration: 5 * time.Minute}},
				outDir,
				RipOptions{
					Key:               "test-key",
					TimeoutMinutes:    2,
					CacheMB:           256,
					ReadErrorLimit:    100,
					NoProgressMinutes: 0,
				},
			)
			if tc.wantErr {
				var re *ReadError
				if !errors.As(err, &re) || !errors.Is(err, ErrRipSaveFailed) || re.Title != 0 {
					t.Fatalf("RipTitles() error = %v, want ErrRipSaveFailed for title 0", err)
				}
			} else if err != nil {
				t.Fatalf("RipTitles() error = %v", err)
			}
			var got []string
			for _, file := range files {
				got = append(got, filepath.Base(file))
			}
			if !reflect.DeepEqual(got, tc.wantFiles) {
				t.Fatalf("RipTitles() files = %v, want %v", got, tc.wantFiles)
			}
			for _, name := range tc.wantFiles {
				if _, statErr := os.Stat(filepath.Join(outDir, name)); statErr != nil {
					t.Fatalf("kept title %s was removed: %v", name, statErr)
				}
			}
			if _, statErr := os.Stat(filepath.Join(outDir, tc.failed)); !os.IsNotExist(statErr) {
				t.Fatalf("failed output %s still exists or could not be checked: %v", tc.failed, statErr)
			}
		})
	}
}

func TestRipTitlesUnnamedSaveFailureKeepsFullLengthTitles(t *testing.T) {
	previous := probeDuration
	t.Cleanup(func() { probeDuration = previous })
	probeDuration = func(_ context.Context, path string) (time.Duration, error) {
		switch filepath.Base(path) {
		case "Disc_t00.mkv":
			return 22*time.Minute + 2*time.Second, nil
		case "Disc_t02.mkv":
			return 9 * time.Minute, nil // cut short by the failed save
		}
		return 0, errors.New("unreadable")
	}

	outDir := t.TempDir()
	fakeMakeMKV(t, `#!/bin/sh
for last; do :; done
printf 'MSG:5014,131072,2,"Saving 3 titles into directory x"\n'
touch "$last/Disc_t00.mkv"
sleep 0.2
touch "$last/Disc_t02.mkv"
sleep 0.2
touch "$last/Disc_t04.mkv"
printf 'MSG:5003,0,0,"Failed to save title"\n'
exit 0
`)

	episode := 22 * time.Minute
	files, err := RipTitles(
		context.Background(),
		"/dev/sr0",
		[]disc.MKVTitle{{Index: 0, Duration: episode}, {Index: 2, Duration: episode}, {Index: 4, Duration: episode}},
		outDir,
		RipOptions{
			Key:               "test-key",
			TimeoutMinutes:    2,
			CacheMB:           256,
			ReadErrorLimit:    100,
			NoProgressMinutes: 0,
		},
	)
	var re *ReadError
	if !errors.Is(err, ErrRipSaveFailed) || !errors.As(err, &re) || re.Title != 2 {
		t.Fatalf("RipTitles() error = %v, want ErrRipSaveFailed for title 2", err)
	}
	if len(files) != 1 || filepath.Base(files[0]) != "Disc_t00.mkv" {
		t.Fatalf("RipTitles() files = %v, want only full-length Disc_t00.mkv", files)
	}
	for _, name := range []string{"Disc_t02.mkv", "Disc_t04.mkv"} {
		if _, statErr := os.Stat(filepath.Join(outDir, name)); !os.IsNotExist(statErr) {
			t.Fatalf("%s (short or unprobeable) still exists or could not be checked: %v", name, statErr)
		}
	}
}

func TestRipTitlesRemovesUnselectedTitlesOnly(t *testing.T) {
	outDir := t.TempDir()
	fakeMakeMKV(t, `#!/bin/sh
for last; do :; done
printf 'MSG:5014,131072,2,"Saving 3 titles into directory x"\n'
touch "$last/Disc_t00.mkv" "$last/Disc_t01.mkv" "$last/Disc_t02.mkv" "$last/Disc.mkv"
exit 0
`)

	files, err := RipTitles(
		context.Background(),
		"/dev/sr0",
		[]disc.MKVTitle{{Index: 0, Duration: 5 * time.Minute}, {Index: 2, Duration: 5 * time.Minute}},
		outDir,
		RipOptions{
			Key:               "test-key",
			TimeoutMinutes:    2,
			CacheMB:           256,
			ReadErrorLimit:    100,
			NoProgressMinutes: 0,
		},
	)
	if err != nil {
		t.Fatalf("RipTitles() error = %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("RipTitles() files = %v, want both selected outputs", files)
	}
	if _, statErr := os.Stat(filepath.Join(outDir, "Disc_t01.mkv")); !os.IsNotExist(statErr) {
		t.Fatalf("unselected extra still exists or could not be checked: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(outDir, "Disc.mkv")); statErr != nil {
		t.Fatalf("file without a title suffix was removed: %v", statErr)
	}
}

func TestRipTitlesTimeoutReturnsErrRipTimeout(t *testing.T) {
	previous := batchTimeoutUnit
	batchTimeoutUnit = 100 * time.Millisecond
	t.Cleanup(func() { batchTimeoutUnit = previous })

	outDir := t.TempDir()
	// exec so the timeout kill reaches sleep, which would otherwise hold stdout open.
	fakeMakeMKV(t, `#!/bin/sh
for last; do :; done
printf 'MSG:5014,131072,1,"Saving 1 titles into directory x"\n'
touch "$last/Disc_t00.mkv"
exec sleep 10
`)

	files, err := RipTitles(
		context.Background(),
		"/dev/sr0",
		[]disc.MKVTitle{{Index: 0, Duration: 5 * time.Minute}},
		outDir,
		RipOptions{
			Key:               "test-key",
			TimeoutMinutes:    3,
			CacheMB:           256,
			ReadErrorLimit:    100,
			NoProgressMinutes: 0,
		},
	)
	if !errors.Is(err, ErrRipTimeout) {
		t.Fatalf("RipTitles() error = %v, want ErrRipTimeout", err)
	}
	if len(files) != 0 {
		t.Fatalf("RipTitles() returned partial files %v, want none", files)
	}
	if _, statErr := os.Stat(filepath.Join(outDir, "Disc_t00.mkv")); !os.IsNotExist(statErr) {
		t.Fatalf("timed-out title output still exists or could not be checked: %v", statErr)
	}
}

func TestWatchdogExpiredOnlyDuringSaveOrReadErrors(t *testing.T) {
	lastProgress := time.Unix(100, 0)
	now := lastProgress.Add(time.Minute)
	if watchdogExpired(PhaseAnalyze, 0, 1, lastProgress, now) {
		t.Fatal("watchdog expired during analyze without read errors")
	}
	if !watchdogExpired(PhaseSave, 0, 1, lastProgress, now) {
		t.Fatal("watchdog did not expire during save")
	}
	if !watchdogExpired(PhaseAnalyze, 1, 1, lastProgress, now) {
		t.Fatal("watchdog did not expire during analyze after read errors")
	}
	if watchdogExpired(PhaseSave, 0, 1, lastProgress, now.Add(-time.Second)) {
		t.Fatal("watchdog expired before the configured interval")
	}
}

func TestRipTitlesIgnoresStaleOutputs(t *testing.T) {
	outDir := t.TempDir()
	// Written just before the rip, inside the same second as its start.
	stale := filepath.Join(outDir, "Disc_t02.mkv")
	if err := os.WriteFile(stale, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeMakeMKV(t, `#!/bin/sh
for last; do :; done
printf 'MSG:5014,131072,2,"Saving 2 titles into directory x"\n'
touch "$last/Disc_t00.mkv"
`)

	files, err := RipTitles(
		context.Background(),
		"/dev/sr0",
		[]disc.MKVTitle{{Index: 0, Duration: 5 * time.Minute}, {Index: 2, Duration: 5 * time.Minute}},
		outDir,
		RipOptions{
			Key:               "test-key",
			TimeoutMinutes:    2,
			CacheMB:           256,
			ReadErrorLimit:    100,
			NoProgressMinutes: 0,
		},
	)
	var re *ReadError
	if !errors.Is(err, ErrRipSaveFailed) || !errors.As(err, &re) || re.Title != 2 {
		t.Fatalf("RipTitles() error = %v, want ErrRipSaveFailed for missing title 2", err)
	}
	if len(files) != 1 || filepath.Base(files[0]) != "Disc_t00.mkv" {
		t.Fatalf("RipTitles() files = %v, want only fresh title 0 output", files)
	}
	if _, statErr := os.Stat(stale); statErr != nil {
		t.Fatalf("pre-existing file was touched: %v", statErr)
	}
}

func TestRipTitlesAcceptsRecoverableReadErrorsWhenAllTitlesWereWritten(t *testing.T) {
	outDir := t.TempDir()
	fakeMakeMKV(t, `#!/bin/sh
for last; do :; done
printf 'MSG:2003,0,3,"Recovered read error"\n'
printf 'MSG:5014,131072,2,"Saving 2 titles into directory x"\n'
touch "$last/Disc_t00.mkv" "$last/Disc_t02.mkv"
exit 0
`)

	files, err := RipTitles(
		context.Background(),
		"/dev/sr0",
		[]disc.MKVTitle{{Index: 0, Duration: 5 * time.Minute}, {Index: 2, Duration: 5 * time.Minute}},
		outDir,
		RipOptions{
			Key:               "test-key",
			TimeoutMinutes:    2,
			CacheMB:           256,
			ReadErrorLimit:    10,
			NoProgressMinutes: 0,
		},
	)
	if err != nil {
		t.Fatalf("RipTitles() error = %v with complete output files", err)
	}
	if len(files) != 2 {
		t.Fatalf("RipTitles() files = %v, want both selected outputs", files)
	}
}

// fakeMakeMKV puts a makemkvcon script first on PATH and points HOME at a
// temp dir, since writeTunedConfig writes $HOME/.MakeMKV/settings.conf.
func fakeMakeMKV(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "makemkvcon"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake makemkvcon: %v", err)
	}
	tools.UseDirForTest(t, dir)
	t.Setenv("HOME", t.TempDir())
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
