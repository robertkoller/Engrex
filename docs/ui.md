# The Swift Menu-Bar App

The macOS app lives in `ui/EngrexUI/` (an Xcode project). It's a menu-bar-only agent
(no dock icon) that gives Engrex a native front end: global hotkeys and a
Spotlight-style query window. It talks to the daemon over the same Unix socket the
CLI uses.

## Components

| File | Role |
|---|---|
| `EngrexUIApp.swift` / `AppDelegate.swift` | Entry point; sets the app as a background agent, wires up the pieces |
| `StatusBarController.swift` | The menu-bar icon (green = daemon up, red = down), the menu, and the Theme submenu |
| `HotkeyManager.swift` | Registers the global hotkeys via Carbon |
| `AccessibilityReader.swift` | Reads the selected text for ⌘⇧B (AX API, with a clipboard fallback) |
| `SocketClient.swift` | POSIX Unix-socket client; speaks the daemon's JSON/stream protocol |
| `QueryWindow.swift` | The floating panel (`NSPanel`) — sizing, positioning, entrance animation |
| `QueryView.swift` | The SwiftUI content — search bar, streaming answer, sources panel, upload zone, chips |
| `VisualEffectView.swift` | Bridges `NSVisualEffectView` for the glass background |
| `ModelSetting.swift` | Which generation model queries use; persists the Deep thinking toggle and fetches model names from the daemon |
| `Theme.swift` | The color presets |
| `ToastPresenter.swift` | The floating "Saved to Engrex" HUD |
| `FileIngestor.swift` | Sends dropped/picked files to the daemon's `addfile` command |

## Hotkeys

- **⌘⇧B — capture selection.** Reads the highlighted text anywhere and sends it to the
  daemon as a `hotkey` add. Shows a toast.
- **⌘⇧Space — open the query window.** A floating glass panel that starts compact
  (just a search bar) and grows as it shows an answer.

## The query window

- Starts as a compact search bar in the upper third of the screen.
- On submit, it expands and streams the answer in, with an animated "thinking"
  indicator. If the answer has sources, a **sources panel** slides in beside it —
  each row opens the original file or URL.
- **Themes:** pick a color preset from the menu-bar Theme submenu; it recolors the
  whole UI and persists (`@AppStorage`).
- **File upload:** the 📄 button opens an upload mode — a big drag-and-drop / click-to-
  browse zone. Dropped files go to the daemon's `addfile` command (with origin).
- **Query tags:** typing `--date` or `--source` turns them into chips that get sent
  along with the query so the answer cites dates/sources.
- **Dismiss:** Esc closes it. It uses a normal window level, so other windows layer
  over it naturally instead of it always floating on top.

## Two things that bit us (and how they're solved)

These are documented in the code comments too, but worth knowing:

- **The window crash.** Making an `NSHostingView` the window's direct `contentView`
  couples SwiftUI's auto-layout to the window; resizing during content updates threw
  `NSGenericException`. Fix: host the SwiftUI view inside a plain `NSView` pinned with
  an autoresizing mask, and resize the window with a single non-animated `setFrame`
  dispatched async.
- **Focus stealing.** The file-browse dialog and (formerly) the click-away dismissal
  fought with drag-and-drop. The window now stays open while you go to Finder to grab
  a file.

## The Deep thinking toggle

A checkable menu item (⌘D) routing queries to a slower, more capable model. It sends
`model` on the `query` command, so switching takes effect on the next question with no
daemon restart.

A single toggle rather than a model picker: the choice people actually make is "answer
fast" versus "think harder", not which weights to load. The names come from the
daemon's `models` command (`ModelSetting.refresh`), so the app hardcodes nothing — and
if that fetch fails, it sends an empty model and the daemon uses its default.

The setting persists in `UserDefaults` under `engrex.useDeepModel`.

## Window reuse

The query panel is created once and reused. `openQueryWindow` previously destroyed and
rebuilt it on every hotkey press, which threw away the SwiftUI `@State` holding the
question, answer, and sources — so glancing at something else and reopening lost the
results you were returning to.

Two consequences of reusing it:

- `showAndFocus` positions the panel only on its **first** appearance. Re-running
  `positionCompact` would squash a window full of results back to the bare search bar,
  and it would also undo any position you dragged it to.
- `.onAppear` now fires only once ever, so the search field would never regain focus.
  The window posts `engrexQueryWindowDidShow` on every show and `QueryView` listens for
  it. Existing text is selected on reopen, so typing starts a new question while the
  previous answer stays until you submit.

## Building the app

Terminal, no Xcode GUI needed (Xcode must still be installed — `xcodebuild` ships with
it):

```bash
make app          # Release build → bin/app/Build/Products/Release/EngrexUI.app
make app-debug    # faster, skips optimization
make app-install  # quits the running app, replaces /Applications/EngrexUI.app
make launch       # build + install + run in the FOREGROUND (Ctrl-C quits)
make app-run      # same but detached
make app-clean
```

`launch` executes the binary inside the bundle rather than `open`-ing the `.app` —
`open` hands off to LaunchServices and returns, leaving the app detached with no way to
stop it from that terminal. It runs the `/Applications` copy because macOS ties
accessibility and input-monitoring permissions to a bundle's **path**; running the
build-directory copy would re-prompt for them and leave the granted ones pointing at an
app you aren't using.

Or open `ui/EngrexUI/EngrexUI.xcodeproj` in Xcode and Run. The project uses Xcode's
file-system-synchronized groups, so new `.swift` files in
`ui/EngrexUI/EngrexUI/` are picked up automatically (bring Xcode to the foreground or
reopen the project if a freshly added file isn't found). The app must be **de-
sandboxed** (`ENABLE_APP_SANDBOX = NO`) for global hotkeys, accessibility, and socket
access, and it needs Accessibility permission granted for ⌘⇧B.

## Keeping the icon in the menu bar

**`make install` has nothing to do with this app.** It builds and installs the Go binary
(`/usr/local/bin/engrex`) only — the Makefile never touches Xcode or
`/Applications/EngrexUI.app`. If the menu-bar icon disappears after a `make install`,
the two are unrelated: the app simply isn't running.

The app is `LSUIElement: true`, so it has **no Dock icon and no window** — the menu-bar
icon is the only sign it's alive. When it's not running there is nothing to notice.

```bash
open -a EngrexUI                 # start it now
pgrep -fl EngrexUI               # confirm it's running
```

It does **not** start at login by default. To make it permanent:

```bash
osascript -e 'tell application "System Events" to make login item \
  at end with properties {path:"/Applications/EngrexUI.app", hidden:true}'
```

Or System Settings → General → Login Items → **+** → `EngrexUI`. Verify with:

```bash
osascript -e 'tell application "System Events" to get the name of every login item'
```

The app is only a client — it needs `engrex daemon` running to do anything, and the
daemon needs Ollama. For the full boot-survival chain see
[mcp.md](mcp.md#permanent-setup-surviving-a-reboot); that section is written for MCP, but
the Ollama → daemon ordering applies to this app too.
