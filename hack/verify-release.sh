#!/usr/bin/env bash
set -euo pipefail

COMMITLINT_VERSION=21.2.3
RENOVATE_VERSION=44.115.0
SEMANTIC_RELEASE_VERSION=25.0.9

REPO_ROOT=$(git rev-parse --show-toplevel)
ACTIONLINT=${ACTIONLINT:-actionlint}
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

cd "$REPO_ROOT"

echo "==> actionlint"
"$ACTIONLINT" -shellcheck= -pyflakes= -ignore 'the runner of ".+" action is too old' .github/workflows/*.yml

echo "==> installing node tools"
npm install --prefix "$WORK/node" --no-audit --no-fund --silent \
  "@commitlint/cli@$COMMITLINT_VERSION" \
  "@commitlint/config-conventional@$COMMITLINT_VERSION" \
  "renovate@$RENOVATE_VERSION" \
  "semantic-release@$SEMANTIC_RELEASE_VERSION"
BIN="$WORK/node/node_modules/.bin"

echo "==> renovate config"
"$BIN/renovate-config-validator" .github/renovate.json

echo "==> commitlint config"
cp .commitlintrc.json "$WORK/node/"
echo "fix: sample message" | (cd "$WORK/node" && "$BIN/commitlint")
if echo "sample message" | (cd "$WORK/node" && "$BIN/commitlint") >/dev/null 2>&1; then
  echo "commitlint accepted a non-conventional message" >&2
  exit 1
fi

echo "==> semantic-release dry run"
git clone --quiet --bare "$REPO_ROOT" "$WORK/remote.git"
git -C "$WORK/remote.git" update-ref refs/heads/main "$(git rev-parse HEAD)"
git clone --quiet --branch main "$WORK/remote.git" "$WORK/repo"
cp .releaserc.json "$WORK/repo/"
(
  cd "$WORK/repo"
  env -u GITHUB_ACTIONS -u GITHUB_TOKEN -u GH_TOKEN "$BIN/semantic-release" \
    --dry-run --no-ci \
    --repository-url "file://$WORK/remote.git" \
    --plugins @semantic-release/commit-analyzer,@semantic-release/release-notes-generator
) | tee "$WORK/semantic-release.log"
grep -qE "The next release version is|There are no relevant changes" "$WORK/semantic-release.log"

echo "==> release config OK"
