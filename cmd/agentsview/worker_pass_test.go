package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/sync"
)

// openTestWriteDB opens a writable DB and acquires the write-owner lock for cfg,
// cleaning both up at test end.
func openTestWriteDB(t *testing.T, cfg config.Config) (*db.DB, *writeOwnerLock) {
	t.Helper()
	database, lock, err := openWriteDB(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { closeWriteDB(database, lock) })
	return database, lock
}

// writeOneSession attempts a single write through the DB writer pool.
func writeOneSession(database *db.DB) error {
	return database.UpsertSession(db.Session{
		ID:      "worker-pass-write",
		Project: "handoff",
		Machine: "local",
		Agent:   "claude",
	})
}

// stubLaunchSyncWorker swaps the launchSyncWorker seam for a test double and
// restores it when the returned func runs.
func stubLaunchSyncWorker(
	t *testing.T,
	fn func(
		context.Context, config.Config, string, func(workerLine),
	) (workerResult, error),
) func() {
	t.Helper()
	prev := launchSyncWorker
	launchSyncWorker = fn
	return func() { launchSyncWorker = prev }
}

func TestRunWorkerWritePassYieldsWriteOwnership(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	engine := sync.NewEngine(database, sync.EngineConfig{})
	defer engine.Close()

	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, _ config.Config, mode string, _ func(workerLine),
	) (workerResult, error) {
		assert.Equal(t, "audit", mode)
		assert.False(t, lock.Held(), "flock released before spawn")
		return workerResult{Status: "ok", DiscoveryComplete: true}, nil
	})
	defer restore()

	result, err := runWorkerWritePass(
		context.Background(), context.Background(), cfg, engine, database, lock, "audit", nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "ok", result.Status)
	assert.True(t, lock.Held(), "flock reacquired after the worker")
	assert.NoError(t, writeOneSession(database), "writer reopened")
}

func TestRunWorkerWritePassReacquiresOnWorkerFailure(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	engine := sync.NewEngine(database, sync.EngineConfig{})
	defer engine.Close()

	workerErr := errors.New("worker boom")
	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, _ config.Config, _ string, _ func(workerLine),
	) (workerResult, error) {
		return workerResult{Status: "failed"}, workerErr
	})
	defer restore()

	result, err := runWorkerWritePass(
		context.Background(), context.Background(), cfg, engine, database, lock, "audit", nil,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, workerErr, "worker error must propagate")
	assert.Equal(t, "failed", result.Status)
	assert.True(t, lock.Held(), "flock reacquired even after worker failure")
	assert.NoError(t, writeOneSession(database),
		"writer reopened even after worker failure")
}

func TestRunWorkerWritePassRetriesReacquireUntilLockFree(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	engine := sync.NewEngine(database, sync.EngineConfig{})
	defer engine.Close()

	prevInitial, prevMax := reacquireBackoffInitial, reacquireBackoffMax
	reacquireBackoffInitial = 5 * time.Millisecond
	reacquireBackoffMax = 20 * time.Millisecond
	defer func() {
		reacquireBackoffInitial, reacquireBackoffMax = prevInitial, prevMax
	}()

	closeErr := make(chan error, 1)
	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, _ config.Config, _ string, _ func(workerLine),
	) (workerResult, error) {
		// The daemon released the flock for the pass; a contender grabs it and
		// holds it briefly so the first reacquire attempts fail, then releases
		// so the retry loop must recover instead of stranding the writer.
		contender, err := tryAcquireWriteOwnerLock(cfg.DataDir)
		require.NoError(t, err, "contender takes the freed lock")
		go func() {
			time.Sleep(40 * time.Millisecond)
			closeErr <- contender.Close()
		}()
		return workerResult{Status: "ok", DiscoveryComplete: true}, nil
	})
	defer restore()

	result, err := runWorkerWritePass(
		context.Background(), context.Background(), cfg, engine, database, lock, "audit", nil,
	)
	require.NoError(t, err, "pass recovers once the contender releases the lock")
	require.NoError(t, <-closeErr, "contender released the lock cleanly")
	assert.Equal(t, "ok", result.Status)
	assert.True(t, lock.Held(), "flock eventually reacquired")
	assert.NoError(t, writeOneSession(database), "writer reopened after recovery")
}

