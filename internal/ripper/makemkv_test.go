package ripper

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// infoFixture is representative of actual `makemkvcon -r info dev:/dev/sr0`
// output for a Blu-ray with one main feature, one extra, and one junk title.
// Includes CINFO, TCOUNT, TINFO, SINFO, PRGV, and MSG lines.
// infoFixture uses the real v1.18.3 attribute IDs:
//
//	TINFO attr 8 = chapter count, 9 = duration, 11 = size bytes
//	SINFO attr 1 = stream type ("Video"/"Audio"/"Subtitles")
const infoFixture = `MSG:1005,0,1,"MakeMKV v1.18.3 linux(x86-64-release)"
MSG:1011,0,1,"/usr/bin/makemkvcon"
CINFO:1,6331,"Blu-ray disc"
CINFO:2,0,"eng"
CINFO:8,0,"1"
CINFO:13,0,"2010/7/13,0:0:0"
CINFO:30,0,"INCEPTION_D1"
CINFO:33,6120,"Blu-ray"
TCOUNT:3
TINFO:0,2,0,"Inception"
TINFO:0,8,0,"24"
TINFO:0,9,0,"2:28:01"
TINFO:0,11,0,"38654705664"
TINFO:0,27,0,"00800.mpls"
SINFO:0,0,1,6201,"Video"
SINFO:0,0,5,0,"V_MPEG4/ISO/MVC"
SINFO:0,1,1,6202,"Audio"
SINFO:0,1,5,0,"A_TRUEHD"
SINFO:0,2,1,6202,"Audio"
SINFO:0,2,5,0,"A_AC3"
SINFO:0,3,1,6202,"Audio"
SINFO:0,3,5,0,"A_AC3"
SINFO:0,4,1,6203,"Subtitles"
SINFO:0,5,1,6203,"Subtitles"
TINFO:1,2,0,"Behind the Scenes"
TINFO:1,8,0,"4"
TINFO:1,9,0,"0:22:15"
TINFO:1,11,0,"4294967296"
TINFO:1,27,0,"00002.mpls"
SINFO:1,0,1,6201,"Video"
SINFO:1,1,1,6202,"Audio"
TINFO:2,2,0,"FBI Warning"
TINFO:2,8,0,"1"
TINFO:2,9,0,"0:01:30"
TINFO:2,11,0,"536870912"
TINFO:2,27,0,"00003.mpls"
SINFO:2,0,1,6201,"Video"
SINFO:2,1,1,6202,"Audio"
PRGV:0,0,10000
MSG:5010,0,1,"Operation successfully completed"
MSG:3307,1,0,"Some drive warning occurred"
`

