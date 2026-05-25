package notify_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/8bitreid/simplerip/internal/notify"
)

func TestSendSuccess(t *testing.T) {
	var received notify.RipPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := notify.NewClient(srv.URL)
	payload := notify.RipCompletePayload(
		"job-1", "Oppenheimer", "OPPENHEIMER",
		"/output/Oppenheimer (2023)",
		[]string{"/output/Oppenheimer (2023)/title_t00.mkv"},
		nil,
	)
	if err := c.Send(context.Background(), payload); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if received.Event != "rip_complete" {
		t.Errorf("event = %q, want rip_complete", received.Event)
	}
	if received.JobID != "job-1" {
		t.Errorf("job_id = %q, want job-1", received.JobID)
	}
}

func TestSendNoop(t *testing.T) {
	// Empty webhook URL → no-op, no error.
	c := notify.NewClient("")
	if err := c.Send(context.Background(), notify.RipPayload{}); err != nil {
		t.Fatalf("Send with empty URL: %v", err)
	}
}

func TestSendBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := notify.NewClient(srv.URL)
	err := c.Send(context.Background(), notify.RipPayload{Event: "rip_complete"})
	if err == nil {
		t.Fatal("want error on 500, got nil")
	}
}

func TestExtrasPayloadActions(t *testing.T) {
	p := notify.ExtrasPayload("j2", "Oppenheimer", "OPPENHEIMER", "/out", []string{"extras/bts.mkv"})
	if p.Event != "extras_ready" {
		t.Errorf("event = %q", p.Event)
	}
	if len(p.Actions) != 2 {
		t.Errorf("actions len = %d, want 2", len(p.Actions))
	}
}
