package db

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
)

func TestWorktreeCandidateDiscoveryUsesActivityScopeAndProjectIdentity(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	const (
		clickedRaw = "/private/example/repository"
		otherRaw   = "/another/private/repository"
		inRange    = "2025-06-02T10:00:00Z"
		outRange   = "2025-06-03T10:00:00Z"
	)

	seedCandidateSession(t, d, "identity-a", clickedRaw, "host-a.example",
		"/srv/worktrees/repository/feature/cmd", inRange)
	seedCandidateSession(t, d, "identity-b", clickedRaw, "host-a.example",
		"/srv/worktrees/repository/feature/frontend", inRange)
	seedCandidateSession(t, d, "aggregate", clickedRaw, "host-b.example",
		"/srv/checkouts/repository/docs", inRange)
	seedCandidateSession(t, d, "fallback", clickedRaw, "host-b.example",
		"/opt/unknown/repository", inRange)
	seedCandidateSession(t, d, "unavailable", clickedRaw, "host-b.example", "", inRange)
	seedCandidateSession(t, d, "outside", clickedRaw, "host-a.example",
		"/srv/worktrees/repository/feature/outside", outRange)
	seedCandidateSession(t, d, "same-display-other-key", otherRaw, "host-a.example",
		"/srv/worktrees/other/main", inRange)
	seedCandidateSession(t, d, "other-machine-filter", clickedRaw, "host-c.example",
		"/srv/worktrees/repository/other", inRange)

	setCandidateSnapshot(t, d, "identity-a", clickedRaw, "host-a.example",
		"/srv/worktrees/repository", "/srv/worktrees/repository/feature")
	setCandidateSnapshot(t, d, "identity-b", clickedRaw, "host-a.example",
		"/srv/worktrees/repository", "/srv/worktrees/repository/feature")
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project: clickedRaw, Machine: "host-b.example",
			RootPath: "/srv/checkouts/repository",
		}), "seed aggregate evidence")
	deleteCandidateSnapshot(t, d, "fallback")
	deleteCandidateSnapshot(t, d, "unavailable")

	projects, err := d.BuildProjectIdentityMap(ctx, []string{clickedRaw, otherRaw})
	require.NoError(t, err)
	require.NotEqual(t, projects[clickedRaw].ProjectKey, projects[otherRaw].ProjectKey)
	assert.Equal(t, export.SafeProjectDisplayLabel(clickedRaw),
		export.SafeProjectDisplayLabel(otherRaw))

	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "day", Date: "2025-06-02", Timezone: "UTC",
	}, time.Date(2025, 6, 4, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	candidates, err := d.ListWorktreeReclassificationCandidates(ctx,
		WorktreeCandidateRequest{
			Filter: AnalyticsFilter{
				Timezone: "UTC", Machine: "host-a.example",
				ExcludeOneShot: false,
			},
			Query:             q,
			ClickedProject:    export.SafeProjectDisplayLabel(clickedRaw),
			ClickedProjectKey: projects[clickedRaw].ProjectKey,
		})
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Equal(t, "host-a.example", candidates[0].Machine)
	assert.Equal(t, "/srv/worktrees/repository/feature", candidates[0].SuggestedPrefix)
	assert.Equal(t, "snapshot", candidates[0].EvidenceKind)
	assert.Equal(t, 2, candidates[0].ContributingSessions)
	assert.Equal(t, 2, candidates[0].DistinctCwds)
	assert.True(t, candidates[0].Available)
	assert.Len(t, candidates[0].Examples, 2)
	assert.NotEmpty(t, candidates[0].ID)
}

