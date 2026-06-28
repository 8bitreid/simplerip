// Package server provides the HTTP server with WebSocket support for
// real-time progress updates during disc ripping operations.
package server

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/8bitreid/simplerip/internal/service"
	"github.com/8bitreid/simplerip/internal/store"
)

//go:embed ui/index.html
var indexHTML []byte

var ejectDevice = func(device string) error {
	return exec.Command("eject", device).Run()
}

// jobStore is the persistence interface the server depends on.
// *store.Store satisfies it; a nil value disables persistence.
type jobStore interface {
	ListJobs(ctx context.Context) ([]store.Job, error)
	GetJob(ctx context.Context, id string) (store.Job, []store.JobEvent, error)
	AddEvent(ctx context.Context, jobID, stage, message string, data any) error
	UpdateJob(ctx context.Context, id, title string, year int, status, pattern string) error
}

// Server wraps the Echo HTTP server and provides WebSocket progress streaming.
type Server struct {
	e          *echo.Echo
	svc        *service.RipService
	store      jobStore
	devices    []string
	mu         sync.RWMutex
	curStates  map[string]service.ProgressEvent // device -> latest event
	ctx        context.Context
	cancel     context.CancelFunc
	shutdownWg sync.WaitGroup
}

// New creates a new Server with the given RipService and optional store.
// st may be nil — job history endpoints return appropriate error responses
// when no database is configured.
func New(svc *service.RipService, st *store.Store, devices []string) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		e:         echo.New(),
		svc:       svc,
		devices:   append([]string(nil), devices...),
		ctx:       ctx,
		cancel:    cancel,
		curStates: make(map[string]service.ProgressEvent),
	}
	// Seed each configured device with an idle state so a fresh client renders
	// the drive cards immediately, before any progress event arrives.
	for _, dev := range s.devices {
		s.curStates[dev] = service.ProgressEvent{
			Device:  dev,
			Stage:   "idle",
			Percent: 0,
			Message: "no disc — waiting",
		}
	}
	if st != nil {
		s.store = st
	}

	s.e.HideBanner = true
	s.e.HidePort = true
	s.e.Use(middleware.Logger())
	s.e.Use(middleware.Recover())

	s.registerRoutes()

	s.shutdownWg.Add(1)
	go s.trackProgress()

	return s
}

func (s *Server) registerRoutes() {
	s.e.GET("/", s.handleIndex)
	s.e.GET("/ws/progress", s.handleProgressWS)
	s.e.GET("/api/status", s.handleStatus)
	s.e.GET("/api/devices", s.handleDevices)
	s.e.POST("/api/eject", s.handleEject)
	s.e.POST("/api/eject/:device", s.handleEject)
	s.e.GET("/api/jobs", s.handleListJobs)
	s.e.GET("/api/jobs/:id", s.handleGetJob)
	s.e.GET("/api/search", s.handleSearch)
	s.e.POST("/api/jobs/:id/reidentify", s.handleReidentify)
}

// Start starts the HTTP server on the given port.
// This is a blocking call — use a goroutine if you need concurrent operation.
func (s *Server) Start(port int) error {
	addr := fmt.Sprintf(":%d", port)
	return s.e.Start(addr)
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.cancel()
	s.shutdownWg.Wait()
	return s.e.Shutdown(ctx)
}

// handleIndex serves the embedded HTML UI.
func (s *Server) handleIndex(c echo.Context) error {
	return c.HTMLBlob(http.StatusOK, indexHTML)
}

// handleStatus returns the current per-device rip status as JSON, keyed by
// device path. Each entry is the most recent progress event seen for that drive.
func (s *Server) handleStatus(c echo.Context) error {
	s.mu.RLock()
	states := make(map[string]service.ProgressEvent, len(s.curStates))
	for dev, st := range s.curStates {
		states[dev] = st
	}
	s.mu.RUnlock()
	return c.JSON(http.StatusOK, states)
}

// handleDevices returns the configured optical devices.
func (s *Server) handleDevices(c echo.Context) error {
	if len(s.devices) == 0 {
		return c.JSON(http.StatusOK, []string{})
	}
	return c.JSON(http.StatusOK, s.devices)
}

func (s *Server) handleEject(c echo.Context) error {
	device := c.Param("device")
	if strings.TrimSpace(device) == "" {
		var body struct {
			Device string `json:"device"`
		}
		if err := c.Bind(&body); err == nil {
			device = strings.TrimSpace(body.Device)
		}
	}
	if strings.TrimSpace(device) == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "device is required"})
	}

	allowed := false
	for _, dev := range s.devices {
		if dev == device {
			allowed = true
			break
		}
	}
	if !allowed {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "unknown device"})
	}

	if err := ejectDevice(device); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("eject failed: %v", err)})
	}

	// Immediately show the drive as idle in UI while poller confirms state.
	s.svc.MarkDeviceIdle(device)
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "device": device})
}

// handleListJobs returns the 100 most recent jobs.
// Returns an empty array when no database is configured.
func (s *Server) handleListJobs(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusOK, []store.Job{})
	}
	jobs, err := s.store.ListJobs(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if jobs == nil {
		jobs = []store.Job{}
	}
	return c.JSON(http.StatusOK, jobs)
}

