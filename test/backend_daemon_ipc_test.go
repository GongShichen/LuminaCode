package test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"LuminaCode/backend"
	"LuminaCode/config"
	"LuminaCode/session"
	luminateam "LuminaCode/team"

	"github.com/gorilla/websocket"
)

func TestBackendDaemonWebSocketStatusAndSessionCreate(t *testing.T) {
	root := t.TempDir()
	cfg := config.NewConfigForCWD(root)
	cfg.SessionDir = filepath.Join(root, "sessions")
	endpointPath := filepath.Join(root, "run", "backend.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- backend.Serve(ctx, backend.DaemonOptions{
			Host:         "127.0.0.1",
			Port:         0,
			Config:       cfg,
			EndpointPath: endpointPath,
		})
	}()
	endpoint := waitForEndpoint(t, endpointPath)
	wrongURL := "ws://" + endpoint.Host + ":" + intString(endpoint.Port) + "/v1/ws?token=wrong"
	_, response, err := websocket.DefaultDialer.Dial(wrongURL, nil)
	if err == nil {
		t.Fatal("expected wrong token dial to fail")
	}
	if response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong token, got %#v", response)
	}
	wsURL := "ws://" + endpoint.Host + ":" + intString(endpoint.Port) + "/v1/ws?token=" + endpoint.AuthToken
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial backend websocket: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{"id": "1", "method": "backend.status", "params": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	status := readRPCResponse(t, conn, "1")
	if !status.OK {
		t.Fatalf("status failed: %#v", status.Error)
	}
	if err := conn.WriteJSON(map[string]any{"id": "2", "method": "session.create", "params": map[string]any{"cwd": root}}); err != nil {
		t.Fatal(err)
	}
	created := readRPCResponse(t, conn, "2")
	if !created.OK {
		t.Fatalf("session.create failed: %#v", created.Error)
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("daemon returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not stop after context cancellation")
	}
}

