// Shared by the SwiftUI app and its Go JSON contract test.
import Foundation

// Additive command outcomes for external frontend compatibility checks. The
// existing SwiftUI process bridge still uses the original CLI exit status.
struct FrontendOutcome: Decodable {
    struct Failure: Decodable { let code: String; let message: String }
    let version: Int
    let event: String
    let ok: Bool
    let cancelled: Bool
    let accounts: [Account]?
    let error: Failure?
}

// MARK: - Model (mirrors `aiu --json`)

struct UsageWindow: Decodable, Hashable {
    let key: String
    let label: String
    let group: String
    let percent: Double
    let resetsAt: Date?
    let severity: String?

    /// "5h session" → "5h", "7d all" → "7d", "7d Fable" stays.
    var shortLabel: String {
        label.replacingOccurrences(of: " session", with: "").replacingOccurrences(of: " all", with: "")
    }
}

struct LoginHealth: Decodable, Hashable {
    let state: String
    let message: String
}

struct BankedResetRequest: Decodable, Hashable {
    let requestId: String
    let creditId: String?
}

struct BankedResetCredit: Decodable, Hashable, Identifiable {
    let id: String
    let resetType: String
    let status: String
    let grantedAt: String
    let expiresAt: String?
    let title: String
    let description: String
    let canRedeem: Bool
}

struct BankedResets: Decodable, Hashable {
    let availableCount: Int?
    let credits: [BankedResetCredit]?
    let fetchedAt: String?
    let stale: String?
    let error: String?
    let canRedeem: Bool
    let pendingRequest: BankedResetRequest?
    let autoReset: Bool
    let autoResetThresholdPercent: Int
    let autoResetStatus: String?

    enum CodingKeys: String, CodingKey {
        case availableCount, credits, fetchedAt, stale, error, canRedeem
        case pendingRequest, autoReset, autoResetThresholdPercent, autoResetStatus
    }

    init(from decoder: Decoder) throws {
        let values = try decoder.container(keyedBy: CodingKeys.self)
        availableCount = try values.decodeIfPresent(Int.self, forKey: .availableCount)
        credits = try values.decodeIfPresent([BankedResetCredit].self, forKey: .credits)
        fetchedAt = try values.decodeIfPresent(String.self, forKey: .fetchedAt)
        stale = try values.decodeIfPresent(String.self, forKey: .stale)
        error = try values.decodeIfPresent(String.self, forKey: .error)
        canRedeem = try values.decodeIfPresent(Bool.self, forKey: .canRedeem) ?? false
        pendingRequest = try values.decodeIfPresent(BankedResetRequest.self, forKey: .pendingRequest)
        autoReset = try values.decodeIfPresent(Bool.self, forKey: .autoReset) ?? false
        autoResetThresholdPercent = try values.decodeIfPresent(Int.self, forKey: .autoResetThresholdPercent) ?? 1
        autoResetStatus = try values.decodeIfPresent(String.self, forKey: .autoResetStatus)
    }
}

struct Account: Decodable, Identifiable, Hashable {
    let provider: String
    let email: String
    let label: String
    /// One address can hold several organizations (Claude) or ChatGPT accounts, each
    /// with its own limits, so the organization is part of what names an account.
    let org: String?
    let orgName: String?
    let active: Bool
    let tier: String?
    let login: LoginHealth
    let readOnly: Bool
    let canSwitch: Bool
    let windows: [UsageWindow]
    let stale: String?
    let fetchedAt: Date?
    let error: String?
    /// The account's place in aiu's ranking for its provider, 0 best, and the few words
    /// the switcher shows beside it ("43% left", "back in 4d 16h").
    let rank: Int?
    let note: String?
    /// The general weekly window is gone: there is nothing to spend on this account until
    /// it resets, whatever its other numbers say.
    let spent: Bool?
    /// The one account per provider aiu says to work in next, and the line explaining why.
    /// `allSpent` turns that into a countdown: nothing has weekly room left, and this is
    /// merely the account that comes back first.
    let recommended: Bool?
    let why: String?
    let allSpent: Bool?
    let bankedResets: BankedResets?

    // The organization is part of the identity: one address can hold several, and a
    // list keyed only by address collapses them into one row.
    var id: String { "\(provider):\(email)#\(org ?? "")" }
    var kind: Provider { Provider(rawValue: provider) ?? .claude }
    var session: UsageWindow? { windows.first { $0.group == "session" } }
    var busiestWeekly: UsageWindow? { windows.filter { $0.group == "weekly" }.max { $0.percent < $1.percent } }
    /// What limits the account right now.
    var tightest: Double { max(session?.percent ?? 0, busiestWeekly?.percent ?? 0) }
    var needsLogin: Bool { ["expired", "missing"].contains(login.state) }
    /// Claude names an individual's own organization "<address>'s Organization", which
    /// says nothing the address does not; only a real organization name is worth room.
    var distinctOrgName: String? {
        guard let orgName, !orgName.isEmpty, !orgName.hasSuffix("'s Organization") else { return nil }
        return orgName
    }
}

enum Provider: String, CaseIterable, Identifiable {
    case claude, codex
    var id: String { rawValue }
    var name: String { self == .claude ? "Claude" : "Codex" }
    var client: String { self == .claude ? "Claude Code" : "Codex" }
    var logoResource: String { self == .claude ? "claude" : "openai" }
}

/// One decoder for everything the Go side prints. Its timestamps are RFC 3339 — with
/// fractional seconds on some fields and not others, which the plain .iso8601 strategy
/// rejects outright.
enum AIUJSON {
    static func decoder() -> JSONDecoder {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .custom { decoder in
            let raw = try decoder.singleValueContainer().decode(String.self)
            let formatter = ISO8601DateFormatter()
            if let date = formatter.date(from: raw) { return date }
            formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
            if let date = formatter.date(from: raw) { return date }
            throw DecodingError.dataCorrupted(.init(codingPath: decoder.codingPath,
                                                    debugDescription: "bad date \(raw)"))
        }
        return decoder
    }
}
