package ripper

import (
	"bufio"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/8bitreid/simplerip/internal/disc"
	"github.com/8bitreid/simplerip/internal/inspect"
)

// ErrRipTimeout is returned when makemkvcon exceeds timeoutMinutes.
// Use errors.Is to detect it distinctly from other failures.
var ErrRipTimeout = errors.New("makemkvcon: rip timed out")

// ErrRipReadErrorLimit is returned when MakeMKV emits too many read errors.
var ErrRipReadErrorLimit = errors.New("makemkvcon: read error limit reached")

// ErrRipNoProgress is returned when the rip stalls for too long.
var ErrRipNoProgress = errors.New("makemkvcon: no progress")

// ErrRipSaveFailed is returned when makemkvcon finishes without writing the
// title, typically because part of the disc could not be read.
var ErrRipSaveFailed = errors.New("makemkvcon: title could not be saved")

// ReadError describes a disc read failure, including where it happened.
// Cause is one of the ErrRip* sentinels, so errors.Is keeps working.
type ReadError struct {
	Cause  error
	Title  int
	Device string
	File   string // file on the disc, e.g. /VIDEO_TS/VTS_01_1.VOB
	Offset int64  // byte offset of the first failed read, -1 if unknown
	Count  int    // number of read errors reported
	Detail string // extra context, e.g. "12 min stalled"
}

func (e *ReadError) Unwrap() error { return e.Cause }

func (e *ReadError) Error() string {
	msg := fmt.Sprintf("%v: title %d on %s (%d read errors", e.Cause, e.Title, e.Device, e.Count)
	if e.Detail != "" {
		msg += ", " + e.Detail
	}
	msg += ")"
	if e.Offset >= 0 {
		msg += fmt.Sprintf(" first at %s offset %d", e.File, e.Offset)
	}
	return msg
}

// RipPhase identifies which of makemkvcon's two passes a progress update belongs to.
// makemkvcon first analyzes the disc (0-100%), then restarts progress at 0%
// when it begins saving the title to disk.
type RipPhase int

const (
	PhaseAnalyze RipPhase = iota // reading/analyzing the disc structure
	PhaseSave                    // writing the MKV file
)

// msgSavingTitles is the makemkvcon message code ("Saving N titles into
// directory ...") emitted when the save pass begins.
const msgSavingTitles = 5014

// ProgressCallback is called with percentage updates during ripping (0-100).
// Percent is relative to the current phase.
type ProgressCallback func(titleIndex int, percent int, phase RipPhase)

// RipTitle runs:
//
//	makemkvcon --cache=<cacheMB> --noscan -r --messages=-stdout --progress=-stdout
//	             mkv dev:<device> <title.Index> <outputDir>
//
// key is exported to the subprocess as MAKEMKV_KEY so the licence is available
// without being visible in the process list.
//
// Progress lines are logged to stdout as "title <index>: <pct>%".
// If progressCb is non-nil, it is called with each percentage update.
// On success the paths of *.mkv files written to outputDir are returned.
// On deadline-exceeded ErrRipTimeout is returned (wrapping the error so
// errors.Is works).
func RipTitle(ctx context.Context, device string, title disc.MKVTitle, outputDir string, key string, timeoutMinutes int, cacheMB int, readErrorLimit int, noProgressMinutes int, progressCb ProgressCallback) ([]string, error) {
	timeout := time.Duration(timeoutMinutes) * time.Minute
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return nil, fmt.Errorf("create output dir: %w", err)
	}

	if err := writeTunedConfig(key); err != nil {
		return nil, fmt.Errorf("write makemkv config: %w", err)
	}

	if cacheMB <= 0 {
		cacheMB = 256 // default to 256 MB if not set or invalid
	}
	cmd := exec.CommandContext(ctx,
		"makemkvcon",
		fmt.Sprintf("--cache=%d", cacheMB),
		"--noscan",
		"-r",
		"--messages=-stdout",
		"--progress=-stdout",
		"mkv",
		"dev:"+device,
		strconv.Itoa(title.Index),
		outputDir,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start makemkvcon: %w", err)
	}

	// Drain stdout in a goroutine so ctx cancellation can break us out of the
	// select below without waiting for the pipe to close (which requires the
	// process — or any child that inherited the fd — to exit first).
	lineCh := make(chan string, 64)
	go func() {
		defer close(lineCh)
		s := bufio.NewScanner(stdout)
		for s.Scan() {
			lineCh <- s.Text()
		}
	}()

	lastPct := -1
	phase := PhaseAnalyze
	lastProgressAt := time.Now()
	readErrors := 0
	firstReadFile, firstReadOffset := "", int64(-1)
	saveFailed := false
	var abortErr error
	readErr := func(cause error, detail string) *ReadError {
		return &ReadError{Cause: cause, Title: title.Index, Device: device, File: firstReadFile,
			Offset: firstReadOffset, Count: readErrors, Detail: detail}
	}

	watchdog := time.NewTicker(10 * time.Second)
	defer watchdog.Stop()
