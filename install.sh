#!/usr/bin/env bash
# install.sh - install or upgrade the maestro CLI and Claude Code skill.
#
# Idempotent: rerun after `git pull` to upgrade. The CLI binary is rebuilt
# in place. The skill is installed as a symlink, so edits to skill/SKILL.md
# in this repo are picked up immediately without a re-run.
#
# Overrides:
#   BIN_DIR     where to put the maestro binary (default: $HOME/bin)
#   SKILL_LINK  where to install the skill symlink (default: $HOME/.claude/skills/maestro)

set -euo pipefail

SOURCE_DIR=$(cd "$(dirname "$0")" && pwd)
SKILL_SRC="$SOURCE_DIR/skill"
SKILL_LINK="${SKILL_LINK:-$HOME/.claude/skills/maestro}"
BIN_DIR="${BIN_DIR:-$HOME/bin}"
BIN_PATH="$BIN_DIR/maestro"

require() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "error: $1 not found on PATH" >&2
        exit 1
    fi
}

require go

echo "==> building CLI"
mkdir -p "$BIN_DIR"
( cd "$SOURCE_DIR" && go build -o "$BIN_PATH" ./cmd/maestro )
echo "    $BIN_PATH"

echo "==> installing skill"
mkdir -p "$(dirname "$SKILL_LINK")"

if [[ -L "$SKILL_LINK" ]]; then
    current=$(readlink "$SKILL_LINK")
    if [[ "$current" == "$SKILL_SRC" ]]; then
        echo "    symlink already correct: $SKILL_LINK"
    else
        echo "    replacing symlink (was -> $current)"
        rm "$SKILL_LINK"
        ln -s "$SKILL_SRC" "$SKILL_LINK"
        echo "    $SKILL_LINK -> $SKILL_SRC"
    fi
elif [[ -e "$SKILL_LINK" ]]; then
    echo "error: $SKILL_LINK exists and is not a symlink." >&2
    echo "       move or remove it manually, then re-run install.sh." >&2
    exit 1
else
    ln -s "$SKILL_SRC" "$SKILL_LINK"
    echo "    $SKILL_LINK -> $SKILL_SRC"
fi

echo
echo "==> done"

case ":$PATH:" in
    *":$BIN_DIR:"*) ;;
    *)
        echo "note: $BIN_DIR is not on your PATH."
        echo "      add it to your shell profile, e.g.:"
        echo "        echo 'export PATH=\"$BIN_DIR:\$PATH\"' >> ~/.zshrc"
        ;;
esac

echo "verify CLI: maestro --help"
echo "skill takes effect after restarting Claude Code"
