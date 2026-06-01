// Package disc provides disc detection and polling functionality.
package disc

import (
	"bufio"
	"context"
	"fmt"
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
	ch := make(chan DiscEvent)
	go func() {
		var wg sync.WaitGroup
		for _, device := range devices {
			device := device
			wg.Add(1)
			go func() {
				defer wg.Done()
				pollDevice(ctx, device, interval, ch, isBusy)
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
) {
	state := false
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	check := func() {
		if isBusy != nil && isBusy(device) {
			fmt.Fprintf(os.Stderr, "poll: device=%s skipped=busy\n", device)
			return
		}

		start := time.Now()
		hasDisc, ok := checkDevice(ctx, device, discProbeTimeout)
		elapsed := time.Since(start).Round(time.Millisecond)
		if !ok {
			fmt.Fprintf(os.Stderr, "poll: device=%s ok=false elapsed=%s\n", device, elapsed)
			return
		}
		fmt.Fprintf(os.Stderr, "poll: device=%s hasDisc=%t prev=%t elapsed=%s\n", device, hasDisc, state, elapsed)
		if hasDisc != state {
			select {
			case ch <- DiscEvent{Device: device, Present: hasDisc}:
			case <-ctx.Done():
				return
			}
		}
		state = hasDisc
	}

	check()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}

// SetMakemkvPath sets a custom path for the makemkvcon binary for testing.
// This is a test hook — production code uses the system PATH.
var makemkvPath = "makemkvcon"

// SetMakemkvPathForTest sets a custom makemkvcon binary path for testing.
func SetMakemkvPathForTest(path string) {
	makemkvPath = path
}

// checkDevice runs makemkvcon to check if a disc is present in the device.
// Returns (hasDisc=true, ok=true) if TCOUNT > 0.
// Returns (hasDisc=false, ok=true) if TCOUNT == 0 (confirmed no disc).
// Returns (hasDisc=false, ok=false) if check failed (timeout, error, drive busy).
func checkDevice(ctx context.Context, device string, timeout time.Duration) (hasDisc bool, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, makemkvPath, "-r", "--cache=1", "info", "dev:"+device)
	cmd.Stderr = nil // Suppress error output
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, false
	}

	if err := cmd.Start(); err != nil {
		return false, false
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
		return false, false
	}

	// Wait for command to finish
	if err := cmd.Wait(); err != nil {
		// If we already parsed TCOUNT, trust it even when makemkvcon exits
		// non-zero. Some drives/tools report a recoverable error after printing
		// the disc count, and disc presence is still authoritative here.
		if foundTCOUNT {
			return tcount > 0, true
		}
		return false, false
	}

	// If we didn't find TCOUNT line, treat as check failure
	if !foundTCOUNT {
		return false, false
	}

	return tcount > 0, true
}

// init allows tests to override the makemkvcon binary path.
func init() {
	if path := os.Getenv("TEST_MAKEMKV_PATH"); path != "" {
		makemkvPath = path
	}
}
