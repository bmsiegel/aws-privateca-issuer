#!/usr/bin/env bash
set -euo pipefail

REPOSITORY=${1:?usage: check-latest.sh <repository> <tag>}
TAG=${2:?usage: check-latest.sh <repository> <tag>}

latest=$(docker buildx imagetools inspect --raw "$REPOSITORY:latest" | sha256sum | cut -d' ' -f1)
tagged=$(docker buildx imagetools inspect --raw "$REPOSITORY:$TAG" | sha256sum | cut -d' ' -f1)

if [[ "$latest" != "$tagged" ]]; then
  echo "$REPOSITORY:latest is sha256:$latest but $REPOSITORY:$TAG is sha256:$tagged" >&2
  exit 1
fi
echo "$REPOSITORY:latest and :$TAG are both sha256:$latest"
