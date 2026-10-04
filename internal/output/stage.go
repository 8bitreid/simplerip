// Package output handles moving ripped MKV files to their final destination
// and writing the rip.json audit log.
package output

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// RipLog is the data written to rip.json in the destination directory.
type RipLog struct {
	Title      string    `json:"title"`
	DiscName   string    `json:"disc_name"`
	RippedAt   time.Time `json:"ripped_at"`
	Files      []string  `json:"files"`
	StagingDir string    `json:"staging_dir"`
	DestDir    string    `json:"dest_dir"`
}

// DeliverResult is returned by Deliver after a successful rsync.
type DeliverResult struct {
	DestDir string
	Files   []string // absolute paths at destination
}

// Deliver rsyncs srcFiles from stagingDir into destDir/subdir, verifies the
// files arrived, then writes rip.json. Returns the destination paths.
//
// srcFiles must be absolute paths inside stagingDir.
// subdir is the subdirectory name to create inside destDir (e.g. "Oppenheimer (2023)").
// title and discName are stored in the log only.
func Deliver(
	ctx context.Context,
	srcFiles []string,
	stagingDir, destDir, subdir string,
	title, discName string,
) (*DeliverResult, error) {
	target := filepath.Join(destDir, sanitizeFileName(subdir))
	if err := os.MkdirAll(target, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %q: %w", target, err)
	}

	if err := rsync(ctx, srcFiles, target); err != nil {
		return nil, err
	}

	// Verify every source file arrived at the destination.
	var destFiles []string
	for _, src := range srcFiles {
		dst := filepath.Join(target, filepath.Base(src))
		if err := verifyFile(src, dst); err != nil {
			return nil, fmt.Errorf("verify %q: %w", filepath.Base(src), err)
		}
		destFiles = append(destFiles, dst)
	}

	log := RipLog{
		Title:      title,
		DiscName:   discName,
		RippedAt:   time.Now().UTC(),
		Files:      destFiles,
		StagingDir: stagingDir,
		DestDir:    target,
	}
	if err := writeLog(target, log); err != nil {
		return nil, err
	}

	return &DeliverResult{DestDir: target, Files: destFiles}, nil
}

// RenameForDelivery renames ripped files in place (within their staging
// directory) to the Jellyfin-style base name: "<name>.mkv" for a single file,
// or "<name> - part1.mkv", "<name> - part2.mkv" ... when there are several.
// It returns the new paths in the same order. If any rename fails, files already
// renamed are restored and an error is returned.
func RenameForDelivery(files []string, name string) ([]string, error) {
	return RenameForDeliveryPattern(files, name, false)
}

// RenameForDeliveryPattern renames ripped files in place. If isTV is true,
// multi-file episodes are formatted as "<name> - E01.mkv", "<name> - E02.mkv"
// so media servers (Jellyfin/Plex) index them as distinct episodes rather than
// stacking them as parts of a single movie.
func RenameForDeliveryPattern(files []string, name string, isTV bool) ([]string, error) {
	name = sanitizeFileName(name)
	if name == "" || len(files) == 0 {
		return files, nil
	}
	out := make([]string, 0, len(files))
	for i, src := range files {
		base := name
		if len(files) > 1 {
			if isTV {
				base = fmt.Sprintf("%s - E%02d", name, i+1)
			} else {
				base = fmt.Sprintf("%s - part%d", name, i+1)
			}
		}
		dst := filepath.Join(filepath.Dir(src), base+filepath.Ext(src))
		if dst != src {
			if _, err := os.Stat(dst); err == nil {
				err = fmt.Errorf("destination %q already exists", dst)
				rollbackRenames(files, out)
				return nil, err
			}
			if err := os.Rename(src, dst); err != nil {
				rollbackRenames(files, out)
				return nil, fmt.Errorf("rename %q: %w", filepath.Base(src), err)
			}
		}
		out = append(out, dst)
	}
	return out, nil
}

func rollbackRenames(orig, renamed []string) {
	for i := range renamed {
		_ = os.Rename(renamed[i], orig[i])
	}
}

// sanitizeFileName strips characters Jellyfin reserves (< > : " / \ | ? *), so
// the folder and file names always match and never trip up SMB shares.
// A colon becomes " -" ("Star Wars: A New Hope" -> "Star Wars - A New Hope").
func sanitizeFileName(s string) string {
	r := strings.NewReplacer(
		"/", "-", "\\", "-", ":", " -",
		"?", "", "*", "", "\"", "", "<", "", ">", "", "|", "", "\x00", "",
	)
	return strings.Join(strings.Fields(r.Replace(s)), " ")
}

// rsync calls the system rsync to copy srcFiles into destDir.
// Uses --checksum so correctness doesn't depend on timestamps.
func rsync(ctx context.Context, srcFiles []string, destDir string) error {
	args := []string{
		"--archive",
		"--checksum",
		"--no-inc-recursive",
	}
	args = append(args, srcFiles...)
	args = append(args, destDir+"/")

	cmd := exec.CommandContext(ctx, "rsync", args...)
	cmd.Stdout = os.Stderr // rsync progress/stats → stderr so stdout stays clean
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("rsync timed out: %w", ctx.Err())
		}
		return fmt.Errorf("rsync: %w", err)
	}
	return nil
}

// verifyFile checks that dst exists and matches src in size.
// A full checksum is too slow for large MKVs; size parity catches truncation.
func verifyFile(src, dst string) error {
	si, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat src: %w", err)
	}
	di, err := os.Stat(dst)
	if err != nil {
		return fmt.Errorf("stat dst: %w", err)
	}
	if si.Size() != di.Size() {
		return fmt.Errorf("size mismatch: src=%d dst=%d", si.Size(), di.Size())
	}
	return nil
}

// writeLog writes rip.json into dir.
func writeLog(dir string, log RipLog) error {
	path := filepath.Join(dir, "rip.json")
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create rip.json: %w", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(log); err != nil {
		return fmt.Errorf("write rip.json: %w", err)
	}
	return nil
}
