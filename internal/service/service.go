// Package service implements the core business logic for SimpleRip.
// It orchestrates disc scanning, ripping, cleaning, and notification workflows
// without containing any CLI-specific code.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/8bitreid/simplerip/internal/diagnose"
	"github.com/8bitreid/simplerip/internal/tools"

	"github.com/8bitreid/simplerip/internal/config"
	"github.com/8bitreid/simplerip/internal/disc"
	"github.com/8bitreid/simplerip/internal/inspect"
	"github.com/8bitreid/simplerip/internal/metadata"
	"github.com/8bitreid/simplerip/internal/notify"
	"github.com/8bitreid/simplerip/internal/output"
	"github.com/8bitreid/simplerip/internal/ripper"
	"github.com/8bitreid/simplerip/internal/store"
)

// ProgressEvent represents a state update during the rip process.
type ProgressEvent struct {
	Device      string `json:"device,omitempty"`       // e.g. /dev/sr0
	Stage       string `json:"stage"`                  // scanning, analyzing, ripping, delivering, done, idle, error
	Title       string `json:"title"`                  // e.g. "Revenge of the Sith"
	Percent     int    `json:"percent"`                // 0-100
	Message     string `json:"message"`                // human-readable status message
	DiscType    string `json:"disc_type,omitempty"`    // bluray, dvd, unknown
	DriveStatus string `json:"drive_status,omitempty"` // disc_present, no_disc, tray_open, loading, detecting, unresponsive
	ETASec      int    `json:"eta_seconds,omitempty"`  // estimated seconds remaining while ripping; 0 = unknown
}

type ManualSelection struct {
	Title        string
	Year         int
	MediaType    string
	Season       int
	EpisodeStart int
}

func pickLongest(titles []disc.MKVTitle) (disc.MKVTitle, bool) {
	if len(titles) == 0 {
		return disc.MKVTitle{}, false
	}
	best := titles[0]
	for _, t := range titles[1:] {
		if t.Duration > best.Duration {
			best = t
		}
	}
	return best, true
}

func findManualCorrection(events []store.JobEvent, since time.Time) (string, int, bool) {
	selection, ok := findManualSelection(events, since)
	return selection.Title, selection.Year, ok
}

func findManualSelection(events []store.JobEvent, since time.Time) (ManualSelection, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.CreatedAt.Before(since) {
			continue
		}
		if ev.Stage != "identify" {
			continue
		}
		if len(ev.Data) == 0 {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			continue
		}
		corr, ok := payload["correction"].(bool)
		if !ok || !corr {
			continue
		}
		title, _ := payload["title"].(string)
		switch v := payload["year"].(type) {
		case float64:
			selection := ManualSelection{Title: title, Year: int(v)}
			if selection = manualSelectionFromPayload(payload, selection); strings.TrimSpace(selection.Title) != "" {
				return selection, true
			}
		case int:
			selection := manualSelectionFromPayload(payload, ManualSelection{Title: title, Year: v})
			if strings.TrimSpace(selection.Title) != "" {
				return selection, true
			}
		default:
			selection := manualSelectionFromPayload(payload, ManualSelection{Title: title})
			if strings.TrimSpace(selection.Title) != "" {
				return selection, true
			}
		}
	}
	return ManualSelection{}, false
}

func manualSelectionFromPayload(payload map[string]any, selection ManualSelection) ManualSelection {
	selection.MediaType, _ = payload["media_type"].(string)
	selection.Season = intFromPayload(payload["season"])
	selection.EpisodeStart = intFromPayload(payload["episode_start"])
	if selection.MediaType == "" {
		selection.MediaType = "movie"
	}
	return selection
}

func intFromPayload(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return 0
	}
}

func (s *RipService) awaitManualSelection(ctx context.Context, jobID string, timeoutMin int) (ManualSelection, error) {
	if s.store == nil {
		return ManualSelection{}, errors.New("database not configured")
	}

	waitCtx := ctx
	cancel := func() {
		// No timeout configured: nothing to cancel.
	}
	if timeoutMin > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, time.Duration(timeoutMin)*time.Minute)
	}
	defer cancel()

	start := time.Now().UTC().Add(-1 * time.Second)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	for {
		job, events, err := s.store.GetJob(waitCtx, jobID)
		if err == nil {
			if selection, ok := findManualSelection(events, start); ok {
				if strings.TrimSpace(job.Title) != "" {
					selection.Title = job.Title
					selection.Year = job.Year
				}
				return selection, nil
			}
		}

		select {
		case <-waitCtx.Done():
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return ManualSelection{}, fmt.Errorf("manual title search timed out")
			}
			return ManualSelection{}, waitCtx.Err()
		case <-tick.C:
		}
	}
}

func (s *RipService) latestManualSelection(ctx context.Context, jobID string) (ManualSelection, bool, error) {
	if s.store == nil {
		return ManualSelection{}, false, nil
	}
	_, events, err := s.store.GetJob(ctx, jobID)
	if err != nil {
		return ManualSelection{}, false, fmt.Errorf("load manual media selection: %w", err)
	}
	selection, ok := findManualSelection(events, time.Time{})
	return selection, ok, nil
}

// EventBus is a simple channel-based fan-out for broadcasting progress events.
// Subscribers receive events on their individual channels.
type EventBus struct {
	mu          sync.RWMutex
	subscribers map[int]chan ProgressEvent
	nextID      int
}

// NewEventBus creates a new EventBus ready for use.
func NewEventBus() *EventBus {
	return &EventBus{
		subscribers: make(map[int]chan ProgressEvent),
	}
}

// Subscribe returns a channel that receives all future events.
// The caller must call Unsubscribe when done to avoid memory leaks.
func (bus *EventBus) Subscribe() (id int, ch <-chan ProgressEvent) {
	bus.mu.Lock()
	defer bus.mu.Unlock()
	id = bus.nextID
	bus.nextID++
	c := make(chan ProgressEvent, 256) // buffered so progress spam never drops terminal (done/error) events
	bus.subscribers[id] = c
	return id, c
}

// Unsubscribe removes a subscriber and closes its channel.
func (bus *EventBus) Unsubscribe(id int) {
	bus.mu.Lock()
	defer bus.mu.Unlock()
	if ch, ok := bus.subscribers[id]; ok {
		close(ch)
		delete(bus.subscribers, id)
	}
}

// Emit sends an event to all active subscribers.
// Non-blocking — if a subscriber's channel is full, the event is dropped for that subscriber.
func (bus *EventBus) Emit(event ProgressEvent) {
	bus.mu.RLock()
	defer bus.mu.RUnlock()
	for _, ch := range bus.subscribers {
		select {
		case ch <- event:
		default:
			// Drop event if subscriber is slow
		}
	}
}

// RipService orchestrates disc scanning, ripping, and delivery workflows.
// It is stateless and contains no CLI dependencies — all I/O is explicit.
type RipService struct {
	cfg      *config.Config
	notify   *notify.Client
	notifier *notify.Dispatcher
	eventBus *EventBus
	store    *store.Store

	// ripMu guards the live per-device rip state below.
	ripMu         sync.Mutex
	activeCancels map[string]context.CancelFunc
	// ripTitles maps a device to the current display/folder title of its
	// in-flight rip. A live re-identify updates this so both progress updates
	// and the final delivered filename pick up the corrected name.
	ripTitles map[string]string
	// lastEvent holds the most recent progress event per device, so a live
	// re-identify can re-emit it immediately with the new title.
	lastEvent map[string]ProgressEvent
	// titleLocked marks devices whose title was set by the user, so automatic
	// identification never overwrites a manual correction.
	titleLocked map[string]bool
	titleFrozen map[string]bool // delivery has started; the name is final
	// pinnedRuntime is the runtime (minutes) of the movie the user chose by
	// hand, used to re-select which title to rip. Absent = no manual runtime.
	pinnedRuntime map[string]int
	// restarts holds the restart handle of each in-flight rip.
	restarts map[string]*restartState
	// alternates holds rippable alternate cuts per job; altBusy marks drives
	// busy with an alternate rip.
	alternates map[string]*altState
	altBusy    map[string]bool
}

var (
	newTMDBClient = metadata.NewClient
	newOMDbClient = metadata.NewOMDbClient
)

func (s *RipService) tmdbConfigured() bool {
	return s.cfg.Metadata.TMDBApiKey != "" || s.cfg.Metadata.TMDBAccessToken != ""
}

func (s *RipService) tmdbClient() *metadata.Client {
	return newTMDBClient(s.cfg.Metadata.TMDBApiKey).WithAccessToken(s.cfg.Metadata.TMDBAccessToken)
}

// New creates a RipService with the given configuration.
// st may be nil — all store calls become no-ops, preserving existing behaviour.
func New(cfg *config.Config, st *store.Store) *RipService {
	return &RipService{
		cfg:       cfg,
		notify:    notify.NewClient(cfg.Notification.WebhookURL),
		notifier:  newNotifier(cfg.Notification),
		eventBus:  NewEventBus(),
		store:     st,
		ripTitles: make(map[string]string),
		lastEvent: make(map[string]ProgressEvent),

		titleLocked: make(map[string]bool),
		titleFrozen: make(map[string]bool),

		pinnedRuntime: make(map[string]int),
		restarts:      make(map[string]*restartState),
		alternates:    make(map[string]*altState),
		altBusy:       make(map[string]bool),
	}
}

// emit records the event as the device's latest state, then broadcasts it.
// All progress emission inside RipService goes through here so a live
// re-identify can re-emit the most recent event with a corrected title.
func (s *RipService) emit(ev ProgressEvent) {
	if ev.Device != "" {
		s.ripMu.Lock()
		s.lastEvent[ev.Device] = ev
		s.ripMu.Unlock()
	}
	s.eventBus.Emit(ev)
}

// beginRipTitle registers (or updates) the live title for a device's rip.
func (s *RipService) beginRipTitle(device, folder string) {
	s.ripMu.Lock()
	s.ripTitles[device] = folder
	s.ripMu.Unlock()
}

// setAutoTitle records an automatically identified title unless the user has
// already corrected it by hand.
func (s *RipService) setAutoTitle(device, folder string) {
	s.ripMu.Lock()
	defer s.ripMu.Unlock()
	if !s.titleLocked[device] {
		s.ripTitles[device] = folder
	}
}

// freezeTitle returns the final title and rejects further edits, since the
// delivered folder and file names are derived from it.
func (s *RipService) freezeTitle(device, fallback string) string {
	s.ripMu.Lock()
	defer s.ripMu.Unlock()
	s.titleFrozen[device] = true
	if t, ok := s.ripTitles[device]; ok {
		return t
	}
	return fallback
}

// needsConfirmation reports whether a ripped single title should be held back
// from delivery: the identity was automatic and either unconfirmed or its runtime doesn't fit.
func needsConfirmation(main []disc.MKVTitle, files int, userLocked bool, tmdbConfirmed bool, tmdbEnabled bool, runtimeMin int) bool {
	if len(main) != 1 || files != 1 || userLocked {
		return false
	}
	if !tmdbEnabled {
		return false
	}
	if !tmdbConfirmed {
		return true
	}
	return durationMismatch(main[0].Duration, runtimeMin)
}

func (s *RipService) titleIsLocked(device string) bool {
	s.ripMu.Lock()
	defer s.ripMu.Unlock()
	return s.titleLocked[device]
}

// userChoseTitle reports whether the user set this rip's title by hand, now or
// during a held-for-confirmation wait.
func (s *RipService) userChoseTitle(device string, run *ripRun) bool {
	return s.titleIsLocked(device) || run.confirmed
}

// TitleFrozen reports whether the rip on device has started delivering.
func (s *RipService) TitleFrozen(device string) bool {
	s.ripMu.Lock()
	defer s.ripMu.Unlock()
	return s.titleFrozen[device]
}

// endRipTitle clears the live title once a rip finishes.
func (s *RipService) endRipTitle(device string) {
	s.ripMu.Lock()
	delete(s.ripTitles, device)
	delete(s.titleLocked, device)
	delete(s.titleFrozen, device)
	delete(s.pinnedRuntime, device)
	s.ripMu.Unlock()
}

// currentTitle returns the live title for a device's rip, or fallback if none.
func (s *RipService) currentTitle(device, fallback string) string {
	s.ripMu.Lock()
	defer s.ripMu.Unlock()
	if v, ok := s.ripTitles[device]; ok && v != "" {
		return v
	}
	return fallback
}

