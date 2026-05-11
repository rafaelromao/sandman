# Security Policy

## Supported Versions

| Version | Supported          |
| ------- | ------------------ |
| Latest  | :white_check_mark: |
| < Latest| :x:                |

Only the latest released version of Sandman receives security updates. Please ensure you are running the most recent version before reporting a vulnerability.

## Reporting a Vulnerability

**Please do not open public issues for security vulnerabilities.**

Instead, report security issues privately via email to: **sandman.support@gmail.com**

We aim to acknowledge receipt within 48 hours and will work with you to understand and resolve the issue. We follow a 90-day disclosure timeline from the date of acknowledgment.

### What to Include

When reporting a vulnerability, please include:

- A description of the vulnerability and its potential impact
- Steps to reproduce the issue
- The version of Sandman affected
- Any suggested remediation (if you have one)

## Scope

The following are in scope for security reports:

- Arbitrary command execution via malicious agent configuration
- Supply-chain attacks (e.g., via `go install` or dependency compromise)
- Privilege escalation in container sandboxes
- Information disclosure via event logs or worktree artifacts
- Unsafe handling of git credentials or SSH keys in sandboxed environments

## Out of Scope

- Issues in dependencies unless they directly affect Sandman's usage
- Social engineering or phishing attacks against maintainers
- Denial of service via resource exhaustion (unless it causes data loss)

## Security-Related Design Decisions

Sandman executes arbitrary commands configured via YAML and shells out to external tools (`gh`, `git`, and user-configured agent commands). This is an inherently high-trust surface. We mitigate this by:

- Requiring explicit user configuration before any agent is invoked
- Isolating agent execution in dedicated git worktrees
- Supporting optional Docker/Podman sandboxing for additional isolation
- Never executing agent commands without explicit user invocation

## Attribution

We will publicly credit reporters who responsibly disclose vulnerabilities, unless they prefer to remain anonymous.
