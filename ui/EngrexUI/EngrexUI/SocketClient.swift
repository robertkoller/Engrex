import Foundation
import Darwin

// Talks to the Engrex Go daemon over its Unix domain socket at ~/.engrex/daemon.sock.
// Uses raw POSIX sockets because Network.framework's Unix socket support is unreliable.
// Wire protocol (matches the Go daemon):
//   - client writes one JSON command object followed by a newline
//   - for "add"/"delete": daemon writes one JSON response ({} or {"error": "..."}) then closes
//   - for "query": daemon streams raw answer text, then closes the connection
class SocketClient {
    private let socketPath: String

    init() {
        let home = FileManager.default.homeDirectoryForCurrentUser.path
        socketPath = "\(home)/.engrex/daemon.sock"
    }

    func isDaemonRunning() -> Bool {
        return FileManager.default.fileExists(atPath: socketPath)
    }

    func sendAdd(text: String, completion: @escaping (Error?) -> Void) {
        DispatchQueue.global(qos: .userInitiated).async {
            let result = self.runRequestReturningData(command: [
                "type": "add", "text": text, "source": "hotkey"
            ])
            let error = self.interpretResponse(result)
            DispatchQueue.main.async { completion(error) }
        }
    }

    func sendDelete(spec: String, completion: @escaping (Error?) -> Void) {
        DispatchQueue.global(qos: .userInitiated).async {
            let result = self.runRequestReturningData(command: [
                "type": "delete", "text": spec
            ])
            let error = self.interpretResponse(result)
            DispatchQueue.main.async { completion(error) }
        }
    }

    // Sends the original file path to the daemon; the daemon copies it into ~/Engrex,
    // ingests it, and records the original path as the chunk's origin.
    func sendAddFile(path: String, completion: @escaping (Error?) -> Void) {
        DispatchQueue.global(qos: .userInitiated).async {
            let result = self.runRequestReturningData(command: [
                "type": "addfile", "text": path
            ])
            let error = self.interpretResponse(result)
            DispatchQueue.main.async { completion(error) }
        }
    }

    // The daemon sends the source file list as a JSON first line, then streams the
    // answer text. onSources fires once with the parsed sources; onToken fires for
    // each subsequent chunk of answer text.
    func sendQuery(
        text: String,
        onSources: @escaping ([String]) -> Void,
        onToken: @escaping (String) -> Void,
        onComplete: @escaping (Error?) -> Void
    ) {
        DispatchQueue.global(qos: .userInitiated).async {
            guard let fileDescriptor = self.openConnection() else {
                DispatchQueue.main.async { onComplete(SocketError.connectionFailed) }
                return
            }
            defer { close(fileDescriptor) }

            guard let payload = self.encode(["type": "query", "text": text]) else {
                DispatchQueue.main.async { onComplete(SocketError.encodingFailed) }
                return
            }
            if !self.writeAll(fileDescriptor, payload) {
                DispatchQueue.main.async { onComplete(SocketError.writeFailed) }
                return
            }

            var headerParsed = false
            var headerBuffer = Data()
            var buffer = [UInt8](repeating: 0, count: 4096)

            while true {
                let bytesRead = read(fileDescriptor, &buffer, buffer.count)
                if bytesRead <= 0 { break }
                let slice = buffer[0..<bytesRead]

                if headerParsed {
                    let chunk = String(decoding: slice, as: UTF8.self)
                    DispatchQueue.main.async { onToken(chunk) }
                    continue
                }

                // Still reading the JSON header line — split on the first newline.
                if let newlineIndex = slice.firstIndex(of: 0x0A) {
                    headerBuffer.append(contentsOf: slice[slice.startIndex..<newlineIndex])
                    let sources = self.parseSources(headerBuffer)
                    DispatchQueue.main.async { onSources(sources) }
                    headerParsed = true

                    let afterIndex = slice.index(after: newlineIndex)
                    if afterIndex < slice.endIndex {
                        let rest = String(decoding: slice[afterIndex..<slice.endIndex], as: UTF8.self)
                        DispatchQueue.main.async { onToken(rest) }
                    }
                } else {
                    headerBuffer.append(contentsOf: slice)
                }
            }
            DispatchQueue.main.async { onComplete(nil) }
        }
    }

