import SwiftUI
import AppKit
import UniformTypeIdentifiers

struct QueryView: View {
    var onResize: (CGSize) -> Void = { _ in }
    var onAddFile: () -> Void = {}
    var onUploadModeChanged: (Bool) -> Void = { _ in }

    @AppStorage(Themes.storageKey) private var themeIndex: Int = 0

    @State private var queryText: String = ""
    @State private var followUpText: String = ""
    @State private var messages: [ChatMessage] = []
    @State private var streamingMessageID: UUID?
    @State private var lastActivity = Date()
    @State private var isUploadMode: Bool = false
    @State private var isGraphMode: Bool = false
    @State private var isDropTargeted: Bool = false
    @State private var tags: [String] = []
    @FocusState private var isFieldFocused: Bool
    @FocusState private var isFollowUpFocused: Bool

    private let socketClient = SocketClient()

    private let compactSize = CGSize(width: 660, height: 92)
    // The window grows down by followUpGrowth for each follow-up, up to maxChatHeight
    private let chatWidth: CGFloat = 660
    private let firstAnswerHeight: CGFloat = 540
    private let followUpGrowth: CGFloat = 140
    private let maxChatHeight: CGFloat = 820
    private let uploadSize = CGSize(width: 640, height: 560)
    private let graphSize = CGSize(width: 940, height: 660)

    // Flags that become chips when typed. Key is the raw flag, value is the chip label.
    private let knownFlags: [(flag: String, label: String)] = [
        ("--date", "date"),
        ("--source", "source")
    ]

    // A conversation left alone this long is dropped on the next reopen, so the window
    // doesn't come back to a stale chat. Nothing is saved across restarts either way
    private let idleResetInterval: TimeInterval = 30 * 60

    private var theme: Theme { Themes.theme(for: themeIndex) }

    private var isChatting: Bool { !messages.isEmpty }

    private var isStreaming: Bool { streamingMessageID != nil }

    private var chatSize: CGSize {
        let followUps = max(0, messages.filter { $0.role == .user }.count - 1)
        let height = min(maxChatHeight, firstAnswerHeight + CGFloat(followUps) * followUpGrowth)
        return CGSize(width: chatWidth, height: height)
    }

    var body: some View {
        ZStack {
            background

            VStack(alignment: .leading, spacing: 14) {
                // The search bar keeps the question that started the conversation, and
                // follow-ups are typed in the composer under the answers
                searchBar

                if isUploadMode {
                    dropZone
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                        .transition(.opacity.combined(with: .move(edge: .top)))
                } else if isGraphMode {
                    graphView
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                        .transition(.opacity.combined(with: .move(edge: .top)))
                } else if isChatting {
                    ChatTranscript(messages: messages, streamingMessageID: streamingMessageID, theme: theme)
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                        .transition(.asymmetric(
                            insertion: .move(edge: .top).combined(with: .opacity),
                            removal: .opacity
                        ))
                    composer
                        .transition(.move(edge: .bottom).combined(with: .opacity))
                }
            }
            .padding(18)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .animation(.spring(response: 0.4, dampingFraction: 0.85), value: isChatting)
        .animation(.spring(response: 0.4, dampingFraction: 0.85), value: isUploadMode)
        .animation(.spring(response: 0.4, dampingFraction: 0.85), value: isGraphMode)
        .onAppear {
            focusFieldShortly()
        }
        // The window is reused between hotkey presses so previous results survive,
        // which means onAppear fires only once. This is what refocuses the field on
        // every reopen.
        .onReceive(NotificationCenter.default.publisher(for: .engrexQueryWindowDidShow)) { _ in
            if isChatting && !isStreaming && Date().timeIntervalSince(lastActivity) > idleResetInterval {
                clear()
            }
            focusFieldShortly()
        }
    }

    // Delay so the window has become key before requesting focus — otherwise the focus
    // request is dropped and the "color in" animation never fires.
    private func focusFieldShortly() {
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.15) {
            withAnimation(.easeInOut(duration: 0.35)) {
                isFieldFocused = true
            }
        }
    }

