import SwiftUI
import AppKit
import UniformTypeIdentifiers

struct QueryView: View {
    var onResize: (CGSize) -> Void = { _ in }
    var onAddFile: () -> Void = {}
    var onUploadModeChanged: (Bool) -> Void = { _ in }

    @AppStorage(Themes.storageKey) private var themeIndex: Int = 0

    @State private var queryText: String = ""
    @State private var answer: String = ""
    @State private var isStreaming: Bool = false
    @State private var hasSubmitted: Bool = false
    @State private var isUploadMode: Bool = false
    @State private var isGraphMode: Bool = false
    @State private var isDropTargeted: Bool = false
    @State private var tags: [String] = []
    @State private var sources: [String] = []
    @FocusState private var isFieldFocused: Bool

    private let socketClient = SocketClient()

    private let compactSize = CGSize(width: 660, height: 92)
    private let expandedSize = CGSize(width: 660, height: 480)
    private let expandedWithSourcesSize = CGSize(width: 880, height: 480)
    private let uploadSize = CGSize(width: 640, height: 560)
    private let graphSize = CGSize(width: 940, height: 660)

    // Flags that become chips when typed. Key is the raw flag, value is the chip label.
    private let knownFlags: [(flag: String, label: String)] = [
        ("--date", "date"),
        ("--source", "source")
    ]

    private var theme: Theme { Themes.theme(for: themeIndex) }

    var body: some View {
        ZStack {
            background

            VStack(alignment: .leading, spacing: 14) {
                searchBar

                if isUploadMode {
                    dropZone
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                        .transition(.opacity.combined(with: .move(edge: .top)))
                } else if isGraphMode {
                    graphView
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                        .transition(.opacity.combined(with: .move(edge: .top)))
                } else if hasSubmitted {
                    answerArea
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                        .transition(.asymmetric(
                            insertion: .move(edge: .top).combined(with: .opacity),
                            removal: .opacity
                        ))
                }
            }
            .padding(18)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .animation(.spring(response: 0.4, dampingFraction: 0.85), value: hasSubmitted)
        .animation(.spring(response: 0.4, dampingFraction: 0.85), value: isUploadMode)
        .animation(.spring(response: 0.4, dampingFraction: 0.85), value: isGraphMode)
        .onAppear {
            // Delay so the window has become key before requesting focus — otherwise
            // the focus request is dropped and the "color in" animation never fires.
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.15) {
                withAnimation(.easeInOut(duration: 0.35)) {
                    isFieldFocused = true
                }
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
                    processTags(newValue)
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

    // Extracts completed flag tokens (flag followed by a space) from the field and
    // turns them into chips, leaving partially typed flags alone.
    private func processTags(_ newValue: String) {
        var remaining = newValue
        for known in knownFlags {
            let token = known.flag + " "
            while let range = remaining.range(of: token) {
                if !tags.contains(known.flag) {
                    tags.append(known.flag)
                }
                remaining.removeSubrange(range)
            }
        }
        if remaining != newValue {
            queryText = remaining
        }
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

    private var answerArea: some View {
        HStack(alignment: .top, spacing: 14) {
            answerScroll

            if !sources.isEmpty {
                sourcesPanel
                    .frame(width: 200)
                    .transition(.move(edge: .trailing).combined(with: .opacity))
            }
        }
        .animation(.spring(response: 0.4, dampingFraction: 0.85), value: sources)
    }

    private var answerScroll: some View {
        ScrollViewReader { proxy in
            ScrollView {
                VStack(alignment: .leading, spacing: 0) {
                    if answer.isEmpty && isStreaming {
                        Text("Thinking…")
                            .font(.system(size: 15))
                            .foregroundStyle(.secondary)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    } else {
                        Text(answer)
                            .font(.system(size: 16))
                            .lineSpacing(4)
                            .foregroundStyle(.primary)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .textSelection(.enabled)
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
            .onChange(of: answer) { _, _ in
                withAnimation(.easeOut(duration: 0.15)) {
                    proxy.scrollTo("bottom", anchor: .bottom)
                }
            }
        }
    }

    private var sourcesPanel: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("SOURCES")
                .font(.system(size: 11, weight: .bold))
                .kerning(0.5)
                .foregroundStyle(.secondary)

            ScrollView {
                VStack(alignment: .leading, spacing: 6) {
                    ForEach(sources, id: \.self) { source in
                        sourceRow(source)
                    }
                }
            }
        }
        .padding(14)
        .frame(maxHeight: .infinity, alignment: .top)
        .background(
            RoundedRectangle(cornerRadius: 16, style: .continuous)
                .fill(Color.black.opacity(0.18))
        )
        .overlay(
            RoundedRectangle(cornerRadius: 16, style: .continuous)
                .strokeBorder(theme.softGradient, lineWidth: 1)
        )
    }

    private func sourceRow(_ source: String) -> some View {
        let name = displayName(for: source)
        return Button {
            openSource(source)
        } label: {
            HStack(spacing: 8) {
                Image(systemName: iconName(for: source))
                    .font(.system(size: 13))
                    .foregroundStyle(theme.gradient)
                Text(name)
                    .font(.system(size: 13))
                    .foregroundStyle(.primary)
                    .lineLimit(1)
                    .truncationMode(.middle)
                Spacer(minLength: 0)
            }
            .padding(.horizontal, 10)
            .padding(.vertical, 7)
            .background(
                RoundedRectangle(cornerRadius: 8, style: .continuous)
                    .fill(Color.white.opacity(0.06))
            )
            .contentShape(RoundedRectangle(cornerRadius: 8, style: .continuous))
        }
        .buttonStyle(.plain)
        .help("Open \(name)")
    }

    private func isWebURL(_ source: String) -> Bool {
        return source.hasPrefix("http://") || source.hasPrefix("https://")
    }

    private func displayName(for source: String) -> String {
        if isWebURL(source) {
            return URL(string: source)?.host ?? source
        }
        return (source as NSString).lastPathComponent
    }

    private func openSource(_ source: String) {
        if isWebURL(source) {
            if let url = URL(string: source) {
                NSWorkspace.shared.open(url)
            }
        } else {
            NSWorkspace.shared.open(URL(fileURLWithPath: source))
        }
    }

    private func iconName(for source: String) -> String {
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
            onResize(hasSubmitted ? expandedSize : compactSize)
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
            onResize(hasSubmitted ? expandedSize : compactSize)
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

    private func submitQuery() {
        let trimmed = queryText.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty, !isStreaming else { return }

        // Re-attach the chip flags so the daemon receives them for parsing.
        let flagString = tags.joined(separator: " ")
        let fullQuery = [trimmed, flagString].filter { !$0.isEmpty }.joined(separator: " ")

        answer = ""
        sources = []
        if isUploadMode {
            withAnimation { isUploadMode = false }
            onUploadModeChanged(false)
        }
        if isGraphMode {
            withAnimation { isGraphMode = false }
        }
        onResize(expandedSize)
        withAnimation {
            hasSubmitted = true
            isStreaming = true
        }

        socketClient.sendQuery(
            text: fullQuery,
            onSources: { sources in
                withAnimation { self.sources = sources }
                if !sources.isEmpty {
                    self.onResize(self.expandedWithSourcesSize)
                }
            },
            onToken: { token in
                self.answer += token
            },
            onComplete: { _ in
                withAnimation { self.isStreaming = false }
            }
        )
    }

    private func clear() {
        onResize(compactSize)
        onUploadModeChanged(false)
        withAnimation {
            queryText = ""
            answer = ""
            sources = []
            hasSubmitted = false
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
