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

## Building the app

Open `ui/EngrexUI/EngrexUI.xcodeproj` in Xcode and Run. The project uses Xcode's
file-system-synchronized groups, so new `.swift` files in
`ui/EngrexUI/EngrexUI/` are picked up automatically (bring Xcode to the foreground or
reopen the project if a freshly added file isn't found). The app must be **de-
sandboxed** (`ENABLE_APP_SANDBOX = NO`) for global hotkeys, accessibility, and socket
access, and it needs Accessibility permission granted for ⌘⇧B.