    private var background: some View {
        ZStack {
            VisualEffectView(material: .hudWindow)
            theme.softGradient
            LinearGradient(
                colors: [Color.white.opacity(0.08), .clear],
                startPoint: .top,
                endPoint: .center
            )
        }
        .ignoresSafeArea()
    }

    private var searchBar: some View {
        HStack(spacing: 12) {
            toolButtons

            Divider()
                .frame(height: 22)

            Image(systemName: "sparkle.magnifyingglass")
                .font(.system(size: 18, weight: .semibold))
                .foregroundStyle(isFieldFocused ? AnyShapeStyle(theme.gradient) : AnyShapeStyle(Color.secondary))
                .animation(.easeInOut(duration: 0.25), value: isFieldFocused)

            ForEach(tags, id: \.self) { flag in
                chipView(flag)
            }

            TextField("Ask your second brain…", text: $queryText)
                .textFieldStyle(.plain)
                .font(.system(size: 20, weight: .regular))
                .focused($isFieldFocused)
                .onChange(of: queryText) { _, newValue in
                    let remaining = extractTags(from: newValue)
                    if remaining != newValue {
                        queryText = remaining
                    }
                }
                .onSubmit { submitQuery() }

            if isStreaming {
                ThinkingIndicator(gradient: theme.gradient)
                    .transition(.opacity)
            } else if !queryText.isEmpty || !tags.isEmpty {
                Button {
                    clear()
                } label: {
                    Image(systemName: "xmark.circle.fill")
                        .font(.system(size: 16))
                        .foregroundStyle(.secondary)
                }
                .buttonStyle(.plain)
                .transition(.opacity.combined(with: .scale))
            }
        }
        .padding(.horizontal, 18)
        .padding(.vertical, 14)
        .background(
            RoundedRectangle(cornerRadius: 16, style: .continuous)
                .fill(.ultraThinMaterial)
        )
        .overlay(
            RoundedRectangle(cornerRadius: 16, style: .continuous)
                .strokeBorder(
                    isFieldFocused ? AnyShapeStyle(theme.gradient) : AnyShapeStyle(Color.white.opacity(0.12)),
                    lineWidth: isFieldFocused ? 1.5 : 1
                )
        )
        .shadow(color: isFieldFocused ? theme.glowColor.opacity(0.35) : .clear, radius: 14)
        .animation(.easeInOut(duration: 0.25), value: isFieldFocused)
        .animation(.easeInOut(duration: 0.2), value: queryText.isEmpty)
        .animation(.easeInOut(duration: 0.2), value: isStreaming)
        .animation(.spring(response: 0.3, dampingFraction: 0.7), value: tags)
    }

    // Upload, folder and graph. Shared by the search bar and the chat composer
    private var toolButtons: some View {
        HStack(spacing: 12) {
            Button {
                setUploadMode(!isUploadMode)
            } label: {
                Image(systemName: isUploadMode ? "xmark" : "doc.badge.plus")
                    .font(.system(size: 16, weight: .semibold))
                    .foregroundStyle(isUploadMode ? AnyShapeStyle(theme.gradient) : AnyShapeStyle(Color.secondary))
            }
            .buttonStyle(.plain)
            .help(isUploadMode ? "Cancel" : "Add a file to ingest")

            Button {
                openEngrexFolder()
            } label: {
                Image(systemName: "folder")
                    .font(.system(size: 15, weight: .semibold))
                    .foregroundStyle(.secondary)
            }
            .buttonStyle(.plain)
            .help("Open the Engrex folder")

            Button {
                setGraphMode(!isGraphMode)
            } label: {
                Image(systemName: isGraphMode ? "xmark" : "point.3.connected.trianglepath.dotted")
                    .font(.system(size: 15, weight: .semibold))
                    .foregroundStyle(isGraphMode ? AnyShapeStyle(theme.gradient) : AnyShapeStyle(Color.secondary))
            }
            .buttonStyle(.plain)
            .help(isGraphMode ? "Close the graph" : "Open the knowledge graph")
        }
    }

