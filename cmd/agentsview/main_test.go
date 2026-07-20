package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	duckdbsync "go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/remotesync"
	"go.kenn.io/agentsview/internal/server"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

func TestRuntimeWarningHelper(t *testing.T) {
	logOutput := captureLogOutput(t)
	var visible bytes.Buffer

	reportRuntimeRecordWrite(
		&visible, errors.New("permission denied"),
		"keeping start lock as fallback",
		"To fix permissions, run: icacls <dir> /setowner <user>",
	)

	assert.Contains(t, visible.String(), "could not write daemon runtime record")
	assert.Contains(t, visible.String(), "icacls <dir> /setowner <user>")
	assert.Contains(t, logOutput.String(), "could not write daemon runtime record")
}

func TestServeRuntimeRecordWriteFailureWarnsVisibleAfterSlowStartup(t *testing.T) {
	out, err := runServeRuntimeWarningHelper(t, true, 1200*time.Millisecond)
	require.NoError(t, err, string(out))
	assert.Contains(t, string(out), "could not write daemon runtime record")
	assert.Contains(t, string(out), "icacls <dir> /setowner <user>")
}

func TestServeRuntimeRecordWriteSuccessDoesNotWarnVisible(t *testing.T) {
	out, err := runServeRuntimeWarningHelper(t, false, 0)
	require.NoError(t, err, string(out))
	assert.Contains(t, string(out), "runtime record write reached")
	assert.NotContains(t, string(out), "could not write daemon runtime record")
}

func TestPGServeRuntimeRecordWriteFailureWarnsVisible(t *testing.T) {
	out, err := runPGRuntimeWarningHelper(t)
	require.NoError(t, err, string(out))
	assert.Contains(t, string(out), "could not write daemon runtime record")
}

func TestDuckDBServeRuntimeRecordWriteFailureWarnsVisible(t *testing.T) {
	out, err := runDuckDBRuntimeWarningHelper(t)
	require.NoError(t, err, string(out))
	assert.Contains(t, string(out), "could not write daemon runtime record")
}

func runServeRuntimeWarningHelper(
	t *testing.T, failWrite bool, startupDelay time.Duration,
) ([]byte, error) {
	t.Helper()
	dataDir := t.TempDir()
	marker := "runtime record write reached"
	if failWrite {
		// Wait for the remedy, which is emitted after the warning, so the parent
		// cannot stop the helper between the two lines under test.
		marker = "icacls <dir> /setowner <user>"
	}
	return runRuntimeWarningHelperProcess(
		t, "serve", "TestRunServeRuntimeWarningHelperProcess",
		[]string{
			"AGENTSVIEW_RUN_SERVE_RUNTIME_WARNING_HELPER=1",
			"AGENTSVIEW_RUN_SERVE_RUNTIME_WARNING_FAIL=" + fmt.Sprint(failWrite),
			"AGENTSVIEW_RUN_SERVE_RUNTIME_WARNING_DELAY=" + startupDelay.String(),
			"AGENTSVIEW_DATA_DIR=" + dataDir,
		},
		marker,
	)
}

func runRuntimeWarningHelperProcess(
	t *testing.T, helperName, testName string, env []string, marker string,
) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+testName+"$")
	cmd.Env = append(os.Environ(), env...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("pipe %s helper stdout: %w", helperName, err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s helper: %w", helperName, err)
	}

	var stdoutOutput bytes.Buffer
	observed := false
	var stopErr error
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		stdoutOutput.WriteString(line)
		stdoutOutput.WriteByte('\n')
		if !observed && strings.Contains(line, marker) {
			observed = true
			stopErr = cmd.Cancel()
		}
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()

	combined := append([]byte(nil), stdoutOutput.Bytes()...)
	combined = append(combined, stderr.Bytes()...)
	if observed {
		if scanErr != nil {
			return combined, fmt.Errorf(
				"scan %s helper stdout after marker: %w\noutput:\n%s",
				helperName, scanErr, combined,
			)
		}
		if stopErr != nil {
			return combined, fmt.Errorf(
				"stop %s helper after stdout marker: %w\noutput:\n%s",
				helperName, stopErr, combined,
			)
		}
		if _, ok := waitErr.(*exec.ExitError); waitErr != nil && !ok {
			return combined, fmt.Errorf(
				"wait for stopped %s helper after stdout marker: %w\noutput:\n%s",
				helperName, waitErr, combined,
			)
		}
		return stdoutOutput.Bytes(), nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return combined, fmt.Errorf(
			"%s helper deadline expired before stdout marker %q: %w\noutput:\n%s",
			helperName, marker, ctx.Err(), combined,
		)
	}
	if scanErr != nil {
		return combined, fmt.Errorf(
			"scan %s helper stdout before marker %q: %w\noutput:\n%s",
			helperName, marker, scanErr, combined,
		)
	}
	if waitErr != nil {
		return combined, fmt.Errorf(
			"%s helper exited nonzero before stdout marker %q: %w\noutput:\n%s",
			helperName, marker, waitErr, combined,
		)
	}
	return combined, fmt.Errorf(
		"%s helper exited with status 0 before stdout marker %q\noutput:\n%s",
		helperName, marker, combined,
	)
}

func TestRunServeRuntimeWarningHelperProcess(t *testing.T) {
	if os.Getenv("AGENTSVIEW_RUN_SERVE_RUNTIME_WARNING_HELPER") != "1" {
		return
	}
	if os.Getenv("AGENTSVIEW_RUN_SERVE_RUNTIME_WARNING_FAIL") == "true" {
		writeDaemonRuntimeWithAuthAndNoSync = func(
			string, string, int, string, bool, bool, bool, ...int,
		) (string, error) {
			return "", errors.New("forced runtime-record write failure")
		}
	} else {
		original := writeDaemonRuntimeWithAuthAndNoSync
		writeDaemonRuntimeWithAuthAndNoSync = func(
			dataDir, host string, port int, version string, readOnly,
			requireAuth, noSync bool, caddyPID ...int,
		) (string, error) {
			path, err := original(
				dataDir, host, port, version, readOnly, requireAuth, noSync,
				caddyPID...,
			)
			fmt.Println("runtime record write reached")
			return path, err
		}
	}
	// This is only an orphan guard if the parent dies; normal completion is
	// driven by the parent observing the expected output on stdout.
	go func() {
		time.Sleep(2 * time.Minute)
		os.Exit(0)
	}()
	startupDelay, err := time.ParseDuration(
		os.Getenv("AGENTSVIEW_RUN_SERVE_RUNTIME_WARNING_DELAY"),
	)
	require.NoError(t, err)
	time.Sleep(startupDelay)
	runServe(config.Config{
		Host:    "127.0.0.1",
		Port:    0,
		DataDir: os.Getenv("AGENTSVIEW_DATA_DIR"),
		DBPath:  filepath.Join(os.Getenv("AGENTSVIEW_DATA_DIR"), "sessions.db"),
		NoSync:  true,
	}, serveOptions{})
}

func runDuckDBRuntimeWarningHelper(t *testing.T) ([]byte, error) {
	t.Helper()
	dataDir := t.TempDir()
	mirrorPath := filepath.Join(dataDir, "mirror.duckdb")
	buildEmptyDuckDBMirrorFixture(t, mirrorPath)
	return runRuntimeWarningHelperProcess(
		t, "DuckDB", "TestRunDuckDBRuntimeWarningHelperProcess",
		[]string{
			"AGENTSVIEW_RUN_DUCKDB_RUNTIME_WARNING_HELPER=1",
			"AGENTSVIEW_DATA_DIR=" + dataDir,
			"AGENTSVIEW_DUCKDB_RUNTIME_WARNING_PATH=" + mirrorPath,
		},
		"could not write daemon runtime record",
	)
}

// buildEmptyDuckDBMirrorFixture creates a schema-compatible, empty DuckDB
// mirror file at path. 'duckdb serve' now probes instead of migrating (see
// probeDuckDBMirrorForServe), so it fatally refuses to serve a missing or
// bare file; tests that just need serve to reach its normal startup path
// must seed a valid mirror first instead of relying on serve to create one.
func buildEmptyDuckDBMirrorFixture(t *testing.T, path string) {
	t.Helper()
	conn, err := duckdbsync.Open(path)
	require.NoError(t, err)
	require.NoError(t, duckdbsync.EnsureSchema(context.Background(), conn))
	require.NoError(t, conn.Close())
}

func runPGRuntimeWarningHelper(t *testing.T) ([]byte, error) {
	t.Helper()
	dataDir := t.TempDir()
	return runRuntimeWarningHelperProcess(
		t, "PostgreSQL", "TestRunPGRuntimeWarningHelperProcess",
		[]string{
			"AGENTSVIEW_RUN_PG_RUNTIME_WARNING_HELPER=1",
			"AGENTSVIEW_DATA_DIR=" + dataDir,
		},
		"could not write daemon runtime record",
	)
}

func TestRunPGRuntimeWarningHelperProcess(t *testing.T) {
	if os.Getenv("AGENTSVIEW_RUN_PG_RUNTIME_WARNING_HELPER") != "1" {
		return
	}
	writeDaemonRuntimeWithAuth = func(
		string, string, int, string, bool, bool, ...int,
	) (string, error) {
		return "", errors.New("forced runtime-record write failure")
	}
	database := dbtest.OpenTestDBAt(
		t, filepath.Join(os.Getenv("AGENTSVIEW_DATA_DIR"), "pg.db"),
	)
	ctx, cancel := context.WithCancel(context.Background())
	port := server.FindAvailablePort("127.0.0.1", 0)
	appCfg := config.Config{
		Host:    "127.0.0.1",
		Port:    port,
		DataDir: os.Getenv("AGENTSVIEW_DATA_DIR"),
	}
	preparePGServe = func(config.Config, string) (pgServeStartup, error) {
		return pgServeStartup{
			cfg: appCfg, ctx: ctx,
			rtOpts: serveRuntimeOptions{
				Mode: "pg-serve", RequestedPort: appCfg.Port,
			},
			srv: server.New(
				appCfg, database, nil,
				server.WithBaseContext(ctx),
			),
			cleanup: func() { cancel(); _ = database.Close() },
		}, nil
	}
	// This is only an orphan guard if the parent dies; normal completion is
	// driven by the parent observing the warning on stdout.
	go func() {
		time.Sleep(2 * time.Minute)
		os.Exit(0)
	}()
	runPGServe(appCfg, "")
}

