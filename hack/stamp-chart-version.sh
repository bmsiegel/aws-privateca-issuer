#!/usr/bin/env bash
set -euo pipefail

VERSION="${1:?usage: stamp-chart-version.sh vX.Y.Z}"
CHART="charts/aws-pca-issuer/Chart.yaml"

if [[ ! "${VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "error: version must look like v1.9.3, got '${VERSION}'" >&2
  exit 1
fi

sed -i.bak -E "s/^version: .*/version: ${VERSION}/" "${CHART}"
sed -i.bak -E "s/^appVersion: .*/appVersion: ${VERSION}/" "${CHART}"
rm -f "${CHART}.bak"

grep -E "^(version|appVersion):" "${CHART}"
