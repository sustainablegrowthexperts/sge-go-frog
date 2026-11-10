---
name: write-change-spec
description: Synthesizes the full discussion from a clarify-change session into a single, comprehensive Change Summary Specification document written as a Markdown file in specs/. Covers objective, scope, architecture, affected files, and a proposed task breakdown. Use this at the end of a clarify-change session or whenever the user wants a written spec from discussion notes.
---

## What I Do

When a clarify-change session has concluded and the user wants a written specification, I synthesize the entire discussion into a structured Change Summary Specification document and save it to `specs/YYYY-MM-DD-<slug>.md`.

**I will:**
- Read any reference materials or discussion notes the user mentions
- Extract decisions, trade-offs, and constraints from the conversation
- Structure them into the canonical 5-section template
- Write the file to the `specs/` directory
- Tell the user where the file was saved

**I will NOT:**
- Ask clarifying questions — the discussion is done, the spec is the output
- Reopen decisions already made — I am a scribe, not an interviewer
- Summarize prematurely — I capture what was decided, not what I think was decided

## When to Use Me

Use this skill:
- At the **end of a clarify-change session** when the user says "generate the spec" or "write the specification"
- When the user provides discussion notes or a transcript and says "turn this into a spec"
- Any time the user wants a written Change Summary Specification from already-completed discussions

Do NOT use this skill during an active clarify-change session — the clarifying questions must be exhausted first.

---

## Output Location

Specs are saved to `specs/YYYY-MM-DD-<slug>.md` at the project root.

- **Date:** today's date in ISO format (YYYY-MM-DD)
- **Slug:** derived from the change name, lowercase kebab-case (e.g., `seo-service-page-writer-v2`, `add-dark-mode-toggle`)
- If the `specs/` directory doesn't exist, create it
- If a spec with the same slug already exists for the same date, append a counter: `<slug>-2.md`, `<slug>-3.md`, etc.

---

## Specification Template

Every spec MUST follow this 5-section structure:

### 1. Objective & Overview
High-level description of what we are building and why. One paragraph max. Answer: what problem does this solve? Who benefits?

### 2. Scope Boundaries
Two tables: **In Scope** and **Out of Scope**. Each row is an item and a rationale.

```markdown
### In Scope

| Item | Detail |
|---|---|
| ... | ... |

### Out of Scope

| Item | Reason |
|---|---|
| ... | ... |
```

### 3. Technical Architecture & Decisions
Key patterns, data structures, or technical choices agreed upon. A table with columns: **Decision** and **Rationale**.

```markdown
| Decision | Rationale |
|---|---|
| ... | ... |
```

### 4. Affected Files

```markdown
| File | Action |
|---|---|
| `path/to/file.py` | Create / Modify / Delete — brief reason |
```

**IMPORTANT — Secrets and environment configuration:** If the change requires environment variables or secrets, list ONLY the `.env.example` template in the Affected Files table. Never list `.env` as a file to create or modify — it is always gitignored and created manually by the developer from the template. The task breakdown should instruct the implementer to create/update the `.env.example` template; the developer copies it to `.env` themselves. Example:

```markdown
| `client-api/.env.example` | Create — template with placeholder keys; developer copies to `.env` and fills in secrets manually |
```

### 5. Proposed Task Breakdown
An ordered, numbered list of atomic units of work grouped into phases. Each item is a concrete, actionable task — not a vague directive.

```markdown
### Phase 1 — Scaffolding

1. **Task name** — One-sentence description of what to do.
2. **Task name** — ...

### Phase 2 — Core Logic
...
```

---

## Interaction Rules

1. **Confirm the slug.** Before writing, propose the slug to the user: "I'll save this as `specs/2026-07-21-seo-service-page-writer-v2.md` — sound right?" Accept corrections.

2. **Write the file.** Use the Write tool. Create `specs/` if it doesn't exist.

3. **Report the path.** After writing, tell the user: "Spec saved to `specs/YYYY-MM-DD-<slug>.md`."

4. **Never reopen decisions.** If the discussion had a gap, note it in the spec with `[NEEDS CLARIFICATION: ...]` — don't ask the user to resolve it unless they ask.

5. **One spec per invocation.** Don't generate multiple specs in one call without explicit instruction.