// ReidentifyRip overrides the title of an in-flight rip on device so progress
// updates and the final delivered filename use the corrected name. It returns
// false when no rip is active on that device (re-identify only applies during a
// rip — a delivered file is out of our control). On success it immediately
// re-emits the device's latest progress event with the new title so connected
// UIs update without waiting for the next progress tick.
func (s *RipService) ReidentifyRip(device, folder string) bool {
	s.ripMu.Lock()
	if _, ok := s.ripTitles[device]; !ok || s.titleFrozen[device] {
		s.ripMu.Unlock()
		return false
	}
	s.ripTitles[device] = folder
	s.titleLocked[device] = true
	last, hasLast := s.lastEvent[device]
	s.ripMu.Unlock()

	if hasLast {
		last.Title = folder
		if last.Stage == "ripping" {
			last.Message = fmt.Sprintf("Ripping %s (%d%%)", folder, last.Percent)
		}
		s.emit(last)
	}
	return true
}

// SetRipRuntime records the runtime of the movie a user picked for the rip on
// device, so title selection can be redone against it. A non-positive runtime
// clears any earlier value. It is a no-op when no rip is active.
func (s *RipService) SetRipRuntime(device string, minutes int) {
	s.ripMu.Lock()
	defer s.ripMu.Unlock()
	if _, ok := s.ripTitles[device]; !ok {
		return
	}
	if minutes > 0 {
		s.pinnedRuntime[device] = minutes
	} else {
		delete(s.pinnedRuntime, device)
	}
}

func (s *RipService) ripRuntime(device string) int {
	s.ripMu.Lock()
	defer s.ripMu.Unlock()
	return s.pinnedRuntime[device]
}

// RuntimeFor looks up the reconciled runtime in minutes for a TMDB movie ID.
// It returns 0 when unknown (no TMDB credential or lookup failure).
func (s *RipService) RuntimeFor(ctx context.Context, tmdbID int) int {
	if tmdbID <= 0 {
		return 0
	}
	details, err := s.EnrichMovie(ctx, metadata.MovieResult{ID: tmdbID})
	if err != nil || details == nil {
		return 0
	}
	return details.RuntimeMinutes
}

// EventBus returns the service's EventBus for subscribing to progress updates.
func (s *RipService) EventBus() *EventBus {
	return s.eventBus
}

// MarkDeviceIdle emits an idle progress event for a device, e.g. after its disc
// has been removed, so subscribed UIs reset that drive's card to "no disc".
func (s *RipService) MarkDeviceIdle(device string) {
	s.emit(ProgressEvent{
		Device:  device,
		Stage:   "idle",
		Percent: 0,
		Message: "no disc — waiting",
	})
}

// SetDriveStatus shows a disc-detection status on an idle drive's card. It is
// ignored while the drive has an active or finished job (anything but idle), so
// it never overwrites rip progress or a "done"/"error" result.
func (s *RipService) SetDriveStatus(device, message string) {
	s.ripMu.Lock()
	last, ok := s.lastEvent[device]
	s.ripMu.Unlock()
	if ok && last.Stage != "idle" {
		return
	}
	if ok && last.Message == message {
		return
	}
	s.emit(ProgressEvent{Device: device, Stage: "idle", Message: message})
}

// ScanDisc scans a physical disc device and classifies its titles.
// Returns the classification result suitable for decision-making.
// device is the optical drive path (e.g. /dev/sr0).
func (s *RipService) ScanDisc(device string) (*ripper.ClassificationResult, error) {
	ctx := context.Background()

	// Scan the disc with makemkvcon.
	scanned, err := s.scanInfo(ctx, device)
	if err != nil {
		return nil, fmt.Errorf("scan device %q: %w", device, err)
	}

	// Classify titles according to detection rules.
	result := ripper.ClassifyTitles(scanned.Titles, s.cfg.Detection)
	return &result, nil
}

func (s *RipService) scanInfo(ctx context.Context, device string) (*disc.ClassifiedDisc, error) {
	timeoutMinutes := s.cfg.MakeMKV.TimeoutMinutes
	if timeoutMinutes <= 0 {
		timeoutMinutes = 120
	}
	scanCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMinutes)*time.Minute)
	defer cancel()
	return ripper.ScanInfo(scanCtx, tools.Path("makemkvcon"), device, s.cfg.MakeMKV.Key)
}

func (s *RipService) ripTVTitles(
	ctx context.Context,
	device, discName, mediaTitle string,
	job store.Job,
	titles []disc.MKVTitle,
	outputDir string,
	discType disc.DiscType,
) ([]string, []int, error) {
	total := len(titles)
	if total == 0 {
		return nil, nil, fmt.Errorf("no TV titles selected")
	}
	analyzeBudget := s.cfg.MakeMKV.BatchAnalyzeBudgetMinutes
	if analyzeBudget < 1 {
		analyzeBudget = 45
	}
	saveBudget := s.cfg.MakeMKV.BatchSaveBudgetMinutes
	if saveBudget < 1 {
		saveBudget = 10
	}
	timeoutMinutes, err := ripper.CalculateBatchTimeoutMinutes(analyzeBudget, saveBudget, total)
	if err != nil {
		return nil, nil, fmt.Errorf("calculate TV batch timeout: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < time.Duration(timeoutMinutes)*time.Minute {
		slog.Warn("parent context deadline is shorter than the TV batch timeout",
			"device", device, "batch_timeout_minutes", timeoutMinutes,
			"parent_remaining", time.Until(deadline).String())
	}

	slog.Info("rip phase started", "phase", "analyzing", "device", device,
		"disc", discName, "title_count", total, "batch", true)
	s.emit(ProgressEvent{
		Device:  device,
		Stage:   "analyzing",
		Title:   s.currentTitle(device, mediaTitle),
		Percent: 0,
		Message: fmt.Sprintf("Analyzing %d selected TV titles", total),
	})
	if s.store != nil {
		for _, title := range titles {
			audioDesc := fmt.Sprintf("%.1fh, %d audio tracks", title.Duration.Hours(), title.AudioTrackCount)
			_ = s.store.AddEvent(ctx, job.ID, "score",
				fmt.Sprintf("selected title %d: %s", title.Index, audioDesc),
				map[string]any{
					"selected_index": title.Index,
					"duration":       title.Duration.String(),
					"size_gb":        title.SizeGB,
					"audio_tracks":   title.AudioTrackCount,
					"chapters":       title.ChapterCount,
				})
		}
	}

	positions := make(map[int]int, total)
	titleNames := make(map[int]string, total)
	for i, title := range titles {
		positions[title.Index] = i
		titleNames[title.Index] = title.Name
	}
	etaTr := &etaTracker{}
	analyzeStartedAt := time.Now()
	lastReportedPct := -10
	saveStarted := false
	progressCb := func(titleIndex, percent int, phase ripper.RipPhase) {
		cur := s.currentTitle(device, mediaTitle)
		if phase == ripper.PhaseAnalyze {
			s.emit(ProgressEvent{
				Device:  device,
				Stage:   "analyzing",
				Title:   cur,
				Percent: percent,
				Message: fmt.Sprintf("Analyzing %d selected TV titles (%d%%)", total, percent),
			})
			return
		}
		if !saveStarted {
			saveStarted = true
			slog.Info("rip phase completed", "phase", "analyzing", "device", device,
				"disc", discName, "title_count", total, "duration", time.Since(analyzeStartedAt).String())
			slog.Info("rip phase started", "phase", "ripping", "device", device,
				"disc", discName, "title_count", total, "batch", true)
		}
		overall := percent
		titleLabel := fmt.Sprintf("%d of %d", 1, total)
		if position, ok := positions[titleIndex]; ok {
			overall = (position*100 + percent) / total
			titleLabel = fmt.Sprintf("%d of %d", position+1, total)
			if name := strings.TrimSpace(titleNames[titleIndex]); name != "" {
				titleLabel += ": " + name
			}
		}
		overall = min(overall, 99)
		remaining := etaTr.Update(time.Now(), overall)
		s.emit(ProgressEvent{
			Device:  device,
			Stage:   "ripping",
			Title:   cur,
			Percent: overall,
			Message: fmt.Sprintf("Ripping TV title %s (%d%%)", titleLabel, overall),
			ETASec:  int(remaining.Seconds()),
		})
		if s.store != nil && overall >= lastReportedPct+10 {
			lastReportedPct = (overall / 10) * 10
			_ = s.store.AddEvent(ctx, job.ID, "rip", fmt.Sprintf("progress: %d%%", overall), nil)
		}
	}

	batchOutputDir, err := os.MkdirTemp(outputDir, "batch-")
	if err != nil {
		return nil, nil, fmt.Errorf("create TV batch output directory: %w", err)
	}
	batchFiles, batchErr := ripper.RipTitles(ctx, device, titles, batchOutputDir, s.ripOptions(timeoutMinutes, discType, progressCb))
	if errors.Is(context.Cause(ctx), errRestart) {
		return nil, nil, errRestart
	}
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	mappedFiles, err := ripper.MapTitleFiles(titles, batchFiles)
	if err != nil {
		return nil, nil, fmt.Errorf("map MakeMKV batch outputs: %w", err)
	}
	if batchErr != nil {
		// RipTitles already deleted unfinished outputs; everything it returned is
		// a finished title, whichever title the error names.
		slog.Warn("TV batch rip ended with an error; preserving completed title files",
			"disc", discName, "error", batchErr, "completed_title_count", len(mappedFiles),
			"selected_title_count", total)
	}

	missing := make([]disc.MKVTitle, 0, total-len(mappedFiles))
	for _, title := range titles {
		if _, ok := mappedFiles[title.Index]; !ok {
			missing = append(missing, title)
		}
	}
	if len(missing) > 0 {
		slog.Info("rip phase started", "phase", "recovering", "device", device,
			"disc", discName, "missing_title_count", len(missing), "batch_error", batchErr)
		for _, title := range missing {
			position := positions[title.Index]
			slog.Info("rip phase started", "phase", "analyzing", "device", device,
				"disc", discName, "title_index", title.Index, "title_number", position+1,
				"title_count", total, "fallback", true)
			singleTimeout := s.cfg.MakeMKV.TimeoutMinutes
			if singleTimeout < 1 {
				singleTimeout = 120
			}
			retryLimit := s.cfg.MakeMKV.MaxRipRetries
			if retryLimit < 0 {
				retryLimit = 0
			}
			var ripErr error
			var titleFile string
			for attempt := 1; attempt <= retryLimit+1; attempt++ {
				attemptDir, err := os.MkdirTemp(outputDir, fmt.Sprintf("fallback-title-%d-", title.Index))
				if err != nil {
					return nil, nil, fmt.Errorf("create recovery output directory for title %d: %w", title.Index, err)
				}
				fallbackSaveStarted := false
				fallbackStartedAt := time.Now()
				fallbackProgress := func(_ int, pct int, phase ripper.RipPhase) {
					overall := min((position*100+pct)/total, 99)
					if phase == ripper.PhaseAnalyze {
						s.emit(ProgressEvent{
							Device:  device,
							Stage:   "analyzing",
							Title:   s.currentTitle(device, mediaTitle),
							Percent: overall,
							Message: fmt.Sprintf("Analyzing recovery title %d of %d (%d%%)", position+1, total, pct),
						})
						return
					}
					if phase == ripper.PhaseSave && !fallbackSaveStarted {
						fallbackSaveStarted = true
						slog.Info("rip phase completed", "phase", "analyzing", "device", device,
							"disc", discName, "title_index", title.Index,
							"duration", time.Since(fallbackStartedAt).String(), "fallback", true)
						slog.Info("rip phase started", "phase", "ripping", "device", device,
							"disc", discName, "title_index", title.Index, "fallback", true)
					}
					remaining := etaTr.Update(time.Now(), overall)
					s.emit(ProgressEvent{
						Device:  device,
						Stage:   "ripping",
						Title:   s.currentTitle(device, mediaTitle),
						Percent: overall,
						Message: fmt.Sprintf("Recovering TV title %d of %d (%d%%)", position+1, total, overall),
						ETASec:  int(remaining.Seconds()),
					})
				}
				_, ripErr = ripper.RipTitle(ctx, device, title, attemptDir, s.ripOptions(singleTimeout, discType, fallbackProgress))
				if errors.Is(context.Cause(ctx), errRestart) {
					return nil, nil, errRestart
				}
				if ctx.Err() != nil {
					if cleanupErr := os.RemoveAll(attemptDir); cleanupErr != nil {
						return nil, nil, fmt.Errorf("remove canceled title %d recovery output: %w", title.Index, cleanupErr)
					}
					return nil, nil, ctx.Err()
				}
				allFiles, listErr := listMKVFiles(attemptDir)
				if listErr != nil {
					if cleanupErr := os.RemoveAll(attemptDir); cleanupErr != nil {
						return nil, nil, fmt.Errorf("%v; remove failed recovery output directory: %w", listErr, cleanupErr)
					}
					return nil, nil, listErr
				}
				fallbackFiles, mapErr := ripper.MapTitleFiles([]disc.MKVTitle{title}, allFiles)
				if mapErr != nil {
					if cleanupErr := os.RemoveAll(attemptDir); cleanupErr != nil {
						return nil, nil, fmt.Errorf("%v; remove failed recovery output directory: %w", mapErr, cleanupErr)
					}
					return nil, nil, mapErr
				}
				titleFile = fallbackFiles[title.Index]
				if ripErr == nil {
					if titleFile == "" {
						ripErr = fmt.Errorf("MakeMKV did not create an output for title index %d", title.Index)
					} else {
						slog.Info("rip phase completed", "phase", "ripping", "device", device,
							"disc", discName, "title_index", title.Index,
							"title_number", position+1, "title_count", total,
							"fallback", true, "duration", time.Since(fallbackStartedAt).String())
						break
					}
				}
				if ripErr != nil {
					if err := os.RemoveAll(attemptDir); err != nil {
						return nil, nil, fmt.Errorf("remove failed title %d recovery output: %w", title.Index, err)
					}
					titleFile = ""
				}
				if attempt < retryLimit+1 {
					slog.Warn("retrying missing TV title", "disc", discName,
						"title_index", title.Index, "attempt", attempt+1,
						"attempts", retryLimit+1, "error", ripErr)
				}
			}
			if ripErr != nil || titleFile == "" {
				return nil, nil, fmt.Errorf("recover TV title %d: %w", title.Index, ripErr)
			}
			mappedFiles[title.Index] = titleFile
		}
	}

	rippedFiles := make([]string, 0, total)
	titleIndices := make([]int, 0, total)
	for _, title := range titles {
		file, ok := mappedFiles[title.Index]
		if !ok {
			return nil, nil, fmt.Errorf("TV title index %d has no mapped output file", title.Index)
		}
		rippedFiles = append(rippedFiles, file)
		titleIndices = append(titleIndices, title.Index)
		if s.store != nil {
			fi, _ := os.Stat(file)
			sizeGB := 0.0
			if fi != nil {
				sizeGB = float64(fi.Size()) / (1024 * 1024 * 1024)
			}
			_ = s.store.AddEvent(ctx, job.ID, "rip",
				fmt.Sprintf("complete: %s (%.1f GB)", filepath.Base(file), sizeGB),
				map[string]any{"file": filepath.Base(file), "size_gb": sizeGB})
		}
	}
	slog.Info("rip phase completed", "phase", "ripping", "device", device,
		"disc", discName, "title_count", total, "file_count", len(rippedFiles), "batch", true)
	return rippedFiles, titleIndices, nil
}

func listMKVFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read rip output directory %q: %w", dir, err)
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), ".mkv") {
			files = append(files, filepath.Join(dir, entry.Name()))
		}
	}
	return files, nil
}