func TestRunDuckDBRuntimeWarningHelperProcess(t *testing.T) {
	if os.Getenv("AGENTSVIEW_RUN_DUCKDB_RUNTIME_WARNING_HELPER") != "1" {
		return
	}
	writeDaemonRuntimeWithAuth = func(
		string, string, int, string, bool, bool, ...int,
	) (string, error) {
		return "", errors.New("forced runtime-record write failure")
	}
	// This is only an orphan guard if the parent dies; normal completion is
	// driven by the parent observing the warning on stdout.
	go func() {
		time.Sleep(2 * time.Minute)
		os.Exit(0)
	}()
	runDuckDBServe(config.Config{
		Host:    "127.0.0.1",
		Port:    0,
		DataDir: os.Getenv("AGENTSVIEW_DATA_DIR"),
		DuckDB: config.DuckDBConfig{
			Path: os.Getenv("AGENTSVIEW_DUCKDB_RUNTIME_WARNING_PATH"),
		},
	}, "")
}

func TestMustLoadConfig(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		wantHost      string
		wantPort      int
		wantPublicURL string
		wantProxyMode string
	}{
		{
			name:          "DefaultArgs",
			args:          []string{},
			wantHost:      "127.0.0.1",
			wantPort:      8080,
			wantPublicURL: "",
			wantProxyMode: "",
		},
		{
			name:          "ExplicitFlags",
			args:          []string{"--host", "0.0.0.0", "--port", "9090", "--public-url", "https://viewer.example.test", "--proxy", "caddy", "--proxy-bind-host", "10.0.60.2", "--public-port", "9443", "--no-browser"},
			wantHost:      "0.0.0.0",
			wantPort:      9090,
			wantPublicURL: "https://viewer.example.test:9443",
			wantProxyMode: "caddy",
		},
		{
			name:          "PartialFlags",
			args:          []string{"--port", "3000"},
			wantHost:      "127.0.0.1",
			wantPort:      3000,
			wantPublicURL: "",
			wantProxyMode: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testDataDir(t)
			cmd := newServeCommand()
			require.NoError(t, cmd.Flags().Parse(tt.args), "Parse")
			cfg := mustLoadConfig(cmd)

			assert.Equal(t, tt.wantHost, cfg.Host)
			assert.Equal(t, tt.wantPort, cfg.Port)
			assert.Equal(t, tt.wantPublicURL, cfg.PublicURL)
			assert.Equal(t, tt.wantProxyMode, cfg.Proxy.Mode)

			assert.NotEmpty(t, cfg.DataDir, "DataDir should be set")
			wantDBPath := filepath.Join(cfg.DataDir, "sessions.db")
			assert.Equal(t, wantDBPath, cfg.DBPath)
		})
	}
}

func TestPrepareServeRuntimeConfigPortZeroUsesAssignedPort(t *testing.T) {
	cfg := config.Config{
		Host: "127.0.0.1",
		Port: 0,
	}

	var err error
	out := captureStdout(t, func() {
		cfg, err = prepareServeRuntimeConfig(
			cfg,
			serveRuntimeOptions{
				Mode:          "serve",
				RequestedPort: 0,
			},
		)
	})
	require.NoError(t, err, "prepareServeRuntimeConfig")
	assert.NotZero(t, cfg.Port, "Port remained literal 0")
	assert.NotContains(t, out, "Port 0 in use",
		"unexpected literal port 0 fallback message")
	assert.Contains(t, out, "Using available port",
		"missing ephemeral port message")
}

func TestSetupLogFile(t *testing.T) {
	dir := t.TempDir()
	// Register after TempDir so LIFO cleanup closes the log file before
	// TempDir removes the directory. On Windows, open files can't be deleted.
	restoreTestLogOutput(t)

	setupLogFile(dir)

	// Log something and verify it reaches the file.
	log.Print("test-log-message")

	logPath := filepath.Join(dir, "debug.log")
	data, err := os.ReadFile(logPath)
	require.NoError(t, err, "reading log file")
	assert.Contains(t, string(data), "test-log-message",
		"log file missing message")
}

func TestSetupLogFileOpenFailure(t *testing.T) {
	// Capture log output to verify warning is emitted.
	buf := captureLogOutput(t)

	// Pass a path that can't be opened (dir doesn't exist
	// and we use a file as the "dir").
	tmpFile := filepath.Join(t.TempDir(), "notadir")
	writeTestFile(t, tmpFile, []byte("x"))

	setupLogFile(tmpFile)

	assert.Contains(t, buf.String(), "cannot open log file",
		"expected warning about log file")
}

func TestTruncateLogFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	// Write a file larger than the limit.
	big := bytes.Repeat([]byte("x"), 1024)
	writeTestFile(t, path, big)

	// Truncate with limit smaller than file size.
	truncateLogFile(path, 512)

	info, err := os.Stat(path)
	require.NoError(t, err, "stat after truncate")
	assert.Equal(t, int64(0), info.Size())
}

func TestTruncateLogFileUnderLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	content := []byte("small log content")
	writeTestFile(t, path, content)

	// File is under limit: should not be truncated.
	truncateLogFile(path, 1024)

	data, err := os.ReadFile(path)
	require.NoError(t, err, "read after truncate")
	assert.Equal(t, string(content), string(data), "content changed")
}

func TestTruncateLogFileMissing(t *testing.T) {
	// Non-existent file: should not panic.
	missing := filepath.Join(t.TempDir(), "missing", "log.txt")
	truncateLogFile(missing, 1024)
}

func TestTruncateLogFileSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.log")
	link := filepath.Join(dir, "link.log")

	// Write a target file larger than the limit.
	big := bytes.Repeat([]byte("x"), 1024)
	writeTestFile(t, target, big)
	requireSymlinkOrSkip(t, target, link)

	// Truncate via symlink: should be a no-op.
	truncateLogFile(link, 512)

	data, err := os.ReadFile(target)
	require.NoError(t, err, "read target")
	assert.Len(t, data, 1024, "symlink target was truncated")
}

func TestNewDaemonIdleTrackerUsesConfigTimeout(t *testing.T) {
	t.Setenv(backgroundChildEnvVar, "1")
	fired := make(chan struct{})
	tracker := newDaemonIdleTracker(config.Config{DaemonIdleTimeout: 20 * time.Millisecond}, func() { close(fired) })
	require.NotNil(t, tracker)
	ctx := t.Context()
	go tracker.Run(ctx)
	select {
	case <-fired:
	case <-time.After(time.Second):
		require.FailNow(t, "idle tracker did not fire")
	}
}

func TestNewDaemonIdleTrackerConfigZeroDisables(t *testing.T) {
	t.Setenv(backgroundChildEnvVar, "1")
	tracker := newDaemonIdleTracker(config.Config{DaemonIdleTimeout: 0}, func() { require.FailNow(t, "idle tracker fired") })
	assert.Nil(t, tracker)
}

func TestNewDaemonIdleTrackerEnvOverridesConfig(t *testing.T) {
	t.Setenv(backgroundChildEnvVar, "1")
	t.Setenv("AGENTSVIEW_DAEMON_IDLE_TIMEOUT", "0")
	tracker := newDaemonIdleTracker(config.Config{DaemonIdleTimeout: 20 * time.Minute}, func() { require.FailNow(t, "idle tracker fired") })
	assert.Nil(t, tracker)
}

type fakeUnwatchedPollSyncer struct {
	calls     int
	callRoots [][]string
	callFull  []bool
}

func (f *fakeUnwatchedPollSyncer) ReconcileWatchRoots(
	_ context.Context, roots []string, full bool,
) error {
	f.calls++
	f.callRoots = append(f.callRoots, append([]string(nil), roots...))
	f.callFull = append(f.callFull, full)
	return nil
}

func TestPollUnwatchedRootsOnceUsesScopedAuthoritativeReconciliation(t *testing.T) {
	fake := &fakeUnwatchedPollSyncer{}
	roots := []string{"/tmp/claude", "/tmp/codex"}

	pollUnwatchedRootsOnce(t.Context(), fake, roots)
	pollUnwatchedRootsOnce(t.Context(), fake, roots)

	require.Equal(t, 2, fake.calls)
	assert.Equal(t, roots, fake.callRoots[0])
	assert.False(t, fake.callFull[0])
	assert.Equal(t, roots, fake.callRoots[1])
	assert.False(t, fake.callFull[1])
}

func TestCollectWatchRootsPreservesDirsSharingWatchRoot(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "codex-state")
	require.NoError(t, os.Mkdir(parent, 0o755), "mkdir parent")

	sessionsDir := filepath.Join(parent, "sessions")
	archivedDir := filepath.Join(parent, "archived_sessions")
	cfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {sessionsDir, archivedDir},
		},
	}

	roots, unwatchedDirs := collectWatchRoots(cfg)

	assert.ElementsMatch(t, []string{sessionsDir, archivedDir}, unwatchedDirs,
		"missing roots retain polling until native activation completes")
	shared, ok := findCollectedWatchRoot(roots, parent)
	require.True(t, ok, "shared watch root should be represented once")
	assert.False(t, shared.recursive, "provider parent root is explicitly shallow")
	assert.True(t, shared.exists)
	assert.ElementsMatch(t, []watchScope{
		{agent: parser.AgentCodex, syncDir: sessionsDir},
		{agent: parser.AgentCodex, syncDir: archivedDir},
	}, shared.scopes)
	assert.ElementsMatch(t, []agentsync.WatchScope{
		{Agent: string(parser.AgentCodex), SyncDir: sessionsDir},
		{Agent: string(parser.AgentCodex), SyncDir: archivedDir},
	}, shared.registeredRoot().Scopes,
		"watcher registration must retain every configured polling scope")

	sessions, ok := findCollectedWatchRoot(roots, sessionsDir)
	require.True(t, ok, "missing sessions root remains in the logical plan")
	assert.True(t, sessions.recursive)
	assert.False(t, sessions.exists)
	assert.Equal(t, []watchScope{{agent: parser.AgentCodex, syncDir: sessionsDir}}, sessions.scopes)
	assert.Equal(t, []string{sessionsDir}, sessions.pendingPollingDirs)
	assert.Empty(t, sessions.persistentPollingDirs)

	archived, ok := findCollectedWatchRoot(roots, archivedDir)
	require.True(t, ok, "missing archive root remains in the logical plan")
	assert.True(t, archived.recursive)
	assert.False(t, archived.exists)
	assert.Equal(t, []watchScope{{agent: parser.AgentCodex, syncDir: archivedDir}}, archived.scopes)
	assert.Equal(t, []string{archivedDir}, archived.pendingPollingDirs)
	assert.Empty(t, archived.persistentPollingDirs)
}

