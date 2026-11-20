# Security Policy

ServerDock controls Docker and, through it, the hosts it manages. We treat security reports as the highest priority.

## Reporting a vulnerability

**Please do not open a public issue.** Report privately through GitHub:
**Security → Report a vulnerability** on [this repository](https://github.com/Docker-Hunterpedia/serverdock/security/advisories/new).

Include what you found, how to reproduce it, the affected version, and its impact. We aim to:

- acknowledge your report within **3 days**;
- confirm or rule out the issue within **10 days**;
- release a fix and publish an advisory, crediting you if you wish.

## Supported versions

ServerDock is in the planning stage and has no releases yet. Once releases begin, the latest minor release gets security fixes.

## Scope

In scope: the control plane, the agent, the agent ↔ control-plane channel, the web UI, the REST API, the MCP endpoint, the installer, and the update mechanism.

Design rules and the threat model are in [docs/security.md](docs/security.md).
