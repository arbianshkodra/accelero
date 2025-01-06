#!/usr/bin/env bash
set -euo pipefail

VERSION="$(git describe --tags --always --dirty || true)"
STRIPPED_VERSION="$(echo "$VERSION" | sed 's/^v//')"
DOCKER_REPO="arbianshkodra/accelero"

PLATFORMS=(
    "linux/amd64"
    "linux/386"
    "linux/arm64"
    "linux/arm/v7"
)

if ! docker info >/dev/null 2>&1; then
    if [[ -n "${DOCKER_CONFIG:-}" ]]; then
        echo "Using Docker config from DOCKER_CONFIG..."
    elif [[ -n "${DOCKER_USERNAME:-}" ]] && [[ -n "${DOCKER_PASSWORD:-}" ]]; then
        echo "Logging into Docker Hub using credentials..."
        echo "$DOCKER_PASSWORD" | docker login -u "$DOCKER_USERNAME" --password-stdin
    else
        echo "No Docker authentication found. Ensure you're logged in or provide credentials."
        exit 1
    fi
fi

echo "Setting up Docker BuildX..."
docker buildx create --use --name multi-arch-builder || true
docker buildx inspect --bootstrap

echo "Building multi-arch images..."
docker buildx build \
    --platform=$(IFS=,; echo "${PLATFORMS[*]}") \
    --build-arg TARGETARCH \
    --tag "${DOCKER_REPO}:${STRIPPED_VERSION}" \
    --tag "${DOCKER_REPO}:latest" \
    --push \
    .

echo "Docker images built and published successfully under:"
echo "  - ${DOCKER_REPO}:${STRIPPED_VERSION}"
echo "  - ${DOCKER_REPO}:latest"

