package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/8bitreid/simplerip/internal/disc"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	targets := []string{"/dev/sr0", "/dev/sr1"}
	fmt.Printf("Listening for udev events on %v... (Press Ctrl+C to stop)\n", targets)

	events := disc.ListenUdevEvents(ctx, targets)

	var lastEvent time.Time
	for ev := range events {
		now := time.Now()
		fmt.Printf("\n── Event Fired ────────────────────────────\n")
		fmt.Printf("Time:    %s\n", now.Format(time.RFC3339Nano))
		if !lastEvent.IsZero() {
			fmt.Printf("Gap:     %s since last event\n", now.Sub(lastEvent))
		}
		lastEvent = now
		fmt.Printf("Device:  %s\n", ev.Device)
		fmt.Printf("Present: %v\n", ev.Present)
	}

	fmt.Println("Listener closed. Exiting cleanly.")
}