package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/8bitreid/simplerip/internal/config"
	"github.com/8bitreid/simplerip/internal/service"
	"github.com/8bitreid/simplerip/internal/store"
)

// ── mock store ────────────────────────────────────────────────────────────────

type mockStore struct {
	listJobs  func(ctx context.Context) ([]store.Job, error)
	getJob    func(ctx context.Context, id string) (store.Job, []store.JobEvent, error)
	addEvent  func(ctx context.Context, jobID, stage, message string, data any) error
	updateJob func(ctx context.Context, id, title string, year int, status, pattern string) error
}

func (m *mockStore) ListJobs(ctx context.Context) ([]store.Job, error) {
	if m.listJobs != nil {
		return m.listJobs(ctx)
	}
	return nil, nil
}

func (m *mockStore) GetJob(ctx context.Context, id string) (store.Job, []store.JobEvent, error) {
	if m.getJob != nil {
		return m.getJob(ctx, id)
	}
	return store.Job{}, nil, nil
}

func (m *mockStore) AddEvent(ctx context.Context, jobID, stage, message string, data any) error {
	if m.addEvent != nil {
		return m.addEvent(ctx, jobID, stage, message, data)
	}
	return nil
}

func (m *mockStore) UpdateJob(ctx context.Context, id, title string, year int, status, pattern string) error {
	if m.updateJob != nil {
		return m.updateJob(ctx, id, title, year, status, pattern)
	}
	return nil
}

// ── test helpers ──────────────────────────────────────────────────────────────

// newTestServer builds a Server suitable for unit tests: no trackProgress
// goroutine, no WebSocket upgrader. Accepts a jobStore interface so mocks
// can be injected directly.
func newTestServer(st jobStore) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	e := echo.New()
	e.HideBanner = true
	s := &Server{
		e:         e,
		svc:       service.New(config.Defaults(), nil),
		store:     st,
		ctx:       ctx,
		cancel:    cancel,
		curStates: map[string]service.ProgressEvent{},
	}
	s.registerRoutes()
	return s
}

func doRequest(t *testing.T, s *Server, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rr := httptest.NewRecorder()
	s.e.ServeHTTP(rr, req)
	return rr
}

func decodeJSON(t *testing.T, rr *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.NewDecoder(rr.Body).Decode(dst); err != nil {
		t.Fatalf("decode response body: %v (body: %s)", err, rr.Body.String())
	}
}

// ── GET /api/jobs ─────────────────────────────────────────────────────────────

func TestHandleListJobs_NilStore(t *testing.T) {
	s := newTestServer(nil)
	rr := doRequest(t, s, http.MethodGet, "/api/jobs", nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got []store.Job
	decodeJSON(t, rr, &got)
	if len(got) != 0 {
		t.Fatalf("expected empty array, got %d jobs", len(got))
	}
}

func TestHandleListJobs_ReturnsJobs(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	jobs := []store.Job{
		{ID: "uuid-1", Device: "/dev/sr0", DiscLabel: "Star Wars", Status: "done", CreatedAt: now},
		{ID: "uuid-2", Device: "/dev/sr1", DiscLabel: "Dune", Status: "ripping", CreatedAt: now},
	}
	ms := &mockStore{
		listJobs: func(_ context.Context) ([]store.Job, error) { return jobs, nil },
	}
	s := newTestServer(ms)
	rr := doRequest(t, s, http.MethodGet, "/api/jobs", nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got []store.Job
	decodeJSON(t, rr, &got)
	if len(got) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(got))
	}
	if got[0].ID != "uuid-1" {
		t.Errorf("first job ID = %q, want uuid-1", got[0].ID)
	}
}

// ── GET /api/jobs/:id ─────────────────────────────────────────────────────────

func TestHandleGetJob_NilStore(t *testing.T) {
	s := newTestServer(nil)
	rr := doRequest(t, s, http.MethodGet, "/api/jobs/some-id", nil)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

func TestHandleGetJob_Found(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	job := store.Job{ID: "abc-123", Device: "/dev/sr0", Title: "Dune", Year: 2021, Status: "done", CreatedAt: now}
	events := []store.JobEvent{
		{ID: 1, JobID: "abc-123", Stage: "scan", Message: "found 3 titles", CreatedAt: now},
	}
	ms := &mockStore{
		getJob: func(_ context.Context, id string) (store.Job, []store.JobEvent, error) {
			if id == "abc-123" {
				return job, events, nil
			}
			return store.Job{}, nil, store.ErrNotFound
		},
	}
	s := newTestServer(ms)
	rr := doRequest(t, s, http.MethodGet, "/api/jobs/abc-123", nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var got struct {
		Job    store.Job        `json:"job"`
		Events []store.JobEvent `json:"events"`
	}
	decodeJSON(t, rr, &got)
	if got.Job.ID != "abc-123" {
		t.Errorf("job.ID = %q, want abc-123", got.Job.ID)
	}
	if len(got.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got.Events))
	}
	if got.Events[0].Stage != "scan" {
		t.Errorf("event stage = %q, want scan", got.Events[0].Stage)
	}
}

