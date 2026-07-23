#!/bin/sh
set -eu

# Generated files are embedded by internal/installer. No host path, service
# manager, network, credential, or qualification operation is used.
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
out="$root/internal/installer/payloads"
mkdir -p "$out/linux_amd64" "$out/linux_arm64"
for arch in amd64 arm64; do
		GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go build -tags=shipmates_qualifier_payload -trimpath -buildvcs=false -ldflags='-s -w' -o "$out/linux_$arch/shipmates-m3-qualifier-run" "$root/cmd/shipmates-m3-qualifier-run"
	GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags='-s -w' -o "$out/linux_$arch/shipmates-cgroup-launcher" "$root/tools/shipmates-cgroup-launcher"
done
