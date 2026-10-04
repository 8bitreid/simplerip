package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/8bitreid/simplerip/internal/disc"
	"github.com/8bitreid/simplerip/internal/notify"
	"github.com/8bitreid/simplerip/internal/output"
	"github.com/8bitreid/simplerip/internal/ripper"
)

var (
	ErrNoAlternate    = errors.New("no such alternate cut for this job")
	ErrDeviceBusy     = errors.New("drive is busy")
	ErrNothingStaged  = errors.New("no output configured")
	alternateMinRatio = 0.75
	alternateMaxRatio = 1.5
)

// altState is what the service remembers about a finished or running job so
// an alternate cut can be ripped later, while the disc is still in the drive.
type altState struct {
	device string
	disc   *disc.ClassifiedDisc
	title  string // final delivered title, e.g. "Ella Enchanted (2004)"
	cands  []disc.MKVTitle
}

// findAlternates returns feature-length titles that look like another cut of
// the same movie: clearly longer or shorter than main (so not the same cut,
// which scoring already arbitrates) but within a plausible range of it.
func findAlternates(all []disc.MKVTitle, main disc.MKVTitle, minFeature time.Duration) []disc.MKVTitle {
	var out []disc.MKVTitle
	for _, t := range all {
		if t.Index == main.Index || t.Duration < minFeature {
			continue
		}
		d := t.Duration - main.Duration
		if d < 0 {
			d = -d
		}
		ratio := float64(t.Duration) / float64(main.Duration)
		if d <= durationTolerance || ratio < alternateMinRatio || ratio > alternateMaxRatio {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Duration < out[j].Duration })
	return out
}

func alternateLabel(t disc.MKVTitle) string {
	return fmt.Sprintf("Alternate (%dmin)", int(t.Duration.Minutes()+0.5))
}

// registerAlternates remembers the candidates for jobID and tells the user
// about them. The main rip continues; nothing is ripped without a request.
func (s *RipService) registerAlternates(ctx context.Context, run *ripRun, device string, scanned *disc.ClassifiedDisc, main disc.MKVTitle, cands []disc.MKVTitle) {
	s.ripMu.Lock()
	s.alternates[run.jobID] = &altState{device: device, disc: scanned, cands: cands}
	s.ripMu.Unlock()

	var details []string
	data := make([]map[string]any, 0, len(cands))
	for _, t := range cands {
		sc := ripper.ScoreTitle(t)
		details = append(details, fmt.Sprintf("title %d: %d min, %s", t.Index, int(t.Duration.Minutes()), sc.Label()))
		data = append(data, map[string]any{
			"index": t.Index, "minutes": int(t.Duration.Minutes()), "score": sc.Total, "label": sc.Label(),
		})
	}
	if s.store != nil {
		_ = s.store.AddEvent(ctx, run.jobID, "alternates",
			fmt.Sprintf("found %d alternate cut(s) besides title %d (%d min); rip one from the job page if wanted", len(cands), main.Index, int(main.Duration.Minutes())),
			map[string]any{"main_index": main.Index, "candidates": data})
	}
	s.notifier.Notify(notify.Message{
		Event:   notify.EventNeedsInput,
		JobID:   run.jobID,
		Disc:    scanned.DiscName,
		Device:  device,
		Title:   run.title,
		Summary: fmt.Sprintf("Alternate cuts found besides the main title (%d min). Ripping continues with the main title; open the job in the UI to rip an alternate.", int(main.Duration.Minutes())),
		Details: details,
	})
}

func (s *RipService) setAlternatesTitle(jobID, title string) {
	s.ripMu.Lock()
	defer s.ripMu.Unlock()
	if a, ok := s.alternates[jobID]; ok {
		a.title = title
	}
}

