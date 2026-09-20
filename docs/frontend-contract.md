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

Commands are `status`, `add`, `login`, `switch`, `remove`, and `sync`.
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

Successful status returns `accounts: []` for an empty store. Rows are the existing
`aiu --json` shape; `windows[].known` is an additive boolean distinguishing an
unknown percentage from zero. The frontend displays Go's `recommended`, `why`,
`canSwitch`, login state, stale messages, and usage values without recomputing them.
Go-produced fixtures in `tests/fixtures/frontend` are decoded by the Swift model
and copied into the Rust repository's contract tests.

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

The frontend must keep draining stdout, send cancellation on shutdown, and wait
for and reap its child instead of killing it during credential exchange. Run at
most one child per frontend; a 60-second display refresh still uses Go's shared
five-minute request spacing and independent 429 cooldown. Cancellation stops cache
waits and ordinary HTTP requests; locks retain their bounded 15-second wait.
The native subprocess tests cover stdin cancellation, EOF, released callback
ports, malformed controls, isolated credential storage, and cache sharing with
the normal CLI.
