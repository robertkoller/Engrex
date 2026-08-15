import AppKit
import SwiftUI

class StatusBarController {
    private let statusItem: NSStatusItem
    private let statusMenu = NSMenu()
    private var queryWindow: QueryWindow?
    private var pollingTimer: Timer?
    private var deepModelItem: NSMenuItem?
    private let socketClient = SocketClient()

    init() {
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        setupButton()
        setupMenu()
        startPolling()
        loadModelNames()
    }

    // Asks the daemon which models it offers so the toggle can name the deep one. The
    // menu is already usable before this returns — the toggle just reads "Deep thinking"
    // until the daemon answers, and an unnamed model is sent as empty, which the daemon
    // treats as its default.
    private func loadModelNames() {
        ModelSetting.shared.refresh(using: socketClient)
        DispatchQueue.main.asyncAfter(deadline: .now() + 1.5) { [weak self] in
            self?.refreshDeepModelLabel()
        }
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

        // Checked = route queries to the slower, more capable model. Kept as a single
        // toggle rather than a model picker because the choice users actually make is
        // "answer fast" vs "think harder", not which weights to load.
        deepModelItem = NSMenuItem(title: ModelSetting.shared.deepModelLabel,
                                   action: #selector(toggleDeepModel(_:)),
                                   keyEquivalent: "d")
        deepModelItem?.target = self
        deepModelItem?.state = ModelSetting.shared.useDeepModel ? .on : .off
        deepModelItem?.toolTip = "Slower, but better at questions about which document "
            + "something came from. Off uses the faster default model."
        if let deepModelItem {
            statusMenu.addItem(deepModelItem)
        }

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

    @objc private func toggleDeepModel(_ sender: NSMenuItem) {
        ModelSetting.shared.useDeepModel.toggle()
        sender.state = ModelSetting.shared.useDeepModel ? .on : .off
    }

    // Called once the daemon has reported its model names, so the menu item can show
    // which model "deep thinking" actually means.
    func refreshDeepModelLabel() {
        deepModelItem?.title = ModelSetting.shared.deepModelLabel
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
        // Reuse the existing panel rather than rebuilding it. The window owns the
        // SwiftUI view whose @State holds the question, answer, and sources, so
        // recreating it silently threw all of that away — reopening after glancing at
        // something else lost the results you were coming back to.
        if let window = queryWindow {
            window.showAndFocus()
            return
        }
        let window = QueryWindow()
        queryWindow = window
        window.showAndFocus()
    }
}
