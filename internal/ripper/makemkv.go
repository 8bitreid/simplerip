// Package ripper wraps makemkvcon for disc scanning and ripping.
package ripper

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/8bitreid/simplerip/internal/disc"
)

// makemkvcon -r info output uses a robot-mode line format:
//
//	CINFO:<id>,<code>,"<value>"                     — disc-level info
//	TCOUNT:<n>                                       — total title count
//	TINFO:<title>,<id>,<code>,"<val>"               — per-title info
//	SINFO:<title>,<stream>,<id>,<code>,"<val>"      — per-stream info
//	PRGV:<current>,<total>,<max>                    — progress (ignored here)
//	MSG:<code>,<flags>,<count>,"<msg>",...           — status/error messages
//
// Attribute IDs for TINFO:
//
//	2  = Name
//	9  = Chapter count
//	11 = Duration (H:MM:SS)
//	27 = Source file name
//	28 = Estimated size (bytes)
//
// Attribute IDs for CINFO:
//
//	30 = Disc name
//
// Attribute IDs for SINFO:
//
//	22 = Stream type text ("Video", "Audio", "Subtitles")

const (
	attrName       = 2
	attrChapters   = 9
	attrDuration   = 11
	attrSourceFile = 27
	attrSizeBytes  = 28
	attrDiscName   = 30
	attrStreamType = 22
)

// writeKey writes the MakeMKV licence key to ~/.MakeMKV/settings.conf,
// which is the only path makemkvcon reads for registration.
func writeKey(key string) error {
	if key == "" {
		return nil
	}
	dir := filepath.Join(os.Getenv("HOME"), ".MakeMKV")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	content := fmt.Sprintf("app_Key = %q\n", key)
	return os.WriteFile(filepath.Join(dir, "settings.conf"), []byte(content), 0o600)
}

// ScanInfo runs `makemkvcon -r info dev:<device>` and returns the parsed titles.
func ScanInfo(ctx context.Context, makemkvBin, device, key string) (*disc.ClassifiedDisc, error) {
	if err := writeKey(key); err != nil {
		return nil, fmt.Errorf("write makemkv key: %w", err)
	}
	cmd := exec.CommandContext(ctx, makemkvBin, "-r", "info", "dev:"+device)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start makemkvcon: %w", err)
	}

	result, parseErr := ScanInfoFromReader(stdout, device)

	if werr := cmd.Wait(); werr != nil {
		if ctx.Err() != nil {
			return result, fmt.Errorf("makemkvcon timed out: %w", ctx.Err())
		}
		// makemkvcon exits non-zero on warnings; return what we parsed.
		if parseErr == nil && len(result.Titles) > 0 {
			return result, nil
		}
		return result, fmt.Errorf("makemkvcon: %w", werr)
	}
	return result, parseErr
}

// ScanInfoFromReader parses a makemkvcon -r info output stream without
// launching a subprocess. device is used only as a label on the result
// and may be empty. Useful for replaying captured output or fixture files.
func ScanInfoFromReader(r io.Reader, device string) (*disc.ClassifiedDisc, error) {
	result, err := parseInfoOutput(r)
	result.Device = device
	return result, err
}

func parseInfoOutput(r io.Reader) (*disc.ClassifiedDisc, error) {
	result := &disc.ClassifiedDisc{}
	titleMap := map[int]*disc.MKVTitle{}
	audioCount := map[int]int{}

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "CINFO:"):
			parseDiscInfo(line[len("CINFO:"):], result)
		case strings.HasPrefix(line, "TINFO:"):
			parseTitleInfo(line[len("TINFO:"):], titleMap)
		case strings.HasPrefix(line, "SINFO:"):
			parseStreamInfo(line[len("SINFO:"):], audioCount)
		case strings.HasPrefix(line, "MSG:"):
			parseMsg(line[len("MSG:"):], result)
		case strings.HasPrefix(line, "TCOUNT:"), strings.HasPrefix(line, "PRGV:"):
			// TCOUNT is informational; PRGV during an info scan is unexpected but harmless.
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("reading makemkvcon output: %w", err)
	}

	// Copy audio counts into title structs before flattening.
	for idx, count := range audioCount {
		if t, ok := titleMap[idx]; ok {
			t.AudioTrackCount = count
		}
	}

	// Flatten titleMap into an ordered slice.
	maxIdx := -1
	for idx := range titleMap {
		if idx > maxIdx {
			maxIdx = idx
		}
	}
	for i := 0; i <= maxIdx; i++ {
		if t, ok := titleMap[i]; ok {
			result.Titles = append(result.Titles, *t)
		}
	}
	return result, nil
}

