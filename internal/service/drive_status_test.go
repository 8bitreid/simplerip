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
