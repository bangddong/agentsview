# Activity project reclassification

## Summary

agentsview can misclassify sessions from worktrees whose directory layout is
self-describing but not recognized by the parser. The immediate example is the
generic layout:

```text
worktrees/github.com/<owner>/<repository>/<worktree>
```

When the remote working directory is unavailable on the machine doing the
ingestion, the parser cannot inspect Git metadata and falls back to the leaf
directory. The worktree name then appears as the project in Activity and other
analytics.

This design has two independently deliverable parts:

1. Recognize the generic GitHub worktree layout in the parser so sessions with
   available source files self-heal through the normal parser-version resync.
1. Add a discoverable Activity workflow for genuinely ambiguous cases. The
   workflow creates or edits the existing `worktree_project_mappings` rules;
   it does not introduce a second project-override system.

The parser fix lands first as a small pull request. The mapping and UI work
lands second.

## Goals

- Derive the repository project from the generic GitHub worktree layout.
- Let a user reclassify a project directly from the Activity project breakdown.
- Keep `worktree_project_mappings` as the only user-managed classification
  mechanism.
- Preview the full historical impact using the same matching semantics as apply.
- Manage rules for local and remotely synced machines in the writable archive.
- Preserve PostgreSQL and DuckDB behavior and query-shape parity after a project
  move.
- Keep the workflow accessible, localized, reversible by retargeting the rule,
  and explicit about resync behavior.

## Non-goals

- A general per-session override or provenance system.
- Automatic undo when a rule is disabled or deleted.
- Speculative detection or badging of labels that resemble branch names.
- Direct mutation from `pg serve`, DuckDB, or another read-only store.
- Generic worktree layouts for code hosts other than GitHub without a concrete
  example that needs them.

## Architecture and sequencing

### Pull request 1: parser layout fix

Add a generic worktree layout equivalent to:

```text
worktrees/github.com/$OWNER/$REPOSITORY/$WORKTREE[/...]
```

The repository segment is the project (`projectPart: 1`) and at least owner,
repository, and worktree segments are required. Normal project-name
normalization still applies, so a repository such as `sample-service` becomes
`sample_service`.

A cwd that stops at `worktrees/github.com/$OWNER/$REPOSITORY` intentionally does
not match this layout because it has no worktree segment. The ordinary
leaf-basename fallback already produces the repository name for that case.

The parser data version advances. Deploying the change requires rebuilding and
restarting the daemon, after which the designed full resync reparses sessions
whose source files still exist. Those sessions acquire the repository label
without a mapping. Orphaned sessions cannot be reparsed and keep their archived
label.

### Pull request 2: mapping workflow

Add an Activity entry point over the existing mapping system. The rule lives in
the writable SQLite archive that syncs the affected machine's sessions. It does
not need to live on the source machine.

Classification precedence is explicit:

1. The parser derives its best project label.
1. The sync engine applies enabled worktree mappings after parsing.
1. An enabled mapping therefore always wins when parser inference and explicit
   user intent disagree.

The same-answer overlap after both pull requests is harmless. A mapping can
remain enabled even when the parser now derives the same canonical project.
During a full resync, carryover mappings are reapplied for every represented
machine, not only the writable archive's local machine.

Settings remains the management surface for listing, editing, disabling,
deleting, and reapplying rules. Activity is only a more discoverable creation
and correction workflow.

## Data model

Add one column to `worktree_project_mappings` through a non-destructive column
migration:

```sql
original_project TEXT NOT NULL DEFAULT ''
```

`original_project` is informational. It never participates in matching,
precedence, filtering, identity resolution, or publication.

Its invariant is set-once:

- Activity supplies the exact project label whose row opened the modal.
- An empty value may transition to that non-empty label.
- A non-empty value is immutable through every edit path.
- Rules created directly in Settings may keep an empty value.
- Editing an existing Settings-created rule from Activity may fill its empty
  value.

This captures what the user saw without trying to derive an "original" value
from the sessions matched by a later, possibly broader prefix.

