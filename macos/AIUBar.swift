// AIU.app — a Liquid Glass menu bar panel for aiu.
//
// This is only a front end. Every number comes from `aiu --json`, and every action
// (switch, sign in, track, remove) runs the aiu binary bundled beside this one, so the
// Go core stays the single owner of tokens, the request throttle and the cache.

import AppKit
import ServiceManagement
import SwiftUI

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

// MARK: - Logos

@MainActor
enum Logo {
    private static var cache: [Provider: NSImage] = [:]

    /// The brand mark as a template image, so it follows the menu bar and panel tint.
    static func image(_ provider: Provider) -> NSImage {
        if let cached = cache[provider] { return cached }
        let url = Bundle.main.url(forResource: provider.logoResource, withExtension: "svg")
        let image = url.flatMap { NSImage(contentsOf: $0) } ?? NSImage(systemSymbolName: "circle", accessibilityDescription: nil)!
        image.isTemplate = true
        cache[provider] = image
        return image
    }
}

struct LogoView: View {
    let provider: Provider
    var size: CGFloat = 14
    var body: some View {
        Image(nsImage: Logo.image(provider))
            .resizable()
            .renderingMode(.template)
            .interpolation(.high)
            .frame(width: size, height: size)
            .accessibilityLabel(provider.name)
    }
}

// MARK: - CLI bridge

struct CLIResult: Sendable {
    let status: Int32
    let stdout: Data
    let stderr: String

    /// The last `error: …` line aiu printed, without the prefix.
    var message: String {
        let lines = stderr.split(separator: "\n").map(String.init)
        let error = lines.last { $0.hasPrefix("error: ") } ?? lines.last ?? "aiu exited with status \(status)"
        return error.replacingOccurrences(of: "error: ", with: "")
    }
}

private final class DataBox: @unchecked Sendable {
    var data = Data()
}

enum CLI {
    /// The Go binary ships inside the bundle, next to this executable.
    static var executable: URL { Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/aiu") }

    static func makeProcess(_ arguments: [String]) -> Process {
        let process = Process()
        process.executableURL = executable
        process.arguments = arguments
        var env = ProcessInfo.processInfo.environment
        env["NO_COLOR"] = "1"
        process.environment = env
        process.standardInput = FileHandle.nullDevice
        return process
    }

    static func run(_ arguments: [String]) async -> CLIResult {
        await withCheckedContinuation { continuation in
            DispatchQueue.global(qos: .userInitiated).async {
                let process = makeProcess(arguments)
                let out = Pipe(), err = Pipe()
                process.standardOutput = out
                process.standardError = err
                do {
                    try process.run()
                } catch {
                    continuation.resume(returning: CLIResult(status: -1, stdout: Data(), stderr: "error: \(error.localizedDescription)"))
                    return
                }
                // Drain both pipes at once so a full stderr can never block stdout.
                let errBox = DataBox()
                let group = DispatchGroup()
                group.enter()
                DispatchQueue.global().async {
                    errBox.data = err.fileHandleForReading.readDataToEndOfFile()
                    group.leave()
                }
                let outData = out.fileHandleForReading.readDataToEndOfFile()
                group.wait()
                process.waitUntilExit()
                continuation.resume(returning: CLIResult(status: process.terminationStatus, stdout: outData, stderr: String(decoding: errBox.data, as: UTF8.self)))
            }
        }
    }
}

// MARK: - Store

@MainActor
@Observable
final class Store {
    var accounts: [Account] = []
    var lastError: String?
    var updatedAt: Date?
    var loading = false
    var notice: String?
    var barImage: NSImage = Store.placeholderImage()

    // Sign-in in progress
    var loginProvider: Provider?
    var loginURL: URL?
    var loginOutput = ""

    var refreshMinutes: Int {
        didSet {
            UserDefaults.standard.set(refreshMinutes, forKey: "refreshMinutes")
            scheduleTimer()
        }
    }

    @ObservationIgnored private var timer: Timer?
    @ObservationIgnored private var loginProcess: Process?

    init() {
        let saved = UserDefaults.standard.integer(forKey: "refreshMinutes")
        refreshMinutes = saved >= 5 ? saved : 5
        scheduleTimer()
        Task { await refresh() }
    }

