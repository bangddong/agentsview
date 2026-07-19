# Activity Project Reclassification Implementation Plan

> **Status (2026-07-17):** Tasks 1-9 are implemented on branch `cool-lillipilli`
> (parser fix `08e556e7e` through kit-ui pin `4e0452638`), with both Playwright
> modes passing. A UI/UX review round was applied on top: reserved action slot
> so the pencil no longer covers the value column, server-normalized target
> shown before apply, stacked Settings layout buttons, disambiguated candidate
> labels, accurate pre-version tooltip, and a Settings deep link that preselects
> the rule's machine. Task 10 remains: rerun whole-branch
> `make lint`/`make test`/`make test-postgres`, the subagent whole-branch
> review, scrub, push, and PR.

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Correct generic GitHub worktree project inference and let users
create, preview, apply, and manage machine-scoped worktree project mappings from
the Activity project breakdown.

**Architecture:** One agentsview feature branch contains the parser fix and the
complete SQLite/API/frontend workflow. SQLite remains the only mutation owner;
PostgreSQL and DuckDB receive project moves through their existing publication
pipelines. A small kit-ui prerequisite adds the reusable accessibility contracts
needed by the modal, typeahead, and focusable read-only action before agentsview
pins its merged commit.

**Tech Stack:** Go 1.24, SQLite/FTS5, PostgreSQL, DuckDB, Huma/OpenAPI, Svelte
5, TypeScript, Paraglide JS, kit-ui, Vitest, and Playwright.

## Global Constraints

- Deliver all agentsview changes in one pull request; the kit-ui prerequisite is
  a separate upstream dependency pull request and must not be merged by an
  agent.
- Follow strict red-green-refactor TDD. Every new behavior gets a test that is
  observed failing for the intended reason before production code is added.
- Enabled mappings run after parser inference and always win.
- Candidate discovery is scoped to the clicked Activity view; preview and apply
  evaluate every visible session in the writable archive for the selected
  machine.
- Matching uses directory boundaries, longest-prefix precedence, and separator
  normalization that works when a Unix archive stores Windows paths or vice
  versa.
- The preview token covers the effective mapping set for one machine only. It
  does not include session revisions; semantically identical mapping state may
  reuse the same token.
- `original_project` is informational and set-once: empty may become the clicked
  label, while a non-empty value is immutable.
- Applying is atomic across the rule, session rewrites, and aggregate identity
  observations, under `sync.Engine.RunExclusive` and one SQLite transaction.
- Immutable session identity snapshots retain parser-time source evidence and
  their source project label. Rebuilt aggregate observations use the sessions'
  current mapped labels.
- Every rewritten session advances `local_modified_at`. Every unsupported or
  deleted former aggregate produces the existing identity tombstone.
- PostgreSQL and DuckDB must both remove stale old-project membership for
  unfiltered, include-project, and exclude-project publication scopes.
- Read-only Activity actions remain focusable with `aria-disabled`; read-only
  Settings keeps its informational-only state and issues no mapping reads.
- All user-facing strings use Paraglide, all five locale catalogs have identical
  keys, and count-sensitive copy uses plural declarations with numeric inputs.
- New tests use testify in Go and assert observable behavior rather than source
  text, mock existence, or implementation details.
- Fixtures, docs, commits, screenshots, and pull-request text use only reserved
  or synthetic hosts, projects, paths, and identities.
- Branch binaries and schema tests use temporary data directories only. Never
  install over the live binary or open the production archive with branch
  code.

______________________________________________________________________

### Task 1: Add the reusable kit-ui accessibility contracts

**Files:**

- Modify in kit-ui: `src/lib/components/Modal.svelte`
- Modify in kit-ui: `src/lib/components/Typeahead.svelte`
- Modify in kit-ui: `src/lib/components/IconButton.svelte`
- Modify in kit-ui: `src/demo/pages/ModalDemo.svelte`
- Modify in kit-ui: `src/demo/pages/TypeaheadDemo.svelte`
- Modify in kit-ui: `tests/browser/focus-trap.spec.ts`
- Modify in kit-ui: `tests/browser/typeahead.spec.ts`
- Modify in kit-ui: `tests/browser/icon-button.spec.ts`
- Modify in kit-ui: `docs/components/modal.md`
- Modify in kit-ui: `docs/components/typeahead.md`
- Modify in kit-ui: `docs/components/icon-button.md`

**Interfaces:**

- Produces: `Modal.closeLabel?: string`, defaulting to `"Close"`.

- Produces: `IconButton.ariaDisabled?: boolean`, which renders
  `aria-disabled="true"`, remains tabbable, and suppresses activation.