The parser-triggered database rebuild already carries worktree mappings into the
fresh database. That copy path must include `original_project` when the old
database has the column and default it to empty when copying an older schema.
The mapping's unique `(machine, path_prefix)` identity and existing precedence
remain unchanged.

The Huma response model and generated frontend model expose `original_project`.
Update request models do not permit overwriting a non-empty value.

## Machine-scoped management

The database supports mappings for any machine, but the current Settings routes
implicitly select the sync engine's local machine. Extend them so the writable
archive can manage every machine represented by stored sessions or mappings.

- Listing accepts a machine selector.
- Create accepts a machine, defaulting to the local engine machine for backward
  compatibility.
- Update, delete, apply, and preview resolve the mapping's actual machine rather
  than silently substituting the local machine.
- Settings offers a bounded machine typeahead populated from archive metadata
  and existing mappings.

This makes a rule created for a remotely synced machine visible and manageable
in Settings.

## Candidate discovery

Add a local-only Activity candidate endpoint. Its request contains:

- the clicked project label;
- the clicked row's opaque project identity key;
- the current Activity range and filters;
- the current timezone and automation scope.

The server reruns the Activity candidate-session selection. Discovery is scoped
to the view because it answers "which sessions produced the row I clicked?" It
does not estimate the final rewrite impact.

Group the contributing sessions by machine and the best available repository or
worktree identity evidence:

1. Prefer the immutable per-session identity snapshot's worktree root or
   repository identity.
1. Use compatible aggregate project identity observations when they can safely
   associate a session path with the same worktree root.
1. Without usable identity evidence, keep each exact cwd as its own candidate.

For an identity-backed group, compute the suggested mapping prefix as the
longest common directory prefix of its recorded cwds, cut only at a path
separator boundary. This collapses sessions that ran from different
subdirectories of one observed worktree.

The exact-cwd fallback is deliberately conservative. Automatically merging
unidentified sibling paths could cross repository boundaries. Users can safely
broaden the editable prefix while watching the live impact preview.

Each candidate contains a stable response-local identifier, machine, suggested
prefix, contributing-session count, distinct-cwd count, and bounded examples.
Candidates with no usable cwd are shown as unavailable rather than being
silently dropped.

## Full-archive preview

Add a local-only dry-run endpoint owned by the worktree-mapping API. Preview
accepts the selected machine, draft path prefix, explicit target project, and an
optional existing mapping ID.

Unlike candidate discovery, preview evaluates the entire visible archive. A rule
created from a one-day Activity view can rewrite matching sessions from all
dates. The response makes that historical scope explicit.

Preview simulates the draft alongside all enabled mappings for the selected
machine. It uses the same directory-boundary matcher, path normalization, layout
resolver, longest-prefix precedence, deletion visibility, and sibling cwd
fallback as apply and the sync engine. A more-specific existing rule keeps
winning over a broader draft.

Path normalization follows the stored path's slash style rather than the
writable archive host's operating system, so a central Unix archive can safely
evaluate Windows session paths and vice versa.

The response contains authoritative totals only, plus bounded samples:

- matched sessions;
- sessions whose project would change;
- distinct current projects among the changing sessions;
- normalized target project;
- up to 10 project-count samples;
- up to 10 session samples.

No unbounded session or project list is returned. A distinct-project count
greater than one is a warning, not a hard block. Zero matches disables apply.

Prefix edits debounce and cancel obsolete preview reads. The database owns the
evaluation; the frontend never reimplements prefix matching or impact counts.

### Preview conflict token

Preview returns a stable token over the selected machine's mapping set,
including mapping identity, prefix, layout, target, enabled state, and update
version. It does not include global archive or session revisions.

Apply rejects the request if that machine's mapping token changed after the
preview. Another rule can change longest-prefix precedence and is therefore a
real conflict. New or updated sessions do not invalidate the preview: the
enabled rule is intended to classify later arrivals, and apply reevaluates the
current matched set under the write lock.

