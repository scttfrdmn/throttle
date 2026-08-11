# Collaboration workflow

This project is intentionally being developed as a collaboration between Scott, Claude Code, and ChatGPT.

## Roles

### Scott

- product owner
- resolves scope and product tradeoffs
- decides when future ideas enter implementation scope

### Claude Code

- primary implementation agent
- owns repository coherence while coding
- runs tests and validates changes
- documents concrete implementation decisions and open questions

### ChatGPT

- architecture/design reviewer
- UX and product-thinking collaborator
- independent implementation review when requested
- helps challenge abstractions before they calcify

## Handoff format

When handing a completed slice to ChatGPT for review, provide:

```text
Goal:
What changed:
Key files:
Tests:
Important decisions:
Known limitations:
Open questions:
```

When ChatGPT recommends changes, Claude Code should distinguish:

- correctness issue
- v0.1 scope improvement
- future-policy idea
- optional preference

Do not implement future-policy items simply because they are interesting; record them in the appropriate document and ask Scott when scope is ambiguous.
