#!/bin/bash

set -exuo pipefail
UKP_PACKAGE=$1
COMMITS=$2

ts_version="$(${UKP_PACKAGE}/usr/sbin/tailscaled --version)"
VERSION=$(echo -n "$ts_version" | head -n 1 | sed 's/^v//')
VERSION="${VERSION}-${COMMITS}+$(git rev-parse --short=7 HEAD)"

printf '%s\n' "${VERSION}"