    private func scheduleTimer() {
        timer?.invalidate()
        timer = Timer.scheduledTimer(withTimeInterval: TimeInterval(refreshMinutes * 60), repeats: true) { [weak self] _ in
            Task { @MainActor in await self?.refresh() }
        }
    }

    func group(_ provider: Provider) -> [Account] { accounts.filter { $0.kind == provider } }

    /// The account the menu bar summarises: the one the CLI is signed in as, else the one with most headroom.
    func headline(_ provider: Provider) -> Account? {
        let usable = group(provider).filter { $0.error == nil }
        return usable.first { $0.active } ?? usable.min { $0.tightest < $1.tightest }
    }

    func mostHeadroom(_ provider: Provider) -> Account? {
        group(provider).filter { $0.error == nil }.min { $0.tightest < $1.tightest }
    }

    /// Cheap to call often: aiu answers from its cache until an account's 5-minute spacing has passed.
    func refresh() async {
        guard !loading else { return }
        loading = true
        defer { loading = false }
        // Development aid: render a saved `aiu --json` instead of calling the CLI.
        if let fixture = ProcessInfo.processInfo.environment["AIU_JSON_FIXTURE"],
           let data = FileManager.default.contents(atPath: fixture) {
            decode(data)
            return
        }
        let result = await CLI.run(["--json"])
        guard result.status == 0 else {
            lastError = result.message
            return
        }
        decode(result.stdout)
    }

    private func decode(_ data: Data) {
        do {
            let decoder = JSONDecoder()
            decoder.dateDecodingStrategy = .custom { decoder in
                let raw = try decoder.singleValueContainer().decode(String.self)
                let formatter = ISO8601DateFormatter()
                if let date = formatter.date(from: raw) { return date }
                formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
                if let date = formatter.date(from: raw) { return date }
                throw DecodingError.dataCorrupted(.init(codingPath: decoder.codingPath, debugDescription: "bad date \(raw)"))
            }
            accounts = try decoder.decode([Account].self, from: data)
            lastError = nil
            updatedAt = Date()
            barImage = renderBarImage()
        } catch {
            lastError = "Could not read aiu output: \(error.localizedDescription)"
        }
    }

    func refreshIfStale() {
        if let updatedAt, Date().timeIntervalSince(updatedAt) < 60 { return }
        Task { await refresh() }
    }

    private func flash(_ message: String) {
        notice = message
        Task {
            try? await Task.sleep(for: .seconds(4))
            if notice == message { notice = nil }
        }
    }

    func switchTo(_ account: Account) async {
        guard confirm(
            title: "Switch \(account.kind.client) to \(account.label)?",
            message: "\(account.email)\n\nRunning \(account.kind.client) sessions follow within about 30 seconds.",
            action: "Switch"
        ) else { return }
        let result = await CLI.run(["switch", account.id])
        if result.status == 0 {
            flash("\(account.kind.client) now uses \(account.label)")
            await refresh()
        } else {
            lastError = result.message
        }
    }

    func track(_ provider: Provider, label: String) async {
        var args = ["add"] + (provider == .codex ? ["--codex"] : [])
        if !label.isEmpty { args += ["--label", label] }
        let result = await CLI.run(args)
        if result.status == 0 {
            flash("Tracking \(provider.client)'s current account")
            await refresh()
        } else {
            lastError = result.message
        }
    }

    func remove(_ account: Account) async {
        guard confirm(
            title: "Remove \(account.label)?",
            message: "Forget \(account.email) and delete its stored tokens. \(account.kind.client)'s own login is not touched.",
            action: "Remove"
        ) else { return }
        let result = await CLI.run(["remove", account.id])
        if result.status == 0 {
            flash("Removed \(account.label)")
            await refresh()
        } else {
            lastError = result.message
        }
    }