Preview also resolves an exact `(machine, path_prefix)` collision and returns
that mapping's ID. Apply may edit only that returned row; callers do not choose
an unrelated existing mapping ID.

## Atomic create or edit and apply

Add one mutation endpoint for the Activity modal. It receives the accepted
preview token and draft rule.

Within the sync engine's exclusive write boundary and one SQLite transaction,
it:

1. validates the current mapping-set token;
1. creates the rule or edits the existing `(machine, path_prefix)` rule;
1. sets `original_project` only if its stored value is empty;
1. reevaluates the full archive with the shared evaluator;
1. rewrites the matched sessions whose project changes;
1. bumps `local_modified_at` on every rewritten session;
1. rebuilds aggregate identity observations for the affected former labels and
   the target label on that machine;
1. commits the rule, rewrites, and observations together.

If a rule already exists for the machine and prefix, the modal edits it rather
than reporting only a uniqueness conflict. A non-empty `original_project` on
that rule remains unchanged.

Apply returns its actual matched and updated counts. Session arrivals after
preview are benign and may make the actual totals larger. A mapping-set change
returns a conflict and requires a fresh preview.

The exclusive lock means a broad user-initiated rewrite temporarily blocks the
watcher and other sync work. This is accepted: SQLite is a single-writer store,
the operation must be atomic, and the write work is bounded by the matched
session set disclosed in preview. Evaluation scans only the selected machine's
candidate sessions, so unrelated machines do not lengthen the critical section.

## Project identity consistency

Per-session project identity snapshots are immutable source evidence. A manual
classification does not rewrite them, including their parser-time source project
label. Snapshot publication therefore remains source-labelled, while session
publication and rebuilt aggregate observations follow the current mapped label.

Aggregate observations describe how current project labels relate to repository
and worktree evidence. After changing session labels, rebuild the aggregates for
the target and every former label affected on that machine:

- copy real repository/worktree facts from the immutable snapshots or existing
  observations;
- associate those facts with the sessions' current mapped labels;
- delete former label/root observations no longer supported by any visible
  session;
- retain observations still supported by unaffected sessions.

Observation inserts, updates, and deletes advance the existing identity
publication revision. Deletes become downstream tombstones. Missing evidence
stays explicitly unknown; the mapping does not invent a Git remote or worktree
relationship.

This prevents both failure modes: a newly assigned label with no identity
evidence and a ghost former label whose sessions have all moved away.

## Mirror publication and backend parity

The mapping rewrite updates `local_modified_at`, and session publication
fingerprints include `project`. Reclassified sessions therefore become
incremental candidates and cannot be skipped as unchanged.

Treat reclassification as a project-scope move, not only an upsert:

- the session appears under the new project;
- stale membership under the former project disappears;
- include/exclude project-filtered publication scopes converge correctly;
- aggregate identity additions and tombstones publish through the existing
  revision mechanism.

SQLite owns the mapping mutation. PostgreSQL and DuckDB remain read-only
consumers and must expose the same post-publication project filtering,
aggregation, ordering, and identity behavior.

`pg serve`, DuckDB, and other read-only stores do not expose mutation endpoints.
Their Activity action remains discoverable but `aria-disabled`, with guidance to
use the writable archive that syncs the affected machine's sessions.

## Activity user interface

### Breakdown action

Only Project rows receive a small kit-ui `IconButton`. Model and Agent rows keep
their current shared rendering without the action.

The action remains in the DOM and keyboard order. On fine pointers it is
visually revealed by row hover, row focus-within, or its own focus without
changing the 14-pixel bar geometry or row width. Under `pointer: coarse`, it is
always visible so the feature is not undiscoverable on touch devices.

Read-only mode uses `aria-disabled` plus click suppression, or a tooltip wrapper
supported by kit-ui, rather than relying on pointer events from a natively
disabled button. The action has localized accessible text.

### Modal flow

The modal shows:

