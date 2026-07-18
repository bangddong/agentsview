// ABOUTME: Multi-session container provider for omnigent: one chat.db fanned
// ABOUTME: out into one session per conversation, with incremental sync.
package parser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// omnigentChangeTracker remembers, per container, the schema and the
// updated_at floor of the last completed member sweep so a watcher event fans
// out only members changed since then. It is an optimization with a
// whole-container backstop: a cold or schema-changed container is returned as
// one whole source, whose complete parse reconciles archived membership and
// seeds the floor, and a scheduled full sync re-fans the container whenever
// its physical fingerprint no longer matches the stored one.
type omnigentTrackedContainer struct {
	schema    omnigentSchema
	checkedAt int64
}

type omnigentChangeTracker struct {
	mu         sync.Mutex
	containers map[string]omnigentTrackedContainer
}

func newOmnigentChangeTracker() *omnigentChangeTracker {
	return &omnigentChangeTracker{
		containers: make(map[string]omnigentTrackedContainer),
	}
}

// Omnigent stores every conversation in one shared SQLite database (chat.db).
// It is a multi-session container provider: discovery surfaces the database as
// one source whose parse fans out into one session per conversation, addressed
// by "<db>#<conversationID>" virtual paths, and watcher events fan out only
// the members changed since the tracker's last sweep.
func newOmnigentProviderFactory(def AgentDef) ProviderFactory {
	tracker := newOmnigentChangeTracker()
	return NewMultiSessionProviderFactory(
		def,
		omnigentProviderCapabilities(),
		func(cfg ProviderConfig) multiSessionContainerSourceSet {
			return NewMultiSessionContainerSourceSet(
				AgentOmnigent,
				cfg.Roots,
				WithContainerDiscovery(omnigentDiscoverContainers),
				WithWatchRoots(omnigentWatchRoots),
				WithChangedPathClassifier(omnigentClassifyPath),
				WithChangedPathMembers(tracker.changedMembers),
				WithMemberLookup(omnigentFindMember),
				WithFingerprint(omnigentFingerprintSource),
				WithContainerParse(tracker.parseContainer),
				WithMemberParse(omnigentParseMember),
				WithMemberResultHashPreservation(),
				WithMemberPresence(omnigentMemberPresent),
				WithUnsupportedSourceError(omnigentSchemaUnsupported),
				WithExcludedSessionIDs(omnigentLegacySessionIDs),
			)
		},
	)
}

// IsOmnigentContainerSource reports whether source addresses the whole
// physical chat database rather than one virtual conversation member.
func IsOmnigentContainerSource(source SourceRef) bool {
	if source.Provider != AgentOmnigent {
		return false
	}
	src, ok := source.Opaque.(multiSessionSource)
	return ok && src.Container != "" && src.MemberID == ""
}

