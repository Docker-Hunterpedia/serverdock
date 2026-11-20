# 0006 — MCP first, light built-in AI later

**Status:** accepted by the maintainer · **Date:** 2026-11-20

## Context
There are two ways to put AI in ServerDock:
- **MCP:** ServerDock exposes tools, and the user's own AI client (Claude Code, Claude Desktop) does the reasoning.
- **Built-in AI:** ServerDock calls Claude itself with the user's API key.

MCP is smaller, needs no API key in ServerDock, and fits the footprint promise. But it can't act when nobody is chatting, and it can't serve users who have no AI client.

## Decision
1. Build **one typed action registry**: tools with a JSON schema, a risk tier, a required scope, a dry run and an execute step. The UI, REST, CLI, MCP and any future built-in AI all use it.
2. **Phase 1:** read-only MCP with scoped `read` tokens.
3. **Phase 2:** MCP write actions. Every write returns a dry run marked `pending_approval`; the user approves it in the ServerDock panel; risk is enforced on the server, never trusted to the client.
4. **Phase 3:** a light built-in AI, only for what MCP can't do. That means automatic explanations of alerts and failed deploys, an "explain this" button, and chat for users without an AI client. It is a small loop over the same tools.

## Consequences
- No AI cost or key handling until Phase 3.
- Approval UX lives in our panel from Phase 2, so it works the same for MCP and the later built-in AI.
- Secret redaction and the "tool output is untrusted" rules apply from Phase 1, because MCP results reach external models too.
