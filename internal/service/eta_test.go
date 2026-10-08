package service

import (
	"testing"
	"time"
)

func TestETATracker(t *testing.T) {
	var tr etaTracker
	start := time.Unix(0, 0)

	if got := tr.Update(start, 0); got != 0 {
		t.Fatalf("first sample should have no estimate, got %v", got)
	}
	if got := tr.Update(start.Add(5*time.Second), 1); got != 0 {
		t.Fatalf("too little history should have no estimate, got %v", got)
	}
	// 10% in 60s => 6s per percent => 90% left = 540s.
	if got := tr.Update(start.Add(60*time.Second), 10); got != 540*time.Second {
		t.Fatalf("got %v, want 9m0s", got)
	}
	// Progress going backwards resets the window.
	if got := tr.Update(start.Add(70*time.Second), 2); got != 0 {
		t.Fatalf("reset should clear the estimate, got %v", got)
	}
}
