import SwiftUI

// A named color preset used across the query UI and toasts.
struct Theme: Identifiable {
    let id: Int
    let name: String
    let colors: [Color]

    var gradient: LinearGradient {
        LinearGradient(colors: colors, startPoint: .leading, endPoint: .trailing)
    }

    var softGradient: LinearGradient {
        LinearGradient(
            colors: colors.map { $0.opacity(0.18) },
            startPoint: .topLeading,
            endPoint: .bottomTrailing
        )
    }

    var glowColor: Color {
        colors[min(1, colors.count - 1)]
    }
}

enum Themes {
    static let storageKey = "themeIndex"

    static let all: [Theme] = [
        Theme(id: 0, name: "Aurora", colors: [
            Color(red: 0.55, green: 0.36, blue: 0.96),
            Color(red: 0.30, green: 0.60, blue: 0.98),
            Color(red: 0.28, green: 0.80, blue: 0.78)
        ]),
        Theme(id: 1, name: "Sunset", colors: [
            Color(red: 0.98, green: 0.55, blue: 0.25),
            Color(red: 0.96, green: 0.35, blue: 0.55),
            Color(red: 0.60, green: 0.35, blue: 0.90)
        ]),
        Theme(id: 2, name: "Mint", colors: [
            Color(red: 0.30, green: 0.85, blue: 0.55),
            Color(red: 0.20, green: 0.75, blue: 0.75),
            Color(red: 0.30, green: 0.55, blue: 0.95)
        ]),
        Theme(id: 3, name: "Rose", colors: [
            Color(red: 0.98, green: 0.45, blue: 0.65),
            Color(red: 0.95, green: 0.35, blue: 0.45),
            Color(red: 0.98, green: 0.60, blue: 0.35)
        ]),
        Theme(id: 4, name: "Ocean", colors: [
            Color(red: 0.20, green: 0.50, blue: 0.95),
            Color(red: 0.20, green: 0.75, blue: 0.90),
            Color(red: 0.35, green: 0.40, blue: 0.92)
        ])
    ]

    static func theme(for index: Int) -> Theme {
        return all[max(0, min(index, all.count - 1))]
    }

    // The currently selected theme, read from the persisted index.
    static var current: Theme {
        return theme(for: UserDefaults.standard.integer(forKey: storageKey))
    }
}