- Produces: `Typeahead.allowCustom` offering any trimmed non-empty query that is
  not already an exact option name, even when partial matches exist.

- Consumed by: Tasks 7 and 8 after agentsview pins the merged kit-ui commit.

- [x] **Step 1: Create an isolated kit-ui worktree and establish its baseline**

    Read kit-ui's repository instructions, create a linked feature worktree from
    the latest locally available `origin/main` without changing the existing
    checkout's branch, install with `bun install`, and run:

    ```bash
    bun run check
    bun test checks/
    bunx playwright test tests/browser/focus-trap.spec.ts \
      tests/browser/typeahead.spec.ts tests/browser/icon-button.spec.ts
    ```

    Expected: exit 0 before edits.

- [x] **Step 2: Write failing browser tests for all three contracts**

    Add assertions equivalent to:

    ```ts
    await page.getByRole("button", { name: "Dismiss example dialog" }).click();
    await expect(page.getByRole("dialog")).toBeHidden();

    await typeahead.fill("project-al");
    await expect(page.getByRole("option", { name: 'Use "project-al"' })).toBeVisible();

    const action = page.getByRole("button", { name: "Unavailable action" });
    await expect(action).toHaveAttribute("aria-disabled", "true");
    await action.focus();
    await action.press("Enter");
    await expect(page.getByTestId("activation-count")).toHaveText("0");
    ```

    Run the focused Playwright command from Step 1. Expected: failures show the
    hard-coded Modal label, missing custom row, and missing focusable disabled
    semantics.

- [x] **Step 3: Implement the minimal shared APIs**

    Use these contracts:

    ```ts
    // Modal.svelte
    closeLabel?: string;
    let { closeLabel = "Close", ...rest }: Props = $props();
    <IconButton ariaLabel={closeLabel} onclick={() => onclose?.()} />

    // IconButton.svelte
    ariaDisabled?: boolean;
    aria-disabled={ariaDisabled || undefined}
    onclick={(event) => {
      if (ariaDisabled) {
        event.preventDefault();
        event.stopPropagation();
        return;
      }
      onclick?.(event);
    }}

    // Typeahead.svelte
    const exactOption = $derived(findByName(options, query.trim()));
    const customValue = $derived(
      allowCustom && query.trim() !== "" && !exactOption ? query.trim() : "",
    );
    ```

    Adjust row counts and indices so the custom row coexists with partial-match
    rows and is always the row named by `aria-activedescendant` when
    highlighted.

- [x] **Step 4: Verify, document, commit, scrub, push, and open the kit-ui PR**

    Run:

    ```bash
    bun run fmt
    bun run fmt:check
    bun run lint
    bun run check
    bun test checks/
    bun run check:usage
    bun run build
    bun run test:browser
    ```

    Commit through the mandatory commit workflow, scrub the complete unpublished
    diff and PR text, push the kit-ui feature branch, and open a rationale-first
    PR. Do not merge it. Record the PR URL and head SHA in the progress ledger.

______________________________________________________________________

### Task 2: Recognize generic GitHub worktree layouts

**Files:**

- Modify: `internal/parser/project.go`
- Modify: `internal/parser/project_git_test.go`
- Modify: `internal/db/db.go`
- Modify: `internal/db/db_test.go`
- Test: `internal/sync/engine_integration_test.go`

**Interfaces:**

- Produces: parser layout marker `worktrees/github.com/` with `projectPart: 1`
  and `minParts: 3`.

- Produces: data version `68`.

- Consumed by: resync lifecycle coverage in Task 6.

