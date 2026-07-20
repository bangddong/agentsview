package sync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// newResyncSplitEngine builds an engine over a fresh archive with three synced
// Claude sessions. It returns the engine, its database, and the source root so
// callers can delete a source file and drive a resync.
func newResyncSplitEngine(t *testing.T) (*Engine, *db.DB, string) {
	t.Helper()
	root := t.TempDir()
	database, err := db.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)

	for _, name := range []string{"keep0", "keep1", "orphan"} {
		path := filepath.Join(root, "project", name+".jsonl")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		content := testjsonl.NewSessionBuilder().
			AddClaudeUser("2026-01-01T00:00:00Z", "hello "+name).
			AddClaudeAssistant("2026-01-01T00:00:01Z", "hi "+name).
			String()
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	require.Equal(t, 3, engine.SyncAll(context.Background(), nil).Synced)
	return engine, database, root
}

// TestResyncBuildThenSwapMatchesResyncAll drives the split resync path
// end-to-end: build the replacement, swap it in, reset caches. It must preserve
// an orphan session whose source file was deleted, clean up the temp file, and
// hand off warm skip state so an immediate sync is a no-op.
func TestResyncBuildThenSwapMatchesResyncAll(t *testing.T) {
	e, database, root := newResyncSplitEngine(t)
	require.NoError(t, os.Remove(filepath.Join(root, "project", "orphan.jsonl")))

	tempPath, stats, err := e.ResyncBuild(context.Background(), nil)
	require.NoError(t, err)
	require.False(t, stats.Aborted)
	require.FileExists(t, tempPath)

	require.NoError(t, e.SwapResyncDatabase(tempPath))
	require.NoError(t, e.ResetCachesAfterSwap())

	assert.False(t, database.NeedsResync())
	orphan, err := database.GetSession(context.Background(), "orphan")
	require.NoError(t, err)
	require.NotNil(t, orphan, "orphan sessions must survive the split resync")
	assert.Positive(t, stats.TotalSessions)
	assert.NoFileExists(t, tempPath)

	warm := e.SyncAll(context.Background(), nil)
	assert.Zero(t, warm.Synced, "persisted skip state must survive the swap")
}

// TestSwapWindowRejectsDirectWrites proves the write barrier: with the writer
// closed a direct star write is rejected with ErrWriterClosed, and the rejected
// write is absent from the rebuilt archive after the swap.
func TestSwapWindowRejectsDirectWrites(t *testing.T) {
	e, database, _ := newResyncSplitEngine(t)

	require.NoError(t, database.CloseWriter())
	_, starErr := database.StarSession("keep0")
	assert.ErrorIs(t, starErr, db.ErrWriterClosed,
		"a direct write during the barrier window must be rejected")

	// The build reads the original and writes only the replacement, so it
	// proceeds while the writer is closed. The swap reopens the writer.
	tempPath, _, err := e.ResyncBuild(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, e.SwapResyncDatabase(tempPath))
	require.NoError(t, e.ResetCachesAfterSwap())

	starred, err := database.ListStarredSessionIDs(context.Background())
	require.NoError(t, err)
	assert.NotContains(t, starred, "keep0",
		"a rejected write must not appear in the swapped archive")

	// The writer is usable again after the swap reopened it.
	ok, err := database.StarSession("keep0")
	require.NoError(t, err)
	assert.True(t, ok)
}

// TestResyncAbortsWhenBarrierCannotBeEstablished pins the CloseWriter failure
// posture: a resync that cannot establish a clean write barrier must abort
// before building instead of proceeding toward an unsafe swap, and the original
// archive must be left untouched.
func TestResyncAbortsWhenBarrierCannotBeEstablished(t *testing.T) {
	e, database, _ := newResyncSplitEngine(t)

	prev := closeWriterForResyncBarrier
	closeWriterForResyncBarrier = func(*db.DB) error {
		return errors.New("barrier boom")
	}
	defer func() { closeWriterForResyncBarrier = prev }()

	stats := e.ResyncAll(context.Background(), nil)
	assert.True(t, stats.Aborted, "resync must abort without a clean barrier")
	require.NotEmpty(t, stats.Warnings)
	assert.Contains(t, stats.Warnings[0], "close writer for barrier")
	assert.Zero(t, stats.Synced, "no build may run without the barrier")

	page, err := database.ListSessions(context.Background(), db.SessionFilter{})
	require.NoError(t, err)
	assert.Len(t, page.Sessions, 3, "original archive must be untouched")
	assert.NoFileExists(t, e.ResyncTempPath(),
		"no replacement build may be left behind")
}

// TestInProcessResyncRejectsConcurrentDirectWrite is the regression for the
// in-process fallback barrier. It fires a direct write (StarSession) during the
// reclassify phase, which runs after every preserved-state copy and before the
// swap's rename. Without the barrier that write lands in the original and is
// discarded by the swap (silently lost); with it the write is rejected with
// ErrWriterClosed. The progress hook blocks the resync until the write returns,
// so the write is guaranteed to land inside that window.
func TestInProcessResyncRejectsConcurrentDirectWrite(t *testing.T) {
	e, database, _ := newResyncSplitEngine(t)

	var starErr error
	var fired atomic.Bool
	done := make(chan struct{})
	onProgress := func(p Progress) {
		if p.Phase == PhaseReclassifying && fired.CompareAndSwap(false, true) {
			go func() {
				_, starErr = database.StarSession("keep0")
				close(done)
			}()
			<-done
		}
	}

	stats := e.ResyncAll(context.Background(), onProgress)
	require.False(t, stats.Aborted)
	require.True(t, fired.Load(), "the concurrent write must have fired mid-resync")

	assert.ErrorIs(t, starErr, db.ErrWriterClosed,
		"a direct write in the copy-to-swap window must be rejected, not lost")
	starred, err := database.ListStarredSessionIDs(context.Background())
	require.NoError(t, err)
	assert.NotContains(t, starred, "keep0",
		"a rejected write must not silently land in the swapped archive")
}