func omnigentLegacySessionIDs(
	src multiSessionSource, results []ParseResult,
) []string {
	legacy := make(map[string]struct{})
	add := func(memberKey string) {
		_, rawID, ok := strings.Cut(memberKey, ":")
		if ok && IsValidSessionID(rawID) {
			legacy[omnigentIDPrefix+rawID] = struct{}{}
		}
	}
	if src.MemberID != "" {
		add(src.MemberID)
	}
	for _, result := range results {
		add(strings.TrimPrefix(result.Session.ID, omnigentIDPrefix))
	}
	ids := make([]string, 0, len(legacy))
	for id := range legacy {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func omnigentProviderCapabilities() Capabilities {
	return Capabilities{
		Source: multiSessionContainerSourceCapabilities(
			CapabilitySupported,
			CapabilitySupported,
		),
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			GitBranch:            CapabilitySupported,
			Relationships:        CapabilitySupported,
			Subagents:            CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
		},
	}
}

func omnigentDiscoverContainers(root string) []string {
	if dbPath := omnigentDBPath(root); dbPath != "" {
		return []string{dbPath}
	}
	return nil
}

func omnigentWatchRoots(roots []string) []WatchRoot {
	out := make([]WatchRoot, 0, len(roots))
	for _, root := range roots {
		out = append(out, WatchRoot{
			Path:         root,
			Recursive:    false,
			IncludeGlobs: []string{omnigentDBName, omnigentDBName + "-*"},
			DebounceKey:  string(AgentOmnigent) + ":db:" + root,
		})
	}
	return out
}

// omnigentClassifyPath maps a stored or changed path to its database container
// and conversation. allowMissing relaxes the regular-file requirement so a
// database delete (or its WAL/SHM sibling) still classifies for tombstones.
func omnigentClassifyPath(
	root, path string, allowMissing bool,
) (multiSessionMatch, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	requireRegular := !allowMissing
	if dbPath, conversationID, ok := parseOmnigentVirtualPath(path); ok {
		if !omnigentDBUnderRoot(root, dbPath, requireRegular) {
			return multiSessionMatch{}, false
		}
		return multiSessionMatch{
			Path:      path,
			Container: dbPath,
			MemberID:  conversationID,
		}, true
	}
	if omnigentDBUnderRoot(root, path, requireRegular) {
		return multiSessionMatch{Path: path, Container: path}, true
	}
	if allowMissing {
		if dbPath, ok := omnigentDBPathForEvent(root, path); ok {
			return multiSessionMatch{Path: dbPath, Container: dbPath}, true
		}
	}
	return multiSessionMatch{}, false
}

func omnigentFindMember(root, rawID string) (multiSessionMatch, bool) {
	if root == "" {
		return multiSessionMatch{}, false
	}
	dbPath := omnigentDBPath(root)
	if dbPath == "" || !omnigentConversationExists(dbPath, rawID) {
		return multiSessionMatch{}, false
	}
	return multiSessionMatch{
		Path:      VirtualSourcePath(dbPath, rawID),
		Container: dbPath,
		MemberID:  rawID,
	}, true
}

func omnigentFingerprintSource(src multiSessionSource) (SourceFingerprint, error) {
	info, err := os.Stat(src.Container)
	if err != nil {
		if os.IsNotExist(err) {
			return SourceFingerprint{}, nil
		}
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", src.Container, err)
	}
	fingerprint := SourceFingerprint{
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
	}
	if src.MemberID == "" {
		if compositeMtime, err := omnigentDBCompositeMtime(src.Container); err == nil {
			fingerprint.MTimeNS = compositeMtime
		}
		fingerprint.Hash, err = hashJSONLSourceFile(src.Container)
		if err != nil {
			return SourceFingerprint{}, err
		}
		return fingerprint, nil
	}

	conn, err := openOmnigentDB(src.Container)
	if err != nil {
		return SourceFingerprint{}, err
	}
	defer conn.Close()
	schema, err := detectOmnigentSchema(conn)
	if err != nil {
		return SourceFingerprint{}, err
	}
	// A member ID that no longer parses under the detected schema identifies
	// a retired legacy member (pre-schema-change identity), not a failure.
	member, err := omnigentMemberForSchema(schema, src.MemberID)
	if err != nil {
		return SourceFingerprint{}, nil
	}
	meta, ok, err := loadOmnigentConversationMeta(conn, schema, member)
	if err != nil {
		return SourceFingerprint{}, err
	}
	if ok {
		fingerprint.MTimeNS = meta.updatedAt * int64(1_000_000_000)
		fingerprint.Hash = meta.fingerprint()
		return fingerprint, nil
	}
	// Conversation row is gone but the DB file remains: return a keyed-empty
	// fingerprint without error so the engine proceeds to Parse, which
	// force-replaces the deleted session out of the archive.
	return SourceFingerprint{}, nil
}

func loadOmnigentConversationMeta(
	conn *sql.DB, schema omnigentSchema, member omnigentMemberID,
) (omnigentMeta, bool, error) {
	idExpr := omnigentIDExpr(schema, "c.id")
	query := `
		SELECT c.rowid, 0, ` + idExpr + `, COALESCE(c.updated_at, 0),
		       COUNT(ci.id), COALESCE(MAX(ci.position), -1)
		  FROM conversations c
		  LEFT JOIN conversation_items ci ON ci.conversation_id = c.id
		 WHERE c.id = ?
		 GROUP BY c.id`
	args := []any{omnigentIDArg(schema, member.rawID)}
	if schema.splitMetadata {
		query = `
			SELECT c.rowid, c.workspace_id, ` + idExpr + `, COALESCE(c.updated_at, 0),
			       COUNT(ci.id), COALESCE(MAX(ci.position), -1)
			  FROM conversations c
			  LEFT JOIN conversation_items ci
			    ON ci.workspace_id = c.workspace_id AND ci.conversation_id = c.id
			 WHERE c.workspace_id = ? AND c.id = ?
			 GROUP BY c.workspace_id, c.id`
		args = []any{member.workspaceID, omnigentIDArg(schema, member.rawID)}
	}
	var meta omnigentMeta
	err := conn.QueryRow(query, args...).Scan(
		&meta.rowID, &meta.workspaceID, &meta.rawID, &meta.updatedAt,
		&meta.itemCount, &meta.maxPosition,
	)
	if err == sql.ErrNoRows {
		return omnigentMeta{}, false, nil
	}
	if err != nil {
		return omnigentMeta{}, false, fmt.Errorf("loading omnigent conversation meta: %w", err)
	}
	return meta, true, nil
}

func (t *omnigentChangeTracker) changedMembers(
	ctx context.Context, root string, req ChangedPathRequest,
) ([]multiSessionMatch, error) {
	match, ok := omnigentClassifyPath(root, req.Path, true)
	if !ok {
		return nil, nil
	}
	if match.MemberID != "" || !IsRegularFile(match.Container) {
		return []multiSessionMatch{match}, nil
	}
	conn, err := openOmnigentDB(match.Container)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	schema, err := detectOmnigentSchema(conn)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	tracked, warm := t.containers[match.Container]
	t.mu.Unlock()
	if !warm || tracked.schema != schema {
		// A cold or schema-changed container parses whole: the complete
		// result set reconciles archived membership and seeds the floor.
		return []multiSessionMatch{match}, nil
	}
	// Capture the new floor before querying so a commit that lands during
	// the sweep is re-observed by the next event instead of skipped. The
	// window's upper bound keeps a clock-skewed future updated_at from
	// re-surfacing on every sweep; such rows wait for the next full parse.
	checkedAt := time.Now().Unix()
	changed, err := listOmnigentConversationMetasSince(
		ctx, conn, schema, max(tracked.checkedAt-1, 0), checkedAt+1,
	)
	if err != nil {
		return nil, err
	}
	tombstones, err := omnigentDeletedMemberTombstones(
		ctx, conn, root, match.Container, schema, req.StoredSourcePaths,
	)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	if current, ok := t.containers[match.Container]; ok && current.schema == schema {
		current.checkedAt = checkedAt
		t.containers[match.Container] = current
	}
	t.mu.Unlock()
	return appendOmnigentMatches(
		omnigentMatches(match.Container, schema, changed), tombstones,
	), nil
}

func listOmnigentConversationMetasSince(
	ctx context.Context, conn *sql.DB, schema omnigentSchema,
	updatedAfter, updatedThrough int64,
) ([]omnigentMeta, error) {
	idExpr := omnigentIDExpr(schema, "c.id")
	query := `
		WITH selected AS (
			SELECT rowid, id, COALESCE(updated_at, 0) AS updated_at
			  FROM conversations
			 WHERE updated_at >= ?
			   AND updated_at <= ?
		)
		SELECT c.rowid, 0, ` + idExpr + `, c.updated_at,
		       COUNT(ci.id), COALESCE(MAX(ci.position), -1)
		  FROM selected c
		  LEFT JOIN conversation_items ci ON ci.conversation_id = c.id
		 GROUP BY c.id
		 ORDER BY c.updated_at, c.rowid`
	if schema.splitMetadata {
		query = `
			WITH selected AS (
				SELECT rowid, workspace_id, id,
				       COALESCE(updated_at, 0) AS updated_at
				  FROM conversations
				 WHERE updated_at >= ?
				   AND updated_at <= ?
			)
			SELECT c.rowid, c.workspace_id, ` + idExpr + `, c.updated_at,
			       COUNT(ci.id), COALESCE(MAX(ci.position), -1)
			  FROM selected c
			  LEFT JOIN conversation_items ci
			    ON ci.workspace_id = c.workspace_id AND ci.conversation_id = c.id
			 GROUP BY c.workspace_id, c.id
			 ORDER BY c.updated_at, c.rowid`
	}
	return queryOmnigentConversationMetas(ctx, conn, query, updatedAfter, updatedThrough)
}

func queryOmnigentConversationMetas(
	ctx context.Context, conn *sql.DB, query string, args ...any,
) ([]omnigentMeta, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing changed omnigent conversations: %w", err)
	}
	defer rows.Close()
	var metas []omnigentMeta
	for rows.Next() {
		var meta omnigentMeta
		if err := rows.Scan(&meta.rowID, &meta.workspaceID, &meta.rawID, &meta.updatedAt,
			&meta.itemCount, &meta.maxPosition); err != nil {
			return nil, fmt.Errorf("scanning changed omnigent conversation: %w", err)
		}
		metas = append(metas, meta)
	}
	return metas, rows.Err()
}

