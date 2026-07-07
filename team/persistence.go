package team

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"LuminaCode/agent"
	"LuminaCode/config"
)

type persistedTeamFile struct {
	ID              string   `json:"id"`
	ParentSessionID string   `json:"parent_session_id"`
	Team            string   `json:"team"`
	Snapshot        Snapshot `json:"snapshot"`
}

func (m *Manager) RestorePersistedForParent(parentSessionID, cwd string) []Snapshot {
	parentSessionID = strings.TrimSpace(parentSessionID)
	if parentSessionID == "" {
		return nil
	}
	baseRoot := filepath.Join(m.Config.SessionDir, parentSessionID, "teams")
	entries, err := os.ReadDir(baseRoot)
	if err != nil {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var snapshots []Snapshot
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		root := filepath.Join(baseRoot, entry.Name())
		var persisted persistedTeamFile
		if !readJSON(filepath.Join(root, "team.json"), &persisted) || persisted.Team == "" {
			continue
		}
		m.mu.Lock()
		existing := m.sessions[persisted.ID]
		m.mu.Unlock()
		if existing != nil {
			snapshots = append(snapshots, existing.Snapshot())
			continue
		}
		cfg := m.Config
		if strings.TrimSpace(cwd) != "" && cwd != cfg.CWD {
			cfg = config.NewConfigForCWD(cwd)
			cfg.TeamDir = m.Config.TeamDir
			applyPinnedTeamConfig(&cfg, m.Config)
		}
		spec, err := NewLoader(cfg).Load(persisted.Team)
		if err != nil {
			continue
		}
		session := NewSession(parentSessionID, cfg, spec, m.emit, m.askPermission)
		session.ID = persisted.ID
		session.rootDir = root
		session.dialogue = readJSONL[DialogueEntry](filepath.Join(root, "dialogue.jsonl"))
		session.timeline = readJSONL[TimelineEvent](filepath.Join(root, "timeline.jsonl"))
		var artifacts []Artifact
		if readJSON(filepath.Join(root, "artifacts", "index.json"), &artifacts) {
			session.artifacts = artifacts
		}
		session.loopIteration = persisted.Snapshot.LoopIteration
		session.waitingForUser = false
		session.gate = persisted.Snapshot.GateStatus
		session.contract = cloneContract(persisted.Snapshot.TeamContract)
		session.gateVerdicts = cloneGateVerdicts(persisted.Snapshot.GateVerdicts)
		if session.gateVerdicts == nil {
			session.gateVerdicts = map[string]GateVerdict{}
		}
		for _, row := range persisted.Snapshot.ActivityRows {
			row = normalizeRestoredActivity(row)
			session.activity[row.AgentID] = row
		}
		for id, runtime := range session.agents {
			var state agent.AgentState
			if readJSON(filepath.Join(root, "agents", id, "state.json"), &state) {
				runtime.State = &state
			}
		}
		m.mu.Lock()
		m.sessions[session.ID] = session
		m.mu.Unlock()
		session.persist()
		snapshots = append(snapshots, session.Snapshot())
	}
	return snapshots
}

func normalizeRestoredActivity(row ActivityRow) ActivityRow {
	if row.Status == "running" {
		row.Status = "interrupted"
		row.Summary = "stopped: backend restarted"
	}
	return row
}

func readJSON(path string, out any) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(data, out) == nil
}

func readJSONL[T any](path string) []T {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	var out []T
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var item T
		if json.Unmarshal(scanner.Bytes(), &item) == nil {
			out = append(out, item)
		}
	}
	return out
}
