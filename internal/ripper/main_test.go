package ripper

import (
	"os"
	"testing"
)

// TestMain points HOME at a throwaway directory so no test can write the
// developer's real ~/.MakeMKV/settings.conf.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "simplerip-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
