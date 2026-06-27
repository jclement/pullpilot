#!/usr/bin/env bash
# Guess the next semver from the latest tag, confirm, then tag and push.
# Pushing the tag triggers the release workflow (binaries + :stable images +
# production worker deploy).
set -euo pipefail

git fetch --tags --quiet
latest="$(git tag --list 'v*' --sort=-v:refname | head -n1)"
latest="${latest:-v0.0.0}"
IFS=. read -r major minor patch <<<"${latest#v}"

bump="${1:-patch}"
case "$bump" in
  major) next="v$((major + 1)).0.0" ;;
  minor) next="v${major}.$((minor + 1)).0" ;;
  patch) next="v${major}.${minor}.$((patch + 1))" ;;
  *) echo "usage: release.sh [major|minor|patch]" >&2; exit 1 ;;
esac

read -rp "Current ${latest} → tag ${next}? [y/N] " ok
[[ "${ok:-}" == "y" || "${ok:-}" == "Y" ]] || { echo "aborted"; exit 1; }

git tag -a "$next" -m "$next"
git push origin "$next"
echo "Pushed $next — the release workflow will build binaries + :stable images and deploy the worker to production."