loop:
	for {
		select {
		case line, ok := <-lineCh:
			if !ok {
				break loop
			}
			if strings.HasPrefix(line, "PRGV:") {
				prog, ok := parsePRGV(line[len("PRGV:"):])
				if !ok {
					continue
				}
				if pct := prog.percent(); pct != lastPct {
					lastPct = pct
					lastProgressAt = time.Now()
					fmt.Printf("title %d: %d%%\n", title.Index, pct)
					if progressCb != nil {
						progressCb(title.Index, pct, phase)
					}
				}
			} else if strings.HasPrefix(line, "MSG:") {
				code := parseMSGCode(line[len("MSG:"):])
				if code == msgSavingTitles && phase == PhaseAnalyze {
					// Progress restarts at 0% for the save pass; force that to be reported.
					phase = PhaseSave
					lastPct = -1
				}
				if code == msgSaveTitleFailed {
					saveFailed = true
				}
				if code == 2003 {
					readErrors++
					if firstReadOffset < 0 {
						firstReadFile, firstReadOffset = parseReadErrorLocation(line[len("MSG:"):])
					}
					if readErrorLimit > 0 && readErrors >= readErrorLimit {
						abortErr = readErr(ErrRipReadErrorLimit, "")
						cancel()
						break loop
					}
				}
				// Log error and warning messages from makemkvcon
				fmt.Fprintln(os.Stderr, line)
			}
		case <-watchdog.C:
			if noProgressMinutes > 0 && readErrors > 0 && time.Since(lastProgressAt) >= time.Duration(noProgressMinutes)*time.Minute {
				abortErr = readErr(ErrRipNoProgress, fmt.Sprintf("%d min stalled", noProgressMinutes))
				cancel()
				break loop
			}
		case <-ctx.Done():
			break loop
		}
	}

	if abortErr != nil {
		_ = cmd.Wait()
		return nil, abortErr
	}

	if werr := cmd.Wait(); werr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("%w: title %d on %s", ErrRipTimeout, title.Index, device)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("makemkvcon: %w", werr)
	}

	files, err := newMKVFiles(outputDir, start)
	if err != nil {
		return nil, err
	}
	// makemkvcon can exit cleanly (and report 100%) after giving up on a
	// title it could not read; surface that instead of an empty result.
	if len(files) == 0 && (saveFailed || readErrors > 0) {
		return nil, readErr(ErrRipSaveFailed, "")
	}
	return files, nil
}

// CalculateBatchTimeoutMinutes returns the total timeout for one multi-title
// MakeMKV invocation.
func CalculateBatchTimeoutMinutes(analyzeBudgetMinutes, saveBudgetMinutes, titleCount int) (int, error) {
	if analyzeBudgetMinutes < 1 || saveBudgetMinutes < 1 || titleCount < 1 {
		return 0, fmt.Errorf("batch analyze budget, save budget, and title count must be positive")
	}
	if titleCount > (int(^uint(0)>>1)-analyzeBudgetMinutes)/saveBudgetMinutes {
		return 0, fmt.Errorf("batch timeout overflows")
	}
	return analyzeBudgetMinutes + titleCount*saveBudgetMinutes, nil
}

