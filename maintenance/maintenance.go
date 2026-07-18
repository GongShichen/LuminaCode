package maintenance

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"LuminaCode/config"

	_ "modernc.org/sqlite"
)

const recentSessionGrace = 24 * time.Hour

type Options struct {
	Enforce         bool
	CurrentSessions map[string]struct{}
	Now             time.Time
}

type Report struct {
	SessionDir      string        `json:"session_dir"`
	ArchiveDir      string        `json:"archive_dir"`
	Mode            string        `json:"mode"`
	Enforced        bool          `json:"enforced"`
	TotalBytes      int64         `json:"total_bytes"`
	SessionCount    int           `json:"session_count"`
	ArchivedCount   int           `json:"archived_count"`
	DeletedCount    int           `json:"deleted_count"`
	FreedBytes      int64         `json:"freed_bytes"`
	Sessions        []SessionInfo `json:"sessions"`
	ArchiveSessions []ArchiveInfo `json:"archive_sessions"`
	Actions         []Action      `json:"actions"`
	Warnings        []string      `json:"warnings,omitempty"`
	SkippedActive   []string      `json:"skipped_active,omitempty"`
	SkippedPinned   []string      `json:"skipped_pinned,omitempty"`
	SkippedRecent   []string      `json:"skipped_recent,omitempty"`
	GeneratedAt     string        `json:"generated_at"`
}

type SessionInfo struct {
	SessionID     string  `json:"session_id"`
	Path          string  `json:"path"`
	SizeBytes     int64   `json:"size_bytes"`
	LastUpdated   float64 `json:"last_updated"`
	MessageCount  int     `json:"message_count"`
	TurnCount     int     `json:"turn_count"`
	TeamCount     int     `json:"team_count"`
	SQLiteCount   int     `json:"sqlite_count"`
	ArtifactCount int     `json:"artifact_count"`
	Pinned        bool    `json:"pinned"`
	Recent        bool    `json:"recent"`
	Active        bool    `json:"active"`
}

type ArchiveInfo struct {
	SessionID   string  `json:"session_id"`
	Path        string  `json:"path"`
	SizeBytes   int64   `json:"size_bytes"`
	LastUpdated float64 `json:"last_updated"`
}

type Action struct {
	Type         string `json:"type"`
	SessionID    string `json:"session_id,omitempty"`
	Path         string `json:"path"`
	ArchivePath  string `json:"archive_path,omitempty"`
	Reason       string `json:"reason"`
	Bytes        int64  `json:"bytes"`
	WouldDelete  bool   `json:"would_delete"`
	WouldArchive bool   `json:"would_archive"`
	Deleted      bool   `json:"deleted"`
	Archived     bool   `json:"archived"`
	Error        string `json:"error,omitempty"`
}

type sessionMeta struct {
	SessionID    string  `json:"session_id"`
	CreatedAt    float64 `json:"created_at"`
	LastUpdated  float64 `json:"last_updated"`
	MessageCount int     `json:"message_count"`
	TurnCount    int     `json:"turn_count"`
	Pinned       bool    `json:"pinned,omitempty"`
}

func Status(cfg config.Config, opts Options) (Report, error) {
	opts.Enforce = false
	return Cleanup(cfg, opts)
}

