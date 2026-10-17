# Shared by start.sh, status.sh and stop.sh. Sourced, not run.

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENGREX_HOME="$HOME/.engrex"
SOCKET_PATH="$ENGREX_HOME/daemon.sock"
CONFIG_PATH="$ENGREX_HOME/config.json"

OLLAMA_URL="http://127.0.0.1:11434"
CACHE_PROXY_URL="http://127.0.0.1:11435"
CACHE_DASHBOARD_URL="http://127.0.0.1:11436"
CAPTURE_URL="http://127.0.0.1:7777"

# Plain `engrex-cache serve` by default, set ENGREX_CACHE_FLAGS=--tolerant to opt in
CACHE_FLAGS="${ENGREX_CACHE_FLAGS:-}"

INSTALLED_ENGREX="/usr/local/bin/engrex"
INSTALLED_CACHE="/usr/local/bin/engrex-cache"
APP_PATH="/Applications/EngrexUI.app"

DAEMON_AGENT_LABEL="com.robertkoller.engrex"
CACHE_AGENT_LABEL="com.robertkoller.engrex-cache"

# Patterns match the binary name followed by the subcommand so a tail of daemon.log
# or an editor with the file open never counts
OLLAMA_PATTERN='(^|/)ollama serve'
OLLAMA_RUNNER_PATTERN='(^|/)ollama runner'
CACHE_PATTERN='(^|/)engrex-cache serve'
DAEMON_PATTERN='(^|/)engrex daemon$'
APP_PROCESS="EngrexUI"

if [ -t 1 ]; then
    GREEN=$'\033[32m'
    RED=$'\033[31m'
    YELLOW=$'\033[33m'
    DIM=$'\033[2m'
    RESET=$'\033[0m'
else
    GREEN=""
    RED=""
    YELLOW=""
    DIM=""
    RESET=""
fi

step() {
    printf '%s==>%s %s\n' "$GREEN" "$RESET" "$1"
}

warn() {
    printf '%s!%s %s\n' "$YELLOW" "$RESET" "$1"
}

fail() {
    printf '%sx%s %s\n' "$RED" "$RESET" "$1" >&2
    exit 1
}

ollama_up() {
    curl -s -m 2 "$OLLAMA_URL/api/tags" >/dev/null
}

cache_up() {
    curl -s -m 2 "$CACHE_DASHBOARD_URL/api/stats" >/dev/null
}

# Up means it answers on the socket, not just that the process exists
daemon_up() {
    [ -S "$SOCKET_PATH" ] && daemon_request '{"type":"models"}' | grep -q '"data"'
}

daemon_request() {
    printf '%s\n' "$1" | nc -U -w 2 "$SOCKET_PATH" 2>/dev/null
}

app_up() {
    pgrep -x "$APP_PROCESS" >/dev/null
}

agent_loaded() {
    launchctl list 2>/dev/null | awk '{print $3}' | grep -qx "$1"
}

# Waits up to the given seconds for a check to pass
wait_for() {
    local description="$1"
    local timeout_seconds="$2"
    shift 2
    local waited=0
    while ! "$@"; do
        if [ "$waited" -ge "$timeout_seconds" ]; then
            return 1
        fi
        sleep 1
        waited=$((waited + 1))
    done
    return 0
}

# Sends TERM, waits, then KILL for anything still alive
stop_pattern() {
    local pattern="$1"
    local name="$2"
    if ! pgrep -f "$pattern" >/dev/null; then
        printf '  %s%s not running%s\n' "$DIM" "$name" "$RESET"
        return 0
    fi
    pkill -TERM -f "$pattern"
    local waited=0
    while pgrep -f "$pattern" >/dev/null; do
        if [ "$waited" -ge 10 ]; then
            warn "$name ignored TERM, killing it"
            pkill -KILL -f "$pattern"
            sleep 1
            break
        fi
        sleep 1
        waited=$((waited + 1))
    done
    printf '  %s stopped\n' "$name"
}

# The configured Ollama URL, empty when unset
configured_ollama_url() {
    if [ ! -f "$CONFIG_PATH" ]; then
        return 0
    fi
    sed -n 's/.*"ollama_url"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CONFIG_PATH"
}

# True when a matching process started before the file was last written, meaning it's
# still running old code or an old config
running_older_than() {
    local pattern="$1"
    local file="$2"
    if [ ! -e "$file" ]; then
        return 1
    fi
    local file_time
    file_time=$(stat -f %m "$file")
    local pid
    for pid in $(pgrep -f "$pattern"); do
        local started
        started=$(LC_ALL=C ps -o lstart= -p "$pid" 2>/dev/null)
        if [ -z "$started" ]; then
            continue
        fi
        local start_time
        start_time=$(LC_ALL=C date -j -f "%a %b %d %T %Y" "$started" +%s 2>/dev/null) || continue
        if [ "$start_time" -lt "$file_time" ]; then
            return 0
        fi
    done
    return 1
}

app_down() {
    ! app_up
}