func TestParseInfoOutput(t *testing.T) {
	r := strings.NewReader(infoFixture)
	result, err := parseInfoOutput(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.DiscName != "INCEPTION_D1" {
		t.Errorf("disc name: got %q, want %q", result.DiscName, "INCEPTION_D1")
	}
	if len(result.Titles) != 3 {
		t.Fatalf("title count: got %d, want 3", len(result.Titles))
	}

	main := result.Titles[0]
	if main.Name != "Inception" {
		t.Errorf("title[0].Name: got %q", main.Name)
	}
	if main.ChapterCount != 24 {
		t.Errorf("title[0].ChapterCount: got %d, want 24", main.ChapterCount)
	}
	wantDur := 2*time.Hour + 28*time.Minute + 1*time.Second
	if main.Duration != wantDur {
		t.Errorf("title[0].Duration: got %v, want %v", main.Duration, wantDur)
	}
	if main.SourceFileName != "00800.mpls" {
		t.Errorf("title[0].SourceFileName: got %q", main.SourceFileName)
	}
	// 38654705664 bytes = 36.0 GiB
	if main.SizeGB < 35.9 || main.SizeGB > 36.1 {
		t.Errorf("title[0].SizeGB: got %.2f, want ~36.0", main.SizeGB)
	}
	if main.AudioTrackCount != 3 {
		t.Errorf("title[0].AudioTrackCount: got %d, want 3 (TrueHD 7.1 + AC3 5.1 + AC3 2.0)", main.AudioTrackCount)
	}

	extra := result.Titles[1]
	if extra.Name != "Behind the Scenes" {
		t.Errorf("title[1].Name: got %q", extra.Name)
	}
	if extra.Duration != 22*time.Minute+15*time.Second {
		t.Errorf("title[1].Duration: got %v", extra.Duration)
	}
	if extra.AudioTrackCount != 1 {
		t.Errorf("title[1].AudioTrackCount: got %d, want 1", extra.AudioTrackCount)
	}

	junk := result.Titles[2]
	if junk.Duration != 90*time.Second {
		t.Errorf("title[2].Duration: got %v, want 1m30s", junk.Duration)
	}
	if junk.AudioTrackCount != 1 {
		t.Errorf("title[2].AudioTrackCount: got %d, want 1", junk.AudioTrackCount)
	}
}

func TestParseInfoWarnings(t *testing.T) {
	r := strings.NewReader(infoFixture)
	result, err := parseInfoOutput(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// flags=1 MSG line should be collected; flags=0 lines should not.
	if len(result.Warnings) != 1 {
		t.Fatalf("warnings count: got %d, want 1: %v", len(result.Warnings), result.Warnings)
	}
	if result.Warnings[0] != "Some drive warning occurred" {
		t.Errorf("warnings[0]: got %q", result.Warnings[0])
	}
}

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"2:28:01", 2*time.Hour + 28*time.Minute + time.Second},
		{"0:01:30", 90 * time.Second},
		{"0:00:00", 0},
		{"10:00:00", 10 * time.Hour},
	}
	for _, c := range cases {
		got, err := parseDuration(c.in)
		if err != nil {
			t.Errorf("parseDuration(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestWriteKey(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	const key = "T-testkey123"
	if err := writeKey(key); err != nil {
		t.Fatalf("writeKey: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmp, ".MakeMKV", "settings.conf"))
	if err != nil {
		t.Fatalf("settings.conf not written: %v", err)
	}
	got := string(data)
	want := `app_Key = "T-testkey123"` + "\n"
	if got != want {
		t.Errorf("settings.conf content:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestWriteKeyEmpty(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	if err := writeKey(""); err != nil {
		t.Fatalf("writeKey empty: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, ".MakeMKV", "settings.conf")); !os.IsNotExist(err) {
		t.Error("settings.conf should not be written for empty key")
	}
}

func TestSplitRobotLine(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`0,2,0,"Title Name"`, []string{"0", "2", "0", `"Title Name"`}},
		{`0,30,0,"DISC,WITH,COMMAS"`, []string{"0", "30", "0", `"DISC,WITH,COMMAS"`}},
		{`0,0,22,6101,"Audio"`, []string{"0", "0", "22", "6101", `"Audio"`}},
		{`3`, []string{"3"}},
	}
	for _, c := range cases {
		got := splitRobotLine(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitRobotLine(%q): len %d, want %d: %v", c.in, len(got), len(c.want), got)
			continue
		}
		for i, g := range got {
			if g != c.want[i] {
				t.Errorf("splitRobotLine(%q)[%d] = %q, want %q", c.in, i, g, c.want[i])
			}
		}
	}
}

func TestParseAngleNumber(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{in: "Title (angle 1)", want: 1},
		{in: "Main title (angle 2)", want: 2},
		{in: "No angle marker", want: 0},
		{in: "Broken (angle x)", want: 0},
	}

	for _, tc := range tests {
		if got := parseAngleNumber(tc.in); got != tc.want {
			t.Fatalf("parseAngleNumber(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParsePRGVInvalid(t *testing.T) {
	tests := []string{"", "1,2", "a,b,c", "1,2,three"}
	for _, in := range tests {
		if _, ok := parsePRGV(in); ok {
			t.Fatalf("parsePRGV(%q) unexpectedly succeeded", in)
		}
	}
}

func TestScanInfoFromReaderSetsDevice(t *testing.T) {
	result, err := ScanInfoFromReader(strings.NewReader(infoFixture), "/dev/sr0")
	if err != nil {
		t.Fatalf("ScanInfoFromReader() error = %v", err)
	}
	if result.Device != "/dev/sr0" {
		t.Fatalf("Device = %q, want %q", result.Device, "/dev/sr0")
	}
}

func TestScanInfoCommand(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	fake := filepath.Join(tmp, "fake-makemkvcon")
	script := `#!/bin/sh
cat <<'EOF'
CINFO:30,0,"TEST_DISC"
TCOUNT:1
TINFO:0,2,0,"Main"
TINFO:0,8,0,"10"
TINFO:0,9,0,"1:40:00"
SINFO:0,0,1,6202,"Audio"
EOF
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake makemkvcon: %v", err)
	}

	result, err := ScanInfo(context.Background(), fake, "/dev/sr0", "test-key")
	if err != nil {
		t.Fatalf("ScanInfo() error = %v", err)
	}
	if len(result.Titles) != 1 || result.Titles[0].Name != "Main" {
		t.Fatalf("unexpected scan result: %+v", result)
	}
}
