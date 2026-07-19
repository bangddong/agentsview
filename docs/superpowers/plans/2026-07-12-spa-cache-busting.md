# SPA Cache Busting Implementation Plan

> **Status:** Complete. Shipped via PR #1114 (fix(server): prevent stale SPA
> asset fallbacks); verified against origin/main 2026-07-17.

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development
> (if subagents available) or superpowers:executing-plans to implement this
> plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prevent stale SPA entry documents from producing broken mixed frontend
assets after a binary upgrade.

**Architecture:** Keep the existing embedded filesystem and SPA fallback, but
classify entry HTML, fingerprinted assets, and client-side routes before serving
them. Protect the browser-visible HTTP contract with in-process handler tests
using a literal in-memory filesystem.

**Tech Stack:** Go, `net/http`, `httptest`, `testing/fstest`, Testify

______________________________________________________________________

### Task 1: Protect the SPA response contract

**Files:**

- Create: `internal/server/spa_test.go`

- Modify: `internal/server/server.go`

- [x] **Step 1: Write failing HTTP tests**

    Add tests which request `/`, an existing fingerprinted JavaScript asset, and a
    missing JavaScript asset. Assert literal cache headers, status codes,
    content types, and bodies. Keep a client-side route assertion to prove the
    fallback remains intact.

- [x] **Step 2: Verify the tests fail for the missing behavior**

    Run `go test -tags fts5 ./internal/server -run 'TestSPA' -count=1` and confirm
    failures show absent cache headers and a 200 HTML response for the missing
    asset.

- [x] **Step 3: Implement the response classification**

    Set `Cache-Control: no-cache` before serving entry HTML, set the immutable
    policy for existing `/assets/` paths, and call `http.NotFound` when an
    `/assets/` path is absent. Preserve client-route and base-path fallbacks.

- [x] **Step 4: Verify the targeted behavior passes**

    Run `go test -tags fts5 ./internal/server -run 'TestSPA' -count=1` and confirm
    all SPA tests pass.

- [x] **Step 5: Verify repository checks**

    Run `go fmt ./...`, `go vet ./...`, `make test-short`, and a production build.
    Start the built binary against an isolated temporary data directory and use
    `curl` to observe the entry cache header and a missing asset's 404 response.

- [x] **Step 6: Publish the focused change**

    Review and scrub the diff and publication text, commit the focused change,
    push `fix/spa-cache-busting`, and open a pull request with a rationale-first
    summary.
