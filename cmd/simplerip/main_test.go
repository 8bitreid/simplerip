package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMoveFile(t *testing.T) {
	cases := []struct {
		name string
		fn   func(src, dst string) error
	}{
		{"rename", moveFile},
		{"copyAndRemove", copyAndRemove},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "src.mkv")
			dst := filepath.Join(dir, "dst.mkv")
			if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := c.fn(src, dst); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if _, err := os.Stat(src); !os.IsNotExist(err) {
				t.Error("src should be removed")
			}
			got, err := os.ReadFile(dst)
			if err != nil || string(got) != "data" {
				t.Errorf("dst content: got %q, want %q", got, "data")
			}
		})
	}
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