// MapTitleFiles maps MakeMKV output files to selected title indexes. MakeMKV's
// output suffix is zero-based (for example, disc_t02.mkv represents index 2).
// Files for unselected titles are ignored.
func MapTitleFiles(titles []disc.MKVTitle, files []string) (map[int]string, error) {
	selected := make(map[int]bool, len(titles))
	for _, title := range titles {
		if selected[title.Index] {
			return nil, fmt.Errorf("duplicate selected title index %d", title.Index)
		}
		selected[title.Index] = true
	}

	mapped := make(map[int]string, len(titles))
	for _, file := range files {
		index, ok := titleIndexFromFilename(file)
		if !ok || !selected[index] {
			continue
		}
		if previous, exists := mapped[index]; exists {
			return nil, fmt.Errorf("multiple MakeMKV files for title index %d: %q and %q", index, previous, file)
		}
		mapped[index] = file
	}
	return mapped, nil
}

func titleIndexFromFilename(path string) (int, bool) {
	name := strings.ToLower(filepath.Base(path))
	if !strings.HasSuffix(name, ".mkv") {
		return 0, false
	}
	suffix := strings.LastIndex(name[:len(name)-len(".mkv")], "_t")
	if suffix < 0 || suffix+2 == len(name)-len(".mkv") {
		return 0, false
	}
	index, err := strconv.Atoi(name[suffix+2 : len(name)-len(".mkv")])
	return index, err == nil && index >= 0
}

// batchTimeoutUnit scales RipTitles' timeoutMinutes; tests shrink it.
var batchTimeoutUnit = time.Minute

// RipTitles runs one MakeMKV process for the selected titles. timeoutMinutes is
// the already-computed batch timeout. Files for finished titles are returned
// even when the command fails, allowing the caller to retry only titles that
// are missing; outputs that cannot be trusted are deleted.
func RipTitles(
	ctx context.Context,
	device string,
	titles []disc.MKVTitle,
	outputDir, key string,
	timeoutMinutes, cacheMB, readErrorLimit, noProgressMinutes int,
	progressCb ProgressCallback,
) ([]string, error) {
	if len(titles) == 0 {
		return nil, fmt.Errorf("no titles selected for batch rip")
	}
	if timeoutMinutes < 1 {
		return nil, fmt.Errorf("batch timeout must be positive")
	}
	minimumDuration := titles[0].Duration
	for _, title := range titles[1:] {
		if title.Duration < minimumDuration {
			minimumDuration = title.Duration
		}
	}
	minimumLengthSeconds := int(minimumDuration/time.Second) - 1
	if minimumLengthSeconds < 1 {
		return nil, fmt.Errorf("shortest selected title is too short for MakeMKV minimum length")
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("create output dir: %w", err)
	}
	preexisting, err := snapshotMKVFiles(outputDir)
	if err != nil {
		return nil, err
	}
	if err := writeTunedConfig(key); err != nil {
		return nil, fmt.Errorf("write makemkv config: %w", err)
	}

	if cacheMB <= 0 {
		cacheMB = 256
	}
	timeout := time.Duration(timeoutMinutes) * batchTimeoutUnit
	ripCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ripCtx,
		"makemkvcon",
		fmt.Sprintf("--cache=%d", cacheMB),
		"--noscan",
		"-r",
		"--messages=-stdout",
		"--progress=-stdout",
		fmt.Sprintf("--minlength=%d", minimumLengthSeconds),
		"mkv",
		"dev:"+device,
		"all",
		outputDir,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	startedAt := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start makemkvcon: %w", err)
	}

	lineCh := make(chan string, 64)
	scannerDone := make(chan struct{})
	scannerErr := make(chan error, 1)
	go func() {
		defer close(scannerDone)
		defer close(lineCh)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			select {
			case lineCh <- scanner.Text():
			case <-ripCtx.Done():
				scannerErr <- scanner.Err()
				return
			}
		}
		scannerErr <- scanner.Err()
	}()

	analyzeStartedAt := startedAt
	saveStartedAt := time.Time{}
	lastProgressAt := startedAt
	lastPct := -1
	lastReportedIndex := -1
	phase := PhaseAnalyze
	readErrors := 0
	firstReadFile, firstReadOffset := "", int64(-1)
	var abortErr error
	tracker := newTitleTracker(titles)
	activeTitleIndex := titles[0].Index
	activeTitleName := titles[0].Name
	activeTitleStartedAt := time.Time{}
	readErrFor := func(index int, cause error, detail string) *ReadError {
		return &ReadError{Cause: cause, Title: index, Device: device,
			File: firstReadFile, Offset: firstReadOffset, Count: readErrors, Detail: detail}
	}
	readErr := func(cause error, detail string) *ReadError {
		return readErrFor(activeTitleIndex, cause, detail)
	}
	logTitleSaveComplete := func(at time.Time) {
		if activeTitleIndex >= 0 && !activeTitleStartedAt.IsZero() {
			slog.Info("per-title save duration", "device", device, "title_index", activeTitleIndex,
				"title_name", activeTitleName, "duration", at.Sub(activeTitleStartedAt).String())
			activeTitleStartedAt = time.Time{}
		}
	}
	setActiveTitle := func(index int, name string) {
		if index == activeTitleIndex {
			return
		}
		if phase == PhaseSave {
			now := time.Now()
			logTitleSaveComplete(now)
			activeTitleStartedAt = now
		}
		activeTitleIndex, activeTitleName = index, name
	}
	refreshActiveTitle := func() error {
		files, err := freshMKVFiles(outputDir, preexisting)
		if err != nil {
			return err
		}
		tracker.observe(files)
		if index, name := newestTitleFromFiles(files, titles); index >= 0 {
			setActiveTitle(index, name)
		}
		return nil
	}
	watchdog := time.NewTicker(10 * time.Second)
	defer watchdog.Stop()
	titlePoll := time.NewTicker(250 * time.Millisecond)
	defer titlePoll.Stop()

