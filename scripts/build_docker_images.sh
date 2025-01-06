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
        echo "Using Docker config from DOCKER_CONFIG"
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

# echo "Creating manifest for version ${STRIPPED_VERSION}..."
# docker manifest create "${DOCKER_REPO}:${STRIPPED_VERSION}" \
#     "${DOCKER_REPO}:${STRIPPED_VERSION}"

# echo "Creating manifest for latest..."
# docker manifest create "${DOCKER_REPO}:latest" \
#     "${DOCKER_REPO}:latest"

# for PLATFORM in "${PLATFORMS[@]}"; do
#     GOOS="${PLATFORM%/*}"
#     GOARCH="${PLATFORM#*/}"
    
#     if [[ "$PLATFORM" == *"arm/v7"* ]]; then
#         GOARCH="arm"
#         VARIANT="v7"
#     else
#         VARIANT=""
#     fi

#     echo "Adding manifest annotation for ${PLATFORM}..."
#     if [[ -n "$VARIANT" ]]; then
#         docker manifest annotate "${DOCKER_REPO}:${STRIPPED_VERSION}" \
#             "${DOCKER_REPO}:${STRIPPED_VERSION}" \
#             --os "${GOOS}" --arch "${GOARCH}" --variant "${VARIANT}"
        
#         docker manifest annotate "${DOCKER_REPO}:latest" \
#             "${DOCKER_REPO}:latest" \
#             --os "${GOOS}" --arch "${GOARCH}" --variant "${VARIANT}"
#     else
#         docker manifest annotate "${DOCKER_REPO}:${STRIPPED_VERSION}" \
#             "${DOCKER_REPO}:${STRIPPED_VERSION}" \
#             --os "${GOOS}" --arch "${GOARCH}"
        
#         docker manifest annotate "${DOCKER_REPO}:latest" \
#             "${DOCKER_REPO}:latest" \
#             --os "${GOOS}" --arch "${GOARCH}"
#     fi
# done

echo "Pushing manifests..."
docker manifest push "${DOCKER_REPO}:${STRIPPED_VERSION}"
docker manifest push "${DOCKER_REPO}:latest"

echo "Docker images built and published successfully!"