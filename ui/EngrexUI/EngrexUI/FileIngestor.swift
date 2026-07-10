import Foundation

// Sends a chosen file to the daemon over the socket. The daemon copies it into
// ~/Engrex, ingests it, and records the original path as the chunk's origin — so
// the local copy is no longer done here.
enum FileIngestor {

    private static let socketClient = SocketClient()

    static func addAndNotify(_ sourceURL: URL) {
        let name = sourceURL.lastPathComponent
        socketClient.sendAddFile(path: sourceURL.path) { error in
            if let error = error {
                ToastPresenter.shared.show(
                    message: "Couldn't add file: \(error.localizedDescription)",
                    systemImage: "xmark.octagon.fill",
                    isError: true
                )
            } else {
                ToastPresenter.shared.show(
                    message: "Added \(name) for ingestion",
                    systemImage: "doc.badge.plus"
                )
            }
        }
    }
}
