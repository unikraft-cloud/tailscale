#!/bin/bash

set -exuo pipefail
UKP_PACKAGE=${1:-}
COMMITS=${2:-}
VERSION_SHORT=${3:-}

if [ -z "${COMMITS}" ]; then
	COMMITS="$(git describe --tags --long | awk -F'-' '{print $2}')"
fi

if [ -z "${VERSION_SHORT}" ]; then
	# No version passed in: ask the built binary. Only works for native
	# builds, cross-compiled binaries cannot be executed here.
	ts_version="$("${UKP_PACKAGE}/usr/sbin/tailscaled" --version)"
	VERSION_SHORT=$(printf '%s' "$ts_version" | head -n 1 | sed 's/^v//')
fi

VERSION="${VERSION_SHORT}-${COMMITS}+$(git rev-parse --short=7 HEAD)"

printf '%s\n' "${VERSION}"
