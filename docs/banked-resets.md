# Codex banked resets

AIU can display and redeem banked Codex resets for a tracked ChatGPT account.
The Go backend owns all requests, preferences, and redemption receipts. The
independent Rust frontend presents those results through the version 1 contract;
the Swift frontend continues to decode the existing account fields unchanged.

```sh
aiu resets codex:work
aiu reset codex:work --yes
aiu auto-reset codex:work --enabled true
aiu auto-reset codex:work --enabled false
```

Use an exact `codex:email#account-id` selector when labels are ambiguous.
`resets --json` returns the count and available credit details; `reset --json`
returns the provider outcome and request ID. To choose a particular credit, pass
`--credit-id ID`. Omit it to let Codex choose. A missing balance is unknown, not
zero, and the balance can exceed the number of credit details returned.

## Automatic resets

Auto reset is **off by default, per account**. When enabled, a successful fresh
usage response with **1% or less remaining (at least 99% used)** in an eligible
main Codex window and a positive reset balance can redeem one reset. Accounts
with only one reported main window are supported. Cached usage, unknown
percentages, and failed provider requests cannot initiate a new automatic spend.

The setting applies to collection by the CLI, `watch`, Swift, and Rust desktop or
tray. AIU must be running and collecting usage; this is not an OS background
service. The shared five-minute usage request spacing still applies, so the
trigger runs on the next eligible fresh reading, not at the exact instant usage
crosses the threshold. Multiple frontends share one preference and reset journal.

After a redemption attempt, AIU waits for a later fresh reading below the
threshold before arming another automatic spend. Delayed provider updates,
restart, repeated enabling, and turning the toggle off and back on do not create
another spend for that exhausted window. Uncertain outcomes reuse the same
request ID. An unresolved manual attempt pauses automatic spending. Removing an
account disables its auto preference but retains its redemption journal.

Enabling this setting authorizes automatic consumption of banked resets. A
successful reset refreshes eligible five-hour and weekly Codex usage windows and
moves the weekly reset date. The provider decides which windows need resetting;
`nothing_to_reset`, `no_credit`, and `already_redeemed` are displayed as their
actual outcomes. There is no refund action in AIU. See OpenAI's explanation of
[how banked Codex resets work](https://help.openai.com/en/articles/20001498-how-banked-codex-resets-work).

## Retry and cancellation

Before sending a redemption, AIU saves its request ID and optional credit ID in
the protected `banked-resets.json` state file beside its existing configuration.
The file contains reset metadata and preferences, not access or refresh tokens.
An uncertain result is shown as pending; retry with the same request:

```sh
aiu reset codex:work --yes --request-id UUID
# Include the same --credit-id ID if the original attempt selected a credit.
```

If the CLI generated the request ID, retrieve the pending request with
`aiu resets codex:work --json`. The Rust panel offers **Retry same request** and
retains this intent across errors. A different intent cannot replace an unresolved
one. Completed receipts prevent local duplicate POSTs; the provider request ID
also protects retries after an ambiguous network or persistence failure.

Cancellation before dispatch stops the request. Once dispatch starts, AIU finishes
receiving and journaling the result within a bounded timeout. Frontend Quit waits
for the child to settle. Do not forcibly kill it or delete the journal to clear a
pending outcome. Reset operations preserve the independent provider 429 cooldown;
successful redemption marks old usage stale rather than inventing zero usage.
Retry-After accepts seconds and HTTP dates; extreme delays saturate at one year
to prevent arithmetic overflow.

## Compatibility and verification

These are the internal WHAM routes used by the
[official Codex client](https://github.com/openai/codex/blob/main/codex-rs/backend-client/src/client/rate_limit_resets.rs),
not a stable public API. Unsupported or malformed responses remain errors or
unknown data. Reset support is Codex-only and requires an eligible account with
banked credits. AIU does not purchase credits or move them between accounts.

Automated tests use isolated stores and synthetic HTTP responses. They check
request paths, authorization and account headers, JSON bodies, idempotency,
unknown data, cancellation, shared throttling, and the automatic reset latch.
No real credits are consumed in tests. Native Windows tests also exercise the
protected file primitives used by the journal. Live provider redemption requires
an eligible account and intentionally spends a finite reset; it is not part of CI.
