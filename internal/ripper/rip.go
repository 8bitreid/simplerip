package ripper

import (
	"bufio"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
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

// RipOptions configures a makemkvcon rip.
type RipOptions struct {
	// Key is the MakeMKV licence key, written to settings.conf so it never
	// appears in the process list.
	Key               string
	TimeoutMinutes    int
	CacheMB           int // read cache size; 256 when <= 0
	ReadErrorLimit    int // abort after this many read errors; 0 disables
	NoProgressMinutes int // abort when stalled this long; 0 disables
	Progress          ProgressCallback
}

func (o RipOptions) cacheMB() int {
	if o.CacheMB <= 0 {
		return 256
	}
	return o.CacheMB
}

// makemkvArgs builds a robot-mode `mkv` command line. extra flags go before
// the command.
func makemkvArgs(cacheMB int, device, titleArg, outputDir string, extra ...string) []string {
	args := []string{
		fmt.Sprintf("--cache=%d", cacheMB),
		"--noscan",
		"-r",
		"--messages=-stdout",
		"--progress=-stdout",
	}
	args = append(args, extra...)
	return append(args, "mkv", "dev:"+device, titleArg, outputDir)
}

// msgReadError is the makemkvcon message code for a failed disc read.
const msgReadError = 2003

// readErrorLog counts MakeMKV read errors and remembers where the first one
// happened.
type readErrorLog struct {
	count  int
	file   string
	offset int64
}

func newReadErrorLog() readErrorLog {
	return readErrorLog{offset: -1}
}

// record notes a read error from a MSG payload and reports whether limit
// (when positive) has been reached.
func (r *readErrorLog) record(payload string, limit int) bool {
	r.count++
	if r.offset < 0 {
		r.file, r.offset = parseReadErrorLocation(payload)
	}
	return limit > 0 && r.count >= limit
}

func (r *readErrorLog) errorFor(cause error, title int, device, detail string) *ReadError {
	return &ReadError{Cause: cause, Title: title, Device: device, File: r.file,
		Offset: r.offset, Count: r.count, Detail: detail}
}

// RipTitle runs:
//
//	makemkvcon --cache=<cacheMB> --noscan -r --messages=-stdout --progress=-stdout
//	             mkv dev:<device> <title.Index> <outputDir>
//
// Progress lines are logged to stdout as "title <index>: <pct>%".
// If opts.Progress is non-nil, it is called with each percentage update.
// On success the paths of *.mkv files written to outputDir are returned.
// On deadline-exceeded ErrRipTimeout is returned (wrapping the error so
// errors.Is works).
func RipTitle(ctx context.Context, device string, title disc.MKVTitle, outputDir string, opts RipOptions) ([]string, error) {
	timeout := time.Duration(opts.TimeoutMinutes) * time.Minute
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return nil, fmt.Errorf("create output dir: %w", err)
	}

	if err := writeTunedConfig(opts.Key); err != nil {
		return nil, fmt.Errorf("write makemkv config: %w", err)
	}

	cmd := exec.CommandContext(ctx, "makemkvcon",
		makemkvArgs(opts.cacheMB(), device, strconv.Itoa(title.Index), outputDir)...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start makemkvcon: %w", err)
	}

	run := &singleRip{
		title:          title.Index,
		device:         device,
		opts:           opts,
		phase:          PhaseAnalyze,
		lastPct:        -1,
		lastProgressAt: time.Now(),
		reads:          newReadErrorLog(),
	}
	if abortErr := run.consume(ctx, streamLines(stdout)); abortErr != nil {
		cancel()
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
	if len(files) == 0 && (run.saveFailed || run.reads.count > 0) {
		return nil, run.reads.errorFor(ErrRipSaveFailed, title.Index, device, "")
	}
	return files, nil
}

// streamLines drains r in a goroutine so ctx cancellation can stop the reader
// loop without waiting for the pipe to close (which requires the process — or
// any child that inherited the fd — to exit first).
func streamLines(r io.Reader) <-chan string {
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		s := bufio.NewScanner(r)
		for s.Scan() {
			lines <- s.Text()
		}
	}()
	return lines
}

// singleRip tracks the output of a one-title makemkvcon run.
type singleRip struct {
	title          int
	device         string
	opts           RipOptions
	phase          RipPhase
	lastPct        int
	lastProgressAt time.Time
	reads          readErrorLog
	saveFailed     bool
}

