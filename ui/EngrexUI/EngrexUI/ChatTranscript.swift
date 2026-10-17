import SwiftUI
import AppKit

// The conversation so far. User messages sit on the right in a tinted bubble, answers
// on the left as markdown with their sources as pills underneath
struct ChatTranscript: View {
    let messages: [ChatMessage]
    let streamingMessageID: UUID?
    let theme: Theme

    // The opening question is already in the search bar above, so it isn't repeated
    private var shownMessages: [ChatMessage] {
        if messages.first?.role == .user {
            return Array(messages.dropFirst())
        }
        return messages
    }

    var body: some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 18) {
                    ForEach(shownMessages) { message in
                        switch message.role {
                        case .user:
                            userBubble(message)
                        case .assistant:
                            answer(message)
                        }
                    }
                    Color.clear.frame(height: 1).id("bottom")
                }
                .padding(20)
            }
            .frame(maxWidth: .infinity)
            .background(
                RoundedRectangle(cornerRadius: 16, style: .continuous)
                    .fill(Color.black.opacity(0.18))
            )
            .overlay(
                RoundedRectangle(cornerRadius: 16, style: .continuous)
                    .strokeBorder(theme.softGradient, lineWidth: 1)
            )
            .onChange(of: messages) { _, _ in
                withAnimation(.easeOut(duration: 0.15)) {
                    proxy.scrollTo("bottom", anchor: .bottom)
                }
            }
            .onAppear {
                proxy.scrollTo("bottom", anchor: .bottom)
            }
        }
    }

    private func userBubble(_ message: ChatMessage) -> some View {
        HStack {
            Spacer(minLength: 80)
            Text(message.text)
                .font(.system(size: 15))
                .foregroundStyle(.primary)
                .textSelection(.enabled)
                .padding(.horizontal, 14)
                .padding(.vertical, 9)
                .background(
                    RoundedRectangle(cornerRadius: 14, style: .continuous)
                        .fill(theme.softGradient)
                )
                .overlay(
                    RoundedRectangle(cornerRadius: 14, style: .continuous)
                        .strokeBorder(theme.gradient.opacity(0.35), lineWidth: 1)
                )
        }
        .transition(.move(edge: .bottom).combined(with: .opacity))
    }

    private func answer(_ message: ChatMessage) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            if message.text.isEmpty && message.id == streamingMessageID {
                Text("Thinking…")
                    .font(.system(size: 15))
                    .foregroundStyle(.secondary)
                    .frame(maxWidth: .infinity, alignment: .leading)
            } else {
                MarkdownText(markdown: message.text, fontSize: 16)
                    .foregroundStyle(.primary)
            }

            if !message.sources.isEmpty {
                ScrollView(.horizontal, showsIndicators: false) {
                    HStack(spacing: 8) {
                        ForEach(message.sources, id: \.self) { source in
                            sourcePill(source)
                        }
                    }
                }
                .transition(.opacity)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func sourcePill(_ source: String) -> some View {
        let name = SourceLink.displayName(for: source)
        return Button {
            SourceLink.open(source)
        } label: {
            HStack(spacing: 6) {
                Image(systemName: SourceLink.iconName(for: source))
                    .font(.system(size: 12))
                    .foregroundStyle(theme.gradient)
                Text(name)
                    .font(.system(size: 12))
                    .foregroundStyle(.primary)
                    .lineLimit(1)
                    .truncationMode(.middle)
            }
            .padding(.horizontal, 11)
            .padding(.vertical, 6)
            .background(Capsule().fill(Color.white.opacity(0.06)))
            .overlay(Capsule().strokeBorder(Color.white.opacity(0.12), lineWidth: 1))
            .frame(maxWidth: 220)
            .contentShape(Capsule())
        }
        .buttonStyle(.plain)
        .help("Open \(name)")
    }
}

// How a source path or URL is named, drawn and opened
enum SourceLink {
    static func isWebURL(_ source: String) -> Bool {
        return source.hasPrefix("http://") || source.hasPrefix("https://")
    }

    static func displayName(for source: String) -> String {
        if isWebURL(source) {
            return URL(string: source)?.host ?? source
        }
        return (source as NSString).lastPathComponent
    }

    static func open(_ source: String) {
        if isWebURL(source) {
            if let url = URL(string: source) {
                NSWorkspace.shared.open(url)
            }
        } else {
            NSWorkspace.shared.open(URL(fileURLWithPath: source))
        }
    }

    static func iconName(for source: String) -> String {
        if isWebURL(source) {
            return "globe"
        }
        switch (source as NSString).pathExtension.lowercased() {
        case "pdf":
            return "doc.richtext"
        case "docx":
            return "doc.text.fill"
        case "md":
            return "text.alignleft"
        case "html", "htm":
            return "globe"
        default:
            return "doc.text"
        }
    }
}
