# aiu-rs

AIU is a Windows-first, cross-platform Rust monitor for Claude Code and Codex accounts. It shows login health, session and weekly usage windows, and a recommendation for the next account. The original Go implementation is retained under [`legacy/`](legacy/); the Rust fork is the active build.

## Install

Download a native archive from [Releases](https://github.com/krflol/aiu-rs/releases/latest): Windows x64, Linux x64, macOS Apple Silicon, or macOS Intel. Extract it and run `aiu gui` (`aiu.exe gui` on Windows). Each archive includes the executable, this README and the license; `SHA256SUMS` lists archive checksums. Linux archives are built on Ubuntu 22.04 and need a compatible glibc plus the desktop runtime libraries described below. macOS binaries are not notarized.

With Rust 1.88 or newer:

```sh
cargo install --git https://github.com/krflol/aiu-rs --locked
```

Run `aiu` or `aiu status` for the terminal view. Use `aiu --json` for an array of `AccountView` records, `aiu list` to inspect saved accounts, `aiu login` to add a browser login, and `aiu gui` for the portable desktop panel. `aiu --fixture fixtures/demo.json --json` renders the synthetic sample without reading credentials or contacting providers. `aiu paths` prints active storage paths.

On Windows, use the MSVC toolchain and Visual Studio C++ Build Tools. From this checkout, run `cargo build --release --locked`, then `.\target\release\aiu.exe gui`. For a smaller terminal-only installation, add `--no-default-features` to the Cargo install command.

```text
aiu add --label personal       Save Claude Code's current login
aiu add --codex --label work   Save Codex's current ChatGPT login
aiu login --codex              Browser sign-in for another Codex account
aiu login --readonly           Claude usage-only login (cannot switch)
aiu login --manual --no-open    Claude manual authorization-code flow
aiu watch --interval 60        Refresh until Ctrl+C
aiu --provider codex           Filter before fetching usage
aiu switch codex:work          Select a saved CLI login
aiu remove personal            Forget an account in AIU
aiu sync                       Adopt newer CLI tokens
aiu whoami                     Identify active CLI logins
aiu update                     Check this fork's releases; never installs
```

Use `provider:email#organization-id` to disambiguate accounts. Start a new CLI session after switching. The desktop panel provides filters, usage bars, reset times, recommendations, browser sign-in, adding the current login, switching, and forgetting accounts. `menubar` is an alias for `gui`.

The desktop panel also keeps a system tray or menu bar icon while it runs. Closing the panel hides it when the tray is available; use the tray menu to show AIU again, refresh usage, or quit. On Windows, left-clicking the icon opens the panel and right-clicking opens its menu. On macOS, AIU appears in the menu bar. Linux uses GTK/AppIndicator, so a desktop session must provide an AppIndicator or StatusNotifier host; the panel remains available as a normal window if tray initialization fails.

Browser sign-in denial ends the wait. Closing a browser tab does not send an OAuth callback; use **Cancel sign-in** in AIU to stop waiting. Quitting during an account update lets the update finish saving credentials before the process exits.

## Storage and settings

Windows stores encrypted token data with DPAPI for the current Windows user, macOS uses the `aiu-rs` Keychain item, and Linux uses owner-only files below `$XDG_CONFIG_HOME/aiu-rs` (normally `~/.config/aiu-rs`). Claude's files and Codex's `auth.json` remain in their normal locations. `AIU_STORE=file` selects the owner-only file store; `AIU_STORE=native` selects native storage where supported. Other settings include `AIU_CONFIG_DIR`, `CLAUDE_CONFIG_DIR`, `AIU_CLAUDE_SERVICE`, and `CODEX_HOME`.

The default AIU directory is `%APPDATA%\aiu-rs` on Windows and `~/Library/Application Support/aiu-rs` on macOS. `AIU_STORE=file` is plaintext; native Windows storage uses a separate `tokens.dpapi` file. Changing backends does not migrate token data. The fork uses a separate directory from upstream: re-add your current login or sign in again to populate it.

Usage is cached across processes under native file locks. Normal polling is limited to one request per account every five minutes. A 429 starts a ten-minute cooldown, doubling up to one hour, and honors longer `Retry-After` values. Recent throttling doubles normal request spacing. Cached failures are marked stale, and revoked logins remain visible as needing sign-in. Refresh hand-back re-reads the live token so it only updates a CLI still holding the token that was spent.

The active CLI owns its shared login: background monitoring adopts newer CLI credentials and leaves their automatic rotation to the CLI. AIU refreshes independent saved credentials when needed. If the active login expires, open its CLI to renew it, then refresh AIU. Explicit **Add current** and **Switch** operations may refresh and update CLI credentials; avoid running CLI login/logout or another account switch concurrently with these operations. AIU's locks coordinate AIU processes, and its hand-back token check is not atomic relative to external programs that ignore those locks.

Every successful rotation is saved in the protected token backend before hand-back or profile lookup. If an import fails afterward, the replacement remains in pending storage without an unverified account index entry. Retrying **Add current** recovers it when the live login matches that token lineage. Independent pending imports do not replace one another. A new credential generation clears rejection of the previous generation while retaining request spacing and throttling.

Background polling owns a separate lock. Account ownership prevents duplicate refreshes and races with removal, while shared state locks are released during HTTP requests. A slow request for one account does not hold the lock used to commit unrelated account commands.

Recommendations prioritize weekly availability, session availability, flagship-model access when reported, and then remaining weekly capacity weighted by explicitly stated plan multipliers.

Explicit locks remain blocking even when utilization is unknown. Unknown capacity is excluded from recommendations. Usage older than 15 minutes remains visible but is not used for recommendations; among usable accounts, fresh evidence is preferred over a cached response marked stale.

## JSON compatibility

JSON status output is an array of Rust `AccountView` records. Window fields use camelCase (`resetsAt`), `known` records whether the provider supplied a usable percentage, and `fetchedAt` is an integer Unix timestamp in milliseconds. The output does not include credentials or the original raw usage response; use normalized `windows`. This shape differs from some fields of the legacy Go CLI's JSON array.

## Development

```sh
cargo fmt --all -- --check
cargo clippy --locked --all-targets --all-features -- -D warnings
cargo test --locked --all-features
cargo run -- gui --fixture fixtures/demo.json
```

On an interactive Windows desktop, run `powershell -File scripts/test-tray.ps1` after `cargo build` to verify native tray creation, hiding and restoring the panel, the menu actions, and quitting while hidden. Use `-BinaryPath target/release/aiu.exe` to check a release build.

`AIU_JSON_FIXTURE` also selects a fixture. Fixture mode skips credential access and provider requests. Tests use synthetic tokens, temporary homes and local mock servers. CI tests Windows, Linux and macOS, and uploads platform binaries as workflow artifacts. Linux desktop builds need `libxkbcommon-dev`, `libwayland-dev`, `libegl1-mesa-dev`, `libgtk-3-dev`, and `libayatana-appindicator3-dev`; installed Linux systems also need the matching `libayatana-appindicator3-1` runtime library. The panel requires a graphical session and the tray requires an AppIndicator or StatusNotifier host.

This is an unofficial fork of [getparable/aiu](https://github.com/getparable/aiu). Provider endpoints are internal and may change. No telemetry is sent. Manage only accounts you are authorized to use.

## License

AIU remains available under the original MIT license. See [`legacy/`](legacy/) for the historical implementation and documentation.
