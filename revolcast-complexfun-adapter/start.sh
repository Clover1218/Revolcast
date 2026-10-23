#!/bin/sh
set -eu

# Change to the directory containing this script.
cd "$(dirname "$0")"

# Build on the host at startup. If the panel restarts this script often,
# build once manually and replace this section with: exec ./controller controller.json
if ! command -v go >/dev/null 2>&1; then
  echo "[controller] Go compiler not found. Upload a prebuilt Linux binary or install Go."
  exit 1
fi

go build -o controller ./main.go
exec ./controller ./controller.json
