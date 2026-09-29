// The Keychain onboarding window.
//
// macOS has no permission API for the Keychain the way it has for the microphone
// or Accessibility, so there is nothing to request up front. Instead the bundled
// `aiu keychain --json` inspects each item's access control without reading it,
// and Allow performs the read that makes macOS ask, at a moment the user expects
// it and with the instruction that makes the answer stick: Always Allow.

import AppKit
import SwiftUI

/// What `aiu keychain --json` reports for each Keychain item AIU uses.
enum KeychainAccess {
    struct Item: Decodable, Equatable, Identifiable {
        var id: String
        var state: String
        var detail: String?

        var isGranted: Bool { state == "granted" }
        /// Only these put a macOS prompt in front of the user.
        var needsApproval: Bool { state == "needs-approval" || state == "unknown" }
        /// Known to prompt. "unknown" offers Allow in the window but never opens it
        /// by itself, or a build that cannot tell would open it on every launch.
        var certainlyPrompts: Bool { state == "needs-approval" }
    }

    struct Report: Decodable {
        var items: [Item] = []
        var error: String?
    }

    static func status() async -> Report { await run(["keychain", "status", "--json"]) }

    /// Blocks until the user answers the macOS prompt, if there is one.
    static func allow(_ id: String) async -> Report { await run(["keychain", "allow", id, "--json"]) }

    private static func run(_ arguments: [String]) async -> Report {
        let result = await CLI.run(arguments)
        if var report = try? JSONDecoder().decode(Report.self, from: result.stdout) {
            if report.error == nil && result.status != 0 { report.error = result.message }
            return report
        }
        return Report(error: result.message)
    }
}

@MainActor
enum OnboardingWindow {
    private static var window: NSWindow?
    private static var closeObserver: NSObjectProtocol?

    static func show(_ store: Store) {
        store.onboardingOpen = true
        if let window {
            window.makeKeyAndOrderFront(nil)
            window.orderFrontRegardless()
            NSApp.activate()
            return
        }
        let root = OnboardingView { window?.close() }.environment(store)
        let window = NSWindow(contentViewController: NSHostingController(rootView: root))
        window.styleMask = [.titled, .closable, .fullSizeContentView]
        window.titlebarAppearsTransparent = true
        window.titleVisibility = .hidden
        window.title = "Keychain Access"
        window.isReleasedWhenClosed = false
        window.center()
        // Closing by any route counts as Continue: the panel may read the Keychain again.
        closeObserver = NotificationCenter.default.addObserver(
            forName: NSWindow.willCloseNotification, object: window, queue: .main
        ) { _ in
            MainActor.assumeIsolated {
                if let closeObserver { NotificationCenter.default.removeObserver(closeObserver) }
                closeObserver = nil
                OnboardingWindow.window = nil
                store.onboardingOpen = false
                Task { await store.refresh() }
            }
        }
        self.window = window
        // A menu bar app launched at login is not active, and macOS will not let it
        // take focus, so put the window in front regardless.
        window.makeKeyAndOrderFront(nil)
        window.orderFrontRegardless()
        NSApp.activate()
    }
}

struct OnboardingView: View {
    let onContinue: () -> Void
    @Environment(Store.self) private var store

    var body: some View {
        VStack(spacing: 0) {
            VStack(spacing: 10) {
                Image(systemName: "lock.shield")
                    .font(.system(size: 30, weight: .medium))
                    .frame(width: 64, height: 64)
                    .cardSurface(cornerRadius: 16)
                    .padding(.bottom, 6)
                Text("Allow Keychain Access")
                    .font(.largeTitle.bold())
                Text("AIU reads the logins Claude Code and AIU keep in your Keychain.")
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
            }
            .padding(.top, 36)

            VStack(spacing: 10) {
                ForEach(Array(store.keychain.enumerated()), id: \.element.id) { index, item in
                    KeychainRow(index: index, item: item)
                }
                if store.keychain.isEmpty {
                    ProgressView().controlSize(.small).padding()
                }
            }
            .padding(.top, 28)

            Label {
                Text("When macOS asks, enter your Mac login password and choose **Always Allow**. **Allow** works only once, so macOS would ask again next time.")
                    .wrapsVertically()
            } icon: {
                Image(systemName: "info.circle")
            }
            .font(.callout)
            .foregroundStyle(.secondary)
            .padding(.top, 16)

            Spacer(minLength: 24)

            HStack {
                Button("Recheck") { Task { await store.checkKeychain() } }
                    .buttonStyle(.glass)
                    .controlSize(.large)
                    .disabled(store.keychainBusy != nil)
                Spacer()
                Button(store.needsKeychainApproval ? "Continue Anyway" : "Continue", action: onContinue)
                    .buttonStyle(.glassProminent)
                    .controlSize(.large)
                    .keyboardShortcut(.defaultAction)
                    .disabled(store.keychainBusy != nil)
            }
        }
        .padding(.horizontal, 36)
        .padding(.bottom, 28)
        .frame(width: 560, height: 560)
        .task { await store.checkKeychain() }
    }
}

private struct KeychainRow: View {
    let index: Int
    let item: KeychainAccess.Item
    @Environment(Store.self) private var store

    private var title: String {
        item.id == "claude" ? "Claude Code login" : "AIU accounts"
    }

    private var purpose: String {
        item.id == "claude"
            ? "Shows which account Claude Code is signed in as, and points it at another when you switch. macOS asks for “security”, the tool Claude Code uses too."
            : "Keeps the tokens for every account you track in one Keychain item."
    }

    /// What to say once the item needs nothing from the user.
    private var settledLabel: String {
        switch item.state {
        case "granted": "Granted"
        case "missing": item.id == "claude" ? "Not signed in" : "Not created yet"
        default: "Not needed"
        }
    }

    /// Why the last Allow did not settle it, if it did not.
    private var followUp: String? {
        guard item.needsApproval, store.keychainAttempted.contains(item.id) else { return nil }
        if let error = store.keychainError { return "macOS did not allow access: \(error)" }
        return "macOS will still ask each time. Choose Always Allow, not Allow."
    }

    var body: some View {
        HStack(alignment: .center, spacing: 14) {
            ZStack {
                RoundedRectangle(cornerRadius: 8).fill(.primary.opacity(0.08))
                if item.needsApproval {
                    Text("\(index + 1)").font(.callout.weight(.semibold)).foregroundStyle(.secondary)
                } else {
                    Image(systemName: "checkmark").font(.callout.weight(.bold))
                }
            }
            .frame(width: 30, height: 30)

            VStack(alignment: .leading, spacing: 3) {
                Text(title).font(.headline)
                Text(purpose)
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .wrapsVertically()
                if let followUp {
                    Text(followUp)
                        .font(.caption)
                        .foregroundStyle(Palette.accent)
                        .wrapsVertically()
                }
            }
            Spacer(minLength: 12)

            if store.keychainBusy == item.id {
                HStack(spacing: 6) {
                    ProgressView().controlSize(.small)
                    Text("Waiting for macOS…").font(.callout).foregroundStyle(.secondary)
                }
            } else if item.needsApproval {
                Button("Allow") { Task { await store.allowKeychain(item.id) } }
                    .buttonStyle(.glassProminent)
                    .controlSize(.large)
                    .disabled(store.keychainBusy != nil)
            } else {
                Text(settledLabel)
                    .font(.callout.weight(.medium))
                    .foregroundStyle(.secondary)
                    .padding(.horizontal, 12)
                    .padding(.vertical, 6)
                    .background(.primary.opacity(0.07), in: .capsule)
            }
        }
        .padding(14)
        .cardSurface(cornerRadius: 14)
    }
}
