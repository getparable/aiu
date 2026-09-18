# Security policy

Please report suspected vulnerabilities privately through [GitHub's private vulnerability reporting form](https://github.com/getparable/aiu/security/advisories/new). Include the affected version, reproduction steps, and potential impact. Do not put credentials or personal account data in the report.

Please do not open a public issue or pull request for an unpatched vulnerability. The maintainers will acknowledge the report through the advisory and coordinate a fix and disclosure there.

AIU stores token copies in the macOS Keychain by default. `AIU_STORE=file` uses an owner-only file at `~/.config/aiu/tokens.json`. When AIU first creates a Claude Code Keychain item, macOS may ask Claude Code to authorize access to that item. An existing item keeps its access settings when AIU updates it.