    /// Runs `aiu login --no-open` and surfaces its URL, so the user can open it in a
    /// private window when the browser is signed in to another account.
    func beginLogin(_ provider: Provider, label: String) {
        cancelLogin()
        var args = ["login", "--no-open"] + (provider == .codex ? ["--codex"] : [])
        if !label.isEmpty { args += ["--label", label] }
        let process = CLI.makeProcess(args)
        let out = Pipe(), err = Pipe()
        process.standardOutput = out
        process.standardError = err
        let errBox = DataBox()
        out.fileHandleForReading.readabilityHandler = { handle in
            let text = String(decoding: handle.availableData, as: UTF8.self)
            guard !text.isEmpty else { return }
            Task { @MainActor [weak self] in
                guard let self else { return }
                self.loginOutput += text
                if self.loginURL == nil, let range = self.loginOutput.range(of: #"https://\S+"#, options: .regularExpression) {
                    self.loginURL = URL(string: String(self.loginOutput[range]))
                    if let url = self.loginURL { NSWorkspace.shared.open(url) }
                }
            }
        }
        err.fileHandleForReading.readabilityHandler = { handle in errBox.data.append(handle.availableData) }
        process.terminationHandler = { finished in
            out.fileHandleForReading.readabilityHandler = nil
            err.fileHandleForReading.readabilityHandler = nil
            let status = finished.terminationStatus
            let stderr = String(decoding: errBox.data, as: UTF8.self)
            Task { @MainActor [weak self] in
                guard let self, self.loginProcess === finished else { return }
                self.loginProcess = nil
                self.loginProvider = nil
                self.loginURL = nil
                if status == 0 {
                    let added = self.loginOutput.split(separator: "\n").first { $0.contains("✔") }
                    self.flash(added.map { String($0).replacingOccurrences(of: "✔ ", with: "").capitalizedFirst } ?? "Signed in")
                    await self.refresh()
                } else if status != 130 && status != 2 {
                    self.lastError = CLIResult(status: status, stdout: Data(), stderr: stderr).message
                }
                self.loginOutput = ""
            }
        }
        do {
            try process.run()
            loginProcess = process
            loginProvider = provider
            lastError = nil
        } catch {
            lastError = error.localizedDescription
        }
    }

    func cancelLogin() {
        loginProcess?.interrupt() // SIGINT: aiu closes its callback listener and exits
        loginProcess = nil
        loginProvider = nil
        loginURL = nil
        loginOutput = ""
    }

    private func confirm(title: String, message: String, action: String) -> Bool {
        NSApp.activate()
        let alert = NSAlert()
        alert.messageText = title
        alert.informativeText = message
        alert.addButton(withTitle: action)
        alert.addButton(withTitle: "Cancel")
        return alert.runModal() == .alertFirstButtonReturn
    }

    // MARK: Menu bar label

    private static func placeholderImage() -> NSImage {
        let image = NSImage(systemSymbolName: "gauge.with.needle", accessibilityDescription: "aiu")!
        image.isTemplate = true
        return image
    }

    /// Logo + tightest percentage per provider, rendered once as a template image so it
    /// tints exactly like system menu bar items.
    private func renderBarImage() -> NSImage {
        let items = Provider.allCases.compactMap { provider in headline(provider).map { (provider, $0.tightest) } }
        guard !items.isEmpty else { return Store.placeholderImage() }
        let content = HStack(spacing: 8) {
            ForEach(items, id: \.0) { provider, percent in
                HStack(spacing: 3) {
                    LogoView(provider: provider, size: 13)
                    Text("\(Int(percent.rounded()))%")
                        .font(.system(size: 12.5, weight: .semibold))
                        .monospacedDigit()
                }
            }
        }
        .foregroundStyle(.black)
        .frame(height: 18)
        let renderer = ImageRenderer(content: content)
        renderer.scale = NSScreen.main?.backingScaleFactor ?? 2
        guard let image = renderer.nsImage else { return Store.placeholderImage() }
        image.isTemplate = true
        return image
    }
}

extension String {
    var capitalizedFirst: String { prefix(1).uppercased() + dropFirst() }
}

// MARK: - Formatting

func compactInterval(until date: Date) -> String {
    let seconds = Int(date.timeIntervalSinceNow)
    guard seconds > 0 else { return "now" }
    let minutes = (seconds + 30) / 60
    let days = minutes / 1440, hours = (minutes % 1440) / 60, mins = minutes % 60
    if days > 0 { return "\(days)d \(hours)h" }
    if hours > 0 { return "\(hours)h \(mins)m" }
    return "\(mins)m"
}

// MARK: - Views

struct UsageBar: View {
    let percent: Double

    /// Monochrome: the fill gets denser as usage rises instead of changing colour.
    private var fillOpacity: Double {
        switch percent {
        case 80...: 0.95
        case 60..<80: 0.75
        default: 0.55
        }
    }

