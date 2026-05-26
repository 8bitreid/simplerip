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

exec /usr/local/bin/simplerip "$@"
