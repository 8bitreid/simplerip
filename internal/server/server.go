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

	"github.com/8bitreid/simplerip/internal/disc"
	"github.com/8bitreid/simplerip/internal/service"
	"github.com/8bitreid/simplerip/internal/store"
)

const jobsPageSize = 100

//go:embed ui/index.html
var indexHTML []byte

var ejectDevice = func(device string) error {
	return exec.Command("eject", device).Run()
}

// jobStore is the persistence interface the server depends on.
// *store.Store satisfies it; a nil value disables persistence.
type jobStore interface {
	ListJobs(ctx context.Context, limit, offset int) ([]store.Job, error)
	GetJob(ctx context.Context, id string) (store.Job, []store.JobEvent, error)
	AddEvent(ctx context.Context, jobID, stage, message string, data any) error
	UpdateJob(ctx context.Context, id, title string, year int, status, pattern string) error
	DeleteJob(ctx context.Context, id string) error
	DeleteFinishedJobs(ctx context.Context) (int64, error)
}

// Server wraps the Echo HTTP server and provides WebSocket progress streaming.
type Server struct {
	e          *echo.Echo
	svc        *service.RipService
	store      jobStore
	devices    []string
	mu         sync.RWMutex
	curStates  map[string]service.ProgressEvent // device -> latest event
	autoEject  map[string]bool
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
		autoEject: make(map[string]bool),
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
	s.shutdownWg.Add(1)
	go s.trackDriveStatus()

	return s
}

func (s *Server) registerRoutes() {
	s.e.GET("/", s.handleIndex)
	s.e.GET("/ws/progress", s.handleProgressWS)
	s.e.GET("/api/status", s.handleStatus)
	s.e.GET("/api/devices", s.handleDevices)
	s.e.POST("/api/eject", s.handleEject)
	s.e.POST("/api/eject/:device", s.handleEject)
	s.e.POST("/api/cancel", s.handleCancelRip)
	s.e.POST("/api/auto-eject", s.handleAutoEject)
	s.e.GET("/api/jobs", s.handleListJobs)
	s.e.GET("/api/jobs/:id", s.handleGetJob)
	s.e.DELETE("/api/jobs/:id", s.handleDeleteJob)
	s.e.DELETE("/api/jobs", s.handleDeleteFinishedJobs)
	s.e.GET("/api/search", s.handleSearch)
	s.e.POST("/api/jobs/:id/reidentify", s.handleReidentify)
	s.e.POST("/api/jobs/:id/alternates/:index/rip", s.handleRipAlternate)
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
	// The service tracks active pipelines by device. Progress events can lag or
	// remain stale (for example, after a worker exits), so they must not block
	// ejecting an otherwise idle drive.
	if s.svc.HasActiveRip(device) {
		return c.JSON(http.StatusConflict, map[string]string{"error": "a rip is in process; ejecting now would cancel it. Cancel the rip first or wait for it to finish."})
	}

	if err := ejectDevice(device); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("eject failed: %v", err)})
	}

	// Immediately show the drive as idle in UI while poller confirms state.
	s.svc.MarkDeviceIdle(device)
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "device": device})
}

func (s *Server) handleCancelRip(c echo.Context) error {
	var body struct {
		Device string `json:"device"`
	}
	if err := c.Bind(&body); err != nil || strings.TrimSpace(body.Device) == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "device is required"})
	}
	if !s.isConfiguredDevice(body.Device) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "unknown device"})
	}
	if !s.svc.CancelRip(body.Device) {
		return c.JSON(http.StatusConflict, map[string]string{"error": "no active rip found for this drive"})
	}
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "device": body.Device})
}

func (s *Server) handleAutoEject(c echo.Context) error {
	var body struct {
		Device  string `json:"device"`
		Enabled bool   `json:"enabled"`
	}
	if err := c.Bind(&body); err != nil || strings.TrimSpace(body.Device) == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "device is required"})
	}
	if !s.isConfiguredDevice(body.Device) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "unknown device"})
	}
	s.mu.Lock()
	s.autoEject[body.Device] = body.Enabled
	s.mu.Unlock()
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "device": body.Device, "enabled": body.Enabled})
}

