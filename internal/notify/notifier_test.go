package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSender struct {
	mu   sync.Mutex
	got  []Message
	err  error
	hold chan struct{}
}

func (f *fakeSender) Send(_ context.Context, m Message) error {
	if f.hold != nil {
		<-f.hold
	}
	f.mu.Lock()
	f.got = append(f.got, m)
	f.mu.Unlock()
	return f.err
}

func waitFor(d *Dispatcher) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d.Wait(ctx)
}

func TestDispatcherFiltersDisabledEvents(t *testing.T) {
	f := &fakeSender{}
	d := NewDispatcher([]Sender{f}, map[Event]bool{EventComplete: true}, "http://ui:8080/")
	d.Notify(Message{Event: EventComplete})
	d.Notify(Message{Event: EventFailed})
	waitFor(d)

	if len(f.got) != 1 || f.got[0].Event != EventComplete {
		t.Fatalf("got %+v, want only the complete event", f.got)
	}
	if f.got[0].Link != "http://ui:8080" {
		t.Fatalf("Link = %q, want trailing slash trimmed", f.got[0].Link)
	}
}

func TestDispatcherNeverBlocksOrFailsCaller(t *testing.T) {
	f := &fakeSender{hold: make(chan struct{}), err: errors.New("boom")}
	d := NewDispatcher([]Sender{f}, map[Event]bool{EventFailed: true}, "")

	returned := make(chan struct{})
	go func() { d.Notify(Message{Event: EventFailed}); close(returned) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("Notify blocked on a stuck sender")
	}
	close(f.hold)
	waitFor(d)
}

func TestDispatcherNilAndEmptyAreNoOps(t *testing.T) {
	var nilD *Dispatcher
	nilD.Notify(Message{Event: EventFailed})
	nilD.Wait(context.Background())
	NewDispatcher(nil, map[Event]bool{EventFailed: true}, "").Notify(Message{Event: EventFailed})
}

func TestDiscordSenderPayload(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	err := NewDiscordSender(srv.URL).Send(context.Background(), Message{
		Event: EventComplete, Disc: "@everyone DISC", Device: "/dev/sr0", Title: "Beethoven (1992)",
		Summary: "done", Details: []string{"a.mkv (1.0 GB)"}, Link: "http://ui:8080",
	})
	if err != nil {
		t.Fatal(err)
	}

	var p struct {
		Embeds []struct {
			Title, Description, URL string
			Fields                  []struct{ Name, Value string }
		}
		AllowedMentions struct{ Parse []string } `json:"allowed_mentions"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	e := p.Embeds[0]
	if e.Title != "Rip complete" || !strings.Contains(e.Description, "a.mkv") || e.URL != "http://ui:8080" {
		t.Fatalf("unexpected embed: %+v", e)
	}
	names := map[string]string{}
	for _, f := range e.Fields {
		names[f.Name] = f.Value
	}
	if names["Disc"] != "@everyone DISC" || names["Device"] != "/dev/sr0" || names["Title"] != "Beethoven (1992)" {
		t.Fatalf("missing identifying fields: %v", names)
	}
	if len(p.AllowedMentions.Parse) != 0 {
		t.Fatalf("mentions must be disabled, got %v", p.AllowedMentions.Parse)
	}
}

func TestDiscordSenderErrorsDoNotLeakURL(t *testing.T) {
	const secret = "SECRET-TOKEN"
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL + "/api/webhooks/1/" + secret
	srv.Close() // connection refused

	err := NewDiscordSender(url).Send(context.Background(), Message{Event: EventFailed})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(scrubError(err).Error(), secret) {
		t.Fatalf("error leaks webhook secret: %v", err)
	}
}

func TestDiscordSenderNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	if err := NewDiscordSender(srv.URL).Send(context.Background(), Message{Event: EventFailed}); err == nil {
		t.Fatal("expected error on 429")
	}
}