// TestRunWorkerSyncPassRecordsSyncBookkeeping pins SyncThenRun parity for the
// worker-backed foreground sync: the full SyncStats payload reaches the
// caller, last-sync state feeds /sync/status, startup is reconciled, and the
// "sync" event is emitted for subscribers.
func TestRunWorkerSyncPassRecordsSyncBookkeeping(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	em := &scopedEmitter{scopes: make(chan string, 4)}
	engine := sync.NewEngine(database, sync.EngineConfig{Emitter: em})
	defer engine.Close()

	workerStats := sync.SyncStats{
		TotalSessions:  5,
		Synced:         3,
		OrphanedCopied: 2,
		Warnings:       []string{"one warning"},
	}
	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, _ config.Config, mode string, _ func(workerLine),
	) (workerResult, error) {
		assert.Equal(t, "sync", mode)
		return workerResult{
			Status: "ok", Synced: 3, DiscoveryComplete: true,
			Stats: &workerStats,
		}, nil
	})
	defer restore()

	stats, ran, err := runWorkerSyncPass(
		context.Background(), context.Background(), cfg, engine, database, lock, false, nil,
	)
	require.NoError(t, err)
	assert.True(t, ran)
	assert.Equal(t, 5, stats.TotalSessions,
		"the full SyncStats payload must survive the worker protocol")
	assert.Equal(t, 2, stats.OrphanedCopied)
	assert.Equal(t, []string{"one warning"}, stats.Warnings)
	assert.False(t, engine.LastSync().IsZero(),
		"last-sync state must reflect the worker-backed pass")
	assert.Equal(t, stats, engine.LastSyncStats())
	assert.True(t, engine.StartupReconciled())
	select {
	case scope := <-em.scopes:
		assert.Equal(t, "sync", scope)
	default:
		require.FailNow(t, "a worker-backed sync with changes must emit the sync event")
	}
}

// TestRunWorkerSyncPassLockHeldRecheckSkipsDuplicate is the regression for the
// deferred-startup race: the timer fires while a foreground worker pass is
// still running, so the pre-lock StartupReconciled check reads false. The
// foreground pass records reconciliation before releasing the exclusive lock,
// and the deferred pass rechecks the gate while holding it, so exactly one
// archive-scale worker runs.
func TestRunWorkerSyncPassLockHeldRecheckSkipsDuplicate(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	engine := sync.NewEngine(database, sync.EngineConfig{})
	defer engine.Close()

	launches := make(chan struct{}, 2)
	release := make(chan struct{})
	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, _ config.Config, _ string, _ func(workerLine),
	) (workerResult, error) {
		launches <- struct{}{}
		<-release
		return workerResult{Status: "ok", Synced: 1, DiscoveryComplete: true}, nil
	})
	defer restore()

	require.False(t, engine.StartupReconciled(),
		"the deferred fallback's pre-lock gate must read false mid-race")

	type passOutcome struct {
		ran bool
		err error
	}
	foreground := make(chan passOutcome, 1)
	go func() {
		_, ran, err := runWorkerSyncPass(
			context.Background(), context.Background(), cfg, engine, database, lock, false, nil,
		)
		foreground <- passOutcome{ran: ran, err: err}
	}()
	<-launches // the foreground worker is running and holds the exclusive lock

	deferred := make(chan passOutcome, 1)
	go func() {
		_, ran, err := runWorkerSyncPass(
			context.Background(), context.Background(), cfg, engine, database, lock, true, nil,
		)
		deferred <- passOutcome{ran: ran, err: err}
	}()
	// Let the deferred pass reach the exclusive lock before the foreground
	// worker finishes, then release the worker.
	time.Sleep(100 * time.Millisecond)
	close(release)

	fg := <-foreground
	require.NoError(t, fg.err)
	assert.True(t, fg.ran)
	df := <-deferred
	require.NoError(t, df.err)
	assert.False(t, df.ran,
		"the deferred pass must skip after the foreground pass reconciled startup")
	assert.Len(t, launches, 0,
		"exactly one archive-scale worker may launch across the race")
	assert.True(t, lock.Held(), "flock restored after both passes")
	assert.NoError(t, writeOneSession(database), "writer restored after both passes")
}

