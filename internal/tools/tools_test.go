package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPathPrefersTestDirThenSystemDirs(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "simplerip-fake-tool")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := Path("simplerip-fake-tool"); got != "/usr/bin/simplerip-fake-tool" {
		t.Errorf("Path() before override = %q, want /usr/bin fallback", got)
	}

	t.Run("override", func(t *testing.T) {
		UseDirForTest(t, dir)
		if got := Path("simplerip-fake-tool"); got != fake {
			t.Errorf("Path() = %q, want %q", got, fake)
		}
	})

	if got := Path("simplerip-fake-tool"); got != "/usr/bin/simplerip-fake-tool" {
		t.Errorf("Path() after cleanup = %q, want /usr/bin fallback", got)
	}
}

func TestPathIgnoresPATH(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "simplerip-fake-tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if got := Path("simplerip-fake-tool"); got != "/usr/bin/simplerip-fake-tool" {
		t.Errorf("Path() = %q, want PATH to be ignored", got)
	}
}

func TestPathSkipsNonExecutable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "simplerip-fake-tool"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	UseDirForTest(t, dir)
	if got := Path("simplerip-fake-tool"); got != "/usr/bin/simplerip-fake-tool" {
		t.Errorf("Path() = %q, want non-executable file skipped", got)
	}
}
