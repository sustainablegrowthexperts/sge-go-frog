---
id: task-007
title: "[Slice] Start requests are bounded by job and rate limits"
status: completed
depends_on: ["task-006"]
target_files:
  - "mcp_limits.go"
  - "server.go"
  - "mcp_tools.go"
  - "mcp_test.go"
---

## Objective

**Objective:** MCP start requests are bounded by per-token and global running-job limits, a short wait queue, and a per-token tool-call rate limit that returns `limit_exceeded` with `retry_after_ms`.

## Layer-by-Layer Changes

### Limits / Quotas

- Extend `mcp_limits.go` with:
  - Global running-job semaphore (default 4).
  - Per-token running-job semaphore (default 2; the v1 deployment has one shared token).
  - Bounded wait queue up to twice the global limit.
  - Per-token token-bucket rate limiter (default 60 tool calls per minute).
- Acquisition is non-blocking with a bounded wait. When the queue is full, return `limit_exceeded` with:
  - `retryable=true`
  - `retry_after_ms` based on the configured limits
  - details naming the limit (`global_jobs`, `per_token_jobs`, or `rate_limit`).
- Release both semaphores on every terminal job state, including error and interrupted.

### Core Logic / Job Manager

- Acquire slots before creating/launching a job, not after. Do not create a job that cannot run.
- Count only `queued`/`running` jobs against the caps.
- Keep the per-token key behind a small helper so a future multi-token deployment can use proxy-provided identity without changing call sites. For v1, use the shared MCP owner key.
- Ensure a queued job that is later admitted starts in `queued` phase and transitions to `starting` when it runs.

### MCP / Tool Layer

- Apply the rate limiter to every tool call, not only `start_crawl`. A rejected call must not execute side effects.
- Map quota/rate failures to the stable error envelope with `limit_exceeded`.
- Expose the effective caps in startup logs/config output, never as secrets.

### API / Server

- The web UI is not subject to MCP semaphores or the MCP rate limiter; its existing behavior is unchanged.
- Do not add a cancellation path in this slice.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `mcp_test.go`
- **Test execution command:**
  ```bash
  go test -run 'TestStartCrawlConcurrencyLimit|TestToolRateLimit' .
  ```
- **Expected result:** The test MUST fail because the semaphores, queue, and rate limiter do not exist. Confirm the failure is due to missing behavior, not an unrelated syntax/import error.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `mcp_limits.go` — semaphores, bounded queue, token bucket, limit errors.
  - `server.go` — acquire/release around job launch and terminal states.
  - `mcp_tools.go` — rate-limit every tool call and map failures.
- **Test re-run command:**
  ```bash
  go test -run 'TestStartCrawlConcurrencyLimit|TestToolRateLimit' .
  ```
- **Expected result:** Tests MUST pass. With a blocking fake runner and global cap 4, four jobs are accepted, the next queued requests wait, the queue overflow returns `limit_exceeded` with `retry_after_ms`, releasing a job admits the queued request, and the 61st tool call in a simulated minute is rejected.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Centralize acquire/release in a small quota helper with `defer`-safe release.
  - Ensure the rate limiter uses an injectable clock for deterministic tests.
  - Confirm a failed tool call does not leak a semaphore slot.
- **Refactor validation command:**
  ```bash
  go test -run 'TestStartCrawlConcurrencyLimit|TestToolRateLimit' . && go test -race ./...
  ```
- **Expected result:** All previously passing tests still pass under the race detector. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing functionality (not an unrelated syntax/import error)
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] Global and per-token job caps are enforced before a job is created
- [ ] Queue overflow returns `limit_exceeded` with `retry_after_ms`
- [ ] Tool rate limiting applies to every tool and has no side effects when rejected
- [ ] Slots are released on done, error, and interrupted states
