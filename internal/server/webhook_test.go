package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/8bitreid/simplerip/internal/server"
)

// postCallback posts a Response to the given handler.
func postCallback(t *testing.T, handler http.Handler, resp server.Response) *http.Response {
	t.Helper()
	body, _ := json.Marshal(resp)
	req := httptest.NewRequest(http.MethodPost, "/callback", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w.Result()
}

func TestAwaitReceivesResponse(t *testing.T) {
	s := server.New(0) // port 0 — we'll use httptest directly via ServeHTTP
	ready, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	<-ready
	defer s.Shutdown(context.Background()) //nolint:errcheck

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Await in a goroutine.
	resultCh := make(chan string, 1)
	go func() {
		val, err := s.Await(ctx, "job-abc")
		if err != nil {
			resultCh <- "ERROR:" + err.Error()
			return
		}
		resultCh <- val
	}()

	// Give Await time to register the channel.
	time.Sleep(20 * time.Millisecond)

	// Post the callback directly via the server's HTTP port.
	addr := s.Addr()
	resp, err := http.Post("http://"+addr+"/callback", "application/json",
		bytes.NewReader(mustMarshal(t, server.Response{JobID: "job-abc", Value: "rip_extras"})))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	val := <-resultCh
	if val != "rip_extras" {
		t.Errorf("Await returned %q, want rip_extras", val)
	}
}

func TestAwaitTimeout(t *testing.T) {
	s := server.New(0)
	ready, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	<-ready
	defer s.Shutdown(context.Background()) //nolint:errcheck

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = s.Await(ctx, "no-one-will-respond")
	if err == nil {
		t.Fatal("want error on timeout, got nil")
	}
}

func TestCallbackUnknownJob(t *testing.T) {
	s := server.New(0)
	ready, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	<-ready
	defer s.Shutdown(context.Background()) //nolint:errcheck

	addr := s.Addr()
	resp, err := http.Post("http://"+addr+"/callback", "application/json",
		bytes.NewReader(mustMarshal(t, server.Response{JobID: "ghost-job", Value: "skip"})))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	// Unknown job must still return 200 (no n8n retry).
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
