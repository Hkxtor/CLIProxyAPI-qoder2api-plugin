#!/usr/bin/env bash
set -euo pipefail

command -v go >/dev/null 2>&1 || {
  echo "Go 1.22 or newer is required." >&2
  exit 1
}

case "$(go env GOOS)" in
  windows) ext="dll" ;;
  darwin) ext="dylib" ;;
  *) ext="so" ;;
esac

mkdir -p dist
go test ./... -count=1
CGO_ENABLED=1 go build \
  -buildvcs=false \
  -buildmode=c-shared \
  -o "dist/qoder2api.${ext}" \
  .

echo "Built dist/qoder2api.${ext}"
