import AppKit
import SwiftUI

// Shows a brief glass "toast" HUD near the top of the screen and auto-dismisses it.
// Used to confirm a hotkey capture without requiring notification permissions.
final class ToastPresenter {
    static let shared = ToastPresenter()

    private var panel: NSPanel?
    private var dismissWorkItem: DispatchWorkItem?

    func show(message: String, systemImage: String = "checkmark.circle.fill", isError: Bool = false) {
        DispatchQueue.main.async {
            self.present(message: message, systemImage: systemImage, isError: isError)
        }
    }

    private func present(message: String, systemImage: String, isError: Bool) {
        dismissWorkItem?.cancel()
        panel?.close()

        let hostingView = NSHostingView(
            rootView: ToastView(message: message, systemImage: systemImage, isError: isError)
        )
        let size = hostingView.fittingSize

        let newPanel = NSPanel(
            contentRect: NSRect(origin: .zero, size: size),
            styleMask: [.borderless, .nonactivatingPanel],
            backing: .buffered,
            defer: false
        )
        newPanel.isOpaque = false
        newPanel.backgroundColor = .clear
        newPanel.hasShadow = true
        newPanel.level = .statusBar
        newPanel.ignoresMouseEvents = true
        newPanel.contentView = hostingView

        if let screen = NSScreen.main {
            let visible = screen.visibleFrame
            let originX = visible.midX - size.width / 2
            let originY = visible.maxY - size.height - 80
            newPanel.setFrameOrigin(NSPoint(x: originX, y: originY))
        }

        newPanel.alphaValue = 0
        newPanel.orderFrontRegardless()
        NSAnimationContext.runAnimationGroup { context in
            context.duration = 0.2
            context.timingFunction = CAMediaTimingFunction(name: .easeOut)
            newPanel.animator().alphaValue = 1
        }
        panel = newPanel

        let work = DispatchWorkItem { [weak self] in self?.dismiss() }
        dismissWorkItem = work
        DispatchQueue.main.asyncAfter(deadline: .now() + 1.7, execute: work)
    }

    private func dismiss() {
        guard let current = panel else { return }
        panel = nil
        NSAnimationContext.runAnimationGroup({ context in
            context.duration = 0.3
            context.timingFunction = CAMediaTimingFunction(name: .easeIn)
            current.animator().alphaValue = 0
        }, completionHandler: {
            current.close()
        })
    }
}

private struct ToastView: View {
    let message: String
    let systemImage: String
    let isError: Bool

    var body: some View {
        HStack(spacing: 10) {
            Image(systemName: systemImage)
                .font(.system(size: 18, weight: .semibold))
                .foregroundStyle(isError ? AnyShapeStyle(Color.red) : AnyShapeStyle(Themes.current.gradient))
            Text(message)
                .font(.system(size: 15, weight: .medium))
                .foregroundStyle(.primary)
        }
        .padding(.horizontal, 20)
        .padding(.vertical, 14)
        .background(
            RoundedRectangle(cornerRadius: 14, style: .continuous)
                .fill(.ultraThinMaterial)
        )
        .overlay(
            RoundedRectangle(cornerRadius: 14, style: .continuous)
                .strokeBorder(Color.white.opacity(0.15), lineWidth: 1)
        )
        .fixedSize()
    }
}