func (s *Server) isConfiguredDevice(device string) bool {
	for _, dev := range s.devices {
		if dev == device {
			return true
		}
	}
	return false
}

// handleListJobs returns a page of the most recent jobs.
// Returns an empty array when no database is configured.
func (s *Server) handleListJobs(c echo.Context) error {
	limit, err := strconv.Atoi(c.QueryParam("limit"))
	if c.QueryParam("limit") == "" {
		limit = jobsPageSize
	} else if err != nil || limit < 1 || limit > jobsPageSize {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("limit must be between 1 and %d", jobsPageSize)})
	}
	offset := 0
	if rawOffset := c.QueryParam("offset"); rawOffset != "" {
		offset, err = strconv.Atoi(rawOffset)
		if err != nil || offset < 0 {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "offset must be a non-negative integer"})
		}
	}
	if s.store == nil {
		return c.JSON(http.StatusOK, []store.Job{})
	}
	jobs, err := s.store.ListJobs(c.Request().Context(), limit, offset)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if jobs == nil {
		jobs = []store.Job{}
	}
	return c.JSON(http.StatusOK, jobs)
}

func (s *Server) trackDriveStatus() {
	defer s.shutdownWg.Done()
	events := disc.PollEventsWithStatus(s.ctx, s.devices, 5*time.Second, s.svc.HasActiveRip, func(device, status string) {
		s.mu.Lock()
		state := s.curStates[device]
		state.Device = device
		state.DriveStatus = status
		s.curStates[device] = state
		s.mu.Unlock()
	})
	for range events {
	}
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

// handleDeleteJob removes one finished job from history. Files already
// delivered to the NAS are not touched.
func (s *Server) handleDeleteJob(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	err := s.store.DeleteJob(c.Request().Context(), c.Param("id"))
	switch {
	case err == nil:
		return c.NoContent(http.StatusNoContent)
	case errors.Is(err, store.ErrNotFound):
		return c.JSON(http.StatusNotFound, map[string]string{"error": "job not found"})
	case errors.Is(err, store.ErrJobActive):
		return c.JSON(http.StatusConflict, map[string]string{"error": "job is still in progress"})
	default:
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

// handleDeleteFinishedJobs clears all finished jobs from history.
func (s *Server) handleDeleteFinishedJobs(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	n, err := s.store.DeleteFinishedJobs(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]int64{"deleted": n})
}

// searchResultJSON is the response shape for each TMDB search hit.
type searchResultJSON struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Year      int    `json:"year"`
	Runtime   int    `json:"runtime"`
	MediaType string `json:"media_type"`
}

// handleSearch queries TMDB's multi-search endpoint for movies and TV shows.
func (s *Server) handleSearch(c echo.Context) error {
	q := strings.TrimSpace(c.QueryParam("q"))
	if q == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "q is required"})
	}

	results, err := s.svc.SearchMedia(c.Request().Context(), q)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "not configured") {
			return c.JSON(http.StatusNotImplemented, map[string]string{"error": "TMDB API key or access token not configured"})
		}
		if strings.Contains(msg, "no TMDB results") {
			return c.JSON(http.StatusOK, []searchResultJSON{})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": msg})
	}

	out := make([]searchResultJSON, 0, len(results))
	for _, r := range results {
		yr, _ := strconv.Atoi(r.Year)
		out = append(out, searchResultJSON{
			ID:        r.ID,
			Title:     r.Title,
			Year:      yr,
			MediaType: r.MediaType,
		})
	}
	return c.JSON(http.StatusOK, out)
}

