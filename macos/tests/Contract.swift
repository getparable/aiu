import Foundation

@main
struct ContractTest {
    static func main() throws {
        let directory = URL(fileURLWithPath: CommandLine.arguments[1])
        let accounts = try AIUJSON.decoder().decode([Account].self,
            from: Data(contentsOf: directory.appendingPathComponent("accounts.json")))
        precondition(accounts.count == 3)
        precondition(accounts[0].id == "claude:work@example.test#org-work")
        precondition(accounts[0].active && accounts[0].canSwitch)
        precondition(accounts[0].recommended == true)
        precondition(accounts[0].windows[0].percent == 12)
        precondition(accounts[0].windows[0].resetsAt != nil && accounts[0].fetchedAt != nil)
        precondition(accounts[1].needsLogin && !accounts[1].canSwitch)
        precondition(accounts[1].windows.isEmpty && accounts[1].error != nil)
        precondition(accounts[2].readOnly && !accounts[2].canSwitch)
        precondition(accounts[2].stale != nil && accounts[2].windows[0].severity == "locked")
        let empty = try AIUJSON.decoder().decode([Account].self,
            from: Data(contentsOf: directory.appendingPathComponent("empty.json")))
        precondition(empty.isEmpty)

        for name in ["failure", "cancelled"] {
            let outcome = try AIUJSON.decoder().decode(FrontendOutcome.self,
                from: Data(contentsOf: directory.appendingPathComponent(name + ".json")))
            precondition(outcome.version == 1 && outcome.event == "result")
            precondition(!outcome.ok && outcome.accounts == nil)
            precondition(outcome.cancelled == (name == "cancelled"))
            precondition(outcome.error != nil)
        }
        // Existing accounts and fractional RFC3339 timestamps still decode.
        let raw = try String(contentsOf: directory.appendingPathComponent("accounts.json"), encoding: .utf8)
            .replacingOccurrences(of: "2030-01-01T00:00:00Z", with: "2030-01-01T00:00:00.123Z")
        _ = try AIUJSON.decoder().decode([Account].self, from: Data(raw.utf8))
        print("PASS: SwiftUI model decodes the shared Go account contract")
    }
}
