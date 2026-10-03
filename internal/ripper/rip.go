package ripper

import (
	"bufio"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/8bitreid/simplerip/internal/disc"
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
