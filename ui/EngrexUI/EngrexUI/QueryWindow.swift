import AppKit
import SwiftUI
import UniformTypeIdentifiers

class QueryWindow: NSPanel {

    private let windowWidth: CGFloat = 660
    private let compactHeight: CGFloat = 92

    init() {
        super.init(
            contentRect: NSRect(x: 0, y: 0, width: 660, height: 92),
            styleMask: [.titled, .closable, .resizable, .fullSizeContentView, .nonactivatingPanel],
            backing: .buffered,
            defer: false
        )

        // Normal window level so other apps' windows layer over Engrex normally
        // (instead of it always floating on top). It stays visible when another
        // window is focused — it just no longer forces itself above everything.
        level = .normal
        isMovableByWindowBackground = true
        titlebarAppearsTransparent = true
        titleVisibility = .hidden
        isReleasedWhenClosed = false
        hidesOnDeactivate = false

        // Transparent window so the SwiftUI glass background and rounded corners show.
        isOpaque = false
        backgroundColor = .clear
        hasShadow = true

        let hostingView = NSHostingView(rootView: QueryView(
            onResize: { [weak self] size in
                self?.resize(to: size)
            },
            onAddFile: { [weak self] in
                self?.presentOpenPanel()
            }
        ))

        // Don't let the hosting view drive the window size — we resize manually.
        hostingView.sizingOptions = []

        // CRITICAL: do NOT make the NSHostingView the window's direct contentView.
        // Doing so couples SwiftUI's internal auto-layout constraints to the window,
        // and every window frame change while the content updates (i.e. every search)
        // re-triggers a constraints pass until it overflows and throws
        // NSGenericException. Instead, host it inside a plain NSView pinned with an
        // autoresizing mask (springs, not constraints), which decouples the two.
        let container = NSView(frame: NSRect(x: 0, y: 0, width: 660, height: 92))
        container.wantsLayer = true
        container.layer?.cornerRadius = 20
        container.layer?.masksToBounds = true

        hostingView.translatesAutoresizingMaskIntoConstraints = true
        hostingView.autoresizingMask = [.width, .height]
        hostingView.frame = container.bounds
        container.addSubview(hostingView)

        contentView = container
    }

    override var canBecomeKey: Bool { true }
    override var canBecomeMain: Bool { true }

    // ESC closes the window. The window no longer auto-closes on focus loss — it
    // stays visible and simply layers behind whatever you click, so ESC (or the
    // menu bar / hotkey re-open) is how you dismiss it.
    override func cancelOperation(_ sender: Any?) {
        close()
    }

    // Presents a file picker as a sheet on this panel and copies the chosen files
    // into ~/Engrex/ for the daemon to ingest.
    private func presentOpenPanel() {
        let panel = NSOpenPanel()
        panel.canChooseFiles = true
        panel.canChooseDirectories = false
        panel.allowsMultipleSelection = true
        panel.prompt = "Add"
        panel.message = "Choose files to ingest into Engrex"

        var allowedTypes: [UTType] = [.plainText, .html, .pdf]
        if let markdown = UTType(filenameExtension: "md") {
            allowedTypes.append(markdown)
        }
        panel.allowedContentTypes = allowedTypes

        panel.beginSheetModal(for: self) { response in
            if response == .OK {
                for url in panel.urls {
                    FileIngestor.addAndNotify(url)
                }
            }
        }
    }

    // Resizes the window, keeping the top edge and horizontal center fixed so it
    // grows around the search bar rather than jumping around the screen.
    //
    // Deliberately NOT animated at the window level: an animated frame change calls
    // setFrameSize repeatedly, and each call makes NSHostingView re-mark the window
    // as needing another constraints pass. Those pile up past the view count and
    // crash with an NSGenericException. A single setFrame does it once. The SwiftUI
    // content still animates internally via withAnimation, so motion is preserved.
    //
    // Dispatched async so the frame change happens after the current SwiftUI update
    // pass rather than reentering layout mid-update.
    private func resize(to size: CGSize) {
        DispatchQueue.main.async { [weak self] in
            guard let self = self else { return }
            var newFrame = self.frame
            let topEdge = newFrame.origin.y + newFrame.height
            let centerX = newFrame.origin.x + newFrame.width / 2
            newFrame.size = size
            newFrame.origin.y = topEdge - size.height
            newFrame.origin.x = centerX - size.width / 2
            self.setFrame(newFrame, display: true)
        }
    }

    func showAndFocus() {
        // Reset to compact size and position in the upper third, Spotlight-style.
        positionCompact()

        alphaValue = 0
        makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)

        if let contentLayer = contentView?.layer {
            contentLayer.removeAnimation(forKey: "entrance")
            let scale = CABasicAnimation(keyPath: "transform.scale")
            scale.fromValue = 0.96
            scale.toValue = 1.0
            scale.duration = 0.22
            scale.timingFunction = CAMediaTimingFunction(name: .easeOut)
            contentLayer.add(scale, forKey: "entrance")
        }

        NSAnimationContext.runAnimationGroup { context in
            context.duration = 0.18
            context.timingFunction = CAMediaTimingFunction(name: .easeOut)
            animator().alphaValue = 1
        }
    }

    private func positionCompact() {
        let screenFrame = (screen ?? NSScreen.main)?.visibleFrame ?? NSRect(x: 0, y: 0, width: 1440, height: 900)
        let originX = screenFrame.midX - windowWidth / 2
        // Place the top edge a bit above vertical center for a Spotlight-like feel.
        let topEdge = screenFrame.midY + screenFrame.height * 0.20
        let originY = topEdge - compactHeight
        setFrame(NSRect(x: originX, y: originY, width: windowWidth, height: compactHeight), display: true)
    }
}
