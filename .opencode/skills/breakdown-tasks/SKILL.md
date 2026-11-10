---
name: breakdown-tasks
description: Reads a Change Summary Specification from specs/ and breaks it into vertical-slice task files under .tasks/. Every task is an end-to-end tracer bullet — never a horizontal layer. Produced files have YAML frontmatter with id, title, status, depends_on, and target_files, plus Objective, Layer-by-Layer Changes, TDD Execution Plan (RED/GREEN/REFACTOR), and Definition of Done sections. Strict TDD enforced: tests MUST be written first and observed to fail before any implementation code is written.
---

## What I Do

When the user invokes this skill (e.g., `/breakdown-tasks` or "break this spec into tasks"), I read the project's Change Summary Specification from `specs/`, then generate a set of vertical-slice task files under `.tasks/`. Each task implements one thin, end-to-end slice of working functionality — touching every tier necessary (DB / logic / API / UI / test) for that single feature.

**I will:**
- Read the spec file the user points to (or the most recent spec if none is specified)
- Analyze the spec's Proposed Task Breakdown (Section 5), Affected Files (Section 4), and Technical Decisions (Section 3)
- Decompose every unit of work into vertical slices — each task adds one user-visible or externally-testable capability
- Generate numbered task files (`001-slice-name.md`, `002-slice-name.md`, ...) in `.tasks/`
- Auto-continue from the next available number if `.tasks/` already contains files from a prior breakdown
- Write YAML frontmatter and four required body sections for every task (Objective, Layer-by-Layer Changes, TDD Execution Plan, Definition of Done)
- Every task MUST follow TDD: RED (write failing test first) → GREEN (minimum implementation) → REFACTOR (clean up)
- Report how many tasks were generated and where they live

**I will NOT:**
- Write or modify any application code — tasks are instructions, not implementations
- Create horizontal-layer tasks (e.g., "add all database schemas", "create all API routes")
- Skip the frontmatter or body sections for any task
- Skip the TDD Execution Plan for any task
- Generate a task that cannot be tested independently after completion

## When to Use Me

Use this skill:
- After a spec has been written and approved (post-`write-change-spec`)
- When the user says "break this down into tasks", "create task files", "/breakdown-tasks", or similar
- When the user wants a runnable task plan before implementation begins

Do NOT use this skill during the clarify-change phase — the spec must exist first.

---

## Core Rule: Vertical Slicing

**This is the hardest rule in the skill. Every task MUST be vertical — touching every layer needed for one feature. Horizontal-layer tasks are NEVER allowed.**

### Horizontal slicing (FORBIDDEN — never create these)

Tasks that touch only one layer of the stack:

| Forbidden pattern | Example |
|---|---|
| "Create all database schemas" | Touches only the DB — no logic, no API, not testable alone |
| "Set up all API routes" | Touches only the web layer — no DB backing, no UI, not testable |
| "Write all unit tests" | No feature — tests without implementation |
| "Add all frontend components" | No backend — UI with no data |
| "Define all models/types" | Schema-only — no behavior |

### Vertical slicing (REQUIRED — every task must follow this)

Each task implements a complete feature sliver that can be invoked, tested, and verified end-to-end:

| Required pattern | Example |
|---|---|
| Single-feature, all layers | "Add content_type input → wire into execute() → test via CLI" |

### The Litmus Test

After describing a task, ask yourself two questions:

1. **"Can someone check out this commit, run one or two shell commands, and see this feature working in isolation?"** If the answer is no, the task is not vertical enough. Decompose it further.
2. **"Can the TDD Execution Plan produce a real failing test BEFORE any implementation code exists?"** If the RED phase cannot produce a meaningful test failure, the slice is either too thin (trivial wiring with no behavior) or too thick (too many behaviors tested at once). Decompose accordingly.

---

## Output Location & Naming

- Save all task files to `.tasks/` at the project root (create the directory if missing)
- Name files sequentially with zero-padded numbers: `001-slice-name.md`, `002-slice-name.md`, `003-slice-name.md`, ...
- Derive the slug from the slice name (lowercase kebab-case, 4 words max)
- If `.tasks/` already has files from a previous breakdown, auto-continue numbering from the next available number (e.g., if the highest is `005-`, start at `006-`). Never overwrite existing files.

