// Package disc provides disc detection and polling functionality.
package disc

import (
	"bufio"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const discProbeTimeout = 90 * time.Second

// DiscEvent reports a change in disc presence on a device.
type DiscEvent struct {
	Device  string // e.g. /dev/sr0
	Present bool   // true = disc inserted, false = disc removed
}

// Probe status values reported to the onStatus callback of PollEventsWithStatus.
const (
	StatusDetecting    = "detecting"    // a probe is in flight
	StatusNoDisc       = "no_disc"      // probe confirmed the drive is empty
	StatusDiscPresent  = "disc_present" // probe found a disc
	StatusUnresponsive = "unresponsive" // probe timed out or failed
	StatusLoading      = "loading"      // disc is spinning up / being read by the drive
	StatusTrayOpen     = "tray_open"    // tray is open
)

// driveState is the kernel's answer about a drive's media.
type driveState int

const (
	driveUnsupported driveState = iota // kernel can't answer for this device; use makemkvcon
	driveLoading                       // disc is loading/spinning up; normal, retry next tick
	driveError                         // couldn't query the drive; retry next tick
	driveEmpty                         // no disc
	driveTrayOpen                      // tray is open
	driveDisc                          // disc loaded
)

// BusyDeviceTracker holds device paths currently owned by an active rip.
// Polling code can use it to avoid probing drives that are in use.
type BusyDeviceTracker struct {
	busy sync.Map // map[string]struct{}
}

// MarkBusy marks device as active.
func (t *BusyDeviceTracker) MarkBusy(device string) {
	if t == nil {
		return
	}
	t.busy.Store(device, struct{}{})
}

// MarkIdle removes device from the active set.
func (t *BusyDeviceTracker) MarkIdle(device string) {
	if t == nil {
		return
	}
	t.busy.Delete(device)
}

// IsBusy reports whether device is currently active.
func (t *BusyDeviceTracker) IsBusy(device string) bool {
	if t == nil {
		return false
	}
	_, ok := t.busy.Load(device)
	return ok
}

// Poll continuously monitors optical devices for disc insertion.
// It checks each device at the given interval by running makemkvcon.
// When a disc is newly inserted, the device path is sent on the returned channel.
// The same disc will not fire twice until it's been removed and reinserted.
// Context cancellation stops the poll loop cleanly and closes the channel.
// Each device is polled independently so one slow drive does not block the others.
//
// Poll reports insertions only; use PollEvents to also observe removals.
func Poll(ctx context.Context, devices []string, interval time.Duration) <-chan string {
	out := make(chan string)
	events := PollEventsWithBusy(ctx, devices, interval, nil)
	go func() {
		defer close(out)
		for ev := range events {
			if !ev.Present {
				continue
			}
			select {
			case out <- ev.Device:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// PollEvents monitors optical devices and reports both insertions and removals
// as DiscEvents. Each device is polled independently so one slow drive does not
// block the others. Context cancellation stops the poll loop and closes the channel.
func PollEvents(ctx context.Context, devices []string, interval time.Duration) <-chan DiscEvent {
	return PollEventsWithBusy(ctx, devices, interval, nil)
}

// PollEventsWithBusy is like PollEvents, but skips polling for any device where
// isBusy(device) returns true. Busy devices are bypassed before any drive probe,
// keeping the hardware channel clear for the active rip process.
func PollEventsWithBusy(
	ctx context.Context,
	devices []string,
	interval time.Duration,
	isBusy func(device string) bool,
) <-chan DiscEvent {
	return PollEventsWithStatus(ctx, devices, interval, isBusy, nil)
}

// PollEventsWithStatus is like PollEventsWithBusy, and additionally calls
// onStatus (if non-nil) with a Status* value as each probe starts and finishes,
// so callers can show live drive state. onStatus is invoked before any
// resulting DiscEvent is sent.
func PollEventsWithStatus(
	ctx context.Context,
	devices []string,
	interval time.Duration,
	isBusy func(device string) bool,
	onStatus func(device, status string),
) <-chan DiscEvent {
	ch := make(chan DiscEvent)
	go func() {
		var wg sync.WaitGroup
		for _, device := range devices {
			device := device
			wg.Add(1)
			go func() {
				defer wg.Done()
				pollDevice(ctx, device, interval, ch, isBusy, onStatus)
			}()
		}
		wg.Wait()
		close(ch)
	}()
	return ch
}

// pollDevice maintains independent state for a single drive, emitting a
// DiscEvent whenever disc presence changes. A failed check (drive busy during a
// rip, timeout, error) does not update state, so it never produces a spurious
// "removed" event while a rip holds the drive.
func pollDevice(
	ctx context.Context,
	device string,
	interval time.Duration,
	ch chan<- DiscEvent,
	isBusy func(device string) bool,
	onStatus func(device, status string),
) {
	// Only report changes, so a steady state never re-renders the card.
	lastStatus := ""
	report := func(status string) {
		if onStatus == nil || status == lastStatus {
			return
		}
		lastStatus = status
		onStatus(device, status)
	}
	state := false
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	check := func() {
		if isBusy != nil && isBusy(device) {
			slog.Debug("poll skipped because device busy", "device", device)
			return
		}

		// Kernel checks return instantly; only show "detecting" if the probe is slow.
		slow := time.AfterFunc(time.Second, func() { report(StatusDetecting) })
		start := time.Now()
		hasDisc, ok, status := checkDeviceStatus(ctx, device, discProbeTimeout)
		slow.Stop()
		elapsed := time.Since(start).Round(time.Millisecond)
		if !ok {
			// A loading drive is normal (disc spinning up); only warn on real failures.
			if status == StatusLoading {
				slog.Debug("drive loading", "device", device)
			} else {
				slog.Warn("drive probe failed", "device", device, "elapsed", elapsed)
			}
			report(status)
			return
		}
		switch {
		case hasDisc:
			report(StatusDiscPresent)
		case status != "":
			report(status)
		default:
			report(StatusNoDisc)
		}
		slog.Debug("drive probe result", "device", device, "has_disc", hasDisc, "previous_state", state, "elapsed", elapsed)
		if hasDisc != state {
			select {
			case ch <- DiscEvent{Device: device, Present: hasDisc}:
			case <-ctx.Done():
				return
			}
		}
		state = hasDisc
	}

	// A probe can outlast the interval, leaving a tick queued. Drain it after
	// every check so a probe never starts right behind an event the consumer
	// hasn't acted on yet (e.g. before it has marked the drive busy for a rip).
	check()
	for {
		select {
		case <-ticker.C:
		default:
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
			select {
			case <-ticker.C:
			default:
			}
		}
	}
}

// SetMakemkvPath sets a custom path for the makemkvcon binary for testing.
// This is a test hook — production code uses the system PATH.
var makemkvPath = "makemkvcon"

// useDriveIoctl makes checkDevice ask the kernel for drive status before
// falling back to a makemkvcon probe. Tests that supply a fake makemkvcon turn
// it off so they never touch a real drive.
var useDriveIoctl = true

// SetMakemkvPathForTest sets a custom makemkvcon binary path for testing.
func SetMakemkvPathForTest(path string) {
	makemkvPath = path
	useDriveIoctl = false
}

// checkDevice reports whether a disc is present in the device, using the kernel
// drive-status ioctl and falling back to a makemkvcon probe.
// Returns (hasDisc=true, ok=true) if TCOUNT > 0.
// Returns (hasDisc=false, ok=true) if TCOUNT == 0 (confirmed no disc).
// Returns (hasDisc=false, ok=false) if check failed (timeout, error, drive busy).
func checkDevice(ctx context.Context, device string, timeout time.Duration) (hasDisc bool, ok bool) {
	hasDisc, ok, _ = checkDeviceStatus(ctx, device, timeout)
	return hasDisc, ok
}

// checkDeviceStatus is checkDevice plus a Status* value for the cases where the
// kernel gave a definite non-answer (loading, tray open); status is "" otherwise.
func checkDeviceStatus(ctx context.Context, device string, timeout time.Duration) (hasDisc bool, ok bool, status string) {
	// The kernel CD-ROM status ioctl answers in microseconds and does not touch
	// the disc. makemkvcon is only used when the kernel can't answer for this
	// device at all; a drive that is merely not ready is retried on the next
	// tick instead of being probed while it spins up.
	if useDriveIoctl {
		switch ioctlDriveStatus(device) {
		case driveDisc:
			return true, true, ""
		case driveEmpty:
			return false, true, ""
		case driveTrayOpen:
			return false, true, StatusTrayOpen
		case driveLoading:
			return false, false, StatusLoading
		case driveError:
			return false, false, StatusUnresponsive
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, makemkvPath, "-r", "--cache=1", "info", "dev:"+device)
	cmd.Stderr = nil // Suppress error output
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, false, StatusUnresponsive
	}

	if err := cmd.Start(); err != nil {
		return false, false, StatusUnresponsive
	}

	// Parse output for TCOUNT line
	scanner := bufio.NewScanner(stdout)
	tcount := 0
	foundTCOUNT := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "TCOUNT:") {
			foundTCOUNT = true
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				if count, err := strconv.Atoi(parts[1]); err == nil {
					tcount = count
					// Continue draining stdout to avoid pipe deadlock
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return false, false, StatusUnresponsive
	}

	// Wait for command to finish
	if err := cmd.Wait(); err != nil {
		// If we already parsed TCOUNT, trust it even when makemkvcon exits
		// non-zero. Some drives/tools report a recoverable error after printing
		// the disc count, and disc presence is still authoritative here.
		if foundTCOUNT {
			return tcount > 0, true, ""
		}
		return false, false, StatusUnresponsive
	}

	// If we didn't find TCOUNT line, treat as check failure
	if !foundTCOUNT {
		return false, false, StatusUnresponsive
	}

	return tcount > 0, true, ""
}

// init allows tests to override the makemkvcon binary path.
func init() {
	if path := os.Getenv("TEST_MAKEMKV_PATH"); path != "" {
		makemkvPath = path
	}
}
