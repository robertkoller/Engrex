import AppKit
import ApplicationServices
import Carbon

class AccessibilityReader {

    // Tries the Accessibility API first (works in native apps, no clipboard touched).
    // Falls back to synthesizing Cmd+C for apps like Chrome that don't expose their
    // selection through AX. The previous clipboard contents are restored afterward.
    static func readSelectedText() -> String? {
        if let text = readViaAccessibility(), !text.isEmpty {
            return text
        }
        return readViaCopy()
    }

    private static func readViaAccessibility() -> String? {
        let systemElement = AXUIElementCreateSystemWide()

        var focusedElement: AnyObject?
        guard AXUIElementCopyAttributeValue(systemElement, kAXFocusedUIElementAttribute as CFString, &focusedElement) == .success,
              let element = focusedElement else {
            return nil
        }

        var selectedText: AnyObject?
        guard AXUIElementCopyAttributeValue(element as! AXUIElement, kAXSelectedTextAttribute as CFString, &selectedText) == .success,
              let text = selectedText as? String,
              !text.isEmpty else {
            return nil
        }

        return text
    }

    private static func readViaCopy() -> String? {
        let pasteboard = NSPasteboard.general
        let saved = savePasteboard(pasteboard)
        let previousChangeCount = pasteboard.changeCount

        simulateCopy()

        // Poll briefly for the frontmost app to place the selection on the pasteboard.
        var captured: String?
        let deadline = Date().addingTimeInterval(0.4)
        while Date() < deadline {
            if pasteboard.changeCount != previousChangeCount {
                captured = pasteboard.string(forType: .string)
                break
            }
            usleep(15_000) // 15ms
        }

        restorePasteboard(pasteboard, items: saved)

        guard let result = captured?.trimmingCharacters(in: .whitespacesAndNewlines), !result.isEmpty else {
            return nil
        }
        return result
    }

    private static func simulateCopy() {
        let source = CGEventSource(stateID: .combinedSessionState)
        let keyCode = CGKeyCode(kVK_ANSI_C)

        let keyDown = CGEvent(keyboardEventSource: source, virtualKey: keyCode, keyDown: true)
        keyDown?.flags = .maskCommand
        let keyUp = CGEvent(keyboardEventSource: source, virtualKey: keyCode, keyDown: false)
        keyUp?.flags = .maskCommand

        keyDown?.post(tap: .cghidEventTap)
        keyUp?.post(tap: .cghidEventTap)
    }

    // Deep-copies the current pasteboard so we can restore it after the synthetic copy.
    private static func savePasteboard(_ pasteboard: NSPasteboard) -> [NSPasteboardItem] {
        var archived: [NSPasteboardItem] = []
        for item in pasteboard.pasteboardItems ?? [] {
            let copy = NSPasteboardItem()
            for type in item.types {
                if let data = item.data(forType: type) {
                    copy.setData(data, forType: type)
                }
            }
            archived.append(copy)
        }
        return archived
    }

    private static func restorePasteboard(_ pasteboard: NSPasteboard, items: [NSPasteboardItem]) {
        pasteboard.clearContents()
        if !items.isEmpty {
            pasteboard.writeObjects(items)
        }
    }

    static func requestPermission() {
        let options = [kAXTrustedCheckOptionPrompt.takeUnretainedValue() as String: true] as CFDictionary
        AXIsProcessTrustedWithOptions(options)
    }

    static func hasPermission() -> Bool {
        return AXIsProcessTrusted()
    }
}
