import Foundation

// One message in the query window's conversation. Lives only in memory, so a restart
// or a new chat drops it
struct ChatMessage: Identifiable, Equatable {
    // Raw values match the daemon's protocol roles
    enum Role: String {
        case user
        case assistant
    }

    let id = UUID()
    let role: Role
    var text: String
    var sources: [String] = []
}
