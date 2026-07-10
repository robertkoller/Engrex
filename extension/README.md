# Engrex Capture (browser extension)

Saves the selected text — or the whole page if nothing is selected — to Engrex,
along with the page URL and title. Posts to the daemon's local endpoint at
`http://127.0.0.1:7777/capture`, so the Engrex daemon must be running.

## Install (unpacked, dev mode)

Chrome / Brave / Edge:

1. Go to `chrome://extensions` (or `brave://extensions`, `edge://extensions`).
2. Turn on **Developer mode** (top-right).
3. Click **Load unpacked** and select this `extension/` folder.
4. The Engrex icon appears in the toolbar.

## Use

- **Keyboard:** select text on any page and press **⌘⇧E** (Ctrl+Shift+E on
  Windows/Linux). With nothing selected, it captures the whole page.
- **Click:** click the toolbar icon to capture.
- Feedback appears as a badge on the icon: **✓** saved, **∅** nothing to capture,
  **!** failed (usually means the daemon isn't running).

## Change the shortcut

Go to `chrome://extensions/shortcuts` to rebind the capture key if ⌘⇧E conflicts.

## Requirements

- The Engrex daemon must be running (`engrex daemon`, or via launchd).
- The daemon listens on `127.0.0.1:7777` — local only, nothing is exposed to the network.