    private func parseSources(_ data: Data) -> [String] {
        guard let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let sources = json["sources"] as? [String] else {
            return []
        }
        return sources
    }

    // Sends a command and reads the full response body until the daemon closes the connection.
    private func runRequestReturningData(command: [String: String]) -> Result<Data, Error> {
        guard let fileDescriptor = openConnection() else {
            return .failure(SocketError.connectionFailed)
        }
        defer { close(fileDescriptor) }

        guard let payload = encode(command) else {
            return .failure(SocketError.encodingFailed)
        }
        if !writeAll(fileDescriptor, payload) {
            return .failure(SocketError.writeFailed)
        }

        var received = Data()
        var buffer = [UInt8](repeating: 0, count: 4096)
        while true {
            let bytesRead = read(fileDescriptor, &buffer, buffer.count)
            if bytesRead <= 0 { break }
            received.append(contentsOf: buffer[0..<bytesRead])
        }
        return .success(received)
    }

    // Turns a raw JSON response into an error (or nil for success).
    private func interpretResponse(_ result: Result<Data, Error>) -> Error? {
        switch result {
        case .failure(let error):
            return error
        case .success(let data):
            if data.isEmpty { return nil }
            if let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
               let message = json["error"] as? String, !message.isEmpty {
                return SocketError.daemonError(message)
            }
            return nil
        }
    }

    private func encode(_ command: [String: String]) -> Data? {
        guard var data = try? JSONSerialization.data(withJSONObject: command) else { return nil }
        data.append(0x0A) // newline
        return data
    }

    // Opens and connects a Unix domain socket, returning the file descriptor.
    private func openConnection() -> Int32? {
        let fileDescriptor = socket(AF_UNIX, SOCK_STREAM, 0)
        if fileDescriptor < 0 { return nil }

        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)

        let pathBytes = socketPath.utf8CString
        let capacity = MemoryLayout.size(ofValue: address.sun_path)
        if pathBytes.count > capacity {
            close(fileDescriptor)
            return nil
        }
        withUnsafeMutablePointer(to: &address.sun_path) { pointer in
            pointer.withMemoryRebound(to: CChar.self, capacity: capacity) { destination in
                pathBytes.withUnsafeBufferPointer { source in
                    destination.update(from: source.baseAddress!, count: pathBytes.count)
                }
            }
        }

        let connectResult = withUnsafePointer(to: &address) { pointer -> Int32 in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) { sockaddrPointer in
                connect(fileDescriptor, sockaddrPointer, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        if connectResult < 0 {
            close(fileDescriptor)
            return nil
        }
        return fileDescriptor
    }

    // Writes the entire buffer, looping until all bytes are sent.
    private func writeAll(_ fileDescriptor: Int32, _ data: Data) -> Bool {
        return data.withUnsafeBytes { rawBuffer -> Bool in
            guard let base = rawBuffer.bindMemory(to: UInt8.self).baseAddress else { return false }
            var totalWritten = 0
            while totalWritten < rawBuffer.count {
                let written = write(fileDescriptor, base + totalWritten, rawBuffer.count - totalWritten)
                if written <= 0 { return false }
                totalWritten += written
            }
            return true
        }
    }
}

enum SocketError: LocalizedError {
    case connectionFailed
    case encodingFailed
    case writeFailed
    case daemonError(String)

    var errorDescription: String? {
        switch self {
        case .connectionFailed:
            return "Could not connect to the Engrex daemon. Is it running?"
        case .encodingFailed:
            return "Failed to encode command."
        case .writeFailed:
            return "Failed to send command to the daemon."
        case .daemonError(let message):
            return message
        }
    }
}