func Cleanup(cfg config.Config, opts Options) (Report, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	sessionDir := cfg.SessionDir
	if strings.TrimSpace(sessionDir) == "" {
		sessionDir = config.NewConfigForCWD(cfg.CWD).SessionDir
	}
	archiveDir := strings.TrimSpace(cfg.SessionArchiveDir)
	if archiveDir == "" {
		archiveDir = ArchiveDir(sessionDir)
	}
	report := Report{
		SessionDir:  sessionDir,
		ArchiveDir:  archiveDir,
		Mode:        maintenanceMode(cfg),
		Enforced:    opts.Enforce,
		GeneratedAt: now.UTC().Format(time.RFC3339Nano),
	}
	sessions, orphanActions, err := scanSessionDir(sessionDir, opts.CurrentSessions, now)
	if err != nil {
		return report, err
	}
	report.Sessions = sessions
	report.SessionCount = len(sessions)
	for _, session := range sessions {
		report.TotalBytes += session.SizeBytes
	}
	report.ArchiveSessions = scanArchives(archiveDir)
	knownSessions := make(map[string]struct{}, len(report.Sessions)+len(report.ArchiveSessions))
	for _, session := range report.Sessions {
		knownSessions[session.SessionID] = struct{}{}
	}
	for _, session := range report.ArchiveSessions {
		knownSessions[session.SessionID] = struct{}{}
	}
	toolResultsRoot := cfg.ToolResultsRoot()
	if entries, readErr := os.ReadDir(toolResultsRoot); readErr == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			path := filepath.Join(toolResultsRoot, entry.Name())
			size, _, _ := scanDirStats(path)
			if entry.Name() == "_legacy" {
				report.Warnings = append(report.Warnings, fmt.Sprintf("unowned legacy tool results require explicit review: %s (%d bytes)", path, size))
				continue
			}
			if _, ok := knownSessions[entry.Name()]; !ok {
				report.Actions = append(report.Actions, Action{Type: "tool-results", SessionID: entry.Name(), Path: path,
					Reason: "owning session no longer exists", Bytes: size, WouldDelete: true})
			}
		}
	}
	for _, action := range orphanActions {
		report.Actions = append(report.Actions, action)
	}
	if !cfg.SessionMaintenanceEnabled {
		report.Actions = nil
		report.Warnings = append(report.Warnings, "session maintenance is disabled")
		return report, nil
	}
	sessionActions := planSessionActions(cfg, sessions, archiveDir, now)
	report.Actions = append(report.Actions, sessionActions...)
	for _, action := range sessionActions {
		toolResultsDir := cfg.ToolResultsDir(action.SessionID)
		if info, err := os.Stat(toolResultsDir); err == nil && info.IsDir() {
			size, _, _ := scanDirStats(toolResultsDir)
			report.Actions = append(report.Actions, Action{
				Type: "tool-results", SessionID: action.SessionID, Path: toolResultsDir,
				Reason: "owning session is scheduled for deletion", Bytes: size, WouldDelete: true,
			})
		}
	}
	if !opts.Enforce {
		return report, nil
	}
	for i := range report.Actions {
		action := &report.Actions[i]
		if action.Type == "orphan" {
			if err := os.RemoveAll(action.Path); err != nil {
				action.Error = err.Error()
				report.Warnings = append(report.Warnings, fmt.Sprintf("remove %s: %v", action.Path, err))
				continue
			}
			action.Deleted = true
			report.FreedBytes += action.Bytes
			continue
		}
		if action.Type == "tool-results" {
			if err := os.RemoveAll(action.Path); err != nil {
				action.Error = err.Error()
				report.Warnings = append(report.Warnings, fmt.Sprintf("remove %s: %v", action.Path, err))
				continue
			}
			action.Deleted = true
			report.FreedBytes += action.Bytes
			continue
		}
		if action.SessionID == "" {
			continue
		}
		if action.WouldArchive {
			if err := archiveSession(sessionDir, archiveDir, action.SessionID); err != nil {
				action.Error = err.Error()
				report.Warnings = append(report.Warnings, fmt.Sprintf("archive %s: %v", action.SessionID, err))
				continue
			}
			action.Archived = true
			report.ArchivedCount++
		}
		if action.WouldDelete {
			if err := os.RemoveAll(action.Path); err != nil {
				action.Error = err.Error()
				report.Warnings = append(report.Warnings, fmt.Sprintf("delete %s: %v", action.Path, err))
				continue
			}
			action.Deleted = true
			report.DeletedCount++
			report.FreedBytes += action.Bytes
		}
	}
	return report, nil
}

func ArchiveDir(sessionDir string) string {
	parent := filepath.Dir(filepath.Clean(sessionDir))
	if parent == "." || parent == "" {
		parent = filepath.Dir(sessionDir)
	}
	if filepath.Base(filepath.Clean(sessionDir)) == "active" {
		return filepath.Join(parent, "archive")
	}
	return filepath.Join(parent, "session-archive")
}

func scanSessionDir(sessionDir string, active map[string]struct{}, now time.Time) ([]SessionInfo, []Action, error) {
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var sessions []SessionInfo
	var orphans []Action
	for _, entry := range entries {
		path := filepath.Join(sessionDir, entry.Name())
		if !entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				continue
			}
			orphans = append(orphans, Action{
				Type:        "orphan",
				Path:        path,
				Reason:      "top-level non-session file under session_dir",
				Bytes:       info.Size(),
				WouldDelete: true,
			})
			continue
		}
		meta := readSessionMeta(filepath.Join(path, "meta.json"))
		id := meta.SessionID
		if id == "" {
			id = entry.Name()
		}
		size, sqliteCount, artifactCount := scanDirStats(path)
		lastUpdated := meta.LastUpdated
		if lastUpdated == 0 {
			if info, err := entry.Info(); err == nil {
				lastUpdated = float64(info.ModTime().UnixNano()) / 1e9
			}
		}
		teamCount := countDirs(filepath.Join(path, "teams"))
		_, isActive := active[id]
		sessions = append(sessions, SessionInfo{
			SessionID:     id,
			Path:          path,
			SizeBytes:     size,
			LastUpdated:   lastUpdated,
			MessageCount:  meta.MessageCount,
			TurnCount:     meta.TurnCount,
			TeamCount:     teamCount,
			SQLiteCount:   sqliteCount,
			ArtifactCount: artifactCount,
			Pinned:        meta.Pinned,
			Recent:        isRecent(lastUpdated, now),
			Active:        isActive,
		})
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].LastUpdated > sessions[j].LastUpdated
	})
	return sessions, orphans, nil
}

