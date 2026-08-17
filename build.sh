#!/bin/sh
# Builds the agentwork CLI and daemon with one shared version stamp.
#
#   AGENTWORK_COMPILE_VERSION=0.0.1-beta.2 ./build.sh
#
# The env var overrides the default (0.0.1-beta.1). Both binaries MUST be
# built with the same stamp: the register-time version check between the
# CLI and the daemon warns on a mismatch, and mixed stamps are the symptom
# of a mixed deploy.
set -e
cd "$(dirname "$0")"

VERSION="${AGENTWORK_COMPILE_VERSION:-0.0.1-beta.1}"
LDFLAGS="-X main.cliVersion=$VERSION \
-X github.com/eushing/agentwork/internal/daemon.DaemonVersion=$VERSION"

mkdir -p build
go build -ldflags "$LDFLAGS" -o build/agentwork ./cmd/agentwork-cli
go build -ldflags "$LDFLAGS" -o build/agentwork-daemon ./cmd/agentwork-daemon
echo "built build/agentwork (CLI) + build/agentwork-daemon v$VERSION"
