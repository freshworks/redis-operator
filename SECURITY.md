# Security policy

## Supported versions

Security fixes are applied to:

- The default development branch (`master`), and
- The **latest** published release line (the most recent `vX.Y.Z` tag).

Older release tags may not receive backports except for critical issues at
maintainer discretion. If you depend on an older line, plan upgrades to a
supported version or coordinate with maintainers via a private report.

**Out of scope for “supported” guarantees:** fork-only tags, experimental
builds, or images not published from this repository’s documented release
process.

## How to report a vulnerability

**Do not** open a public GitHub issue for an undisclosed security vulnerability.

Please report privately using **[GitHub Security Advisories](https://github.com/freshworks/redis-operator/security/advisories/new)**
(**Security** tab → **Report a vulnerability**) so the maintainers can assess
and coordinate a fix before public disclosure.

If you cannot use GitHub’s private reporting flow, contact repository
maintainers through an organization-approved private channel (see
[MAINTAINERS.md](MAINTAINERS.md)) and ask to route the report securely.

## Response expectations

- **Initial triage:** we aim to acknowledge reports within **7 calendar days**.
  Complex reports may need more time for reproduction.
- **Fix timeline:** depends on severity, impact, and release coordination; we
  will communicate realistic expectations in the advisory thread.

These timelines are goals, not guarantees, but we treat security reports
seriously.

## Scope

**In scope**

- The operator controller binary and its default configuration as shipped in
  this repository
- The published container image(s) built from this repository’s release
  process
- The Helm chart defaults under `charts/redisoperator` when used as documented

**Out of scope**

- Vulnerabilities in Redis, Sentinel, or base container images you choose to run
  (report those to the respective vendors or image maintainers)
- Misconfiguration of Kubernetes clusters, RBAC, or network policies
- Issues in third-party Helm repositories or mirrors not controlled by this
  project

## Disclosure

We follow coordinated disclosure: details are published (for example via a
GitHub Security Advisory or release notes) after a fix is available, unless a
reporter and maintainers agree on a different timeline for a specific case.

## Automated checks (supply chain)

Maintainers may enable GitHub **Dependency graph** and related features for this
repository; those are optional org/repo settings and are not required for the
workflows in this tree. They never replace reporting suspected vulnerabilities
through the private channels above.

## Security-related configuration

Hardening guidance for deploying the operator in production belongs in
documentation and examples; this file focuses on vulnerability reporting and
supported versions.

## Go toolchain (stdlib fixes)

The module declares **`go 1.26.5`** in [`go.mod`](go.mod). CI uses
**`go-version-file: go.mod`** (`actions/setup-go`), so CI and contributors should
build with that same Go release. Run **`make ci-govulncheck`** after upgrades; some
advisories also require bumping dependencies (for example **`golang.org/x/net`**),
not only the Go toolchain.

Current toolchain and module pins (for example **`golang.org/x/net` v0.53.0+**)
address **reachable** reports such as:

| ID | One-line summary |
|----|------------------|
| GO-2026-4971 | Panic in `Dial` / `LookupPort` when handling NUL byte on Windows in `net` (fixed in `net` @ Go 1.25.10+). |
| GO-2026-4918 | Infinite loop in HTTP/2 transport with bad `SETTINGS_MAX_FRAME_SIZE` (`golang.org/x/net`, `net/http`; fixed in `x/net` v0.53.0+ and `net/http` @ Go 1.25.10+). |

Migrating from **Go 1.24.x** to the **1.25** line cleared additional **reachable**
stdlib findings (same database):

| ID | One-line summary |
|----|------------------|
| GO-2026-4947 | Unexpected work during chain building in `crypto/x509`. |
| GO-2026-4946 | Inefficient policy validation in `crypto/x509`. |
| GO-2026-4870 | TLS 1.3 KeyUpdate handling can retain connections (DoS) in `crypto/tls`. |
| GO-2026-4601 | Incorrect parsing of IPv6 host literals in `net/url`. |
| GO-2026-4341 | Memory exhaustion in query parameter parsing in `net/url`. |
| GO-2026-4340 | TLS handshake messages processed at wrong encryption level in `crypto/tls`. |
| GO-2026-4337 | Unexpected session resumption in `crypto/tls`. |
| GO-2025-4175 | Wildcard name verification ignored excluded DNS constraints in `crypto/x509`. |
| GO-2025-4155 | High resource use when printing host validation errors in `crypto/x509`. |
| GO-2025-4013 | Panic when validating certificates with DSA public keys in `crypto/x509`. |
| GO-2025-4012 | Cookie parsing without limits can exhaust memory in `net/http`. |
| GO-2025-4011 | DER parsing could exhaust memory in `encoding/asn1`. |
| GO-2025-4010 | Insufficient validation of bracketed IPv6 hostnames in `net/url`. |
| GO-2025-4009 | Quadratic cost parsing some invalid PEM in `encoding/pem`. |
| GO-2025-4008 | ALPN error strings could leak attacker-controlled data in `crypto/tls`. |
| GO-2025-4007 | Quadratic cost checking name constraints in `crypto/x509`. |
| GO-2025-3956 | Unexpected paths from `LookPath` in `os/exec`. |

Re-verify locally after upgrades: `make ci-govulncheck` (uses the toolchain from `go.mod`).
