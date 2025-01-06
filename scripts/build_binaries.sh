#!/usr/bin/env bash
set -euo pipefail

VERSION="$(git describe --tags --always --dirty || true)"
COMMIT="$(git rev-parse --short HEAD || true)"
DATE="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"

MAIN_PACKAGE="./cmd/main.go"
BINARY_NAME="accelero"

PLATFORMS=(
  "linux/amd64"
  "linux/386"
  "linux/arm"
  "linux/arm64"
  "windows/amd64"
  "windows/386"
  "windows/arm64"
  "windows/arm"
  "darwin/amd64"
  "darwin/arm64"
)

echo "Running go mod tidy..."
go mod tidy

rm -rf dist
mkdir -p dist

for PLATFORM in "${PLATFORMS[@]}"; do
  GOOS="${PLATFORM%/*}"
  GOARCH="${PLATFORM#*/}"
  
  if [[ "$GOARCH" == "arm" ]]; then
    GOARM=7
  fi

  OUTPUT_NAME="${BINARY_NAME}_${GOOS}_${GOARCH}"
  if [[ "$GOOS" == "windows" ]]; then
    OUTPUT_NAME+=".exe"
  fi

  echo "Building $BINARY_NAME for $GOOS/$GOARCH ..."
  env CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
    go build \
      -ldflags "-s -w \
        -X main.version=$VERSION \
        -X main.commit=$COMMIT \
        -X main.date=$DATE" \
      -o "dist/${OUTPUT_NAME}" \
      "$MAIN_PACKAGE"
done

cp LICENSE.md dist/

pushd dist > /dev/null
  for PLATFORM in "${PLATFORMS[@]}"; do
    GOOS="${PLATFORM%/*}"
    GOARCH="${PLATFORM#*/}"
    OUTPUT_NAME="${BINARY_NAME}_${GOOS}_${GOARCH}"
    if [[ "$GOOS" == "windows" ]]; then
      zip "${OUTPUT_NAME}.zip" "${OUTPUT_NAME}.exe" LICENSE.md
    else
      tar -czf "${OUTPUT_NAME}.tar.gz" "${OUTPUT_NAME}" LICENSE.md
    fi
  done
popd > /dev/null

rm -f dist/LICENSE.md

pushd dist > /dev/null
  shasum -a 256 *.tar.gz *.zip > checksums.txt
popd > /dev/null

echo "All builds done. Binaries are in the dist/ directory."
