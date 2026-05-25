package ripper

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	files, err := RipTitle(context.Background(), "/dev/sr0", title, outDir, "test-key", 2)
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
