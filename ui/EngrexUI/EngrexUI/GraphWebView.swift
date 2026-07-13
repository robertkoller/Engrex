import SwiftUI
import WebKit

// Embeds the graph web UI (served by the daemon on localhost:7778) inline inside the
// query window via WKWebView. Recreated each time graph mode opens, so it loads fresh
// with the current theme and data.
struct GraphWebView: NSViewRepresentable {
    let url: URL

    func makeCoordinator() -> Coordinator {
        Coordinator()
    }

    func makeNSView(context: Context) -> WKWebView {
        let configuration = WKWebViewConfiguration()
        // Bridge: the web page posts a source path/URL here and we open it natively.
        configuration.userContentController.add(context.coordinator, name: "openSource")

        let webView = WKWebView(frame: .zero, configuration: configuration)
        // Make the web view transparent so the app's glass background shows through,
        // exactly like the rest of the window. drawsBackground is the KVC toggle for it.
        webView.setValue(false, forKey: "drawsBackground")
        webView.underPageBackgroundColor = .clear
        webView.load(URLRequest(url: url))
        return webView
    }

    func updateNSView(_ nsView: WKWebView, context: Context) {}

    // Receives "openSource" messages from the page and opens the file or URL.
    class Coordinator: NSObject, WKScriptMessageHandler {
        func userContentController(_ userContentController: WKUserContentController, didReceive message: WKScriptMessage) {
            guard message.name == "openSource",
                  let path = message.body as? String,
                  !path.isEmpty else {
                return
            }
            if path.hasPrefix("http://") || path.hasPrefix("https://") {
                if let webURL = URL(string: path) {
                    NSWorkspace.shared.open(webURL)
                }
            } else {
                NSWorkspace.shared.open(URL(fileURLWithPath: path))
            }
        }
    }
}