func TestBackendShutdownCLIStopsDaemon(t *testing.T) {
	root := t.TempDir()
	cfg := config.NewConfigForCWD(root)
	cfg.SessionDir = filepath.Join(root, "sessions")
	endpointPath := filepath.Join(root, "run", "backend.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := startTestDaemon(t, ctx, cfg, endpointPath)
	_ = waitForEndpoint(t, endpointPath)

	if err := backend.RunShutdownCLI([]string{"--endpoint", endpointPath, "--timeout", "3s"}); err != nil {
		t.Fatalf("shutdown cli failed: %v", err)
	}
	if _, err := os.Stat(endpointPath); !os.IsNotExist(err) {
		t.Fatalf("endpoint should be removed after shutdown, got %v", err)
	}
	mustStopDaemon(t, errCh)
}

func TestBackendDaemonSessionsDoNotMixEventsOrTranscript(t *testing.T) {
	root := t.TempDir()
	cfg := config.NewConfigForCWD(root)
	cfg.SessionDir = filepath.Join(root, "sessions")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	endpointPath := filepath.Join(root, "run", "backend.json")
	errCh := startTestDaemon(t, ctx, cfg, endpointPath)
	conn := dialEndpoint(t, waitForEndpoint(t, endpointPath))
	defer conn.Close()

	first := createSession(t, conn, root, "create-1")
	second := createSession(t, conn, root, "create-2")
	if first.SessionID == second.SessionID {
		t.Fatalf("sessions should have unique ids: %s", first.SessionID)
	}
	writeRPC(t, conn, "submit-1", "session.submit", map[string]any{"session_id": first.SessionID, "input": "/help"})
	waitForRPCOK(t, conn, "submit-1")
	writeRPC(t, conn, "submit-2", "session.submit", map[string]any{"session_id": second.SessionID, "input": "/tokens"})
	waitForRPCOK(t, conn, "submit-2")
	waitForSessionDone(t, conn, map[string]bool{first.SessionID: false, second.SessionID: false})

	firstSnapshot := snapshotSession(t, conn, first.SessionID, "snapshot-1")
	secondSnapshot := snapshotSession(t, conn, second.SessionID, "snapshot-2")
	firstText := transcriptText(firstSnapshot)
	secondText := transcriptText(secondSnapshot)
	if !strings.Contains(firstText, "/help") || strings.Contains(firstText, "/tokens") {
		t.Fatalf("first session transcript mixed or missing content:\n%s", firstText)
	}
	if !strings.Contains(secondText, "/tokens") || strings.Contains(secondText, "/help") {
		t.Fatalf("second session transcript mixed or missing content:\n%s", secondText)
	}
	cancel()
	mustStopDaemon(t, errCh)
}

func TestBackendDaemonCreatesTeamTemplate(t *testing.T) {
	root := t.TempDir()
	cfg := config.NewConfigForCWD(root)
	cfg.SessionDir = filepath.Join(root, "sessions")
	cfg.TeamDir = filepath.Join(root, "TEAM")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	endpointPath := filepath.Join(root, "run", "backend.json")
	errCh := startTestDaemon(t, ctx, cfg, endpointPath)
	conn := dialEndpoint(t, waitForEndpoint(t, endpointPath))
	defer conn.Close()

	writeRPC(t, conn, "new-team", "team.create_template", map[string]any{"name": "Data Analysis Team"})
	response := waitForRPCOK(t, conn, "new-team")
	var result luminateam.TeamTemplateResult
	decodeResult(t, response.Result, &result)
	if result.TeamName != "data-analysis-team" || result.AgentCount != 1 {
		t.Fatalf("unexpected team template result: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(result.Path, "team-leader", "skills")); err != nil {
		t.Fatalf("expected team leader skills dir: %v", err)
	}
	writeRPC(t, conn, "team-list", "team.list", map[string]any{})
	listResponse := waitForRPCOK(t, conn, "team-list")
	var teams []luminateam.TeamListItem
	decodeResult(t, listResponse.Result, &teams)
	if len(teams) != 1 || teams[0].Name != "data-analysis-team" {
		t.Fatalf("expected created team in list, got %#v", teams)
	}
	cancel()
	mustStopDaemon(t, errCh)
}

func TestBackendDaemonSameSessionSubmitReturnsBusy(t *testing.T) {
	root := t.TempDir()
	requestSeen := make(chan struct{}, 1)
	release := make(chan struct{})
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requestSeen <- struct{}{}:
		default:
		}
		<-release
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer apiServer.Close()

	cfg := config.NewConfigForCWD(root)
	cfg.SessionDir = filepath.Join(root, "sessions")
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = apiServer.URL
	cfg.APIType = "openai_compatible"
	cfg.APIModel = "test-model"
	config.PinFields(&cfg, "api_key", "api_base_url", "api_type", "api_model")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	endpointPath := filepath.Join(root, "run", "backend.json")
	errCh := startTestDaemon(t, ctx, cfg, endpointPath)
	conn := dialEndpoint(t, waitForEndpoint(t, endpointPath))
	defer conn.Close()

	session := createSession(t, conn, root, "create")
	writeRPC(t, conn, "submit-1", "session.submit", map[string]any{"session_id": session.SessionID, "input": "first request"})
	waitForRPCOK(t, conn, "submit-1")
	select {
	case <-requestSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("fake API did not receive first request")
	}
	writeRPC(t, conn, "submit-2", "session.submit", map[string]any{"session_id": session.SessionID, "input": "second request"})
	response := readRPCResponse(t, conn, "submit-2")
	if response.OK || response.Error == nil || response.Error.Code != "session_busy" {
		t.Fatalf("expected session_busy for concurrent submit, got %#v", response)
	}
	close(release)
	waitForSessionDone(t, conn, map[string]bool{session.SessionID: false})
	cancel()
	mustStopDaemon(t, errCh)
}

func TestBackendDaemonSkillAndResumeRPCReplaceGoTUICommands(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	skillDir := filepath.Join(root, "skills", "reader")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rawSkill := `---
name: Reader
description: Read project files carefully
---
Read carefully: $ARGUMENTS
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(rawSkill), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewConfigForCWD(root)
	cfg.SessionDir = filepath.Join(home, ".Lumina", "sessions")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	endpointPath := filepath.Join(home, "run", "backend.json")
	errCh := startTestDaemon(t, ctx, cfg, endpointPath)
	conn := dialEndpoint(t, waitForEndpoint(t, endpointPath))
	defer conn.Close()

	created := createSession(t, conn, root, "create")
	writeRPC(t, conn, "slash", "slash.list", map[string]any{"session_id": created.SessionID})
	slashResp := readRPCResponse(t, conn, "slash")
	if !slashResp.OK {
		t.Fatalf("slash.list failed: %#v", slashResp.Error)
	}
	var slashPayload struct {
		Items []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"items"`
	}
	decodeResult(t, slashResp.Result, &slashPayload)
	foundHelp := false
	for _, item := range slashPayload.Items {
		if item.Name == "/help" && item.Description != "" {
			foundHelp = true
		}
	}
	if !foundHelp {
		t.Fatalf("expected slash.list to expose lowercase completion items, got %#v", slashPayload.Items)
	}
	writeRPC(t, conn, "skills", "skills.list", map[string]any{"session_id": created.SessionID})
	skillsResp := readRPCResponse(t, conn, "skills")
	if !skillsResp.OK {
		t.Fatalf("skills.list failed: %#v", skillsResp.Error)
	}
	var skills []map[string]any
	decodeResult(t, skillsResp.Result, &skills)
	foundReader := false
	for _, skill := range skills {
		if skill["name"] == "reader" && skill["source"] == "project" {
			foundReader = true
		}
	}
	if !foundReader {
		t.Fatalf("expected project reader skill from RPC, got %#v", skills)
	}
	writeRPC(t, conn, "pick", "skills.pick", map[string]any{"session_id": created.SessionID, "name": "reader"})
	pickResp := readRPCResponse(t, conn, "pick")
	var pick map[string]any
	decodeResult(t, pickResp.Result, &pick)
	if pick["text"] != "/reader " {
		t.Fatalf("skills.pick should return input draft, got %#v", pick)
	}
	if err := session.NewStore(cfg.SessionDir).Save(created.SessionID, []map[string]any{{"role": "user", "content": "hello"}}, 1); err != nil {
		t.Fatal(err)
	}
	writeRPC(t, conn, "list", "session.list", map[string]any{})
	listResp := readRPCResponse(t, conn, "list")
	if !listResp.OK {
		t.Fatalf("session.list failed: %#v", listResp.Error)
	}
	writeRPC(t, conn, "resume", "session.resume", map[string]any{"session_id": created.SessionID, "cwd": root})
	resumeResp := readRPCResponse(t, conn, "resume")
	if !resumeResp.OK {
		t.Fatalf("session.resume failed: %#v", resumeResp.Error)
	}
	cancel()
	mustStopDaemon(t, errCh)
}