- the clicked label as "Originally shown as";
- a preselected summary when one candidate exists;
- a required bounded selector when several candidates exist;
- the candidate machine and contributing-session count;
- an editable kit-ui `TextInput` for the path prefix;
- a target project control;
- the debounced full-archive impact preview;
- a Settings link explaining where the persistent rule is managed.

The target control extends the existing `ProjectTypeahead`. It excludes the "All
projects" option, suggests known archive projects, and permits committing a
non-empty custom project. If the pinned kit-ui `Typeahead` lacks that contract,
add it upstream and bump the dependency instead of implementing one-off local
control chrome.

Use the same upstream change to add a localized close-button label to kit-ui
`Modal`, resolving the known hard-coded English gap documented in `DESIGN.md`.

The preview displays matched and changing session totals plus the distinct
project count. More than one current project uses warning styling and bounded
project samples. Apply stays disabled for invalid input, a pending preview, a
stale preview, or zero matches.

### Success and refresh

After successful apply, explicitly reload the Activity report and invalidate and
reload project filter options. Do not rely on sync SSE: Activity deliberately
avoids report reloads on every sync event.

If apply succeeds and report refresh fails, keep the modal in an "Applied,
refresh failed" state with a refresh-only retry. Never offer a second Apply for
the already committed mutation.

After a successful refresh, close the modal. Return focus to the initiating
button if it still exists; if the source row disappeared after aggregation,
focus the Project panel heading.

## Settings user interface

Extend Worktree mappings with:

- a machine typeahead covering local and remotely synced machines;
- `original_project` rendered as "Originally shown as ..." when present;
- the existing edit, enable, apply, and delete controls scoped to the selected
  machine;
- confirmation before deleting or disabling a rule.

On read-only stores, Settings keeps the existing local-only informational state.
It does not list mappings or render disabled management controls; the Activity
action is the discoverable surface that explains where changes must be made.

The localized confirmation explains:

> Sessions already reclassified keep their current project initially, but
> sessions whose source files still exist can revert to parser-derived names on
> a later reparse or full resync. Orphaned sessions retain their stored
> classification.

Editing a target and reapplying is the supported manual reversal. A user can use
the remembered original label as the replacement target without requiring
per-session provenance.

## Localization and accessibility

Every new heading, field label, tooltip, warning, error, success state,
confirmation, button label, empty state, and accessible name uses Paraglide
messages. Add identical key sets to every locale listed in
`frontend/project.inlang/settings.json` in the same change.

Count-sensitive copy uses plural variants and locale-aware formatted numbers.
Technical paths, machine names, and project identifiers are not translated.

The modal supports keyboard-only candidate selection, editing, preview, apply,
cancel, Settings navigation, and focus restoration. The project-row action is
screen-reader visible even when visually hidden on fine pointers.

## Failure behavior

- Candidate discovery and preview perform no writes.
- Invalid or zero-impact prefixes cannot be applied.
- Directory-boundary semantics prevent a prefix such as `/worktrees/service`
  from matching `/worktrees/service-old`.
- A mapping-set conflict refreshes preview rather than applying stale
  precedence.
- Any rule, session, or observation write failure rolls back the entire apply.
- A post-commit Activity refresh failure never reports the mapping as failed.
- Read-only stores return a clear unsupported response for mutation APIs.

## Testing

### Pull request 1

- Table-driven parser tests cover the generic layout at the worktree root and in
  nested directories.
- Guard cases cover insufficient segments, adjacent non-matching directory
  names, and project normalization.
- A remote-sync fixture uses a foreign cwd unavailable on the test machine and
  proves the repository segment wins over the worktree leaf.
- The current data-version test proves the parser change requests a full resync.

### Pull request 2 database and API

- The column migration preserves an existing archive.
- `original_project` may move from empty to non-empty but cannot be overwritten.
- New-schema and old-schema resync copies preserve or default the field.
- Machine-scoped Settings APIs manage local and remotely synced machines.
- Candidate tests cover identity grouping, path-boundary common prefixes,
  exact-cwd fallback, unavailable cwd, and Activity-range scoping.