func TestCollectWatchRootsPollsRecursiveSymlinkProviderRoot(t *testing.T) {
	root := t.TempDir()
	targetVSRoot := filepath.Join(t.TempDir(), "vs-target")
	sessionsRoot := filepath.Join(
		targetVSRoot, "SampleApp", "copilot-chat", "thread", "sessions",
	)
	require.NoError(t, os.MkdirAll(sessionsRoot, 0o755))
	requireSymlinkOrSkip(t, targetVSRoot, filepath.Join(root, ".VS"))
	cfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {root},
		},
	}

	roots, unwatchedDirs := collectWatchRoots(cfg)

	require.Len(t, roots, 2)
	assert.Equal(t, root, roots[0].path)
	assert.False(t, roots[0].recursive)
	assert.True(t, roots[0].exists)
	assert.Equal(t, []watchScope{{agent: parser.AgentVSCopilot, syncDir: root}}, roots[0].scopes)
	assert.Equal(
		t,
		filepath.Join(root, ".VS", "SampleApp", "copilot-chat", "thread", "sessions"),
		roots[1].path,
	)
	assert.False(t, roots[1].recursive)
	assert.True(t, roots[1].exists)
	assert.Equal(t, []watchScope{{agent: parser.AgentVSCopilot, syncDir: root}}, roots[1].scopes)
	assert.ElementsMatch(t, []string{root}, unwatchedDirs)
}

// fakeEmitter records Emit calls; safe for concurrent use.
type fakeEmitter struct {
	count atomic.Int64
}

func (f *fakeEmitter) Emit(_ string) { f.count.Add(1) }

func TestStartRemoteHostSync_EmitsAfterSuccess(t *testing.T) {
	em := &fakeEmitter{}
	syncFn := func() (int, error) { return 3, nil }

	done := make(chan struct{})
	exited := make(chan struct{})
	interval := 10 * time.Millisecond
	go func() {
		runRemoteHostSyncLoop(context.Background(), "test-host", interval, syncFn, em, nil, done)
		close(exited)
	}()

	time.Sleep(3 * interval)
	close(done)
	<-exited

	assert.Positive(t, em.count.Load(), "emitter should have been called at least once")
}

func TestRemoteHostSyncFuncSerializesWithEngineExclusiveLock(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	engine := agentsync.NewEngine(database, agentsync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{},
		Machine:   "local",
	})

	remoteEntered := make(chan struct{})
	releaseRemote := make(chan struct{})
	syncFn := remoteHostSyncFunc(
		context.Background(),
		config.Config{},
		database,
		engine,
		config.RemoteHost{Host: "test-host"},
		func(
			context.Context, config.Config, *db.DB, config.RemoteHost, bool,
		) (remotesync.SyncStats, error) {
			close(remoteEntered)
			<-releaseRemote
			return remotesync.SyncStats{}, nil
		},
	)

	syncErr := make(chan error, 1)
	go func() {
		_, err := syncFn()
		syncErr <- err
	}()

	select {
	case <-remoteEntered:
	case <-time.After(time.Second):
		require.FailNow(t, "remote sync did not enter")
	}

	exclusiveEntered := make(chan struct{})
	exclusiveErr := make(chan error, 1)
	go func() {
		exclusiveErr <- engine.RunExclusive(func() error {
			close(exclusiveEntered)
			return nil
		})
	}()

	select {
	case <-exclusiveEntered:
		assert.Fail(t, "exclusive operation overlapped scheduled remote sync")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseRemote)

	select {
	case err := <-syncErr:
		require.NoError(t, err)
	case <-time.After(time.Second):
		require.FailNow(t, "scheduled remote sync did not finish")
	}
	select {
	case err := <-exclusiveErr:
		require.NoError(t, err)
	case <-time.After(time.Second):
		require.FailNow(t, "exclusive operation did not finish")
	}
}

type scheduledLockOrderRunner struct {
	held bool
}

func (r *scheduledLockOrderRunner) RunExclusive(work func() error) error {
	if r.held {
		return errors.New("engine lock acquired recursively")
	}
	r.held = true
	defer func() { r.held = false }()
	return work()
}

type scheduledLockOrderCleanup struct {
	runner  remoteSyncExclusiveRunner
	retries int
}

func (c *scheduledLockOrderCleanup) Error() string { return "pending cleanup" }

func (c *scheduledLockOrderCleanup) RetryCleanup() error {
	c.retries++
	if c.retries == 1 {
		return errors.New("retain pending cleanup")
	}
	return c.runner.RunExclusive(func() error { return nil })
}

func TestRemoteHostSyncFuncAcquiresHTTPCleanupBeforeEngine(t *testing.T) {
	originalRegistry := httpRemoteCleanupRegistry
	httpRemoteCleanupRegistry = new(remotesync.CleanupRegistry)
	t.Cleanup(func() { httpRemoteCleanupRegistry = originalRegistry })
	runner := &scheduledLockOrderRunner{}
	owner := &scheduledLockOrderCleanup{runner: runner}
	_, seedErr := httpRemoteCleanupRegistry.Run(
		func() (remotesync.SyncStats, error) {
			return remotesync.SyncStats{}, owner
		},
	)
	require.Error(t, seedErr)
	originalHTTP := runHTTPRemoteSync
	httpCalls := 0
	runHTTPRemoteSync = func(
		context.Context, config.Config, *db.DB, config.RemoteHost, bool,
	) (remotesync.SyncStats, error) {
		httpCalls++
		return remotesync.SyncStats{SessionsSynced: 1}, nil
	}
	t.Cleanup(func() { runHTTPRemoteSync = originalHTTP })
	database := dbtest.OpenTestDB(t)
	syncFn := remoteHostSyncFunc(
		context.Background(), config.Config{}, database, runner,
		config.RemoteHost{Host: "http-host", Transport: config.RemoteTransportHTTP},
		func(
			ctx context.Context, cfg config.Config, database *db.DB,
			rh config.RemoteHost, full bool,
		) (remotesync.SyncStats, error) {
			return runRemoteSyncTransportWithCleanup(
				ctx, cfg, database, rh, full, false,
			)
		},
	)

	synced, err := syncFn()

	require.NoError(t, err)
	assert.Equal(t, 1, synced)
	assert.Equal(t, 1, httpCalls)
	assert.Equal(t, 2, owner.retries,
		"retained cleanup must acquire the engine before scheduled work")
}

func TestRemoteHostSyncFuncUsesCallerContext(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	engine := agentsync.NewEngine(database, agentsync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{},
		Machine:   "local",
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	syncFn := remoteHostSyncFunc(
		ctx,
		config.Config{},
		database,
		engine,
		config.RemoteHost{Host: "test-host"},
		func(
			runCtx context.Context, _ config.Config, _ *db.DB,
			_ config.RemoteHost, _ bool,
		) (remotesync.SyncStats, error) {
			return remotesync.SyncStats{}, runCtx.Err()
		},
	)

	_, err := syncFn()

	require.ErrorIs(t, err, context.Canceled)
}

func TestRemoteHostSyncFuncDispatchesHTTPTransport(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	engine := agentsync.NewEngine(database, agentsync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{},
		Machine:   "local",
	})
	var called config.RemoteHost
	restore := stubHTTPRemoteSyncForTest(t, func(
		_ context.Context,
		rh config.RemoteHost,
		full bool,
	) (remotesync.SyncStats, error) {
		called = rh
		assert.False(t, full)
		return remotesync.SyncStats{SessionsSynced: 1}, nil
	})
	defer restore()
	syncFn := remoteHostSyncFunc(
		context.Background(),
		config.Config{},
		database,
		engine,
		config.RemoteHost{
			Host:      "test-host",
			Transport: config.RemoteTransportHTTP,
			URL:       "https://test-host.example.test",
		},
		func(
			ctx context.Context, cfg config.Config, database *db.DB,
			rh config.RemoteHost, full bool,
		) (remotesync.SyncStats, error) {
			return runRemoteSyncTransportWithCleanup(
				ctx, cfg, database, rh, full, false,
			)
		},
	)

	synced, err := syncFn()

	require.NoError(t, err)
	assert.Equal(t, 1, synced)
	assert.Equal(t, "https://test-host.example.test", called.URL)
}

func TestRemoteHostSyncFuncForcesFullWhenDatabaseNeedsResync(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	raw, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = raw.Exec("PRAGMA user_version = 0")
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	database, err = db.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	require.True(t, database.NeedsResync())
	engine := agentsync.NewEngine(database, agentsync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{},
		Machine:   "local",
	})

	var gotFull bool
	syncFn := remoteHostSyncFunc(
		context.Background(),
		config.Config{},
		database,
		engine,
		config.RemoteHost{Host: "test-host"},
		func(
			_ context.Context, _ config.Config, _ *db.DB,
			_ config.RemoteHost, full bool,
		) (remotesync.SyncStats, error) {
			gotFull = full
			return remotesync.SyncStats{}, nil
		},
	)

	_, err = syncFn()

	require.NoError(t, err)
	assert.True(t, gotFull, "scheduled remote sync should force full when DB needs resync")
}

func TestStartRemoteHostSync_TracksRemoteWorkForIdleReaper(t *testing.T) {
	idleFired := make(chan struct{})
	idleTracker := server.NewIdleTracker(20*time.Millisecond, func() {
		close(idleFired)
	})
	ctx := t.Context()

	syncEntered := make(chan struct{}, 1)
	releaseSync := make(chan struct{})
	syncFn := func() (int, error) {
		select {
		case syncEntered <- struct{}{}:
		default:
		}
		<-releaseSync
		return 1, nil
	}

	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		runRemoteHostSyncLoop(ctx, "test-host", time.Millisecond, syncFn, nil, idleTracker, done)
		close(exited)
	}()

	select {
	case <-syncEntered:
	case <-time.After(time.Second):
		require.FailNow(t, "remote sync did not enter")
	}
	go idleTracker.Run(ctx)

	select {
	case <-idleFired:
		require.FailNow(t, "idle tracker fired while remote sync was active")
	case <-time.After(80 * time.Millisecond):
	}

	close(releaseSync)
	close(done)
	select {
	case <-exited:
	case <-time.After(time.Second):
		require.FailNow(t, "remote sync loop did not exit")
	}
}

func TestStartRemoteHostSync_ExitsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	syncCalled := make(chan struct{}, 1)
	go func() {
		runRemoteHostSyncLoop(
			ctx,
			"test-host",
			time.Hour,
			func() (int, error) {
				syncCalled <- struct{}{}
				return 0, nil
			},
			nil,
			nil,
			nil,
		)
		close(exited)
	}()

	cancel()

	select {
	case <-exited:
	case <-time.After(time.Second):
		require.FailNow(t, "remote sync loop did not exit after context cancel")
	}
	select {
	case <-syncCalled:
		require.FailNow(t, "sync ran before context cancel")
	default:
	}
}