    // The follow-up input under the answers, laid out like the AI_Chat template: a
    // growing text box with a toolbar under it. The tools stay in the search bar above
    private var composer: some View {
        VStack(alignment: .leading, spacing: 0) {
            TextField("Ask a follow-up…", text: $followUpText, axis: .vertical)
                .textFieldStyle(.plain)
                .font(.system(size: 16, weight: .regular))
                .lineLimit(1...6)
                .focused($isFollowUpFocused)
                .onChange(of: followUpText) { _, newValue in
                    let remaining = extractTags(from: newValue)
                    if remaining != newValue {
                        followUpText = remaining
                    }
                }
                // Return sends and Shift-Return adds a line, as in the template
                .onKeyPress(.return, phases: .down) { press in
                    if press.modifiers.contains(.shift) {
                        followUpText += "\n"
                        return .handled
                    }
                    submitFollowUp()
                    return .handled
                }
                .onSubmit { submitFollowUp() }
                .padding(.horizontal, 16)
                .padding(.top, 14)
                .padding(.bottom, 6)

            HStack(spacing: 12) {
                Button {
                    clear()
                } label: {
                    HStack(spacing: 4) {
                        Image(systemName: "plus")
                            .font(.system(size: 11, weight: .bold))
                        Text("New chat")
                            .font(.system(size: 12, weight: .medium))
                    }
                    .foregroundStyle(.secondary)
                    .padding(.horizontal, 9)
                    .padding(.vertical, 5)
                    .overlay(
                        RoundedRectangle(cornerRadius: 8, style: .continuous)
                            .strokeBorder(Color.white.opacity(0.18), style: StrokeStyle(lineWidth: 1, dash: [3, 3]))
                    )
                    .contentShape(RoundedRectangle(cornerRadius: 8, style: .continuous))
                }
                .buttonStyle(.plain)
                .help("Start a new conversation")

                Spacer()

                sendButton
            }
            .padding(.horizontal, 14)
            .padding(.vertical, 10)
        }
        .background(
            RoundedRectangle(cornerRadius: 16, style: .continuous)
                .fill(.ultraThinMaterial)
        )
        .overlay(
            RoundedRectangle(cornerRadius: 16, style: .continuous)
                .strokeBorder(
                    isFollowUpFocused ? AnyShapeStyle(theme.gradient) : AnyShapeStyle(Color.white.opacity(0.12)),
                    lineWidth: isFollowUpFocused ? 1.5 : 1
                )
        )
        .shadow(color: isFollowUpFocused ? theme.glowColor.opacity(0.35) : .clear, radius: 14)
        .animation(.easeInOut(duration: 0.25), value: isFollowUpFocused)
        .animation(.easeInOut(duration: 0.2), value: followUpText.isEmpty)
        .animation(.easeInOut(duration: 0.2), value: isStreaming)
    }

    private var canSend: Bool {
        !isStreaming && !followUpText.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }

    // Fills with the theme gradient once there's something to send, and turns into the
    // thinking dots while an answer streams
    @ViewBuilder
    private var sendButton: some View {
        if isStreaming {
            ThinkingIndicator(gradient: theme.gradient)
                .frame(width: 30, height: 30)
                .transition(.opacity)
        } else {
            Button {
                submitFollowUp()
            } label: {
                Image(systemName: "arrow.up")
                    .font(.system(size: 13, weight: .bold))
                    .foregroundStyle(canSend ? AnyShapeStyle(Color.white) : AnyShapeStyle(Color.secondary))
                    .frame(width: 30, height: 30)
                    .background(
                        RoundedRectangle(cornerRadius: 8, style: .continuous)
                            .fill(canSend ? AnyShapeStyle(theme.gradient) : AnyShapeStyle(Color.clear))
                    )
                    .overlay(
                        RoundedRectangle(cornerRadius: 8, style: .continuous)
                            .strokeBorder(Color.white.opacity(canSend ? 0 : 0.18), lineWidth: 1)
                    )
                    .contentShape(RoundedRectangle(cornerRadius: 8, style: .continuous))
            }
            .buttonStyle(.plain)
            .disabled(!canSend)
            .help("Send")
            .transition(.opacity)
        }
    }