func TestWorktreeCandidateDiscoveryFallsBackAndBoundsExamples(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	const raw = "branch-label"
	for i := range 12 {
		id := "aggregate-" + string(rune('a'+i))
		seedCandidateSession(t, d, id, raw, "host.example",
			"/srv/repository/subdir", "2025-06-02T10:00:00Z")
	}
	seedCandidateSession(t, d, "fallback", raw, "host.example",
		"/opt/exact", "2025-06-02T10:00:00Z")
	deleteCandidateSnapshot(t, d, "fallback")
	seedCandidateSession(t, d, "unavailable", raw, "host.example", "",
		"2025-06-02T10:00:00Z")
	deleteCandidateSnapshot(t, d, "unavailable")
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project: raw, Machine: "host.example", RootPath: "/srv/repository",
		}), "seed aggregate evidence")
	projects, err := d.BuildProjectIdentityMap(ctx, []string{raw})
	require.NoError(t, err)
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "day", Date: "2025-06-02", Timezone: "UTC",
	}, time.Date(2025, 6, 4, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)

	candidates, err := d.ListWorktreeReclassificationCandidates(ctx,
		WorktreeCandidateRequest{
			Filter: AnalyticsFilter{Timezone: "UTC"}, Query: q,
			ClickedProject: raw, ClickedProjectKey: projects[raw].ProjectKey,
		})
	require.NoError(t, err)
	require.Len(t, candidates, 3)
	assert.Equal(t, "aggregate", candidates[0].EvidenceKind)
	assert.Equal(t, 12, candidates[0].ContributingSessions)
	assert.Len(t, candidates[0].Examples, 10)
	assert.Equal(t, "fallback", candidates[1].EvidenceKind)
	assert.Equal(t, "/opt/exact", candidates[1].SuggestedPrefix)
	assert.False(t, candidates[2].Available)
	assert.Empty(t, candidates[2].SuggestedPrefix)
	assert.Equal(t, "unavailable", candidates[2].EvidenceKind)
	again, err := d.ListWorktreeReclassificationCandidates(ctx,
		WorktreeCandidateRequest{
			Filter: AnalyticsFilter{Timezone: "UTC"}, Query: q,
			ClickedProject: raw, ClickedProjectKey: projects[raw].ProjectKey,
		})
	require.NoError(t, err)
	assert.Equal(t, candidates, again, "candidate order and IDs should be deterministic")
}

func TestListArchiveWorktreeCandidatesIgnoresDateRange(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	const raw = "date-spread-project"

	seedCandidateSession(t, d, "old-session", raw, "host-a.example",
		"/srv/worktrees/repository/feature/cmd", "2020-01-01T10:00:00Z")
	seedCandidateSession(t, d, "new-session", raw, "host-a.example",
		"/srv/worktrees/repository/feature/frontend", "2025-06-02T10:00:00Z")
	setCandidateSnapshot(t, d, "old-session", raw, "host-a.example",
		"/srv/worktrees/repository", "/srv/worktrees/repository/feature")
	setCandidateSnapshot(t, d, "new-session", raw, "host-a.example",
		"/srv/worktrees/repository", "/srv/worktrees/repository/feature")

	projects, err := d.BuildProjectIdentityMap(ctx, []string{raw})
	require.NoError(t, err)

	candidates, err := d.ListArchiveWorktreeCandidates(ctx, ArchiveWorktreeCandidateRequest{
		ProjectLabel: export.SafeProjectDisplayLabel(raw),
		ProjectKey:   projects[raw].ProjectKey,
	})
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Equal(t, 2, candidates[0].ContributingSessions,
		"archive-wide selection must cover both the old and new session")

	// Sanity check: the same fixture through the Activity-scoped method with
	// a one-day query only sees the new session, confirming the archive-wide
	// method is not merely reproducing a wide-enough Activity window.
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "day", Date: "2025-06-02", Timezone: "UTC",
	}, time.Date(2025, 6, 4, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	scoped, err := d.ListWorktreeReclassificationCandidates(ctx,
		WorktreeCandidateRequest{
			Filter:            AnalyticsFilter{Timezone: "UTC"},
			Query:             q,
			ClickedProject:    export.SafeProjectDisplayLabel(raw),
			ClickedProjectKey: projects[raw].ProjectKey,
		})
	require.NoError(t, err)
	require.Len(t, scoped, 1)
	assert.Equal(t, 1, scoped[0].ContributingSessions)
}