func TestBackendDaemonSessionExitStopsWhenNoOtherWebSocketClients(t *testing.T) {
	root := t.TempDir()
	cfg := config.NewConfigForCWD(root)
	cfg.SessionDir = filepath.Join(root, "sessions")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	endpointPath := filepath.Join(root, "run", "backend.json")
	errCh := startTestDaemon(t, ctx, cfg, endpointPath)
	conn := dialEndpoint(t, waitForEndpoint(t, endpointPath))

	created := createSession(t, conn, root, "create")
	writeRPC(t, conn, "exit", "session.exit", map[string]any{"session_id": created.SessionID})
	exitResp := waitForRPCOK(t, conn, "exit")
	var exitPayload map[string]any
	decodeResult(t, exitResp.Result, &exitPayload)
	if exitPayload["shutting_down"] == true || exitPayload["shutdown_after_disconnect"] != true {
		t.Fatalf("single client session.exit should defer shutdown until websocket closes, got %#v", exitPayload)
	}
	writeRPC(t, conn, "status-after-exit", "backend.status", map[string]any{})
	waitForRPCOK(t, conn, "status-after-exit")
	_ = conn.Close()
	mustStopDaemon(t, errCh)
}

func TestBackendDaemonSessionExitKeepsBackendWhenOtherWebSocketClientsRemain(t *testing.T) {
	root := t.TempDir()
	cfg := config.NewConfigForCWD(root)
	cfg.SessionDir = filepath.Join(root, "sessions")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	endpointPath := filepath.Join(root, "run", "backend.json")
	errCh := startTestDaemon(t, ctx, cfg, endpointPath)
	endpoint := waitForEndpoint(t, endpointPath)
	firstConn := dialEndpoint(t, endpoint)
	secondConn := dialEndpoint(t, endpoint)
	defer secondConn.Close()

	first := createSession(t, firstConn, root, "create-1")
	_ = createSession(t, secondConn, root, "create-2")
	writeRPC(t, firstConn, "exit-1", "session.exit", map[string]any{"session_id": first.SessionID})
	exitResp := waitForRPCOK(t, firstConn, "exit-1")
	var exitPayload map[string]any
	decodeResult(t, exitResp.Result, &exitPayload)
	if exitPayload["shutting_down"] == true {
		t.Fatalf("session.exit should keep backend while another websocket is alive, got %#v", exitPayload)
	}
	_ = firstConn.Close()
	writeRPC(t, secondConn, "status", "backend.status", map[string]any{})
	waitForRPCOK(t, secondConn, "status")
	cancel()
	mustStopDaemon(t, errCh)
}