func omnigentMatches(
	container string, schema omnigentSchema, metas []omnigentMeta,
) []multiSessionMatch {
	matches := make([]multiSessionMatch, 0, len(metas))
	seen := make(map[string]struct{}, len(metas))
	for _, meta := range metas {
		key := meta.member().key(schema)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		matches = append(matches, multiSessionMatch{
			Path: VirtualSourcePath(container, key), Container: container, MemberID: key,
		})
	}
	return matches
}

func appendOmnigentMatches(
	primary, extra []multiSessionMatch,
) []multiSessionMatch {
	seen := make(map[string]struct{}, len(primary)+len(extra))
	out := make([]multiSessionMatch, 0, len(primary)+len(extra))
	for _, matches := range [][]multiSessionMatch{primary, extra} {
		for _, match := range matches {
			if _, exists := seen[match.Path]; exists {
				continue
			}
			seen[match.Path] = struct{}{}
			out = append(out, match)
		}
	}
	return out
}

// omnigentDeletedMemberTombstones emits a match for each stored member of the
// changed container whose conversation row no longer exists, so an in-place
// deletion is retired by the next sweep instead of waiting for the scheduled
// full sync. Members still present are not re-emitted: one indexed ID scan on
// the already-open connection replaces per-member existence probes, and the
// fan-out stays bounded by the changed set plus actual deletions.
func omnigentDeletedMemberTombstones(
	ctx context.Context, conn *sql.DB, root, container string,
	schema omnigentSchema, storedSourcePaths []string,
) ([]multiSessionMatch, error) {
	var stored []multiSessionMatch
	for _, storedPath := range storedSourcePaths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		match, ok := omnigentClassifyPath(root, storedPath, true)
		if !ok || match.MemberID == "" || !samePath(match.Container, container) {
			continue
		}
		stored = append(stored, match)
	}
	if len(stored) == 0 {
		return nil, nil
	}
	live, err := listOmnigentMemberKeys(ctx, conn, schema)
	if err != nil {
		return nil, err
	}
	var tombstones []multiSessionMatch
	for _, match := range stored {
		if _, present := live[match.MemberID]; present {
			continue
		}
		tombstones = append(tombstones, match)
	}
	return tombstones, nil
}

