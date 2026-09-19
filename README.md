# aiu-rs

AIU is a Windows-first, cross-platform Rust monitor for Claude Code and Codex accounts. It shows login health, session and weekly usage windows, and a recommendation for the next account. The original Go implementation is retained under [`legacy/`](legacy/); the Rust fork is the active build.

## Install

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

Use `provider:email#organization-id` to disambiguate accounts. Start a new CLI session after switching. The desktop panel provides filters, usage bars, reset times, recommendations, browser sign-in, adding the current login, switching, and forgetting accounts. It is a regular window; tray integration and autostart are not included. `menubar` is an alias for `gui`.

## Storage and settings

Windows stores encrypted token data with DPAPI for the current Windows user, macOS uses the `aiu-rs` Keychain item, and Linux uses owner-only files below `$XDG_CONFIG_HOME/aiu-rs` (normally `~/.config/aiu-rs`). Claude's files and Codex's `auth.json` remain in their normal locations. `AIU_STORE=file` selects the owner-only file store; `AIU_STORE=native` selects native storage where supported. Other settings include `AIU_CONFIG_DIR`, `CLAUDE_CONFIG_DIR`, `AIU_CLAUDE_SERVICE`, and `CODEX_HOME`.

The default AIU directory is `%APPDATA%\aiu-rs` on Windows and `~/Library/Application Support/aiu-rs` on macOS. `AIU_STORE=file` is plaintext; native Windows storage uses a separate `tokens.dpapi` file. Changing backends does not migrate token data. The fork uses a separate directory from upstream: re-add your current login or sign in again to populate it.

Usage is cached across processes under native file locks. Normal polling is limited to one request per account every five minutes. A 429 starts a ten-minute cooldown, doubling up to one hour, and honors longer `Retry-After` values. Recent throttling doubles normal request spacing. Cached failures are marked stale, and revoked logins remain visible as needing sign-in. Refresh hand-back re-reads the live token so it only updates a CLI still holding the token that was spent.

Recommendations prioritize weekly availability, session availability, flagship-model access when reported, and then remaining weekly capacity weighted by explicitly stated plan multipliers.

## JSON compatibility

JSON status output is an array of Rust `AccountView` records. Window fields use camelCase (`resetsAt`), `known` records whether the provider supplied a usable percentage, and `fetchedAt` is an integer Unix timestamp in milliseconds. The output does not include credentials or the original raw usage response; use normalized `windows`. This shape differs from some fields of the legacy Go CLI's JSON array.

## Development

```sh
cargo fmt --all -- --check
cargo clippy --locked --all-targets --all-features -- -D warnings
cargo test --locked --all-features
cargo run -- gui --fixture fixtures/demo.json
```

`AIU_JSON_FIXTURE` also selects a fixture. Fixture mode skips credential access and provider requests. Tests use synthetic tokens, temporary homes and local mock servers. CI tests Windows, Linux and macOS, and uploads platform binaries as workflow artifacts. Linux desktop builds may need `libxkbcommon-dev`, `libwayland-dev` and `libegl1-mesa-dev`; the panel requires a graphical session.

This is an unofficial fork of [getparable/aiu](https://github.com/getparable/aiu). Provider endpoints are internal and may change. No telemetry is sent. Manage only accounts you are authorized to use.

## License

AIU remains available under the original MIT license. See [`legacy/`](legacy/) for the historical implementation and documentation.
