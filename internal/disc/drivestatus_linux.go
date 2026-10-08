//go:build linux

package disc

import (
	"syscall"
)

const (
	cdromDriveStatus = 0x5326     // CDROM_DRIVE_STATUS
	cdslCurrent      = 0x7fffffff // CDSL_CURRENT: query the current slot

	cdsNoInfo      = 0
	cdsNoDisc      = 1
	cdsTrayOpen    = 2
	cdsDriveNotRdy = 3
	cdsDiscOK      = 4
)

// ioctlDriveStatus asks the kernel whether media is loaded, without spinning
// up the drive or running makemkvcon. The device is opened non-blocking so the
// call never waits on a drive that is busy or reading a disc.
func ioctlDriveStatus(device string) driveState {
	fd, err := syscall.Open(device, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return driveError
	}
	defer syscall.Close(fd)

	r, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), cdromDriveStatus, cdslCurrent)
	switch errno {
	case 0:
	case syscall.ENOTTY, syscall.EINVAL:
		return driveUnsupported // not a CD-ROM device; the kernel can't answer
	default:
		return driveError
	}
	switch int(r) {
	case cdsDiscOK:
		return driveDisc
	case cdsNoDisc:
		return driveEmpty
	case cdsTrayOpen:
		return driveTrayOpen
	default: // cdsNoInfo, cdsDriveNotRdy: normal while a disc loads/spins up
		return driveLoading
	}
}