func listOmnigentMemberKeys(
	ctx context.Context, conn *sql.DB, schema omnigentSchema,
) (map[string]struct{}, error) {
	idExpr := omnigentIDExpr(schema, "id")
	query := `SELECT 0, ` + idExpr + ` FROM conversations`
	if schema.splitMetadata {
		query = `SELECT workspace_id, ` + idExpr + ` FROM conversations`
	}
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("listing omnigent member keys: %w", err)
	}
	defer rows.Close()
	keys := make(map[string]struct{})
	for rows.Next() {
		var member omnigentMemberID
		if err := rows.Scan(&member.workspaceID, &member.rawID); err != nil {
			return nil, fmt.Errorf("scanning omnigent member key: %w", err)
		}
		keys[member.key(schema)] = struct{}{}
	}
	return keys, rows.Err()
}

func (t *omnigentChangeTracker) parseContainer(
	src multiSessionSource, req ParseRequest,
) ([]ParseResult, error) {
	// Capture the floor before reading so a commit that lands during the
	// parse is re-observed by the next changed-member sweep.
	checkedAt := time.Now().Unix()
	results, schema, _, err := omnigentParseContainerData(src, req)
	if err != nil {
		return nil, err
	}
	if IsRegularFile(src.Container) {
		t.mu.Lock()
		t.containers[src.Container] = omnigentTrackedContainer{
			schema: schema, checkedAt: checkedAt,
		}
		t.mu.Unlock()
	}
	return results, nil
}

