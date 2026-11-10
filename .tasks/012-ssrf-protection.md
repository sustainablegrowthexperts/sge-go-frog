---
id: task-012
title: "[Slice] MCP crawl targets are restricted to public HTTP(S)"
status: pending
depends_on: ["task-011"]
target_files:
  - "mcp_ssrf.go"
  - "mcp_ssrf_test.go"
  - "spider.go"
  - "list.go"
  - "sitemap.go"
  - "server.go"
  - "sitemap_integration_test.go"
---

## Objective

**Objective:** MCP crawl targets, redirects, sitemap URLs, and list URLs are validated and dialed so private, loopback, link-local, unspecified, multicast, IPv4-mapped IPv6, and cloud-metadata endpoints cannot be reached.

## Layer-by-Layer Changes

### SSRF Policy

- Create `mcp_ssrf.go` with a public-target guard:
  - Allow only `http` and `https`.
  - Reject empty hosts, malformed URLs, and URLs with credentials.
  - Resolve the hostname and reject any address that is loopback, private, link-local (unicast or multicast), unspecified, multicast, IPv4-mapped IPv6, or a known cloud-metadata address (at minimum `169.254.169.254`; include IPv6 metadata equivalents).
  - Provide a clear, non-leaky error suitable for `invalid_argument`.
- Implement a custom `DialContext` that validates the resolved address at dial time and connects to the validated IP. This must prevent DNS rebinding between validation and connection; do not validate once and then use the default dialer.
- Preserve TLS SNI and host verification for HTTPS. Do not disable certificate verification.
- No private-network opt-in in v1.

### Crawl Pipeline

- `spider.go`:
  - Use the SSRF-validated HTTP client/dialer for all page fetches.
  - Validate redirect `Location` targets before enqueueing them.
  - Validate sitemap page URLs before visiting them.
- `list.go`:
  - Validate every supplied list URL before visiting.
  - Use the same validated client/dialer.
- `sitemap.go`:
  - Fetch `robots.txt` and sitemap documents with the validated client/dialer.
  - Validate sitemap document URLs and discovered page URLs; do not fetch non-public targets.
- Keep the existing proxy support: when a proxy is configured, apply the same policy before the request leaves the process.

### MCP / Tool Layer

- `start_crawl` validates the initial target (spider `start_url` or every list URL) through the guard and returns `invalid_argument` for blocked targets.
- Never include the resolved private IP, internal hostname, or dialer details in the tool error.
- Log blocked target classes for operators without logging full internal URLs.

### Compatibility

- The existing CLI and UI crawling path must continue to work for public sites.
- `go-frog-audit` behavior is unchanged.
- Existing sitemap discovery behavior is preserved for public sites.

## TDD Execution Plan

### Step 1: RED Phase (Write Failing Test)

- **Test file to create/update:** `mcp_ssrf_test.go` and `sitemap_integration_test.go`
- **Test execution command:**
  ```bash
  go test -run 'TestSSRF' .
  go test -run 'TestSitemapUsesValidatedClient' .
  ```
- **Expected result:** Tests MUST fail because the guard, custom dialer, and crawl-pipeline validation do not exist. Confirm the failures are due to missing behavior, not unrelated syntax/import errors.

### Step 2: GREEN Phase (Implementation)

- **Minimum application code to write/modify:**
  - `mcp_ssrf.go` — policy, resolver, validated dialer, URL validation.
  - `spider.go` — validated client, redirect/sitemap checks.
  - `list.go` — list URL validation and validated client.
  - `sitemap.go` — validated robots/sitemap fetches.
  - `server.go` — start-crawl validation and error mapping.
- **Test re-run command:**
  ```bash
  go test -run 'TestSSRF' .
  go test -run 'TestSitemapUsesValidatedClient' .
  ```
- **Expected result:** Tests MUST pass. Private/loopback/link-local/metadata IPv4 and IPv6 targets are rejected; public targets pass; a redirect to a private target is blocked; a sitemap containing a private URL is not fetched; a DNS-rebinding resolver is blocked at dial time; sitemap discovery still works against a public `httptest` site.

### Step 3: REFACTOR Phase

- **Cleanup tasks:**
  - Extract address classification into a pure table-tested function.
  - Keep one validated transport factory shared by spider, list, and sitemap.
  - Confirm no test performs real external network calls; use `httptest` and injectable resolvers.
- **Refactor validation command:**
  ```bash
  go test -run 'TestSSRF|TestSitemapUsesValidatedClient' . && go test -race ./...
  ```
- **Expected result:** All previously passing tests still pass under the race detector. No regressions.

## Definition of Done

- [ ] Failing test written and executed first — failure observed and confirmed to be due to missing functionality (not an unrelated syntax/import error)
- [ ] Implementation completed and tests re-run — all tests pass cleanly
- [ ] Entire test suite passes without regressions (`go test ./...`)
- [ ] Private, loopback, link-local, metadata, unspecified, multicast, and IPv4-mapped IPv6 targets are rejected
- [ ] Redirects, sitemap documents, discovered sitemap URLs, and list URLs are validated
- [ ] DNS rebinding is blocked at dial time
- [ ] Error messages do not reveal internal IPs, hostnames, or dialer details
- [ ] Public-site crawling and sitemap discovery still work