    var body: some View {
        GeometryReader { geometry in
            ZStack(alignment: .leading) {
                // Color.primary, not the .primary style: glass would make the latter vibrant and wash it out.
                Capsule().fill(Color.primary.opacity(0.14))
                Capsule()
                    .fill(Color.primary.opacity(fillOpacity))
                    .frame(width: max(percent > 0 ? 4 : 0, geometry.size.width * min(percent, 100) / 100))
            }
        }
        .frame(height: 6)
        .animation(.smooth, value: percent)
    }
}

struct WindowRow: View {
    let window: UsageWindow

    var body: some View {
        HStack(spacing: 10) {
            Text(window.shortLabel)
                .font(.caption)
                .foregroundStyle(.secondary)
                .frame(width: 44, alignment: .leading)
                .lineLimit(1)
            UsageBar(percent: window.percent)
            Text("\(Int(window.percent.rounded()))%")
                .font(.caption.weight(window.percent >= 80 ? .bold : .medium))
                .monospacedDigit()
                .frame(width: 34, alignment: .trailing)
            Text(window.resetsAt.map { compactInterval(until: $0) } ?? "")
                .font(.caption2)
                .foregroundStyle(.tertiary)
                .monospacedDigit()
                .frame(width: 46, alignment: .trailing)
        }
        .help(window.resetsAt.map { "\(window.label) resets \($0.formatted(date: .abbreviated, time: .shortened))" } ?? window.label)
    }
}

/// One account inside a card: header, subtitle and its windows. `showsAddress` is off
/// when the card's header already names the address, and the organization names the row.
struct AccountBody: View {
    @Environment(Store.self) private var store
    let account: Account
    var showsAddress = true

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 8) {
                Image(systemName: account.active ? "circle.fill" : "circle")
                    .font(.system(size: 7))
                    .foregroundStyle(account.active ? .primary : .tertiary)
                    .help(account.active ? "Active in \(account.kind.client)" : "Not active")
                Text(account.label)
                    .font(.system(.body, weight: .semibold))
                    .lineLimit(1)
                if let tier = account.tier, !tier.isEmpty {
                    Text(tier)
                        .font(.caption2.weight(.medium))
                        .foregroundStyle(.secondary)
                        .padding(.horizontal, 6)
                        .padding(.vertical, 2)
                        .background(.primary.opacity(0.07), in: .capsule)
                }
                Spacer(minLength: 4)
                actions
            }
            // Top level: the address. Nested under an address: the organization.
            Text(showsAddress ? account.email : (account.distinctOrgName ?? "Personal"))
                .font(.caption)
                .foregroundStyle(.secondary)
                .lineLimit(1)
                .truncationMode(.middle)

            if let error = account.error {
                Label(error, systemImage: "exclamationmark.triangle")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .lineLimit(3)
            } else {
                VStack(spacing: 6) {
                    ForEach(account.windows, id: \.key) { WindowRow(window: $0) }
                }
                .padding(.top, 2)
            }

            if account.login.state != "ok" || account.stale != nil {
                VStack(alignment: .leading, spacing: 2) {
                    if account.login.state != "ok" {
                        Label(account.login.message, systemImage: "key")
                    }
                    if let stale = account.stale {
                        Label(stale, systemImage: "clock.arrow.circlepath")
                    }
                }
                .font(.caption2)
                .foregroundStyle(.secondary)
                .lineLimit(2)
            }
        }
    }

    private var actions: some View {
        Menu {
            if account.canSwitch && !account.active {
                Button("Switch \(account.kind.client) to This Account") { Task { await store.switchTo(account) } }
            }
            if account.needsLogin || account.login.state == "expiring" {
                Button("Sign In Again…") { store.beginLogin(account.kind, label: account.label) }
            }
            Divider()
            Button("Remove…", role: .destructive) { Task { await store.remove(account) } }
        } label: {
            Image(systemName: "ellipsis")
                .font(.system(size: 12, weight: .semibold))
                .frame(width: 22, height: 22)
                .contentShape(.circle)
        }
        .menuStyle(.button)
        .menuIndicator(.hidden)
        .buttonStyle(.glass)
        .buttonBorderShape(.circle)
        .wrapsVertically()
    }
}

