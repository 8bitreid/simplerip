package disc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"time"
)

// probeSize is the number of bytes read from the raw block device when probing
// for disc type markers. 4 MB covers the ISO 9660 Primary Volume Descriptor
// (sector 16, byte offset 32 KB) and the UDF partition start (sector 256,
// byte offset 128 KB) with enough headroom for the root directory blocks that
// hold the BDMV / VIDEO_TS entries.
const probeSize = 4 * 1024 * 1024

// ProbeDiscType identifies the disc type by scanning the raw block device for
// well-known root-directory markers:
//
//   - "BDMV"     → DiscTypeBluRay  (mandatory root dir on every Blu-ray disc)
//   - "VIDEO_TS" → DiscTypeDVD     (mandatory root dir on every DVD-Video disc)
//
// It is intended as a fallback when makemkvcon does not emit CINFO:1 — e.g. on
// a damaged or heavily encrypted disc where the process exits before writing
// disc-level CINFO lines. A hard 10-second deadline is enforced so a stuck
// drive can never block the caller. Returns DiscTypeUnknown on any error or
// when neither marker is found.
func ProbeDiscType(ctx context.Context, device string) DiscType {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	type result struct {
		buf []byte
		err error
	}
	ch := make(chan result, 1)

	go func() {
		f, err := os.Open(device)
		if err != nil {
			ch <- result{err: err}
			return
		}
		defer f.Close()

		buf := make([]byte, probeSize)
		n, err := io.ReadAtLeast(f, buf, 1)
		if err != nil && n == 0 {
			ch <- result{err: err}
			return
		}
		ch <- result{buf: buf[:n]}
	}()

	select {
	case <-ctx.Done():
		fmt.Fprintf(os.Stderr, "probe: disc type probe timed out for %s\n", device)
		return DiscTypeUnknown
	case r := <-ch:
		if r.err != nil {
			fmt.Fprintf(os.Stderr, "probe: disc type probe failed for %s: %v\n", device, r.err)
			return DiscTypeUnknown
		}
		switch {
		case bytes.Contains(r.buf, []byte("BDMV")):
			return DiscTypeBluRay
		case bytes.Contains(r.buf, []byte("VIDEO_TS")):
			return DiscTypeDVD
		default:
			return DiscTypeUnknown
		}
	}
}
