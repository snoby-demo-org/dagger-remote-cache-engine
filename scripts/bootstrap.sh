#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
PIN=a45241b20a5e07fbfc5954479ef0b61da843499c
DEST="$ROOT/.upstream/dagger"
if [ ! -d "$DEST/.git" ]; then
  git clone --depth 1 --branch v1.0.0-beta.25 https://github.com/dagger/dagger.git "$DEST"
fi
ACTUAL=$(git -C "$DEST" rev-parse HEAD)
if [ "$ACTUAL" != "$PIN" ]; then
  printf 'Refusing incompatible upstream revision: %s\n' "$ACTUAL" >&2
  exit 1
fi
printf 'Pinned Dagger source verified: %s\n' "$ACTUAL"