loop:
	for {
		select {
		case line, ok := <-lineCh:
			if !ok {
				break loop
			}
			if strings.HasPrefix(line, "PRGC:") || strings.HasPrefix(line, "PRGT:") {
				// Logged raw so real batch runs show which task each PRGV field tracks.
				slog.Info("makemkv progress label", "device", device, "line", line)
				continue
			}
			if strings.HasPrefix(line, "PRGV:") {
				prog, ok := parsePRGV(line[len("PRGV:"):])
				if !ok {
					continue
				}
				// Check outputs first so a PRGV printed just after the next title's
				// file appears is credited to that title, not the previous one.
				if err := refreshActiveTitle(); err != nil {
					abortErr = fmt.Errorf("find active title output: %w", err)
					cancel()
					break loop
				}
				if pct := batchProgressPercent(prog, phase); pct != lastPct || activeTitleIndex != lastReportedIndex {
					lastPct, lastReportedIndex = pct, activeTitleIndex
					lastProgressAt = time.Now()
					fmt.Printf("title %d: %d%% (%s)\n", activeTitleIndex, pct, line)
					if progressCb != nil {
						progressCb(activeTitleIndex, pct, phase)
					}
				}
				continue
			}
			if !strings.HasPrefix(line, "MSG:") {
				continue
			}
			payload := line[len("MSG:"):]
			code := parseMSGCode(payload)
			if code == msgSavingTitles && phase == PhaseAnalyze {
				now := time.Now()
				slog.Info("batch analyze duration", "device", device, "title_count", len(titles),
					"duration", now.Sub(analyzeStartedAt).String())
				phase = PhaseSave
				saveStartedAt = now
				lastProgressAt = now
				lastPct = -1
				activeTitleStartedAt = now
			}
			if code == msgSaveTitleFailed {
				tracker.markFailed(payload)
			}
			if code == 2003 {
				readErrors++
				if firstReadOffset < 0 {
					firstReadFile, firstReadOffset = parseReadErrorLocation(payload)
				}
				if readErrorLimit > 0 && readErrors >= readErrorLimit {
					abortErr = readErr(ErrRipReadErrorLimit, "")
					cancel()
					break loop
				}
			}
			fmt.Fprintln(os.Stderr, line)
		case <-titlePoll.C:
			if err := refreshActiveTitle(); err != nil {
				abortErr = fmt.Errorf("find active title output: %w", err)
				cancel()
				break loop
			}
		case <-watchdog.C:
			if watchdogExpired(phase, readErrors, noProgressMinutes, lastProgressAt, time.Now()) {
				abortErr = readErr(ErrRipNoProgress, fmt.Sprintf("%d min stalled", noProgressMinutes))
				cancel()
				break loop
			}
		case <-ripCtx.Done():
			break loop
		}
	}

	select {
	case <-scannerDone:
	case <-time.After(30 * time.Second):
		slog.Warn("timed out waiting for makemkvcon stdout reader; closing pipe", "device", device)
		if closeErr := stdout.Close(); closeErr != nil {
			slog.Warn("close makemkvcon stdout pipe", "device", device, "error", closeErr)
		}
		<-scannerDone
	}
	stdoutErr := <-scannerErr
	waitErr := cmd.Wait()
	// A title can finish between the last check and exit, so look once more.
	if err := refreshActiveTitle(); err != nil {
		return nil, fmt.Errorf("find MakeMKV outputs: %w", err)
	}
	files, err := freshMKVFiles(outputDir, preexisting)
	if err != nil {
		return nil, err
	}
	if phase == PhaseAnalyze {
		slog.Info("batch analyze duration", "device", device, "title_count", len(titles),
			"duration", time.Since(analyzeStartedAt).String(), "incomplete", true)
	} else {
		logTitleSaveComplete(time.Now())
		if !saveStartedAt.IsZero() {
			slog.Info("batch save duration", "device", device, "title_count", len(titles),
				"duration", time.Since(saveStartedAt).String())
		}
	}

	var runErr error
	switch {
	case abortErr != nil:
		runErr = abortErr
	case stdoutErr != nil:
		runErr = fmt.Errorf("read makemkvcon stdout: %w", stdoutErr)
	case ctx.Err() != nil:
		runErr = ctx.Err()
	case ripCtx.Err() == context.DeadlineExceeded:
		runErr = readErr(ErrRipTimeout, fmt.Sprintf("batch of %d titles timed out", len(titles)))
	case waitErr != nil:
		runErr = readErr(fmt.Errorf("makemkvcon: %w", waitErr), "")
	}
	if tracker.failedUnknown {
		tracker.resolveUnnamedFailure(ctx)
	}
	if runErr == nil {
		if _, err := MapTitleFiles(titles, files); err != nil {
			return nil, err
		}
	}
	kept, settleErr := tracker.settle(files, runErr == nil)
	if runErr != nil {
		if settleErr != nil {
			return kept, errors.Join(runErr, settleErr)
		}
		return kept, runErr
	}
	if settleErr != nil {
		return kept, settleErr
	}
	for _, title := range titles {
		if tracker.complete(title.Index, true) {
			continue
		}
		detail := fmt.Sprintf("title index %d was not written", title.Index)
		if tracker.failed[title.Index] {
			detail = fmt.Sprintf("MakeMKV failed to save title index %d", title.Index)
		}
		return kept, readErrFor(title.Index, ErrRipSaveFailed, detail)
	}
	if readErrors > 0 {
		slog.Warn("MakeMKV reported recoverable read errors; all selected title files were written",
			"device", device, "read_errors", readErrors, "title_count", len(titles))
	}
	return kept, nil
}