- [x] **Step 1: Add failing parser and version tests**

    Extend the existing table with synthetic cases:

    ```go
    {
        name: "GenericGitHubWorktreeNested",
        cwd: filepath.FromSlash(
            "/srv/worktrees/github.com/example-org/sample-service/fix-123/cmd/server",
        ),
        branch: "fix-123",
        want: "sample_service",
    },
    {
        name: "GenericGitHubRepositoryRootUsesFallback",
        cwd: filepath.FromSlash(
            "/srv/worktrees/github.com/example-org/sample-service",
        ),
        want: "sample_service",
    },
    {
        name: "AdjacentGitHubWorktreesNameDoesNotMatch",
        cwd: filepath.FromSlash(
            "/srv/not-worktrees/github.com/example-org/sample-service/fix-123",
        ),
        want: "fix_123",
    },
    ```

    Rename the version test to `TestCurrentDataVersionGenericGitHubWorktrees` and
    expect `68`. Add an engine fixture whose cwd does not exist locally and
    assert that its persisted project is `sample_service`.

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/parser ./internal/db ./internal/sync \
      -run 'Test(ExtractProjectFromCwd|CurrentDataVersionGeneric|Remote.*Worktree)'
    ```

    Expected: generic nested case and version assertion fail.

- [x] **Step 2: Add the guarded layout and bump the version**

    Add this layout after the tool-specific GitHub layout:

    ```go
    {
        marker: sep + "worktrees" + sep + "github.com" + sep,
        projectPart: 1,
        minParts: 3,
    },
    ```

    Set `dataVersion = 68` without changing schema version behavior.

- [x] **Step 3: Run focused tests and commit**

    Run the Step 1 command, `go fmt ./...`, and `go vet ./internal/parser/...`.
    Expected: exit 0. Commit as
    `fix(parser): recognize generic GitHub worktrees`.

______________________________________________________________________

### Task 3: Persist set-once mapping context and manage every machine

**Files:**

- Modify: `internal/db/schema.sql`
- Modify: `internal/db/db.go`
- Modify: `internal/db/worktree_mappings.go`
- Modify: `internal/db/worktree_mappings_test.go`
- Modify: `internal/db/orphaned.go`
- Modify: `internal/db/orphaned_test.go`
- Modify: `internal/server/worktree_mappings.go`
- Modify: `internal/server/huma_routes_settings.go`
- Modify: `internal/server/worktree_mappings_test.go`

**Interfaces:**

- Produces: `WorktreeProjectMapping.OriginalProject string`.

- Produces: `ListWorktreeProjectMappingMachines(ctx) ([]string, error)`.

- Produces: Settings GET query `machine`, create body `machine`, and selected
  machine apply body.

- Produces: mapping responses with `machine`, `local_machine`, `machines`, and
  `mappings`.

- Consumed by: atomic apply in Task 4 and Settings UI in Task 7.

- [x] **Step 1: Add failing migration, set-once, carryover, and remote-machine
  tests**

    Tests must prove:

    ```go
    assert.Equal(t, "branch_label", created.OriginalProject)
    assert.Equal(t, "branch_label", edited.OriginalProject,
        "non-empty original_project cannot be overwritten")
    assert.Equal(t, "activity_label", filled.OriginalProject,
        "an empty Settings-created value may be filled once")
    assert.Equal(t, "activity_label", copied.OriginalProject)
    assert.ElementsMatch(t, []string{"host-a.example", "host-b.example"}, machines)
    ```

    Build a legacy attached database with no `original_project` column and assert
    both carryover paths default it to empty. Server tests create, list, update,
    apply, and delete a rule for a non-local machine.

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/server \
      -run 'Test(Worktree|CopyWorktree|CopySessionMetadata|SchemaColumn|RemoteMachine)'
    ```

    Expected: compile failures for the missing field and API contracts.

- [x] **Step 2: Add the non-destructive column and update every scan/copy path**

    Add:

    ```sql
    original_project TEXT NOT NULL DEFAULT ''
    ```

    to the table DDL and:

    ```go
    {
        "worktree_project_mappings", "original_project",
        "ALTER TABLE worktree_project_mappings ADD COLUMN original_project TEXT NOT NULL DEFAULT ''",
    },
    ```

    to `schemaColumnMigrations()`. Include the field in every mapping SELECT,
    INSERT, scan, resync copy, and metadata reconciliation statement. Use a
    conditional `originalProjectSelect` for old schemas.

- [x] **Step 3: Enforce set-once updates and machine discovery**

    The update expression is:

    ```sql
    original_project = CASE
        WHEN original_project = '' THEN ?
        ELSE original_project
    END
    ```

    The machine union is deterministic and bounded to distinct stored values:

    ```sql
    SELECT machine FROM sessions WHERE deleted_at IS NULL AND machine != ''
    UNION
    SELECT machine FROM worktree_project_mappings WHERE machine != ''
    ORDER BY machine
    ```

    Keep existing DB methods backward compatible where sync code consumes them;
    resolve update/delete machine by the globally unique mapping ID at the Huma
    boundary.

- [x] **Step 4: Verify and commit**

    Run the Step 1 command, `go fmt ./...`, and
    `go vet ./internal/db/... ./internal/server/...`. Expected: exit 0. Commit
    as `feat(settings): manage worktree mappings by machine`.

______________________________________________________________________

### Task 4: Build one authoritative preview and atomic apply evaluator

**Files:**