type scopedEmitter struct {
	scopes chan string
}

func (e *scopedEmitter) Emit(scope string) {
	select {
	case e.scopes <- scope:
	default:
	}
}

func TestStartRemoteHostSync_EmitsSessionsScopeAfterSuccess(t *testing.T) {
	em := &scopedEmitter{scopes: make(chan string, 1)}
	syncFn := func() (int, error) { return 3, nil }

	done := make(chan struct{})
	exited := make(chan struct{})
	interval := 10 * time.Millisecond
	go func() {
		runRemoteHostSyncLoop(context.Background(), "test-host", interval, syncFn, em, nil, done)
		close(exited)
	}()

	select {
	case scope := <-em.scopes:
		assert.Equal(t, "sessions", scope)
	case <-time.After(3 * interval):
		require.FailNow(t, "timed out waiting for remote sync event")
	}
	close(done)
	<-exited
}

func TestStartRemoteHostSync_NoEmitOnZeroSynced(t *testing.T) {
	em := &fakeEmitter{}
	syncFn := func() (int, error) { return 0, nil }

	done := make(chan struct{})
	exited := make(chan struct{})
	interval := 10 * time.Millisecond
	go func() {
		runRemoteHostSyncLoop(context.Background(), "test-host", interval, syncFn, em, nil, done)
		close(exited)
	}()

	time.Sleep(3 * interval)
	close(done)
	<-exited

	assert.Zero(t, em.count.Load(), "emitter should not fire when no sessions synced")
}

func TestStartRemoteHostSync_NoEmitOnError(t *testing.T) {
	em := &fakeEmitter{}
	syncFn := func() (int, error) { return 0, errors.New("ssh failure") }

	done := make(chan struct{})
	exited := make(chan struct{})
	interval := 10 * time.Millisecond
	go func() {
		runRemoteHostSyncLoop(context.Background(), "test-host", interval, syncFn, em, nil, done)
		close(exited)
	}()

	time.Sleep(3 * interval)
	close(done)
	<-exited

	assert.Zero(t, em.count.Load(), "emitter should not fire when sync fails")
}

func TestStartRemoteHostSync_NilEmitterSafe(t *testing.T) {
	syncFn := func() (int, error) { return 1, nil }

	done := make(chan struct{})
	exited := make(chan struct{})
	interval := 10 * time.Millisecond
	go func() {
		runRemoteHostSyncLoop(context.Background(), "test-host", interval, syncFn, nil, nil, done)
		close(exited)
	}()

	time.Sleep(2 * interval)
	close(done)
	<-exited
}

func TestCollectWatchRootsHermesSessionsWatchesStateDBParent(t *testing.T) {
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.Mkdir(sessionsDir, 0o755), "mkdir sessions")

	cfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {sessionsDir},
		},
	}

	roots, unwatchedDirs := collectWatchRoots(cfg)

	require.Empty(t, unwatchedDirs, "unwatched dirs before watcher setup")
	require.Len(t, roots, 2)
	assert.Equal(t, root, roots[0].path)
	assert.False(t, roots[0].recursive)
	assert.True(t, roots[0].exists)
	assert.Equal(t, []watchScope{{agent: parser.AgentHermes, syncDir: sessionsDir}}, roots[0].scopes)
	assert.Equal(t, sessionsDir, roots[1].path)
	assert.True(t, roots[1].recursive)
	assert.True(t, roots[1].exists)
	assert.Equal(t, []watchScope{{agent: parser.AgentHermes, syncDir: sessionsDir}}, roots[1].scopes)
}

func TestCollectWatchRootsUsesCoworkProviderRecursiveRoot(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCowork: {root},
		},
	}

	roots, unwatchedDirs := collectWatchRoots(cfg)

	require.Empty(t, unwatchedDirs, "cowork root should be watched directly")
	got, ok := findCollectedWatchRoot(roots, root)
	require.True(t, ok, "cowork provider WatchPlan root not collected")
	assert.True(t, got.recursive,
		"cowork provider recursive WatchPlan must override legacy ShallowWatch")
	assert.True(t, got.exists)
	assert.Equal(t, []watchScope{{agent: parser.AgentCowork, syncDir: root}}, got.scopes)
}

func TestCollectWatchRootsUsesGeminiProviderMetadataRoot(t *testing.T) {
	root := t.TempDir()
	tmpRoot := filepath.Join(root, "tmp")
	require.NoError(t, os.Mkdir(tmpRoot, 0o755), "mkdir tmp")
	cfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentGemini: {root},
		},
	}

	roots, unwatchedDirs := collectWatchRoots(cfg)

	require.Empty(t, unwatchedDirs, "all gemini provider roots exist")
	metadataRoot, ok := findCollectedWatchRoot(roots, root)
	require.True(t, ok, "gemini provider metadata root not collected")
	assert.False(t, metadataRoot.recursive)
	assert.True(t, metadataRoot.exists)
	tmp, ok := findCollectedWatchRoot(roots, tmpRoot)
	require.True(t, ok, "gemini provider recursive tmp root not collected")
	assert.True(t, tmp.recursive)
	assert.True(t, tmp.exists)
}

func TestCollectWatchRootsUsesAntigravityCLIHistoryRoot(t *testing.T) {
	root := t.TempDir()
	for _, subdir := range []string{"brain", "conversations", "implicit"} {
		require.NoError(t, os.Mkdir(filepath.Join(root, subdir), 0o755))
	}
	cfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentAntigravityCLI: {root},
		},
	}

	roots, unwatchedDirs := collectWatchRoots(cfg)

	require.Empty(t, unwatchedDirs, "all antigravity cli provider roots exist")
	historyRoot, ok := findCollectedWatchRoot(roots, root)
	require.True(t, ok, "antigravity cli history.jsonl root not collected")
	assert.False(t, historyRoot.recursive)
	conversations, ok := findCollectedWatchRoot(
		roots, filepath.Join(root, "conversations"),
	)
	require.True(t, ok, "antigravity cli conversations root not collected")
	assert.False(t, conversations.recursive)
	brain, ok := findCollectedWatchRoot(roots, filepath.Join(root, "brain"))
	require.True(t, ok, "antigravity cli brain root not collected")
	assert.True(t, brain.recursive)
}

func TestCollectWatchRootsIncludesDevinProviderRootsForNonFileAgent(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "cli", "transcripts"), 0o755))
	writeTestFile(t, filepath.Join(root, "cli", "sessions.db"), []byte("sqlite"))

	cfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentDevin: {root},
		},
	}

	roots, unwatchedDirs := collectWatchRoots(cfg)

	require.Empty(t, unwatchedDirs)
	cliRoot, ok := findCollectedWatchRoot(roots, filepath.Join(root, "cli"))
	require.True(t, ok, "devin cli root not collected")
	assert.False(t, cliRoot.recursive)
	assert.True(t, cliRoot.exists)
	assert.Equal(t, []watchScope{{agent: parser.AgentDevin, syncDir: root}}, cliRoot.scopes)
	transcriptsRoot, ok := findCollectedWatchRoot(roots, filepath.Join(root, "cli", "transcripts"))
	require.True(t, ok, "devin transcripts root not collected")
	assert.False(t, transcriptsRoot.recursive)
	assert.True(t, transcriptsRoot.exists)
	assert.Equal(t, []watchScope{{agent: parser.AgentDevin, syncDir: root}}, transcriptsRoot.scopes)
}

func TestCollectWatchRootsTracksExactAgentsForSharedRoot(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentClaude: {root},
		parser.AgentCodex:  {root},
	}}

	roots, unwatchedDirs := collectWatchRoots(cfg)

	require.Empty(t, unwatchedDirs)
	shared, ok := findCollectedWatchRoot(roots, root)
	require.True(t, ok)
	assert.True(t, shared.recursive,
		"a recursive owner must preserve recursive physical coverage")
	assert.ElementsMatch(t, []watchScope{
		{agent: parser.AgentClaude, syncDir: root},
		{agent: parser.AgentCodex, syncDir: root},
	}, shared.scopes)
	assert.ElementsMatch(t, []agentsync.WatchScope{
		{Agent: string(parser.AgentClaude), SyncDir: root},
		{Agent: string(parser.AgentCodex), SyncDir: root},
	}, shared.registeredRoot().Scopes,
		"watcher registration must retain every rename-prefix owner")
}

func TestCollectWatchRootsPreservesMissingProviderRoots(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentDevin: {root},
		},
	}

	roots, unwatchedDirs := collectWatchRoots(cfg)

	require.Len(t, roots, 2)
	cliRoot, ok := findCollectedWatchRoot(roots, filepath.Join(root, "cli"))
	require.True(t, ok)
	assert.False(t, cliRoot.recursive)
	assert.False(t, cliRoot.exists)
	assert.Equal(t, []watchScope{{agent: parser.AgentDevin, syncDir: root}}, cliRoot.scopes)
	transcriptsRoot, ok := findCollectedWatchRoot(roots, filepath.Join(root, "cli", "transcripts"))
	require.True(t, ok)
	assert.False(t, transcriptsRoot.recursive)
	assert.False(t, transcriptsRoot.exists)
	assert.Equal(t, []watchScope{{agent: parser.AgentDevin, syncDir: root}}, transcriptsRoot.scopes)
	assert.Equal(t, []string{root}, unwatchedDirs)
}

func TestPathCoveredByAnyWatchRootCreationDoesNotTreatShallowAncestorAsRecursive(t *testing.T) {
	root := filepath.Clean(filepath.Join(t.TempDir(), "state"))
	shallowRoots := []watchRoot{{path: root, recursive: false, exists: true}}
	recursiveRoots := []watchRoot{{path: root, recursive: true, exists: true}}

	assert.True(t,
		pathCoveredByAnyWatchRootCreation(filepath.Join(root, "sessions"), shallowRoots),
		"shallow roots can observe immediate child creation")
	assert.False(t,
		pathCoveredByAnyWatchRootCreation(filepath.Join(root, "nested", "sessions"), shallowRoots),
		"shallow ancestors must not be treated like recursive watches")
	assert.True(t,
		pathCoveredByAnyWatchRootCreation(filepath.Join(root, "nested", "sessions"), recursiveRoots),
		"recursive roots cover nested missing roots")
}

