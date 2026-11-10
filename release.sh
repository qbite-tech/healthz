#!/usr/bin/env bash

set -euo pipefail

usage() {
  cat >&2 <<EOF
Usage: $0 <major|minor|patch|vX.Y.Z|list>

Creates and pushes an annotated semver tag from main.
Use list (or --list) to show the 10 most recent semver tags.

Examples:
  $0 patch
  $0 minor
  $0 v1.4.0
  $0 list
EOF
}

if [[ $# -ne 1 ]]; then
  usage
  exit 2
fi

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(git -C "$script_dir" rev-parse --show-toplevel)"
cd "$repo_root"

semver_pattern='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'

semver_tags() {
  local candidate

  while IFS= read -r candidate; do
    if [[ "$candidate" =~ $semver_pattern ]]; then
      printf '%s\n' "$candidate"
    fi
  done < <(git tag --list 'v*' --sort=-version:refname)
}

action="$1"
if [[ "$action" == "list" || "$action" == "--list" ]]; then
  mapfile -t tags < <(semver_tags)
  if [[ ${#tags[@]} -eq 0 ]]; then
    echo "No semver tags found."
  else
    printf '%s\n' "${tags[@]:0:10}"
  fi
  exit 0
fi

branch="$(git branch --show-current)"
if [[ "$branch" != "main" ]]; then
  echo "error: releases must be created from main (currently on ${branch:-a detached HEAD})" >&2
  exit 1
fi

if [[ -n "$(git status --porcelain)" ]]; then
  echo "error: the working tree is not clean; commit or stash changes first" >&2
  exit 1
fi

if ! git fetch --quiet --tags origin; then
  echo "error: could not fetch tags from origin" >&2
  exit 1
fi

case "$action" in
  major | minor | patch)
    mapfile -t tags < <(semver_tags)
    latest="${tags[0]:-v0.0.0}"
    [[ "$latest" =~ $semver_pattern ]]
    major="${BASH_REMATCH[1]}"
    minor="${BASH_REMATCH[2]}"
    patch="${BASH_REMATCH[3]}"

    case "$action" in
      major)
        ((major += 1))
        minor=0
        patch=0
        ;;
      minor)
        ((minor += 1))
        patch=0
        ;;
      patch)
        ((patch += 1))
        ;;
    esac
    tag="v${major}.${minor}.${patch}"
    ;;
  *)
    tag="$action"
    if [[ ! "$tag" =~ $semver_pattern ]]; then
      echo "error: expected major, minor, patch, vX.Y.Z, list, or --list" >&2
      usage
      exit 2
    fi
    ;;
esac

if git show-ref --verify --quiet "refs/tags/$tag"; then
  echo "error: tag $tag already exists locally" >&2
  exit 1
fi

if ! remote_tags="$(git ls-remote --tags origin "refs/tags/$tag")"; then
  echo "error: could not check tags on origin" >&2
  exit 1
fi

if [[ -n "$remote_tags" ]]; then
  echo "error: tag $tag already exists on origin" >&2
  exit 1
fi

git tag --annotate "$tag" --message "Release $tag"

if ! git push origin "$tag"; then
  echo "error: failed to push $tag; the tag still exists locally" >&2
  echo "retry with: git push origin $tag" >&2
  exit 1
fi

echo "Released $tag. The pushed tag triggers the release workflow, which publishes the binaries."
