import AppKit

class AppDelegate: NSObject, NSApplicationDelegate {
    private var statusBarController: StatusBarController?
    private var hotkeyManager: HotkeyManager?

    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.setActivationPolicy(.accessory)

        AccessibilityReader.requestPermission()

        statusBarController = StatusBarController()
        hotkeyManager = HotkeyManager(statusBarController: statusBarController!)
        hotkeyManager?.registerHotkeys()
    }

    func applicationWillTerminate(_ notification: Notification) {
        hotkeyManager?.unregisterHotkeys()
    }
}