func TestBackendDaemonIdleHeartbeatStopsAfterConsecutiveEmptyChecks(t *testing.T) {
	root := t.TempDir()
	cfg := config.NewConfigForCWD(root)
	cfg.SessionDir = filepath.Join(root, "sessions")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	endpointPath := filepath.Join(root, "run", "backend.json")
	errCh := make(chan error, 1)
	go func() {
		errCh <- backend.Serve(ctx, backend.DaemonOptions{
			Host:              "127.0.0.1",
			Port:              0,
			Config:            cfg,
			EndpointPath:      endpointPath,
			IdleCheckInterval: 25 * time.Millisecond,
			IdleEmptyChecks:   2,
		})
	}()
	_ = waitForEndpoint(t, endpointPath)
	mustStopDaemon(t, errCh)
}

func TestBackendDaemonIdleHeartbeatKeepsAliveWhileClientConnected(t *testing.T) {
	root := t.TempDir()
	cfg := config.NewConfigForCWD(root)
	cfg.SessionDir = filepath.Join(root, "sessions")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	endpointPath := filepath.Join(root, "run", "backend.json")
	errCh := make(chan error, 1)
	go func() {
		errCh <- backend.Serve(ctx, backend.DaemonOptions{
			Host:              "127.0.0.1",
			Port:              0,
			Config:            cfg,
			EndpointPath:      endpointPath,
			IdleCheckInterval: 25 * time.Millisecond,
			IdleEmptyChecks:   2,
		})
	}()
	conn := dialEndpoint(t, waitForEndpoint(t, endpointPath))
	time.Sleep(80 * time.Millisecond)
	writeRPC(t, conn, "status", "backend.status", map[string]any{})
	status := waitForRPCOK(t, conn, "status")
	var payload map[string]any
	decodeResult(t, status.Result, &payload)
	if payload["active_connections"] == float64(0) {
		t.Fatalf("heartbeat should count live websocket connection, got %#v", payload)
	}
	_ = conn.Close()
	mustStopDaemon(t, errCh)
}

func TestBackendDaemonSessionYoloTogglesCurrentSessionState(t *testing.T) {
	root := t.TempDir()
	cfg := config.NewConfigForCWD(root)
	cfg.SessionDir = filepath.Join(root, "sessions")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	endpointPath := filepath.Join(root, "run", "backend.json")
	errCh := startTestDaemon(t, ctx, cfg, endpointPath)
	conn := dialEndpoint(t, waitForEndpoint(t, endpointPath))
	defer conn.Close()

	created := createSession(t, conn, root, "create")
	writeRPC(t, conn, "yolo", "session.yolo", map[string]any{"session_id": created.SessionID})
	yoloResp := waitForRPCOK(t, conn, "yolo")
	var yoloPayload map[string]any
	decodeResult(t, yoloResp.Result, &yoloPayload)
	if yoloPayload["yolo"] != true {
		t.Fatalf("expected yolo enabled, got %#v", yoloPayload)
	}
	snapshot := snapshotSession(t, conn, created.SessionID, "snapshot")
	state := session.NewStore(cfg.SessionDir).LoadState(created.SessionID)
	if state == nil || state.PermissionState == nil || !state.PermissionState.YoloMode {
		t.Fatalf("expected yolo state persisted, snapshot=%#v state=%#v", snapshot, state)
	}
	cancel()
	mustStopDaemon(t, errCh)
}

func TestBackendDaemonSessionCreateCanStartInYoloMode(t *testing.T) {
	root := t.TempDir()
	cfg := config.NewConfigForCWD(root)
	cfg.SessionDir = filepath.Join(root, "sessions")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	endpointPath := filepath.Join(root, "run", "backend.json")
	errCh := startTestDaemon(t, ctx, cfg, endpointPath)
	conn := dialEndpoint(t, waitForEndpoint(t, endpointPath))
	defer conn.Close()

	writeRPC(t, conn, "create-yolo", "session.create", map[string]any{"cwd": root, "yolo": true})
	created := waitForRPCOK(t, conn, "create-yolo")
	var snapshot backend.SessionSnapshot
	decodeResult(t, created.Result, &snapshot)
	state := session.NewStore(cfg.SessionDir).LoadState(snapshot.SessionID)
	if state == nil || state.PermissionState == nil || !state.PermissionState.YoloMode {
		t.Fatalf("expected session.create yolo=true to persist yolo state, snapshot=%#v state=%#v", snapshot, state)
	}

	cancel()
	mustStopDaemon(t, errCh)
}

