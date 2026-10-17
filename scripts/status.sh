#!/usr/bin/env bash
# Shows whether each piece is up. Exits 1 if anything is down.

set -uo pipefail
source "$(dirname "$0")/common.sh"

all_up=true

row() {
    local name="$1"
    local state="$2"
    local detail="$3"
    local color="$GREEN"
    if [ "$state" = "down" ]; then
        color="$RED"
        all_up=false
    elif [ "$state" = "-" ]; then
        color="$DIM"
    elif [ "$state" != "up" ]; then
        color="$YELLOW"
    fi
    printf '%-10s %s%-5s%s %s\n' "$name" "$color" "$state" "$RESET" "$detail"
}

json_field() {
    sed -n "s/.*\"$1\":\"\\{0,1\\}\\([^\",}]*\\).*/\\1/p"
}

if ollama_up; then
    loaded=$(curl -s -m 2 "$OLLAMA_URL/api/ps" | grep -o '"name":"[^"]*"' | cut -d'"' -f4 | tr '\n' ' ')
    row "ollama" "up" "$OLLAMA_URL  loaded: ${loaded:-none}"
else
    row "ollama" "down" "$OLLAMA_URL"
fi

if cache_up; then
    stats=$(curl -s -m 2 "$CACHE_DASHBOARD_URL/api/stats")
    hits=$(printf '%s' "$stats" | json_field hits)
    misses=$(printf '%s' "$stats" | json_field misses)
    entries=$(printf '%s' "$stats" | json_field entries)
    row "cache" "up" "$CACHE_PROXY_URL  ${hits:-?} hits, ${misses:-?} misses, ${entries:-?} entries"
else
    row "cache" "down" "$CACHE_PROXY_URL"
fi

# A setting, not a service: where config.json tells the daemon to send model calls
route=$(configured_ollama_url)
if [ "$route" = "$CACHE_PROXY_URL" ]; then
    route_detail="config: engrex -> cache -> ollama"
elif [ -z "$route" ]; then
    route_detail="config: engrex -> ollama directly, cache skipped"
else
    route_detail="config: engrex -> $route"
fi
if [ "$route" = "$CACHE_PROXY_URL" ] && ! cache_up && daemon_up; then
    row "route" "warn" "$route_detail, but the cache is down so queries will fail"
else
    row "route" "-" "$route_detail"
fi

if daemon_up; then
    models=$(daemon_request '{"type":"models"}')
    default_model=$(printf '%s' "$models" | json_field default)
    deep_model=$(printf '%s' "$models" | json_field deep)
    cloud_model=$(printf '%s' "$models" | json_field cloud)
    pid=$(pgrep -f "$DAEMON_PATTERN" | head -1)
    row "daemon" "up" "pid ${pid:-?}  default ${default_model}, deep ${deep_model}, gemini ${cloud_model:-off (no key)}"
elif pgrep -f "$DAEMON_PATTERN" >/dev/null; then
    row "daemon" "down" "process exists but the socket isn't answering"
else
    row "daemon" "down" "$SOCKET_PATH"
fi

# Any answer at all means it's listening, GET on /capture is a 405
if curl -s -m 2 -o /dev/null "$CAPTURE_URL/capture"; then
    row "capture" "up" "$CAPTURE_URL (browser extension)"
else
    row "capture" "down" "$CAPTURE_URL (browser extension)"
fi

if app_up; then
    row "app" "up" "pid $(pgrep -x "$APP_PROCESS" | head -1)"
else
    row "app" "down" "$APP_PATH"
fi

stale=""
if running_older_than "$CACHE_PATTERN" "$INSTALLED_CACHE"; then
    stale="$stale cache"
fi
if running_older_than "$DAEMON_PATTERN" "$INSTALLED_ENGREX" || running_older_than "$DAEMON_PATTERN" "$CONFIG_PATH"; then
    stale="$stale daemon"
fi
if running_older_than "^$APP_PATH/Contents/MacOS/$APP_PROCESS" "$APP_PATH/Contents/MacOS/$APP_PROCESS"; then
    stale="$stale app"
fi
if [ -n "$stale" ]; then
    row "stale" "warn" "running old code or config:$stale (start.sh restarts them)"
fi

if [ -f "$REPO_DIR/bin/engrex" ] && [ -f "$INSTALLED_ENGREX" ]; then
    if cmp -s "$REPO_DIR/bin/engrex" "$INSTALLED_ENGREX"; then
        row "binary" "up" "installed engrex matches bin/"
    else
        row "binary" "warn" "installed engrex differs from bin/, run start.sh to install"
    fi
fi

$all_up
