package output_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/8bitreid/simplerip/internal/output"
)

func TestDeliver(t *testing.T) {
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