// consume processes makemkvcon output until it ends or ctx is done. A non-nil
// error means the rip must be aborted.
func (r *singleRip) consume(ctx context.Context, lines <-chan string) error {
	watchdog := time.NewTicker(10 * time.Second)
	defer watchdog.Stop()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return nil
			}
			if err := r.handleLine(line); err != nil {
				return err
			}
		case <-watchdog.C:
			if r.stalled() {
				return r.reads.errorFor(ErrRipNoProgress, r.title, r.device,
					fmt.Sprintf("%d min stalled", r.opts.NoProgressMinutes))
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func (r *singleRip) stalled() bool {
	return r.opts.NoProgressMinutes > 0 && r.reads.count > 0 &&
		time.Since(r.lastProgressAt) >= time.Duration(r.opts.NoProgressMinutes)*time.Minute
}

func (r *singleRip) handleLine(line string) error {
	switch {
	case strings.HasPrefix(line, "PRGV:"):
		r.handleProgress(line[len("PRGV:"):])
	case strings.HasPrefix(line, "MSG:"):
		if r.handleMessage(line[len("MSG:"):]) {
			return r.reads.errorFor(ErrRipReadErrorLimit, r.title, r.device, "")
		}
		// Log error and warning messages from makemkvcon
		fmt.Fprintln(os.Stderr, line)
	}
	return nil
}

func (r *singleRip) handleProgress(payload string) {
	prog, ok := parsePRGV(payload)
	if !ok {
		return
	}
	pct := prog.percent()
	if pct == r.lastPct {
		return
	}
	r.lastPct = pct
	r.lastProgressAt = time.Now()
	fmt.Printf("title %d: %d%%\n", r.title, pct)
	if r.opts.Progress != nil {
		r.opts.Progress(r.title, pct, r.phase)
	}
}

// handleMessage updates state from a MSG payload and reports whether the read
// error limit was reached.
func (r *singleRip) handleMessage(payload string) bool {
	switch parseMSGCode(payload) {
	case msgSavingTitles:
		if r.phase == PhaseAnalyze {
			// Progress restarts at 0% for the save pass; force that to be reported.
			r.phase = PhaseSave
			r.lastPct = -1
		}
	case msgSaveTitleFailed:
		r.saveFailed = true
	case msgReadError:
		return r.reads.record(payload, r.opts.ReadErrorLimit)
	}
	return false
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

// batchTimeoutUnit scales RipTitles' opts.TimeoutMinutes; tests shrink it.
var batchTimeoutUnit = time.Minute

// RipTitles runs one MakeMKV process for the selected titles.
// opts.TimeoutMinutes is the already-computed batch timeout. Files for
// finished titles are returned even when the command fails, allowing the
// caller to retry only titles that are missing; outputs that cannot be trusted
// are deleted.
func RipTitles(ctx context.Context, device string, titles []disc.MKVTitle, outputDir string, opts RipOptions) ([]string, error) {
	if len(titles) == 0 {
		return nil, fmt.Errorf("no titles selected for batch rip")
	}
	if opts.TimeoutMinutes < 1 {
		return nil, fmt.Errorf("batch timeout must be positive")
	}
	minimumLengthSeconds, err := batchMinimumLength(titles)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("create output dir: %w", err)
	}
	preexisting, err := snapshotMKVFiles(outputDir)
	if err != nil {
		return nil, err
	}
	if err := writeTunedConfig(opts.Key); err != nil {
		return nil, fmt.Errorf("write makemkv config: %w", err)
	}

	timeout := time.Duration(opts.TimeoutMinutes) * batchTimeoutUnit
	ripCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ripCtx, "makemkvcon",
		makemkvArgs(opts.cacheMB(), device, "all", outputDir,
			fmt.Sprintf("--minlength=%d", minimumLengthSeconds))...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	startedAt := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start makemkvcon: %w", err)
	}

	lines, scannerDone, scannerErr := scanBatchOutput(ripCtx, stdout)
	b := newBatchRun(device, titles, outputDir, preexisting, opts, startedAt)
	abortErr := b.consume(ripCtx, lines)
	if abortErr != nil {
		cancel()
	}

	awaitScanner(device, stdout, scannerDone)
	stdoutErr := <-scannerErr
	waitErr := cmd.Wait()
	// A title can finish between the last check and exit, so look once more.
	if err := b.refreshActiveTitle(); err != nil {
		return nil, fmt.Errorf("find MakeMKV outputs: %w", err)
	}
	files, err := freshMKVFiles(outputDir, preexisting)
	if err != nil {
		return nil, err
	}
	b.logPhaseDurations()

	var runErr error
	switch {
	case abortErr != nil:
		runErr = abortErr
	case stdoutErr != nil:
		runErr = fmt.Errorf("read makemkvcon stdout: %w", stdoutErr)
	case ctx.Err() != nil:
		runErr = ctx.Err()
	case ripCtx.Err() == context.DeadlineExceeded:
		runErr = b.readErr(ErrRipTimeout, fmt.Sprintf("batch of %d titles timed out", len(titles)))
	case waitErr != nil:
		runErr = b.readErr(fmt.Errorf("makemkvcon: %w", waitErr), "")
	}
	return b.finish(ctx, files, runErr)
}