// RipDisc executes the full automated pipeline:
//  1. Scan the disc with makemkvcon to get raw disc data (DiscName, titles, durations)
//  2. Classify titles according to detection rules (TV/Movie/Ambiguous patterns)
//  3. TMDB metadata lookup using DiscName (auto-selects best runtime match for daemon mode)
//  4. Rip main titles with enriched metadata (progress shows actual movie title)
//  5. Deliver files to NAS with proper folder naming (e.g. "Title (Year)/Title (Year).mkv")
//  6. Clean up staging directory after successful delivery
//  7. Send Discord notification on completion
//
// This method is intended for unattended daemon mode. TMDB lookup is optional — if
// the API key is not configured or lookup fails, the raw DiscName is used instead.
// Returns an error if scan/rip/delivery steps fail. Notification is best-effort.
func (s *RipService) RipDisc(ctx context.Context, device string) error {
	ctx, cancel := context.WithCancel(ctx)
	s.ripMu.Lock()
	if s.activeCancels == nil {
		s.activeCancels = make(map[string]context.CancelFunc)
	}
	s.activeCancels[device] = cancel
	s.ripMu.Unlock()
	defer func() {
		cancel()
		s.ripMu.Lock()
		delete(s.activeCancels, device)
		s.ripMu.Unlock()
	}()
	run := &ripRun{}
	defer s.endRipTitle(device)

	var err error
	for {
		err = s.ripDisc(ctx, device, run)
		if !errors.Is(err, errRestart) {
			break
		}
		run.restarted = true
	}
	// Cancellation is terminal, but remains distinct from a rip failure.
	if errors.Is(err, context.Canceled) {
		const message = "Rip canceled"
		slog.Info("rip cancelled", "device", device, "disc", run.disc, "job", run.jobID)
		if s.store != nil {
			persistCtx, persistCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if run.jobID == "" {
				job, createErr := s.store.CreateJob(persistCtx, device, run.disc, "")
				if createErr != nil {
					slog.Error("failed to create canceled rip job", "device", device, "error", createErr)
				} else {
					run.jobID = job.ID
				}
			}
			if run.jobID != "" {
				if persistErr := s.store.AddEvent(persistCtx, run.jobID, "cancelled", message, nil); persistErr != nil {
					slog.Error("failed to record canceled rip event", "device", device, "job", run.jobID, "error", persistErr)
				}
				if persistErr := s.store.UpdateStatus(persistCtx, run.jobID, "cancelled"); persistErr != nil {
					slog.Error("failed to mark rip as cancelled", "device", device, "job", run.jobID, "error", persistErr)
				}
			}
			persistCancel()
		}
		s.emit(ProgressEvent{Device: device, Stage: "cancelled", Message: message})
	} else if err != nil {
		slog.Error("rip failed", "device", device, "disc", run.disc, "job", run.jobID, "error", err)
	} else {
		slog.Info("rip completed", "device", device, "disc", run.disc, "job", run.jobID, "title", run.title)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		s.notifier.Notify(notify.Message{
			Event:   notify.EventFailed,
			JobID:   run.jobID,
			Disc:    run.disc,
			Device:  device,
			Title:   run.title,
			Summary: err.Error(),
		})
	}
	return err
}