func TestRunWorkerWritePassRecoversLockAfterRequestCancel(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	engine := sync.NewEngine(database, sync.EngineConfig{})
	defer engine.Close()

	prevInitial, prevMax := reacquireBackoffInitial, reacquireBackoffMax
	reacquireBackoffInitial = 5 * time.Millisecond
	reacquireBackoffMax = 20 * time.Millisecond
	defer func() {
		reacquireBackoffInitial, reacquireBackoffMax = prevInitial, prevMax
	}()

	ctx, cancel := context.WithCancel(context.Background())
	closeErr := make(chan error, 1)
	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, _ config.Config, _ string, _ func(workerLine),
	) (workerResult, error) {
		// The client disconnects mid-pass while a contender briefly holds the
		// freed lock: recovery must still reacquire and reopen the writer.
		contender, err := tryAcquireWriteOwnerLock(cfg.DataDir)
		require.NoError(t, err, "contender takes the freed lock")
		cancel()
		go func() {
			time.Sleep(40 * time.Millisecond)
			closeErr <- contender.Close()
		}()
		return workerResult{Status: "ok", DiscoveryComplete: true}, nil
	})
	defer restore()

	result, err := runWorkerWritePass(
		ctx, context.Background(), cfg, engine, database, lock, "sync", nil,
	)
	require.NoError(t, err,
		"recovery must survive request cancellation during contention")
	require.NoError(t, <-closeErr, "contender released the lock cleanly")
	assert.Equal(t, "ok", result.Status)
	assert.True(t, lock.Held(), "flock reacquired despite cancelled request")
	assert.NoError(t, writeOneSession(database),
		"writer reopened despite cancelled request")
}

func TestReacquireWriteOwnerLockStopsOnContextCancel(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	_, lock := openTestWriteDB(t, cfg)
	require.NoError(t, lock.Release())

	prevInitial := reacquireBackoffInitial
	reacquireBackoffInitial = time.Hour // never elapses within the test
	defer func() { reacquireBackoffInitial = prevInitial }()

	// A contender holds the lock so every reacquire attempt fails.
	contender, err := tryAcquireWriteOwnerLock(cfg.DataDir)
	require.NoError(t, err)
	defer func() { assert.NoError(t, contender.Close()) }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = reacquireWriteOwnerLock(ctx, lock, "audit")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestReadWorkerResultRequiresExactlyOneResult(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantErr   string
		wantLines int
	}{
		{
			name: "single result",
			input: `{"progress":{"phase":"syncing"}}` + "\n" +
				`{"result":{"status":"ok","discoveryComplete":true}}` + "\n",
			wantLines: 2,
		},
		{
			name:      "zero results is a protocol failure",
			input:     `{"progress":{"phase":"syncing"}}` + "\n",
			wantErr:   "0 terminal results",
			wantLines: 1,
		},
		{
			name: "duplicate results is a protocol failure",
			input: `{"result":{"status":"ok","discoveryComplete":true}}` + "\n" +
				`{"result":{"status":"ok","discoveryComplete":true}}` + "\n",
			wantErr:   "2 terminal results",
			wantLines: 2,
		},
		{
			name: "malformed line is a protocol failure",
			input: "not json\n" +
				`{"result":{"status":"ok","discoveryComplete":true}}` + "\n",
			wantErr:   "malformed",
			wantLines: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen := 0
			result, err := readWorkerResult(
				strings.NewReader(tt.input),
				func(workerLine) { seen++ },
			)
			assert.Equal(t, tt.wantLines, seen, "forwarded line count")
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "ok", result.Status)
			assert.True(t, result.DiscoveryComplete)
		})
	}
}