// handleGetJob returns a single job and its events.
func (s *Server) handleGetJob(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	id := c.Param("id")
	job, events, err := s.store.GetJob(c.Request().Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return c.JSON(http.StatusNotFound, map[string]string{"error": "job not found"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if events == nil {
		events = []store.JobEvent{}
	}
	return c.JSON(http.StatusOK, map[string]any{"job": job, "events": events})
}

// searchResultJSON is the response shape for each TMDB search hit.
type searchResultJSON struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Year      int    `json:"year"`
	Runtime   int    `json:"runtime"`
	MediaType string `json:"media_type"`
}

// handleSearch queries TMDB and returns up to 5 matching movies.
func (s *Server) handleSearch(c echo.Context) error {
	q := strings.TrimSpace(c.QueryParam("q"))
	if q == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "q is required"})
	}

	results, err := s.svc.SearchMovie(c.Request().Context(), q)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "tmdb_api_key not configured") {
			return c.JSON(http.StatusNotImplemented, map[string]string{"error": "TMDB API key not configured"})
		}
		if strings.Contains(msg, "no TMDB results") {
			return c.JSON(http.StatusOK, []searchResultJSON{})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": msg})
	}

	out := make([]searchResultJSON, 0, len(results))
	for _, r := range results {
		yr, _ := strconv.Atoi(r.Year())
		out = append(out, searchResultJSON{
			ID:        r.ID,
			Title:     r.Title,
			Year:      yr,
			Runtime:   0, // not available from TMDB search endpoint
			MediaType: "movie",
		})
	}
	return c.JSON(http.StatusOK, out)
}

// reidentifyRequest is the body for POST /api/jobs/:id/reidentify.
type reidentifyRequest struct {
	TMDBID int    `json:"tmdb_id"`
	Title  string `json:"title"`
	Year   int    `json:"year"`
}

// handleReidentify applies a manual metadata correction to an existing job.
func (s *Server) handleReidentify(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}

	id := c.Param("id")
	ctx := c.Request().Context()

	var body reidentifyRequest
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}

	// Verify the job exists and capture its current status/pattern so a manual
	// correction only updates title/year — it must not reset a completed job
	// back to "identifying" or wipe its detection pattern.
	existing, _, err := s.store.GetJob(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return c.JSON(http.StatusNotFound, map[string]string{"error": "job not found"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	_ = s.store.AddEvent(ctx, id, "identify",
		fmt.Sprintf("manual correction: %s (%d)", body.Title, body.Year),
		map[string]any{
			"tmdb_id":    body.TMDBID,
			"title":      body.Title,
			"year":       body.Year,
			"correction": true,
		})

	if err := s.store.UpdateJob(ctx, id, body.Title, body.Year, existing.Status, existing.Pattern); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	// If this job is currently ripping on a device, update the in-memory
	// live title immediately so websocket progress text and final deliver
	// naming reflect the correction without waiting for a new scan.
	liveTitle := body.Title
	if body.Year > 0 {
		liveTitle = fmt.Sprintf("%s (%d)", body.Title, body.Year)
	}
	_ = s.svc.ReidentifyRip(ctx, existing.Device, liveTitle, body.TMDBID)

	job, _, err := s.store.GetJob(ctx, id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, job)
}

// WebSocket keepalive tuning. The server pings the client periodically and
// expects a pong within pongWait; a read pump enforces this. Without it,
// half-open connections (common behind proxies/tailnets) linger undetected and
// the client repaints from a stale snapshot on every reconnect.
const (
	wsWriteWait  = 10 * time.Second
	wsPongWait   = 60 * time.Second
	wsPingPeriod = (wsPongWait * 9) / 10
)

// handleProgressWS upgrades to WebSocket and streams progress events. It
// replays the latest state for every known device on connect so a freshly
// (re)connected client immediately has the true state of all drives, then
// streams live updates. A read pump + ping ticker keep the connection healthy
// and detect dead peers promptly.
func (s *Server) handleProgressWS(c echo.Context) error {
	ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return err
	}
	defer ws.Close()

	subID, eventCh := s.svc.EventBus().Subscribe()
	defer s.svc.EventBus().Unsubscribe(subID)

	// Read pump: required for gorilla to process control frames (pong/close).
	// It discards client payloads and signals the writer when the peer goes away.
	connClosed := make(chan struct{})
	ws.SetReadDeadline(time.Now().Add(wsPongWait))
	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(wsPongWait))
		return nil
	})
	go func() {
		defer close(connClosed)
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// Replay current per-device state so all drive cards are correct on connect.
	s.mu.RLock()
	snapshot := make([]service.ProgressEvent, 0, len(s.curStates))
	for _, st := range s.curStates {
		snapshot = append(snapshot, st)
	}
	s.mu.RUnlock()
	for _, st := range snapshot {
		ws.SetWriteDeadline(time.Now().Add(wsWriteWait))
		if err := ws.WriteJSON(st); err != nil {
			return nil
		}
	}

	ping := time.NewTicker(wsPingPeriod)
	defer ping.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return nil
		case <-connClosed:
			return nil
		case event, ok := <-eventCh:
			if !ok {
				return nil
			}
			ws.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := ws.WriteJSON(event); err != nil {
				return nil
			}
		case <-ping.C:
			ws.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteWait)); err != nil {
				return nil
			}
		}
	}
}

// trackProgress subscribes to the EventBus and maintains the latest state per
// device so that /api/status and new WebSocket connections always have current
// data for every drive.
func (s *Server) trackProgress() {
	defer s.shutdownWg.Done()
	subID, eventCh := s.svc.EventBus().Subscribe()
	defer s.svc.EventBus().Unsubscribe(subID)

	for {
		select {
		case <-s.ctx.Done():
			return
		case event := <-eventCh:
			if event.Device == "" {
				continue
			}
			s.mu.Lock()
			s.curStates[event.Device] = event
			s.mu.Unlock()
		}
	}
}

// upgrader is the WebSocket upgrader with default options.
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		host := "http://" + r.Host
		if r.TLS != nil {
			host = "https://" + r.Host
		}
		return origin == host
	},
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}