// CancelRip cancels the active pipeline on device, including scanning and
// delivery. It returns false when that device has no active pipeline.
func (s *RipService) CancelRip(device string) bool {
	s.ripMu.Lock()
	cancel := s.activeCancels[device]
	s.ripMu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// HasActiveRip reports whether a pipeline is currently running on device.
func (s *RipService) HasActiveRip(device string) bool {
	s.ripMu.Lock()
	defer s.ripMu.Unlock()
	return s.activeCancels[device] != nil
}

func (s *RipService) ripDisc(ctx context.Context, device string, run *ripRun) error {
	// job tracks the DB record; zero value is safe when s.store == nil.
	var job store.Job

	// Step 1: Scan disc.
	slog.Info("rip phase started", "phase", "scanning", "device", device, "restart", run.restarted)
	s.emit(ProgressEvent{
		Device:  device,
		Stage:   "scanning",
		Title:   "",
		Percent: 0,
		Message: fmt.Sprintf("Scanning disc in %s", device),
	})

	scanned := run.scanned
	if scanned == nil {
		var err error
		scanned, err = s.scanInfo(ctx, device)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
				return fmt.Errorf("scan device %q: %w", device, context.Canceled)
			}
			slog.Error("rip phase failed", "phase", "scanning", "device", device, "error", err)
			scanDiag := diagnose.Scan(err)
			friendlyMsg := scanDiag.Summary
			s.emit(ProgressEvent{
				Device:  device,
				Stage:   "error",
				Title:   "",
				Percent: 0,
				Message: friendlyMsg,
			})
			if s.store != nil {
				persistCtx := ctx
				persistCancel := func() {
					// Parent context still live: nothing to cancel.
				}
				if ctx.Err() != nil {
					persistCtx, persistCancel = context.WithTimeout(context.Background(), 5*time.Second)
				}
				defer persistCancel()
				if failedJob, createErr := s.store.CreateJob(persistCtx, device, "", ""); createErr == nil {
					run.jobID = failedJob.ID
					data := scanDiag.Data()
					data["device"] = device
					if eventErr := s.store.AddEvent(persistCtx, failedJob.ID, "error", friendlyMsg, data); eventErr != nil {
						slog.Error("failed to record scan failure", "device", device, "job", failedJob.ID, "error", eventErr)
					}
					if statusErr := s.store.UpdateStatus(persistCtx, failedJob.ID, "error"); statusErr != nil {
						slog.Error("failed to mark scan failure", "device", device, "job", failedJob.ID, "error", statusErr)
					}
				} else {
					slog.Error("failed to create job for scan failure", "device", device, "error", createErr)
				}
			}
			return fmt.Errorf("scan device %q: %w", device, err)
		}
	}
	run.scanned = scanned

	discTypeStr := scanned.Type.String()
	slog.Info("rip phase completed", "phase", "scanning", "device", device,
		"disc", scanned.DiscName, "disc_type", discTypeStr, "title_count", len(scanned.Titles))

	// Broadcast disc type to the drive card immediately after scan completes.
	s.emit(ProgressEvent{
		Device:   device,
		Stage:    "scanning",
		DiscType: discTypeStr,
		Percent:  100,
		Message:  fmt.Sprintf("Found %d titles (%s)", len(scanned.Titles), scanned.Type),
	})

	// Create the DB job record now so TMDB events have a job ID to attach to.
	// A restart keeps the original job so its history stays in one place.
	if run.restarted {
		job = run.job
	} else if s.store != nil {
		job, _ = s.store.CreateJob(ctx, device, scanned.DiscName, discTypeStr)
	}
	run.job = job
	run.jobID, run.disc = job.ID, scanned.DiscName
	if !run.restarted {
		run.title = scanned.DiscName
	}

	// Register the live title as soon as the job exists so a re-identify at any
	// point (identify, analyze, rip) corrects the progress display and the final
	// delivered folder/file name. Cleared when the job ends (any return path).
	// The wrapper clears it, so a restart keeps the corrected title.
	if !run.restarted {
		s.beginRipTitle(device, scanned.DiscName)
	}

	// Step 2: TMDB lookup — run before classification so the result can inform it.
	// Use the longest title's duration as a proxy for the main feature runtime.
	mediaTitle := scanned.DiscName
	tmdbConfirmedMovie := false
	mediaType := run.mediaType
	season := run.season
	episodeStart := run.episodeStart
	episodeNumbers := run.episodeNumbers
	autoTVMatch := false
	tvSuggestion := ""
	// Do not let an unrelated movie match hide a disc that looks like a TV set.
	preliminaryDisc := ripper.ClassifyTitles(scanned.Titles, s.cfg.Detection)
	runtimeMin := 0
	manualIdentity := false
	if !run.restarted {
		selection, ok, selectionErr := s.latestManualSelection(ctx, job.ID)
		if selectionErr != nil {
			return selectionErr
		}
		if ok {
			mediaType, season, episodeStart = selection.MediaType, selection.Season, selection.EpisodeStart
			episodeNumbers = nil
			job.Title, job.Year = selection.Title, selection.Year
			mediaTitle = selection.Title
			if selection.Year > 0 {
				mediaTitle = fmt.Sprintf("%s (%d)", selection.Title, selection.Year)
			}
			manualIdentity = true
			autoTVMatch = false
		}
	}
	slog.Info("rip phase started", "phase", "identifying", "device", device,
		"disc", scanned.DiscName, "tmdb_enabled", s.tmdbConfigured())
	if run.restarted {
		// The user chose the movie by hand; don't second-guess it.
		mediaTitle = s.currentTitle(device, mediaTitle)
		tmdbConfirmedMovie = true
		runtimeMin = s.ripRuntime(device)
	} else if !manualIdentity && mediaType == "" && preliminaryDisc.Pattern == ripper.DiscPatternTV {
		lookupCtx, lookupCancel := context.WithTimeout(ctx, 20*time.Second)
		tv := s.identifyTV(lookupCtx, scanned.DiscName, preliminaryDisc.MainTitles)
		lookupCancel()
		if errors.Is(ctx.Err(), context.Canceled) {
			return context.Canceled
		}
		if tv.ShowCertain && tv.Show != nil {
			autoTVMatch = true
			mediaType = "tv"
			mediaTitle = tv.Show.Title
			if yr, _ := strconv.Atoi(tv.Show.Year); yr > 0 {
				mediaTitle = fmt.Sprintf("%s (%d)", mediaTitle, yr)
			}
			season = tv.Season
			episodeNumbers = tv.Episodes
			run.mediaType, run.season = mediaType, season
			run.episodeStart, run.episodeNumbers = 0, episodeNumbers
			if s.store != nil {
				year, _ := strconv.Atoi(tv.Show.Year)
				job.Title, job.Year = tv.Show.Title, year
				if err := s.store.UpdateAutoIdentity(ctx, job.ID, tv.Show.Title, year); err != nil {
					slog.Error("failed to update automatic TV identity", "job", job.ID, "error", err)
				}
			} else if tv.Show != nil {
				tvSuggestion = tv.Show.Title
				if yr, _ := strconv.Atoi(tv.Show.Year); yr > 0 {
					tvSuggestion = fmt.Sprintf("%s (%d)", tvSuggestion, yr)
				}
			}
		}
		if s.store != nil {
			message := "TV identification evidence recorded"
			if tv.LookupError != "" {
				message += "; continuing with safe fallback: " + tv.LookupError
			}
			if tv.ShowCertain && tv.Season == 0 {
				message += "; show matched, season unresolved"
			}
			if tv.ShowCertain && tv.Season > 0 && len(tv.Episodes) == 0 {
				message += "; season matched, episode mapping unresolved"
			}
			if err := s.store.AddEvent(ctx, job.ID, "identify", message, tvIdentificationEventData(tv)); err != nil {
				slog.Error("failed to record TV identification evidence", "job", job.ID, "error", err)
			}
		}
		if tv.LookupError != "" {
			slog.Warn("TV metadata lookup incomplete; continuing with disc evidence",
				"disc", scanned.DiscName, "error", tv.LookupError)
		}
	} else if !manualIdentity && mediaType == "" && s.tmdbConfigured() && scanned.DiscName != "" && preliminaryDisc.Pattern != ripper.DiscPatternTV {
		s.emit(ProgressEvent{
			Device:   device,
			Stage:    "identifying",
			DiscType: discTypeStr,
			Title:    "",
			Percent:  0,
			Message:  fmt.Sprintf("Looking up metadata for %q", scanned.DiscName),
		})

		query := metadata.QueryFromDirName(scanned.DiscName)
		movies, err := s.SearchMovie(ctx, query)
		if err == nil && len(movies) > 0 {
			// Use the longest title's duration for runtime matching (classification
			// hasn't run yet, so we don't have a MainTitle to reference).
			var longestDuration time.Duration
			for _, t := range scanned.Titles {
				if t.Duration > longestDuration {
					longestDuration = t.Duration
				}
			}

			tmdbClient := s.tmdbClient()
			chosen, runtimeWinner, logMsg, err := metadata.BestMatch(ctx, tmdbClient, movies, longestDuration)
			if err == nil {
				if runtimeWinner && logMsg != "" {
					slog.Info("tmdb runtime match selected", "message", logMsg)
				}
				details, err := s.EnrichMovie(ctx, chosen)
				if err == nil {
					runtimeMin = details.RuntimeMinutes
					tmdbConfirmedMovie = isConfidentMovieMatch(details.RuntimeMinutes, longestDuration)
					yr, _ := strconv.Atoi(details.Year)
					if tmdbConfirmedMovie {
						mediaType = "movie"
						mediaTitle = details.FolderName()
						if s.store != nil {
							matchReason := "best_match"
							if runtimeWinner {
								matchReason = "runtime_match"
							}
							_ = s.store.AddEvent(ctx, job.ID, "identify",
								fmt.Sprintf("identified as: %s (%d)", details.Title, yr),
								map[string]any{
									"tmdb_id":         chosen.ID,
									"title":           details.Title,
									"year":            yr,
									"runtime_minutes": details.RuntimeMinutes,
									"match_reason":    matchReason,
									"confirmed":       true,
								})
							if err := s.store.UpdateAutoIdentity(ctx, job.ID, details.Title, yr); err != nil {
								slog.Error("failed to update automatic movie identity", "job", job.ID, "error", err)
							}
							job.Title, job.Year = details.Title, yr
						}
					} else {
						slog.Warn("tmdb match unconfirmed: runtime does not match disc feature",
							"match", details.FolderName(),
							"runtime_min", details.RuntimeMinutes,
							"longest_duration", longestDuration.String(),
							"disc", scanned.DiscName)
						if s.store != nil {
							_ = s.store.AddEvent(ctx, job.ID, "identify",
								fmt.Sprintf("unconfirmed TMDB match %q (%d min vs disc %d min); continuing under disc label",
									details.Title, details.RuntimeMinutes, int(longestDuration.Minutes())),
								map[string]any{
									"tmdb_id":         chosen.ID,
									"suggested_title": details.Title,
									"year":            yr,
									"runtime_minutes": details.RuntimeMinutes,
									"confirmed":       false,
								})
						}
					}
				}
			} else {
				// Fall back to first result if BestMatch fails.
				details, err := s.EnrichMovie(ctx, movies[0])
				if err == nil {
					runtimeMin = details.RuntimeMinutes
					tmdbConfirmedMovie = false
					if s.store != nil {
						yr, _ := strconv.Atoi(details.Year)
						_ = s.store.AddEvent(ctx, job.ID, "identify",
							fmt.Sprintf("unconfirmed TMDB match %q (%d min vs disc %d min); continuing under disc label",
								details.Title, details.RuntimeMinutes, int(longestDuration.Minutes())),
							map[string]any{
								"tmdb_id":         movies[0].ID,
								"suggested_title": details.Title,
								"year":            yr,
								"runtime_minutes": details.RuntimeMinutes,
								"confirmed":       false,
								"match_reason":    "first_result",
							})
					}
				}
			}
		}
	}

	if selection, ok, err := s.latestManualSelection(ctx, job.ID); err != nil {
		return err
	} else if ok {
		mediaType, season, episodeStart = selection.MediaType, selection.Season, selection.EpisodeStart
		episodeNumbers = nil
		autoTVMatch = false
		job.Title, job.Year = selection.Title, selection.Year
		run.mediaType, run.season, run.episodeStart = mediaType, season, episodeStart
		run.episodeNumbers = nil
		mediaTitle = selection.Title
		if selection.Year > 0 {
			mediaTitle = fmt.Sprintf("%s (%d)", selection.Title, selection.Year)
		}
	}

	// Step 3: Classify titles.
	detCfg := s.cfg.Detection
	if tmdbConfirmedMovie && mediaType != "tv" && preliminaryDisc.Pattern != ripper.DiscPatternTV {
		detCfg.TVThreshold = 99
	}
	result := ripper.ClassifyTitles(scanned.Titles, detCfg)

	// Create a DB job record now that we have a disc label.
	if s.store != nil {
		pattern := strings.ToLower(result.Pattern.String())
		mainIndex := -1
		mainIndices := make([]int, 0, len(result.MainTitles))
		if len(result.MainTitles) > 0 {
			mainIndex = result.MainTitles[0].Index
		}
		for _, title := range result.MainTitles {
			mainIndices = append(mainIndices, title.Index)
		}
		extraIndices := make([]int, 0, len(result.ExtraTitles))
		for _, title := range result.ExtraTitles {
			extraIndices = append(extraIndices, title.Index)
		}
		_ = s.store.AddEvent(ctx, job.ID, "scan",
			fmt.Sprintf("found %d titles, pattern: %s", len(scanned.Titles), pattern),
			map[string]any{
				"titles":        scanned.Titles,
				"pattern":       pattern,
				"main_index":    mainIndex,
				"main_indices":  mainIndices,
				"extra_indices": extraIndices,
			})
		_ = s.store.UpdateStatusPattern(ctx, job.ID, "scanning", pattern)
	}

	if result.Pattern == ripper.DiscPatternTV && (!autoTVMatch || season == 0 || len(episodeNumbers) != len(result.MainTitles)) {
		identificationMessage := "TV disc detected. Search for the show and select its season."
		switch {
		case autoTVMatch && season > 0:
			identificationMessage = fmt.Sprintf("TV disc detected. Matched %s; episode order is unresolved, correct or confirm the episode numbers.", mediaTitle)
		case autoTVMatch:
			identificationMessage = fmt.Sprintf("TV disc detected. Matched %s; season and episode numbers are unresolved.", mediaTitle)
		case tvSuggestion != "":
			identificationMessage = fmt.Sprintf("TV disc detected. Possible show suggestion: %s; not selected automatically. Search to confirm or correct it.", tvSuggestion)
		}
		s.emit(ProgressEvent{
			Device:  device,
			Stage:   "identifying",
			Title:   mediaTitle,
			Percent: 0,
			Message: identificationMessage,
		})
		s.notifyTVSelection(job.ID, scanned.DiscName, device, mediaTitle)
		if s.store != nil {
			_ = s.store.AddEvent(ctx, job.ID, "identify", "TV metadata unresolved; continuing without guessing season or episode order", map[string]any{"action": "tv_manual_search_available"})
			_ = s.store.UpdateStatus(ctx, job.ID, "identifying")
		}
	}

	mainTitleIndices := make([]int, 0, len(result.MainTitles))
	for _, title := range result.MainTitles {
		mainTitleIndices = append(mainTitleIndices, title.Index)
	}
	slog.Info("rip phase completed", "phase", "identifying", "device", device,
		"disc", scanned.DiscName, "pattern", result.Pattern.String(),
		"title_count", len(scanned.Titles), "classified_main_title_indices", mainTitleIndices,
		"extra_title_count", len(result.ExtraTitles), "junk_title_count", len(result.JunkTitles),
		"missing_metadata", result.MissingMetadata, "multi_angle", result.MultiAngle)

	// Adopt the identified title unless the user already corrected it by hand
	// while identification was running.
	s.setAutoTitle(device, mediaTitle)
	mediaTitle = s.currentTitle(device, mediaTitle)
	run.title = mediaTitle

	// With a TMDB key configured, an unconfirmed match means the disc will be
	// delivered under its raw label unless someone corrects it.
	if len(result.MainTitles) > 0 && s.tmdbConfigured() && !tmdbConfirmedMovie && !(autoTVMatch && mediaType == "tv") {
		s.notifier.Notify(notify.Message{
			Event:   notify.EventNeedsInput,
			JobID:   job.ID,
			Disc:    scanned.DiscName,
			Device:  device,
			Title:   mediaTitle,
			Summary: "No confident TMDB match. Ripping continues under the disc label; correct the title in the UI.",
		})
	}

	// Step 4: Determine which titles to rip.
	// In daemon mode, we always rip MainTitles immediately.
	// For now, skip extras — future enhancement will integrate Discord callbacks.
	if len(result.MainTitles) == 0 {
		if !run.restarted && mediaType == "" {
			s.emit(ProgressEvent{
				Device:  device,
				Stage:   "identifying",
				Title:   s.currentTitle(device, mediaTitle),
				Percent: 0,
				Message: "No main title detected. Waiting for manual media search...",
			})
			if s.store != nil {
				_ = s.store.AddEvent(ctx, job.ID, "identify", "no clear main title, waiting for manual media search", map[string]any{"action": "await_manual_search"})
				_ = s.store.UpdateStatus(ctx, job.ID, "identifying")
			}
			s.notifier.Notify(notify.Message{
				Event:   notify.EventNeedsInput,
				JobID:   job.ID,
				Disc:    scanned.DiscName,
				Device:  device,
				Title:   mediaTitle,
				Summary: fmt.Sprintf("No main title detected (%s pattern, %d titles). Waiting for a manual movie or TV search in the UI.", strings.ToLower(result.Pattern.String()), len(scanned.Titles)),
			})

			selection, waitErr := s.awaitManualSelection(ctx, job.ID, s.cfg.Notification.ResponseTimeoutMin)
			if waitErr != nil {
				if errors.Is(waitErr, context.Canceled) {
					return waitErr
				}
				s.emit(ProgressEvent{
					Device:  device,
					Stage:   "error",
					Title:   s.currentTitle(device, mediaTitle),
					Percent: 0,
					Message: fmt.Sprintf("No main titles found and no manual selection received: %v", waitErr),
				})
				if s.store != nil {
					_ = s.store.AddEvent(ctx, job.ID, "error", "manual media search timed out after no main title", map[string]any{"error": waitErr.Error()})
					_ = s.store.UpdateStatus(ctx, job.ID, "error")
				}
				return fmt.Errorf("no main titles found on disc %q", device)
			}

			mediaType, season, episodeStart = selection.MediaType, selection.Season, selection.EpisodeStart
			episodeNumbers = nil
			autoTVMatch = false
			job.Title, job.Year = selection.Title, selection.Year
			run.mediaType, run.season, run.episodeStart = mediaType, season, episodeStart
			run.episodeNumbers = nil
			if selection.Year > 0 {
				mediaTitle = fmt.Sprintf("%s (%d)", selection.Title, selection.Year)
			} else {
				mediaTitle = selection.Title
			}
			s.beginRipTitle(device, mediaTitle)
			run.title = mediaTitle
		}

		fallbackMain, ok := pickByRuntime(result.AllTitles, s.ripRuntime(device))
		if !ok {
			fallbackMain, ok = pickLongest(result.AllTitles)
		}
		if !ok {
			s.emit(ProgressEvent{
				Device:  device,
				Stage:   "error",
				Title:   mediaTitle,
				Percent: 0,
				Message: "Manual title selected but no candidate titles available to rip",
			})
			if s.store != nil {
				_ = s.store.AddEvent(ctx, job.ID, "error", "manual title selected but no candidate titles available", nil)
				_ = s.store.UpdateStatus(ctx, job.ID, "error")
			}
			return fmt.Errorf("no candidate titles available to rip on disc %q", device)
		}

		result.MainTitles = []disc.MKVTitle{fallbackMain}
		s.emit(ProgressEvent{
			Device:  device,
			Stage:   "identifying",
			Title:   mediaTitle,
			Percent: 0,
			Message: fmt.Sprintf("Manual title selected. Continuing with title #%d", fallbackMain.Index),
		})
		if s.store != nil {
			_ = s.store.AddEvent(ctx, job.ID, "identify", fmt.Sprintf("manual title selected; using title index %d", fallbackMain.Index), map[string]any{"selected_index": fallbackMain.Index, "selected_duration": fallbackMain.Duration.String()})
		}
	}

	// Choose among alternate cuts of the feature. The reference runtime is the
	// hand-picked movie's, else a confirmed TMDB match's, else the title the
	// classifier already chose. Titles within tolerance are the same cut, and
	// the one with the richest audio wins. Only applies before ripping starts;
	// an edit during a rip is handled by RestartIfNeeded.
	if canReselect(result) {
		slog.Info("rip phase started", "phase", "scoring", "device", device,
			"disc", scanned.DiscName, "candidate_title_count", len(result.AllTitles))
		var ref time.Duration
		switch rt := s.ripRuntime(device); {
		case rt > 0:
			ref = time.Duration(rt) * time.Minute
		case tmdbConfirmedMovie && runtimeMin > 0:
			ref = time.Duration(runtimeMin) * time.Minute
		case len(result.MainTitles) == 1:
			ref = result.MainTitles[0].Duration
		}
		if t, ok := pickBestNear(result.AllTitles, ref); ok && (len(result.MainTitles) != 1 || result.MainTitles[0].Index != t.Index) {
			prev := "none"
			if len(result.MainTitles) > 0 {
				prev = strconv.Itoa(result.MainTitles[0].Index)
			}
			result.MainTitles = []disc.MKVTitle{t}
			if s.store != nil {
				_ = s.store.AddEvent(ctx, job.ID, "identify",
					fmt.Sprintf("selected title %d (%d min, %s) as the best cut near %d min", t.Index, int(t.Duration.Minutes()), ripper.ScoreTitle(t).Label(), int(ref.Minutes())),
					map[string]any{"selected_index": t.Index, "previous_index": prev, "reference_minutes": int(ref.Minutes()), "score": ripper.ScoreTitle(t).Total})
			}
		}
		if len(result.MainTitles) == 1 {
			score := ripper.ScoreTitle(result.MainTitles[0])
			slog.Info("rip phase completed", "phase", "scoring", "device", device,
				"disc", scanned.DiscName, "selected_title_index", result.MainTitles[0].Index,
				"score", score.Total, "score_label", score.Label(), "reference_runtime", ref.String())
		} else {
			slog.Info("rip phase completed", "phase", "scoring", "device", device,
				"disc", scanned.DiscName, "selected_title_count", len(result.MainTitles),
				"reference_runtime", ref.String())
		}
	} else {
		slog.Info("rip phase skipped", "phase", "scoring", "device", device,
			"disc", scanned.DiscName, "pattern", result.Pattern.String(),
			"reason", "title scoring is only used to reselect a single movie cut")
	}

	if canReselect(result) && len(result.MainTitles) == 1 {
		minFeature := time.Duration(s.cfg.Detection.MinFeatureMinutes) * time.Minute
		if alts := findAlternates(result.AllTitles, result.MainTitles[0], minFeature); len(alts) > 0 {
			s.registerAlternates(ctx, run, device, scanned, result.MainTitles[0], alts)
		}
	}

	// Step 5: Rip each main title to staging.
	ctx, cancelRip := context.WithCancelCause(ctx)
	defer cancelRip(nil)
	s.registerRestart(device, cancelRip, result)
	defer s.unregisterRestart(device)

	stagingDir := s.cfg.Output.StagingDir
	if stagingDir == "" {
		stagingDir = "/tmp/simplerip-staging"
	}
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		s.emit(ProgressEvent{
			Device:  device,
			Stage:   "error",
			Title:   "",
			Percent: 0,
			Message: fmt.Sprintf("Failed to create staging dir: %v", err),
		})
		if s.store != nil {
			_ = s.store.AddEvent(ctx, job.ID, "error", err.Error(), nil)
			_ = s.store.UpdateStatus(ctx, job.ID, "error")
		}
		return fmt.Errorf("create staging dir: %w", err)
	}

	jobID := fmt.Sprintf("rip-%d", time.Now().UnixNano())
	ripOutputDir := filepath.Join(stagingDir, jobID)
	if err := os.MkdirAll(ripOutputDir, 0o755); err != nil {
		s.emit(ProgressEvent{
			Device:  device,
			Stage:   "error",
			Title:   "",
			Percent: 0,
			Message: fmt.Sprintf("Failed to create output dir: %v", err),
		})
		if s.store != nil {
			_ = s.store.AddEvent(ctx, job.ID, "error", err.Error(), nil)
			_ = s.store.UpdateStatus(ctx, job.ID, "error")
		}
		return fmt.Errorf("create rip output dir: %w", err)
	}

	var rippedFiles []string
	var rippedTitleIndices []int
	totalTitles := len(result.MainTitles)
	if s.store != nil {
		_ = s.store.UpdateStatus(ctx, job.ID, "ripping")
	}
	etaTr := &etaTracker{}
	isTVBatch := mediaType == "tv" || (mediaType == "" && result.Pattern == ripper.DiscPatternTV)
	titlesToRip := result.MainTitles
	if isTVBatch {
		var ripErr error
		rippedFiles, rippedTitleIndices, ripErr = s.ripTVTitles(
			ctx, device, scanned.DiscName, mediaTitle, job,
			result.MainTitles, ripOutputDir, scanned.Type,
		)
		if errors.Is(ripErr, errRestart) || errors.Is(context.Cause(ctx), errRestart) {
			s.restartCleanup(device, stagingDir, ripOutputDir, job.ID, -1)
			return errRestart
		}
		if ripErr != nil {
			if errors.Is(ripErr, context.Canceled) {
				return ripErr
			}
			slog.Error("rip phase failed", "phase", "ripping", "device", device,
				"disc", scanned.DiscName, "title_count", totalTitles, "error", ripErr)
			diag := diagnose.Rip(ripErr)
			s.emit(ProgressEvent{
				Device:  device,
				Stage:   "error",
				Title:   s.currentTitle(device, mediaTitle),
				Percent: 0,
				Message: diag.Summary,
			})
			if s.store != nil {
				_ = s.store.AddEvent(ctx, job.ID, "error", diag.Summary, diag.Data())
				_ = s.store.UpdateStatus(ctx, job.ID, "error")
			}
			return fmt.Errorf("rip TV titles: %w", ripErr)
		}
		if s.store != nil {
			_ = s.store.UpdateStatus(ctx, job.ID, "ripping")
		}
		titlesToRip = nil
	}
	for idx, title := range titlesToRip {
		cur := s.currentTitle(device, mediaTitle)
		slog.Info("rip phase started", "phase", "analyzing", "device", device,
			"disc", scanned.DiscName, "title_index", title.Index, "title_number", idx+1,
			"title_count", totalTitles, "duration", title.Duration.String())
		s.emit(ProgressEvent{
			Device:  device,
			Stage:   "analyzing",
			Title:   cur,
			Percent: 0,
			Message: fmt.Sprintf("Analyzing title %d of %d (MakeMKV title index %d)", idx+1, totalTitles, title.Index),
		})

		if s.store != nil {
			audioDesc := fmt.Sprintf("%.1fh, %d audio tracks", title.Duration.Hours(), title.AudioTrackCount)
			_ = s.store.AddEvent(ctx, job.ID, "score",
				fmt.Sprintf("selected title %d: %s", title.Index, audioDesc),
				map[string]any{
					"selected_index": title.Index,
					"duration":       title.Duration.String(),
					"size_gb":        title.SizeGB,
					"audio_tracks":   title.AudioTrackCount,
					"chapters":       title.ChapterCount,
				})
		}

		// Create progress callback that emits to EventBus, normalizing per-title
		// progress (0-100) to overall progress across all titles.
		titleIdx := idx // capture for closure
		lastReportedPct := -10
		saveStarted := false
		progressCb := func(_ int, percent int, phase ripper.RipPhase) {
			// Read the live title each tick so a mid-rip re-identify is reflected.
			cur := s.currentTitle(device, mediaTitle)
			if phase == ripper.PhaseAnalyze {
				s.emit(ProgressEvent{
					Device:  device,
					Stage:   "analyzing",
					Title:   cur,
					Percent: percent,
					Message: fmt.Sprintf("Analyzing title %d of %d (MakeMKV title index %d, %d%%)", titleIdx+1, totalTitles, title.Index, percent),
				})
				return
			}
			if !saveStarted {
				saveStarted = true
				slog.Info("rip phase completed", "phase", "analyzing", "device", device,
					"disc", scanned.DiscName, "title_index", title.Index,
					"title_number", idx+1, "title_count", totalTitles)
				slog.Info("rip phase started", "phase", "ripping", "device", device,
					"disc", scanned.DiscName, "title_index", title.Index,
					"title_number", idx+1, "title_count", totalTitles)
			}
			// Never show 100% until the file is confirmed saved; makemkvcon also
			// reports 100% when it gives up on an unreadable title.
			overall := min((titleIdx*100+percent)/totalTitles, 99)
			remaining := etaTr.Update(time.Now(), overall)
			s.emit(ProgressEvent{
				Device:  device,
				Stage:   "ripping",
				Title:   cur,
				Percent: overall,
				Message: fmt.Sprintf("Ripping %s (%d%%)", cur, overall),
				ETASec:  int(remaining.Seconds()),
			})
			if s.store != nil && overall >= lastReportedPct+10 {
				lastReportedPct = (overall / 10) * 10
				_ = s.store.AddEvent(ctx, job.ID, "rip",
					fmt.Sprintf("progress: %d%%", overall), nil)
			}
		}

		maxRetries := s.cfg.MakeMKV.MaxRipRetries
		if maxRetries < 0 {
			maxRetries = 0
		}
		attempts := maxRetries + 1

		var (
			files []string
			err   error
		)
		for attempt := 1; attempt <= attempts; attempt++ {
			files, err = ripper.RipTitle(ctx, device, title, ripOutputDir, s.ripOptions(s.cfg.MakeMKV.TimeoutMinutes, scanned.Type, progressCb))
			if err == nil {
				slog.Info("rip phase completed", "phase", "ripping", "device", device,
					"disc", scanned.DiscName, "title_index", title.Index,
					"title_number", idx+1, "title_count", totalTitles, "file_count", len(files))
				break
			}

			retryable := !errors.Is(err, context.Canceled)

			if !retryable || attempt == attempts {
				break
			}

			cur := s.currentTitle(device, mediaTitle)
			s.emit(ProgressEvent{
				Device:  device,
				Stage:   "ripping",
				Title:   cur,
				Percent: (idx * 100) / totalTitles,
				Message: fmt.Sprintf("Read issues detected, retrying title %d (%d/%d)", title.Index, attempt+1, attempts),
			})
			if s.store != nil {
				_ = s.store.AddEvent(ctx, job.ID, "rip",
					fmt.Sprintf("retrying title %d: attempt %d/%d", title.Index, attempt+1, attempts),
					map[string]any{"error": err.Error()})
			}
		}
		if err != nil && errors.Is(context.Cause(ctx), errRestart) {
			s.restartCleanup(device, stagingDir, ripOutputDir, job.ID, title.Index)
			return errRestart
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return fmt.Errorf("rip title %d: %w", title.Index, err)
			}
			failedPhase := "analyzing"
			if saveStarted {
				failedPhase = "ripping"
			}
			slog.Error("rip phase failed", "phase", failedPhase, "device", device,
				"disc", scanned.DiscName, "title_index", title.Index,
				"title_number", idx+1, "title_count", totalTitles, "error", err)
			diag := diagnose.Rip(err)
			s.emit(ProgressEvent{
				Device:  device,
				Stage:   "error",
				Title:   s.currentTitle(device, mediaTitle),
				Percent: (idx * 100) / totalTitles,
				Message: diag.Summary,
			})
			if s.store != nil {
				_ = s.store.AddEvent(ctx, job.ID, "error", diag.Summary, diag.Data())
				_ = s.store.UpdateStatus(ctx, job.ID, "error")
			}
			return fmt.Errorf("rip title %d: %w", title.Index, err)
		}

		if s.store != nil {
			for _, f := range files {
				fi, _ := os.Stat(f)
				sizeGB := 0.0
				if fi != nil {
					sizeGB = float64(fi.Size()) / (1024 * 1024 * 1024)
				}
				_ = s.store.AddEvent(ctx, job.ID, "rip",
					fmt.Sprintf("complete: %s (%.1f GB)", filepath.Base(f), sizeGB),
					map[string]any{"file": filepath.Base(f), "size_gb": sizeGB})
			}
			_ = s.store.UpdateStatus(ctx, job.ID, "ripping")
		}

		rippedFiles = append(rippedFiles, files...)
		for range files {
			rippedTitleIndices = append(rippedTitleIndices, title.Index)
		}
	}

	if selection, ok, err := s.latestManualSelection(ctx, job.ID); err != nil {
		return err
	} else if ok {
		mediaType, season, episodeStart = selection.MediaType, selection.Season, selection.EpisodeStart
		episodeNumbers = nil
		autoTVMatch = false
		job.Title, job.Year = selection.Title, selection.Year
		run.mediaType, run.season, run.episodeStart = mediaType, season, episodeStart
		run.episodeNumbers = nil
		mediaTitle = selection.Title
		if selection.Year > 0 {
			mediaTitle = fmt.Sprintf("%s (%d)", selection.Title, selection.Year)
		}
	}

	// An automatic identification whose runtime doesn't fit the ripped title is
	// probably the wrong movie. Hold the file in staging instead of delivering
	// it under that name, and wait for the user to confirm or correct the title.
	// Editing the title (even to the same one) releases the hold.
	tmdbConfigured := s.tmdbConfigured()
	if s.store != nil && needsConfirmation(result.MainTitles, len(rippedFiles), s.titleIsLocked(device), tmdbConfirmedMovie, tmdbConfigured, runtimeMin) {
		held := int(result.MainTitles[0].Duration.Minutes())
		var summary string
		if !tmdbConfirmedMovie {
			if runtimeMin > 0 {
				summary = fmt.Sprintf("No confident TMDB match for %q (ripped %d min, suggested match runs %d min). The file is held in staging and not delivered: edit the title in the UI to confirm or correct.",
					scanned.DiscName, held, runtimeMin)
			} else {
				summary = fmt.Sprintf("No confident TMDB match found for disc %q (%d min). The file is held in staging and not delivered: edit the title in the UI to set the title.",
					scanned.DiscName, held)
			}
		} else {
			summary = fmt.Sprintf("Ripped title is %d min but %q runs %d min, so the match is probably wrong. The file is held in staging and not delivered: edit the title in the UI (pick the right movie, or re-select the same one to confirm).",
				held, s.currentTitle(device, mediaTitle), runtimeMin)
		}
		s.emit(ProgressEvent{Device: device, Stage: "identifying", Title: s.currentTitle(device, mediaTitle), Percent: 95, Message: "Waiting for title confirmation: " + summary})
		_ = s.store.AddEvent(ctx, job.ID, "identify", "held before delivery: "+summary, map[string]any{"held": true, "ripped_minutes": held, "runtime_minutes": runtimeMin, "confirmed": tmdbConfirmedMovie})
		_ = s.store.UpdateStatus(ctx, job.ID, "identifying")
		s.notifier.Notify(notify.Message{
			Event: notify.EventDurationMismatch, JobID: job.ID, Disc: scanned.DiscName, Device: device,
			Title: s.currentTitle(device, mediaTitle), Summary: summary,
		})
		selection, waitErr := s.awaitManualSelection(ctx, job.ID, s.cfg.Notification.ResponseTimeoutMin)
		if errors.Is(context.Cause(ctx), errRestart) {
			s.restartCleanup(device, stagingDir, ripOutputDir, job.ID, result.MainTitles[0].Index)
			return errRestart
		}
		if waitErr != nil {
			if errors.Is(waitErr, context.Canceled) {
				return waitErr
			}
			msg := "No title confirmation received; the ripped file was left in staging: " + ripOutputDir
			s.emit(ProgressEvent{Device: device, Stage: "error", Title: s.currentTitle(device, mediaTitle), Message: msg})
			_ = s.store.AddEvent(ctx, job.ID, "error", msg, map[string]any{"error": waitErr.Error()})
			_ = s.store.UpdateStatus(ctx, job.ID, "error")
			return fmt.Errorf("title not confirmed for disc %q: %w", device, waitErr)
		}
		mediaType, season, episodeStart = selection.MediaType, selection.Season, selection.EpisodeStart
		episodeNumbers = nil
		autoTVMatch = false
		job.Title, job.Year = selection.Title, selection.Year
		run.mediaType, run.season, run.episodeStart = mediaType, season, episodeStart
		run.episodeNumbers = nil
		mediaTitle = selection.Title
		if selection.Year > 0 {
			mediaTitle = fmt.Sprintf("%s (%d)", selection.Title, selection.Year)
		}
		runtimeMin = s.ripRuntime(device)
		run.confirmed = true
	}

	if !s.claimDelivery(device) {
		s.restartCleanup(device, stagingDir, ripOutputDir, job.ID, -1)
		return errRestart
	}

	if len(rippedFiles) > 1 {
		s.notifier.Notify(notify.Message{
			Event:   notify.EventMultiTitle,
			JobID:   job.ID,
			Disc:    scanned.DiscName,
			Device:  device,
			Title:   mediaTitle,
			Summary: fmt.Sprintf("%d titles ripped (%s pattern).", len(rippedFiles), strings.ToLower(result.Pattern.String())),
		})
	}

	if len(rippedFiles) == 0 {
		s.emit(ProgressEvent{
			Device:  device,
			Stage:   "error",
			Title:   "",
			Percent: 0,
			Message: diagnose.NoFiles().Summary,
		})
		if s.store != nil {
			diag := diagnose.NoFiles()
			_ = s.store.AddEvent(ctx, job.ID, "error", diag.Summary, diag.Data())
			_ = s.store.UpdateStatus(ctx, job.ID, "error")
		}
		return fmt.Errorf("no files ripped from disc %q", device)
	}

	// Step 6: Deliver to NAS if configured.
	destDir := s.cfg.Output.NASPath
	delivered := false
	if destDir != "" {
		// Resolve the final title now (a mid-rip re-identify has already landed)
		// and use it for both the folder/file name and progress.
		deliverTitle := s.freezeTitle(device, mediaTitle)
		run.title = deliverTitle
		s.setAlternatesTitle(job.ID, deliverTitle)
		slog.Info("rip phase started", "phase", "delivering", "device", device,
			"disc", scanned.DiscName, "title", deliverTitle, "file_count", len(rippedFiles),
			"destination", destDir)
		if s.store != nil {
			_ = s.store.UpdateStatus(ctx, job.ID, "delivering")
		}
		s.emit(ProgressEvent{
			Device:  device,
			Stage:   "delivering",
			Title:   deliverTitle,
			Percent: 90,
			Message: fmt.Sprintf("Delivering %s to NAS", deliverTitle),
		})

		// Name the file(s) after the title (Jellyfin: "Title (Year)/Title (Year).mkv")
		// instead of makemkvcon's "title_t00.mkv".
		isTV := mediaType == "tv" || (mediaType == "" && result.Pattern == ripper.DiscPatternTV)
		deliverSubdir := deliverTitle
		if isTV {
			mapped := autoTVMatch && season > 0 && len(rippedFiles) == len(result.MainTitles) &&
				len(rippedTitleIndices) == len(rippedFiles) &&
				len(episodeNumbers) == len(result.MainTitles)
			if mapped {
				titleIndices := append([]int(nil), rippedTitleIndices...)
				episodes := make([]int, len(result.MainTitles))
				for i, titleIndex := range titleIndices {
					episodes[i] = episodeNumbers[titleIndex]
					if episodes[i] < 1 {
						mapped = false
					}
				}
				if mapped {
					if err := output.ValidateTVEpisodeNumbers(destDir, deliverTitle, season, episodes); err != nil {
						slog.Warn("inferred TV episode names conflict; delivering without episode numbering",
							"show", deliverTitle, "season", season, "error", err)
						mapped = false
					} else {
						renamed, rerr := output.RenameForDeliveryEpisodeNumbers(rippedFiles, titleIndices, episodes)
						if rerr != nil {
							return fmt.Errorf("name inferred TV episodes: %w", rerr)
						}
						rippedFiles = renamed
						deliverSubdir = output.TVSeasonDirectory(deliverTitle, season)
					}
				}
			}
			if mapped {
				// The episode number for each title came from explicit title
				// metadata or a unique runtime match, never MakeMKV index order.
			} else if autoTVMatch {
				label := metadata.QueryFromDirName(scanned.DiscName)
				if label == "" {
					label = "unidentified-disc"
				}
				if mediaTitle != scanned.DiscName {
					if season > 0 {
						deliverSubdir = filepath.Join(output.TVSeasonDirectory(deliverTitle, season), "unidentified - "+label)
					} else {
						deliverSubdir = filepath.Join(deliverTitle, "unidentified - "+label)
					}
				} else {
					deliverSubdir = label
				}
			} else if season > 0 {
				start, err := output.TVEpisodeStart(destDir, deliverTitle, season, episodeStart, len(rippedFiles))
				if err != nil {
					return fmt.Errorf("choose TV episode numbers: %w", err)
				}
				renamed, rerr := output.RenameForDeliveryEpisodes(rippedFiles, start)
				if rerr != nil {
					return fmt.Errorf("name TV episodes: %w", rerr)
				}
				rippedFiles = renamed
				deliverSubdir = output.TVSeasonDirectory(deliverTitle, season)
			} else {
				deliverSubdir = metadata.QueryFromDirName(scanned.DiscName)
				if deliverSubdir == "" {
					deliverSubdir = "unidentified-disc"
				}
			}
		} else if renamed, rerr := output.RenameForDeliveryPattern(rippedFiles, deliverTitle, false); rerr != nil {
			slog.Warn("could not rename ripped files; delivering with original names", "error", rerr)
		} else {
			rippedFiles = renamed
		}

		// Deliver files to NAS using proper folder name from metadata.
		deliverResult, err := output.Deliver(
			ctx,
			rippedFiles,
			ripOutputDir,
			destDir,
			deliverSubdir,
			deliverTitle,
			scanned.DiscName,
		)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return fmt.Errorf("deliver to NAS: %w", err)
			}
			slog.Error("rip phase failed", "phase", "delivering", "device", device,
				"disc", scanned.DiscName, "title", deliverTitle, "destination", destDir, "error", err)
			s.emit(ProgressEvent{
				Device:  device,
				Stage:   "error",
				Title:   deliverTitle,
				Percent: 90,
				Message: diagnose.Delivery(err).Summary,
			})
			if s.store != nil {
				diag := diagnose.Delivery(err)
				_ = s.store.AddEvent(ctx, job.ID, "error", diag.Summary, diag.Data())
				_ = s.store.UpdateStatus(ctx, job.ID, "error")
			}
			return fmt.Errorf("deliver to NAS: %w", err)
		}
		// Update rippedFiles to point to destination paths for notification.
		rippedFiles = deliverResult.Files
		delivered = true
		slog.Info("rip phase completed", "phase", "delivering", "device", device,
			"disc", scanned.DiscName, "title", deliverTitle, "file_count", len(deliverResult.Files),
			"destination", deliverResult.DestDir)

		if s.store != nil {
			var deliveredGB float64
			for _, f := range deliverResult.Files {
				if fi, statErr := os.Stat(f); statErr == nil {
					deliveredGB += float64(fi.Size()) / (1024 * 1024 * 1024)
				}
			}
			_ = s.store.AddEvent(ctx, job.ID, "deliver",
				fmt.Sprintf("rsync complete: %.1f GB verified on NAS", deliveredGB),
				map[string]any{"nas_path": deliverResult.DestDir, "size_gb": deliveredGB})
			_ = s.store.UpdateStatus(ctx, job.ID, "done")
		}

		// Clean up staging directory after successful delivery.
		// Deliver() has already verified all files exist at destination with matching sizes,
		// so it's safe to delete the staging copies. Use cleaned paths and a strict prefix
		// check (rather than a substring match) to guard against path traversal accidents.
		cleanedRipDir := filepath.Clean(ripOutputDir)
		cleanedStaging := filepath.Clean(stagingDir)
		ripPrefix := cleanedStaging + string(filepath.Separator)
		if cleanedRipDir != "" &&
			cleanedRipDir != cleanedStaging &&
			strings.HasPrefix(cleanedRipDir, ripPrefix) &&
			strings.HasPrefix(filepath.Base(cleanedRipDir), "rip-") {
			if err := os.RemoveAll(cleanedRipDir); err != nil {
				// Log warning but don't fail — delivery succeeded.
				slog.Warn("failed to clean up staging dir", "path", cleanedRipDir, "error", err)
			}
		} else {
			slog.Warn("skipping cleanup due to invalid path", "path", cleanedRipDir)
		}
	} else {
		slog.Info("rip phase skipped", "phase", "delivering", "device", device,
			"disc", scanned.DiscName, "reason", "no destination configured")
	}

	// Step 7: Send completion notification.
	// Build minimal metadata for Discord.
	var media []notify.MKVMeta
	for _, file := range rippedFiles {
		info, err := inspect.Probe(ctx, file)
		if err != nil {
			// Log warning but continue — notification is best-effort.
			continue
		}
		// Convert AudioTrack slice to string slice.
		var audioTracks []string
		for _, track := range info.Audio {
			audioTracks = append(audioTracks, track.String())
		}
		if len(result.MainTitles) == 1 && !s.userChoseTitle(device, run) && durationMismatch(info.Duration, runtimeMin) {
			s.notifier.Notify(notify.Message{
				Event:  notify.EventDurationMismatch,
				JobID:  job.ID,
				Disc:   scanned.DiscName,
				Device: device,
				Title:  run.title,
				Summary: fmt.Sprintf("Ripped file is %d min but TMDB/OMDb runtime is %d min. This may be a different cut or the wrong movie.",
					int(info.Duration.Minutes()), runtimeMin),
			})
		}
		media = append(media, notify.MKVMeta{
			File:        filepath.Base(file),
			VideoCodec:  info.VideoCodec,
			Resolution:  info.Resolution,
			AudioTracks: audioTracks,
			SizeBytes:   info.SizeBytes,
		})
	}

	payload := notify.RipCompletePayload(
		jobID,
		run.title,
		scanned.DiscName,
		destDir,
		rippedFiles,
		media,
	)

	details := make([]string, 0, len(media))
	for _, m := range media {
		line := fmt.Sprintf("%s (%.1f GB", m.File, float64(m.SizeBytes)/(1<<30))
		if m.Resolution != "" {
			line += ", " + m.Resolution
		}
		details = append(details, line+")")
	}
	summary := fmt.Sprintf("Delivered and verified: %d file(s) in %s.", len(rippedFiles), filepath.Base(filepath.Dir(rippedFiles[0])))
	if !delivered {
		summary = fmt.Sprintf("Ripped to staging: %d file(s) in %s.", len(rippedFiles), filepath.Base(filepath.Dir(rippedFiles[0])))
	}
	s.notifier.Notify(notify.Message{
		Event:   notify.EventComplete,
		JobID:   job.ID,
		Disc:    scanned.DiscName,
		Device:  device,
		Title:   run.title,
		Summary: summary,
		Details: details,
	})

	if err := s.notify.Send(ctx, payload); err != nil {
		// Notification failure is not fatal — the rip succeeded.
		s.emit(ProgressEvent{
			Device:  device,
			Stage:   "done",
			Title:   run.title,
			Percent: 100,
			Message: run.title + " completed (notification failed)",
		})
		if s.store != nil {
			_ = s.store.UpdateJob(ctx, job.ID, run.title, job.Year, "done", job.Pattern)
		}
		return nil
	}

	s.emit(ProgressEvent{
		Device:  device,
		Stage:   "done",
		Title:   run.title,
		Percent: 100,
		Message: run.title + " completed successfully",
	})
	if s.store != nil {
		_ = s.store.UpdateJob(ctx, job.ID, run.title, job.Year, "done", job.Pattern)
	}

	return nil
}

