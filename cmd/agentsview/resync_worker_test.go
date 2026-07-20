package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/sync"
)

// TestForegroundResyncRunnerFallsBackInProcess verifies the resync seam: under a
// test binary the runner takes the in-process ResyncAll arm (the worker arm is
// gated by !testing.Testing()), rebuilds the archive, and clears the stale-data
// flag. The worker build-and-swap mechanism is covered by the engine split tests
// and the resync-build worker mode test.
func TestForegroundResyncRunnerFallsBackInProcess(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, err := db.Open(cfg.DBPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	engine := sync.NewEngine(database, workerEngineConfig(cfg))
	t.Cleanup(engine.Close)
	require.Equal(t, 3, engine.SyncAll(context.Background(), nil).Synced)

	runner := newForegroundResyncRunner(context.Background(), cfg, engine, database)
	stats, err := runner(context.Background(), nil)

	require.NoError(t, err)
	assert.False(t, stats.Aborted)
	assert.Equal(t, 3, stats.Synced, "in-process resync fallback rebuilds the archive")
	assert.False(t, database.NeedsResync())
}
