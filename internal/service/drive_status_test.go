package service

import (
	"testing"

	"github.com/8bitreid/simplerip/internal/config"
)

func TestSetDriveStatus_OnlyWhenIdle(t *testing.T) {
	s := New(&config.Config{}, nil)
	_, ch := s.EventBus().Subscribe()

	s.SetDriveStatus("/dev/sr1", "detecting disc…")
	if ev := <-ch; ev.Stage != "idle" || ev.Message != "detecting disc…" {
		t.Fatalf("unexpected event %+v", ev)
	}

	s.emit(ProgressEvent{Device: "/dev/sr1", Stage: "ripping", Message: "x"})
	<-ch
	s.SetDriveStatus("/dev/sr1", "no disc — waiting")
	select {
	case ev := <-ch:
		t.Fatalf("status must not overwrite an active drive, got %+v", ev)
	default:
	}
}

func TestManualTitleSurvivesAutoIdentify(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.beginRipTitle("/dev/sr1", "SR_2000")
	if !s.ReidentifyRip("/dev/sr1", "Real Steel (2011)") {
		t.Fatal("re-identify should apply to an active job")
	}
	s.setAutoTitle("/dev/sr1", "Wrong Match (1999)")
	if got := s.currentTitle("/dev/sr1", ""); got != "Real Steel (2011)" {
		t.Fatalf("manual title was overwritten: %q", got)
	}
	s.endRipTitle("/dev/sr1")
	s.beginRipTitle("/dev/sr1", "Next Disc")
	s.setAutoTitle("/dev/sr1", "Auto (2020)")
	if got := s.currentTitle("/dev/sr1", ""); got != "Auto (2020)" {
		t.Fatalf("lock leaked into the next job: %q", got)
	}
}

func TestTitleFrozenOnDelivery(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.beginRipTitle("/dev/sr0", "Disc")
	s.ReidentifyRip("/dev/sr0", "Real Steel (2011)")
	if got := s.freezeTitle("/dev/sr0", "x"); got != "Real Steel (2011)" {
		t.Fatalf("freeze returned %q", got)
	}
	if s.ReidentifyRip("/dev/sr0", "Other (2000)") {
		t.Fatal("re-identify must be rejected after delivery starts")
	}
	if !s.TitleFrozen("/dev/sr0") {
		t.Fatal("expected frozen")
	}
	s.endRipTitle("/dev/sr0")
	if s.TitleFrozen("/dev/sr0") {
		t.Fatal("freeze leaked past job end")
	}
}

func TestPinnedRuntimeLifecycle(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.SetRipRuntime("/dev/sr0", 90) // no active rip: ignored
	if got := s.ripRuntime("/dev/sr0"); got != 0 {
		t.Fatalf("runtime set without an active rip: %d", got)
	}
	s.beginRipTitle("/dev/sr0", "Disc")
	s.SetRipRuntime("/dev/sr0", 90)
	if got := s.ripRuntime("/dev/sr0"); got != 90 {
		t.Fatalf("runtime = %d, want 90", got)
	}
	s.SetRipRuntime("/dev/sr0", 0)
	if got := s.ripRuntime("/dev/sr0"); got != 0 {
		t.Fatalf("runtime not cleared: %d", got)
	}
	s.SetRipRuntime("/dev/sr0", 90)
	s.endRipTitle("/dev/sr0")
	if got := s.ripRuntime("/dev/sr0"); got != 0 {
		t.Fatalf("runtime leaked into next job: %d", got)
	}
}

func TestReidentifyRipClearsSearchPrompt(t *testing.T) {
	s := New(&config.Config{}, nil)
	_, ch := s.EventBus().Subscribe()
	s.beginRipTitle("/dev/sr0", "SPONGEBOB_DISC1")
	s.emit(ProgressEvent{Device: "/dev/sr0", Stage: "identifying",
		Message: "TV disc detected. Search for the show and select its season."})
	<-ch

	if !s.ReidentifyRip("/dev/sr0", "SpongeBob SquarePants (1999)") {
		t.Fatal("re-identify should apply to an active job")
	}
	ev := <-ch
	if ev.Title != "SpongeBob SquarePants (1999)" {
		t.Errorf("Title = %q, want the chosen show", ev.Title)
	}
	if ev.Message != "Identified as SpongeBob SquarePants (1999)" {
		t.Errorf("Message = %q, want the search prompt replaced", ev.Message)
	}
}

func TestNeedsUnconfirmedMatchNotice(t *testing.T) {
	tests := []struct {
		name                                                    string
		hasMain, tmdbConfigured, confirmed, tvMatched, tvPrompt bool
		want                                                    bool
	}{
		{"unmatched movie", true, true, false, false, false, true},
		{"confirmed movie", true, true, true, false, false, false},
		{"no TMDB key", true, false, false, false, false, false},
		{"no main titles", false, true, false, false, false, false},
		{"matched TV show", true, true, false, true, false, false},
		{"unresolved TV already prompted", true, true, false, false, true, false},
		{"matched TV with unresolved season", true, true, false, true, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := needsUnconfirmedMatchNotice(tt.hasMain, tt.tmdbConfigured, tt.confirmed, tt.tvMatched, tt.tvPrompt)
			if got != tt.want {
				t.Errorf("needsUnconfirmedMatchNotice() = %v, want %v", got, tt.want)
			}
		})
	}
}