    private func chipView(_ flag: String) -> some View {
        let label = knownFlags.first(where: { $0.flag == flag })?.label ?? flag
        return HStack(spacing: 5) {
            Text(label)
                .font(.system(size: 13, weight: .semibold))
            Image(systemName: "xmark")
                .font(.system(size: 9, weight: .bold))
        }
        .foregroundStyle(.white)
        .padding(.horizontal, 10)
        .padding(.vertical, 5)
        .background(Capsule().fill(theme.gradient))
        .contentShape(Capsule())
        .onTapGesture {
            tags.removeAll { $0 == flag }
        }
        .transition(.scale.combined(with: .opacity))
    }

    // Turns completed flag tokens (flag followed by a space) into chips and returns the
    // text without them, leaving partially typed flags alone. Both inputs share the chips
    private func extractTags(from text: String) -> String {
        var remaining = text
        for known in knownFlags {
            let token = known.flag + " "
            while let range = remaining.range(of: token) {
                if !tags.contains(known.flag) {
                    tags.append(known.flag)
                }
                remaining.removeSubrange(range)
            }
        }
        return remaining
    }

    private var graphView: some View {
        Group {
            if let url = graphURL {
                GraphWebView(url: url)
            } else {
                Color.black.opacity(0.18)
            }
        }
        .clipShape(RoundedRectangle(cornerRadius: 18, style: .continuous))
        .overlay(
            RoundedRectangle(cornerRadius: 18, style: .continuous)
                .strokeBorder(theme.softGradient, lineWidth: 1)
        )
    }