// batchMinimumLength returns the --minlength value (seconds) that keeps every
// selected title.
func batchMinimumLength(titles []disc.MKVTitle) (int, error) {
	minimumDuration := titles[0].Duration
	for _, title := range titles[1:] {
		minimumDuration = min(minimumDuration, title.Duration)
	}
	seconds := int(minimumDuration/time.Second) - 1
	if seconds < 1 {
		return 0, fmt.Errorf("shortest selected title is too short for MakeMKV minimum length")
	}
	return seconds, nil
}

// scanBatchOutput streams makemkvcon stdout lines until EOF or ctx is done.
// done closes when the reader goroutine exits; errc then holds its error.
func scanBatchOutput(ctx context.Context, r io.Reader) (lines <-chan string, done <-chan struct{}, errc <-chan error) {
	lineCh := make(chan string, 64)
	doneCh := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		defer close(doneCh)
		defer close(lineCh)
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			select {
			case lineCh <- scanner.Text():
			case <-ctx.Done():
				errCh <- scanner.Err()
				return
			}
		}
		errCh <- scanner.Err()
	}()
	return lineCh, doneCh, errCh
}

// awaitScanner waits for the stdout reader to exit, closing the pipe to
// unblock it if makemkvcon (or a child holding the fd) keeps it open.
func awaitScanner(device string, stdout io.Closer, done <-chan struct{}) {
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		slog.Warn("timed out waiting for makemkvcon stdout reader; closing pipe", "device", device)
		if closeErr := stdout.Close(); closeErr != nil {
			slog.Warn("close makemkvcon stdout pipe", "device", device, "error", closeErr)
		}
		<-done
	}
}

// batchRun tracks the output of a multi-title makemkvcon run.
type batchRun struct {
	device      string
	titles      []disc.MKVTitle
	outputDir   string
	preexisting map[string]fileStamp
	opts        RipOptions
	tracker     *titleTracker
	reads       readErrorLog

	phase             RipPhase
	analyzeStartedAt  time.Time
	saveStartedAt     time.Time
	lastProgressAt    time.Time
	lastPct           int
	lastReportedIndex int

	activeTitleIndex     int
	activeTitleName      string
	activeTitleStartedAt time.Time
}

func newBatchRun(device string, titles []disc.MKVTitle, outputDir string, preexisting map[string]fileStamp, opts RipOptions, startedAt time.Time) *batchRun {
	return &batchRun{
		device:            device,
		titles:            titles,
		outputDir:         outputDir,
		preexisting:       preexisting,
		opts:              opts,
		tracker:           newTitleTracker(titles),
		reads:             newReadErrorLog(),
		phase:             PhaseAnalyze,
		analyzeStartedAt:  startedAt,
		lastProgressAt:    startedAt,
		lastPct:           -1,
		lastReportedIndex: -1,
		activeTitleIndex:  titles[0].Index,
		activeTitleName:   titles[0].Name,
	}
}

func (b *batchRun) readErr(cause error, detail string) *ReadError {
	return b.reads.errorFor(cause, b.activeTitleIndex, b.device, detail)
}

func (b *batchRun) logTitleSaveComplete(at time.Time) {
	if b.activeTitleIndex >= 0 && !b.activeTitleStartedAt.IsZero() {
		slog.Info("per-title save duration", "device", b.device, "title_index", b.activeTitleIndex,
			"title_name", b.activeTitleName, "duration", at.Sub(b.activeTitleStartedAt).String())
		b.activeTitleStartedAt = time.Time{}
	}
}

func (b *batchRun) setActiveTitle(index int, name string) {
	if index == b.activeTitleIndex {
		return
	}
	if b.phase == PhaseSave {
		now := time.Now()
		b.logTitleSaveComplete(now)
		b.activeTitleStartedAt = now
	}
	b.activeTitleIndex, b.activeTitleName = index, name
}

func (b *batchRun) refreshActiveTitle() error {
	files, err := freshMKVFiles(b.outputDir, b.preexisting)
	if err != nil {
		return err
	}
	b.tracker.observe(files)
	if index, name := newestTitleFromFiles(files, b.titles); index >= 0 {
		b.setActiveTitle(index, name)
	}
	return nil
}

