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
//	8  = Chapter count
//	9  = Duration (H:MM:SS)
//	11 = Estimated size (bytes)
//	27 = Source file name
//	30 = Title description (contains angle info)
//
// Attribute IDs for CINFO:
//
//	30 = Disc name
//
// Attribute IDs for SINFO:
//
//	1  = Stream type text ("Video", "Audio", "Subtitles")
//	5  = Short codec tag ("A_TRUEHD", "A_DTS", etc.)
//	6  = Long codec name ("DTS-HD Master Audio", etc.)
//	19 = Audio layout ("7.1", "5.1", "stereo")
//	28 = ISO language code ("eng", "fra", etc.)

const (
	tinfoName       = 2
	tinfoChapters   = 8
	tinfoDuration   = 9
	tinfoSizeBytes  = 11
	tinfoSourceFile = 27
	tinfoDesc       = 30

	cinfoDiscType = 1
	cinfoDiscName = 30

	sinfoStreamType  = 1
	sinfoCodecName   = 5
	sinfoCodecLong   = 6
	sinfoAudioLayout = 19
	sinfoLanguage    = 28
)

// writeTunedConfig writes a performance-tuned ~/.MakeMKV/settings.conf.
// It injects the licence key alongside hardware-optimisation parameters suited
// for high-bitrate 4K UHD and Blu-ray ripping. If the file already contains
// the exact key it is left untouched so that fields written by `makemkvcon reg`
// (sdf_Stop, etc.) are preserved.
func writeTunedConfig(key string) error {
	if key == "" {
		return nil
	}
	dir := filepath.Join(os.Getenv("HOME"), ".MakeMKV")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	conf := filepath.Join(dir, "settings.conf")
	existing, _ := os.ReadFile(conf)
	if strings.Contains(string(existing), key) {
		fmt.Fprintf(os.Stderr, "scan: key already in settings.conf, skipping write\n")
		return nil
	}
	content := fmt.Sprintf(
		"# Advanced Hardware Optimization Profile\n"+
			"app_Key = %q\n"+
			"\n"+
			"# Maximize RAM cache to handle high-bitrate 4K UHD and Blu-ray layer transitions smoothly\n"+
			"io_MaxReadCacheMb = \"1024\"\n"+
			"\n"+
			"# Fail fast on scratched discs/bad sectors instead of letting the drive controller lock up indefinitely\n"+
			"io_RetryCount = \"5\"\n"+
			"\n"+
			"# Enable internal Java support to accurately decode structural BD-J playlist obfuscation protections\n"+
			"app_JavaType = \"internal\"\n",
		key,
	)
	return os.WriteFile(conf, []byte(content), 0o600)
}

// ScanInfo runs `makemkvcon -r info dev:<device>` and returns the parsed titles.
func ScanInfo(ctx context.Context, makemkvBin, device, key string) (*disc.ClassifiedDisc, error) {
	fmt.Fprintf(os.Stderr, "scan: writing tuned config to settings.conf (len=%d)\n", len(key))
	if err := writeTunedConfig(key); err != nil {
		return nil, fmt.Errorf("write makemkv config: %w", err)
	}
	fmt.Fprintf(os.Stderr, "scan: running %s -r info dev:%s\n", makemkvBin, device)
	cmd := exec.CommandContext(ctx, makemkvBin, "--cache=128", "-r", "info", "dev:"+device)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start makemkvcon: %w", err)
	}

	result, parseErr := ScanInfoFromReader(io.TeeReader(stdout, os.Stderr), device)

	if result.Type == disc.DiscTypeUnknown && device != "" {
		if t := disc.ProbeDiscType(ctx, device); t != disc.DiscTypeUnknown {
			fmt.Fprintf(os.Stderr, "scan: CINFO:1 absent, disc type from raw probe: %s\n", t)
			result.Type = t
		}
	}

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

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "CINFO:"):
			parseDiscInfo(line[len("CINFO:"):], result)
		case strings.HasPrefix(line, "TINFO:"):
			parseTitleInfo(line[len("TINFO:"):], titleMap)
		case strings.HasPrefix(line, "SINFO:"):
			parseStreamInfo(line[len("SINFO:"):], titleMap)
		case strings.HasPrefix(line, "MSG:"):
			parseMsg(line[len("MSG:"):], result)
		case strings.HasPrefix(line, "TCOUNT:"), strings.HasPrefix(line, "PRGV:"):
			// TCOUNT is informational; PRGV during an info scan is unexpected but harmless.
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("reading makemkvcon output: %w", err)
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
	if attrID == cinfoDiscName {
		result.DiscName = unquote(parts[2])
	}
	if attrID == cinfoDiscType {
		val := strings.ToLower(unquote(parts[2]))
		switch {
		case strings.Contains(val, "blu-ray"):
			result.Type = disc.DiscTypeBluRay
		case strings.Contains(val, "dvd"):
			result.Type = disc.DiscTypeDVD
		}
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
	case tinfoName:
		t.Name = val
	case tinfoChapters:
		if n, err := strconv.Atoi(val); err == nil {
			t.ChapterCount = n
		}
	case tinfoDuration:
		if d, err := parseDuration(val); err == nil {
			t.Duration = d
		}
	case tinfoSourceFile:
		t.SourceFileName = val
	case tinfoSizeBytes:
		if b, err := strconv.ParseInt(val, 10, 64); err == nil {
			t.SizeGB = float64(b) / (1024 * 1024 * 1024)
		}
	case tinfoDesc:
		// Extract angle number from "(angle N)" in description
		t.AngleNumber = parseAngleNumber(val)
	}
}

// parseStreamInfo handles one SINFO payload (everything after "SINFO:").
// Format: <titleIdx>,<streamIdx>,<attrID>,<code>,"<value>"
func parseStreamInfo(payload string, titleMap map[int]*disc.MKVTitle) {
	parts := splitRobotLine(payload)
	if len(parts) < 5 {
		return
	}
	titleIdx, err := strconv.Atoi(parts[0])
	if err != nil {
		return
	}
	streamIdx, err := strconv.Atoi(parts[1])
	if err != nil {
		return
	}
	attrID, err := strconv.Atoi(parts[2])
	if err != nil {
		return
	}
	val := unquote(parts[4])

	t := titleMap[titleIdx]
	if t == nil {
		t = &disc.MKVTitle{Index: titleIdx}
		titleMap[titleIdx] = t
	}

	for len(t.Tracks) <= streamIdx {
		t.Tracks = append(t.Tracks, disc.Track{Index: len(t.Tracks)})
	}

	track := &t.Tracks[streamIdx]
	switch attrID {
	case sinfoStreamType:
		track.Type = val
		if val == "Audio" {
			t.AudioTrackCount++
		}
	case sinfoCodecName:
		track.CodecID = val
	case sinfoCodecLong:
		track.CodecLong = val
	case sinfoAudioLayout:
		track.AudioLayout = val
	case sinfoLanguage:
		track.Language = val
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

// parseAngleNumber extracts the angle number from strings like "Title - info (angle 1)".
// Returns 0 if no angle marker is found.
func parseAngleNumber(s string) int {
	// Look for "(angle N)" pattern
	idx := strings.Index(s, "(angle ")
	if idx == -1 {
		return 0
	}
	rest := s[idx+7:] // skip "(angle "
	endIdx := strings.Index(rest, ")")
	if endIdx == -1 {
		return 0
	}
	numStr := strings.TrimSpace(rest[:endIdx])
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return 0
	}
	return n
}
