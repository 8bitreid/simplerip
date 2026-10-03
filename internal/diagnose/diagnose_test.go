package diagnose

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/8bitreid/simplerip/internal/ripper"
)

func TestRipReadError(t *testing.T) {
	err := fmt.Errorf("rip title 0: %w", &ripper.ReadError{
		Cause: ripper.ErrRipSaveFailed, File: "/VIDEO_TS/VTS_01_1.VOB", Offset: 601227264, Count: 20,
	})
	d := Rip(err)
	if d.Code != "disc_read_error" || !strings.Contains(d.Summary, "573 MB") || d.Hint == "" {
		t.Fatalf("unexpected diagnosis: %+v", d)
	}
}

func TestRipClassification(t *testing.T) {
	cases := map[string]error{
		"disc_read_errors": &ripper.ReadError{Cause: ripper.ErrRipReadErrorLimit, Offset: -1, Count: 5},
		"disc_stalled":     &ripper.ReadError{Cause: ripper.ErrRipNoProgress, Offset: -1},
		"rip_timeout":      fmt.Errorf("x: %w", ripper.ErrRipTimeout),
		"rip_failed":       errors.New("boom"),
	}
	for want, err := range cases {
		if got := Rip(err).Code; got != want {
			t.Errorf("%v: code = %q, want %q", err, got, want)
		}
	}
}

func TestNoFilesAndScan(t *testing.T) {
	if d := NoFiles(); d.Code != "no_files" || d.Hint == "" {
		t.Fatalf("bad NoFiles: %+v", d)
	}
	if d := Scan(errors.New("RPC protection")); d.Code != "drive_rpc" {
		t.Fatalf("bad Scan: %+v", d)
	}
}