- Create: `internal/db/worktree_reclassification.go`
- Create: `internal/db/worktree_reclassification_test.go`
- Modify: `internal/db/worktree_mappings.go`
- Modify: `internal/db/project_identity.go`
- Modify: `internal/db/project_identity_test.go`
- Modify: `internal/sync/engine.go`
- Test: `internal/sync/engine_integration_test.go`

**Interfaces:**

- Produces:

    ```go
    type WorktreeReclassificationDraft struct {
        Machine, PathPrefix, Layout, Project, OriginalProject string
        Enabled bool
    }

    type WorktreeReclassificationPreview struct {
        MappingToken, NormalizedProject string
        ExistingMappingID *int64
        MatchedSessions, UpdatedSessions, DistinctProjects int
        ProjectSamples []WorktreeReclassificationProjectSample
        SessionSamples []WorktreeReclassificationSessionSample
    }

    var ErrWorktreeMappingSetChanged = errors.New("worktree mapping set changed")

    func (db *DB) PreviewWorktreeReclassification(
        ctx context.Context, draft WorktreeReclassificationDraft,
    ) (WorktreeReclassificationPreview, error)

    func (db *DB) ApplyWorktreeReclassification(
        ctx context.Context, draft WorktreeReclassificationDraft,
        acceptedToken string, existingMappingID *int64,
    ) (WorktreeProjectMapping, WorktreeReclassificationPreview, error)
    ```

- Consumed by: Huma handlers in Task 5.

