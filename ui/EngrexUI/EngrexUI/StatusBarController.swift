import AppKit
import SwiftUI

class StatusBarController {
    private let statusItem: NSStatusItem
    private let statusMenu = NSMenu()
    private var queryWindow: QueryWindow?
    private var pollingTimer: Timer?
    private let socketClient = SocketClient()

    init() {
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        setupButton()
        setupMenu()
        startPolling()
    }

    private func setupButton() {
        applyIcon(color: .systemRed)
    }

    // Renders the menu bar symbol in an explicit color. On macOS 26 the menu bar
    // ignores contentTintColor for template symbols, so we bake the color into the
    // image with a palette symbol configuration instead.
    private func applyIcon(color: NSColor) {
        guard let button = statusItem.button else { return }
        let configuration = NSImage.SymbolConfiguration(paletteColors: [color])
        if let base = NSImage(systemSymbolName: "brain.head.profile", accessibilityDescription: "Engrex"),
           let colored = base.withSymbolConfiguration(configuration) {
            colored.isTemplate = false
            button.image = colored
            button.title = ""
        } else {
            button.image = nil
            button.attributedTitle = NSAttributedString(
                string: "◉",
                attributes: [.foregroundColor: color]
            )
        }
    }

    private func setupMenu() {
        let openItem = NSMenuItem(title: "Open Query Window", action: #selector(openQueryWindow), keyEquivalent: "k")
        openItem.target = self
        statusMenu.addItem(openItem)

        statusMenu.addItem(.separator())

        let statusHeader = NSMenuItem(title: "Daemon: checking…", action: nil, keyEquivalent: "")
        statusHeader.isEnabled = false
        statusMenu.addItem(statusHeader)

        statusMenu.addItem(.separator())

        let themeParent = NSMenuItem(title: "Theme", action: nil, keyEquivalent: "")
        themeParent.submenu = makeThemeMenu()
        statusMenu.addItem(themeParent)

        statusMenu.addItem(.separator())

        let quitItem = NSMenuItem(title: "Quit Engrex", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        statusMenu.addItem(quitItem)

        // Assigning the menu makes clicking the status item show it automatically.
        statusItem.menu = statusMenu
    }

    private func makeThemeMenu() -> NSMenu {
        let menu = NSMenu()
        let current = UserDefaults.standard.integer(forKey: Themes.storageKey)
        for theme in Themes.all {
            let item = NSMenuItem(title: theme.name, action: #selector(selectTheme(_:)), keyEquivalent: "")
            item.target = self
            item.tag = theme.id
            item.image = swatchImage(for: theme)
            item.state = (theme.id == current) ? .on : .off
            menu.addItem(item)
        }
        return menu
    }

    @objc private func selectTheme(_ sender: NSMenuItem) {
        UserDefaults.standard.set(sender.tag, forKey: Themes.storageKey)
        if let siblings = sender.menu?.items {
            for item in siblings {
                item.state = (item.tag == sender.tag) ? .on : .off
            }
        }
    }

    // Draws a small rounded gradient chip representing a theme for the menu items.
    private func swatchImage(for theme: Theme) -> NSImage {
        let size = NSSize(width: 16, height: 16)
        let image = NSImage(size: size)
        image.lockFocus()
        let rect = NSRect(origin: .zero, size: size).insetBy(dx: 1, dy: 1)
        let path = NSBezierPath(roundedRect: rect, xRadius: 4, yRadius: 4)
        let colors = theme.colors.map { NSColor($0) }
        if let gradient = NSGradient(colors: colors) {
            gradient.draw(in: path, angle: 0)
        }
        image.unlockFocus()
        image.isTemplate = false
        return image
    }

    func setDaemonRunning(_ running: Bool) {
        DispatchQueue.main.async {
            self.applyIcon(color: running ? .systemGreen : .systemRed)
            // The daemon status line is the third item (index 2).
            if self.statusMenu.items.count > 2 {
                self.statusMenu.items[2].title = running ? "Daemon: running" : "Daemon: stopped"
            }
        }
    }

    private func startPolling() {
        let timer = Timer.scheduledTimer(withTimeInterval: 5.0, repeats: true) { [weak self] _ in
            guard let self = self else { return }
            self.setDaemonRunning(self.socketClient.isDaemonRunning())
        }
        RunLoop.main.add(timer, forMode: .common)
        timer.fire()
        pollingTimer = timer
    }

    @objc func openQueryWindow() {
        // Recreate fresh each time so the panel always opens compact and empty.
        queryWindow?.close()
        let window = QueryWindow()
        queryWindow = window
        window.showAndFocus()
    }
}
