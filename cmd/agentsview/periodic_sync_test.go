package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
)

func TestScheduledReconcileTargetsSelectsOnlyOptedInProviders(t *testing.T) {
	home := t.TempDir()
	aiderDir := filepath.Join(home, "aider")
	coworkDir := filepath.Join(home, "cowork")
	claudeDir := filepath.Join(home, "claude")
	require.NoError(t, os.MkdirAll(aiderDir, 0o755))
	require.NoError(t, os.MkdirAll(coworkDir, 0o755))
	require.NoError(t, os.MkdirAll(claudeDir, 0o755))

	cfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentAider:  {aiderDir},
			parser.AgentCowork: {coworkDir},
			parser.AgentClaude: {claudeDir},
		},
	}
	targets := scheduledReconcileTargets(cfg)
	require.Len(t, targets, 1, "only the opted-in provider is scheduled")
	assert.Equal(t, parser.AgentAider, targets[0].Agent)
	assert.Equal(t, []string{aiderDir}, targets[0].Roots)
}

type fakeScheduledEngine struct {
	calls []scheduledReconcileTarget
	err   error
}

func (f *fakeScheduledEngine) ReconcileProviderRoots(
	_ context.Context, agent parser.AgentType, roots []string,
) error {
	f.calls = append(f.calls, scheduledReconcileTarget{Agent: agent, Roots: roots})
	return f.err
}

func TestRunScheduledSyncPassCallsPerAgent(t *testing.T) {
	engine := &fakeScheduledEngine{}
	runScheduledSyncPass(context.Background(), engine, nil)
	assert.Empty(t, engine.calls, "no targets -> no reconciliation")

	runScheduledSyncPass(context.Background(), engine,
		[]scheduledReconcileTarget{{Agent: parser.AgentAider, Roots: []string{"/a"}}})
	require.Len(t, engine.calls, 1)
	assert.Equal(t, parser.AgentAider, engine.calls[0].Agent)
	assert.Equal(t, []string{"/a"}, engine.calls[0].Roots)
}