func planSessionActions(cfg config.Config, sessions []SessionInfo, archiveDir string, now time.Time) []Action {
	var actions []Action
	total := int64(0)
	for _, session := range sessions {
		total += session.SizeBytes
	}
	protected := map[string]struct{}{}
	for _, session := range sessions {
		if session.Active || (cfg.SessionProtectPinned && session.Pinned) || session.Recent {
			protected[session.SessionID] = struct{}{}
		}
	}
	addAction := func(session SessionInfo, reason string) {
		if _, skip := protected[session.SessionID]; skip {
			return
		}
		for _, action := range actions {
			if action.SessionID == session.SessionID {
				return
			}
		}
		actions = append(actions, Action{
			Type:         "session",
			SessionID:    session.SessionID,
			Path:         session.Path,
			ArchivePath:  filepath.Join(archiveDir, session.SessionID),
			Reason:       reason,
			Bytes:        session.SizeBytes,
			WouldArchive: cfg.SessionArchiveBeforeDelete,
			WouldDelete:  true,
		})
	}
	retentionDays := cfg.SessionRetentionDays
	if retentionDays <= 0 {
		retentionDays = 30
	}
	cutoff := now.Add(-time.Duration(retentionDays) * 24 * time.Hour)
	for _, session := range oldestFirst(sessions) {
		if session.LastUpdated > 0 && unixTime(session.LastUpdated).Before(cutoff) {
			addAction(session, fmt.Sprintf("older than session_retention_days=%d", retentionDays))
		}
	}
	maxEntries := cfg.SessionMaxEntries
	if maxEntries <= 0 {
		maxEntries = 500
	}
	counted := 0
	for _, session := range sessions {
		if _, skip := protected[session.SessionID]; skip {
			continue
		}
		counted++
		if counted > maxEntries {
			addAction(session, fmt.Sprintf("exceeds session_max_entries=%d", maxEntries))
		}
	}
	budget := cfg.SessionMaxDiskBytes
	if budget > 0 && total > highWaterBytes(budget, cfg.SessionHighWaterRatio) {
		projected := total
		for _, action := range actions {
			projected -= action.Bytes
		}
		for _, session := range oldestFirst(sessions) {
			if projected <= budget {
				break
			}
			if _, skip := protected[session.SessionID]; skip {
				continue
			}
			before := len(actions)
			addAction(session, fmt.Sprintf("reduces session_max_disk_bytes=%d", budget))
			if len(actions) > before {
				projected -= session.SizeBytes
			}
		}
	}
	return actions
}

func oldestFirst(sessions []SessionInfo) []SessionInfo {
	out := append([]SessionInfo(nil), sessions...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastUpdated < out[j].LastUpdated
	})
	return out
}

func highWaterBytes(maxBytes int64, ratio float64) int64 {
	if ratio <= 0 || ratio > 1 {
		ratio = 0.8
	}
	return int64(float64(maxBytes) * ratio)
}

func maintenanceMode(cfg config.Config) string {
	mode := strings.ToLower(strings.TrimSpace(cfg.SessionMaintenanceMode))
	if mode != "warn" && mode != "enforce" {
		return "warn"
	}
	return mode
}

func isRecent(lastUpdated float64, now time.Time) bool {
	if lastUpdated <= 0 {
		return false
	}
	return now.Sub(unixTime(lastUpdated)) < recentSessionGrace
}

func unixTime(value float64) time.Time {
	sec := int64(value)
	nsec := int64((value - float64(sec)) * 1e9)
	return time.Unix(sec, nsec)
}

func readSessionMeta(path string) sessionMeta {
	data, err := os.ReadFile(path)
	if err != nil {
		return sessionMeta{}
	}
	var meta sessionMeta
	_ = json.Unmarshal(data, &meta)
	return meta
}

