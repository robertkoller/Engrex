import AppKit
import Carbon

// File-level callback — required because C function pointers cannot be closures or methods.
func hotkeyEventCallback(_ nextHandler: EventHandlerCallRef?, _ event: EventRef?, _ userData: UnsafeMutableRawPointer?) -> OSStatus {
    guard let userData = userData else { return OSStatus(eventNotHandledErr) }
    let manager = Unmanaged<HotkeyManager>.fromOpaque(userData).takeUnretainedValue()
    manager.handleEvent(event)
    return noErr
}

class HotkeyManager {
    private weak var statusBarController: StatusBarController?
    private var captureHotkeyRef: EventHotKeyRef?
    private var queryHotkeyRef: EventHotKeyRef?
    private var eventHandlerRef: EventHandlerRef?

    // Signature 'ENGR' encoded as UInt32.
    private let hotkeySignature: OSType = 0x454E4752

    init(statusBarController: StatusBarController) {
        self.statusBarController = statusBarController
    }

    func registerHotkeys() {
        var eventType = EventTypeSpec(
            eventClass: OSType(kEventClassKeyboard),
            eventKind: UInt32(kEventHotKeyPressed)
        )

        InstallEventHandler(
            GetApplicationEventTarget(),
            hotkeyEventCallback,
            1,
            &eventType,
            Unmanaged.passUnretained(self).toOpaque(),
            &eventHandlerRef
        )

        let captureID = EventHotKeyID(signature: hotkeySignature, id: 1)
        RegisterEventHotKey(
            UInt32(kVK_ANSI_B),
            UInt32(cmdKey | shiftKey),
            captureID,
            GetApplicationEventTarget(),
            0,
            &captureHotkeyRef
        )

        let queryID = EventHotKeyID(signature: hotkeySignature, id: 2)
        RegisterEventHotKey(
            UInt32(kVK_Space),
            UInt32(cmdKey | shiftKey),
            queryID,
            GetApplicationEventTarget(),
            0,
            &queryHotkeyRef
        )
    }

    func unregisterHotkeys() {
        if let reference = captureHotkeyRef { UnregisterEventHotKey(reference) }
        if let reference = queryHotkeyRef { UnregisterEventHotKey(reference) }
        if let reference = eventHandlerRef { RemoveEventHandler(reference) }
    }

    func handleEvent(_ event: EventRef?) {
        var hotkeyID = EventHotKeyID()
        GetEventParameter(
            event,
            EventParamName(kEventParamDirectObject),
            EventParamType(typeEventHotKeyID),
            nil,
            MemoryLayout<EventHotKeyID>.size,
            nil,
            &hotkeyID
        )

        switch hotkeyID.id {
        case 1:
            handleCaptureHotkey()
        case 2:
            handleQueryHotkey()
        default:
            break
        }
    }

    private func handleCaptureHotkey() {
        guard let text = AccessibilityReader.readSelectedText(), !text.isEmpty else {
            ToastPresenter.shared.show(
                message: "No text selected",
                systemImage: "exclamationmark.triangle.fill",
                isError: true
            )
            return
        }
        SocketClient().sendAdd(text: text) { error in
            if let error = error {
                ToastPresenter.shared.show(
                    message: "Save failed: \(error.localizedDescription)",
                    systemImage: "xmark.octagon.fill",
                    isError: true
                )
            } else {
                ToastPresenter.shared.show(
                    message: "Saved to Engrex",
                    systemImage: "checkmark.circle.fill"
                )
            }
        }
    }

    private func handleQueryHotkey() {
        DispatchQueue.main.async {
            self.statusBarController?.openQueryWindow()
        }
    }
}