struct AccountCard: View {
    let account: Account

    var body: some View {
        AccountBody(account: account)
            .padding(12)
            .cardSurface()
    }
}

/// Several organizations on one address: one card, headed by the address, with each
/// organization's own limits nested inside it.
struct GroupedAccountCard: View {
    let email: String
    let accounts: [Account]

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(spacing: 8) {
                Image(systemName: accounts.contains(where: \.active) ? "circle.fill" : "circle")
                    .font(.system(size: 7))
                    .foregroundStyle(accounts.contains(where: \.active) ? .primary : .tertiary)
                Text(email)
                    .font(.system(.body, weight: .semibold))
                    .lineLimit(1)
                    .truncationMode(.middle)
                Spacer(minLength: 4)
                Text("\(accounts.count) organizations")
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
            }
            ForEach(Array(accounts.enumerated()), id: \.element.id) { index, account in
                if index > 0 { Divider().opacity(0.35) }
                AccountBody(account: account, showsAddress: false)
                    .padding(.leading, 10)
            }
        }
        .padding(12)
        .cardSurface()
    }
}

struct ProviderSection: View {
    @Environment(Store.self) private var store
    let provider: Provider

    var body: some View {
        let accounts = store.group(provider)
        if !accounts.isEmpty {
            VStack(alignment: .leading, spacing: 8) {
                HStack(spacing: 6) {
                    LogoView(provider: provider, size: 13)
                    Text(provider.name.uppercased())
                        .font(.caption.weight(.semibold))
                        .tracking(0.6)
                    Spacer()
                    if accounts.count > 1, let best = store.mostHeadroom(provider) {
                        Text("most headroom: \(best.label)")
                            .font(.caption2)
                            .foregroundStyle(.tertiary)
                    }
                }
                .foregroundStyle(.secondary)
                .padding(.horizontal, 4)
                ForEach(groupedByAddress(accounts), id: \.0) { email, group in
                    if group.count == 1 {
                        AccountCard(account: group[0])
                    } else {
                        GroupedAccountCard(email: email, accounts: group)
                    }
                }
            }
        }
    }
}

/// Accounts by address, in the order they were added.
func groupedByAddress(_ accounts: [Account]) -> [(String, [Account])] {
    var order: [String] = []
    var byEmail: [String: [Account]] = [:]
    for account in accounts {
        if byEmail[account.email] == nil { order.append(account.email) }
        byEmail[account.email, default: []].append(account)
    }
    return order.map { ($0, byEmail[$0] ?? []) }
}

struct AddAccountView: View {
    @Environment(Store.self) private var store
    @State private var provider: Provider = .claude
    @State private var label = ""
    let close: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            if let pending = store.loginProvider {
                waiting(pending)
            } else {
                Picker("Provider", selection: $provider) {
                    ForEach(Provider.allCases) { p in Text(p.name).tag(p) }
                }
                .pickerStyle(.segmented)
                .labelsHidden()
                .tint(Color.secondary) // monochrome selection instead of accent blue

                TextField("Label (optional)", text: $label)
                    .textFieldStyle(.roundedBorder)

                Button {
                    store.beginLogin(provider, label: label)
                } label: {
                    Label("Sign in with browser", systemImage: "globe")
                        .fontWeight(.semibold)
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(.glass)
                .controlSize(.large)

                Button {
                    Task {
                        await store.track(provider, label: label)
                        close()
                    }
                } label: {
                    Label("Track \(provider.client)'s current login", systemImage: "arrow.down.circle")
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(.glass)
                .controlSize(.large)

                Text("Tip: prefer tracking the account \(provider.client) already uses. For another account, copy the sign-in link into a private window.")
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
                    .wrapsVertically()
            }
        }
        .padding(14)
        .cardSurface()
    }

    private func waiting(_ pending: Provider) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(spacing: 8) {
                ProgressView().controlSize(.small)
                Text("Waiting for \(pending.name) sign-in…")
                    .font(.callout.weight(.medium))
            }
            Text("Finish signing in in your browser. If it opened the wrong account, copy the link into a private window instead.")
                .font(.caption)
                .foregroundStyle(.secondary)
                .wrapsVertically()
            HStack {
                if let url = store.loginURL {
                    Button("Copy Link", systemImage: "doc.on.doc") {
                        NSPasteboard.general.clearContents()
                        NSPasteboard.general.setString(url.absoluteString, forType: .string)
                    }
                    .buttonStyle(.glass)
                }
                Spacer()
                Button("Cancel") { store.cancelLogin() }
                    .buttonStyle(.glass)
            }
        }
    }
}

