package disc

import (
	"bufio"
	"context"
	"log/slog"
	"os/exec"
	"strings"
)

// ListenUdevEvents monitors the kernel's udev event stream for block devices.
// It spawns a single long-running `udevadm monitor` subprocess for the
// lifetime of ctx and emits a DiscEvent for each "change" action on a
// target device.
func ListenUdevEvents(ctx context.Context, targetDevices []string) <-chan DiscEvent {
	out := make(chan DiscEvent)

	targetMap := make(map[string]bool, len(targetDevices))
	for _, d := range targetDevices {
		targetMap[d] = true
	}

	go func() {
		defer close(out)

		cmd := exec.CommandContext(ctx, "udevadm", "monitor", "--environment", "--subsystem=block")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			slog.Error("failed to create stdout pipe for udevadm", "err", err)
			return
		}
		if err := cmd.Start(); err != nil {
			slog.Error("failed to start udevadm", "err", err)
			return
		}

		defer func() {
			if werr := cmd.Wait(); werr != nil && ctx.Err() == nil {
				slog.Error("udevadm exited unexpectedly", "err", werr)
			}
		}()

		scanner := bufio.NewScanner(stdout)
		block := make(map[string]string)

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())

			if line == "" {
				if len(block) > 0 {
					devName := block["DEVNAME"]

					if targetMap[devName] && block["ACTION"] == "change" {
						present := block["ID_CDROM_MEDIA"] == "1"
						event := DiscEvent{
							Device:  devName,
							Present: present,
						}

						slog.Info("udev event matched",
							"device", devName,
							"present", present,
							"raw_properties", block,
						)

						select {
						case out <- event:
						case <-ctx.Done():
							return
						}
					}
				}
				block = make(map[string]string)
				continue
			}

			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				block[parts[0]] = parts[1]
			}
		}

		if err := scanner.Err(); err != nil {
			slog.Error("udevadm scanner error", "err", err)
		}
	}()

	return out
}