func watchdogExpired(phase RipPhase, readErrors, noProgressMinutes int, lastProgressAt, now time.Time) bool {
	return noProgressMinutes > 0 &&
		(phase == PhaseSave || readErrors > 0) &&
		now.Sub(lastProgressAt) >= time.Duration(noProgressMinutes)*time.Minute
}

// batchProgressPercent converts a PRGV payload to a percent. PRGV is
// current,total,max: current tracks the task named by the last PRGC, which
// MakeMKV restarts for each title while saving; total tracks the whole
// operation named by PRGT. Analyze reports total so the bar doesn't restart
// at every analysis step; save reports current so the percent is per title.
func batchProgressPercent(prog ripProgress, phase RipPhase) int {
	if prog.max <= 0 {
		return 0
	}
	value := prog.total
	if phase == PhaseSave {
		value = prog.current
	}
	return min(max(value*100/prog.max, 0), 100)
}

// titleTracker records the order in which selected titles' output files
// appear. MakeMKV saves one title at a time, so once a later selected title's
// file exists, every earlier one is finished.
type titleTracker struct {
	selection     []disc.MKVTitle
	selected      map[int]int    // title index -> position in selection
	order         []int          // title indexes in order of first appearance
	files         map[int]string // title index -> output path
	failed        map[int]bool   // titles named by MSG 5003
	failedUnknown bool           // a MSG 5003 named no recognizable output
}