- [x] **Step 1: Add failing evaluator, token, atomicity, and identity tests**

    Cover literal observable cases:

    - `/worktrees/service` matches itself and descendants but not
      `/worktrees/service-old`.
    - Stored `C:\worktrees\service\subdir` matches a draft expressed with
      backslashes even when tests run on Unix.
    - A more-specific enabled rule wins over the draft.
    - Preview returns exact totals and at most 10 deterministic samples.
    - A mapping edit changes the token and causes apply conflict.
    - A new matching session does not change the token and is included by apply.
    - Exact `(machine, path_prefix)` collision returns that rule ID; the client
      never supplies an unrelated ID.
    - Any injected mapping/session/identity write error rolls back all changes.
    - Rewritten rows advance `local_modified_at`.
    - Target aggregates exist, unsupported former aggregates tombstone, supported
      former aggregates remain, and snapshot rows are byte-for-byte unchanged.

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/sync \
      -run 'Test(WorktreeReclassification|ProjectIdentityReclassification|RunExclusive)'
    ```

    Expected: compile failures for the new API.

- [x] **Step 2: Extract a separator-neutral matcher and shared evaluator**

    Normalize stored matching paths without consulting the archive host OS:

    ```go
    func normalizedMappingPath(value string) string {
        value = strings.TrimSpace(strings.ReplaceAll(value, `\`, "/"))
        if value == "" { return "" }
        return path.Clean(value)
    }

    func worktreePathMatches(prefix, cwd string) bool {
        prefix = strings.TrimSuffix(normalizedMappingPath(prefix), "/")
        cwd = normalizedMappingPath(cwd)
        return cwd == prefix || strings.HasPrefix(cwd, prefix+"/")
    }
    ```

    Preserve filesystem-root and Windows-volume root cases. Refactor the existing
    full-machine apply, sync apply, and preview to consume one evaluator that
    loads rows once, applies sibling-cwd fallback once, and returns matches plus
    proposed updates.

- [x] **Step 3: Implement the machine mapping-set token and exact collision
  rule**

    Hash sorted effective mapping rows with length-prefixed fields:

    ```go
    id, machine, normalizedPrefix, layout, project, enabled, updatedAt
    ```

    Session rows and global archive revisions are absent. Preview overlays the
    normalized draft, replacing the exact `(machine, path_prefix)` rule if one
    exists, and returns its ID. Apply accepts only that returned ID and rejects
    if either the token or exact collision identity changed.

- [x] **Step 4: Implement one transaction for rule, sessions, and aggregates**

    Under the existing DB mutex and one transaction:

    ```text
    validate token -> upsert exact rule -> evaluate current sessions ->
    update changed sessions/local_modified_at -> rebuild affected aggregates ->
    commit
    ```

    Rebuild aggregates by joining current `sessions.project` to immutable
    `session_project_identity_snapshots` evidence. Delete affected aggregate
    rows first so existing triggers record tombstones, then reinsert only
    evidence still supported by visible sessions. Never update snapshot rows.

- [x] **Step 5: Serialize apply with sync and verify**

    Invoke the DB mutation only inside `Engine.RunExclusive`. Add a channel-based
    test proving watcher/sync work cannot overlap the transaction. Run the Step
    1 command, `go fmt ./...`, and
    `go vet ./internal/db/... ./internal/sync/...`. Expected: exit 0. Commit as
    `feat(activity): add atomic project reclassification`.

______________________________________________________________________

### Task 5: Add range-scoped candidate discovery and Huma endpoints

**Files:**

- Modify: `internal/db/activityreport.go`
- Create: `internal/db/worktree_candidates.go`
- Create: `internal/db/worktree_candidates_test.go`
- Modify: `internal/server/huma_routes_activity.go`
- Modify: `internal/server/huma_routes_settings.go`
- Modify: `internal/server/worktree_mappings.go`
- Modify: `internal/server/activity_report_test.go`
- Modify: `internal/server/worktree_mappings_test.go`
- Regenerate: `frontend/src/lib/api/generated/**`

**Interfaces:**

- Produces: `GET /api/v1/activity/project-reclassification/candidates`.

- Produces: `POST /api/v1/settings/worktree-mappings/preview`.

- Produces: `POST /api/v1/settings/worktree-mappings/reclassify`.

- Candidate input includes `clicked_project`, `clicked_project_key`, and every
  Activity query field, including the separate current `project` filter.

- Preview/apply responses use Task 4's authoritative totals and bounded samples.

- [x] **Step 1: Write failing candidate and handler tests**

    Seed range-in and range-out sessions, two machines, identity-backed sibling
    cwds, an exact-cwd fallback, an unavailable cwd, and two raw labels with the
    same safe display label but different project keys. Assert discovery returns
    only sessions contributing to the clicked row identity and current Activity
    selection.

    Handler tests assert 400 validation, 409 stale mapping token, 501 read-only
    behavior, full-archive preview counts outside the Activity range, and
    bounded JSON arrays.

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/server \
      -run 'Test(WorktreeCandidate|ActivityProjectReclassification|WorktreePreview)'
    ```

    Expected: missing-method and missing-route failures.

- [x] **Step 2: Reuse Activity selection and group candidates**

    Extract the Activity input resolution into a helper shared by report and
    candidate handlers. Candidate DB selection reuses `activityReportSessions`,
    builds the raw-label project map, and filters by both clicked display label
    and `clicked_project_key`.

    Group in this order:

    ```text
    snapshot worktree_root_path/root_path -> compatible aggregate root evidence ->
    exact cwd fallback
    ```

    Compute identity-group prefixes with a slash-normalized longest common
    directory prefix. Stable candidate IDs are SHA-256 hashes of length-prefixed
    machine, evidence kind, evidence root, and fallback cwd. Return counts and
    at most 10 sorted examples; represent missing cwd as unavailable.

- [x] **Step 3: Register writable-only preview and apply routes**

    The writable guard casts to `*db.DB` and requires a non-nil engine. Preview is
    read-only but local-only because it depends on archive path evidence. Apply
    calls `engine.RunExclusive(func() error { ... })`. Map token conflicts to
    HTTP 409 and invalid drafts to HTTP 400.

- [x] **Step 4: Generate the client, verify, and commit**

    Run:

    ```bash
    cd frontend && npm run generate:api
    cd ..
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/server \
      -run 'Test(WorktreeCandidate|ActivityProjectReclassification|WorktreePreview)'
    go fmt ./...
    go vet ./internal/db/... ./internal/server/...
    ```

    Expected: exit 0 and only route-related generated client changes. Commit as
    `feat(api): expose project reclassification workflow`.

______________________________________________________________________

### Task 6: Preserve reclassification across resync and mirror publication

**Files:**

- Modify: `internal/sync/engine.go`
- Modify: `internal/sync/engine_integration_test.go`
- Modify: `internal/postgres/push.go`
- Modify: `internal/postgres/push_pgtest_test.go`
- Modify: `internal/postgres/project_identity_pgtest_test.go`
- Modify: `internal/duckdb/push.go`
- Modify: `internal/duckdb/store_test.go`
- Modify: `internal/duckdb/project_identity_upsert_test.go`
- Test: `internal/activity/parity_pgtest_test.go`

**Interfaces:**

- Produces: resync reapplication for every machine represented by enabled
  mappings, not only the local engine machine.

- Produces: incremental identity observations keyed to the persisted mapped
  session project while immutable snapshots retain the parser source project.

- Produces: filtered PG and DuckDB stale-session reconciliation after a project
  scope move.

- [x] **Step 1: Write failing lifecycle and mirror-move tests**

    The lifecycle test creates one live-source and one orphaned session, applies a
    remote-machine mapping, runs a full resync, and asserts both keep the
    target. A companion disabled/deleted-rule case asserts the live row can
    revert while the orphan keeps its stored target.

    Backend matrices cover:

    ```go
    []struct {
        name string
        projects []string
        excludeProjects []string
    }{
        {name: "unfiltered"},
        {name: "include former", projects: []string{"former_project"}},
        {name: "include target", projects: []string{"target_project"}},
        {name: "exclude former", excludeProjects: []string{"former_project"}},
        {name: "exclude target", excludeProjects: []string{"target_project"}},
    }
    ```

    Each asserts target membership appears when in scope and stale former
    membership disappears. Identity assertions cover target observations and
    former tombstones while snapshots remain source-labelled.

    Run focused SQLite/DuckDB tests and the `pgtest` matrix. Expected: remote
    resync and PG old-scope cases fail before implementation.

- [x] **Step 2: Apply mappings for every represented machine during resync**

    After metadata carryover, query distinct mapping machines and invoke
    `ApplyWorktreeProjectMappingsFromSync` for each. Keep the phase non-fatal
    only if existing resync policy already treats mapping application that way;
    collect and report every machine-specific error rather than stopping at the
    first.

    In incremental writes, resolve or reload the persisted current project before
    writing aggregate identity observations. Do not modify immutable snapshots.

- [x] **Step 3: Reconcile filtered mirror scope moves**

    For each source archive/machine ownership scope, compare mirror session IDs in
    the configured project scope with current local IDs in that same scope.
    Delete mirror rows no longer present in scope before upserting current
    candidates. Reuse the DuckDB reconciliation shape in PostgreSQL and preserve
    hard-delete ownership checks.

- [x] **Step 4: Verify parity and commit**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/sync ./internal/duckdb \
      -run 'Test(ReclassificationSurvives|Mapping.*Resync|Push.*ProjectMove|ProjectIdentity)'
    make test-postgres
    go fmt ./...
    go vet ./internal/sync/... ./internal/postgres/... ./internal/duckdb/...
    ```

    Expected: exit 0. Commit as
    `fix(sync): preserve mapped project moves across mirrors`.

______________________________________________________________________

### Task 7: Pin kit-ui and extend shared project and Settings controls

**Files:**

- Modify: `frontend/package.json`
- Modify: `frontend/package-lock.json`
- Modify: `frontend/src/lib/components/layout/ProjectTypeahead.svelte`
- Create: `frontend/src/lib/components/layout/ProjectTypeahead.test.ts`
- Modify: `frontend/src/lib/components/settings/WorktreeMappingSettings.svelte`
- Modify: `frontend/src/lib/components/settings/WorktreeMappingSettings.test.ts`
- Modify: `frontend/src/lib/components/settings/SettingsPage.test.ts`
- Modify: every existing kit-ui Modal callsite under
  `frontend/src/lib/components/`

**Interfaces:**

- Produces: `ProjectTypeahead` props `includeAll`, `allowCustom`, `customLabel`,
  `placeholder`, `title`, and `emptyLabel`, with existing-filter compatible
  defaults.

- Produces: machine-selected Settings management, set-once context display, and
  disable/delete confirmation.

- Consumed by: Activity modal in Task 8.

- [x] **Step 1: Pin the merged kit-ui SHA and add failing shared-control tests**

    After the upstream PR merges, replace the package hash with its merge commit
    and run `npm install`. Keep npm's canonical lockfile URL form.

    Add tests proving default `ProjectTypeahead` still shows All, modal mode omits
    All, a partial-match custom query can be committed, and empty input cannot.
    Expected: tests fail before wrapper props exist.

- [x] **Step 2: Implement the backward-compatible wrapper and localize all Modal
  close labels**

    Use defaults:

    ```ts
    includeAll = true;
    allowCustom = false;
    customLabel = m.activity_reclassify_use_custom_project({ query: "{query}" });
    ```

    Pass `closeLabel={m.*()}` at every existing Modal callsite using its existing
    close message; add no new hard-coded English.

- [x] **Step 3: Add failing Settings interaction tests**

    Assert machine-switch request arguments and stale-response cancellation,
    selected-machine create/apply, original-label rendering, confirmation before
    delete or enabled-to-disabled save, and the existing read-only no-request
    behavior.

- [x] **Step 4: Implement Settings with kit-ui controls**

    Replace new interactive chrome with kit-ui `Typeahead`, `TextInput`, `Button`,
    and `Modal`. Reset editing state on machine changes. The confirmation copy
    states that live-source sessions may revert on reparse/full resync while
    orphaned sessions retain stored classification. Do not add controls to the
    read-only informational branch.

- [x] **Step 5: Verify and commit**

    Run:

    ```bash
    cd frontend
    npm run i18n:compile
    npm run check
    npm run check:kit-ui
    npm test -- src/lib/components/layout/ProjectTypeahead.test.ts \
      src/lib/components/settings/WorktreeMappingSettings.test.ts \
      src/lib/components/settings/SettingsPage.test.ts
    ```

    Expected: exit 0. Commit as `feat(settings): manage remote worktree mappings`.

______________________________________________________________________

### Task 8: Add the Activity project reclassification workflow

**Files:**

- Modify: `frontend/src/lib/stores/activity.svelte.ts`
- Modify: `frontend/src/lib/stores/activity.test.ts`
- Modify: `frontend/src/lib/components/activity/Breakdowns.svelte`
- Modify: `frontend/src/lib/components/activity/Breakdowns.test.ts`
- Modify: `frontend/src/lib/components/activity/ActivityPage.svelte`
- Modify: `frontend/src/lib/components/activity/ActivityPage.test.ts`
- Create:
  `frontend/src/lib/components/activity/ProjectReclassificationModal.svelte`
- Create:
  `frontend/src/lib/components/activity/ProjectReclassificationModal.test.ts`
- Modify: `frontend/messages/en.json`
- Modify: `frontend/messages/fr.json`
- Modify: `frontend/messages/ko.json`
- Modify: `frontend/messages/zh-CN.json`
- Modify: `frontend/messages/zh-TW.json`

**Interfaces:**

- Produces: `ActivityStore.queryParams()` as the single source for report and
  candidate request scope.

- Produces: `ActivityStore.refreshAfterReclassification(): Promise<boolean>`,
  preserving the last report while reporting refresh failure.

- Produces: `Breakdowns` props `readOnly`, `onReclassifyProject`, and project
  heading focus fallback.

- Produces: page-owned modal state that survives report refresh failure.

- [x] **Step 1: Add failing store and breakdown action tests**

    Tests assert the candidate request receives exactly the same range/filter
    fields as report load; only Project rows have action buttons; fine-pointer
    buttons reveal on hover/focus; coarse-pointer buttons remain visible; and a
    not-yet-known or read-only server renders a tabbable `aria-disabled` action
    whose activation makes no request.

- [x] **Step 2: Extract Activity query construction and add refresh result
  semantics**

    `queryParams()` returns the generated Activity request object, including
    custom half-open range conversion. Both report load and candidate discovery
    consume it. `refreshAfterReclassification()` runs report load plus filter
    cache invalidation/reload, preserves the last good report on error, and
    returns `false` instead of hiding the error outcome.

- [x] **Step 3: Add failing modal state-machine tests**

    Cover one candidate preselection, multiple candidates, unavailable cwd,
    editable prefix debounce/cancellation, full-archive impact totals, warning
    for more than one affected project, zero-match blocking, known/custom
    target, mapping-token conflict refresh, exactly one Apply, successful
    report/options reload, applied-refresh-failed state, and refresh-only retry.

- [x] **Step 4: Implement the page-owned modal and dense-row action**

    Keep modal state outside the `{#if activity.report}` subtree. The button is an
    absolutely positioned kit-ui `IconButton`; its DOM position and focusability
    do not change with opacity. Use `ariaDisabled` from Task 1 and localized
    guidance to the writable archive.

    The modal uses only generated API services and kit-ui controls. It stores the
    exact clicked display label as `original_project`. It clears an accepted
    preview token whenever machine, candidate, prefix, or target changes.

- [x] **Step 5: Implement focus restoration and failure-safe completion**

    After apply succeeds, call `refreshAfterReclassification()`. On success, close
    and focus the connected trigger; if the row disappeared, focus the Project
    heading. On failure, keep the modal open with only a refresh retry and never
    call apply again.

- [x] **Step 6: Add synchronized localized copy and verify**

    Add identical `activity_reclassify_*` and new `worktree_*` keys to every
    catalog. Use Paraglide plural variants for sessions, candidates, and
    projects, passing numeric counts.

    Run:

    ```bash
    cd frontend
    npm run i18n:compile
    npm run check
    npm run check:kit-ui
    npm test -- src/lib/stores/activity.test.ts \
      src/lib/components/activity/Breakdowns.test.ts \
      src/lib/components/activity/ActivityPage.test.ts \
      src/lib/components/activity/ProjectReclassificationModal.test.ts
    ```

    Expected: exit 0. Commit as
    `feat(activity): reclassify projects from breakdowns`.

______________________________________________________________________

### Task 9: Commit the browser workflow and synthetic fixture

**Files:**

- Modify: `cmd/testfixture/main.go`
- Create: `frontend/e2e/activity-project-reclassification.spec.ts`

**Interfaces:**

- Produces: synthetic `remote-example-host` sessions under
  `/srv/worktrees/github.com/example-org/sample-service/example-worktree` with
  immutable identity evidence and a deliberately wrong display project.

- Produces: writable SQLite and read-only DuckDB browser regression coverage.

- [x] **Step 1: Extend the fixture and prove the new data shape**

    Insert at least two root sessions for one remote machine, including a nested
    cwd. After session insertion, call `UpsertProjectIdentityObservation` with a
    synthetic worktree root so immutable snapshots group the two cwds. Add a Go
    fixture test or observable DB assertion for machine, cwd, project, and
    snapshot evidence.

- [x] **Step 2: Write the failing writable Playwright workflow**

    The committed spec must:

    1. navigate to Activity and reveal the dense Project action by hover and
       keyboard focus;
    1. open the modal and observe the preselected remote candidate;
    1. edit the prefix and observe authoritative totals;
    1. choose or enter `sample_service` and apply once;
    1. observe the refreshed breakdown and focus fallback;
    1. open Settings, select `remote-example-host`, and find the persisted rule
       and original label.

    Run the focused Chromium spec. Expected: fail before fixture/API/UI support is
    complete, then pass after wiring.

- [x] **Step 3: Add coarse-pointer and read-only browser coverage**

    Use a real Chromium touch/coarse-pointer context where supported to assert the
    action is visible without hover. Run the same screen against DuckDB and
    assert the action is present, tabbable, `aria-disabled`, explains the
    writable archive, and sends no candidate or mutation request.

- [x] **Step 4: Verify and commit**

    Run:

    ```bash
    cd frontend
    npx playwright test e2e/activity-project-reclassification.spec.ts \
      --project=chromium
    AGENTSVIEW_E2E_BACKEND=duckdb npx playwright test \
      e2e/activity-project-reclassification.spec.ts --project=chromium
    ```

    Expected: both exit 0. Commit as
    `test(e2e): cover activity project reclassification`.

______________________________________________________________________

### Task 10: Run whole-branch gates, review, and publish the agentsview PR

**Files:**

- Modify only files required by verified failures or review findings.

**Interfaces:**

- Consumes: every prior task.

- Produces: one scrubbed agentsview PR ready for human merge.

- [ ] **Step 1: Run format, generated-output, and focused gates**

    ```bash
    go fmt ./...
    go vet ./...
    make lint
    cd frontend && npm run i18n:compile && npm run check && npm run check:kit-ui && npm test
    cd ..
    ```

    Expected: exit 0 with no warnings treated as failures.

- [ ] **Step 2: Run complete backend and browser gates**

    ```bash
    make test
    make test-postgres
    make e2e
    make e2e-duckdb
    ```

    Expected: exit 0. Tests use scratch archives and dedicated test databases
    only.

- [ ] **Step 3: Perform the subagent whole-branch review**

    Generate a review package from the branch merge base through HEAD. Dispatch a
    fresh high-judgment reviewer against the design, this plan, the progress
    ledger, and the package. Fix every Critical or Important finding with one
    fresh fix subagent, rerun covering tests, and re-review until both spec
    compliance and code quality are approved.

- [ ] **Step 4: Run the explicitly requested roborev-fix workflow once**

    Run `roborev fix --list`, fetch every actionable open failing review, fix all
    findings in severity/file order, run the covering suites, comment and close
    exactly the original actionable jobs, commit fixes through the mandatory
    commit workflow, and audit each job with `roborev show --job <id> --json`
    until it reports `closed=true`. Do not invoke `roborev review`.

- [ ] **Step 5: Re-run final evidence gates after all review fixes**

    At minimum rerun `go fmt ./...`, `go vet ./...`, `make lint`, `make test`,
    `make test-postgres`, frontend i18n/check/tests, and both committed
    reclassification Playwright modes. Expected: every command exits 0 after the
    final code change.

- [ ] **Step 6: Scrub, commit remaining changes, push, and open one agentsview
  PR**

    Scan the complete unpublished commit range, messages, fixture output, any
    screenshots, and drafted PR text against the private-terms file and
    structural heuristics. Require zero hits. Ensure the worktree is clean, push
    the feature branch, and open one rationale-first agentsview PR with no
    test-plan section. Do not merge it.
