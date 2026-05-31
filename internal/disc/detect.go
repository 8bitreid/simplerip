// Package disc provides disc detection and polling functionality.
package disc

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Poll continuously monitors optical devices for disc insertion.
// It checks each device at the given interval by running makemkvcon.
// When a disc is newly inserted, the device path is sent on the returned channel.
// The same disc will not fire twice until it's been removed and reinserted.
// Context cancellation stops the poll loop cleanly and closes the channel.
// The per-device check timeout equals the poll interval, keeping total check
// time bounded to interval × device count.
func Poll(ctx context.Context, devices []string, interval time.Duration) <-chan string {
	ch := make(chan string)
	go func() {
		defer close(ch)
		state := make(map[string]bool) // device -> disc present
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// Initial check before first tick
		checkDevices(ctx, devices, state, ch, interval)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				checkDevices(ctx, devices, state, ch, interval)
			}
		}
	}()
	return ch
}

// checkDevices polls each device and sends events for newly inserted discs.
func checkDevices(ctx context.Context, devices []string, state map[string]bool, ch chan<- string, timeout time.Duration) {
	for _, device := range devices {
		hasDisc, ok := checkDevice(ctx, device, timeout)
		
		// If check failed (drive busy, timeout, error), don't update state.
		// This prevents false "disc removed" events during active rips.
		if !ok {
			continue
		}
		
		wasPresent := state[device]

		if hasDisc && !wasPresent {
			// Disc newly inserted
			select {
			case ch <- device:
			case <-ctx.Done():
				return
			}
		}

		state[device] = hasDisc
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

	// Wait for command to finish
	if err := cmd.Wait(); err != nil {
		// Command failed - might be drive busy, timeout, or error
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
