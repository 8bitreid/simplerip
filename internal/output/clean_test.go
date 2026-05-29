package output

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/8bitreid/simplerip/internal/inspect"
)

func installAnalyzeFakeFFProbe(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	script := filepath.Join(binDir, "ffprobe")
	content := `#!/bin/sh
file=""
for arg in "$@"; do
  file="$arg"
done
base=$(basename "$file")
case "$base" in
  keeper.mkv)
    cat <<'JSON'
{"format":{"duration":"600","size":"20000000000"},"streams":[{"codec_type":"video","codec_name":"h264","width":1920,"height":1080},{"codec_type":"audio","codec_name":"truehd","channels":8,"channel_layout":"7.1","tags":{"language":"eng"}},{"codec_type":"subtitle","codec_name":"subrip"}]}
JSON
    ;;
  dup.mkv)
    cat <<'JSON'
{"format":{"duration":"610","size":"10000000000"},"streams":[{"codec_type":"video","codec_name":"h264","width":1920,"height":1080},{"codec_type":"audio","codec_name":"ac3","channels":6,"channel_layout":"5.1","tags":{"language":"eng"}}]}
JSON
    ;;
  solo.mkv)
    cat <<'JSON'
{"format":{"duration":"900","size":"9000000000"},"streams":[{"codec_type":"video","codec_name":"h264","width":1920,"height":1080},{"codec_type":"audio","codec_name":"ac3","channels":6,"channel_layout":"5.1","tags":{"language":"eng"}}]}
JSON
    ;;
  *)
    exit 2
    ;;
esac
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake ffprobe: %v", err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
}

func TestAnalyzeDir(t *testing.T) {
	t.Run("no mkv files", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := AnalyzeDir(context.Background(), dir); err == nil {
			t.Fatal("expected AnalyzeDir to fail with no mkv files")
		}
	})

	t.Run("picks highest score as keeper", func(t *testing.T) {
		installAnalyzeFakeFFProbe(t)
		dir := t.TempDir()
		for _, name := range []string{"keeper.mkv", "dup.mkv", "solo.mkv"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
				t.Fatal(err)
			}
		}

		analyses, err := AnalyzeDir(context.Background(), dir)
		if err != nil {
			t.Fatalf("AnalyzeDir() error = %v", err)
		}
		if len(analyses) != 1 {
			t.Fatalf("analyses len = %d, want 1", len(analyses))
		}
		if filepath.Base(analyses[0].Keeper.Path) != "keeper.mkv" {
			t.Fatalf("keeper = %q, want keeper.mkv", analyses[0].Keeper.Path)
		}
		if len(analyses[0].Duplicates) != 1 || filepath.Base(analyses[0].Duplicates[0].Path) != "dup.mkv" {
			t.Fatalf("duplicates = %+v, want dup.mkv", analyses[0].Duplicates)
		}
	})
}

func TestGroupByDuration(t *testing.T) {
	tests := []struct {
		name      string
		durations []time.Duration
		tolerance time.Duration
		wantSizes []int
	}{
		{
			name:      "clusters overlapping durations",
			durations: []time.Duration{100 * time.Second, 115 * time.Second, 200 * time.Second, 220 * time.Second},
			tolerance: 20 * time.Second,
			wantSizes: []int{2, 2},
		},
		{
			name:      "all isolated",
			durations: []time.Duration{100 * time.Second, 150 * time.Second, 220 * time.Second},
			tolerance: 5 * time.Second,
			wantSizes: []int{1, 1, 1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			infos := make([]*inspect.FileInfo, 0, len(tc.durations))
			for i, dur := range tc.durations {
				infos = append(infos, &inspect.FileInfo{Path: filepath.Join("/tmp", string(rune('a'+i))+".mkv"), Duration: dur})
			}

			groups := groupByDuration(infos, tc.tolerance)
			gotSizes := make([]int, 0, len(groups))
			for _, group := range groups {
				gotSizes = append(gotSizes, len(group))
			}
			if !reflect.DeepEqual(gotSizes, tc.wantSizes) {
				t.Fatalf("group sizes = %v, want %v", gotSizes, tc.wantSizes)
			}
		})
	}
}

func TestFlattenSubdirs(t *testing.T) {
	tests := []struct {
		name          string
		makeLayout    func(t *testing.T, dir string)
		wantMovedBase []string
		wantRemain    []string
	}{
		{
			name: "moves files out of subdir",
			makeLayout: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(dir, "extras"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "extras", "a.mkv"), []byte("a"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "extras", "b.mkv"), []byte("b"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantMovedBase: []string{"a.mkv", "b.mkv"},
			wantRemain:    []string{"a.mkv", "b.mkv"},
		},
		{
			name: "renames colliding filename",
			makeLayout: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "a.mkv"), []byte("root"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(dir, "extras"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "extras", "a.mkv"), []byte("extra"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantMovedBase: []string{"a_extras.mkv"},
			wantRemain:    []string{"a.mkv", "a_extras.mkv"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.makeLayout(t, dir)

			moved, err := FlattenSubdirs(dir)
			if err != nil {
				t.Fatalf("FlattenSubdirs() error = %v", err)
			}

			var gotMovedBase []string
			for _, path := range moved {
				gotMovedBase = append(gotMovedBase, filepath.Base(path))
			}
			if !reflect.DeepEqual(gotMovedBase, tc.wantMovedBase) {
				t.Fatalf("moved = %v, want %v", gotMovedBase, tc.wantMovedBase)
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("ReadDir() error = %v", err)
			}
			var gotRemain []string
			for _, entry := range entries {
				if !entry.IsDir() {
					gotRemain = append(gotRemain, entry.Name())
				}
			}
			if !reflect.DeepEqual(gotRemain, tc.wantRemain) {
				t.Fatalf("remaining files = %v, want %v", gotRemain, tc.wantRemain)
			}
		})
	}
}

func TestRenameToTitle(t *testing.T) {
	tests := []struct {
		name     string
		folder   string
		fileName string
		wantPath string
	}{
		{name: "basic rename", folder: "Oppenheimer (2023)", fileName: "title.mkv", wantPath: filepath.Join("Oppenheimer (2023)", "Oppenheimer (2023).mkv")},
		{name: "spaces preserved", folder: "Blade Runner (1982)", fileName: "movie.mkv", wantPath: filepath.Join("Blade Runner (1982)", "Blade Runner (1982).mkv")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, tc.fileName)
			if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
				t.Fatal(err)
			}

			got, err := RenameToTitle(src, dir, tc.folder)
			if err != nil {
				t.Fatalf("RenameToTitle() error = %v", err)
			}
			if got != filepath.Join(dir, tc.wantPath) {
				t.Fatalf("RenameToTitle() = %q, want %q", got, filepath.Join(dir, tc.wantPath))
			}
			if _, err := os.Stat(got); err != nil {
				t.Fatalf("renamed file missing: %v", err)
			}
		})
	}
}

func TestExecuteDedupe(t *testing.T) {
	tests := []struct {
		name      string
		makeFiles func(t *testing.T, dir string) CleanAnalysis
		wantMoved []string
		wantKept  string
		wantErr   string
	}{
		{
			name: "moves duplicates into _duplicates",
			makeFiles: func(t *testing.T, dir string) CleanAnalysis {
				t.Helper()
				keeper := filepath.Join(dir, "keeper.mkv")
				dup1 := filepath.Join(dir, "dup1.mkv")
				dup2 := filepath.Join(dir, "dup2.mkv")
				for _, path := range []string{keeper, dup1, dup2} {
					if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				return CleanAnalysis{
					Keeper: FileReport{Path: keeper, SizeBytes: 10, Duration: time.Minute},
					Duplicates: []FileReport{
						{Path: dup1, SizeBytes: 9, Duration: time.Minute},
						{Path: dup2, SizeBytes: 8, Duration: time.Minute},
					},
				}
			},
			wantMoved: []string{"_duplicates/dup1.mkv", "_duplicates/dup2.mkv"},
			wantKept:  "keeper.mkv",
		},
		{
			name: "missing duplicate file returns error",
			makeFiles: func(t *testing.T, dir string) CleanAnalysis {
				t.Helper()
				keeper := filepath.Join(dir, "keeper.mkv")
				if err := os.WriteFile(keeper, []byte("keeper"), 0o644); err != nil {
					t.Fatal(err)
				}
				return CleanAnalysis{
					Keeper:     FileReport{Path: keeper},
					Duplicates: []FileReport{{Path: filepath.Join(dir, "missing.mkv")}},
				}
			},
			wantErr: "move \"missing.mkv\"",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			analysis := tc.makeFiles(t, dir)

			results, err := ExecuteDedupe(dir, []CleanAnalysis{analysis})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ExecuteDedupe() error = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExecuteDedupe() error = %v", err)
			}
			if len(results) != 1 {
				t.Fatalf("results len = %d, want 1", len(results))
			}
			if filepath.Base(results[0].Kept) != tc.wantKept {
				t.Fatalf("Kept = %q, want base %q", results[0].Kept, tc.wantKept)
			}
			var gotMoved []string
			for _, moved := range results[0].Duplicates {
				gotMoved = append(gotMoved, filepath.Base(filepath.Dir(moved))+"/"+filepath.Base(moved))
			}
			if !reflect.DeepEqual(gotMoved, tc.wantMoved) {
				t.Fatalf("moved = %v, want %v", gotMoved, tc.wantMoved)
			}
		})
	}
}

func TestVerifyFileAndWriteLog(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(t *testing.T, dir string) (src, dst string)
		wantErr  string
		writeLog bool
	}{
		{
			name: "verify file ok",
			setup: func(t *testing.T, dir string) (string, string) {
				t.Helper()
				src := filepath.Join(dir, "src.mkv")
				dst := filepath.Join(dir, "dst.mkv")
				if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(dst, []byte("data"), 0o644); err != nil {
					t.Fatal(err)
				}
				return src, dst
			},
		},
		{
			name: "verify size mismatch",
			setup: func(t *testing.T, dir string) (string, string) {
				t.Helper()
				src := filepath.Join(dir, "src.mkv")
				dst := filepath.Join(dir, "dst.mkv")
				if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(dst, []byte("different"), 0o644); err != nil {
					t.Fatal(err)
				}
				return src, dst
			},
			wantErr: "size mismatch",
		},
		{
			name: "write rip log",
			setup: func(t *testing.T, dir string) (string, string) {
				t.Helper()
				return dir, dir
			},
			writeLog: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src, dst := tc.setup(t, dir)

			if tc.writeLog {
				log := RipLog{Title: "Oppenheimer", DiscName: "DISC1", StagingDir: "/staging", DestDir: "/output", Files: []string{"/output/Oppenheimer (2023)/Oppenheimer (2023).mkv"}}
				if err := writeLog(dir, log); err != nil {
					t.Fatalf("writeLog() error = %v", err)
				}
				data, err := os.ReadFile(filepath.Join(dir, "rip.json"))
				if err != nil {
					t.Fatalf("ReadFile() error = %v", err)
				}
				var got RipLog
				if err := json.Unmarshal(data, &got); err != nil {
					t.Fatalf("Unmarshal() error = %v", err)
				}
				if got.Title != "Oppenheimer" || got.DiscName != "DISC1" {
					t.Fatalf("rip.json = %+v", got)
				}
				return
			}

			err := verifyFile(src, dst)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("verifyFile() error = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("verifyFile() error = %v", err)
			}
		})
	}
}
