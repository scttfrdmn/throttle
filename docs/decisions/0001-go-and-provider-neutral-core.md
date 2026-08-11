# ADR 0001: Go and a provider-neutral core

Status: accepted

## Decision

Implement throttle in Go. Keep budgeting, pacing, accounting, reservations, and enforcement independent of any provider SDK.

AWS Bedrock is the first integration/proving ground, not a core dependency.

## Consequences

- provider-specific behavior stays in adapter packages
- the same budget/ledger engine can govern Bedrock, OpenAI, Anthropic, Gemini, or future endpoints
- provider adapters may evolve independently
- tests for budget math require no network/provider credentials
