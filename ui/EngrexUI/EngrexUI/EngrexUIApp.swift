import SwiftUI

@main
struct EngrexUIApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) var appDelegate

    var body: some Scene {
        // No windows — this is a menu bar only app.
        Settings { EmptyView() }
    }
}
