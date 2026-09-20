# Windows and Linux CLI support

This implements the first milestone of [#10](https://github.com/getparable/aiu/issues/10).
Go owns account management, credentials, OAuth, usage, caching, and recommendations
on every platform. The existing SwiftUI app, macOS paths, status JSON, and
Homebrew workflow remain unchanged. The Rust desktop adapter and its process
contract are subsequent milestones, not dependencies of these CLI archives.

## Build and download

Build from source with the Go version in `go.mod`:

```sh
go test ./...
go build -o aiu ./cmd/aiu
```

On Windows, use `go build -o aiu.exe ./cmd/aiu`. Native Windows is supported
independently of WSL. `aiu menubar` remains macOS-only; on Windows/Linux use
`aiu watch` until the desktop adapter is available.

The CI workflow provides downloadable CLI artifacts. After a maintainer publishes
a release, the CLI workflow tests and attaches these archives to that same
[getparable/aiu release](https://github.com/getparable/aiu/releases):

| Platform | Archive | Execution coverage |
| --- | --- | --- |
| Windows 10/11, x86-64 | `aiu-<version>-windows-amd64.zip` | Native Windows tests |
| Windows 10/11, ARM64 | `aiu-<version>-windows-arm64.zip` | Cross-build only |
| Linux, x86-64 | `aiu-<version>-linux-amd64.tar.gz` | Native Ubuntu CI tests |
| Linux, ARM64 | `aiu-<version>-linux-arm64.tar.gz` | Cross-build only |

Each archive contains the executable, license, and this guide. Builds disable
cgo; the Linux CLI needs no desktop session, GTK, tray libraries, or glibc.
Opening a browser on Linux uses `xdg-open`; headless users can open the printed
URL themselves. Codex requires its localhost callback and does not support the
manual pasted-code flow. Claude supports `login --manual` where provider policy
permits it; see the main README's OAuth guidance.

Verify the SHA-256 manifest from the directory containing the downloads:

```sh
sha256sum --check aiu-<version>-checksums.txt
```

On PowerShell, compare `Get-FileHash <archive> -Algorithm SHA256` with the manifest.
Extract and run `aiu/aiu` or `aiu/aiu.exe`. `aiu link install` installs a shortcut
in `~/.local/bin`: Unix uses a symlink; Windows copies `aiu.exe` with a hash-based
ownership marker. Add that directory to your user PATH if `aiu link status`
reports it missing. AIU does not change persistent PATH settings. Windows refuses
to overwrite or remove a file whose ownership marker no longer matches. To
upgrade, run `link install` from the newly downloaded executable. `aiu update`
still checks upstream releases and never replaces the running executable.

## Paths and credentials

`AIU_CONFIG_DIR` always overrides the default. Without it:

- macOS keeps `~/.config/aiu` and Keychain exactly as before.
- An existing `~/.config/aiu` remains authoritative on Windows/Linux.
- New Windows installs use `%APPDATA%\aiu`.
- New Linux installs use `$XDG_CONFIG_HOME/aiu` when that variable is absolute,
  otherwise `~/.config/aiu`. Relative XDG paths are ignored.

Nothing is silently moved. `CODEX_HOME` and `CLAUDE_CONFIG_DIR` keep their existing
meanings. Windows and WSL are separate installations and credential environments;
Windows DPAPI stores cannot be decrypted by the Linux CLI.

Windows defaults to current-user DPAPI encryption in `tokens.dpapi`. AIU files
and its directory receive protected DACLs granting the current user access;
new temporary files are protected at creation. `AIU_STORE=file` explicitly opts
into plaintext `tokens.json`, still protected by Windows ACLs. On Linux the
initial backend is plaintext JSON with owner-only files/directories (0600/0700).
macOS retains Keychain unless `AIU_STORE=file` is selected.

If an old Windows plaintext store exists, DPAPI mode reports an error directing
you to `AIU_STORE=file`; it does not silently import, erase, or hide that store.
Automatic store conversion is not included. Keep one storage mode per directory.
Do not copy a Rust-port store into the Go store: migration from #9's different
schema is not part of this milestone.

External CLI credentials remain in the formats those CLIs expect. Updates retain
unrelated JSON fields and existing Windows file ACLs. On Unix, external credential
files stay 0600, while `.claude.json` retains its settings-file mode. Files are
flushed and replaced from a temporary file in the same directory. If Windows
refuses replacement because another process denies delete sharing, the command
fails with the old file intact. AIU's locks coordinate AIU processes; external
CLIs do not participate in those locks.

## Cancellation and automated checks

Ctrl+C ends browser callback waits and manual input waits. Once a token exchange
has begun, the CLI gives it and account persistence a bounded opportunity to finish
before exiting, so cancellation does not discard an otherwise successful issued
login. Provider/storage failures and abrupt process termination are separate cases;
#10's credential-recovery follow-up remains necessary. This console behavior is
not yet the frontend cancellation protocol planned for the Rust adapter.

Run `go test ./...` natively on Windows and Linux. Tests use temporary directories,
synthetic credentials, and loopback HTTP servers; no provider login is required.
Coverage includes import, browser/manual login, switch, sync, remove, status,
watch cancellation, release checking, subprocess cache reuse and locking,
Windows DPAPI, ACLs, and failed credential replacement. The unchanged macOS CI
job continues to build the universal SwiftUI app and Go backend.

## Manual smoke checklist

Use isolated `AIU_CONFIG_DIR`, `CLAUDE_CONFIG_DIR`, and `CODEX_HOME` directories
and a disposable test login. Record OS, architecture, binary version, and results.
Do not put credentials, authorization codes, or verifier values in logs/issues.
Native Windows and native Linux need separate results; WSL does not establish
native Windows behavior.

- [ ] Import a CLI login with `aiu add` / `aiu add --codex`; verify `aiu --json`.
- [ ] Complete browser login for each provider; verify `aiu status`.
- [ ] Complete Claude `aiu login --manual --no-open`, where supported.
- [ ] Cancel browser and manual waits with Ctrl+C; immediately start another login
      and confirm callback ports are released. Check browser denial too.
- [ ] Switch between two test accounts and verify the external CLI picks up the
      selected account and keeps unrelated settings.
- [ ] Refresh a login through the external CLI, run `aiu sync`, and verify adoption.
- [ ] Run two `aiu watch` processes and `aiu --json`; verify shared request spacing.
      Stop both watchers with Ctrl+C.
- [ ] Remove one account; confirm the other remains usable.
- [ ] Run `aiu update --force`; confirm it checks getparable/aiu without installing.
- [ ] Windows: repeat with default DPAPI and a separate explicit file-store directory.
- [ ] Linux: verify 0600 credential files and 0700 AIU directory permissions.

Live-provider approval, external CLI interoperability, ARM64 execution, and desktop
tray behavior must be recorded separately from fixture test results. This PR does
not claim those manual checks have passed.
