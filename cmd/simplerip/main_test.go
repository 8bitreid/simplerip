package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func assertMovedFile(t *testing.T, src, dst string) {
	t.Helper()

	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("src should be removed")
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "data" {
		t.Errorf("dst content: got %q, want %q", got, "data")
	}
}

func deviceID(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, syscall.EINVAL
	}
	return uint64(st.Dev), nil
}

func findCrossDeviceDir(t *testing.T, srcDir string) (string, bool) {
	t.Helper()

	srcDev, err := deviceID(srcDir)
	if err != nil {
		t.Fatalf("stat source device: %v", err)
	}

	for _, candidate := range []string{"/dev/shm", "/var/tmp", "/tmp"} {
		info, err := os.Stat(candidate)
		if err != nil || !info.IsDir() {
			continue
		}
		dev, err := deviceID(candidate)
		if err != nil {
			continue
		}
		if dev != srcDev {
			return candidate, true
		}
	}

	return "", false
}

func TestMoveFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	dst := filepath.Join(dir, "dst.mkv")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := moveFile(src, dst); err != nil {
		t.Fatalf("moveFile: %v", err)
	}

	assertMovedFile(t, src, dst)
}

func TestCopyAndRemove(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	dst := filepath.Join(dir, "dst.mkv")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := copyAndRemove(src, dst); err != nil {
		t.Fatalf("copyAndRemove: %v", err)
	}

	assertMovedFile(t, src, dst)
}

func TestMoveFileCrossDeviceFallback(t *testing.T) {
	srcDir := t.TempDir()
	dstRoot, ok := findCrossDeviceDir(t, srcDir)
	if !ok {
		t.Skip("no writable directory on a different device is available to exercise moveFile fallback")
	}

	dstDir := filepath.Join(dstRoot, "simplerip-test-cross-device")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatalf("mkdir dst dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(dstDir)
	})

	src := filepath.Join(srcDir, "src.mkv")
	dst := filepath.Join(dstDir, "dst.mkv")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	srcDev, err := deviceID(srcDir)
	if err != nil {
		t.Fatalf("stat source device: %v", err)
	}
	dstDev, err := deviceID(dstDir)
	if err != nil {
		t.Fatalf("stat destination device: %v", err)
	}
	if srcDev == dstDev {
		t.Skip("source and destination are on the same device; cannot exercise cross-device fallback")
	}

	if err := moveFile(src, dst); err != nil {
		t.Fatalf("moveFile cross-device fallback: %v", err)
	}

	assertMovedFile(t, src, dst)
}

func TestQueryFromMKVPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/staging/Revenge of the Sith_t00.mkv", "Revenge of the Sith"},
		{"/staging/Revenge of the Sith_t02.mkv", "Revenge of the Sith"},
		{"/output/THE_DARK_KNIGHT_t00.mkv", "THE DARK KNIGHT"},
		{"/output/Oppenheimer_t00.mkv", "Oppenheimer"},
		// No _tNN suffix — use name as-is.
		{"/output/Oppenheimer.mkv", "Oppenheimer"},
		// _t not followed by digits — leave alone.
		{"/output/District_t9000_extra.mkv", "District t 9000 extra"},
	}
	for _, c := range cases {
		got := queryFromMKVPath(c.in)
		if got != c.want {
			t.Errorf("queryFromMKVPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
