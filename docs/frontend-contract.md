# Frontend JSONL contract

Go is the sole owner of credentials, OAuth, provider requests, the shared cache,
and recommendations. A frontend launches its known bundled Go executable with
an argument array. It must not read the account/token files or call providers.
The independently released [Rust frontend](https://github.com/krflol/aiu-desktop)
uses this contract; the SwiftUI app continues to consume compatible `aiu --json`.

Run one child per operation:

```text
aiu frontend status --contract-version 1
aiu frontend login --provider claude --label Work --contract-version 1
aiu frontend switch claude:person@example.test#organization --contract-version 1
```

Commands are `status`, `add`, `login`, `switch`, `remove`, `sync`, `resets`,
`reset`, and `auto-reset`.
`--provider claude|codex` narrows provider selection. `switch` and `remove`
require one full account selector; append `#` even for an empty organization.
`--label` applies to import/login. Browser login is supported; use the normal
terminal `login --manual` for manual Claude authorization. `--no-open` is intended
for controlled tests; the contract deliberately never emits the authorization URL.

Stdout contains JSON Lines only. Each line has `version: 1` and an `event`:

| Event | Fields |
| --- | --- |
| `hello` | `capabilities`: supported commands plus `cancel` |
| `progress` | `phase`: `working`, `waiting`, or `exchanging`; optional redacted `message` |
| `result` | `ok`, `cancelled`, optional `message`, `accounts`, optional `error: {code, message}` |

`hello` is always first and `result` is terminal. Clients must request version 1
and validate the hello version/capabilities. An unsupported requested version is
rejected before any operation. Clients must tolerate additive fields. Error codes
are `invalid_command`, `invalid_input`, `unsupported_version`, `operation_failed`,
and `cancelled`. Exit statuses are 0 (success), 1 (operation failed), 2 (invalid
request/version), and 130 (cancelled).

`--help` and `--version`, including their short forms, return a successful
informational result after `hello` without executing the requested command.

Successful status returns `accounts: []` for an empty store. Rows are the existing
`aiu --json` shape; `windows[].known` is an additive boolean distinguishing an
unknown percentage from zero. The frontend displays Go's `recommended`, `why`,
`canSwitch`, login state, stale messages, and usage values without recomputing them.
Go-produced fixtures in `tests/fixtures/frontend` are decoded by the Swift model
and copied into the Rust repository's contract tests.

Codex rows add an optional `bankedResets` object: `availableCount` is nullable,
`credits` is nullable when details have not been fetched, and `canRedeem` is Go's
decision. Each credit contains `id`, `resetType`, `status`, `grantedAt`, `expiresAt`,
`title`, `description`, and `canRedeem`. Optional strings `fetchedAt`, `stale`, and
`error` describe cached details. `autoReset` defaults false;
`autoResetThresholdPercent` is the per-account whole remaining percentage (0–99),
defaulting to 1 when absent in an older response. Explicit zero is valid.
`autoResetStatus`
explains the current automation state. `pendingRequest: {requestId, creditId?}`
is authoritative for retrying an uncertain redemption. Unknown counts and dates
must not be rendered as zero or an invented expiry.

```text
aiu frontend resets codex:person@example.test#account-id --contract-version 1
aiu frontend reset codex:person@example.test#account-id --yes --request-id UUID --contract-version 1
aiu frontend auto-reset codex:person@example.test#account-id --enabled true --contract-version 1
aiu frontend auto-reset codex:person@example.test#account-id --threshold 5 --contract-version 1
```

These commands require one selector and advertise separate hello capabilities.
`reset` requires explicit frontend confirmation before passing `--yes`. Generate
one UUID per confirmed intent and retain it for every retry, including after
network failure. Optional `--credit-id ID` selects a credit; omission lets the
provider choose. Never change that selection while retrying the same ID. A Go
pending request takes precedence over a local intent that was never accepted.
`auto-reset` requires `--enabled true|false`, `--threshold N`, or both. Omitting a
setting preserves it; supplied settings are validated and saved atomically.
The frontend displays the saved settings and never implements the trigger policy
or calls the consume route itself. Editing a percentage does not submit a command
until the user saves it. Preferences never clear pending requests or the recovery
requirement from a prior attempt.
See [reset policy and recovery](banked-resets.md). Status uses the usage response's
embedded balance plus cached details; it does not issue an extra listing request
on every poll.

Mutations return a refreshed snapshot when available. If a mutation committed but
snapshot collection was cancelled or failed, the result remains `ok: true`, with
`accounts: null` and a message ending in `snapshot unavailable`. Keep the prior
display and refresh later; do not repeat a completed login. Other events and
failed results also use `accounts: null`. Credentials and authorization codes
never belong in presentation events, arguments, or frontend logs.

Keep stdin open for the operation's lifetime. Send `{"cancel":true}` followed by
a newline to cancel; an optional `version: 1` is accepted. EOF also cancels, so
an abruptly terminated parent relinquishes its child without OS-specific signals.
Malformed/unknown controls or a line over 8 KiB cancel with `invalid_input`.

Cancel/Quit closes pending OAuth listeners. Closing a browser tab cannot be
observed by a loopback listener: the user can press Cancel, provider denial ends
the wait, and the callback timeout is five minutes. Once credential exchange or
rotation begins, Go shields that operation from cancellation long enough to receive
and persist its response, bounded by its one-minute operation context and HTTP
timeouts. A completed login is reported as success even if cancellation arrived
during exchange. Storage/profile failures remain ordinary errors; abrupt OS kill
or machine failure cannot guarantee persistence.

Banked-reset POSTs use the same cancellation boundary: cancel before dispatch,
then finish receiving and saving the issued outcome before exiting. A successful
terminal redemption remains successful if its follow-up snapshot is cancelled.
Do not treat Cancel as evidence that a dispatched reset was not consumed.

The frontend must keep draining stdout, send cancellation on shutdown, and wait
for and reap its child instead of killing it during credential exchange. Run at
most one child per frontend; a 60-second display refresh still uses Go's shared
five-minute request spacing and independent 429 cooldown. Cancellation stops cache
waits and ordinary HTTP requests; locks retain their bounded 15-second wait.
The native subprocess tests cover stdin cancellation, EOF, released callback
ports, malformed controls, isolated credential storage, and cache sharing with
the normal CLI.
