// Package service implements the core business logic for SimpleRip.
// It orchestrates disc scanning, ripping, cleaning, and notification workflows
// without containing any CLI-specific code.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/8bitreid/simplerip/internal/diagnose"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

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
	Device   string `json:"device,omitempty"`      // e.g. /dev/sr0
	Stage    string `json:"stage"`                 // scanning, analyzing, ripping, delivering, done, idle, error
	Title    string `json:"title"`                 // e.g. "Revenge of the Sith"
	Percent  int    `json:"percent"`               // 0-100
	Message  string `json:"message"`               // human-readable status message
	DiscType string `json:"disc_type,omitempty"`   // bluray, dvd, unknown
	ETASec   int    `json:"eta_seconds,omitempty"` // estimated seconds remaining while ripping; 0 = unknown
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
		year := 0
		switch v := payload["year"].(type) {
		case float64:
			year = int(v)
		case int:
			year = v
		}
		if strings.TrimSpace(title) == "" {
			continue
		}
		return title, year, true
	}
	return "", 0, false
}

func (s *RipService) awaitManualReidentify(ctx context.Context, jobID string, timeoutMin int) (string, int, error) {
	if s.store == nil {
		return "", 0, errors.New("database not configured")
	}

	waitCtx := ctx
	cancel := func() {}
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
			if title, year, ok := findManualCorrection(events, start); ok {
				if strings.TrimSpace(job.Title) != "" {
					title = job.Title
					year = job.Year
				}
				return title, year, nil
			}
		}

		select {
		case <-waitCtx.Done():
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return "", 0, fmt.Errorf("manual title search timed out")
			}
			return "", 0, waitCtx.Err()
		case <-tick.C:
		}
	}
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
	ripMu sync.Mutex
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
// from delivery: the identity was automatic and its runtime doesn't fit.
func needsConfirmation(main []disc.MKVTitle, files int, userLocked bool, runtimeMin int) bool {
	return len(main) == 1 && files == 1 && !userLocked && durationMismatch(main[0].Duration, runtimeMin)
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
// It returns 0 when unknown (no API key, lookup failure).
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
	scanned, err := ripper.ScanInfo(ctx, "makemkvcon", device, s.cfg.MakeMKV.Key)
	if err != nil {
		return nil, fmt.Errorf("scan device %q: %w", device, err)
	}

	// Classify titles according to detection rules.
	result := ripper.ClassifyTitles(scanned.Titles, s.cfg.Detection)
	return &result, nil
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
	// A cancelled context means shutdown or a user abort, not a failure.
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

func (s *RipService) ripDisc(ctx context.Context, device string, run *ripRun) error {
	// job tracks the DB record; zero value is safe when s.store == nil.
	var job store.Job

	// Step 1: Scan disc.
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
		scanned, err = ripper.ScanInfo(ctx, "makemkvcon", device, s.cfg.MakeMKV.Key)
		if err != nil {
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
				if failedJob, createErr := s.store.CreateJob(ctx, device, "", ""); createErr == nil {
					data := scanDiag.Data()
					data["device"] = device
					_ = s.store.AddEvent(ctx, failedJob.ID, "error", friendlyMsg, data)
					_ = s.store.UpdateStatus(ctx, failedJob.ID, "error")
				}
			}
			return fmt.Errorf("scan device %q: %w", device, err)
		}
	}
	run.scanned = scanned

	discTypeStr := scanned.Type.String()

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
	runtimeMin := 0
	if run.restarted {
		// The user chose the movie by hand; don't second-guess it.
		mediaTitle = s.currentTitle(device, mediaTitle)
		tmdbConfirmedMovie = true
		runtimeMin = s.ripRuntime(device)
	} else if s.cfg.Metadata.TMDBApiKey != "" && scanned.DiscName != "" {
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

			tmdbClient := newTMDBClient(s.cfg.Metadata.TMDBApiKey)
			chosen, runtimeWinner, logMsg, err := metadata.BestMatch(ctx, tmdbClient, movies, longestDuration)
			if err == nil {
				if runtimeWinner && logMsg != "" {
					slog.Info("tmdb runtime match selected", "message", logMsg)
				}
				details, err := s.EnrichMovie(ctx, chosen)
				if err == nil {
					mediaTitle = details.FolderName()
					tmdbConfirmedMovie = true
					runtimeMin = details.RuntimeMinutes
					if s.store != nil {
						yr, _ := strconv.Atoi(details.Year)
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
							})
						_ = s.store.UpdateJob(ctx, job.ID, details.Title, yr, "identifying", "")
					}
				}
			} else {
				// Fall back to first result if BestMatch fails.
				details, err := s.EnrichMovie(ctx, movies[0])
				if err == nil {
					mediaTitle = details.FolderName()
					tmdbConfirmedMovie = true
					runtimeMin = details.RuntimeMinutes
					if s.store != nil {
						yr, _ := strconv.Atoi(details.Year)
						_ = s.store.AddEvent(ctx, job.ID, "identify",
							fmt.Sprintf("identified as: %s (%d)", details.Title, yr),
							map[string]any{
								"tmdb_id":         movies[0].ID,
								"title":           details.Title,
								"year":            yr,
								"runtime_minutes": details.RuntimeMinutes,
								"match_reason":    "first_result",
							})
						_ = s.store.UpdateJob(ctx, job.ID, details.Title, yr, "identifying", "")
					}
				}
			}
		}
	}

	// Step 3: Classify titles.
	// If TMDB already confirmed this is a movie, suppress the TV-cluster rule
	// so a disc with 3+ same-duration copies of the same film isn't misclassified.
	detCfg := s.cfg.Detection
	if tmdbConfirmedMovie {
		detCfg.TVThreshold = 99
	}
	result := ripper.ClassifyTitles(scanned.Titles, detCfg)

	// Create a DB job record now that we have a disc label.
	if s.store != nil {
		pattern := strings.ToLower(result.Pattern.String())
		mainIndex := -1
		if len(result.MainTitles) > 0 {
			mainIndex = result.MainTitles[0].Index
		}
		_ = s.store.AddEvent(ctx, job.ID, "scan",
			fmt.Sprintf("found %d titles, pattern: %s", len(scanned.Titles), pattern),
			map[string]any{
				"titles":     scanned.Titles,
				"pattern":    pattern,
				"main_index": mainIndex,
			})
		_ = s.store.UpdateJob(ctx, job.ID, "", 0, "scanning", pattern)
	}

	// Adopt the identified title unless the user already corrected it by hand
	// while identification was running.
	s.setAutoTitle(device, mediaTitle)
	mediaTitle = s.currentTitle(device, mediaTitle)
	run.title = mediaTitle

	// With a TMDB key configured, an unconfirmed match means the disc will be
	// delivered under its raw label unless someone corrects it.
	if len(result.MainTitles) > 0 && s.cfg.Metadata.TMDBApiKey != "" && !tmdbConfirmedMovie {
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
		s.emit(ProgressEvent{
			Device:  device,
			Stage:   "identifying",
			Title:   s.currentTitle(device, mediaTitle),
			Percent: 0,
			Message: "No main title detected. Waiting for manual movie search...",
		})
		if s.store != nil {
			_ = s.store.AddEvent(ctx, job.ID, "identify", "no clear main title, waiting for manual movie search", map[string]any{"action": "await_manual_search"})
			_ = s.store.UpdateStatus(ctx, job.ID, "identifying")
		}
		s.notifier.Notify(notify.Message{
			Event:   notify.EventNeedsInput,
			JobID:   job.ID,
			Disc:    scanned.DiscName,
			Device:  device,
			Title:   mediaTitle,
			Summary: fmt.Sprintf("No main title detected (%s pattern, %d titles). Waiting for a manual movie search in the UI.", strings.ToLower(result.Pattern.String()), len(scanned.Titles)),
		})

		title, year, waitErr := s.awaitManualReidentify(ctx, job.ID, s.cfg.Notification.ResponseTimeoutMin)
		if waitErr != nil {
			s.emit(ProgressEvent{
				Device:  device,
				Stage:   "error",
				Title:   s.currentTitle(device, mediaTitle),
				Percent: 0,
				Message: fmt.Sprintf("No main titles found and no manual selection received: %v", waitErr),
			})
			if s.store != nil {
				_ = s.store.AddEvent(ctx, job.ID, "error", "manual movie search timed out after no main title", map[string]any{"error": waitErr.Error()})
				_ = s.store.UpdateStatus(ctx, job.ID, "error")
			}
			return fmt.Errorf("no main titles found on disc %q", device)
		}

		if year > 0 {
			mediaTitle = fmt.Sprintf("%s (%d)", title, year)
		} else {
			mediaTitle = title
		}
		s.beginRipTitle(device, mediaTitle)
		run.title = mediaTitle

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
	totalTitles := len(result.MainTitles)
	if s.store != nil {
		_ = s.store.UpdateStatus(ctx, job.ID, "ripping")
	}
	etaTr := &etaTracker{}
	for idx, title := range result.MainTitles {
		cur := s.currentTitle(device, mediaTitle)
		s.emit(ProgressEvent{
			Device:  device,
			Stage:   "analyzing",
			Title:   cur,
			Percent: 0,
			Message: fmt.Sprintf("Analyzing disc for %s (title %d of %d)", cur, idx+1, totalTitles),
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
		progressCb := func(_ int, percent int, phase ripper.RipPhase) {
			// Read the live title each tick so a mid-rip re-identify is reflected.
			cur := s.currentTitle(device, mediaTitle)
			if phase == ripper.PhaseAnalyze {
				s.emit(ProgressEvent{
					Device:  device,
					Stage:   "analyzing",
					Title:   cur,
					Percent: percent,
					Message: fmt.Sprintf("Analyzing disc for %s (%d%%)", cur, percent),
				})
				return
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
			files, err = ripper.RipTitle(
				ctx,
				device,
				title,
				ripOutputDir,
				s.cfg.MakeMKV.Key,
				s.cfg.MakeMKV.TimeoutMinutes,
				cacheMBForDisc(s.cfg.MakeMKV.CacheMB, scanned.Type),
				s.cfg.MakeMKV.ReadErrorLimit,
				s.cfg.MakeMKV.NoProgressMin,
				progressCb,
			)
			if err == nil {
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
	}

	// An automatic identification whose runtime doesn't fit the ripped title is
	// probably the wrong movie. Hold the file in staging instead of delivering
	// it under that name, and wait for the user to confirm or correct the title.
	// Editing the title (even to the same one) releases the hold.
	if s.store != nil && needsConfirmation(result.MainTitles, len(rippedFiles), s.titleIsLocked(device), runtimeMin) {
		held := int(result.MainTitles[0].Duration.Minutes())
		summary := fmt.Sprintf("Ripped title is %d min but %q runs %d min, so the match is probably wrong. The file is held in staging and not delivered: edit the title in the UI (pick the right movie, or re-select the same one to confirm).",
			held, s.currentTitle(device, mediaTitle), runtimeMin)
		s.emit(ProgressEvent{Device: device, Stage: "identifying", Title: s.currentTitle(device, mediaTitle), Percent: 95, Message: "Waiting for title confirmation: " + summary})
		_ = s.store.AddEvent(ctx, job.ID, "identify", "held before delivery: "+summary, map[string]any{"held": true, "ripped_minutes": held, "runtime_minutes": runtimeMin})
		_ = s.store.UpdateStatus(ctx, job.ID, "identifying")
		s.notifier.Notify(notify.Message{
			Event: notify.EventDurationMismatch, JobID: job.ID, Disc: scanned.DiscName, Device: device,
			Title: s.currentTitle(device, mediaTitle), Summary: summary,
		})
		title, year, waitErr := s.awaitManualReidentify(ctx, job.ID, s.cfg.Notification.ResponseTimeoutMin)
		if errors.Is(context.Cause(ctx), errRestart) {
			s.restartCleanup(device, stagingDir, ripOutputDir, job.ID, result.MainTitles[0].Index)
			return errRestart
		}
		if waitErr != nil {
			msg := "No title confirmation received; the ripped file was left in staging: " + ripOutputDir
			s.emit(ProgressEvent{Device: device, Stage: "error", Title: s.currentTitle(device, mediaTitle), Message: msg})
			_ = s.store.AddEvent(ctx, job.ID, "error", msg, map[string]any{"error": waitErr.Error()})
			_ = s.store.UpdateStatus(ctx, job.ID, "error")
			return fmt.Errorf("title not confirmed for disc %q: %w", device, waitErr)
		}
		mediaTitle = title
		if year > 0 {
			mediaTitle = fmt.Sprintf("%s (%d)", title, year)
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
		if renamed, rerr := output.RenameForDelivery(rippedFiles, deliverTitle); rerr != nil {
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
			deliverTitle,
			deliverTitle,
			scanned.DiscName,
		)
		if err != nil {
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
		mediaTitle,
		scanned.DiscName,
		destDir,
		rippedFiles,
		media,
	)

	if delivered {
		details := make([]string, 0, len(media))
		for _, m := range media {
			line := fmt.Sprintf("%s (%.1f GB", m.File, float64(m.SizeBytes)/(1<<30))
			if m.Resolution != "" {
				line += ", " + m.Resolution
			}
			details = append(details, line+")")
		}
		s.notifier.Notify(notify.Message{
			Event:   notify.EventComplete,
			JobID:   job.ID,
			Disc:    scanned.DiscName,
			Device:  device,
			Title:   run.title,
			Summary: fmt.Sprintf("Delivered and verified: %d file(s) in %s.", len(rippedFiles), filepath.Base(filepath.Dir(rippedFiles[0]))),
			Details: details,
		})
	}

	if err := s.notify.Send(ctx, payload); err != nil {
		// Notification failure is not fatal — the rip succeeded.
		s.emit(ProgressEvent{
			Device:  device,
			Stage:   "done",
			Title:   "",
			Percent: 100,
			Message: "Rip completed (notification failed)",
		})
		if s.store != nil {
			_ = s.store.UpdateStatus(ctx, job.ID, "done")
		}
		return nil
	}

	s.emit(ProgressEvent{
		Device:  device,
		Stage:   "done",
		Title:   "",
		Percent: 100,
		Message: "Rip completed successfully",
	})
	if s.store != nil {
		_ = s.store.UpdateStatus(ctx, job.ID, "done")
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
// reconciled. Returns an error if the TMDB API key is not configured.
func (s *RipService) EnrichMovie(ctx context.Context, chosen metadata.MovieResult) (*metadata.MovieDetails, error) {
	if s.cfg.Metadata.TMDBApiKey == "" {
		return nil, fmt.Errorf("metadata.tmdb_api_key not configured")
	}

	tmdbClient := newTMDBClient(s.cfg.Metadata.TMDBApiKey)

	var omdbClient *metadata.OMDbClient
	if s.cfg.Metadata.OMDbApiKey != "" {
		omdbClient = newOMDbClient(s.cfg.Metadata.OMDbApiKey)
	}

	return metadata.Enrich(ctx, tmdbClient, omdbClient, chosen)
}

// SearchMovie searches TMDB for movies matching query. When the initial query
// returns no results, it retries with progressively shorter queries by dropping
// the last word until results are found or all words are exhausted.
// Returns an error if the TMDB API key is not configured or no results are found.
func (s *RipService) SearchMovie(ctx context.Context, query string) ([]metadata.MovieResult, error) {
	if s.cfg.Metadata.TMDBApiKey == "" {
		return nil, fmt.Errorf("metadata.tmdb_api_key not configured")
	}

	client := newTMDBClient(s.cfg.Metadata.TMDBApiKey)

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
func (s *RipService) RestartIfNeeded(device string) bool {
	s.ripMu.Lock()
	defer s.ripMu.Unlock()
	st, ok := s.restarts[device]
	if !ok || st.restarting || !st.reselect || s.titleFrozen[device] {
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
	if len(st.selected) > 0 {
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
