# Security policy

Report suspected vulnerabilities privately to the maintainer of [krflol/aiu-rs](https://github.com/krflol/aiu-rs). Include the affected version, reproduction steps, and potential impact. Do not put credentials or personal account data in a public report.

AIU is an unofficial client of provider endpoints. It stores token copies locally: Windows uses DPAPI for the current Windows user, macOS uses the `aiu-rs` Keychain item, and Linux uses an owner-only file under the XDG configuration directory. `AIU_STORE=file` explicitly selects the owner-only file store on any platform. Keep that file and your OS account protected.

Tests use synthetic data and do not contain real accounts, tokens, or provider credentials. Do not open a public issue or pull request for an unpatched vulnerability; use private reporting so maintainers can coordinate a fix and disclosure.