// omnigentDBCompositeMtime tracks content-bearing SQLite files only. Opening a
// read connection can update the shared-memory file, so including -shm would
// turn the provider's own probes into apparent source changes and keep recovery
// sweeps running forever.
func omnigentDBCompositeMtime(dbPath string) (int64, error) {
	var maxMtime int64
	for _, suffix := range []string{"", "-wal"} {
		info, err := os.Stat(dbPath + suffix)
		if err != nil {
			continue
		}
		maxMtime = max(maxMtime, info.ModTime().UnixNano())
	}
	if maxMtime == 0 {
		return 0, &os.PathError{Op: "stat", Path: dbPath, Err: os.ErrNotExist}
	}
	return maxMtime, nil
}

func omnigentMemberPresent(src multiSessionSource) bool {
	if src.MemberID == "" {
		return IsRegularFile(src.Container)
	}
	return omnigentConversationExists(src.Container, src.MemberID)
}

func omnigentParseMember(
	src multiSessionSource, req ParseRequest,
) (*ParseResult, error) {
	dbInfo, err := os.Stat(src.Container)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat %s: %w", src.Container, err)
	}
	conn, err := openOmnigentDB(src.Container)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	schema, err := detectOmnigentSchema(conn)
	if err != nil {
		return nil, err
	}
	// A member ID that no longer parses under the detected schema is a retired
	// legacy identity; a nil result retires its archived session.
	member, err := omnigentMemberForSchema(schema, src.MemberID)
	if err != nil {
		return nil, nil
	}
	return parseOmnigentConversationFromDB(
		conn, schema, src.Container, member, req.Machine, dbInfo,
	)
}

func omnigentParseContainerData(
	src multiSessionSource, req ParseRequest,
) ([]ParseResult, omnigentSchema, []omnigentMeta, error) {
	dbInfo, err := os.Stat(src.Container)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, omnigentSchema{}, nil, nil
		}
		return nil, omnigentSchema{}, nil,
			fmt.Errorf("stat %s: %w", src.Container, err)
	}
	conn, err := openOmnigentDB(src.Container)
	if err != nil {
		return nil, omnigentSchema{}, nil, err
	}
	defer conn.Close()

	schema, err := detectOmnigentSchema(conn)
	if err != nil {
		return nil, omnigentSchema{}, nil, err
	}
	metas, err := listOmnigentConversationMetas(conn, schema)
	if err != nil {
		return nil, omnigentSchema{}, nil, err
	}
	results := make([]ParseResult, 0, len(metas))
	for i := range metas {
		result, err := parseOmnigentConversationFromDB(
			conn, schema, src.Container, metas[i].member(),
			req.Machine, dbInfo,
		)
		if err != nil {
			return nil, omnigentSchema{}, nil, err
		}
		if result == nil {
			continue
		}
		results = append(results, *result)
	}
	return results, schema, metas, nil
}

func omnigentSchemaUnsupported(err error) bool {
	var unsupported ErrOmnigentUnsupportedSchema
	return errors.As(err, &unsupported)
}

func omnigentDBUnderRoot(root, dbPath string, requireRegular bool) bool {
	root = filepath.Clean(root)
	dbPath = filepath.Clean(dbPath)
	rel, ok := relUnder(root, dbPath)
	if !ok || filepath.ToSlash(rel) != omnigentDBName {
		return false
	}
	return !requireRegular || IsRegularFile(dbPath)
}

func omnigentDBPathForEvent(root, path string) (string, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	// The provider's own read connections update the WAL shared-memory
	// file's mtime, so treating -shm events as source changes would make
	// every sweep trigger the next one, a permanent watcher loop. Real
	// commits always touch the database file or -wal as well.
	if strings.HasSuffix(path, "-shm") {
		return "", false
	}
	rel, ok := relUnder(root, path)
	if !ok {
		return "", false
	}
	if filepath.ToSlash(rel) == omnigentDBName ||
		(filepath.Dir(rel) == "." &&
			strings.HasPrefix(filepath.Base(rel), omnigentDBName+"-")) {
		return filepath.Join(root, omnigentDBName), true
	}
	return "", false
}

func parseOmnigentVirtualPath(path string) (string, string, bool) {
	container, member, ok := ParseVirtualSourcePathForBase(path, omnigentDBName)
	if !ok || strings.ContainsAny(member, `/\`) {
		return "", "", false
	}
	return container, member, true
}

// ParseOmnigentVirtualSourcePath recognizes only virtual member paths whose
// separator follows the physical chat.db basename. Directory names containing
// '#' therefore remain valid physical container paths.
func ParseOmnigentVirtualSourcePath(path string) (string, string, bool) {
	return parseOmnigentVirtualPath(path)
}
