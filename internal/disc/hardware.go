package disc

import (
	"os"
	"path/filepath"
	"strings"
)

// sysBlockDir is where Linux exposes block devices. Tests point it elsewhere.
var sysBlockDir = "/sys/block"

// DriveHardware is what the kernel reports about an optical drive.
type DriveHardware struct {
	Vendor   string `json:"vendor,omitempty"`
	Model    string `json:"model,omitempty"`
	Firmware string `json:"firmware,omitempty"`
}

// ReadDriveHardware reads a drive's vendor, model and firmware revision from
// sysfs (e.g. /sys/block/sr0/device/model for /dev/sr0). Fields the kernel
// does not report are left empty; it never fails.
func ReadDriveHardware(device string) DriveHardware {
	dir := filepath.Join(sysBlockDir, filepath.Base(device), "device")
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	return DriveHardware{Vendor: read("vendor"), Model: read("model"), Firmware: read("rev")}
}

// SetSysBlockDirForTest points ReadDriveHardware at a fake sysfs tree.
func SetSysBlockDirForTest(dir string) (restore func()) {
	previous := sysBlockDir
	sysBlockDir = dir
	return func() { sysBlockDir = previous }
}