func TestSyncWorkerAcquiresWriteLockWhenDaemonYielded(t *testing.T) {
	dataDir, cfg := writeDBConfigForTest(t)
	t.Setenv(syncWorkerChildEnvVar, "1")
	_, err := WriteDaemonRuntime(dataDir, "127.0.0.1", 9, "test", false)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dataDir) })

	// The daemon lives but has yielded: writer closed, write lock free.
	database, release, err := openWorkerWriteDB(cfg)
	require.NoError(t, err,
		"worker must acquire while a yielded daemon still lives")
	release()
	database.Close()
}

func TestSyncWorkerRefusedWhenDaemonHoldsWriteLock(t *testing.T) {
	dataDir, cfg := writeDBConfigForTest(t)
	t.Setenv(syncWorkerChildEnvVar, "1")
	_, err := WriteDaemonRuntime(dataDir, "127.0.0.1", 9, "test", false)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dataDir) })
	holdWriteOwnerLockForTest(t, dataDir) // daemon has NOT yielded the flock

	_, _, err = openWorkerWriteDB(cfg)
	require.Error(t, err,
		"worker must be refused while the daemon still holds the write lock")
	assert.ErrorContains(t, err, "write lock")
}

// A failed writer restoration must be retried, not logged and abandoned:
// giving up leaves the daemon running with every write endpoint failing
// until restart.
func TestRestoreArchiveAccessRetriesUntilSuccess(t *testing.T) {
	prevInitial := reacquireBackoffInitial
	reacquireBackoffInitial = time.Millisecond
	defer func() { reacquireBackoffInitial = prevInitial }()

	attempts := 0
	err := restoreArchiveAccess(
		context.Background(), "test restore", func() error {
			attempts++
			if attempts < 3 {
				return fmt.Errorf("attempt %d fails", attempts)
			}
			return nil
		},
	)
	require.NoError(t, err)
	assert.Equal(t, 3, attempts,
		"restoration must retry failures until it succeeds")
}

func TestRestoreArchiveAccessStopsOnContextCancel(t *testing.T) {
	prevInitial := reacquireBackoffInitial
	reacquireBackoffInitial = time.Hour // never elapses within the test
	defer func() { reacquireBackoffInitial = prevInitial }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := restoreArchiveAccess(ctx, "test restore", func() error {
		return fmt.Errorf("always fails")
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// Persistent recovery failure must not outlive the daemon: recovery ignores
// request cancellation but stops at daemon shutdown, so a held-forever lock
// cannot block exit while the pass holds the engine's exclusive sync lock.
func TestRunWorkerWritePassShutdownStopsPersistentRecovery(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	engine := sync.NewEngine(database, sync.EngineConfig{})
	defer engine.Close()

	prevInitial, prevMax := reacquireBackoffInitial, reacquireBackoffMax
	reacquireBackoffInitial = 5 * time.Millisecond
	reacquireBackoffMax = 20 * time.Millisecond
	defer func() {
		reacquireBackoffInitial, reacquireBackoffMax = prevInitial, prevMax
	}()

	daemonCtx, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	var contender *writeOwnerLock
	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, _ config.Config, _ string, _ func(workerLine),
	) (workerResult, error) {
		// A contender takes the freed lock and never releases it, then the
		// daemon shuts down while reacquisition is retrying.
		taken, err := tryAcquireWriteOwnerLock(cfg.DataDir)
		require.NoError(t, err, "contender takes the freed lock")
		contender = taken
		shutdown()
		return workerResult{Status: "ok", DiscoveryComplete: true}, nil
	})
	defer restore()
	defer func() {
		if contender != nil {
			assert.NoError(t, contender.Close())
		}
	}()

	done := make(chan error, 1)
	go func() {
		_, err := runWorkerWritePass(
			context.Background(), daemonCtx, cfg, engine, database, lock,
			"sync", nil,
		)
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err,
			"an unrecovered pass must surface its failure")
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("recovery kept retrying past daemon shutdown")
	}
}