func TestStartFileWatcherSuppressesPendingPollingWhenLifecycleIsOwned(t *testing.T) {
	root := t.TempDir()
	pending := watchRoot{
		path:               filepath.Join(root, "state", "sessions"),
		recursive:          true,
		scopes:             []watchScope{{agent: parser.AgentDevin, syncDir: root}},
		pendingPollingDirs: []string{root},
	}

	got := accountRegisteredWatchRoots(
		[]string{root}, []watchRoot{pending},
		[]agentsync.RecursiveWatchResult{{
			Watched:                   1,
			MissingRootLifecycleOwned: true,
		}},
	)

	assert.Empty(t, got,
		"native lifecycle ownership replaces the startup polling obligation")
	assert.Empty(t, watchPollingObligations(
		[]watchRoot{pending},
		[]agentsync.RecursiveWatchResult{{
			Watched:                   1,
			MissingRootLifecycleOwned: true,
		}},
		got,
	), "owned missing roots must not schedule authoritative polling")
}

func TestStartupReconciliationHandlerCheckpointsBeforeOpeningDispatch(t *testing.T) {
	ctx := t.Context()
	order := make([]string, 0, 2)
	handler := newStartupReconciliationHandler(
		ctx,
		func(got context.Context) error {
			assert.Equal(t, ctx, got)
			order = append(order, "checkpoint")
			return nil
		},
		func() { order = append(order, "open") },
	)

	handler(agentsync.SyncStats{}, nil)
	assert.Equal(t, []string{"checkpoint", "open"}, order)
}

func TestInitialSyncWatcherStartupOwnerReconciles(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	reconciled := make(chan agentsync.SyncStats, 1)
	engine := agentsync.NewEngine(database, agentsync.EngineConfig{
		OnStartupReconciled: func(stats agentsync.SyncStats, err error) {
			require.NoError(t, err)
			reconciled <- stats
		},
	})
	t.Cleanup(engine.Close)

	stats := runInitialSync(t.Context(), engine, nil)

	assert.False(t, stats.Aborted)
	select {
	case got := <-reconciled:
		assert.Equal(t, stats, got)
	case <-time.After(time.Second):
		require.FailNow(t, "initial sync did not reconcile watcher startup")
	}
}

func TestInitialResyncWatcherStartupOwnerReportsFirstIncompleteAttempt(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	missingPath := filepath.Join(t.TempDir(), "missing.jsonl")
	dbtest.SeedSession(t, database, "existing", "project", func(s *db.Session) {
		s.FilePath = &missingPath
	})
	type startupResult struct {
		stats agentsync.SyncStats
		err   error
	}
	reconciled := make(chan startupResult, 1)
	engine := agentsync.NewEngine(database, agentsync.EngineConfig{
		OnStartupReconciled: func(stats agentsync.SyncStats, err error) {
			reconciled <- startupResult{stats: stats, err: err}
		},
	})
	t.Cleanup(engine.Close)

	covered, stats := runInitialResync(t.Context(), engine, nil)

	assert.False(t, covered)
	assert.False(t, stats.Aborted, "incremental fallback is the successful owner")
	select {
	case got := <-reconciled:
		assert.True(t, got.stats.Aborted,
			"dispatch reports the first incomplete attempt without waiting for fallback")
		assert.ErrorContains(t, got.err, "startup discovery incomplete")
	case <-time.After(time.Second):
		require.FailNow(t, "incomplete startup attempt was not reported")
	}
}

func TestStartupReconciliationHandlerKeepsDispatchClosedAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	checkpointed := false
	opened := false
	handler := newStartupReconciliationHandler(
		ctx,
		func(context.Context) error { checkpointed = true; return nil },
		func() { opened = true },
	)

	handler(agentsync.SyncStats{}, nil)
	assert.False(t, checkpointed)
	assert.False(t, opened)
}

func TestStartupReconciliationHandlerCheckpointErrorStillOpensDispatch(t *testing.T) {
	opened := false
	handler := newStartupReconciliationHandler(
		t.Context(),
		func(context.Context) error { return errors.New("checkpoint unavailable") },
		func() { opened = true },
	)

	handler(agentsync.SyncStats{}, nil)
	assert.True(t, opened,
		"committed reconciliation remains valid after a best-effort checkpoint error")
}

func TestStartupReconciliationHandlerPartialDiscoveryStillOpensDispatch(t *testing.T) {
	checkpointed := false
	opened := false
	handler := newStartupReconciliationHandler(
		t.Context(),
		func(context.Context) error { checkpointed = true; return nil },
		func() { opened = true },
	)

	handler(agentsync.SyncStats{}, errors.New("one provider unavailable"))

	assert.False(t, checkpointed,
		"an incomplete startup pass must not checkpoint as authoritative")
	assert.True(t, opened,
		"a failed provider must not strand native watcher dispatch")
}

func TestStartupReconciliationHandlerLogsBusyCheckpointBeforeOpeningDispatch(
	t *testing.T,
) {
	logs := captureLogOutput(t)
	opened := false
	loggedBeforeOpen := false
	handler := newStartupReconciliationHandler(
		t.Context(),
		func(context.Context) error { return db.ErrWALCheckpointBusy },
		func() {
			loggedBeforeOpen = strings.Contains(
				logs.String(), db.ErrWALCheckpointBusy.Error(),
			)
			opened = true
		},
	)

	handler(agentsync.SyncStats{}, nil)

	assert.Contains(t, logs.String(), "post-sync wal checkpoint")
	assert.Contains(t, logs.String(), db.ErrWALCheckpointBusy.Error())
	assert.True(t, loggedBeforeOpen, "busy checkpoint must be logged before dispatch")
	assert.True(t, opened,
		"busy checkpoint is logged but committed reconciliation still opens dispatch")
}

func TestStartFileWatcherKeepsPollingWhenAnyPendingRootLacksCoverage(t *testing.T) {
	root := t.TempDir()
	roots := []watchRoot{
		{path: filepath.Join(root, "state", "sessions"), recursive: true, scopes: []watchScope{{agent: parser.AgentDevin, syncDir: root}}},
		{path: filepath.Join(root, "state", "archive"), recursive: true, scopes: []watchScope{{agent: parser.AgentDevin, syncDir: root}}},
	}

	got := accountRegisteredWatchRoots(
		[]string{root}, roots,
		[]agentsync.RecursiveWatchResult{{Watched: 1}, {Unwatched: 1, Err: errors.New("descriptor unavailable")}},
	)

	assert.Equal(t, []string{root}, got)
}

func TestStartFileWatcherKeepsPollingForIndependentReasonAfterPendingCoverage(t *testing.T) {
	root := t.TempDir()
	roots := []watchRoot{
		{
			path: filepath.Join(root, "state", "sessions"), recursive: true,
			scopes:             []watchScope{{agent: parser.AgentDevin, syncDir: root}},
			pendingPollingDirs: []string{root},
		},
		{
			path: root, exists: true,
			scopes:                []watchScope{{agent: parser.AgentDevin, syncDir: root}},
			persistentPollingDirs: []string{root},
		},
	}

	got := accountRegisteredWatchRoots(
		[]string{root}, roots,
		[]agentsync.RecursiveWatchResult{{
			Watched:                   1,
			MissingRootLifecycleOwned: true,
		}, {Watched: 1}},
	)

	assert.Equal(t, []string{root}, got,
		"covering a missing root must not erase an independent polling reason")
}

func TestWatchPollingObligationsMissingRootLifecycleCardinality(t *testing.T) {
	for _, rootCount := range []int{1, 128} {
		t.Run(fmt.Sprintf("roots=%d", rootCount), func(t *testing.T) {
			base := t.TempDir()
			roots := make([]watchRoot, 0, rootCount)
			owned := make([]agentsync.RecursiveWatchResult, 0, rootCount)
			portable := make([]agentsync.RecursiveWatchResult, 0, rootCount)
			unwatched := make([]string, 0, rootCount)
			for i := range rootCount {
				syncDir := filepath.Join(base, fmt.Sprintf("agent-%03d", i))
				roots = append(roots, watchRoot{
					path:               filepath.Join(syncDir, "sessions"),
					recursive:          true,
					scopes:             []watchScope{{agent: parser.AgentClaude, syncDir: syncDir}},
					pendingPollingDirs: []string{syncDir},
				})
				owned = append(owned, agentsync.RecursiveWatchResult{
					Watched:                   1,
					MissingRootLifecycleOwned: true,
				})
				portable = append(portable, agentsync.RecursiveWatchResult{})
				unwatched = append(unwatched, syncDir)
			}

			assert.Empty(t, watchPollingObligations(roots, owned, nil),
				"native lifecycle work must remain independent of configured archive cardinality")
			assert.Len(t, watchPollingObligations(roots, portable, unwatched), rootCount,
				"portable backends retain one obligation per uncovered missing root")
		})
	}
}

func TestOpenCodeFormatMissingRootsUseNativeLifecycleWithoutPolling(t *testing.T) {
	for _, rootCount := range []int{1, 128} {
		t.Run(fmt.Sprintf("roots=%d", rootCount), func(t *testing.T) {
			base := t.TempDir()
			dirs := make([]string, 0, rootCount)
			for i := range rootCount {
				dirs = append(dirs, filepath.Join(base, fmt.Sprintf("mimocode-%03d", i)))
			}
			cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
				parser.AgentMiMoCode: dirs,
			}}

			roots, unwatched := collectWatchRoots(cfg)
			require.Len(t, roots, rootCount)
			results := make([]agentsync.RecursiveWatchResult, rootCount)
			for i := range results {
				results[i] = agentsync.RecursiveWatchResult{
					Watched:                   1,
					MissingRootLifecycleOwned: true,
				}
			}
			unwatched = accountRegisteredWatchRoots(unwatched, roots, results)

			assert.Empty(t, watchPollingObligations(roots, results, unwatched),
				"absent OpenCode-format providers must not add archive-scale polling")
		})
	}
}

func TestWatchPollingObligationsKeepPendingAndPersistentReasonsIndependent(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "shared")
	pendingPath := filepath.Join(shared, "pending")
	roots := []watchRoot{
		{
			path:               pendingPath,
			scopes:             []watchScope{{agent: parser.AgentDevin, syncDir: shared}},
			pendingPollingDirs: []string{shared},
		},
		{
			path: filepath.Join(shared, "existing"), exists: true,
			scopes:                []watchScope{{agent: parser.AgentDevin, syncDir: shared}},
			persistentPollingDirs: []string{shared},
		},
	}

	got := watchPollingObligations(
		roots,
		[]agentsync.RecursiveWatchResult{{Watched: 1}, {Watched: 1}},
		[]string{shared},
	)

	assert.Equal(t, []agentsync.PollingObligation{
		{Key: pendingPath, Roots: []string{shared}, Probe: pendingPath},
		{Key: "persistent:" + shared, Roots: []string{shared}, Probe: shared},
	}, got)
}

