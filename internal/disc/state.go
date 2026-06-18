// Package disc provides disc detection and per-drive state tracking.
package disc

import "sync"

// DiscEvent reports a change in disc presence on a device. It is emitted by the
// udev listener (see udev.go) and consumed by DriveStateTracker.
type DiscEvent struct {
	Device  string // e.g. /dev/sr0
	Present bool   // true = disc inserted, false = disc removed
}

// DriveState is the lifecycle stage of a single optical drive.
type DriveState string

const (
	StateEmpty         DriveState = "empty"
	StateMediaDetected DriveState = "media_detected"
	StateRipping       DriveState = "ripping"
	StateDone          DriveState = "done"
	StateError         DriveState = "error"
)

// DriveStateTracker holds the current DriveState for each device. It is
// thread-safe. The zero value is not usable; construct with NewDriveStateTracker.
//
// Its purpose is to ensure a disc that has already been ripped (or errored)
// does not re-trigger a rip while it's still sitting in the drive: only the
// empty -> media_detected transition signals a new rip.
type DriveStateTracker struct {
	mu     sync.Mutex
	states map[string]DriveState
}

// NewDriveStateTracker returns a ready-to-use tracker. Unseen devices default
// to StateEmpty.
func NewDriveStateTracker() *DriveStateTracker {
	return &DriveStateTracker{states: make(map[string]DriveState)}
}

// Get returns the current state for device, defaulting to StateEmpty if the
// device has not been seen.
func (t *DriveStateTracker) Get(device string) DriveState {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.states[device]; ok {
		return s
	}
	return StateEmpty
}

// Set records s as the current state for device.
func (t *DriveStateTracker) Set(device string, s DriveState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.states[device] = s
}

// OnDiscEvent advances the per-drive state machine for ev and reports whether a
// rip should be started in response.
//
// Rules:
//   - empty + Present=true                              -> media_detected, return true
//   - (media_detected|ripping|done|error) + Present=true -> no change, return false
//     (already-known disc, ignore duplicate event)
//   - ANY state + Present=false                         -> empty, return false
//     (removal always resets; if the previous state was ripping this is purely
//     informational — RipDisc's own ctx/error handling is the real abort path)
func (t *DriveStateTracker) OnDiscEvent(ev DiscEvent) (shouldStartRip bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !ev.Present {
		t.states[ev.Device] = StateEmpty
		return false
	}

	cur, ok := t.states[ev.Device]
	if !ok {
		cur = StateEmpty
	}
	if cur == StateEmpty {
		t.states[ev.Device] = StateMediaDetected
		return true
	}
	return false
}
