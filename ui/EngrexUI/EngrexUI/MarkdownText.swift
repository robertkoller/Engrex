import SwiftUI

// MarkdownText renders the markdown answers come back in (headings, bullet lists, code
// fences, bold/italic/inline code). SwiftUI's own markdown support only handles inline
// styling, so block structure is split out here line by line. It's reparsed on every
// streamed token, which is fine at answer sizes, and a half streamed fence just renders
// as an open code block until it closes
struct MarkdownText: View {
    let markdown: String
    var fontSize: CGFloat = 16

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            ForEach(Array(MarkdownBlock.parse(markdown).enumerated()), id: \.offset) { _, block in
                view(for: block)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .textSelection(.enabled)
    }

    @ViewBuilder
    private func view(for block: MarkdownBlock) -> some View {
        switch block {
        case .heading(let level, let text):
            Text(Self.inline(text))
                .font(.system(size: fontSize + CGFloat(max(0, 4 - level)) * 2, weight: .bold))
                .padding(.top, 4)
        case .bullet(let depth, let text):
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                Text(depth == 0 ? "•" : "◦")
                Text(Self.inline(text))
            }
            .font(.system(size: fontSize))
            .lineSpacing(4)
            .padding(.leading, CGFloat(depth) * 18)
        case .numbered(let depth, let marker, let text):
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                Text(marker)
                Text(Self.inline(text))
            }
            .font(.system(size: fontSize))
            .lineSpacing(4)
            .padding(.leading, CGFloat(depth) * 18)
        case .code(let text):
            ScrollView(.horizontal, showsIndicators: false) {
                Text(text)
                    .font(.system(size: fontSize - 2, design: .monospaced))
                    .padding(10)
            }
            .background(
                RoundedRectangle(cornerRadius: 8, style: .continuous)
                    .fill(Color.black.opacity(0.25))
            )
        case .paragraph(let text):
            Text(Self.inline(text))
                .font(.system(size: fontSize))
                .lineSpacing(4)
        }
    }

    // Bold, italic, inline code and links. Falls back to the raw text if the markdown
    // doesn't parse, which can happen mid stream with an unclosed marker
    static func inline(_ text: String) -> AttributedString {
        let options = AttributedString.MarkdownParsingOptions(interpretedSyntax: .inlineOnlyPreservingWhitespace)
        if let attributed = try? AttributedString(markdown: text, options: options) {
            return attributed
        }
        return AttributedString(text)
    }
}

enum MarkdownBlock: Equatable {
    case heading(level: Int, text: String)
    case bullet(depth: Int, text: String)
    case numbered(depth: Int, marker: String, text: String)
    case code(String)
    case paragraph(String)

    // Splits markdown into blocks. Consecutive plain lines are joined into one paragraph
    static func parse(_ markdown: String) -> [MarkdownBlock] {
        var blocks: [MarkdownBlock] = []
        var paragraph: [String] = []
        var codeLines: [String]? = nil
        var codeIndent = 0

        func flushParagraph() {
            if !paragraph.isEmpty {
                blocks.append(.paragraph(paragraph.joined(separator: "\n")))
                paragraph = []
            }
        }

        for rawLine in markdown.components(separatedBy: "\n") {
            let trimmed = rawLine.trimmingCharacters(in: .whitespaces)
            let indent = rawLine.prefix(while: { $0 == " " || $0 == "\t" }).count

            if trimmed.hasPrefix("```") {
                if let lines = codeLines {
                    blocks.append(.code(lines.joined(separator: "\n")))
                    codeLines = nil
                } else {
                    flushParagraph()
                    codeLines = []
                    codeIndent = indent
                }
                continue
            }
            if codeLines != nil {
                // Fences nested under a bullet are indented, strip that indent back off
                codeLines?.append(String(rawLine.dropFirst(min(codeIndent, indent))))
                continue
            }

            if trimmed.isEmpty {
                flushParagraph()
                continue
            }

            if let heading = headingMatch(trimmed) {
                flushParagraph()
                blocks.append(.heading(level: heading.level, text: heading.text))
            } else if let bulletText = bulletMatch(trimmed) {
                flushParagraph()
                blocks.append(.bullet(depth: indent / 2, text: bulletText))
            } else if let numbered = numberedMatch(trimmed) {
                flushParagraph()
                blocks.append(.numbered(depth: indent / 2, marker: numbered.marker, text: numbered.text))
            } else if trimmed.allSatisfy({ $0 == "-" || $0 == "*" || $0 == "_" }) && trimmed.count >= 3 {
                flushParagraph()
            } else {
                paragraph.append(trimmed)
            }
        }

        // An unclosed fence is still streaming, show what's arrived
        if let lines = codeLines {
            blocks.append(.code(lines.joined(separator: "\n")))
        }
        flushParagraph()
        return blocks
    }

    private static func headingMatch(_ line: String) -> (level: Int, text: String)? {
        let hashes = line.prefix(while: { $0 == "#" }).count
        guard hashes >= 1, hashes <= 6 else {
            return nil
        }
        let rest = line.dropFirst(hashes)
        guard rest.first == " " else {
            return nil
        }
        return (hashes, rest.trimmingCharacters(in: .whitespaces))
    }

    private static func bulletMatch(_ line: String) -> String? {
        for marker in ["* ", "- ", "+ "] where line.hasPrefix(marker) {
            return String(line.dropFirst(marker.count))
        }
        return nil
    }

    private static func numberedMatch(_ line: String) -> (marker: String, text: String)? {
        let digits = line.prefix(while: { $0.isNumber })
        guard !digits.isEmpty, digits.count <= 3 else {
            return nil
        }
        let rest = line.dropFirst(digits.count)
        guard rest.hasPrefix(". ") || rest.hasPrefix(") ") else {
            return nil
        }
        return ("\(digits).", String(rest.dropFirst(2)))
    }
}