---

## Required Frontmatter Format

Every task file MUST begin with YAML frontmatter. This is non-negotiable:

```yaml
---
id: task-001
title: "[Slice] Short description"
status: pending
depends_on: [] # List prerequisite task IDs, e.g., ["task-000"]
target_files:
  - "path/to/file.py"
---
```

### Frontmatter rules

| Field | Rule |
|---|---|
| `id` | `task-001`, `task-002`, etc. — zero-padded sequential |
| `title` | `[Slice]` prefix followed by a concise description (one sentence) |
| `status` | Always `pending` — the implementer updates this |
| `depends_on` | Array of prerequisite task `id` strings. Empty array `[]` if no dependencies. Task N can only depend on task M where M < N |
| `target_files` | Array of file paths this task is expected to create or modify. Use project-relative paths. At minimum one file, typically 1-5 |

---

## Required Task Body Sections

Every task file MUST contain these four sections in this order:

### 1. Objective

A single sentence stating what this vertical slice delivers when complete. Format:

```
**Objective:** <One sentence describing the end-to-end capability this task adds.>
```

The objective must describe user-visible or CLI-testable behavior — not internal artifacts. 

Good: "Content type input is accepted, validated, and surfaced in the execution output via CLI."
Bad: "Add content_type field to Inputs class."

### 2. Layer-by-Layer Changes

Step-by-step instructions across the stack for this specific slice. Use these sub-headings adapted to the project's architecture. Not every slice touches every layer — include only the layers relevant to this task:

```markdown
### Layer-by-Layer Changes

#### Data / Schema
- ...

#### Core Logic / Engine
- ...

#### Process / SDK
- ...

#### API / Server
- ...

#### Frontend / Template
- ...
```

Each bullet is a concrete, actionable instruction an implementer can follow without guessing. Reference exact classes, functions, fields, and file paths from the spec.

**Do NOT include testing steps here.** Testing is covered in the TDD Execution Plan below.

### 3. TDD Execution Plan

Every task MUST prescribe a strict TDD workflow. The implementer writes the test first, observes it fail, then writes the minimum code to make it pass, then refactors. No implementation code may be written before the test.

Use this exact format:

```markdown
### TDD Execution Plan

#### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `tests/test_<name>.py` (or equivalent for the project's test framework)
- **Test execution command:**
  ```bash
  python -m pytest tests/test_<name>.py -v
  ```
- **Expected result:** Test MUST fail — the implementation code does not exist yet. Confirm the failure is due to missing functionality (not a syntax error or import error).

#### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `processes/<file>.py` — <specific change>
  - (any other files this slice touches)
- **Test re-run command:**
  ```bash
  python -m pytest tests/test_<name>.py -v
  ```
- **Expected result:** Test MUST pass cleanly. All assertions succeed. No warnings.

#### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Review for code duplication — extract shared helpers if appropriate
  - Verify naming conventions match the project style
  - Remove any debug/logging code added during implementation
- **Refactor validation command:**
  ```bash
  python -m pytest tests/test_<name>.py -v
  ```
- **Expected result:** All previously passing tests still pass. No regressions.
```

#### TDD Execution Plan rules

| Rule | Detail |
|---|---|
| Test command must be exact | Use the project's real test runner (`pytest`, `npm test`, `cargo test`, etc.). Check for a README or AGENTS.md first — if present, use the documented command. If neither file exists, infer the test runner from project structure: look for `pytest` config files (`pyproject.toml`, `pytest.ini`, `setup.cfg`), `package.json` scripts, `Cargo.toml`, `Makefile`, or existing test directories. Warn the user if the runner could not be determined. |
| RED phase must list a specific test file | Never say "write some tests" — name the exact file path |
| GREEN phase must list specific source files | Cross-reference the `target_files` frontmatter field |
| Expected results must state observable pass/fail | "MUST fail" / "MUST pass" — never "should" or "may" |
| REFACTOR must validate no regressions | Always include the re-run command after refactoring |
| No implementation before test | The RED phase always comes first and must produce a real test failure |

### 4. Definition of Done