func TestWatchPollingObligationsCoverRegistrationFailureByLogicalRoot(t *testing.T) {
	syncDir := t.TempDir()
	watchPath := filepath.Join(syncDir, "sessions")
	roots := []watchRoot{{
		path: watchPath, exists: true, recursive: true,
		scopes: []watchScope{{agent: parser.AgentClaude, syncDir: syncDir}},
	}}

	got := watchPollingObligations(
		roots,
		[]agentsync.RecursiveWatchResult{{Unwatched: 1, Err: errors.New("watch failed")}},
		[]string{syncDir},
	)

	assert.Equal(t, []agentsync.PollingObligation{{
		Key: watchPath, Roots: []string{syncDir}, Probe: watchPath,
	}}, got)
}

func findCollectedWatchRoot(roots []watchRoot, path string) (watchRoot, bool) {
	path = filepath.Clean(path)
	for _, root := range roots {
		if filepath.Clean(root.path) == path {
			return root, true
		}
	}
	return watchRoot{}, false
}

func TestResyncCoversSignals(t *testing.T) {
	tests := []struct {
		name     string
		stats    agentsync.SyncStats
		fellBack bool
		want     bool
	}{
		{
			name:  "clean resync no orphans covers signals",
			stats: agentsync.SyncStats{Synced: 5},
			want:  true,
		},
		{
			name: "fell back to incremental sync needs backfill",
			stats: agentsync.SyncStats{
				Synced: 2, Aborted: true,
			},
			fellBack: true,
			want:     false,
		},
		{
			name: "orphans copied need backfill",
			stats: agentsync.SyncStats{
				Synced: 5, OrphanedCopied: 3,
			},
			want: false,
		},
		{
			name: "orphans copied even with fallback false",
			stats: agentsync.SyncStats{
				Synced: 0, OrphanedCopied: 1,
			},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resyncCoversSignals(tc.stats, tc.fellBack)
			assert.Equal(t, tc.want, got)
		})
	}
}

type fakeSignalsBackfillMarker struct {
	calls int
	err   error
}

func (f *fakeSignalsBackfillMarker) MarkSignalsBackfillDone() error {
	f.calls++
	return f.err
}

func TestFinishInitialResyncMarksCoveredSignals(t *testing.T) {
	marker := &fakeSignalsBackfillMarker{}

	finishInitialResync(marker, true)

	assert.Equal(t, 1, marker.calls)
}

func TestFinishInitialResyncSkipsMarkerWhenSignalsNeedBackfill(t *testing.T) {
	marker := &fakeSignalsBackfillMarker{}

	finishInitialResync(marker, false)

	assert.Equal(t, 0, marker.calls)
}

func TestFormatAnomalySummary(t *testing.T) {
	tests := []struct {
		name        string
		anomalies   agentsync.AnomalyStats
		wantEmpty   bool
		wantContain []string
		wantOmit    []string
	}{
		{
			name:      "clean run omits the section",
			anomalies: agentsync.AnomalyStats{},
			wantEmpty: true,
		},
		{
			name: "malformed lines only",
			anomalies: agentsync.AnomalyStats{
				MalformedLinesByAgent: map[string]int{
					"claude": 3, "codex": 1,
				},
				MalformedLinesTotal: 4,
			},
			wantContain: []string{
				"Parser anomalies (this run):",
				"malformed lines: 4 total",
				"claude: 3",
				"codex: 1",
			},
			wantOmit: []string{"sanitized fields"},
		},
		{
			name: "sanitize fixes only, zero categories omitted",
			anomalies: agentsync.AnomalyStats{
				Sanitize: agentsync.SanitizeStats{
					ControlCharsStripped: 2,
					ModelClamped:         1,
				},
			},
			wantContain: []string{
				"sanitized fields: 3 total",
				"control chars stripped: 2",
				"model clamped: 1",
			},
			wantOmit: []string{
				"malformed lines",
				"tokens clamped",
				"role coerced",
				"timestamps blanked",
			},
		},
		{
			name: "both sections present",
			anomalies: agentsync.AnomalyStats{
				MalformedLinesByAgent: map[string]int{"gemini": 7},
				MalformedLinesTotal:   7,
				Sanitize: agentsync.SanitizeStats{
					TokensClamped:     4,
					TimestampsBlanked: 1,
				},
			},
			wantContain: []string{
				"malformed lines: 7 total",
				"gemini: 7",
				"sanitized fields: 5 total",
				"tokens clamped: 4",
				"timestamps blanked: 1",
			},
		},
		{
			name: "unknown schema sessions only",
			anomalies: agentsync.AnomalyStats{
				UnknownSchemaSessionsByAgent: map[string]int{
					"antigravity": 2, "antigravity-cli": 1,
				},
				UnknownSchemaSessionsTotal: 3,
			},
			wantContain: []string{
				"Parser anomalies (this run):",
				"unrecognized schema sessions: 3 total",
				"antigravity: 2",
				"antigravity-cli: 1",
			},
			wantOmit: []string{"malformed lines", "sanitized fields"},
		},
		{
			name: "gen_metadata without usage only",
			anomalies: agentsync.AnomalyStats{
				GenMetadataWithoutUsageByAgent: map[string]int{
					"antigravity": 1, "antigravity-cli": 2,
				},
				GenMetadataWithoutUsageTotal: 3,
			},
			wantContain: []string{
				"Parser anomalies (this run):",
				"gen_metadata without usage: 3 total",
				"antigravity: 1",
				"antigravity-cli: 2",
			},
			wantOmit: []string{
				"malformed lines",
				"unrecognized schema sessions",
				"sanitized fields",
			},
		},
		{
			name: "all sections present",
			anomalies: agentsync.AnomalyStats{
				MalformedLinesByAgent:          map[string]int{"gemini": 7},
				MalformedLinesTotal:            7,
				UnknownSchemaSessionsByAgent:   map[string]int{"antigravity": 2},
				UnknownSchemaSessionsTotal:     2,
				GenMetadataWithoutUsageByAgent: map[string]int{"antigravity-cli": 3},
				GenMetadataWithoutUsageTotal:   3,
				Sanitize: agentsync.SanitizeStats{
					TokensClamped:     4,
					TimestampsBlanked: 1,
				},
			},
			wantContain: []string{
				"malformed lines: 7 total",
				"gemini: 7",
				"unrecognized schema sessions: 2 total",
				"antigravity: 2",
				"gen_metadata without usage: 3 total",
				"antigravity-cli: 3",
				"sanitized fields: 5 total",
				"tokens clamped: 4",
				"timestamps blanked: 1",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatAnomalySummary(tc.anomalies)
			if tc.wantEmpty {
				assert.Empty(t, got)
				return
			}
			for _, want := range tc.wantContain {
				assert.Contains(t, got, want)
			}
			for _, omit := range tc.wantOmit {
				assert.NotContains(t, got, omit)
			}
		})
	}
}

func TestPrintSyncSummaryAnomalySection(t *testing.T) {
	t.Run("clean run omits anomaly section", func(t *testing.T) {
		out := captureStdout(t, func() {
			printSyncSummary(agentsync.SyncStats{Synced: 3}, time.Now())
		})
		assert.Contains(t, out, "Sync complete: 3 sessions synced")
		assert.NotContains(t, out, "Parser anomalies")
	})

	t.Run("non-zero anomalies print the section", func(t *testing.T) {
		stats := agentsync.SyncStats{
			Synced: 2,
			Anomalies: agentsync.AnomalyStats{
				MalformedLinesByAgent: map[string]int{"claude": 5},
				MalformedLinesTotal:   5,
				Sanitize: agentsync.SanitizeStats{
					ControlCharsStripped: 2,
				},
			},
		}
		out := captureStdout(t, func() {
			printSyncSummary(stats, time.Now())
		})
		assert.Contains(t, out, "Parser anomalies (this run):")
		assert.Contains(t, out, "malformed lines: 5 total")
		assert.Contains(t, out, "claude: 5")
		assert.Contains(t, out, "control chars stripped: 2")
		// Anomaly section follows the one-line summary.
		idx := strings.Index(out, "Sync complete")
		anomalyIdx := strings.Index(out, "Parser anomalies")
		assert.Less(t, idx, anomalyIdx)
	})
}

func TestSchemaUpgradeHint(t *testing.T) {
	t.Run("guides outdated-schema errors to a daemon restart", func(t *testing.T) {
		base := &db.SchemaUpgradeRequiredError{
			Table:  "tool_calls",
			Column: "file_path",
		}
		got := schemaUpgradeHint(base)
		// The original error stays wrappable so logs keep the detail, and the
		// hint names the command that actually runs the pending migration.
		assert.ErrorIs(t, got, base)
		assert.Contains(t, got.Error(), "agentsview daemon restart")
	})

	t.Run("passes unrelated errors through unchanged", func(t *testing.T) {
		base := errors.New("disk is on fire")
		assert.Equal(t, base, schemaUpgradeHint(base))
	})
}

type watchSyncRecorder struct {
	pathCalls      [][]string
	pathErr        error
	lookupResults  map[string]bool
	lookupCalls    [][2]string
	reconcileCalls []watchReconcileCall
	reconcileRoots map[string][]string
	reconcileErr   error
	callOrder      []string
	ctxValue       any
}

type watchReconcileCall struct {
	roots      []string
	full       bool
	lostEvents bool
}

type watchRetryBatchError interface {
	error
	WatchRetryBatch() agentsync.WatchBatch
}

type scopedReconciliationError struct {
	roots []string
}

func (e scopedReconciliationError) Error() string { return "partial reconciliation" }

func (e scopedReconciliationError) ReconciliationRetryRoots() []string {
	return append([]string(nil), e.roots...)
}

func requireWatchRetryBatch(t *testing.T, err error) agentsync.WatchBatch {
	t.Helper()
	var retryErr watchRetryBatchError
	require.ErrorAs(t, err, &retryErr)
	return retryErr.WatchRetryBatch()
}

func (r *watchSyncRecorder) SyncPathsContext(ctx context.Context, paths []string) error {
	r.pathCalls = append(r.pathCalls, append([]string(nil), paths...))
	r.callOrder = append(r.callOrder, "paths")
	r.ctxValue = ctx.Value(watchSyncContextKey{})
	return r.pathErr
}

