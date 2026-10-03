//go:build !linux

package disc

func ioctlDriveStatus(device string) driveState { return driveUnsupported }