func newTitleTracker(titles []disc.MKVTitle) *titleTracker {
	t := &titleTracker{
		selection: titles,
		selected:  make(map[int]int, len(titles)),
		files:     make(map[int]string, len(titles)),
		failed:    make(map[int]bool),
	}
	for i, title := range titles {
		t.selected[title.Index] = i
	}
	return t
}

func (t *titleTracker) observe(files []string) {
	type appearance struct {
		index, position int
		file            string
		modTime         time.Time
	}
	var appeared []appearance
	for _, file := range files {
		index, ok := titleIndexFromFilename(file)
		if !ok {
			continue
		}
		position, selected := t.selected[index]
		if _, known := t.files[index]; !selected || known {
			continue
		}
		var modTime time.Time
		if info, err := os.Stat(file); err == nil {
			modTime = info.ModTime()
		}
		appeared = append(appeared, appearance{index, position, file, modTime})
	}
	// Several titles can appear between checks; the earlier-written one finished first.
	sort.Slice(appeared, func(i, j int) bool {
		if !appeared[i].modTime.Equal(appeared[j].modTime) {
			return appeared[i].modTime.Before(appeared[j].modTime)
		}
		return appeared[i].position < appeared[j].position
	})
	for _, a := range appeared {
		t.files[a.index] = a.file
		t.order = append(t.order, a.index)
	}
}

// markFailed records the output named by a MSG 5003 payload:
// code,flags,count,"text","format",params... The file is the last parameter,
// sometimes as a file:// URL. It can name a title that already looked
// finished, so the active title is never assumed to be the one that failed.
func (t *titleTracker) markFailed(payload string) {
	r := csv.NewReader(strings.NewReader(payload))
	r.LazyQuotes = true
	r.FieldsPerRecord = -1
	if fields, err := r.Read(); err == nil && len(fields) >= 6 {
		if index, ok := titleIndexFromFilename(strings.TrimPrefix(fields[len(fields)-1], "file://")); ok {
			t.failed[index] = true
			return
		}
	}
	t.failedUnknown = true
}

// probeDuration reads an output's length; tests replace it.
var probeDuration = func(ctx context.Context, path string) (time.Duration, error) {
	info, err := inspect.Probe(ctx, path)
	if err != nil {
		return 0, err
	}
	return info.Duration, nil
}

// unnamedFailureTolerance is how far an output's length may differ from the
// scanned title length and still count as fully written.
const unnamedFailureTolerance = 30 * time.Second

// resolveUnnamedFailure handles a MSG 5003 that named no output. Each written
// title is kept only if its length matches the scanned title, so one bad save
// doesn't discard every finished episode. A title that can't be probed, or has
// no scanned length to compare against, is treated as failed.
func (t *titleTracker) resolveUnnamedFailure(ctx context.Context) {
	for _, title := range t.selection {
		file, ok := t.files[title.Index]
		if !ok {
			continue
		}
		duration, err := probeDuration(ctx, file)
		if err != nil || title.Duration <= 0 ||
			!inspect.DurationWithin(duration, title.Duration, unnamedFailureTolerance) {
			slog.Warn("dropping title after unnamed MakeMKV save failure", "title_index", title.Index,
				"file", filepath.Base(file), "probed", duration.String(),
				"expected", title.Duration.String(), "error", err)
			t.failed[title.Index] = true
		}
	}
	t.failedUnknown = false
}

func (t *titleTracker) complete(index int, clean bool) bool {
	if t.failedUnknown || t.failed[index] {
		return false
	}
	pos := slices.Index(t.order, index)
	return pos >= 0 && (clean || pos < len(t.order)-1)
}