// consume processes makemkvcon output until it ends or ctx is done. A non-nil
// error means the rip must be aborted.
func (b *batchRun) consume(ctx context.Context, lines <-chan string) error {
	watchdog := time.NewTicker(10 * time.Second)
	defer watchdog.Stop()
	titlePoll := time.NewTicker(250 * time.Millisecond)
	defer titlePoll.Stop()

	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return nil
			}
			if err := b.handleLine(line); err != nil {
				return err
			}
		case <-titlePoll.C:
			if err := b.refreshActiveTitle(); err != nil {
				return fmt.Errorf("find active title output: %w", err)
			}
		case <-watchdog.C:
			if watchdogExpired(b.phase, b.reads.count, b.opts.NoProgressMinutes, b.lastProgressAt, time.Now()) {
				return b.readErr(ErrRipNoProgress, fmt.Sprintf("%d min stalled", b.opts.NoProgressMinutes))
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func (b *batchRun) handleLine(line string) error {
	switch {
	case strings.HasPrefix(line, "PRGC:"), strings.HasPrefix(line, "PRGT:"):
		// Logged raw so real batch runs show which task each PRGV field tracks.
		slog.Info("makemkv progress label", "device", b.device, "line", line)
	case strings.HasPrefix(line, "PRGV:"):
		return b.handleProgress(line)
	case strings.HasPrefix(line, "MSG:"):
		if b.handleMessage(line[len("MSG:"):]) {
			return b.readErr(ErrRipReadErrorLimit, "")
		}
		fmt.Fprintln(os.Stderr, line)
	}
	return nil
}

func (b *batchRun) handleProgress(line string) error {
	prog, ok := parsePRGV(line[len("PRGV:"):])
	if !ok {
		return nil
	}
	// Check outputs first so a PRGV printed just after the next title's
	// file appears is credited to that title, not the previous one.
	if err := b.refreshActiveTitle(); err != nil {
		return fmt.Errorf("find active title output: %w", err)
	}
	pct := batchProgressPercent(prog, b.phase)
	if pct == b.lastPct && b.activeTitleIndex == b.lastReportedIndex {
		return nil
	}
	b.lastPct, b.lastReportedIndex = pct, b.activeTitleIndex
	b.lastProgressAt = time.Now()
	fmt.Printf("title %d: %d%% (%s)\n", b.activeTitleIndex, pct, line)
	if b.opts.Progress != nil {
		b.opts.Progress(b.activeTitleIndex, pct, b.phase)
	}
	return nil
}

// handleMessage updates state from a MSG payload and reports whether the read
// error limit was reached.
func (b *batchRun) handleMessage(payload string) bool {
	switch parseMSGCode(payload) {
	case msgSavingTitles:
		if b.phase == PhaseAnalyze {
			b.startSavePhase()
		}
	case msgSaveTitleFailed:
		b.tracker.markFailed(payload)
	case msgReadError:
		return b.reads.record(payload, b.opts.ReadErrorLimit)
	}
	return false
}

func (b *batchRun) startSavePhase() {
	now := time.Now()
	slog.Info("batch analyze duration", "device", b.device, "title_count", len(b.titles),
		"duration", now.Sub(b.analyzeStartedAt).String())
	b.phase = PhaseSave
	b.saveStartedAt = now
	b.lastProgressAt = now
	b.lastPct = -1
	b.activeTitleStartedAt = now
}

func (b *batchRun) logPhaseDurations() {
	if b.phase == PhaseAnalyze {
		slog.Info("batch analyze duration", "device", b.device, "title_count", len(b.titles),
			"duration", time.Since(b.analyzeStartedAt).String(), "incomplete", true)
		return
	}
	b.logTitleSaveComplete(time.Now())
	if !b.saveStartedAt.IsZero() {
		slog.Info("batch save duration", "device", b.device, "title_count", len(b.titles),
			"duration", time.Since(b.saveStartedAt).String())
	}
}

// finish settles the outputs against the run result: it keeps trustworthy
// files and reports the first selected title that was not written.
func (b *batchRun) finish(ctx context.Context, files []string, runErr error) ([]string, error) {
	if b.tracker.failedUnknown {
		b.tracker.resolveUnnamedFailure(ctx)
	}
	if runErr == nil {
		if _, err := MapTitleFiles(b.titles, files); err != nil {
			return nil, err
		}
	}
	kept, settleErr := b.tracker.settle(files, runErr == nil)
	if runErr != nil {
		if settleErr != nil {
			return kept, errors.Join(runErr, settleErr)
		}
		return kept, runErr
	}
	if settleErr != nil {
		return kept, settleErr
	}
	for _, title := range b.titles {
		if b.tracker.complete(title.Index, true) {
			continue
		}
		detail := fmt.Sprintf("title index %d was not written", title.Index)
		if b.tracker.failed[title.Index] {
			detail = fmt.Sprintf("MakeMKV failed to save title index %d", title.Index)
		}
		return kept, b.reads.errorFor(ErrRipSaveFailed, title.Index, b.device, detail)
	}
	if b.reads.count > 0 {
		slog.Warn("MakeMKV reported recoverable read errors; all selected title files were written",
			"device", b.device, "read_errors", b.reads.count, "title_count", len(b.titles))
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