// reidentifyRequest is the body for POST /api/jobs/:id/reidentify.
type reidentifyRequest struct {
	TMDBID       int    `json:"tmdb_id"`
	Title        string `json:"title"`
	Year         int    `json:"year"`
	MediaType    string `json:"media_type"`
	Season       int    `json:"season"`
	EpisodeStart int    `json:"episode_start"`
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
	body.MediaType = strings.ToLower(strings.TrimSpace(body.MediaType))
	if body.MediaType == "" {
		body.MediaType = "movie"
	}
	if body.MediaType != "movie" && body.MediaType != "tv" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "media_type must be movie or tv"})
	}
	if strings.TrimSpace(body.Title) == "" || body.TMDBID <= 0 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "a title and valid TMDB ID are required"})
	}
	if body.MediaType == "tv" && body.Season < 1 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "season must be at least 1 for a TV show"})
	}
	if body.EpisodeStart < 0 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "episode_start cannot be negative"})
	}
	if body.MediaType == "movie" && (body.Season != 0 || body.EpisodeStart != 0) {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "season and episode_start are only valid for TV shows"})
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

	if s.svc.TitleFrozen(existing.Device) && !isFinished(existing.Status) {
		return c.JSON(http.StatusConflict, map[string]string{"error": "delivery has started; the title can no longer be changed"})
	}

	// Pin the chosen movie's runtime before the correction event is written:
	// a rip waiting on manual input wakes on that event and reads it.
	if !isFinished(existing.Status) && body.MediaType == "movie" {
		s.svc.SetRipRuntime(existing.Device, s.svc.RuntimeFor(ctx, body.TMDBID))
	}

	_ = s.store.AddEvent(ctx, id, "identify",
		fmt.Sprintf("manual correction: %s (%d)", body.Title, body.Year),
		map[string]any{
			"tmdb_id":       body.TMDBID,
			"title":         body.Title,
			"year":          body.Year,
			"media_type":    body.MediaType,
			"season":        body.Season,
			"episode_start": body.EpisodeStart,
			"correction":    true,
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
	_ = s.svc.ReidentifyRip(existing.Device, liveTitle)
	// If the chosen movie needs a different title than the one being ripped,
	// stop and start over with the corrected identity.
	message := ""
	if !isFinished(existing.Status) {
		restarted := s.svc.RestartIfNeeded(existing.Device)
		message = s.svc.EditMessage(existing.Device, restarted)
		_ = s.store.AddEvent(ctx, id, "identify", message, map[string]any{"restart": restarted})
	}

	job, _, err := s.store.GetJob(ctx, id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, struct {
		store.Job
		Message string `json:"message,omitempty"`
	}{job, message})
}

// handleRipAlternate starts ripping one alternate cut found on a job's disc.
func (s *Server) handleRipAlternate(c echo.Context) error {
	idx, err := strconv.Atoi(c.Param("index"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid title index"})
	}
	switch err := s.svc.StartAlternateRip(c.Param("id"), idx); {
	case err == nil:
		return c.JSON(http.StatusAccepted, map[string]string{"status": "started"})
	case errors.Is(err, service.ErrNoAlternate):
		return c.JSON(http.StatusNotFound, map[string]string{"error": "that alternate is no longer available (the daemon restarted or the disc changed)"})
	case errors.Is(err, service.ErrDeviceBusy):
		return c.JSON(http.StatusConflict, map[string]string{"error": "the drive is busy; try again when it is idle"})
	default:
		return c.JSON(http.StatusConflict, map[string]string{"error": "the main rip has not been delivered yet, or no output path is configured"})
	}
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
			event.DriveStatus = s.curStates[event.Device].DriveStatus
			s.curStates[event.Device] = event
			auto := event.Stage == "done" && s.autoEject[event.Device]
			s.mu.Unlock()
			if auto {
				if err := ejectDevice(event.Device); err == nil {
					s.svc.MarkDeviceIdle(event.Device)
				}
			}
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

func isFinished(status string) bool { return status == "done" || status == "error" }
