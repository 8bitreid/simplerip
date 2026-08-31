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
