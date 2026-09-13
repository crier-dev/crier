# Security Policy

## Supported versions

Crier is pre-1.0; only the latest `main` is supported. Please test against the
current `main` branch before reporting.

## Reporting a vulnerability

**Do NOT open a public GitHub issue for security problems.**

Use GitHub's private vulnerability reporting:
**https://github.com/crier-dev/crier/security/advisories/new**

Include: what you found, how to reproduce it, the impact you estimate, and
(ideally) a proof-of-concept. You will get an acknowledgment within **48
hours** and a fix or a remediation plan within **7 days** for critical
issues. Reporters are credited in the release notes unless they prefer
otherwise — please say so in your report.

## Scope

In scope: the crier server (`cmd/server`), the MCP bridge (`cmd/crier-mcp`),
the guard (message classification and its bypasses), signing/authentication
bypasses, and the shipped scripts under `scripts/`.

Out of scope: the standalone `examples/` demos run against local-only
servers, denial-of-service via resource exhaustion without a novel vector,
and findings that require operator misconfiguration (e.g. running with
`CR_REQUIRE_AGENT_SIG=false` on a public interface).

## Known trade-offs (not vulnerabilities)

Crier is deliberately pragmatic about two things; treating them as bugs
will get the report closed as "works as documented":

- **Guard fails open without an LLM key.** When no guard provider is
  configured (or the provider errors), delivery proceeds and is marked
  `X-Crier-Guard-Error: true`. The guard is a content filter, not an
  authentication boundary — deployments that need delivery guarantees under
  provider failure should use `fail_closed` policies.
- **`CR_REQUIRE_AGENT_SIG=false` disables request signing.** Several demos
  and test batteries run this way for convenience. It is a deployment
  choice, documented in the README; the default is `true`.
