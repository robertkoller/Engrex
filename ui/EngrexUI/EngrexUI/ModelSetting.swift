import Foundation
import Combine

// ModelSetting holds which generation model queries should use.
//
// The daemon owns the model names — it reports them over the socket — so the app never
// hardcodes "qwen3:4b". That way changing the models is a config edit on the daemon
// side rather than a rebuild of the app.
final class ModelSetting: ObservableObject {
    static let shared = ModelSetting()

    private static let deepEnabledKey = "engrex.useDeepModel"
    private static let cloudEnabledKey = "engrex.useCloudModel"

    // useDeepModel is persisted, so the choice survives a restart. It is the only piece
    // of state the toggle owns; everything else is reported by the daemon.
    @Published var useDeepModel: Bool {
        didSet { UserDefaults.standard.set(useDeepModel, forKey: Self.deepEnabledKey) }
    }

    // useCloudModel sends answers to Gemini instead of Ollama. It wins over the deep
    // model toggle, and does nothing while the daemon reports no Gemini key
    @Published var useCloudModel: Bool {
        didSet { UserDefaults.standard.set(useCloudModel, forKey: Self.cloudEnabledKey) }
    }

    // Names as reported by the daemon. Empty until the first successful fetch, which is
    // why modelForQuery falls back to sending nothing rather than guessing a name.
    @Published private(set) var defaultModel: String = ""
    @Published private(set) var deepModel: String = ""
    @Published private(set) var cloudModel: String = ""

    private init() {
        useDeepModel = UserDefaults.standard.bool(forKey: Self.deepEnabledKey)
        useCloudModel = UserDefaults.standard.bool(forKey: Self.cloudEnabledKey)
    }

    // True when queries will actually go to Gemini, not just when the toggle is on
    var isUsingCloud: Bool {
        useCloudModel && !cloudModel.isEmpty
    }

    // modelForQuery is what gets sent with a query. Empty means "daemon's default",
    // which is also the safe answer before the model names have loaded.
    var modelForQuery: String {
        if isUsingCloud {
            return cloudModel
        }
        return useDeepModel ? deepModel : ""
    }

    var cloudModelLabel: String {
        cloudModel.isEmpty ? "Use Gemini (no key in .env)" : "Use Gemini (\(cloudModel))"
    }

    // Label for the menu item, falling back to generic wording until the daemon answers.
    var deepModelLabel: String {
        deepModel.isEmpty ? "Deep thinking" : "Deep thinking (\(deepModel))"
    }

    // refresh asks the daemon which models it offers. Failure is silent: the toggle
    // still works, it just sends an empty model and the daemon uses its default.
    func refresh(using client: SocketClient, completion: (() -> Void)? = nil) {
        client.fetchModels { [weak self] (models: SocketClient.Models?) in
            guard let self, let models else { return }
            self.defaultModel = models.defaultModel
            self.deepModel = models.deep
            self.cloudModel = models.cloud
            completion?()
        }
    }
}
