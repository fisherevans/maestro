#!/usr/bin/env bash
# enable-statusline.sh - opt-in setup for the maestro statusLine in Claude Code.
#
# Deliberately not run by install.sh. The statusLine is a single slot in
# Claude Code's settings.json, so wiring this up replaces whatever you
# already have there (e.g. ccstatusline). Making that automatic would be
# rude. Run this script when you've decided you actually want to swap.
#
# Usage:
#   enable-statusline.sh             # print the snippet, do nothing
#   enable-statusline.sh --apply     # write to ~/.claude/settings.json (with backup)
#   enable-statusline.sh --remove    # delete the statusLine key (use the .bak to restore)
#
# Override the settings file path with $CLAUDE_SETTINGS.

set -euo pipefail

SETTINGS="${CLAUDE_SETTINGS:-$HOME/.claude/settings.json}"
MODE="print"

for arg in "$@"; do
    case "$arg" in
        --apply)  MODE="apply" ;;
        --remove) MODE="remove" ;;
        -h|--help)
            sed -n '1,/^set -euo/p' "$0" | sed -n 's/^# \?//p' | head -n -1
            exit 0
            ;;
        *)
            echo "unknown arg: $arg (use --apply, --remove, or no flag for print)" >&2
            exit 1
            ;;
    esac
done

STATUSLINE_JSON='{"type":"command","command":"maestro statusline","refreshInterval":5}'

if [[ "$MODE" == "print" ]]; then
    cat <<EOF
Add this under the top-level "statusLine" key in $SETTINGS:

  "statusLine": {
    "type": "command",
    "command": "maestro statusline",
    "refreshInterval": 5
  }

This REPLACES whatever statusLine you currently have configured.
To do it automatically (with a timestamped backup of the existing settings), run:

  $0 --apply

To revert, restore from the .bak file or run:

  $0 --remove
EOF
    exit 0
fi

if ! command -v jq >/dev/null 2>&1; then
    echo "error: jq is required for --apply/--remove (brew install jq)" >&2
    exit 1
fi

mkdir -p "$(dirname "$SETTINGS")"
[[ -f "$SETTINGS" ]] || echo '{}' > "$SETTINGS"

backup="$SETTINGS.bak.$(date +%Y%m%dT%H%M%S)"
cp "$SETTINGS" "$backup"
echo "backup: $backup"

tmp="$SETTINGS.tmp"
if [[ "$MODE" == "remove" ]]; then
    jq 'del(.statusLine)' "$SETTINGS" > "$tmp"
    mv "$tmp" "$SETTINGS"
    echo "removed .statusLine from $SETTINGS"
else
    jq --argjson sl "$STATUSLINE_JSON" '.statusLine = $sl' "$SETTINGS" > "$tmp"
    mv "$tmp" "$SETTINGS"
    echo "set .statusLine to maestro in $SETTINGS"
fi

echo "(restart Claude Code or open a new session to pick up the change)"