// CleanDir runs the post-rip deduplication and TMDB enrichment workflow.
// It wraps output.Clean logic but remains agnostic to CLI concerns.
//
// Parameters:
//   - dir: absolute path to the directory containing MKV files
//
// Returns an error if the directory cannot be cleaned.
// For interactive prompts (TMDB selection, confirmation), the caller must
// handle user input — this method is designed for programmatic use.
func (s *RipService) CleanDir(dir string) error {
	ctx := context.Background()

	// Step 1: Flatten extras/ subdirectories.
	_, err := output.FlattenSubdirs(dir, false)
	if err != nil {
		return fmt.Errorf("flatten subdirs: %w", err)
	}

	// Step 2: Analyze for duplicates.
	analyses, err := output.AnalyzeDir(ctx, dir)
	if err != nil {
		return fmt.Errorf("analyze dir: %w", err)
	}

	// Step 3: Execute deduplication if needed.
	if len(analyses) > 0 {
		_, err := output.ExecuteDedupe(dir, analyses)
		if err != nil {
			return fmt.Errorf("dedupe: %w", err)
		}
	}

	// Step 4: TMDB enrichment and renaming.
	// Future enhancement: add TMDB search, OMDb cross-reference, and file renaming.
	// Current implementation intentionally stops after deduplication.

	return nil
}