func TestListArchiveWorktreeCandidatesKeyMismatch(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	const raw = "mismatch-project"

	seedCandidateSession(t, d, "session-a", raw, "host-a.example",
		"/srv/worktrees/repository/feature", "2025-06-02T10:00:00Z")
	setCandidateSnapshot(t, d, "session-a", raw, "host-a.example",
		"/srv/worktrees/repository", "/srv/worktrees/repository/feature")

	candidates, err := d.ListArchiveWorktreeCandidates(ctx, ArchiveWorktreeCandidateRequest{
		ProjectLabel: export.SafeProjectDisplayLabel(raw),
		ProjectKey:   "wrong-key",
	})
	require.NoError(t, err)
	assert.Empty(t, candidates, "right label with wrong key must return no candidates, no error")

	_, err = d.ListArchiveWorktreeCandidates(ctx, ArchiveWorktreeCandidateRequest{
		ProjectLabel: export.SafeProjectDisplayLabel(raw),
		ProjectKey:   "",
	})
	require.Error(t, err, "empty project key must error")
}

func TestListArchiveWorktreeCandidatesMatchesActivityScopedAllTime(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	const raw = "parity-project"

	seedCandidateSession(t, d, "identity-a", raw, "host-a.example",
		"/srv/worktrees/repository/feature/cmd", "2025-06-02T10:00:00Z")
	seedCandidateSession(t, d, "identity-b", raw, "host-a.example",
		"/srv/worktrees/repository/feature/frontend", "2025-06-02T10:00:00Z")
	seedCandidateSession(t, d, "aggregate", raw, "host-b.example",
		"/srv/checkouts/repository/docs", "2025-06-02T10:00:00Z")
	setCandidateSnapshot(t, d, "identity-a", raw, "host-a.example",
		"/srv/worktrees/repository", "/srv/worktrees/repository/feature")
	setCandidateSnapshot(t, d, "identity-b", raw, "host-a.example",
		"/srv/worktrees/repository", "/srv/worktrees/repository/feature")
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project: raw, Machine: "host-b.example",
			RootPath: "/srv/checkouts/repository",
		}), "seed aggregate evidence")
	deleteCandidateSnapshot(t, d, "aggregate")

	projects, err := d.BuildProjectIdentityMap(ctx, []string{raw})
	require.NoError(t, err)

	allTime := activity.Query{
		RangeStart: time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
		RangeEnd:   time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC),
		Loc:        time.UTC,
	}
	scoped, err := d.ListWorktreeReclassificationCandidates(ctx,
		WorktreeCandidateRequest{
			Filter:            AnalyticsFilter{Timezone: "UTC"},
			Query:             allTime,
			ClickedProject:    export.SafeProjectDisplayLabel(raw),
			ClickedProjectKey: projects[raw].ProjectKey,
		})
	require.NoError(t, err)

	archiveWide, err := d.ListArchiveWorktreeCandidates(ctx,
		ArchiveWorktreeCandidateRequest{
			ProjectLabel: export.SafeProjectDisplayLabel(raw),
			ProjectKey:   projects[raw].ProjectKey,
		})
	require.NoError(t, err)

	assert.Equal(t, scoped, archiveWide,
		"archive-wide and Activity-scoped-all-time must produce identical candidates")
}

func seedCandidateSession(
	t *testing.T, d *DB, id, project, machine, cwd, started string,
) {
	t.Helper()
	ended := started
	require.NoError(t, d.UpsertSession(Session{
		ID: id, Project: project, Machine: machine, Agent: "codex", Cwd: cwd,
		StartedAt: &started, EndedAt: &ended, MessageCount: 1,
	}), "seed candidate session %s", id)
}

func deleteCandidateSnapshot(t *testing.T, d *DB, id string) {
	t.Helper()
	_, err := d.getWriter().Exec(
		`DELETE FROM session_project_identity_snapshots WHERE session_id = ?`, id)
	require.NoError(t, err)
}

func setCandidateSnapshot(
	t *testing.T, d *DB, id, project, machine, root, worktreeRoot string,
) {
	t.Helper()
	deleteCandidateSnapshot(t, d, id)
	_, err := d.getWriter().Exec(`
		INSERT INTO session_project_identity_snapshots (
			session_id, project, machine, root_path, worktree_root_path, observed_at
		) VALUES (?, ?, ?, ?, ?, ?)`,
		id, project, machine, root, worktreeRoot, "2025-06-02T10:00:00Z")
	require.NoError(t, err)
}