func scanDirStats(root string) (size int64, sqliteCount int, artifactCount int) {
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		size += info.Size()
		if strings.HasSuffix(entry.Name(), ".sqlite") || strings.HasSuffix(entry.Name(), ".sqlite-wal") || strings.HasSuffix(entry.Name(), ".sqlite-shm") {
			sqliteCount++
		}
		if strings.Contains(filepath.ToSlash(path), "/artifacts/") && entry.Name() != "index.json" {
			artifactCount++
		}
		return nil
	})
	return size, sqliteCount, artifactCount
}

func countDirs(root string) int {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			count++
		}
	}
	return count
}

func scanArchives(archiveDir string) []ArchiveInfo {
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		return nil
	}
	var out []ArchiveInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(archiveDir, entry.Name())
		size, _, _ := scanDirStats(path)
		lastUpdated := float64(0)
		if info, err := entry.Info(); err == nil {
			lastUpdated = float64(info.ModTime().UnixNano()) / 1e9
		}
		out = append(out, ArchiveInfo{SessionID: entry.Name(), Path: path, SizeBytes: size, LastUpdated: lastUpdated})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastUpdated > out[j].LastUpdated })
	return out
}

func archiveSession(sessionDir, archiveDir, sessionID string) error {
	source := filepath.Join(sessionDir, sessionID)
	if _, err := os.Stat(source); err != nil {
		return err
	}
	target := filepath.Join(archiveDir, sessionID)
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	meta := readSessionMeta(filepath.Join(source, "meta.json"))
	_ = writeJSON(filepath.Join(target, "meta.json"), meta)
	artifacts := collectArtifactIndexes(source)
	_ = writeJSON(filepath.Join(target, "artifacts.json"), artifacts)
	commits := collectCommitList(source)
	_ = writeJSON(filepath.Join(target, "session-memory-commits.json"), commits)
	summary := renderArchiveSummary(sessionID, meta, artifacts, commits)
	return os.WriteFile(filepath.Join(target, "summary.md"), []byte(summary), 0o600)
}

func collectArtifactIndexes(sessionRoot string) []map[string]any {
	var out []map[string]any
	matches, _ := filepath.Glob(filepath.Join(sessionRoot, "teams", "*", "artifacts", "index.json"))
	for _, path := range matches {
		var entries []map[string]any
		if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &entries) == nil {
			for _, entry := range entries {
				entry["team_artifact_index"] = path
				out = append(out, entry)
			}
		}
	}
	return out
}

func collectCommitList(sessionRoot string) []map[string]any {
	var out []map[string]any
	_ = filepath.WalkDir(sessionRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sqlite") {
			return nil
		}
		db, err := sql.Open("sqlite", path)
		if err != nil {
			return nil
		}
		defer db.Close()
		rows, err := db.QueryContext(context.Background(), `SELECT commit_no, COALESCE(title, ''), COALESCE(summary, ''), start_turn_count, end_turn_count, created_at FROM commits ORDER BY commit_no`)
		if err != nil {
			return nil
		}
		defer rows.Close()
		for rows.Next() {
			var commitNo, startTurn, endTurn int
			var title, summary string
			var created float64
			if rows.Scan(&commitNo, &title, &summary, &startTurn, &endTurn, &created) == nil {
				out = append(out, map[string]any{
					"sqlite_path":      path,
					"commit_no":        commitNo,
					"title":            title,
					"summary_preview":  clamp(summary, 600),
					"start_turn_count": startTurn,
					"end_turn_count":   endTurn,
					"created_at":       created,
				})
			}
		}
		return nil
	})
	return out
}

func renderArchiveSummary(sessionID string, meta sessionMeta, artifacts []map[string]any, commits []map[string]any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Lumina Session Archive\n\n")
	fmt.Fprintf(&b, "- Session ID: `%s`\n", sessionID)
	fmt.Fprintf(&b, "- Messages: %d\n", meta.MessageCount)
	fmt.Fprintf(&b, "- Turns: %d\n", meta.TurnCount)
	fmt.Fprintf(&b, "- Artifacts indexed: %d\n", len(artifacts))
	fmt.Fprintf(&b, "- Session memory commits indexed: %d\n", len(commits))
	if meta.LastUpdated > 0 {
		fmt.Fprintf(&b, "- Last updated: %s\n", unixTime(meta.LastUpdated).Format(time.RFC3339))
	}
	b.WriteString("\nThis archive is intentionally lightweight. It does not contain the full transcript, full tool results, or full team timeline, and it cannot be resumed.\n")
	return b.String()
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func clamp(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "..."
}