// RenameEntry is an MKV file paired with its probed duration.
type RenameEntry struct {
	Path string
	Dur  time.Duration
}

// RenamePlan describes a single file rename operation.
type RenamePlan struct {
	Src    string // full source path
	Folder string // destination subfolder name, e.g. "Oppenheimer (2023)"
	Base   string // destination base name without extension
}

// PlanRename computes a rename plan for the given keepers. The keeper whose
// duration is closest to the theatrical reference (details.RuntimeMinutes)
// receives the clean folder name. All others receive an "Alternate" label
// (or the edition name if provided), with a duration suffix appended unless
// exactly one alternate exists and a specific edition name is given.
func PlanRename(keepers []RenameEntry, details *metadata.MovieDetails, edition string) []RenamePlan {
	if details == nil {
		return []RenamePlan{}
	}

	ref := time.Duration(details.RuntimeMinutes) * time.Minute
	closestIdx, closestDiff := 0, time.Duration(1<<62)
	for i, k := range keepers {
		d := k.Dur - ref
		if d < 0 {
			d = -d
		}
		if d < closestDiff {
			closestDiff, closestIdx = d, i
		}
	}

	alternateCount := len(keepers) - 1
	folder := details.FolderName()

	plans := make([]RenamePlan, len(keepers))
	for i, k := range keepers {
		var label string
		if i != closestIdx {
			label = "Alternate"
			if edition != "" {
				label = edition
			}
			if alternateCount > 1 || edition == "" {
				label = fmt.Sprintf("%s (%dmin)", label, int(k.Dur.Minutes()))
			}
		}
		base := folder
		if label != "" {
			base += " - " + label
		}
		plans[i] = RenamePlan{
			Src:    k.Path,
			Folder: folder,
			Base:   base,
		}
	}
	return plans
}

