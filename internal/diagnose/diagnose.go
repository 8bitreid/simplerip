// Package diagnose turns low-level rip/scan/delivery errors into a short,
// human-readable summary plus a suggestion for what to try next. The result is
// stored with the failed job so the UI can show it later.
package diagnose

import (
	"errors"
	"fmt"
	"strings"

	"github.com/8bitreid/simplerip/internal/ripper"
)

// Diagnosis explains a failure to the user.
type Diagnosis struct {
	Code    string `json:"code"`    // stable identifier, e.g. "disc_read_error"
	Summary string `json:"summary"` // what went wrong
	Hint    string `json:"hint"`    // what to try
	Detail  string `json:"detail"`  // raw technical error
}

// Data returns the diagnosis as job-event data.
func (d Diagnosis) Data() map[string]any {
	return map[string]any{"code": d.Code, "summary": d.Summary, "hint": d.Hint, "error": d.Detail}
}

const discCareHint = "Clean the disc with a soft cloth, wiping from the centre outward, and retry. " +
	"If it still fails, try the other drive (drives differ in how well they read damaged discs), " +
	"or make an image of the disc with ddrescue and rip from that."

// Rip classifies an error returned while ripping a title.
func Rip(err error) Diagnosis {
	d := Diagnosis{Detail: err.Error()}
	var re *ripper.ReadError
	switch {
	case errors.As(err, &re):
		where := ""
		if re.Offset >= 0 {
			where = fmt.Sprintf(" at %s", fmtBytes(re.Offset))
			if re.File != "" {
				where += " into " + re.File
			}
		}
		switch {
		case errors.Is(err, ripper.ErrRipNoProgress):
			d.Code = "disc_stalled"
			d.Summary = "Rip stalled on unreadable disc area" + where
		case errors.Is(err, ripper.ErrRipReadErrorLimit):
			d.Code = "disc_read_errors"
			d.Summary = fmt.Sprintf("Too many disc read errors (%d)%s", re.Count, where)
		default:
			d.Code = "disc_read_error"
			d.Summary = "Disc read error" + where + " — the disc may be scratched or dirty"
		}
		d.Hint = discCareHint
	case errors.Is(err, ripper.ErrRipTimeout):
		d.Code = "rip_timeout"
		d.Summary = "Rip timed out"
		d.Hint = "The drive may have hung. Eject and reinsert the disc, or raise makemkv.timeout_minutes in the config if the disc is simply slow."
	default:
		d.Code = "rip_failed"
		d.Summary = "Rip failed: " + err.Error()
		d.Hint = "Check the container logs for makemkvcon output. Reinserting the disc or trying the other drive often helps."
	}
	return d
}

// NoFiles is used when makemkvcon succeeded but produced nothing.
func NoFiles() Diagnosis {
	return Diagnosis{
		Code:    "no_files",
		Summary: "No files were produced from the disc",
		Hint:    "The selected title may be unreadable or protected. Check the logs. " + discCareHint,
		Detail:  "no files ripped",
	}
}

// Scan classifies an error returned while scanning a disc.
func Scan(err error) Diagnosis {
	msg := strings.ToLower(err.Error())
	d := Diagnosis{Detail: err.Error()}
	if strings.Contains(msg, "copy protection key exchange failure") ||
		strings.Contains(msg, "key not present") ||
		strings.Contains(msg, "rpc protection") {
		d.Code = "drive_rpc"
		d.Summary = "Drive region/RPC protection blocked disc authentication"
		d.Hint = "Set the drive region or update the drive firmware (or use a LibreDrive-capable drive), then retry."
		return d
	}
	d.Code = "scan_failed"
	d.Summary = "Scan failed: " + err.Error()
	d.Hint = "Make sure the disc is clean and fully loaded, then eject and reinsert it. Try the other drive if it keeps failing."
	return d
}

// Delivery classifies an error returned while copying to the NAS.
func Delivery(err error) Diagnosis {
	return Diagnosis{
		Code:    "delivery_failed",
		Summary: "Delivery to the NAS failed: " + err.Error(),
		Hint:    "Check that the NAS is reachable and the output share is mounted and has free space. The ripped file is still in the staging folder.",
		Detail:  err.Error(),
	}
}

func fmtBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%d MB", n>>20)
	default:
		return fmt.Sprintf("%d KB", n>>10)
	}
}
