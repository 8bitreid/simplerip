package disc_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/8bitreid/simplerip/internal/disc"
)

// TestPollDetectsNewDisc verifies that a newly inserted disc fires an event.
func TestPollDetectsNewDisc(t *testing.T) {
	mockPath := createMockMakemkv(t, map[string][]int{
		"/dev/sr0": {0, 0, 1, 1, 1}, // No disc, then disc inserted
	})
	defer os.Remove(mockPath)

	disc.SetMakemkvPathForTest(mockPath)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch := disc.Poll(ctx, []string{"/dev/sr0"}, 100*time.Millisecond)

	// Should receive exactly one event for the inserted disc
	select {
	case device := <-ch:
		if device != "/dev/sr0" {
			t.Errorf("got device %q, want /dev/sr0", device)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for disc detection event")
	}

	// Should not receive duplicate events
	select {
	case device := <-ch:
		t.Errorf("unexpected duplicate event for device %q", device)
	case <-time.After(500 * time.Millisecond):
		// Expected: no duplicate events
	}
}

// TestPollDeduplication verifies that the same disc doesn't fire twice.
func TestPollDeduplication(t *testing.T) {
	mockPath := createMockMakemkv(t, map[string][]int{
		"/dev/sr0": {1, 1, 1, 1, 1}, // Disc always present
	})
	defer os.Remove(mockPath)

	disc.SetMakemkvPathForTest(mockPath)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	ch := disc.Poll(ctx, []string{"/dev/sr0"}, 100*time.Millisecond)

	// Should receive exactly one event (initial detection)
	events := 0
	timeout := time.After(800 * time.Millisecond)
	for {
		select {
		case <-ch:
			events++
		case <-timeout:
			goto done
		}
	}

done:
	if events != 1 {
		t.Errorf("got %d events, want 1 (no duplicates)", events)
	}
}

// TestPollDiscRemovalAndReinsertion verifies that a disc can be removed and reinserted.
func TestPollDiscRemovalAndReinsertion(t *testing.T) {
	mockPath := createMockMakemkv(t, map[string][]int{
		"/dev/sr0": {1, 1, 0, 0, 1, 1}, // Disc present, removed, reinserted
	})
	defer os.Remove(mockPath)

	disc.SetMakemkvPathForTest(mockPath)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch := disc.Poll(ctx, []string{"/dev/sr0"}, 100*time.Millisecond)

	// First detection
	select {
	case device := <-ch:
		if device != "/dev/sr0" {
			t.Errorf("first event: got device %q, want /dev/sr0", device)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for first disc detection")
	}

	// Second detection after removal and reinsertion
	select {
	case device := <-ch:
		if device != "/dev/sr0" {
			t.Errorf("second event: got device %q, want /dev/sr0", device)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for second disc detection after reinsertion")
	}
}

// TestPollContextCancellation verifies that cancelling the context stops the poll loop.
func TestPollContextCancellation(t *testing.T) {
	mockPath := createMockMakemkv(t, map[string][]int{
		"/dev/sr0": {0, 0, 0}, // No disc ever
	})
	defer os.Remove(mockPath)

	disc.SetMakemkvPathForTest(mockPath)

	ctx, cancel := context.WithCancel(context.Background())
	ch := disc.Poll(ctx, []string{"/dev/sr0"}, 100*time.Millisecond)

	// Wait a bit to ensure polling has started
	time.Sleep(150 * time.Millisecond)

	// Cancel context
	cancel()

	// Channel should close within a reasonable time
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel should be closed after context cancellation")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("channel did not close after context cancellation")
	}
}

// TestPollMultipleDevices verifies that multiple devices can be monitored.
func TestPollMultipleDevices(t *testing.T) {
	mockPath := createMockMakemkv(t, map[string][]int{
		"/dev/sr0": {0, 1, 1, 1}, // Disc inserted on sr0
		"/dev/sr1": {0, 0, 1, 1}, // Disc inserted on sr1 later
	})
	defer os.Remove(mockPath)

	disc.SetMakemkvPathForTest(mockPath)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch := disc.Poll(ctx, []string{"/dev/sr0", "/dev/sr1"}, 100*time.Millisecond)

	events := make(map[string]int)
	timeout := time.After(1 * time.Second)
	for {
		select {
		case device := <-ch:
			events[device]++
		case <-timeout:
			goto done
		}
	}

done:
	if events["/dev/sr0"] != 1 {
		t.Errorf("sr0: got %d events, want 1", events["/dev/sr0"])
	}
	if events["/dev/sr1"] != 1 {
		t.Errorf("sr1: got %d events, want 1", events["/dev/sr1"])
	}
}

func TestPollTreatsParsedTCOUNTAsSuccessEvenOnExitError(t *testing.T) {
	tmpDir := t.TempDir()
	mockPath := filepath.Join(tmpDir, "mock-makemkvcon")
	// checkDevice runs: makemkvcon -r --cache=1 info dev:/dev/sr0
	// so the device argument is $4. A case pattern cannot contain unquoted
	// spaces, so match on the device arg alone rather than the whole arg list.
	script := `#!/bin/sh
case "$4" in
  dev:/dev/sr0)
    echo 'TCOUNT:1'
    exit 1
    ;;
esac
echo 'TCOUNT:0'
exit 0
`
	if err := os.WriteFile(mockPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	disc.SetMakemkvPathForTest(mockPath)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	ch := disc.Poll(ctx, []string{"/dev/sr0"}, 100*time.Millisecond)

	select {
	case device := <-ch:
		if device != "/dev/sr0" {
			t.Fatalf("got device %q, want /dev/sr0", device)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("timeout waiting for disc detection event")
	}
}

// TestPollEventsReportsRemoval verifies that PollEvents emits a Present=false
// event when a disc is removed, after the initial Present=true insertion.
func TestPollEventsReportsRemoval(t *testing.T) {
	mockPath := createMockMakemkv(t, map[string][]int{
		"/dev/sr0": {1, 1, 0, 0}, // disc present, then removed
	})
	defer os.Remove(mockPath)

	disc.SetMakemkvPathForTest(mockPath)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch := disc.PollEvents(ctx, []string{"/dev/sr0"}, 100*time.Millisecond)

	// First: insertion.
	select {
	case ev := <-ch:
		if ev.Device != "/dev/sr0" || !ev.Present {
			t.Fatalf("first event = %+v, want {/dev/sr0 true}", ev)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for insertion event")
	}

	// Then: removal.
	select {
	case ev := <-ch:
		if ev.Device != "/dev/sr0" || ev.Present {
			t.Fatalf("second event = %+v, want {/dev/sr0 false}", ev)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for removal event")
	}
}

func TestPollEventsWithBusySkipsProbeWhileActive(t *testing.T) {
	tmpDir := t.TempDir()
	mockPath := filepath.Join(tmpDir, "mock-makemkvcon")
	stateFile := filepath.Join(tmpDir, "calls.txt")

	script := fmt.Sprintf(`#!/bin/sh
COUNT=0
if [ -f %q ]; then
  COUNT=$(cat %q)
fi
COUNT=$((COUNT + 1))
echo "$COUNT" > %q
echo 'TCOUNT:1'
exit 0
`, stateFile, stateFile, stateFile)
	if err := os.WriteFile(mockPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	disc.SetMakemkvPathForTest(mockPath)

	var busy int32
	isBusy := func(string) bool {
		return atomic.LoadInt32(&busy) == 1
	}

	ctx, cancel := context.WithCancel(context.Background())

	ch := disc.PollEventsWithBusy(ctx, []string{"/dev/sr0"}, 100*time.Millisecond, isBusy)
	defer func() {
		cancel()
		for range ch {
		}
	}()

	// Initial probe should happen and emit insertion.
	select {
	case ev := <-ch:
		if ev.Device != "/dev/sr0" || !ev.Present {
			t.Fatalf("first event = %+v, want {/dev/sr0 true}", ev)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for initial insertion event")
	}

	atomic.StoreInt32(&busy, 1)
	time.Sleep(300 * time.Millisecond)

	callsDuringBusy := readCallCount(t, stateFile)
	time.Sleep(300 * time.Millisecond)
	if got := readCallCount(t, stateFile); got != callsDuringBusy {
		t.Fatalf("probe count changed while busy: start=%d end=%d", callsDuringBusy, got)
	}

	// Clear busy flag and verify polling resumes.
	atomic.StoreInt32(&busy, 0)
	time.Sleep(300 * time.Millisecond)
	if got := readCallCount(t, stateFile); got <= callsDuringBusy {
		t.Fatalf("expected probes to resume after idle; busyCount=%d resumedCount=%d", callsDuringBusy, got)
	}
}

func readCallCount(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read call count: %v", err)
	}
	s := string(data)
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		t.Fatalf("parse call count %q: %v", s, err)
	}
	return n
}

// createMockMakemkv creates a mock makemkvcon script that returns different
// TCOUNT values on successive invocations for each device.
// tcounts maps device paths to a sequence of title counts.
func createMockMakemkv(t *testing.T, tcounts map[string][]int) string {
	t.Helper()

	tmpDir := t.TempDir()
	mockPath := filepath.Join(tmpDir, "mock-makemkvcon")
	stateDir := filepath.Join(tmpDir, "state")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Generate the mock script content
	script := "#!/bin/sh\n"
	script += "set -e\n"
	script += fmt.Sprintf("STATE_DIR=%q\n", stateDir)
	script += `
# Parse device from args
DEVICE=""
for arg in "$@"; do
    case "$arg" in
        dev:*)
            DEVICE="${arg#dev:}"
            break
            ;;
    esac
done

if [ -z "$DEVICE" ]; then
    echo "MSG:3321,0,0,\"No device specified\"" >&2
    exit 1
fi

# Get or initialize call count for this device
COUNT_FILE="$STATE_DIR/$(echo "$DEVICE" | sed 's|/|_|g').count"
if [ ! -f "$COUNT_FILE" ]; then
    echo "0" > "$COUNT_FILE"
fi
CALL_COUNT=$(cat "$COUNT_FILE")
echo $((CALL_COUNT + 1)) > "$COUNT_FILE"

# Return TCOUNT based on device and call count
`

	// Add device-specific TCOUNT responses
	for device, counts := range tcounts {
		safeName := filepath.Base(device)
		script += fmt.Sprintf("\nif [ \"$DEVICE\" = %q ]; then\n", device)
		script += "    case $CALL_COUNT in\n"
		for i, count := range counts {
			script += fmt.Sprintf("        %d) echo \"TCOUNT:%d\" ;;\n", i, count)
		}
		// Default to last value for any additional calls
		lastCount := 0
		if len(counts) > 0 {
			lastCount = counts[len(counts)-1]
		}
		script += fmt.Sprintf("        *) echo \"TCOUNT:%d\" ;;\n", lastCount)
		script += "    esac\n"
		script += "    exit 0\n"
		script += "fi\n"
		_ = safeName // suppress unused warning
	}

	script += `
# Unknown device
echo "TCOUNT:0"
exit 0
`

	if err := os.WriteFile(mockPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	return mockPath
}