Acceptance criteria that MUST be met before the task is complete. The first three criteria enforce that TDD was followed: the test was written first, observed to fail, then observed to pass.

```markdown
### Definition of Done

- [ ] Failing test written and executed first — failure was observed and confirmed to be due to missing functionality (not a syntax/import error)
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`<full-suite-command>`)
- [ ] <Any additional slice-specific criteria>
```

The third checkbox MUST include the project's full test-suite command so the implementer verifies no regressions before marking the task done. Derive this command from the project's README or AGENTS.md if they exist. If neither file exists, infer the command from project structure: check for `pytest` config, `package.json` scripts, `Cargo.toml`, `Makefile`, or existing test directories. Warn the user if the command could not be determined — but still produce a best-effort command.

---

## Secrets & Environment Files (.env)

**`.env` files contain secrets (API keys, credentials, tokens) and are ALWAYS gitignored — they will NOT exist in a fresh checkout.** Tasks must never assume `.env` files are present.

### Rules for tasks that touch .env

1. **Never instruct reading or copying from a live `.env` file.** Do not write steps like "copy the value from `.env`" or "read `PLOI_API_KEY` from the root `.env`". The live `.env` does not exist in a fresh checkout and the task runner/opencode has no way to read it. Always reference `.env.example` templates instead.

2. **Target the `.env.example` template, not `.env`.** If a task creates an environment file, the `target_files` frontmatter should list the `.env.example` template (which IS committed). The task body instructs the implementer to copy the template to `.env` and fill in secrets manually. Do NOT list `.env` in `target_files`.

3. **Scaffolding tasks create the template, not the live file.** A task like "Create .env" creates `client-api/.env.example` with placeholder values. The task documents that the developer manually creates `.env` from the template and fills in real secrets — this is never automated by a task or performed by the LLM.

4. **Bootstrap/loader code must handle missing `.env` gracefully.** Tasks that create code reading `.env` files must include a requirement for graceful handling — return a JSON error, don't crash with a PHP fatal error.

5. **Tests must not depend on live `.env` files.** Test plans should use fixture data or mocks for API keys and configuration. Never instruct the tester to create a real `.env` for tests.

### Example: Correct .env handling in a task

```markdown
#### Data / Schema
- `client-api/.env.example` — committed template with placeholder values:
  ```
  PLOI_API_KEY=
  PLOI_API_BASE_URL=https://ploi.io/api
  ```
  **The developer manually creates `.env` from the template and fills in real secrets — this is never automated by a task or performed by the LLM.**
  `.env` is gitignored — it never exists in the repo.
```

---

## Interaction Rules

1. **Confirm the spec.** Before generating tasks, identify the spec file and confirm with the user: "Breaking down `specs/2026-07-21-foo.md` — proceed?"

2. **Report the count.** Before writing files, tell the user how many task files you will generate and give a one-line summary of each slice. Allow them to veto, reorder, or merge slices.

3. **Write all tasks in one pass.** After confirmation, create all task files. Don't stop mid-way for more feedback.

4. **Report the output.** After writing, list every file created with its path and title.

5. **Dependency graph must be acyclic.** `depends_on` must only reference lower-numbered tasks. If a circular dependency emerges, the slices are wrong — re-decompose.

6. **One task = one file.** Never put multiple tasks in a single file.

7. **Respect the spec's phases.** If the spec groups work into phases (Phase 1, Phase 2, ...), task files should follow that ordering. Phase boundaries are natural `depends_on` breakpoints — tasks in Phase 2 depend on the last task of Phase 1.

8. **Target files must be precise.** Use the filenames from the spec's Affected Files table. Don't guess paths.

9. **Verification commands must use the project's real CLI.** Check for a README or AGENTS.md first — if present, use the documented command. If neither file exists, infer the correct command from project structure: look for `pytest` config files, `package.json` scripts, `Cargo.toml`, `Makefile`, or existing test directories. If the command cannot be determined with confidence, use a reasonable default and warn the user. Never invent a fake command.

10. **Auto-continue numbering.** If `.tasks/` is non-empty, read existing files and start numbering from the next available number (highest existing + 1). Never overwrite existing task files.