struct SettingsView: View {
    static let trademarkNotice = "Claude is a trademark of Anthropic, PBC. OpenAI and Codex are trademarks of OpenAI. AIU is not affiliated with or endorsed by either; their logos only label whose usage is shown."
    static let version = Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "dev"

    @Environment(Store.self) private var store
    @State private var launchAtLogin = SMAppService.mainApp.status == .enabled
    @State private var launchError: String?

    var body: some View {
        @Bindable var store = store
        VStack(alignment: .leading, spacing: 12) {
            Picker("Refresh every", selection: $store.refreshMinutes) {
                ForEach([5, 10, 15, 30], id: \.self) { Text("\($0) min").tag($0) }
            }
            Toggle("Launch at login", isOn: $launchAtLogin)
                .onChange(of: launchAtLogin) { _, on in
                    do {
                        if on { try SMAppService.mainApp.register() } else { try SMAppService.mainApp.unregister() }
                        launchError = nil
                    } catch {
                        launchError = error.localizedDescription
                        launchAtLogin = SMAppService.mainApp.status == .enabled
                    }
                }
            if let launchError {
                Text(launchError).font(.caption).foregroundStyle(.secondary)
            }
            Text("aiu reads each account at most once every 5 minutes, shared with the terminal.")
                .font(.caption2)
                .foregroundStyle(.tertiary)
                .wrapsVertically()
            Divider().opacity(0.4)
            Text(Self.trademarkNotice)
                .font(.caption2)
                .foregroundStyle(.tertiary)
                .wrapsVertically()
            HStack {
                Text("AIU \(Self.version)")
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
                Spacer()
                Button("Quit AIU") { NSApp.terminate(nil) }
                    .buttonStyle(.glass)
            }
        }
        .padding(14)
        .cardSurface()
    }
}

enum Pane { case usage, add, settings }

struct Panel: View {
    /// The list grows with its content up to this, then scrolls.
    private static let maxListHeight: CGFloat = 520

    @Environment(Store.self) private var store
    @State private var pane: Pane = .usage
    @State private var listHeight: CGFloat = 0

    var body: some View {
        GlassEffectContainer(spacing: 8) {
            VStack(alignment: .leading, spacing: 12) {
                header
                if let error = store.lastError {
                    Label(error, systemImage: "exclamationmark.triangle")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineLimit(4)
                        .padding(.horizontal, 4)
                }
                switch pane {
                case .usage: usage
                case .add: AddAccountView { pane = .usage }
                case .settings: SettingsView()
                }
                footer
            }
            .padding(12)
        }
        .frame(width: 360)
        .onAppear {
            pane = .usage
            store.refreshIfStale()
        }
        .animation(.smooth(duration: 0.25), value: pane)
    }

    private var header: some View {
        HStack(spacing: 8) {
            Text(pane == .add ? "Add Account" : pane == .settings ? "Settings" : "Usage")
                .font(.title3.weight(.semibold))
            Spacer()
            if pane == .usage {
                iconButton("arrow.clockwise", help: "Refresh") { Task { await store.refresh() } }
                    .disabled(store.loading)
                iconButton("plus", help: "Add account") { pane = .add }
                iconButton("gearshape", help: "Settings") { pane = .settings }
            } else {
                iconButton("xmark", help: "Back") { pane = .usage }
            }
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 8)
        .cardSurface(cornerRadius: 18)
    }

