// Package tools resolves the external programs SimpleRip runs (makemkvcon,
// ffprobe, rsync, ...) to absolute paths in fixed system directories, so a
// writable directory placed earlier in PATH cannot substitute another binary.
package tools

import (
	"os"
	"path/filepath"
	"sync"
)

// systemDirs are root-owned directories searched in order.
var systemDirs = []string{
	"/usr/local/bin",
	"/usr/bin",
	"/bin",
	"/usr/local/sbin",
	"/usr/sbin",
	"/sbin",
}

var (
	mu       sync.RWMutex
	testDirs []string
)

// Path returns the absolute path of the named program. When it is not found,
// the /usr/bin path is returned so the exec error names a concrete file.
func Path(name string) string {
	mu.RLock()
	dirs := append(append([]string(nil), testDirs...), systemDirs...)
	mu.RUnlock()
	for _, dir := range dirs {
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return filepath.Join("/usr/bin", name)
}

// testingT is the subset of testing.TB used here, so production code does not
// import the testing package.
type testingT interface {
	Helper()
	Cleanup(func())
}

// UseDirForTest makes Path search dir before the system directories until the
// test ends, so tests can substitute fake programs.
func UseDirForTest(t testingT, dir string) {
	t.Helper()
	mu.Lock()
	previous := testDirs
	testDirs = append([]string{dir}, testDirs...)
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		testDirs = previous
		mu.Unlock()
	})
}
