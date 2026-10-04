package notify

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Event identifies the kind of pipeline occurrence a Message describes.
type Event string

const (
	EventNeedsInput       Event = "needs_input"
	EventMultiTitle       Event = "multi_title"
	EventComplete         Event = "complete"
	EventFailed           Event = "failed"
	EventDurationMismatch Event = "duration_mismatch"
)

// Message is a channel-agnostic notification. Each Sender renders it in its
// own format, so adding ntfy or email only means implementing Sender.
type Message struct {
	Event   Event
	JobID   string
	Disc    string
	Device  string
	Title   string
	Summary string
	Details []string
	Link    string // filled in by the Dispatcher
}

// Sender delivers a Message to one channel (Discord, ntfy, email, ...).
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

const sendTimeout = 10 * time.Second

// Dispatcher filters events, stamps the UI link, and sends asynchronously.
// A slow or dead channel can never block or fail a rip: Notify returns
// immediately and failures are only logged.
type Dispatcher struct {
	senders []Sender
	enabled map[Event]bool
	uiURL   string
	wg      sync.WaitGroup
}

// NewDispatcher returns a Dispatcher. Events missing from enabled are off.
// A nil or empty senders slice makes Notify a no-op.
func NewDispatcher(senders []Sender, enabled map[Event]bool, uiURL string) *Dispatcher {
	return &Dispatcher{
		senders: senders,
		enabled: enabled,
		uiURL:   strings.TrimRight(uiURL, "/"),
	}
}

// Notify queues msg for delivery if its event is enabled. Safe on a nil Dispatcher.
func (d *Dispatcher) Notify(msg Message) {
	if d == nil || len(d.senders) == 0 || !d.enabled[msg.Event] {
		return
	}
	msg.Link = d.uiURL

	for _, s := range d.senders {
		d.wg.Add(1)
		go func(s Sender) {
			defer d.wg.Done()
			defer func() {
				if r := recover(); r != nil {
					slog.Warn("notification sender panicked", "event", msg.Event, "panic", r)
				}
			}()
			// Detached from the rip context so a failure notice still goes out
			// when the rip was cancelled or timed out.
			ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
			defer cancel()
			if err := s.Send(ctx, msg); err != nil {
				slog.Warn("notification failed", "event", msg.Event, "error", scrubError(err))
			}
		}(s)
	}
}

// Wait blocks until in-flight notifications finish or ctx is done.
func (d *Dispatcher) Wait(ctx context.Context) {
	if d == nil {
		return
	}
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// scrubError drops the request URL from transport errors: webhook URLs embed
// their secret token and must not reach logs.
func scrubError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return errors.New(ue.Op + ": " + ue.Err.Error())
	}
	return err
}