func (r *watchSyncRecorder) HasActiveSessionSourceBelow(agent, path string) (bool, error) {
	r.lookupCalls = append(r.lookupCalls, [2]string{agent, path})
	return r.lookupResults[agent+"\x00"+path], nil
}

func (r *watchSyncRecorder) ReconciliationRootsForAgent(agent string) []string {
	return append([]string(nil), r.reconcileRoots[agent]...)
}

func (r *watchSyncRecorder) ReconcileWatchRoots(
	ctx context.Context, roots []string, full bool,
) error {
	r.reconcileCalls = append(r.reconcileCalls, watchReconcileCall{
		roots: append([]string(nil), roots...),
		full:  full,
	})
	r.callOrder = append(r.callOrder, "reconcile")
	r.ctxValue = ctx.Value(watchSyncContextKey{})
	return r.reconcileErr
}

func (r *watchSyncRecorder) ReconcileWatchRootsAfterLostEvents(
	ctx context.Context, roots []string, full bool,
) error {
	r.reconcileCalls = append(r.reconcileCalls, watchReconcileCall{
		roots:      append([]string(nil), roots...),
		full:       full,
		lostEvents: true,
	})
	r.callOrder = append(r.callOrder, "reconcile")
	r.ctxValue = ctx.Value(watchSyncContextKey{})
	return r.reconcileErr
}

type watchSyncContextKey struct{}

func TestSyncWatchBatch(t *testing.T) {
	ctx := context.WithValue(context.Background(), watchSyncContextKey{}, "serve")
	tempDir := t.TempDir()
	filePath := filepath.Join(tempDir, "session.jsonl")
	require.NoError(t, os.WriteFile(filePath, []byte("session"), 0o600))
	dirPath := filepath.Join(tempDir, "renamed-dir")
	require.NoError(t, os.Mkdir(dirPath, 0o700))
	missingPath := filepath.Join(tempDir, "missing")
	root := filepath.Join(tempDir, "root")
	agent := string(parser.AgentCodex)

	t.Run("file rename uses changed path", func(t *testing.T) {
		recorder := &watchSyncRecorder{}
		err := syncWatchBatch(ctx, recorder, agentsync.WatchBatch{Renames: []agentsync.WatchRename{{
			Path: filePath, Root: root, Agent: agent, ItemType: agentsync.ItemIsFile,
		}}})

		require.NoError(t, err)
		assert.Equal(t, [][]string{{filePath}}, recorder.pathCalls)
		assert.Empty(t, recorder.lookupCalls)
		assert.Empty(t, recorder.reconcileCalls)
		assert.Equal(t, "serve", recorder.ctxValue)
	})

	t.Run("directory rename reconciles only owning provider roots", func(t *testing.T) {
		otherRoot := filepath.Join(tempDir, "other-root")
		recorder := &watchSyncRecorder{reconcileRoots: map[string][]string{
			agent: {root, otherRoot},
		}}
		err := syncWatchBatch(ctx, recorder, agentsync.WatchBatch{Renames: []agentsync.WatchRename{{
			Path: dirPath, Root: root, Agent: agent, ItemType: agentsync.ItemIsDir,
		}}})

		require.NoError(t, err)
		assert.Empty(t, recorder.pathCalls)
		assert.Empty(t, recorder.lookupCalls)
		assert.Equal(t, []watchReconcileCall{{roots: []string{root, otherRoot}}}, recorder.reconcileCalls)
	})

	t.Run("directory rename includes every shared-root owner", func(t *testing.T) {
		claude := string(parser.AgentClaude)
		claudeRoot := filepath.Join(tempDir, "claude-root")
		recorder := &watchSyncRecorder{reconcileRoots: map[string][]string{
			agent:  {root},
			claude: {claudeRoot},
		}}
		err := syncWatchBatch(ctx, recorder, agentsync.WatchBatch{Renames: []agentsync.WatchRename{
			{Path: dirPath, Root: root, Agent: agent, ItemType: agentsync.ItemIsDir},
			{Path: dirPath, Root: root, Agent: claude, ItemType: agentsync.ItemIsDir},
		}})

		require.NoError(t, err)
		assert.Equal(t, []watchReconcileCall{{roots: []string{root, claudeRoot}}}, recorder.reconcileCalls)
	})

	t.Run("unknown rename stats file", func(t *testing.T) {
		recorder := &watchSyncRecorder{}
		err := syncWatchBatch(ctx, recorder, agentsync.WatchBatch{Renames: []agentsync.WatchRename{{
			Path: filePath, Root: root, Agent: agent, ItemType: agentsync.ItemIsUnknown,
		}}})

		require.NoError(t, err)
		assert.Equal(t, [][]string{{filePath}}, recorder.pathCalls)
		assert.Empty(t, recorder.lookupCalls)
		assert.Empty(t, recorder.reconcileCalls)
	})

	for _, tc := range []struct {
		name     string
		itemType agentsync.WatchItemType
		path     string
	}{
		{name: "directory metadata", itemType: agentsync.ItemIsDir, path: missingPath},
		{name: "unknown rename stats directory", itemType: agentsync.ItemIsUnknown, path: dirPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &watchSyncRecorder{}
			err := syncWatchBatch(ctx, recorder, agentsync.WatchBatch{Renames: []agentsync.WatchRename{{
				Path: tc.path, Root: root, Agent: agent, ItemType: tc.itemType,
			}}})

			require.NoError(t, err)
			assert.Empty(t, recorder.pathCalls)
			assert.Empty(t, recorder.lookupCalls)
			assert.Equal(t, []watchReconcileCall{{full: true}}, recorder.reconcileCalls)
		})
	}

	t.Run("missing unknown with active descendant reconciles fully", func(t *testing.T) {
		recorder := &watchSyncRecorder{lookupResults: map[string]bool{
			agent + "\x00" + missingPath: true,
		}}
		err := syncWatchBatch(ctx, recorder, agentsync.WatchBatch{Renames: []agentsync.WatchRename{{
			Path: missingPath, Root: root, Agent: agent, ItemType: agentsync.ItemIsUnknown,
		}}})

		require.NoError(t, err)
		assert.Empty(t, recorder.pathCalls)
		assert.Equal(t, [][2]string{{agent, missingPath}}, recorder.lookupCalls)
		assert.Equal(t, []watchReconcileCall{{full: true}}, recorder.reconcileCalls)
	})

	t.Run("missing unknown without active descendant emits tombstone path", func(t *testing.T) {
		recorder := &watchSyncRecorder{}
		err := syncWatchBatch(ctx, recorder, agentsync.WatchBatch{Renames: []agentsync.WatchRename{{
			Path: missingPath, Root: root, Agent: agent, ItemType: agentsync.ItemIsUnknown,
		}}})

		require.NoError(t, err)
		assert.Equal(t, [][]string{{missingPath}}, recorder.pathCalls)
		assert.Equal(t, [][2]string{{agent, missingPath}}, recorder.lookupCalls)
		assert.Empty(t, recorder.reconcileCalls)
	})

	t.Run("overlapping unrelated agent source does not promote", func(t *testing.T) {
		recorder := &watchSyncRecorder{lookupResults: map[string]bool{
			string(parser.AgentClaude) + "\x00" + missingPath: true,
		}}
		err := syncWatchBatch(ctx, recorder, agentsync.WatchBatch{Renames: []agentsync.WatchRename{{
			Path: missingPath, Root: root, Agent: agent, ItemType: agentsync.ItemIsUnknown,
		}}})

		require.NoError(t, err)
		assert.Equal(t, [][2]string{{agent, missingPath}}, recorder.lookupCalls)
		assert.Equal(t, [][]string{{missingPath}}, recorder.pathCalls)
		assert.Empty(t, recorder.reconcileCalls)
	})

	t.Run("any exact shared-root owner can promote", func(t *testing.T) {
		claude := string(parser.AgentClaude)
		recorder := &watchSyncRecorder{lookupResults: map[string]bool{
			claude + "\x00" + missingPath: true,
		}}
		err := syncWatchBatch(ctx, recorder, agentsync.WatchBatch{Renames: []agentsync.WatchRename{
			{Path: missingPath, Root: root, Agent: claude, ItemType: agentsync.ItemIsUnknown},
			{Path: missingPath, Root: root, Agent: agent, ItemType: agentsync.ItemIsUnknown},
		}})

		require.NoError(t, err)
		assert.Equal(t, [][2]string{{claude, missingPath}, {agent, missingPath}}, recorder.lookupCalls)
		assert.Empty(t, recorder.pathCalls)
		assert.Equal(t, []watchReconcileCall{{full: true}}, recorder.reconcileCalls)
	})

	t.Run("ordinary paths precede deduplicated root reconciliation", func(t *testing.T) {
		recorder := &watchSyncRecorder{}
		err := syncWatchBatch(ctx, recorder, agentsync.WatchBatch{
			Paths:          []string{"/sessions/a.jsonl", "/sessions/b.jsonl"},
			ReconcileRoots: []string{"/sessions", "/sessions"},
		})

		require.NoError(t, err)
		assert.Equal(t, []string{"paths", "reconcile"}, recorder.callOrder)
		assert.Equal(t, []watchReconcileCall{{roots: []string{"/sessions"}}}, recorder.reconcileCalls)
	})

	t.Run("root-count overflow reconciles all roots directly", func(t *testing.T) {
		recorder := &watchSyncRecorder{}
		err := syncWatchBatch(ctx, recorder, agentsync.WatchBatch{
			FullSync: true, LostEvents: true,
		})

		require.NoError(t, err)
		assert.Empty(t, recorder.pathCalls)
		assert.Equal(t, []watchReconcileCall{{full: true, lostEvents: true}}, recorder.reconcileCalls)
		assert.Equal(t, "serve", recorder.ctxValue)
	})

	t.Run("reconciliation error is returned for watcher retry", func(t *testing.T) {
		reconcileErr := errors.New("provider discovery incomplete")
		recorder := &watchSyncRecorder{reconcileErr: reconcileErr}

		err := syncWatchBatch(ctx, recorder, agentsync.WatchBatch{
			ReconcileRoots: []string{"/sessions"},
		})

		assert.ErrorContains(t, err, reconcileErr.Error())
		assert.Equal(t, []watchReconcileCall{{roots: []string{"/sessions"}}}, recorder.reconcileCalls)
	})

	t.Run("full reconciliation retries only failed provider roots", func(t *testing.T) {
		failedRoot := "/sessions/unavailable-provider"
		reconcileErr := scopedReconciliationError{roots: []string{failedRoot}}
		recorder := &watchSyncRecorder{reconcileErr: reconcileErr}

		err := syncWatchBatch(ctx, recorder, agentsync.WatchBatch{
			FullSync: true, LostEvents: true,
		})

		assert.ErrorContains(t, err, reconcileErr.Error())
		assert.Equal(t, agentsync.WatchBatch{
			ReconcileRoots: []string{failedRoot},
			LostEvents:     true,
		}, requireWatchRetryBatch(t, err))
	})

	t.Run("scoped lost-event retry keeps forced recovery", func(t *testing.T) {
		root := "/sessions/unavailable-provider"
		recorder := &watchSyncRecorder{}

		err := syncWatchBatch(ctx, recorder, agentsync.WatchBatch{
			ReconcileRoots: []string{root},
			LostEvents:     true,
		})

		require.NoError(t, err)
		assert.Equal(t, []watchReconcileCall{{
			roots: []string{root}, lostEvents: true,
		}}, recorder.reconcileCalls)
	})

	t.Run("changed path failure retains the exact bounded batch", func(t *testing.T) {
		pathErr := errors.New("archive write failed")
		recorder := &watchSyncRecorder{pathErr: pathErr}

		err := syncWatchBatch(ctx, recorder, agentsync.WatchBatch{
			Paths: []string{"/sessions/a.jsonl", "/sessions/b.jsonl"},
		})

		assert.ErrorIs(t, err, pathErr)
		assert.Equal(t, agentsync.WatchBatch{
			Paths: []string{"/sessions/a.jsonl", "/sessions/b.jsonl"},
		}, requireWatchRetryBatch(t, err))
		assert.Empty(t, recorder.reconcileCalls)
	})

	t.Run("changed path failure retains pending root reconciliation", func(t *testing.T) {
		pathErr := errors.New("archive write failed")
		recorder := &watchSyncRecorder{pathErr: pathErr}

		err := syncWatchBatch(ctx, recorder, agentsync.WatchBatch{
			Paths:          []string{"/sessions/a.jsonl"},
			ReconcileRoots: []string{"/sessions", "/sessions"},
		})

		assert.ErrorIs(t, err, pathErr)
		assert.Equal(t, agentsync.WatchBatch{
			Paths:          []string{"/sessions/a.jsonl"},
			ReconcileRoots: []string{"/sessions"},
		}, requireWatchRetryBatch(t, err))
		assert.Empty(t, recorder.reconcileCalls)
	})

	t.Run("changed path failure retains directory rename reconciliation", func(t *testing.T) {
		pathErr := errors.New("archive write failed")
		recorder := &watchSyncRecorder{pathErr: pathErr}

		err := syncWatchBatch(ctx, recorder, agentsync.WatchBatch{
			Paths: []string{"/sessions/a.jsonl"},
			Renames: []agentsync.WatchRename{{
				Path: dirPath, Root: root, Agent: agent, ItemType: agentsync.ItemIsDir,
			}},
		})

		assert.ErrorIs(t, err, pathErr)
		assert.Equal(t, agentsync.WatchBatch{FullSync: true}, requireWatchRetryBatch(t, err))
		assert.Empty(t, recorder.reconcileCalls)
	})
}

