package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/8bitreid/simplerip/internal/config"
	"github.com/8bitreid/simplerip/internal/service"
	"github.com/8bitreid/simplerip/internal/store"
	"github.com/8bitreid/simplerip/internal/tools"
)

// ── mock store ────────────────────────────────────────────────────────────────

type mockStore struct {
	listJobs     func(ctx context.Context, limit, offset int) ([]store.Job, error)
	statusCounts map[string]int64
	getJob       func(ctx context.Context, id string) (store.Job, []store.JobEvent, error)
	addEvent     func(ctx context.Context, jobID, stage, message string, data any) error
	updateJob    func(ctx context.Context, id, title string, year int, status, pattern string) error
	deleteJob    func(ctx context.Context, id string) error
}

func (m *mockStore) DeleteJob(ctx context.Context, id string) error {
	if m.deleteJob != nil {
		return m.deleteJob(ctx, id)
	}
	return nil
}

func (m *mockStore) DeleteFinishedJobs(ctx context.Context) (int64, error) { return 3, nil }

func (m *mockStore) JobStatusCounts(ctx context.Context) (map[string]int64, error) {
	return m.statusCounts, nil
}

func (m *mockStore) ListJobs(ctx context.Context, limit, offset int) ([]store.Job, error) {
	if m.listJobs != nil {
		return m.listJobs(ctx, limit, offset)
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
		cfg:       config.Defaults(),
		build:     BuildMetadata{Version: "dev", Commit: "unknown", BuildDate: "unknown"},
		hostname:  "test-host",
		startedAt: time.Now().UTC(),
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

func TestHandleInfoShapeAndSecretRedaction(t *testing.T) {
	st := &mockStore{statusCounts: map[string]int64{"done": 8, "error": 2, "cancelled": 1}}
	s := newTestServer(st)
	s.cfg.Output.StagingDir = t.TempDir()
	s.cfg.Output.NASPath = "rsync://nas-user:nas-password@nas.example.com/archive"
	s.cfg.Metadata.TMDBApiKey = "tmdb-key-secret"
	s.cfg.Metadata.TMDBAccessToken = "tmdb-token-secret"
	s.cfg.Notification.WebhookURL = "https://n8n.example.com/webhook/webhook-secret"
	s.cfg.Notification.DiscordWebhookURL = "https://discord.example.com/webhook/discord-secret"
	s.build = BuildMetadata{Version: "v1.2.3", Commit: "abc1234", BuildDate: "2026-10-07T12:00:00Z"}
	s.curStates["/dev/sr0"] = service.ProgressEvent{Device: "/dev/sr0", DriveStatus: "disc_present"}
	s.infoCache.tools = toolVersions{
		MakeMKV: toolVersion{Version: stringPointer("MakeMKV v1.18.3")},
		FFprobe: toolVersion{Version: stringPointer("7.1")},
	}
	s.infoCache.deliveryReachable = true
	s.infoCache.deliveryExpiresAt = time.Now().Add(time.Minute)

	rr := doRequest(t, s, http.MethodGet, "/api/info", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/info status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var got map[string]json.RawMessage
	body := rr.Body.Bytes()
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode info response shape: %v (body: %s)", err, body)
	}
	wantFields := []string{
		"name", "version", "commit", "build_date", "go_version", "hostname",
		"started_at", "uptime_seconds", "drives_detected", "tools", "delivery",
		"staging", "integrations", "stats",
	}
	if len(got) != len(wantFields) {
		t.Fatalf("response has %d top-level fields, want %d: %s", len(got), len(wantFields), rr.Body.String())
	}
	for _, key := range wantFields {
		if _, ok := got[key]; !ok {
			t.Errorf("response missing field %q", key)
		}
	}
	for _, secret := range []string{
		"nas-user", "nas-password", "tmdb-key-secret", "tmdb-token-secret",
		"webhook-secret", "discord-secret",
	} {
		if strings.Contains(string(body), secret) {
			t.Errorf("response exposed secret %q", secret)
		}
	}

	var response infoResponse
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode info response: %v", err)
	}
	if response.Name != "SimpleRip" || response.Version != "v1.2.3" || response.Commit != "abc1234" {
		t.Errorf("build identity = (%q, %q, %q)", response.Name, response.Version, response.Commit)
	}
	if response.DrivesDetected != 1 {
		t.Errorf("drives_detected = %d, want 1", response.DrivesDetected)
	}
	if response.Delivery.Destination != "rsync://redacted@nas.example.com/archive" || !response.Delivery.Reachable {
		t.Errorf("delivery = %+v", response.Delivery)
	}
	if !response.Integrations.TMDBConfigured || !response.Integrations.DiscordConfigured {
		t.Errorf("integration configuration = %+v", response.Integrations)
	}
	if response.Stats != (ripStats{Done: 8, Error: 2, Cancelled: 1}) {
		t.Errorf("stats = %+v", response.Stats)
	}
	if response.Staging.FreeBytes == nil || response.Staging.TotalBytes == nil || response.Staging.Error != "" {
		t.Errorf("staging info = %+v", response.Staging)
	}
}

func stringPointer(value string) *string { return &value }

func TestParseFFprobeVersion(t *testing.T) {
	output := []byte("ffprobe version 6.1.1-3ubuntu5 Copyright (c) 2007-2024 the FFmpeg developers\nbuilt with gcc\n")
	if got, want := parseFFprobeVersion(output), "6.1.1-3ubuntu5"; got != want {
		t.Fatalf("parseFFprobeVersion() = %q, want %q", got, want)
	}
}

func TestParseMakeMKVVersionFromStderr(t *testing.T) {
	output := []byte("MakeMKV v2.0.0 linux(x64-release) started\nUse: makemkvcon [switches] Command [Parameters]\n")
	if got, want := parseMakeMKVVersion(output), "MakeMKV v2.0.0"; got != want {
		t.Fatalf("parseMakeMKVVersion() = %q, want %q", got, want)
	}
}

func TestProbeMakeMKVVersionAcceptsNonZeroExitWithStderrBanner(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "makemkvcon")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' 'MakeMKV v2.1.0 linux(x64-release) started' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tools.UseDirForTest(t, dir)

	got := probeMakeMKVVersion(context.Background())
	if got.Version == nil || *got.Version != "MakeMKV v2.1.0" || got.Error != "" {
		t.Fatalf("probeMakeMKVVersion() = %+v, want parsed banner despite non-zero exit", got)
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
		listJobs: func(_ context.Context, limit, offset int) ([]store.Job, error) {
			if limit != jobsPageSize || offset != 0 {
				t.Errorf("ListJobs(limit, offset) = (%d, %d), want (%d, 0)", limit, offset, jobsPageSize)
			}
			return jobs, nil
		},
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

func TestHandleListJobs_Pagination(t *testing.T) {
	ms := &mockStore{
		listJobs: func(_ context.Context, limit, offset int) ([]store.Job, error) {
			if limit != 25 || offset != 100 {
				t.Errorf("ListJobs(limit, offset) = (%d, %d), want (25, 100)", limit, offset)
			}
			return []store.Job{}, nil
		},
	}
	s := newTestServer(ms)
	rr := doRequest(t, s, http.MethodGet, "/api/jobs?limit=25&offset=100", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestHandleListJobs_RejectsInvalidPagination(t *testing.T) {
	s := newTestServer(&mockStore{})
	for _, path := range []string{"/api/jobs?limit=101", "/api/jobs?limit=0", "/api/jobs?offset=-1", "/api/jobs?offset=abc"} {
		rr := doRequest(t, s, http.MethodGet, path, nil)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want 400", path, rr.Code)
		}
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
	// config.Defaults() has no TMDB credentials, so search is unavailable.
	s := newTestServer(nil)
	rr := doRequest(t, s, http.MethodGet, "/api/search?q=dune", nil)

	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rr.Code)
	}
}

func TestHandleReidentify_TVSelectionRequiresSeasonAndPersistsSelection(t *testing.T) {
	var eventData map[string]any
	st := &mockStore{
		getJob: func(_ context.Context, id string) (store.Job, []store.JobEvent, error) {
			return store.Job{ID: id, Device: "/dev/sr0", Status: "identifying"}, nil, nil
		},
		addEvent: func(_ context.Context, _, _, _ string, data any) error {
			encoded, err := json.Marshal(data)
			if err != nil {
				return err
			}
			return json.Unmarshal(encoded, &eventData)
		},
	}
	s := newTestServer(st)

	rr := doRequest(t, s, http.MethodPost, "/api/jobs/job-1/reidentify", []byte(`{
		"tmdb_id": 1438,
		"title": "The Wire",
		"year": 2002,
		"media_type": "tv",
		"season": 2,
		"episode_start": 4
	}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if eventData["media_type"] != "tv" || eventData["season"] != float64(2) || eventData["episode_start"] != float64(4) {
		t.Fatalf("persisted selection = %#v", eventData)
	}

	rr = doRequest(t, s, http.MethodPost, "/api/jobs/job-1/reidentify", []byte(`{
		"tmdb_id": 1438,
		"title": "The Wire",
		"year": 2002,
		"media_type": "tv"
	}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing season status = %d, want 400", rr.Code)
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

func TestHandleEject_Success(t *testing.T) {
	s := newTestServer(nil)
	s.devices = []string{"/dev/sr0", "/dev/sr1"}

	origEject := ejectDevice
	t.Cleanup(func() { ejectDevice = origEject })

	var gotDevice string
	ejectDevice = func(device string) error {
		gotDevice = device
		return nil
	}

	body := []byte(`{"device":"/dev/sr1"}`)
	rr := doRequest(t, s, http.MethodPost, "/api/eject", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if gotDevice != "/dev/sr1" {
		t.Fatalf("eject called with %q, want /dev/sr1", gotDevice)
	}
}

func TestHandleEject_UnknownDevice(t *testing.T) {
	s := newTestServer(nil)
	s.devices = []string{"/dev/sr0"}

	body := []byte(`{"device":"/dev/sr1"}`)
	rr := doRequest(t, s, http.MethodPost, "/api/eject", body)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", rr.Code, rr.Body.String())
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

func TestHandleDeleteJob(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"ok", nil, http.StatusNoContent},
		{"missing", store.ErrNotFound, http.StatusNotFound},
		{"active", store.ErrJobActive, http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(&mockStore{deleteJob: func(context.Context, string) error { return tc.err }})
			if rr := doRequest(t, s, http.MethodDelete, "/api/jobs/x", nil); rr.Code != tc.want {
				t.Fatalf("status = %d, want %d", rr.Code, tc.want)
			}
		})
	}
}

func TestHandleDeleteFinishedJobs(t *testing.T) {
	s := newTestServer(&mockStore{})
	rr := doRequest(t, s, http.MethodDelete, "/api/jobs", nil)
	var got map[string]int64
	decodeJSON(t, rr, &got)
	if rr.Code != http.StatusOK || got["deleted"] != 3 {
		t.Fatalf("status=%d body=%v", rr.Code, got)
	}
}

func TestUpgraderCheckOrigin(t *testing.T) {
	tests := []struct {
		name   string
		origin string
		tls    bool
		want   bool
	}{
		{"no origin header", "", false, true},
		{"same origin http", "http://rip.local:8080", false, true},
		{"same origin https", "https://rip.local:8080", true, true},
		{"scheme mismatch", "https://rip.local:8080", false, false},
		{"other host", "http://evil.example:8080", false, false},
		{"other port", "http://rip.local:9090", false, false},
		{"origin with path", "http://rip.local:8080/x", false, false},
		{"unparseable", "http://%zz", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/ws/progress", nil)
			r.Host = "rip.local:8080"
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			if tt.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if got := upgrader.CheckOrigin(r); got != tt.want {
				t.Errorf("CheckOrigin(%q) = %v, want %v", tt.origin, got, tt.want)
			}
		})
	}
}
