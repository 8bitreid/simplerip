package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/8bitreid/simplerip/internal/config"
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
