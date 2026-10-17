#!/usr/bin/env bash
# Builds everything, then brings up Ollama, the cache, the daemon and the menu bar app.
# Everything runs in the background with logs in ~/.engrex. Stop it all with stop.sh.
#
#   scripts/start.sh             build, install if changed, start
#   scripts/start.sh --no-build  start with what's already installed

set -euo pipefail
source "$(dirname "$0")/common.sh"

build=true
for argument in "$@"; do
    case "$argument" in
        --no-build) build=false ;;
        -h|--help)
            sed -n '2,7p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *) fail "unknown option: $argument" ;;
    esac
done

mkdir -p "$ENGREX_HOME"
cd "$REPO_DIR"

# Launchd copies would respawn as soon as they're killed and fight over the socket
for label in "$DAEMON_AGENT_LABEL" "$CACHE_AGENT_LABEL"; do
    if agent_loaded "$label"; then
        warn "unloading launchd agent $label so this script owns the process"
        launchctl unload "$HOME/Library/LaunchAgents/$label.plist" || true
    fi
done

if $build; then
    step "Building engrex and engrex-cache"
    make compile cache
    if ! cmp -s bin/engrex "$INSTALLED_ENGREX" || ! cmp -s bin/engrex-cache "$INSTALLED_CACHE"; then
        step "Installing to /usr/local/bin (needs sudo)"
        make install
    else
        printf '  %sinstalled binaries already match the build%s\n' "$DIM" "$RESET"
    fi

    step "Building and installing the menu bar app"
    make app-install
elif [ ! -x "$INSTALLED_ENGREX" ] || [ ! -x "$INSTALLED_CACHE" ]; then
    fail "nothing installed yet, run without --no-build first"
fi

step "Ollama"
if ollama_up; then
    printf '  already running\n'
else
    command -v ollama >/dev/null || fail "ollama not found on PATH (brew install ollama)"
    nohup ollama serve >>"$ENGREX_HOME/ollama.log" 2>&1 &
    disown
    wait_for "ollama" 30 ollama_up || fail "ollama didn't come up, see $ENGREX_HOME/ollama.log"
    printf '  started\n'
fi

step "Cache"
if running_older_than "$CACHE_PATTERN" "$INSTALLED_CACHE"; then
    printf '  running an older binary, restarting\n'
    stop_pattern "$CACHE_PATTERN" "cache"
fi
if cache_up; then
    printf '  already running\n'
else
    # shellcheck disable=SC2086
    nohup "$INSTALLED_CACHE" serve $CACHE_FLAGS >>"$ENGREX_HOME/cache.log" 2>&1 &
    disown
    wait_for "cache" 30 cache_up || fail "cache didn't come up, see $ENGREX_HOME/cache.log"
    printf '  started (%s)\n' "${CACHE_FLAGS:-strict}"
fi
if [ "$(configured_ollama_url)" != "$CACHE_PROXY_URL" ]; then
    "$INSTALLED_CACHE" enable >/dev/null
    printf '  pointed engrex at the cache\n'
fi

step "Daemon"
# The daemon reads its config once at startup, so a newer binary or config means a restart
if running_older_than "$DAEMON_PATTERN" "$INSTALLED_ENGREX"; then
    printf '  running an older binary, restarting\n'
    stop_pattern "$DAEMON_PATTERN" "daemon"
elif running_older_than "$DAEMON_PATTERN" "$CONFIG_PATH"; then
    printf '  config changed since it started, restarting\n'
    stop_pattern "$DAEMON_PATTERN" "daemon"
fi
if daemon_up; then
    printf '  already running\n'
else
    if pgrep -f "$DAEMON_PATTERN" >/dev/null; then
        warn "a daemon process exists but isn't answering, replacing it"
        stop_pattern "$DAEMON_PATTERN" "daemon"
    fi
    nohup "$INSTALLED_ENGREX" daemon >>"$ENGREX_HOME/daemon.log" 2>&1 &
    disown
    wait_for "daemon" 30 daemon_up || fail "daemon didn't come up, see $ENGREX_HOME/daemon.log"
    printf '  started\n'
fi

step "Menu bar app"
if app_up && running_older_than "^$APP_PATH/Contents/MacOS/$APP_PROCESS" "$APP_PATH/Contents/MacOS/$APP_PROCESS"; then
    printf '  running an older build, restarting\n'
    osascript -e "quit app \"$APP_PROCESS\"" >/dev/null 2>&1 || true
    wait_for "app to quit" 10 app_down || pkill -x "$APP_PROCESS" || true
fi
if app_up; then
    printf '  already running\n'
elif [ -d "$APP_PATH" ]; then
    open "$APP_PATH"
    printf '  opened\n'
else
    warn "$APP_PATH not installed, run without --no-build"
fi

echo
"$(dirname "$0")/status.sh" || true
