# AIU

Rate limits and reset times for several **Claude** (Pro/Max) and **ChatGPT/Codex** accounts in one place — as a CLI and a Liquid Glass menu bar panel on macOS 26.

- Live 5-hour and weekly windows per account, straight from the endpoints `/usage` and `/status` use.
- Keeps its own copy of each account's login, so signing Claude Code or Codex into another account never loses one.
- `aiu switch` points Claude Code or Codex at any tracked account; running Claude Code sessions follow within about 30 seconds.

> **Unofficial.** AIU is not affiliated with, endorsed by, or supported by Anthropic or OpenAI. It uses internal, undocumented endpoints and the same OAuth clients as Claude Code and the Codex CLI; either company can change or block that at any time. Check the terms of your subscriptions before using it — Anthropic's consumer terms restrict using Claude Pro/Max OAuth tokens in other tools. Use at your own discretion.

## Install

```sh
make install        # builds AIU.app into ~/Applications and links ~/.local/bin/aiu
```

A downloaded `AIU.app` carries the CLI inside it; **Settings → Terminal command → Install**
adds the `~/.local/bin/aiu` shortcut (same as `aiu link install`).

Requires macOS 26, Go 1.27+ and Xcode 27 (Swift 6.4) to build.

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
aiu --json                     # for scripts and status bars
```

For a second account, sign in from a private browser window so the sign-in page does not reuse the account your browser is already logged in to.

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
   make release VERSION=0.2.0 BUNDLE_ID=com.example.aiu
   ```
   This runs the tests, builds a universal app, signs both binaries with the hardened runtime, notarizes, staples, and writes `dist/AIU-0.2.0.zip`.

## Trademarks

Claude is a trademark of Anthropic, PBC. OpenAI and Codex are trademarks of OpenAI. Their logos (via [Simple Icons](https://simpleicons.org), whose [disclaimer](https://github.com/simple-icons/simple-icons/blob/develop/DISCLAIMER.md) applies) are used unmodified and only to label which provider's usage is shown. AIU is not affiliated with or endorsed by either company, and the marks will be removed on request. The AIU app icon contains no third-party marks.
