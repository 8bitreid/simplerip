// Package server provides the HTTP server with WebSocket support for
// real-time progress updates during disc ripping operations.
package server

import (
	"context"
	_ "embed"
	"fmt"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/8bitreid/simplerip/internal/service"
)

//go:embed ui/index.html
var indexHTML []byte

// Server wraps the Echo HTTP server and provides WebSocket progress streaming.
type Server struct {
	e           *echo.Echo
	svc         *service.RipService
	mu          sync.RWMutex
	curState    service.ProgressEvent
	ctx         context.Context
	cancel      context.CancelFunc
	shutdownWg  sync.WaitGroup
}

// New creates a new Server with the given RipService.
func New(svc *service.RipService) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		e:      echo.New(),
		svc:    svc,
		ctx:    ctx,
		cancel: cancel,
		curState: service.ProgressEvent{
			Stage:   "idle",
			Title:   "",
			Percent: 0,
			Message: "No active rip job",
		},
	}

	// Configure Echo
	s.e.HideBanner = true
	s.e.HidePort = true
	s.e.Use(middleware.Logger())
	s.e.Use(middleware.Recover())

	// Routes
	s.e.GET("/", s.handleIndex)
	s.e.GET("/ws/progress", s.handleProgressWS)
	s.e.GET("/api/status", s.handleStatus)

	// Placeholder groups for future expansion
	api := s.e.Group("/api")
	_ = api // Future: config endpoints, auth, job history, etc.

	ws := s.e.Group("/ws")
	_ = ws // Future: additional WebSocket endpoints

	// Start background goroutine to track latest state from EventBus
	s.shutdownWg.Add(1)
	go s.trackProgress()

	return s
}

// Start starts the HTTP server on the given port.
// This is a blocking call — use a goroutine if you need concurrent operation.
func (s *Server) Start(port int) error {
	addr := fmt.Sprintf(":%d", port)
	return s.e.Start(addr)
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	// Cancel background goroutines
	s.cancel()
	// Wait for trackProgress to finish
	s.shutdownWg.Wait()
	return s.e.Shutdown(ctx)
}

// handleIndex serves the embedded HTML UI.
func (s *Server) handleIndex(c echo.Context) error {
	return c.HTMLBlob(http.StatusOK, indexHTML)
}

// handleStatus returns the current rip status as JSON.
func (s *Server) handleStatus(c echo.Context) error {
	s.mu.RLock()
	state := s.curState
	s.mu.RUnlock()
	return c.JSON(http.StatusOK, state)
}

// handleProgressWS upgrades to WebSocket and streams progress events.
func (s *Server) handleProgressWS(c echo.Context) error {
	ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return err
	}
	defer ws.Close()

	// Subscribe to EventBus
	subID, eventCh := s.svc.EventBus().Subscribe()
	defer s.svc.EventBus().Unsubscribe(subID)

	// Send current state immediately on connect
	s.mu.RLock()
	currentState := s.curState
	s.mu.RUnlock()

	if err := ws.WriteJSON(currentState); err != nil {
		return err
	}

	// Stream events until disconnect
	for event := range eventCh {
		if err := ws.WriteJSON(event); err != nil {
			// Client disconnected
			return nil
		}
	}

	return nil
}

// trackProgress subscribes to the EventBus and maintains the latest state
// so that /api/status and new WebSocket connections always have current data.
func (s *Server) trackProgress() {
	defer s.shutdownWg.Done()
	subID, eventCh := s.svc.EventBus().Subscribe()
	defer s.svc.EventBus().Unsubscribe(subID)

	for {
		select {
		case <-s.ctx.Done():
			return
		case event := <-eventCh:
			s.mu.Lock()
			s.curState = event
			s.mu.Unlock()
		}
	}
}

// upgrader is the WebSocket upgrader with default options.
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		// Only allow same-origin by default for security.
		// This prevents cross-site WebSocket hijacking.
		// If you need cross-origin access, configure explicitly.
		return r.Header.Get("Origin") == ""
	},
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}