    private var dropZone: some View {
        VStack(spacing: 18) {
            Image(systemName: "tray.and.arrow.down.fill")
                .font(.system(size: 58, weight: .regular))
                .foregroundStyle(theme.gradient)
                .scaleEffect(isDropTargeted ? 1.12 : 1.0)

            Text(isDropTargeted ? "Release to add" : "Drag & drop a file here")
                .font(.system(size: 20, weight: .semibold))
                .foregroundStyle(.primary)

            Text("or click anywhere to browse")
                .font(.system(size: 14))
                .foregroundStyle(.secondary)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(
            RoundedRectangle(cornerRadius: 20, style: .continuous)
                .fill(theme.softGradient)
        )
        .overlay(
            RoundedRectangle(cornerRadius: 20, style: .continuous)
                .strokeBorder(
                    theme.gradient.opacity(isDropTargeted ? 1.0 : 0.5),
                    lineWidth: isDropTargeted ? 2.5 : 1.5
                )
        )
        .contentShape(RoundedRectangle(cornerRadius: 20, style: .continuous))
        .onTapGesture { onAddFile() }
        .onDrop(of: [.fileURL], isTargeted: $isDropTargeted) { providers in
            let handled = handleDrop(providers)
            if handled {
                setUploadMode(false)
            }
            return handled
        }
        .animation(.easeInOut(duration: 0.2), value: isDropTargeted)
    }

    private func setUploadMode(_ on: Bool) {
        withAnimation {
            isUploadMode = on
            if on { isGraphMode = false }
        }
        onUploadModeChanged(on)
        if on {
            isFieldFocused = false
            onResize(uploadSize)
        } else {
            onResize(isChatting ? chatSize : compactSize)
        }
    }

    private func openEngrexFolder() {
        let url = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Engrex")
        try? FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        NSWorkspace.shared.open(url)
    }

    // The graph URL (served by the daemon on :7778), with the current theme's colors
    // as params so the embedded graph matches the app.
    private var graphURL: URL? {
        var components = URLComponents(string: "http://localhost:7778")!
        let colors = theme.colors.prefix(3).map { hexString(from: $0) }
        if colors.count == 3 {
            components.queryItems = [
                URLQueryItem(name: "c1", value: colors[0]),
                URLQueryItem(name: "c2", value: colors[1]),
                URLQueryItem(name: "c3", value: colors[2]),
            ]
        }
        return components.url
    }

    private func setGraphMode(_ on: Bool) {
        withAnimation {
            isGraphMode = on
            if on { isUploadMode = false }
        }
        if on {
            isFieldFocused = false
            onResize(graphSize)
        } else {
            onResize(isChatting ? chatSize : compactSize)
        }
    }

    private func hexString(from color: Color) -> String {
        let nsColor = NSColor(color).usingColorSpace(.sRGB) ?? NSColor(color)
        let red = Int(round(nsColor.redComponent * 255))
        let green = Int(round(nsColor.greenComponent * 255))
        let blue = Int(round(nsColor.blueComponent * 255))
        return String(format: "%02X%02X%02X", red, green, blue)
    }

    private func handleDrop(_ providers: [NSItemProvider]) -> Bool {
        var handledAny = false
        for provider in providers where provider.canLoadObject(ofClass: URL.self) {
            handledAny = true
            _ = provider.loadObject(ofClass: URL.self) { url, _ in
                guard let url = url else { return }
                DispatchQueue.main.async {
                    FileIngestor.addAndNotify(url)
                }
            }
        }
        return handledAny
    }

    // The search bar always starts a new conversation, and the question stays in it so
    // the top of the window shows what the conversation is about
    private func submitQuery() {
        let trimmed = queryText.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty, !isStreaming else { return }
        messages = []
        followUpText = ""
        send(trimmed)
    }

    private func submitFollowUp() {
        let trimmed = followUpText.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty, !isStreaming, isChatting else { return }
        followUpText = ""
        send(trimmed)
    }

    private func send(_ question: String) {
        // Re-attach the chip flags so the daemon receives them for parsing.
        let flagString = tags.joined(separator: " ")
        let fullQuery = [question, flagString].filter { !$0.isEmpty }.joined(separator: " ")

        // History is everything before this question. The daemon strips flags and its
        // own grounding notes from it, so the transcript is sent as shown
        let history = messages
        let reply = ChatMessage(role: .assistant, text: "")
        let replyID = reply.id

        if isUploadMode {
            withAnimation { isUploadMode = false }
            onUploadModeChanged(false)
        }
        if isGraphMode {
            withAnimation { isGraphMode = false }
        }
        lastActivity = Date()
        withAnimation {
            messages.append(ChatMessage(role: .user, text: question))
            messages.append(reply)
            streamingMessageID = replyID
        }
        onResize(chatSize)

        // Every callback looks the reply up by ID, since New chat can drop it while the
        // answer is still streaming
        socketClient.sendQuery(
            text: fullQuery,
            history: history,
            model: ModelSetting.shared.modelForQuery,
            onSources: { sources in
                guard let index = self.messages.firstIndex(where: { $0.id == replyID }) else { return }
                withAnimation { self.messages[index].sources = sources }
            },
            onToken: { token in
                guard let index = self.messages.firstIndex(where: { $0.id == replyID }) else { return }
                self.messages[index].text += token
            },
            onComplete: { error in
                if let error, let index = self.messages.firstIndex(where: { $0.id == replyID }),
                   self.messages[index].text.isEmpty {
                    self.messages[index].text = error.localizedDescription
                }
                if self.streamingMessageID == replyID {
                    withAnimation { self.streamingMessageID = nil }
                    // Ready for a follow-up once the answer is in, unless a new question
                    // is already being typed up top
                    if !self.isFieldFocused {
                        self.isFollowUpFocused = true
                    }
                }
                self.lastActivity = Date()
            }
        )
    }

    private func clear() {
        onResize(compactSize)
        onUploadModeChanged(false)
        withAnimation {
            queryText = ""
            followUpText = ""
            messages = []
            streamingMessageID = nil
            isUploadMode = false
            isGraphMode = false
            tags = []
        }
        isFieldFocused = true
    }
}

// Three dots pulsing in sequence. Driven by TimelineView so the values are computed
// every frame from the current time — SwiftUI's implicit animation would interpolate
// only between endpoints and appear static.
struct ThinkingIndicator: View {
    let gradient: LinearGradient

    var body: some View {
        TimelineView(.animation) { timeline in
            let time = timeline.date.timeIntervalSinceReferenceDate
            HStack(spacing: 5) {
                ForEach(0..<3) { index in
                    let phase = time * 4 - Double(index) * 0.6
                    let value = (sin(phase) + 1) / 2
                    Circle()
                        .fill(gradient)
                        .frame(width: 7, height: 7)
                        .scaleEffect(0.6 + 0.5 * value)
                        .opacity(0.35 + 0.65 * value)
                }
            }
        }
    }
}