    @ViewBuilder
    private var usage: some View {
        if store.accounts.isEmpty && store.updatedAt != nil {
            VStack(spacing: 10) {
                Image(systemName: "person.crop.circle.badge.plus").font(.largeTitle).foregroundStyle(.secondary)
                Text("No accounts tracked yet").font(.headline)
                Button("Add Account") { pane = .add }.buttonStyle(.glass)
            }
            .frame(maxWidth: .infinity)
            .padding(24)
            .cardSurface()
        } else if store.accounts.isEmpty {
            ProgressView().frame(maxWidth: .infinity).padding(24)
        } else {
            ScrollView {
                VStack(alignment: .leading, spacing: 14) {
                    ForEach(Provider.allCases) { ProviderSection(provider: $0) }
                }
                // Keep the cards clear of the indicator instead of under it.
                .padding(.trailing, 9)
                .onGeometryChange(for: CGFloat.self) { $0.size.height } action: { listHeight = $0 }
            }
            .scrollBounceBehavior(.basedOnSize)
            .scrollIndicators(.hidden)
            .slimScrollIndicator()
            // The measured height, capped: a ScrollView has no natural height of its
            // own, and fixedSize would let it grow past the panel and paint over the
            // pinned header instead of scrolling under it.
            .frame(height: min(max(listHeight, 80), Self.maxListHeight))
            .clipShape(.rect(cornerRadius: 18))
        }
    }

    private var footer: some View {
        HStack {
            if let notice = store.notice {
                Label(notice, systemImage: "checkmark").lineLimit(1)
            } else if store.loading {
                Text("Refreshing…")
            } else if let updatedAt = store.updatedAt {
                Text("Updated \(updatedAt.formatted(date: .omitted, time: .shortened))")
            }
            Spacer()
        }
        .font(.caption2)
        .foregroundStyle(.tertiary)
        .padding(.horizontal, 6)
    }

    private func iconButton(_ symbol: String, help: String, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            Image(systemName: symbol)
                .font(.system(size: 12, weight: .semibold))
                .frame(width: 24, height: 24)
        }
        .buttonStyle(.glass)
        .buttonBorderShape(.circle)
        .help(help)
    }
}

extension View {
    /// The panel's card surface. Deliberately not `.glassEffect`: stacked glass reads
    /// as murky over a busy desktop, and its backdrop layer ignores a scroll view's
    /// clip, so cards drew over the pinned header.
    func cardSurface(cornerRadius: CGFloat = 16) -> some View {
        background(.thickMaterial, in: .rect(cornerRadius: cornerRadius))
            .overlay(
                RoundedRectangle(cornerRadius: cornerRadius)
                    .strokeBorder(.primary.opacity(0.07), lineWidth: 1)
            )
    }

    /// Let wrapped text size itself vertically inside the fixed-width panel.
    func wrapsVertically() -> some View { fixedSize(horizontal: false, vertical: true) }

    /// A thin indicator drawn over the scroll view's trailing edge. The system one is
    /// wider and sits under the glass cards; this one stays in front of them.
    func slimScrollIndicator() -> some View { modifier(SlimScrollIndicator()) }
}

private struct ScrollMetrics: Equatable {
    var offset: CGFloat = 0
    var visible: CGFloat = 0
    var content: CGFloat = 0

    var scrollable: CGFloat { max(0, content - visible) }
    var thumbHeight: CGFloat { max(28, visible * (visible / max(content, 1))) }
    var thumbOffset: CGFloat {
        guard scrollable > 0 else { return 0 }
        let travel = visible - thumbHeight
        return min(max(offset / scrollable, 0), 1) * travel
    }
}

private struct SlimScrollIndicator: ViewModifier {
    @State private var metrics = ScrollMetrics()

    func body(content: Content) -> some View {
        content
            .onScrollGeometryChange(for: ScrollMetrics.self) { geometry in
                ScrollMetrics(
                    offset: geometry.contentOffset.y,
                    visible: geometry.containerSize.height,
                    content: geometry.contentSize.height
                )
            } action: { _, new in
                metrics = new
            }
            .overlay(alignment: .topTrailing) {
                if metrics.scrollable > 1 {
                    Capsule()
                        .fill(.primary.opacity(0.28))
                        .frame(width: 3, height: metrics.thumbHeight)
                        .offset(y: metrics.thumbOffset)
                        .padding(.trailing, 1)
                        .allowsHitTesting(false)
                        .transition(.opacity)
                }
            }
    }
}

// MARK: - App

@main
struct AIUBarApp: App {
    @State private var store = Store()

    var body: some Scene {
        MenuBarExtra {
            Panel().environment(store)
        } label: {
            Image(nsImage: store.barImage)
                .accessibilityLabel("aiu usage")
        }
        .menuBarExtraStyle(.window)
    }
}