// parseDiscInfo handles one CINFO payload (everything after "CINFO:").
func parseDiscInfo(payload string, result *disc.ClassifiedDisc) {
	parts := splitRobotLine(payload)
	if len(parts) < 3 {
		return
	}
	attrID, err := strconv.Atoi(parts[0])
	if err != nil {
		return
	}
	if attrID == attrDiscName {
		result.DiscName = unquote(parts[2])
	}
}

// parseTitleInfo handles one TINFO payload (everything after "TINFO:").
func parseTitleInfo(payload string, titleMap map[int]*disc.MKVTitle) {
	parts := splitRobotLine(payload)
	if len(parts) < 4 {
		return
	}
	titleIdx, err := strconv.Atoi(parts[0])
	if err != nil {
		return
	}
	attrID, err := strconv.Atoi(parts[1])
	if err != nil {
		return
	}
	val := unquote(parts[3])

	t := titleMap[titleIdx]
	if t == nil {
		t = &disc.MKVTitle{Index: titleIdx}
		titleMap[titleIdx] = t
	}

	switch attrID {
	case attrName:
		t.Name = val
	case attrChapters:
		if n, err := strconv.Atoi(val); err == nil {
			t.ChapterCount = n
		}
	case attrDuration:
		if d, err := parseDuration(val); err == nil {
			t.Duration = d
		}
	case attrSourceFile:
		t.SourceFileName = val
	case attrSizeBytes:
		if b, err := strconv.ParseFloat(val, 64); err == nil {
			t.SizeGB = b / (1024 * 1024 * 1024)
		}
	}
}

// parseStreamInfo handles one SINFO payload (everything after "SINFO:").
// Format: <titleIdx>,<streamIdx>,<attrID>,<code>,"<value>"
// We only care about attrID 22 (stream type) to count audio tracks.
func parseStreamInfo(payload string, audioCount map[int]int) {
	parts := splitRobotLine(payload)
	if len(parts) < 5 {
		return
	}
	titleIdx, err := strconv.Atoi(parts[0])
	if err != nil {
		return
	}
	attrID, err := strconv.Atoi(parts[2])
	if err != nil {
		return
	}
	if attrID == attrStreamType && unquote(parts[4]) == "Audio" {
		audioCount[titleIdx]++
	}
}

// parseMsg handles one MSG payload (everything after "MSG:").
// Format: <code>,<flags>,<count>,"<message>"[,...]
// Any MSG with non-zero flags is collected as a warning.
func parseMsg(payload string, result *disc.ClassifiedDisc) {
	parts := splitRobotLine(payload)
	if len(parts) < 4 {
		return
	}
	flags, err := strconv.Atoi(parts[1])
	if err != nil {
		return
	}
	if flags != 0 {
		result.Warnings = append(result.Warnings, unquote(parts[3]))
	}
}

// splitRobotLine splits a robot-mode payload on commas, respecting quoted strings.
func splitRobotLine(s string) []string {
	var parts []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQuote = !inQuote
			cur.WriteByte(c)
		case c == ',' && !inQuote:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	parts = append(parts, cur.String())
	return parts
}

// unquote strips surrounding double-quotes if present.
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// ripProgress holds the three fields from a PRGV line emitted during a rip.
//
//	PRGV:<current>,<total>,<max>
//
// current tracks the current sub-operation; total tracks overall progress.
// Both are on the scale [0, max].
type ripProgress struct {
	current int
	total   int
	max     int
}

// percent returns the overall rip progress as an integer 0–100.
func (p ripProgress) percent() int {
	if p.max == 0 {
		return 0
	}
	return p.total * 100 / p.max
}

// parsePRGV parses a PRGV payload (everything after "PRGV:").
// Returns the progress and true on success, zero value and false otherwise.
func parsePRGV(payload string) (ripProgress, bool) {
	parts := strings.SplitN(payload, ",", 3)
	if len(parts) != 3 {
		return ripProgress{}, false
	}
	cur, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	tot, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	max, err3 := strconv.Atoi(strings.TrimSpace(parts[2]))
	if err1 != nil || err2 != nil || err3 != nil {
		return ripProgress{}, false
	}
	return ripProgress{current: cur, total: tot, max: max}, true
}

// parseDuration parses "H:MM:SS" as returned by makemkvcon.
func parseDuration(s string) (time.Duration, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, fmt.Errorf("unexpected duration format: %q", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, err
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, err
	}
	sec, err := strconv.Atoi(parts[2])
	if err != nil {
		return 0, err
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec)*time.Second, nil
}
