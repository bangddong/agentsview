package sync

import (
	"context"
	"log"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// This file implements the engine half of the shared-container scheduling
// capability (parser.SourceCapabilities.ContainerScheduling): bounded
// scheduling for providers whose sessions live inside one mutable physical
// container (a shared SQLite database) rather than one file per session.
// Provider-specific identity mapping comes from parser.ContainerScheduler;
// everything here is provider-agnostic and keyed by agent.
//
// It has three cooperating parts:
//
//   - A stored-hint sweep pages archived source-path hints per (agent, watch
//     root) so one filesystem event never queries every archived member at
//     once. Sweeps activate on container-level events and drain across
//     subsequent events and discovery passes.
//   - A retry queue records sources and sessions whose parse or write did not
//     complete, bounded by collapsing a container's member retries into one
//     container recovery entry when they exceed one batch.
//   - Discovery hooks re-emit retry and reconciliation sources one bounded
//     page per pass, and an aborted resync queues container retries so
//     advanced provider change-tracking state cannot strand unstored work.

const (
	containerStoredHintBatchSize = 32
	containerRetryBatchSize      = 32
)

func containerSchedulerMap(
	factories map[parser.AgentType]parser.ProviderFactory,
) map[parser.AgentType]parser.ContainerScheduler {
	schedulers := make(map[parser.AgentType]parser.ContainerScheduler)
	for agent, factory := range factories {
		if factory == nil {
			continue
		}
		if factory.Capabilities().Source.ContainerScheduling !=
			parser.CapabilitySupported {
			continue
		}
		scheduler, ok := parser.ContainerSchedulerForFactory(factory)
		if !ok {
			log.Printf(
				"%s: ContainerScheduling capability declared without a "+
					"ContainerScheduler factory implementation",
				agent,
			)
			continue
		}
		schedulers[agent] = scheduler
	}
	return schedulers
}

// containerChangedPathOwnedByWatchRoot reports whether a changed path (or the
// container of a virtual member path) is a direct child of watchRoot, so an
// event under one configured root never claims another root's stored hints.
func containerChangedPathOwnedByWatchRoot(
	scheduler parser.ContainerScheduler, path, watchRoot string,
) bool {
	if container, _, virtual := scheduler.SplitContainerMemberPath(path); virtual {
		path = container
	}
	rel, err := filepath.Rel(filepath.Clean(watchRoot), filepath.Clean(path))
	return err == nil && rel != "." && filepath.Dir(rel) == "."
}

// containerHintCursor tracks one (agent, watch root) stored-hint sweep. A
// sweep activates with an empty page cursor, claims one page at a time
// (inFlight), and deactivates after a short page completes successfully.
// reactivate records an activation that arrived mid-sweep so the sweep
// restarts from the beginning instead of ending with unseen hints.
type containerHintCursor struct {
	after          string
	nextAfter      string
	active         bool
	inFlight       bool
	completeOnDone bool
	reactivate     bool
}

func containerHintCursorKey(agent parser.AgentType, watchRoot string) string {
	return string(agent) + "\x00" + filepath.Clean(watchRoot)
}

func (e *Engine) changedPathStoredSourcePaths(
	agent parser.AgentType, watchRoot string,
) ([]string, bool, error) {
	if _, ok := e.containerSchedulers[agent]; !ok {
		paths, err := e.db.ListStoredSourcePathHints(string(agent), []string{watchRoot})
		return paths, false, err
	}
	return e.nextContainerStoredHintPage(
		agent, watchRoot, true, containerStoredHintBatchSize,
	)
}

func (e *Engine) nextContainerStoredHintPage(
	agent parser.AgentType, watchRoot string, activate bool, limit int,
) ([]string, bool, error) {
	if limit <= 0 {
		return nil, false, nil
	}
	key := containerHintCursorKey(agent, watchRoot)
	e.containerHintMu.Lock()
	defer e.containerHintMu.Unlock()
	if e.containerHintCursors == nil {
		e.containerHintCursors = make(map[string]containerHintCursor)
	}
	cursor := e.containerHintCursors[key]
	if activate {
		if cursor.active {
			cursor.reactivate = true
		} else {
			cursor.active = true
			cursor.after = ""
		}
		e.containerHintCursors[key] = cursor
	}
	if !cursor.active || cursor.inFlight {
		return nil, false, nil
	}
	paths, err := e.db.ListStoredSourcePathHintPage(
		string(agent), watchRoot, cursor.after,
		limit,
	)
	if err != nil {
		return nil, false, err
	}
	if len(paths) == 0 {
		if cursor.reactivate {
			cursor = containerHintCursor{active: true}
		} else {
			cursor = containerHintCursor{}
		}
		e.containerHintCursors[key] = cursor
		return nil, false, nil
	}
	cursor.inFlight = true
	cursor.nextAfter = ""
	cursor.completeOnDone = len(paths) < limit
	if len(paths) == limit {
		cursor.nextAfter = paths[len(paths)-1]
	}
	e.containerHintCursors[key] = cursor
	return paths, true, nil
}

func (e *Engine) finishContainerStoredHintPage(
	agent parser.AgentType, watchRoot string, success bool,
) {
	key := containerHintCursorKey(agent, watchRoot)
	e.containerHintMu.Lock()
	defer e.containerHintMu.Unlock()
	cursor := e.containerHintCursors[key]
	if !cursor.inFlight {
		return
	}
	if success {
		cursor.after = cursor.nextAfter
		if cursor.completeOnDone {
			if cursor.reactivate {
				cursor.active = true
				cursor.after = ""
				cursor.reactivate = false
			} else {
				cursor.active = false
				cursor.after = ""
			}
		}
	}
	cursor.inFlight = false
	cursor.nextAfter = ""
	cursor.completeOnDone = false
	e.containerHintCursors[key] = cursor
}

func (e *Engine) activateContainerStoredHintSweep(
	agent parser.AgentType, watchRoot string,
) {
	key := containerHintCursorKey(agent, watchRoot)
	e.containerHintMu.Lock()
	defer e.containerHintMu.Unlock()
	if e.containerHintCursors == nil {
		e.containerHintCursors = make(map[string]containerHintCursor)
	}
	cursor := e.containerHintCursors[key]
	if cursor.active {
		cursor.reactivate = true
	} else {
		cursor.active = true
		cursor.after = ""
	}
	e.containerHintCursors[key] = cursor
}

func (e *Engine) containerStoredHintSweepActive(
	agent parser.AgentType, watchRoots []string,
) bool {
	e.containerHintMu.Lock()
	defer e.containerHintMu.Unlock()
	for _, watchRoot := range watchRoots {
		if e.containerHintCursors[containerHintCursorKey(agent, watchRoot)].active {
			return true
		}
	}
	return false
}

// containerRetrySource identifies one unit of unfinished container work: a
// session write, a source parse, or a whole-container recovery sweep.
type containerRetrySource struct {
	agent     parser.AgentType
	sessionID string
	filePath  string
	recovery  bool
}

type containerRetryEntry struct {
	containerRetrySource
	groupKey   string
	reactivate bool
	prev       *containerRetryEntry
	next       *containerRetryEntry
}

func (r containerRetrySource) key() string {
	if r.recovery {
		return string(r.agent) + "\x00recovery\x00" + filepath.Clean(r.filePath)
	}
	if r.sessionID != "" {
		return string(r.agent) + "\x00session\x00" + r.sessionID
	}
	return string(r.agent) + "\x00source\x00" + filepath.Clean(r.filePath)
}

func containerRetryGroupKey(agent parser.AgentType, container string) string {
	if container == "" {
		return ""
	}
	return string(agent) + "\x00" + container
}

// retryContainerForPath resolves the physical container a retry path belongs
// to. member is true when the path addresses one virtual member.
func retryContainerForPath(
	scheduler parser.ContainerScheduler, path string,
) (string, bool) {
	if container, _, virtual := scheduler.SplitContainerMemberPath(path); virtual {
		return filepath.Clean(container), true
	}
	path = filepath.Clean(path)
	if path == "" || path == "." {
		return "", false
	}
	return path, false
}

func (e *Engine) markContainerSourceRetry(agent parser.AgentType, path string) {
	scheduler, ok := e.containerSchedulers[agent]
	if !ok {
		return
	}
	retry := containerRetrySource{agent: agent, filePath: path}
	if _, memberID, virtual := scheduler.SplitContainerMemberPath(path); virtual &&
		memberID != "" {
		retry.sessionID = scheduler.MemberSessionID(memberID)
	}
	e.storeContainerRetry(scheduler, retry)
}

func (e *Engine) markContainerSessionRetry(pw pendingWrite) {
	scheduler, ok := e.containerSchedulers[pw.sess.Agent]
	if !ok || pw.sess.ID == "" || pw.sess.File.Path == "" {
		return
	}
	staleVersion := max(db.CurrentDataVersion()-1, 0)
	if e.db.GetSessionDataVersion(pw.sess.ID) > staleVersion {
		if err := e.db.SetSessionDataVersion(pw.sess.ID, staleVersion); err != nil {
			log.Printf("mark container retry stale for %s: %v", pw.sess.ID, err)
		}
	}
	e.storeContainerRetry(scheduler, containerRetrySource{
		agent:     pw.sess.Agent,
		sessionID: pw.sess.ID,
		filePath:  pw.sess.File.Path,
	})
}

func (e *Engine) storeContainerRetry(
	scheduler parser.ContainerScheduler, retry containerRetrySource,
) {
	e.containerRetryMu.Lock()
	if e.containerRetrySources == nil {
		e.containerRetrySources = make(map[string]*containerRetryEntry)
	}
	if e.containerRetryGroups == nil {
		e.containerRetryGroups = make(map[string]map[string]struct{})
	}
	activateRoot := e.storeContainerRetryLocked(scheduler, retry)
	e.containerRetryMu.Unlock()
	if activateRoot != "" {
		e.activateContainerStoredHintSweep(retry.agent, activateRoot)
	}
}

func (e *Engine) storeContainerRetryLocked(
	scheduler parser.ContainerScheduler, retry containerRetrySource,
) string {
	key := retry.key()
	if _, exists := e.containerRetrySources[key]; exists {
		return ""
	}
	container, member := retryContainerForPath(scheduler, retry.filePath)
	containerKey := containerRetrySource{
		agent: retry.agent, filePath: container,
	}.key()
	recoveryKey := containerRetrySource{
		agent: retry.agent, filePath: container, recovery: true,
	}.key()
	groupKey := containerRetryGroupKey(retry.agent, container)
	if member {
		if _, recoveringContainer := e.containerRetrySources[containerKey]; recoveringContainer {
			return ""
		}
		if recovery := e.containerRetrySources[recoveryKey]; recovery != nil {
			recovery.reactivate = true
			return ""
		}
	} else if container != "" {
		e.collapseContainerRetryGroupLocked(groupKey)
	}

	entry := &containerRetryEntry{
		containerRetrySource: retry,
		groupKey:             groupKey,
	}
	e.containerRetrySources[key] = entry
	e.appendContainerRetryLocked(entry)
	if groupKey != "" {
		bucket := e.containerRetryGroups[groupKey]
		if bucket == nil {
			bucket = make(map[string]struct{})
			e.containerRetryGroups[groupKey] = bucket
		}
		bucket[key] = struct{}{}
		if member && len(bucket) >= containerRetryBatchSize {
			e.collapseContainerRetryGroupLocked(groupKey)
			e.storeContainerRetryLocked(scheduler, containerRetrySource{
				agent:    retry.agent,
				filePath: container,
				recovery: true,
			})
			return filepath.Dir(container)
		}
	}
	return ""
}

func (e *Engine) appendContainerRetryLocked(entry *containerRetryEntry) {
	entry.prev = e.containerRetryTail
	entry.next = nil
	if e.containerRetryTail != nil {
		e.containerRetryTail.next = entry
	} else {
		e.containerRetryHead = entry
	}
	e.containerRetryTail = entry
}

func (e *Engine) moveContainerRetryToTailLocked(entry *containerRetryEntry) {
	if entry == nil || entry == e.containerRetryTail {
		return
	}
	if entry.prev != nil {
		entry.prev.next = entry.next
	} else {
		e.containerRetryHead = entry.next
	}
	entry.next.prev = entry.prev
	e.appendContainerRetryLocked(entry)
}

func (e *Engine) collapseContainerRetryGroupLocked(groupKey string) {
	for key := range e.containerRetryGroups[groupKey] {
		e.removeContainerRetryLocked(key)
	}
}

func (e *Engine) removeContainerRetryLocked(key string) {
	entry, exists := e.containerRetrySources[key]
	if !exists {
		return
	}
	if entry.prev != nil {
		entry.prev.next = entry.next
	} else {
		e.containerRetryHead = entry.next
	}
	if entry.next != nil {
		entry.next.prev = entry.prev
	} else {
		e.containerRetryTail = entry.prev
	}
	delete(e.containerRetrySources, key)
	if bucket := e.containerRetryGroups[entry.groupKey]; bucket != nil {
		delete(bucket, key)
		if len(bucket) == 0 {
			delete(e.containerRetryGroups, entry.groupKey)
		}
	}
}

func (e *Engine) clearContainerSessionRetry(pw pendingWrite) {
	if _, ok := e.containerSchedulers[pw.sess.Agent]; !ok || pw.sess.ID == "" {
		return
	}
	e.containerRetryMu.Lock()
	e.removeContainerRetryLocked(containerRetrySource{
		agent:     pw.sess.Agent,
		sessionID: pw.sess.ID,
	}.key())
	e.containerRetryMu.Unlock()
}

func (e *Engine) clearContainerSourceRetry(agent parser.AgentType, path string) {
	scheduler, ok := e.containerSchedulers[agent]
	if !ok || path == "" {
		return
	}
	retry := containerRetrySource{agent: agent, filePath: path}
	if _, memberID, virtual := scheduler.SplitContainerMemberPath(path); virtual &&
		memberID != "" {
		retry.sessionID = scheduler.MemberSessionID(memberID)
	}
	e.containerRetryMu.Lock()
	e.removeContainerRetryLocked(retry.key())
	e.containerRetryMu.Unlock()
}

// discoverContainerRetrySources re-emits one bounded page of an agent's
// pending retries as discovery sources. Paged entries move to the queue tail
// so unserved entries are not starved by a failing head.
func (e *Engine) discoverContainerRetrySources(
	ctx context.Context,
	agent parser.AgentType,
	provider parser.Provider,
	currentSources map[string]struct{},
) ([]parser.SourceRef, int) {
	e.containerRetryMu.Lock()
	pageSize := min(containerRetryBatchSize, len(e.containerRetrySources))
	matched := make([]*containerRetryEntry, 0, pageSize)
	for entry := e.containerRetryHead; entry != nil &&
		len(matched) < pageSize; entry = entry.next {
		if entry.agent == agent {
			matched = append(matched, entry)
		}
	}
	pending := make([]containerRetrySource, 0, len(matched))
	for _, entry := range matched {
		pending = append(pending, entry.containerRetrySource)
		e.moveContainerRetryToTailLocked(entry)
	}
	e.containerRetryMu.Unlock()

	var sources []parser.SourceRef
	var failures int
	for _, retry := range pending {
		if retry.recovery {
			recovered, err := provider.SourcesForChangedPath(
				ctx,
				parser.ChangedPathRequest{
					Path:      retry.filePath,
					EventKind: parser.ChangedPathEventRecovery,
					WatchRoot: filepath.Dir(retry.filePath),
				},
			)
			if err != nil {
				log.Printf("%s provider recovery lookup: %v", agent, err)
				failures++
				continue
			}
			if len(recovered) < containerRetryBatchSize {
				e.containerRetryMu.Lock()
				entry := e.containerRetrySources[retry.key()]
				if entry != nil && entry.reactivate {
					entry.reactivate = false
				} else {
					e.removeContainerRetryLocked(retry.key())
				}
				e.containerRetryMu.Unlock()
			}
			for _, source := range recovered {
				path := filepath.Clean(providerDiscoveredPath(source))
				if path == "." {
					continue
				}
				if _, exists := currentSources[path]; exists {
					continue
				}
				currentSources[path] = struct{}{}
				sources = append(sources, source)
			}
			continue
		}
		source, found, err := provider.FindSource(ctx, parser.FindSourceRequest{
			FullSessionID:      retry.sessionID,
			StoredFilePath:     retry.filePath,
			FingerprintKey:     retry.filePath,
			PreferStoredSource: true,
		})
		if err != nil {
			log.Printf("%s provider retry lookup: %v", agent, err)
			failures++
			continue
		}
		if !found {
			continue
		}
		path := filepath.Clean(providerDiscoveredPath(source))
		if _, exists := currentSources[path]; exists {
			continue
		}
		currentSources[path] = struct{}{}
		sources = append(sources, source)
	}
	return sources, failures
}

// discoverContainerReconciliationSources classifies one bounded page of
// archived source-path hints per active sweep so members deleted from a
// container are eventually tombstoned without an archive-wide query.
func (e *Engine) discoverContainerReconciliationSources(
	ctx context.Context,
	agent parser.AgentType,
	provider parser.Provider,
	currentSources map[string]struct{},
	limit int,
) ([]parser.SourceRef, int) {
	if limit <= 0 {
		return nil, 0
	}
	plan, err := provider.WatchPlan(ctx)
	if err != nil {
		log.Printf("%s provider reconciliation watch plan: %v", agent, err)
		return nil, 1
	}
	var sources []parser.SourceRef
	var failures int
	for _, watchRoot := range plan.Roots {
		hints, claimed, err := e.nextContainerStoredHintPage(
			agent, watchRoot.Path, false, limit-len(sources),
		)
		if err != nil {
			log.Printf("%s provider reconciliation hints: %v", agent, err)
			failures++
			continue
		}
		if len(hints) == 0 {
			continue
		}
		classified := false
		for _, include := range watchRoot.IncludeGlobs {
			if include == "" || strings.ContainsAny(include, `*?[\`) {
				continue
			}
			container := filepath.Join(watchRoot.Path, include)
			if !parser.IsRegularFile(container) {
				continue
			}
			matches, err := provider.SourcesForChangedPath(
				ctx,
				parser.ChangedPathRequest{
					Path:              container,
					EventKind:         parser.ChangedPathEventReconcile,
					WatchRoot:         watchRoot.Path,
					StoredSourcePaths: hints,
				},
			)
			if err != nil {
				log.Printf(
					"%s provider reconciliation classification: %v",
					agent, err,
				)
				failures++
				continue
			}
			classified = true
			for _, source := range matches {
				path := filepath.Clean(providerDiscoveredPath(source))
				if path == "." {
					continue
				}
				if _, exists := currentSources[path]; exists {
					continue
				}
				currentSources[path] = struct{}{}
				sources = append(sources, source)
			}
		}
		if claimed {
			e.finishContainerStoredHintPage(agent, watchRoot.Path, classified)
		}
	}
	return sources, failures
}

// queueContainerRetriesAfterAbortedResync marks every reachable physical
// container of every scheduling-capable agent for retry. It runs when an
// aborted resync already advanced provider change-tracking state, so work the
// discarded rebuild consumed cannot silently disappear from the kept archive.
func (e *Engine) queueContainerRetriesAfterAbortedResync() {
	agents := slices.SortedFunc(
		maps.Keys(e.containerSchedulers),
		func(a, b parser.AgentType) int {
			return strings.Compare(string(a), string(b))
		},
	)
	for _, agent := range agents {
		factory := e.providerFactories[agent]
		roots := e.agentDirs[agent]
		if factory == nil || len(roots) == 0 {
			continue
		}
		provider := factory.NewProvider(parser.ProviderConfig{
			Roots:   roots,
			Machine: e.machine,
		})
		plan, err := provider.WatchPlan(context.Background())
		if err != nil {
			log.Printf("%s provider abort recovery watch plan: %v", agent, err)
			continue
		}
		for _, root := range plan.Roots {
			for _, include := range root.IncludeGlobs {
				if include == "" || strings.ContainsAny(include, `*?[\`) {
					continue
				}
				path := filepath.Join(root.Path, include)
				if parser.IsRegularFile(path) {
					e.markContainerSourceRetry(agent, path)
				}
			}
		}
	}
}
