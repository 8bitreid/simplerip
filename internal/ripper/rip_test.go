package ripper

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/8bitreid/simplerip/internal/disc"
)

// TestRipTitleSuccess injects a fake makemkvcon via PATH, verifies that PRGV
// lines are consumed without error, and that the returned paths point to the
// .mkv files the script created.
func TestRipTitleSuccess(t *testing.T) {
	outDir := t.TempDir()

	// Build a fake makemkvcon in its own temp dir.
	// The real command is: makemkvcon mkv --noscan -r --messages=-stdout
	//                       --progress=-stdout dev:X <idx> <outdir>
	// We grab the last positional argument (outdir) with the POSIX idiom
	// "for last; do :; done" which works in any /bin/sh implementation.
	scriptDir := t.TempDir()
	script := filepath.Join(scriptDir, "makemkvcon")
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
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write fake script: %v", err)
	}

	// Prepend the fake binary's directory so exec.LookPath finds it first.
	t.Setenv("PATH", scriptDir+":"+os.Getenv("PATH"))

	t.Logf("outDir: %s", outDir)

	title := disc.MKVTitle{Index: 0, Name: "Inception"}
	files, err := RipTitle(context.Background(), "/dev/sr0", title, outDir, "test-key", 2, 256, 100, 15, nil)
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
	scriptDir := t.TempDir()
	script := filepath.Join(scriptDir, "makemkvcon")
	scriptBody := `#!/bin/sh
exit 3
`
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write fake script: %v", err)
	}
	t.Setenv("PATH", scriptDir+":"+os.Getenv("PATH"))

	_, err := RipTitle(context.Background(), "/dev/sr0", disc.MKVTitle{Index: 1}, outDir, "test-key", 1, 256, 100, 15, nil)
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
	scriptDir := t.TempDir()
	script := filepath.Join(scriptDir, "makemkvcon")
	scriptBody := `#!/bin/sh
printf 'MSG:2003,0,3,"Read error one"\n'
printf 'MSG:2003,0,3,"Read error two"\n'
printf 'MSG:2003,0,3,"Read error three"\n'
sleep 1
exit 1
`
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write fake script: %v", err)
	}
	t.Setenv("PATH", scriptDir+":"+os.Getenv("PATH"))

	_, err := RipTitle(context.Background(), "/dev/sr0", disc.MKVTitle{Index: 2}, outDir, "test-key", 2, 256, 3, 0, nil)
	if err == nil {
		t.Fatal("expected read error limit failure")
	}
	if !errors.Is(err, ErrRipReadErrorLimit) {
		t.Fatalf("expected ErrRipReadErrorLimit, got %v", err)
	}
}

func TestRipTitleReportsAnalyzeThenSavePhases(t *testing.T) {
	outDir := t.TempDir()
	scriptDir := t.TempDir()
	script := filepath.Join(scriptDir, "makemkvcon")
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
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", scriptDir+":"+os.Getenv("PATH"))

	type step struct {
		pct   int
		phase RipPhase
	}
	var got []step
	cb := func(_ int, pct int, phase RipPhase) { got = append(got, step{pct, phase}) }
	if _, err := RipTitle(context.Background(), "/dev/sr0", disc.MKVTitle{Index: 0}, outDir, "k", 2, 256, 100, 15, cb); err != nil {
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
	scriptDir := t.TempDir()
	body := `#!/bin/sh
printf 'MSG:2003,0,3,"Error x","fmt","Scsi error","/VIDEO_TS/VTS_01_1.VOB","601227264"\n'
printf 'MSG:5003,0,2,"Failed to save title 0","fmt","0","f.mkv"\n'
printf 'PRGV:100,100,100\n'
exit 0
`
	if err := os.WriteFile(filepath.Join(scriptDir, "makemkvcon"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", scriptDir+":"+os.Getenv("PATH"))

	_, err := RipTitle(context.Background(), "/dev/sr0", disc.MKVTitle{Index: 0}, outDir, "k", 2, 256, 0, 0, nil)
	var re *ReadError
	if !errors.Is(err, ErrRipSaveFailed) || !errors.As(err, &re) {
		t.Fatalf("expected ReadError(ErrRipSaveFailed), got %v", err)
	}
	if re.Offset != 601227264 || re.File != "/VIDEO_TS/VTS_01_1.VOB" {
		t.Fatalf("bad location: %+v", re)
	}
}
