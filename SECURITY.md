# Security policy

Ephemeris runs code you don't trust, so isolation bugs matter more than usual.

**Please don't open a public issue for a vulnerability.** Report it privately through GitHub: Security tab -> "Report a vulnerability". Include the spec, the backend (`ephemeris doctor` output helps) and steps to reproduce. You'll get a reply within a week.

In scope: sandbox escapes, egress allowlist bypasses, secrets leaking to disk, logs or env, and sandboxes surviving teardown.

Out of scope: host kernel bugs (use `runtime: runsc` for a stronger boundary), and anything a sandbox sends through a host you allowlisted.