// StartAlternateRip rips one alternate cut of a finished job in the
// background and delivers it beside the main file as
// "<Title> - Alternate (Nmin).mkv". It returns immediately.
func (s *RipService) StartAlternateRip(jobID string, index int) error {
	s.ripMu.Lock()
	a, ok := s.alternates[jobID]
	if !ok {
		s.ripMu.Unlock()
		return ErrNoAlternate
	}
	var cand *disc.MKVTitle
	for i := range a.cands {
		if a.cands[i].Index == index {
			cand = &a.cands[i]
		}
	}
	if cand == nil {
		s.ripMu.Unlock()
		return ErrNoAlternate
	}
	if _, ripping := s.ripTitles[a.device]; ripping || s.altBusy[a.device] {
		s.ripMu.Unlock()
		return ErrDeviceBusy
	}
	if a.title == "" || s.cfg.Output.NASPath == "" {
		s.ripMu.Unlock()
		return ErrNothingStaged
	}
	s.altBusy[a.device] = true
	s.ripMu.Unlock()

	go func() {
		defer func() {
			s.ripMu.Lock()
			delete(s.altBusy, a.device)
			s.ripMu.Unlock()
		}()
		timeout := time.Duration(s.cfg.MakeMKV.TimeoutMinutes) * time.Minute
		if timeout <= 0 {
			timeout = 120 * time.Minute
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := s.ripAlternate(ctx, jobID, a, *cand); err != nil {
			slog.Error("alternate rip failed", "job", jobID, "title", cand.Index, "error", err)
		}
	}()
	return nil
}

func (s *RipService) ripAlternate(ctx context.Context, jobID string, a *altState, t disc.MKVTitle) error {
	fail := func(err error) error {
		if s.store != nil {
			_ = s.store.AddEvent(ctx, jobID, "error", fmt.Sprintf("alternate title %d failed: %v", t.Index, err), nil)
		}
		s.notifier.Notify(notify.Message{
			Event: notify.EventFailed, JobID: jobID, Disc: a.disc.DiscName, Device: a.device,
			Title: a.title, Summary: fmt.Sprintf("Alternate cut (title %d) failed: %v", t.Index, err),
		})
		return err
	}

	stagingDir := s.cfg.Output.StagingDir
	if stagingDir == "" {
		stagingDir = "/tmp/simplerip-staging"
	}
	outDir := filepath.Join(stagingDir, fmt.Sprintf("rip-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fail(err)
	}
	defer func() {
		if filepath.Base(outDir) != "" && strings.HasPrefix(filepath.Base(outDir), "rip-") {
			_ = os.RemoveAll(outDir)
		}
	}()

	name := a.title + " - " + alternateLabel(t)
	if s.store != nil {
		_ = s.store.AddEvent(ctx, jobID, "rip", fmt.Sprintf("ripping alternate title %d (%d min)", t.Index, int(t.Duration.Minutes())), nil)
	}
	s.emit(ProgressEvent{Device: a.device, Stage: "ripping", Title: name, Message: "Ripping " + name})

	files, err := ripper.RipTitle(ctx, a.device, t, outDir, s.cfg.MakeMKV.Key, s.cfg.MakeMKV.TimeoutMinutes,
		cacheMBForDisc(s.cfg.MakeMKV.CacheMB, a.disc.Type), s.cfg.MakeMKV.ReadErrorLimit, s.cfg.MakeMKV.NoProgressMin,
		func(_ int, pct int, _ ripper.RipPhase) {
			s.emit(ProgressEvent{Device: a.device, Stage: "ripping", Title: name, Percent: pct, Message: fmt.Sprintf("Ripping %s (%d%%)", name, pct)})
		})
	if err != nil {
		return fail(err)
	}
	if len(files) == 0 {
		return fail(errors.New("no file produced"))
	}
	if renamed, rerr := output.RenameForDelivery(files, name); rerr == nil {
		files = renamed
	}
	res, err := output.Deliver(ctx, files, outDir, s.cfg.Output.NASPath, a.title, name, a.disc.DiscName)
	if err != nil {
		return fail(err)
	}
	if s.store != nil {
		_ = s.store.AddEvent(ctx, jobID, "deliver", fmt.Sprintf("alternate delivered: %s", filepath.Base(res.Files[0])),
			map[string]any{"nas_path": res.DestDir, "index": t.Index})
	}
	s.emit(ProgressEvent{Device: a.device, Stage: "done", Title: name, Percent: 100, Message: "Delivered " + name})
	s.notifier.Notify(notify.Message{
		Event: notify.EventComplete, JobID: jobID, Disc: a.disc.DiscName, Device: a.device, Title: name,
		Summary: "Alternate cut delivered.", Details: res.Files,
	})
	return nil
}