// ExecuteRename creates destination directories and moves each source file to
// baseDir/plan.Folder/plan.Base+".mkv". It returns the destination paths of
// all successfully moved files. If a destination already exists or a move
// fails, execution stops immediately and the error is returned.
func ExecuteRename(plans []RenamePlan, baseDir string) ([]string, error) {
	var renamed []string
	for _, p := range plans {
		destDir := filepath.Join(baseDir, p.Folder)
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return renamed, err
		}
		dst := filepath.Join(destDir, p.Base+".mkv")
		if _, err := os.Stat(dst); err == nil {
			return renamed, fmt.Errorf("destination already exists: %s", dst)
		} else if !os.IsNotExist(err) {
			return renamed, fmt.Errorf("stat destination %s: %w", dst, err)
		}
		if err := MoveFile(p.Src, dst); err != nil {
			return renamed, err
		}
		renamed = append(renamed, dst)
	}
	return renamed, nil
}

// EnrichMovie fetches full TMDB detail + OMDb metadata for the chosen movie
// result. If an OMDb API key is configured, runtimes are cross-referenced and
// reconciled. Returns an error if no TMDB credential is configured.
func (s *RipService) EnrichMovie(ctx context.Context, chosen metadata.MovieResult) (*metadata.MovieDetails, error) {
	if !s.tmdbConfigured() {
		return nil, fmt.Errorf("metadata.tmdb_api_key or metadata.tmdb_access_token not configured")
	}

	tmdbClient := s.tmdbClient()

	var omdbClient *metadata.OMDbClient
	if s.cfg.Metadata.OMDbApiKey != "" {
		omdbClient = newOMDbClient(s.cfg.Metadata.OMDbApiKey)
	}

	return metadata.Enrich(ctx, tmdbClient, omdbClient, chosen)
}

// SearchMovie searches TMDB for movies matching query. When the initial query
// returns no results, it retries with progressively shorter queries by dropping
// the last word until results are found or all words are exhausted.
// Returns an error if no TMDB credential is configured or no results are found.
func (s *RipService) SearchMovie(ctx context.Context, query string) ([]metadata.MovieResult, error) {
	if !s.tmdbConfigured() {
		return nil, fmt.Errorf("metadata.tmdb_api_key or metadata.tmdb_access_token not configured")
	}

	client := s.tmdbClient()

	words := strings.Fields(query)
	for len(words) > 0 {
		q := strings.Join(words, " ")
		results, err := client.SearchMovie(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("tmdb search %q: %w", q, err)
		}
		if len(results) > 0 {
			return results, nil
		}
		words = words[:len(words)-1]
	}

	return nil, fmt.Errorf("no TMDB results for %q", query)
}

// SearchMedia searches for movies and TV shows, retrying progressively shorter
// queries when TMDB returns no supported media results.
func (s *RipService) SearchMedia(ctx context.Context, query string) ([]metadata.MediaSearchResult, error) {
	if !s.tmdbConfigured() {
		return nil, fmt.Errorf("metadata.tmdb_api_key or metadata.tmdb_access_token not configured")
	}

	client := s.tmdbClient()
	words := strings.Fields(query)
	for len(words) > 0 {
		q := strings.Join(words, " ")
		results, err := client.SearchMulti(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("tmdb multi search %q: %w", q, err)
		}
		if len(results) > 0 {
			return results, nil
		}
		words = words[:len(words)-1]
	}
	return nil, fmt.Errorf("no TMDB results for %q", query)
}

// MoveFile moves src to dst, falling back to a copy+delete when src and dst
// are on different devices (EXDEV). This is necessary when staging and NAS
// are separate mounts.
func MoveFile(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("destination already exists: %s", dst)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat destination %s: %w", dst, err)
	}

	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}

	var linkErr *os.LinkError
	if errors.As(err, &linkErr) && errors.Is(linkErr.Err, syscall.EXDEV) {
		return copyAndRemove(src, dst)
	}

	return err
}

func copyAndRemove(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	return os.Remove(src)
}

// QueryFromMKVPath derives a TMDB search query from a MKV file path.
// makemkvcon names output files like "Revenge of the Sith_t00.mkv"; this
// strips the _tNN suffix and extension before passing through metadata.QueryFromDirName.
func QueryFromMKVPath(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), ".mkv")
	if i := strings.LastIndex(name, "_t"); i != -1 && isAllDigits(name[i+2:]) {
		name = name[:i]
	}
	return metadata.QueryFromDirName(name)
}

func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