// settle deletes outputs that can't be trusted (unfinished or failed selected
// titles, and unselected _tNN titles) and returns finished selected files in
// selection order. Files whose names don't parse as _tNN are never touched.
func (t *titleTracker) settle(files []string, clean bool) ([]string, error) {
	var errs []error
	for _, file := range files {
		index, ok := titleIndexFromFilename(file)
		if !ok {
			continue
		}
		if _, selected := t.selected[index]; selected && t.complete(index, clean) {
			continue
		}
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove MakeMKV output %q: %w", filepath.Base(file), err))
		}
	}
	kept := make([]string, 0, len(t.order))
	for _, title := range t.selection {
		if file, ok := t.files[title.Index]; ok && t.complete(title.Index, clean) {
			kept = append(kept, file)
		}
	}
	return kept, errors.Join(errs...)
}

func newestTitleFromFiles(files []string, titles []disc.MKVTitle) (int, string) {
	selected := make(map[int]string, len(titles))
	positions := make(map[int]int, len(titles))
	for i, title := range titles {
		selected[title.Index] = title.Name
		positions[title.Index] = i
	}
	activeIndex, activeName, activePosition := -1, "", -1
	var newest time.Time
	for _, file := range files {
		index, ok := titleIndexFromFilename(file)
		if !ok {
			continue
		}
		name, selected := selected[index]
		if !selected {
			continue
		}
		info, err := os.Stat(file)
		if err != nil {
			if !os.IsNotExist(err) {
				slog.Warn("cannot stat MakeMKV title output while tracking progress",
					"file", file, "error", err)
			}
			continue
		}
		position := positions[index]
		modTime := info.ModTime()
		if activeIndex < 0 || modTime.After(newest) || (modTime.Equal(newest) && position > activePosition) {
			activeIndex, activeName, activePosition, newest = index, name, position, modTime
		}
	}
	return activeIndex, activeName
}

type fileStamp struct {
	modTime time.Time
	size    int64
}

// snapshotMKVFiles records the .mkv files already in dir, so a batch never
// mistakes a leftover for its own output, even one written the same second.
func snapshotMKVFiles(dir string) (map[string]fileStamp, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.mkv"))
	if err != nil {
		return nil, fmt.Errorf("glob %s/*.mkv: %w", dir, err)
	}
	stamps := make(map[string]fileStamp, len(matches))
	for _, p := range matches {
		if info, err := os.Stat(p); err == nil {
			stamps[p] = fileStamp{info.ModTime(), info.Size()}
		}
	}
	return stamps, nil
}

// freshMKVFiles returns .mkv files in dir that are new or changed since before.
func freshMKVFiles(dir string, before map[string]fileStamp) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.mkv"))
	if err != nil {
		return nil, fmt.Errorf("glob %s/*.mkv: %w", dir, err)
	}
	var fresh []string
	for _, p := range matches {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if old, ok := before[p]; ok && old.modTime.Equal(info.ModTime()) && old.size == info.Size() {
			continue
		}
		fresh = append(fresh, p)
	}
	return fresh, nil
}

// msgSaveTitleFailed is the makemkvcon message code "Failed to save title".
const msgSaveTitleFailed = 5003

// parseReadErrorLocation extracts the file and byte offset from a MSG:2003
// payload: code,flags,count,"text","fmt","reason","file","offset".
func parseReadErrorLocation(payload string) (string, int64) {
	r := csv.NewReader(strings.NewReader(payload))
	r.LazyQuotes = true
	r.FieldsPerRecord = -1
	f, err := r.Read()
	if err != nil || len(f) < 8 {
		return "", -1
	}
	off, err := strconv.ParseInt(strings.TrimSpace(f[7]), 10, 64)
	if err != nil {
		return "", -1
	}
	return f[6], off
}

func parseMSGCode(payload string) int {
	parts := strings.SplitN(payload, ",", 2)
	if len(parts) == 0 {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return -1
	}
	return n
}

// newMKVFiles returns paths of *.mkv files in dir whose mtime is at or after
// since (truncated to the nearest second to survive low-resolution clocks).
func newMKVFiles(dir string, since time.Time) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.mkv"))
	if err != nil {
		return nil, fmt.Errorf("glob %s/*.mkv: %w", dir, err)
	}
	cutoff := since.Truncate(time.Second)
	var result []string
	for _, p := range matches {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if !info.ModTime().Before(cutoff) {
			result = append(result, p)
		}
	}
	return result, nil
}
