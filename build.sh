#!/bin/sh
set -eu

ROOT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
DIST_DIR="${ROOT_DIR}/dist"
GO_BIN=""

rm -rf "${DIST_DIR}"
mkdir -p "${DIST_DIR}"

if command -v go >/dev/null 2>&1; then
  GO_BIN="$(command -v go)"
elif [ -x /opt/homebrew/bin/go ]; then
  GO_BIN="/opt/homebrew/bin/go"
elif [ -x /usr/local/go/bin/go ]; then
  GO_BIN="/usr/local/go/bin/go"
elif [ -x "$HOME/go/bin/go" ]; then
  GO_BIN="$HOME/go/bin/go"
fi

if [ -n "${GO_BIN}" ]; then
  cd "${ROOT_DIR}"
  GOCACHE="${ROOT_DIR}/.cache/go-build" GOMODCACHE="${ROOT_DIR}/.cache/go-mod" CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "${GO_BIN}" build -o "${DIST_DIR}/mnemo-server" ./cmd/mnemo-server
elif command -v docker >/dev/null 2>&1; then
  mkdir -p "${ROOT_DIR}/.cache/go-build" "${ROOT_DIR}/.cache/go-mod"
  docker run --rm \
    -v "${ROOT_DIR}:/workspace" \
    -v "${ROOT_DIR}/.cache/go-build:/root/.cache/go-build" \
    -v "${ROOT_DIR}/.cache/go-mod:/go/pkg/mod" \
    -w /workspace \
    golang:1.24-alpine \
    sh -lc 'CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /workspace/dist/mnemo-server ./cmd/mnemo-server'
else
  echo "No Go toolchain found, and docker is unavailable. Install Go or enable docker." >&2
  exit 1
fi

cd "${ROOT_DIR}"
cp "${ROOT_DIR}/run.sh" "${DIST_DIR}/run.sh"
chmod +x "${DIST_DIR}/mnemo-server" "${DIST_DIR}/run.sh"