- Preview tests cover full-archive impact, longest-prefix precedence, directory
  boundaries, distinct-project totals, and response bounds.
- Mapping-token tests prove mapping edits conflict while unrelated or new
  session writes do not.
- Apply tests prove preview/evaluator parity, atomic rollback,
  `local_modified_at`, existing-rule edit behavior, and sync-lock
  serialization.
- Identity tests prove target observations appear, supported former observations
  remain, empty former observations publish tombstones, and immutable
  snapshots do not change.
- Read-only server tests reject mapping mutation endpoints and expose
  explanatory UI state.

### Mirror parity

- PostgreSQL and DuckDB integration tests cover unfiltered and include/exclude
  project-filtered publication.
- Each case asserts the new project is present and stale former-project
  membership is absent.
- Fingerprint and identity-revision tests protect both publication triggers.

### Resync lifecycle round trip

Add an integration test that creates both a live-source session and an orphaned
session under one mapping, applies the mapping, and performs a full resync.

- The live-source session keeps the target because parsing is followed by the
  still-enabled mapping.
- The orphaned session keeps the target because orphan preservation copies the
  archived row.
- A companion disable/delete case documents that live-source sessions can return
  to parser-derived labels while the orphan stays classified.

This protects the composed user guarantee rather than only its individual units.

### Frontend and Playwright

Component tests cover:

- project-only action rendering and keyboard access;
- fine-pointer hover/focus reveal state and coarse-pointer visibility contract;
- read-only `aria-disabled` behavior and explanation;
- one and multiple candidates;
- custom project targets;
- cancelation of obsolete debounced previews;
- multi-project warnings and zero-match blocking;
- one Apply call, explicit report refresh, and refresh-only retry after a
  post-commit load failure;
- Settings machine switching, original-label display, and disable/delete
  confirmation.

Commit a Playwright spec under `frontend/e2e/` for the complete user-visible
workflow. Extend `cmd/testfixture` with generic, non-private remote-machine
worktree sessions so the spec can:

1. verify the dense Project row action reveals on hover and keyboard focus;
1. open the modal and inspect the preselected candidate;
1. edit the prefix and observe authoritative impact counts;
1. choose or enter the target and apply once;
1. observe focus restoration and the refreshed Activity breakdown;
1. open Settings and find the persisted remote-machine rule and original label.

CSS media-query behavior that jsdom cannot compute is covered by the committed
stylesheet/component contract and a real-browser coarse-pointer Playwright
context where supported. Any browser-engine limitation is documented in the spec
beside the narrower assertion; it is not replaced by a source-grep test.

## Validation

For each implementation pull request, run the relevant focused tests first and
then the repository gates:

- `go fmt ./...`
- `go vet ./...`
- `make test`
- `make lint`
- `make test-postgres`
- relevant DuckDB integration tests
- `npm run i18n:compile` from `frontend/`
- `npm run check` from `frontend/`
- `npm run check:kit-ui` from `frontend/`, comparing the exact diagnostic set
  with the clean pre-feature branch baseline. Reject new or changed
  diagnostics; the 37 unrelated baseline findings remain separate cleanup
  work.
- frontend component tests
- the committed Playwright reclassification spec

Run `make lint` over the complete branch before each pull request is opened, not
only after individual implementation tasks, so golangci-lint and NilAway see
every cross-task code path.

Run the private-data scrub over code, fixtures, commit messages, and pull
request descriptions. Tests and documentation use reserved example projects,
generic host labels, and synthetic paths rather than details from the incident
that motivated the feature.

## Accepted trade-offs

- Unknown identity groups fall back to exact cwd rather than speculative sibling
  clustering.
- Disabling or deleting a rule is not immediate undo.
- A broad apply briefly blocks the watcher, bounded by the matched set and
  initiated explicitly by the user.
- Touch users see a permanently visible action instead of the denser hover-only
  treatment.
- The first parser fix recognizes GitHub only; more hosts require evidence and
  their own guarded layouts.
