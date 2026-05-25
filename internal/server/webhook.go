// Package server provides the HTTP callback endpoint for n8n Discord responses.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// Response is a single Discord action response delivered by n8n.
type Response struct {
	JobID string `json:"job_id"`
	Value string `json:"value"` // e.g. "rip_extras", "skip_extras", "rip_all", "skip"
}

// Server is an HTTP server that receives n8n callbacks.
// Each in-flight job registers a channel; the callback handler routes
// responses to the right channel by job ID.
type Server struct {
	mu      sync.Mutex
	pending map[string]chan Response
	srv     *http.Server
	addr    string // resolved address after Start (handles port 0)
}

// New creates a Server that will listen on the given port.
func New(port int) *Server {
	s := &Server{
		pending: make(map[string]chan Response),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", s.handleCallback)
	s.srv = &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	return s
}

// Start begins listening in a background goroutine.
// The server is ready when the returned channel is closed.
func (s *Server) Start() (<-chan struct{}, error) {
	ln, err := net.Listen("tcp", s.srv.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", s.srv.Addr, err)
	}
	s.addr = ln.Addr().String()
	ready := make(chan struct{})
	go func() {
		close(ready)
		_ = s.srv.Serve(ln) // returns ErrServerClosed on Shutdown
	}()
	return ready, nil
}

// Addr returns the address the server is listening on (e.g. "127.0.0.1:8090").
// Only valid after Start returns successfully.
func (s *Server) Addr() string { return s.addr }

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

// Await registers jobID and blocks until n8n posts a response or the context
// expires. Returns ("", ctx.Err()) on timeout; the response value otherwise.
func (s *Server) Await(ctx context.Context, jobID string) (string, error) {
	ch := make(chan Response, 1)
	s.mu.Lock()
	s.pending[jobID] = ch
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pending, jobID)
		s.mu.Unlock()
	}()

	select {
	case resp := <-ch:
		return resp.Value, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// handleCallback receives POST /callback with a JSON Response body.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var resp Response
	if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	ch, ok := s.pending[resp.JobID]
	s.mu.Unlock()

	if !ok {
		// Unknown job — may have timed out already; respond 200 to avoid n8n retries.
		w.WriteHeader(http.StatusOK)
		return
	}

	select {
	case ch <- resp:
	default:
		// Channel already received; discard duplicate.
	}
	w.WriteHeader(http.StatusOK)
}
