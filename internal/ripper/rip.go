package ripper

import (
	"bufio"
	"context"
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

// RipTitle runs:
//
//	makemkvcon mkv --noscan -r --messages=-stdout --progress=-stdout
//	             dev:<device> <title.Index> <outputDir>
//
// key is exported to the subprocess as MAKEMKV_KEY so the licence is available
// without being visible in the process list.
//
// Progress lines are logged to stdout as "title <index>: <pct>%".
// On success the paths of *.mkv files written to outputDir are returned.
// On deadline-exceeded ErrRipTimeout is returned (wrapping the error so
// errors.Is works).
func RipTitle(ctx context.Context, device string, title disc.MKVTitle, outputDir string, key string, timeoutMinutes int) ([]string, error) {
	timeout := time.Duration(timeoutMinutes) * time.Minute
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()

	if err := writeKey(key); err != nil {
		return nil, fmt.Errorf("write makemkv key: %w", err)
	}
	cmd := exec.CommandContext(ctx,
		"makemkvcon", "mkv",
		"--noscan", "-r",
		"--messages=-stdout",
		"--progress=-stdout",
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
loop:
	for {
		select {
		case line, ok := <-lineCh:
			if !ok {
				break loop
			}
			if !strings.HasPrefix(line, "PRGV:") {
				continue
			}
			prog, ok := parsePRGV(line[len("PRGV:"):])
			if !ok {
				continue
			}
			if pct := prog.percent(); pct != lastPct {
				lastPct = pct
				fmt.Printf("title %d: %d%%\n", title.Index, pct)
			}
		case <-ctx.Done():
			break loop
		}
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

	return newMKVFiles(outputDir, start)
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