func waitForEndpoint(t *testing.T, path string) backend.EndpointInfo {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			var endpoint backend.EndpointInfo
			if err := json.Unmarshal(data, &endpoint); err == nil && endpoint.Port > 0 && endpoint.AuthToken != "" {
				return endpoint
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("endpoint file was not written: %s", path)
	return backend.EndpointInfo{}
}

func startTestDaemon(t *testing.T, ctx context.Context, cfg config.Config, endpointPath string) chan error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() {
		errCh <- backend.Serve(ctx, backend.DaemonOptions{
			Host:         "127.0.0.1",
			Port:         0,
			Config:       cfg,
			EndpointPath: endpointPath,
		})
	}()
	return errCh
}

func dialEndpoint(t *testing.T, endpoint backend.EndpointInfo) *websocket.Conn {
	t.Helper()
	wsURL := "ws://" + endpoint.Host + ":" + intString(endpoint.Port) + "/v1/ws?token=" + endpoint.AuthToken
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial backend websocket: %v", err)
	}
	return conn
}

func writeRPC(t *testing.T, conn *websocket.Conn, id, method string, params map[string]any) {
	t.Helper()
	if err := conn.WriteJSON(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		t.Fatal(err)
	}
}

func waitForRPCOK(t *testing.T, conn *websocket.Conn, id string) backend.RPCResponse {
	t.Helper()
	response := readRPCResponse(t, conn, id)
	if !response.OK {
		t.Fatalf("%s failed: %#v", id, response.Error)
	}
	return response
}

func createSession(t *testing.T, conn *websocket.Conn, root, id string) backend.SessionSnapshot {
	t.Helper()
	writeRPC(t, conn, id, "session.create", map[string]any{"cwd": root})
	response := waitForRPCOK(t, conn, id)
	var snapshot backend.SessionSnapshot
	decodeResult(t, response.Result, &snapshot)
	if snapshot.SessionID == "" {
		t.Fatalf("session.create returned empty session id: %#v", snapshot)
	}
	return snapshot
}

func snapshotSession(t *testing.T, conn *websocket.Conn, sessionID, id string) backend.SessionSnapshot {
	t.Helper()
	writeRPC(t, conn, id, "session.snapshot", map[string]any{"session_id": sessionID})
	response := waitForRPCOK(t, conn, id)
	var snapshot backend.SessionSnapshot
	decodeResult(t, response.Result, &snapshot)
	return snapshot
}

func waitForSessionDone(t *testing.T, conn *websocket.Conn, wanted map[string]bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var raw map[string]json.RawMessage
		if err := conn.ReadJSON(&raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["type"]; !ok {
			continue
		}
		var event backend.PushEvent
		data, _ := json.Marshal(raw)
		if err := json.Unmarshal(data, &event); err != nil {
			t.Fatal(err)
		}
		if event.Event == nil {
			continue
		}
		payload, _ := event.Event.(map[string]any)
		if payload["type"] == "session.done" {
			if _, ok := wanted[event.SessionID]; ok {
				wanted[event.SessionID] = true
			}
			allDone := true
			for _, done := range wanted {
				if !done {
					allDone = false
				}
			}
			if allDone {
				return
			}
		}
	}
	t.Fatalf("timed out waiting for session.done events: %#v", wanted)
}

func transcriptText(snapshot backend.SessionSnapshot) string {
	var parts []string
	for _, entry := range snapshot.Frame.TranscriptEntries {
		parts = append(parts, fmt.Sprint(entry["text"]))
	}
	return strings.Join(parts, "\n")
}

func decodeResult(t *testing.T, result any, out any) {
	t.Helper()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatal(err)
	}
}

func mustStopDaemon(t *testing.T, errCh chan error) {
	t.Helper()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("daemon returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not stop after context cancellation")
	}
}

func readRPCResponse(t *testing.T, conn *websocket.Conn, id string) backend.RPCResponse {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var raw map[string]json.RawMessage
		if err := conn.ReadJSON(&raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["type"]; ok {
			continue
		}
		var response backend.RPCResponse
		data, _ := json.Marshal(raw)
		if err := json.Unmarshal(data, &response); err != nil {
			t.Fatal(err)
		}
		if response.ID == id {
			return response
		}
	}
	t.Fatalf("timed out waiting for response %s", id)
	return backend.RPCResponse{}
}

func intString(value int) string {
	return strconv.Itoa(value)
}
