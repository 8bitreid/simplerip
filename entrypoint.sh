#!/bin/sh
set -e

# Extract sdf.bin from appdata.tar on first run if not already present.
# The volume mount at /root/.MakeMKV hides the image directory, so we
# need to do this at runtime rather than at build time.
if [ ! -f /root/.MakeMKV/sdf.bin ] && [ -f /usr/share/MakeMKV/appdata.tar ]; then
    mkdir -p /root/.MakeMKV
    cd /tmp
    tar -xf /usr/share/MakeMKV/appdata.tar --wildcards 'sdf_*.bin' 2>/dev/null
    sdf=$(ls sdf_*.bin 2>/dev/null | head -1)
    if [ -n "$sdf" ]; then
        mv "$sdf" /root/.MakeMKV/sdf.bin
        echo "simplerip: extracted sdf.bin from appdata.tar" >&2
    fi
fi

# Start systemd-udevd inside the container so block "change" events get
# enriched with ID_CDROM_MEDIA by the cdrom_id helper. Without a running
# udevd, `udevadm monitor` only sees raw KERNEL uevents (which never carry
# ID_CDROM_MEDIA), so disc insertion can't be distinguished from removal.
# The container has its own network namespace, so the host's udevd events
# don't reach us — we must run our own.
udevd_bin="$(command -v systemd-udevd || true)"
[ -z "$udevd_bin" ] && [ -x /lib/systemd/systemd-udevd ] && udevd_bin=/lib/systemd/systemd-udevd
[ -z "$udevd_bin" ] && [ -x /usr/lib/systemd/systemd-udevd ] && udevd_bin=/usr/lib/systemd/systemd-udevd
if [ -n "$udevd_bin" ]; then
    "$udevd_bin" --daemon
    # Replay current device state so already-inserted discs are seen.
    udevadm trigger --subsystem-match=block --action=change || true
    echo "simplerip: started udevd ($udevd_bin)" >&2
else
    echo "simplerip: WARNING: systemd-udevd not found; udev disc detection will not work" >&2
fi

exec /usr/local/bin/simplerip "$@"