func TestHandleGetJob_NotFound(t *testing.T) {
	ms := &mockStore{
		getJob: func(_ context.Context, id string) (store.Job, []store.JobEvent, error) {
			return store.Job{}, nil, store.ErrNotFound
		},
	}
	s := newTestServer(ms)
	rr := doRequest(t, s, http.MethodGet, "/api/jobs/ghost", nil)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// ── GET /api/search ───────────────────────────────────────────────────────────

func TestHandleSearch_EmptyQuery(t *testing.T) {
	s := newTestServer(nil)
	rr := doRequest(t, s, http.MethodGet, "/api/search", nil)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestHandleSearch_NoTMDBKey(t *testing.T) {
	// config.Defaults() has empty TMDB key, so SearchMovie returns 501-triggering error.
	s := newTestServer(nil)
	rr := doRequest(t, s, http.MethodGet, "/api/search?q=dune", nil)

	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rr.Code)
	}
}

func TestHandleDevices_ReturnsConfiguredDevices(t *testing.T) {
	s := newTestServer(nil)
	s.devices = []string{"/dev/sr0", "/dev/sr1"}
	rr := doRequest(t, s, http.MethodGet, "/api/devices", nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got []string
	decodeJSON(t, rr, &got)
	if len(got) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(got))
	}
	if got[0] != "/dev/sr0" || got[1] != "/dev/sr1" {
		t.Fatalf("got devices %v, want [/dev/sr0 /dev/sr1]", got)
	}
}

// ── POST /api/jobs/:id/reidentify ─────────────────────────────────────────────

func TestHandleReidentify_NilStore(t *testing.T) {
	s := newTestServer(nil)
	body, _ := json.Marshal(reidentifyRequest{TMDBID: 123, Title: "Dune", Year: 2021})
	rr := doRequest(t, s, http.MethodPost, "/api/jobs/abc-123/reidentify", body)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

func TestHandleReidentify_NotFound(t *testing.T) {
	ms := &mockStore{
		getJob: func(_ context.Context, id string) (store.Job, []store.JobEvent, error) {
			return store.Job{}, nil, store.ErrNotFound
		},
	}
	s := newTestServer(ms)
	body, _ := json.Marshal(reidentifyRequest{TMDBID: 123, Title: "Dune", Year: 2021})
	rr := doRequest(t, s, http.MethodPost, "/api/jobs/ghost/reidentify", body)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestHandleReidentify_Success(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	original := store.Job{ID: "abc-123", Device: "/dev/sr0", Title: "Dune Part Two", Year: 2024, Status: "done", CreatedAt: now}

	var capturedStage, capturedMsg string
	var capturedTitle, capturedStatus string
	var capturedYear int

	callCount := 0
	ms := &mockStore{
		getJob: func(_ context.Context, id string) (store.Job, []store.JobEvent, error) {
			callCount++
			if callCount == 1 {
				// first call: existence check
				return original, nil, nil
			}
			// second call: return updated job, preserving the original status
			return store.Job{ID: id, Title: capturedTitle, Year: capturedYear, Status: capturedStatus}, nil, nil
		},
		addEvent: func(_ context.Context, jobID, stage, message string, data any) error {
			capturedStage = stage
			capturedMsg = message
			return nil
		},
		updateJob: func(_ context.Context, id, title string, year int, status, pattern string) error {
			capturedTitle = title
			capturedYear = year
			capturedStatus = status
			return nil
		},
	}
	s := newTestServer(ms)

	reqBody := reidentifyRequest{TMDBID: 693134, Title: "Dune Part Two", Year: 2024}
	body, _ := json.Marshal(reqBody)
	rr := doRequest(t, s, http.MethodPost, "/api/jobs/abc-123/reidentify", body)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}

	var got store.Job
	decodeJSON(t, rr, &got)
	if got.Title != "Dune Part Two" {
		t.Errorf("job.Title = %q, want Dune Part Two", got.Title)
	}
	if got.Year != 2024 {
		t.Errorf("job.Year = %d, want 2024", got.Year)
	}
	if capturedStatus != "done" {
		t.Errorf("status passed to UpdateJob = %q, want done (a manual correction must not reset a completed job's status)", capturedStatus)
	}
	if capturedStage != "identify" {
		t.Errorf("event stage = %q, want identify", capturedStage)
	}
	if capturedMsg == "" {
		t.Error("event message should not be empty")
	}
	_ = capturedMsg
}
