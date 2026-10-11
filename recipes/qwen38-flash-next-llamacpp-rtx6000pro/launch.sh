#!/bin/sh
set -eu

# The reviewed artifact is checksum-verified before this container starts.
# Install on the container's disk-backed layer, not in host RAM or a tmpfs.
dependency_dir=/opt/llama-dependencies
mkdir -p "$dependency_dir"
dpkg-deb --extract /var/lib/lmw/artifacts/libgomp1.deb "$dependency_dir"
export LD_LIBRARY_PATH="$dependency_dir/usr/lib/x86_64-linux-gnu${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"

runtime_dir=/opt/llama-runtime
mkdir -p "$runtime_dir"
tar --extract --gzip --file /var/lib/lmw/artifacts/llama-cuda13.tar.gz \
    --directory "$runtime_dir" --no-same-owner
exec "$runtime_dir/llama-server" "$@"