// cacheMBForDisc returns the read-cache size in MB to pass to makemkvcon for a
// full rip. If the operator set an explicit value via config (> 0), that always
// wins. Otherwise a sensible per-media-type default is chosen:
//
//	DVD     → 512 MB  (standard-definition sequential reads)
//	Blu-ray → 1024 MB (dual-layer high-bitrate, ~40 Mbps peak)
//	Unknown → 512 MB  (safe floor)
//
// 4K UHD discs are classified as Blu-ray by makemkvcon, so they receive the
// 1024 MB budget which comfortably covers their ~128 Mbps peak bitrate.
// ripOptions builds makemkvcon rip settings from config for one run.
func (s *RipService) ripOptions(timeoutMinutes int, discType disc.DiscType, progress ripper.ProgressCallback) ripper.RipOptions {
	return ripper.RipOptions{
		Key:               s.cfg.MakeMKV.Key,
		TimeoutMinutes:    timeoutMinutes,
		CacheMB:           cacheMBForDisc(s.cfg.MakeMKV.CacheMB, discType),
		ReadErrorLimit:    s.cfg.MakeMKV.ReadErrorLimit,
		NoProgressMinutes: s.cfg.MakeMKV.NoProgressMin,
		Progress:          progress,
	}
}

func cacheMBForDisc(cfgCacheMB int, discType disc.DiscType) int {
	if cfgCacheMB > 0 {
		return cfgCacheMB
	}
	switch discType {
	case disc.DiscTypeBluRay:
		return 1024
	case disc.DiscTypeDVD:
		return 512
	default:
		return 512
	}
}

// ScanInfoFromReader scans a disc from a captured makemkvcon output fixture.
// It wraps ripper.ScanInfoFromReader and is useful for tests and for --fixture CLI mode.
func (s *RipService) ScanInfoFromReader(r io.Reader, deviceLabel string) (*ripper.ClassificationResult, error) {
	scanned, err := ripper.ScanInfoFromReader(r, deviceLabel)
	if err != nil {
		return nil, fmt.Errorf("scan fixture: %w", err)
	}

	result := ripper.ClassifyTitles(scanned.Titles, s.cfg.Detection)
	return &result, nil
}

// ripRun carries identifying details of an in-flight rip out of ripDisc so a
// failure can be reported with the disc, job and title it concerned.
type ripRun struct {
	jobID, disc, title string
	mediaType          string
	season             int
	episodeStart       int
	episodeNumbers     map[int]int

	// Kept across restarts so a restart reuses the scan and the job.
	scanned   *disc.ClassifiedDisc
	job       store.Job
	restarted bool
	confirmed bool // the user answered a held-for-confirmation prompt
}

// errRestart is returned by ripDisc when a title edit changed which title
// should be ripped; RipDisc then runs the pipeline again.
var errRestart = errors.New("restart rip with corrected title")

// restartState tracks what the in-flight rip on a device selected, so an edit
// can tell whether the new movie needs a different title.
type restartState struct {
	cancel     context.CancelCauseFunc
	all        []disc.MKVTitle
	selected   []disc.MKVTitle
	reselect   bool
	restarting bool
}

func (s *RipService) registerRestart(device string, cancel context.CancelCauseFunc, result ripper.ClassificationResult) {
	s.ripMu.Lock()
	defer s.ripMu.Unlock()
	s.restarts[device] = &restartState{
		cancel:   cancel,
		all:      result.AllTitles,
		selected: result.MainTitles,
		reselect: canReselect(result),
	}
}

func (s *RipService) unregisterRestart(device string) {
	s.ripMu.Lock()
	defer s.ripMu.Unlock()
	delete(s.restarts, device)
}

// RestartIfNeeded stops the in-flight rip on device and makes the pipeline
// start over when the pinned runtime points at a different title than the one
// being ripped. It returns false when the current selection is still right or
// the rip can no longer be restarted (not ripping yet, or delivering).
//
// A TV batch is one MakeMKV process, so a restart cancels the whole batch.
// That only happens when the selected titles would change (a movie edit whose
// runtime picks a single title); TV edits clear the runtime and only rename.
func (s *RipService) RestartIfNeeded(device string) bool {
	s.ripMu.Lock()
	defer s.ripMu.Unlock()
	st, ok := s.restarts[device]
	if !ok || st.restarting || !st.reselect || s.titleFrozen[device] || s.pinnedRuntime[device] <= 0 {
		return false
	}
	t, found := pickByRuntime(st.all, s.pinnedRuntime[device])
	if !found {
		return false
	}
	if len(st.selected) == 1 && st.selected[0].Index == t.Index {
		return false
	}
	st.restarting = true
	st.cancel(errRestart)
	return true
}

// EditMessage explains to the user what an edit did to the in-flight rip on
// device. restarted is the result of RestartIfNeeded.
func (s *RipService) EditMessage(device string, restarted bool) string {
	s.ripMu.Lock()
	defer s.ripMu.Unlock()
	st, ok := s.restarts[device]
	runtime := s.pinnedRuntime[device]
	if restarted && ok {
		t, _ := pickByRuntime(st.all, runtime)
		return fmt.Sprintf("Restarting: ripping title %d (%dm) to match the movie's %dm runtime.",
			t.Index, int(t.Duration.Minutes()), runtime)
	}
	if !ok {
		return "Saved. Track selection will use the corrected movie."
	}
	cur := "none"
	if len(st.selected) > 1 {
		cur = fmt.Sprintf("%d selected titles", len(st.selected))
	} else if len(st.selected) == 1 {
		cur = fmt.Sprintf("title %d (%dm)", st.selected[0].Index, int(st.selected[0].Duration.Minutes()))
	}
	switch {
	case st.restarting:
		return "Saved. A restart is already in progress."
	case s.titleFrozen[device]:
		return "Saved. Delivery has started, so the current file keeps its track."
	case !st.reselect:
		return "Renamed only: this disc type is not re-selected automatically. Keeping " + cur + "."
	case runtime <= 0:
		return "Renamed only: the movie has no known runtime to match tracks against. Keeping " + cur + "."
	}
	if _, found := pickByRuntime(st.all, runtime); !found {
		return fmt.Sprintf("Renamed only: no title is within %d min of the movie's %dm runtime. Keeping %s.",
			int(durationTolerance.Minutes()), runtime, cur)
	}
	return fmt.Sprintf("Renamed only: %s already matches the movie's %dm runtime.", cur, runtime)
}

// claimDelivery marks the rip as past the point of restarting. It returns
// false if a restart was already requested, in which case the caller must
// restart instead of delivering the stale selection.
func (s *RipService) claimDelivery(device string) bool {
	s.ripMu.Lock()
	defer s.ripMu.Unlock()
	if st, ok := s.restarts[device]; ok {
		if st.restarting {
			return false
		}
		delete(s.restarts, device)
	}
	return true
}

// restartCleanup discards the staged files from the superseded selection and
// tells the UI and job history that the rip is starting over.
func (s *RipService) restartCleanup(device, stagingDir, ripDir, jobID string, titleIdx int) {
	cleaned := filepath.Clean(ripDir)
	prefix := filepath.Clean(stagingDir) + string(filepath.Separator)
	if strings.HasPrefix(cleaned, prefix) && strings.HasPrefix(filepath.Base(cleaned), "rip-") {
		if err := os.RemoveAll(cleaned); err != nil {
			slog.Warn("failed to clean staging after restart", "path", cleaned, "error", err)
		}
	}
	title := s.currentTitle(device, "")
	s.emit(ProgressEvent{
		Device:  device,
		Stage:   "identifying",
		Title:   title,
		Percent: 0,
		Message: fmt.Sprintf("Restarting with %s", title),
	})
	if s.store != nil {
		_ = s.store.AddEvent(context.Background(), jobID, "identify",
			fmt.Sprintf("restarting rip with corrected title %q (stopped title %d)", title, titleIdx),
			map[string]any{"restart": true, "stopped_index": titleIdx})
		_ = s.store.UpdateStatus(context.Background(), jobID, "identifying")
	}
}

// durationTolerance matches the theatrical-cut tolerance in metadata.EditionLabel.
const durationTolerance = 3 * time.Minute

// durationMismatch reports whether a ripped file's length differs from the
// expected runtime by more than durationTolerance. Unknown runtime never mismatches.
func durationMismatch(actual time.Duration, runtimeMin int) bool {
	if runtimeMin <= 0 || actual <= 0 {
		return false
	}
	diff := actual - time.Duration(runtimeMin)*time.Minute
	if diff < 0 {
		diff = -diff
	}
	return diff > durationTolerance
}

// newNotifier builds the notification dispatcher from config. With no webhook
// configured it returns a dispatcher that drops everything.
func newNotifier(cfg config.NotificationConfig) *notify.Dispatcher {
	var senders []notify.Sender
	if cfg.DiscordWebhookURL != "" {
		senders = append(senders, notify.NewDiscordSender(cfg.DiscordWebhookURL))
	}
	return notify.NewDispatcher(senders, map[notify.Event]bool{
		notify.EventNeedsInput:       cfg.Events.NeedsInput,
		notify.EventMultiTitle:       cfg.Events.MultiTitle,
		notify.EventComplete:         cfg.Events.Complete,
		notify.EventFailed:           cfg.Events.Failed,
		notify.EventDurationMismatch: cfg.Events.DurationMismatch,
	}, cfg.UIURL)
}

func (s *RipService) notifyTVSelection(jobID, discName, device, title string) {
	s.notifier.Notify(notify.Message{
		Event:   notify.EventNeedsInput,
		JobID:   jobID,
		Disc:    discName,
		Device:  device,
		Title:   title,
		Summary: "TV metadata is unresolved. Open SimpleRip to inspect the evidence, confirm or correct the show, and choose a season if known.",
	})
}

// canReselect reports whether the disc has a single-feature shape where
// swapping the selected title for a runtime match is safe. TV discs and
// multi-angle discs rip several or specific titles, so they are left alone.
func canReselect(r ripper.ClassificationResult) bool {
	if r.MultiAngle || r.MissingMetadata {
		return false
	}
	return r.Pattern == ripper.DiscPatternMovie || r.Pattern == ripper.DiscPatternAmbiguous
}

// pickByRuntime returns the best title near runtimeMin; see pickBestNear.
func pickByRuntime(titles []disc.MKVTitle, runtimeMin int) (disc.MKVTitle, bool) {
	return pickBestNear(titles, time.Duration(runtimeMin)*time.Minute)
}

// pickBestNear returns the best-scoring title within durationTolerance of
// ref. Titles that close are the same cut, so they compete on ripper.ScoreTitle
// (audio codec/channels, English subtitles, resolution, size). Disqualified
// titles lose to any qualified one; remaining ties go to the closest runtime.
func pickBestNear(titles []disc.MKVTitle, ref time.Duration) (disc.MKVTitle, bool) {
	if ref <= 0 {
		return disc.MKVTitle{}, false
	}
	diff := func(t disc.MKVTitle) time.Duration {
		d := t.Duration - ref
		if d < 0 {
			d = -d
		}
		return d
	}
	var best disc.MKVTitle
	var bestScore ripper.TitleScore
	found := false
	for _, t := range titles {
		if diff(t) > durationTolerance {
			continue
		}
		sc := ripper.ScoreTitle(t)
		better := !found
		if found {
			switch {
			case sc.Disqualified != bestScore.Disqualified:
				better = !sc.Disqualified
			case sc.Total != bestScore.Total:
				better = sc.Total > bestScore.Total
			default:
				better = diff(t) < diff(best)
			}
		}
		if better {
			best, bestScore, found = t, sc, true
		}
	}
	return best, found
}

// isConfidentMovieMatch reports whether a TMDB movie match's runtime plausibly
// matches the longest title on the disc. This guards against cryptic disc labels
// matching unrelated films and prevents TV episode discs from being misclassified
// as movies.
func isConfidentMovieMatch(runtimeMin int, longestTitle time.Duration) bool {
	if runtimeMin <= 0 || longestTitle <= 0 {
		return false
	}
	// Feature films with playlist obfuscation or duplicate titles are feature-length.
	// Titles shorter than 60 minutes with duplicate playlists are TV episodes and
	// should not suppress TV detection.
	if longestTitle < 60*time.Minute || runtimeMin < 60 {
		return false
	}
	ref := time.Duration(runtimeMin) * time.Minute
	diff := longestTitle - ref
	if diff < 0 {
		diff = -diff
	}
	ratio := float64(longestTitle) / float64(ref)
	return diff <= 20*time.Minute || (ratio >= 0.70 && ratio <= 1.60)
}
