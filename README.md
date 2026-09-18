# AIU

Rate limits and reset times for several **Claude** (Pro/Max) and **ChatGPT/Codex** accounts in one place — as a CLI and a Liquid Glass menu bar panel on macOS 26.

- Live 5-hour and weekly windows per account, straight from the endpoints `/usage` and `/status` use.
- Keeps its own copy of each account's login, so signing Claude Code or Codex into another account never loses one.
- `aiu switch` points Claude Code or Codex at any tracked account; running Claude Code sessions follow within about 30 seconds.

> **Unofficial.** AIU is not affiliated with, endorsed by, or supported by Anthropic or OpenAI. It uses internal, undocumented endpoints and the same OAuth clients as Claude Code and the Codex CLI; either company can change or block that at any time. Check the terms of your subscriptions before using it — Anthropic's consumer terms restrict using Claude Pro/Max OAuth tokens in other tools. Use at your own discretion.

## Install

```sh
brew install getparable/tap/aiu
```

Then put the menu bar app where macOS looks for apps, so Launch at Login and Spotlight
find it. That path stays valid across upgrades, so the link survives `brew upgrade aiu`:

```sh
ln -sfn "$(brew --prefix aiu)/AIU.app" ~/Applications/AIU.app
open ~/Applications/AIU.app
```

On Apple Silicon this pours a prebuilt bottle in a couple of seconds — no compiler and
no Xcode. An Intel Mac has no bottle and builds from source instead, which is what the
Xcode requirement below is for.

Either way the app is built or packaged outside a browser download, so it carries no
quarantine flag and Gatekeeper never asks. It is ad-hoc signed, not notarized.

From a clone instead:

```sh
make install        # builds AIU.app into ~/Applications and links ~/.local/bin/aiu
```

A downloaded `AIU.app` carries the CLI inside it; **Settings → Terminal command → Install**
adds the `~/.local/bin/aiu` shortcut (same as `aiu link install`).

Building from source requires macOS 26, Go 1.27+ and Xcode 27 — the panel needs the
Swift 6.4 toolchain, so Xcode 26 is not enough. Pouring the bottle requires none of it.

## Use

```sh
aiu add                        # track the account Claude Code is signed in to
aiu add --codex                # track the account Codex is signed in to
aiu login --label work         # browser sign-in for another Claude account
aiu login --codex              # …or another ChatGPT account
aiu                            # all accounts, all windows, reset times
aiu watch                      # live view
aiu switch work                # point Claude Code at a tracked account
aiu switch codex:alt           # same for Codex
aiu list | remove <name> | sync | whoami
aiu update                     # is there a newer release?
aiu --json                     # for scripts and status bars
```

For a second account, sign in from a private browser window so the sign-in page does not reuse the account your browser is already logged in to.

## Updates

`aiu update` says whether a newer release exists and prints the command that installs
it. **Settings → Check for updates automatically** does the same thing every six hours
and shows the result in the panel; turn it off and nothing is ever requested.

aiu never replaces itself. A Homebrew install belongs to Homebrew, so it tells you to
run `brew upgrade getparable/tap/aiu`; a build from a clone is told to pull and
`make install`. The check reaches GitHub's public release endpoint and nothing else,
answers from a six-hour cache shared between the panel and the terminal, and a check
that fails never claims an update is available.

## What "use next" means

Both the CLI summary and the menu bar panel name one account per provider to work in
next, and the panel puts a **Switch** button beside it. The ranking is deliberately
lexicographic — two gates first, capacity second — so no plan can buy its way past a
limit that is shut:

1. **Weekly room.** An account whose 7d window is gone is out of the running entirely.
   When every account is gone, the pick becomes the one that comes back first and says
   so instead of recommending a dead account.
2. **The 5h window.** An account that has hit its session limit ranks below every
   account that has not, however large its plan.
3. **The flagship model.** An account out of Fable — or on a plan that has no Fable
   window at all — ranks below one that still has it. The gate only applies when some
   account in the comparison reports such a window, so it never fires on Codex.
4. **Capacity, not percentage.** Within a band, the winner is `7d percent left × plan
   multiplier`: a half-spent Max 20x carries more work than an untouched Pro plan. Only
   a multiplier the plan states itself counts (`Max 20x` → 20, `Max 5x` → 5); every
   other plan weighs one, so those rank on percentages alone.

`aiu --json` carries the verdict as `recommended`, `why` and `allSpent` on the winning
account, so the panel and the CLI can never disagree about it.

## How it stays polite

Reading usage costs no quota, but the endpoints throttle hard. Every AIU process on the machine — CLI, `watch`, the menu bar — shares one cache under a file lock: each account is read at most once every 5 minutes, and a 429 backs that account off for 10 minutes, doubling to an hour.

## Where things live

| What | Where |
| --- | --- |
| Token copies | macOS Keychain, service `aiu` (or `~/.config/aiu/tokens.json` with `AIU_STORE=file`) |
| Account index, cache, lock | `~/.config/aiu/` |
| Claude Code's login (read; written by `switch` and refresh hand-back) | Keychain `Claude Code-credentials`, `~/.claude.json` |
| Codex's login (same) | `~/.codex/auth.json` |

Keychain access may prompt for approval when AIU first reads or updates an existing item. If AIU creates Claude Code's Keychain item during a switch, macOS may ask Claude Code to approve access on its next read. Release builds enable cgo for native Keychain access. Builds without cgo cannot read Keychain items, even when AIU's own store uses `AIU_STORE=file`.

Tokens are only ever sent to Anthropic's and OpenAI's own hosts. There is no telemetry.

## Developing the panel

`AIU_JSON_FIXTURE=/path/to/aiu.json` makes the panel render a saved `aiu --json`
instead of calling the CLI — useful for checking a layout (several organizations on
one address, an expiring login) without touching real accounts.

## Layout

```
cmd/aiu            CLI entry point
internal/core      tokens, OAuth, usage fetch, throttle, switch (tested with fakes)
internal/cli       terminal rendering and commands
macos/AIUBar.swift the SwiftUI panel — a front end that runs the bundled aiu binary
macos/icon         the app icon source (make icon)
```

## Releasing

Distributing to other Macs needs a **Developer ID Application** certificate and notarization.

1. **Certificate** (once): Xcode → Settings → Accounts → your team → Manage Certificates → **+** → *Developer ID Application*. Only the team's Account Holder can create one; on a team account, ask them.
2. **Notary credentials** (once): create an app-specific password at [account.apple.com](https://account.apple.com) → Sign-In and Security, then
   ```sh
   xcrun notarytool store-credentials aiu --apple-id you@example.com --team-id YOURTEAMID
   ```
3. **Release**: set the bundle id to a domain you own, then
   ```sh
   make release VERSION=0.1.3 BUNDLE_ID=com.example.aiu
   ```
   This runs the tests, builds a universal app, signs both binaries with the hardened runtime, notarizes, staples, and writes `dist/AIU-0.1.3.zip`.

## Trademarks

Claude is a trademark of Anthropic, PBC. OpenAI and Codex are trademarks of OpenAI. Their logos (via [Simple Icons](https://simpleicons.org), whose [disclaimer](https://github.com/simple-icons/simple-icons/blob/develop/DISCLAIMER.md) applies) are used unmodified and only to label which provider's usage is shown. AIU is not affiliated with or endorsed by either company, and the marks will be removed on request. The AIU app icon contains no third-party marks.

## License

[MIT](LICENSE) © 2026 Michael Visser.

aiu stores and switches credentials for accounts you already hold. It is for one
person managing their own logins; using it to work around a provider's limits is
between you and that provider's terms, not something this license speaks to.
