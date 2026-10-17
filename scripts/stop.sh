#!/usr/bin/env bash
# Stops the app, the daemon, the cache and Ollama, however they were started.
#
#   scripts/stop.sh               stop everything
#   scripts/stop.sh --keep-ollama leave Ollama running

set -uo pipefail
source "$(dirname "$0")/common.sh"

keep_ollama=false
for argument in "$@"; do
    case "$argument" in
        --keep-ollama) keep_ollama=true ;;
        -h|--help)
            sed -n '2,5p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *) fail "unknown option: $argument" ;;
    esac
done

# Launchd agents have KeepAlive, so they'd respawn whatever gets killed below
for label in "$DAEMON_AGENT_LABEL" "$CACHE_AGENT_LABEL"; do
    if agent_loaded "$label"; then
        launchctl unload "$HOME/Library/LaunchAgents/$label.plist" || true
        printf '  unloaded launchd agent %s\n' "$label"
    fi
done

step "Menu bar app"
if app_up; then
    osascript -e "quit app \"$APP_PROCESS\"" >/dev/null 2>&1 || true
    sleep 1
    if app_up; then
        pkill -x "$APP_PROCESS" || true
    fi
    printf '  app stopped\n'
else
    printf '  %sapp not running%s\n' "$DIM" "$RESET"
fi

step "Daemon"
stop_pattern "$DAEMON_PATTERN" "daemon"

step "Cache"
stop_pattern "$CACHE_PATTERN" "cache"

step "Ollama"
if $keep_ollama; then
    printf '  %sleft running%s\n' "$DIM" "$RESET"
else
    stop_pattern "$OLLAMA_PATTERN" "ollama"
    # Model runners normally exit with the server, this catches orphans
    if pgrep -f "$OLLAMA_RUNNER_PATTERN" >/dev/null; then
        pkill -TERM -f "$OLLAMA_RUNNER_PATTERN" || true
    fi
    # The desktop app restarts its own server, so quit it too if present
    if pgrep -x "Ollama" >/dev/null; then
        osascript -e 'quit app "Ollama"' >/dev/null 2>&1 || true
        printf '  Ollama.app quit\n'
    fi
fi