type stubRetryRootsError struct{ roots []string }

func (e *stubRetryRootsError) Error() string { return "reconciliation incomplete" }

func (e *stubRetryRootsError) ReconciliationRetryRoots() []string { return e.roots }

// TestGapReconciliationRetryBatch pins the startup-gap classification: a gap
// reconciliation error carrying retry roots yields a scoped retry batch, and
// any other failure escalates to an authoritative full sync.
func TestGapReconciliationRetryBatch(t *testing.T) {
	scoped := gapReconciliationRetryBatch(fmt.Errorf(
		"gap: %w", &stubRetryRootsError{roots: []string{"/b", "/a", "/a"}},
	))
	assert.Equal(t, agentsync.WatchBatch{
		ReconcileRoots: []string{"/b", "/a"},
	}, scoped)

	full := gapReconciliationRetryBatch(errors.New("plain failure"))
	assert.Equal(t, agentsync.WatchBatch{FullSync: true}, full)

	empty := gapReconciliationRetryBatch(fmt.Errorf(
		"gap: %w", &stubRetryRootsError{},
	))
	assert.Equal(t, agentsync.WatchBatch{FullSync: true}, empty,
		"an empty retry scope must escalate to a full sync")
}

func TestSyncWatchBatchReportsClassifiedReconciliationRetryScope(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	filePath := filepath.Join(tempDir, "session.jsonl")
	require.NoError(t, os.WriteFile(filePath, []byte("session"), 0o600))
	dirPath := filepath.Join(tempDir, "renamed-dir")
	require.NoError(t, os.Mkdir(dirPath, 0o700))
	missingPath := filepath.Join(tempDir, "missing")
	root := filepath.Join(tempDir, "root")
	agent := string(parser.AgentCodex)
	reconcileErr := errors.New("provider discovery incomplete")

	t.Run("unknown rename that stats as file retries roots", func(t *testing.T) {
		recorder := &watchSyncRecorder{reconcileErr: reconcileErr}
		err := syncWatchBatch(ctx, recorder, agentsync.WatchBatch{
			Renames: []agentsync.WatchRename{{
				Path: filePath, Root: root, Agent: agent,
				ItemType: agentsync.ItemIsUnknown,
			}},
			ReconcileRoots: []string{root, root},
		})

		assert.ErrorIs(t, err, reconcileErr)
		assert.Equal(t, agentsync.WatchBatch{
			ReconcileRoots: []string{root},
		}, requireWatchRetryBatch(t, err))
		assert.Equal(t, [][]string{{filePath}}, recorder.pathCalls)
		assert.Empty(t, recorder.lookupCalls)
		assert.Equal(t, []watchReconcileCall{{roots: []string{root}}}, recorder.reconcileCalls)
	})

	t.Run("missing unknown rename with negative lookup retries roots", func(t *testing.T) {
		recorder := &watchSyncRecorder{reconcileErr: reconcileErr}
		err := syncWatchBatch(ctx, recorder, agentsync.WatchBatch{
			Renames: []agentsync.WatchRename{{
				Path: missingPath, Root: root, Agent: agent,
				ItemType: agentsync.ItemIsUnknown,
			}},
			ReconcileRoots: []string{root, root},
		})

		assert.ErrorIs(t, err, reconcileErr)
		assert.Equal(t, agentsync.WatchBatch{
			ReconcileRoots: []string{root},
		}, requireWatchRetryBatch(t, err))
		assert.Equal(t, [][]string{{missingPath}}, recorder.pathCalls)
		assert.Equal(t, [][2]string{{agent, missingPath}}, recorder.lookupCalls)
		assert.Equal(t, []watchReconcileCall{{roots: []string{root}}}, recorder.reconcileCalls)
	})

	t.Run("directory rename retries full reconciliation", func(t *testing.T) {
		recorder := &watchSyncRecorder{reconcileErr: reconcileErr}
		err := syncWatchBatch(ctx, recorder, agentsync.WatchBatch{Renames: []agentsync.WatchRename{{
			Path: dirPath, Root: root, Agent: agent,
			ItemType: agentsync.ItemIsDir,
		}}})

		assert.ErrorIs(t, err, reconcileErr)
		assert.Equal(t, agentsync.WatchBatch{FullSync: true}, requireWatchRetryBatch(t, err))
		assert.Empty(t, recorder.pathCalls)
		assert.Empty(t, recorder.lookupCalls)
		assert.Equal(t, []watchReconcileCall{{full: true}}, recorder.reconcileCalls)
	})

	t.Run("missing unknown rename with positive lookup retries full reconciliation", func(t *testing.T) {
		recorder := &watchSyncRecorder{
			reconcileErr: reconcileErr,
			lookupResults: map[string]bool{
				agent + "\x00" + missingPath: true,
			},
		}
		err := syncWatchBatch(ctx, recorder, agentsync.WatchBatch{Renames: []agentsync.WatchRename{{
			Path: missingPath, Root: root, Agent: agent,
			ItemType: agentsync.ItemIsUnknown,
		}}})

		assert.ErrorIs(t, err, reconcileErr)
		assert.Equal(t, agentsync.WatchBatch{FullSync: true}, requireWatchRetryBatch(t, err))
		assert.Empty(t, recorder.pathCalls)
		assert.Equal(t, [][2]string{{agent, missingPath}}, recorder.lookupCalls)
		assert.Equal(t, []watchReconcileCall{{full: true}}, recorder.reconcileCalls)
	})
}

// The serve daemon installs its default soft memory limit only when the
// operator has not set GOMEMLIMIT themselves.
func TestApplyServeMemoryLimit(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })

	t.Run("sets the default when GOMEMLIMIT is unset", func(t *testing.T) {
		t.Setenv("GOMEMLIMIT", "")
		debug.SetMemoryLimit(math.MaxInt64)
		applyServeMemoryLimit()
		assert.Equal(t, int64(serveMemoryLimitBytes),
			debug.SetMemoryLimit(-1))
	})

	t.Run("respects an operator override", func(t *testing.T) {
		t.Setenv("GOMEMLIMIT", "1GiB")
		debug.SetMemoryLimit(math.MaxInt64)
		applyServeMemoryLimit()
		assert.Equal(t, int64(math.MaxInt64), debug.SetMemoryLimit(-1))
	})
}

// An absent configured root (unmounted volume, deleted directory) or a
// present root whose physical session subtree is missing (Gemini's
// <root>/tmp) streams an empty discovery without error, so the gap
// reconciliation and archive audit must defer those scopes instead of
// tombstoning every session beneath them.
func TestReconcileRootPathsDefersUnavailableScopes(t *testing.T) {
	presentDir := t.TempDir()
	missingDir := filepath.Join(t.TempDir(), "gone")
	geminiRoot := t.TempDir()
	cfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {presentDir, missingDir},
			parser.AgentGemini: {geminiRoot},
		},
	}

	paths := reconcileRootPaths(cfg)

	assert.Contains(t, paths, presentDir)
	assert.NotContains(t, paths, missingDir,
		"an absent configured root must be deferred, not reconciled as empty")
	assert.NotContains(t, paths, geminiRoot,
		"a configured dir whose physical session subtree is missing keeps that subtree as its probe")
	assert.NotContains(t, paths, filepath.Join(geminiRoot, "tmp"),
		"the missing physical subtree itself must not be reconciled")

	require.NoError(t, os.MkdirAll(filepath.Join(geminiRoot, "tmp"), 0o755))
	paths = reconcileRootPaths(cfg)
	assert.Contains(t, paths, filepath.Join(geminiRoot, "tmp"),
		"a present physical subtree rejoins the reconciliation scope")
}
