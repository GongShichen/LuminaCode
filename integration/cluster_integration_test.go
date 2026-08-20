//go:build integration

package integration

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"LuminaCode/backend"
	"LuminaCode/cluster"
	"LuminaCode/config"
	"LuminaCode/harness"
	"LuminaCode/memory"
	"LuminaCode/session"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

type integrationServices struct {
	postgresURL string
	redisURL    string
	postgres    *testcontainers.DockerContainer
	redis       *testcontainers.DockerContainer
}

func TestMain(m *testing.M) {
	if os.Getenv("DOCKER_HOST") == "" {
		if output, err := exec.Command("docker", "context", "inspect", "--format", "{{json .Endpoints.docker.Host}}").Output(); err == nil {
			host := strings.Trim(strings.TrimSpace(string(output)), `"`)
			if strings.HasPrefix(host, "unix://") {
				_ = os.Setenv("DOCKER_HOST", host)
				if strings.Contains(host, ".colima/") && os.Getenv("TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE") == "" {
					_ = os.Setenv("TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE", "/var/run/docker.sock")
				}
			}
		}
	}
	os.Exit(m.Run())
}

func startServices(t *testing.T) integrationServices {
	t.Helper()
	ctx := context.Background()
	postgres, err := testcontainers.Run(ctx, "pgvector/pgvector:0.8.6-pg17",
		testcontainers.WithEnv(map[string]string{"POSTGRES_PASSWORD": "postgres", "POSTGRES_DB": "lumina"}),
		testcontainers.WithExposedPorts("5432/tcp"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2)))
	if err != nil {
		t.Fatal(err)
	}
	redis, err := testcontainers.Run(ctx, "redis:8.2-alpine",
		testcontainers.WithExposedPorts("6379/tcp"),
		testcontainers.WithWaitStrategy(wait.ForLog("Ready to accept connections")))
	if err != nil {
		_ = postgres.Terminate(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = redis.Terminate(context.Background())
		_ = postgres.Terminate(context.Background())
	})
	postgresHost, _ := postgres.Host(ctx)
	postgresPort, _ := postgres.MappedPort(ctx, "5432/tcp")
	redisHost, _ := redis.Host(ctx)
	redisPort, _ := redis.MappedPort(ctx, "6379/tcp")
	return integrationServices{postgresURL: fmt.Sprintf("postgres://postgres:postgres@%s:%s/lumina?sslmode=disable",
		postgresHost, postgresPort.Port()), redisURL: fmt.Sprintf("redis://%s:%s/0", redisHost, redisPort.Port()),
		postgres: postgres, redis: redis}
}

func TestRedisLeaseSingleOwnerAndPostgresFence(t *testing.T) {
	services := startServices(t)
	ctx := context.Background()
	runtime, err := cluster.NewRedisRuntime(ctx, services.redisURL, "integration")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	repository, err := session.NewPostgresRepository(ctx, services.postgresURL, "integration")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	const attempts = 100
	var wg sync.WaitGroup
	var mu sync.Mutex
	var leases []cluster.SessionLease
	for index := 0; index < attempts; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			lease, acquireErr := runtime.Acquire(ctx, "tenant-a", "same-session",
				fmt.Sprintf("instance-%d", index), 15*time.Second, 5*time.Second)
			if acquireErr == nil {
				mu.Lock()
				leases = append(leases, lease)
				mu.Unlock()
			} else if !errors.Is(acquireErr, cluster.ErrSessionOwned) {
				t.Errorf("unexpected acquire error: %v", acquireErr)
			}
		}(index)
	}
	wg.Wait()
	if len(leases) != 1 {
		t.Fatalf("owners=%d, want 1", len(leases))
	}
	first := leases[0]
	firstStore, err := repository.OpenRuntime(ctx, "tenant-a", "same-session",
		session.RuntimeOpenOptions{CreateIfMissing: true, FenceToken: first.Owner().FenceToken})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := runtime.Acquire(ctx, "tenant-a", "same-session", "replacement", 15*time.Second, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release(ctx)
	if second.Owner().FenceToken <= first.Owner().FenceToken {
		t.Fatalf("replacement fence=%d, old=%d", second.Owner().FenceToken, first.Owner().FenceToken)
	}
	secondStore, err := repository.OpenRuntime(ctx, "tenant-a", "same-session",
		session.RuntimeOpenOptions{FenceToken: second.Owner().FenceToken})
	if err != nil {
		t.Fatal(err)
	}
	defer secondStore.Journal.Close()
	if _, err := firstStore.Journal.Append(ctx, harness.AnyStreamSeq,
		harness.PendingEvent{Type: harness.EventSessionStatusChanged, Payload: map[string]any{"status": "stale"}}); !errors.Is(err, session.ErrSessionFenceLost) {
		t.Fatalf("stale append error=%v, want ErrSessionFenceLost", err)
	}
	_ = firstStore.Journal.Close()

	otherTenant, err := runtime.Acquire(ctx, "tenant-b", "same-session", "tenant-b-owner", 15*time.Second, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer otherTenant.Release(ctx)
	if _, err := repository.OpenRuntime(ctx, "tenant-b", "same-session",
		session.RuntimeOpenOptions{CreateIfMissing: true, FenceToken: otherTenant.Owner().FenceToken}); err != nil {
		t.Fatal(err)
	}
	crashedRuntime, err := cluster.NewRedisRuntime(ctx, services.redisURL, "integration")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crashedRuntime.Acquire(ctx, "tenant-a", "crash-session", "crashed-owner",
		3*time.Second, time.Second); err != nil {
		t.Fatal(err)
	}
	_ = crashedRuntime.Close() // Simulate a process disappearing without lease release.
	failoverDeadline := time.Now().Add(8 * time.Second)
	for {
		replacement, acquireErr := runtime.Acquire(ctx, "tenant-a", "crash-session", "replacement-owner",
			3*time.Second, time.Second)
		if acquireErr == nil {
			defer replacement.Release(ctx)
			break
		}
		if !errors.Is(acquireErr, cluster.ErrSessionOwned) || time.Now().After(failoverDeadline) {
			t.Fatalf("crashed owner was not replaced before deadline: %v", acquireErr)
		}
		time.Sleep(100 * time.Millisecond)
	}

	localDir := filepath.Join(t.TempDir(), "local-sessions")
	local, err := session.OpenRuntimeJournal(ctx, localDir, "migration-session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.Append(ctx, 0, harness.PendingEvent{Type: harness.EventSessionCreated,
		Payload: map[string]any{"session_id": "migration-session"}}); err != nil {
		t.Fatal(err)
	}
	if err := local.CreateStream(ctx, harness.StreamDescriptor{ID: "team-child", Kind: string(harness.ScopeTeam),
		ParentID: "migration-session", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if _, err := local.Append(ctx, 0, harness.PendingEvent{StreamID: "team-child",
		Type: harness.EventTeamCreated, Payload: map[string]any{"team_session_id": "team-child"}}); err != nil {
		t.Fatal(err)
	}
	blobID, err := local.PutBlob(ctx, "text/plain", []byte("artifact"))
	if err != nil || blobID == "" {
		t.Fatal(err)
	}
	if err := local.SaveCheckpoint(ctx, harness.Checkpoint{StreamID: "migration-session", Projector: "fixture",
		ProjectorVersion: 1, UpToSeq: 2, State: []byte(`{"ok":true}`)}); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveCommandResult(ctx, harness.CommandResult{CommandID: uuid.NewString(),
		SessionID: "migration-session", AcceptedSeq: 2, Result: []byte(`{"accepted":true}`)}); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveConsumerOffset(ctx, "fixture", 2); err != nil {
		t.Fatal(err)
	}
	migrationLease, err := runtime.Acquire(ctx, "tenant-a", "migration-session", "migration", 15*time.Second, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	report, err := repository.ImportLocalRuntime(ctx, "tenant-a", "migration-session", t.TempDir(),
		"migration-fixture", migrationLease.Owner().FenceToken, local, false)
	_ = local.Close()
	_ = migrationLease.Release(ctx)
	if err != nil || report.EventCount != 2 || report.StreamCount != 2 || report.BlobCount != 1 {
		t.Fatalf("migration report=%#v err=%v", report, err)
	}
	exportDir := filepath.Join(t.TempDir(), "exported")
	exported, err := repository.ExportRuntimeToLocal(ctx, "tenant-a", "migration-session", exportDir)
	if err != nil || exported.Checksum != report.Checksum || exported.EventCount != report.EventCount {
		t.Fatalf("import report=%#v export report=%#v err=%v", report, exported, err)
	}
	exportedJournal, err := session.OpenRuntimeJournal(ctx, exportDir, "migration-session")
	if err != nil {
		t.Fatal(err)
	}
	defer exportedJournal.Close()
	if events, err := exportedJournal.Load(ctx, 0, 10); err != nil || len(events) != 2 {
		t.Fatalf("exported events=%d err=%v", len(events), err)
	}
}

type deterministicRetrievalEncoder struct{}

func (deterministicRetrievalEncoder) Model() string         { return "integration-bge" }
func (deterministicRetrievalEncoder) Revision() string      { return "integration-bge-v1" }
func (deterministicRetrievalEncoder) TokenizerHash() string { return "integration-tokenizer" }
func (deterministicRetrievalEncoder) Split(text string, _, _ int) ([]string, error) {
	return []string{text}, nil
}
func (deterministicRetrievalEncoder) Encode(_ context.Context, texts []string,
	_ memory.RetrievalEncodingKind) ([]memory.RetrievalEncoding, error) {
	result := make([]memory.RetrievalEncoding, len(texts))
	for index, text := range texts {
		dense := make([]float32, 1024)
		for i, value := range []byte(text) {
			dense[i%len(dense)] += float32(value) / 255
		}
		result[index] = memory.RetrievalEncoding{Dense: dense,
			Sparse: map[int64]float32{int64(len(text)): 1}}
	}
	return result, nil
}

type deterministicVectorizer struct{ deterministicRetrievalEncoder }

func (deterministicVectorizer) Dimensions() int { return 1024 }
func (v deterministicVectorizer) Embed(ctx context.Context, texts []string,
	_ memory.VectorPurpose) ([][]float32, error) {
	encoded, err := v.Encode(ctx, texts, memory.RetrievalDocument)
	result := make([][]float32, len(encoded))
	for index := range encoded {
		result[index] = encoded[index].Dense
	}
	return result, err
}

func TestPostgresFabricTenantIsolationAndRetrieval(t *testing.T) {
	services := startServices(t)
	ctx := context.Background()
	options := memory.DefaultFabricOptions(t.TempDir())
	options.StartWorkers = false
	options.RemoteProcessing = memory.RemoteProcessingOff
	options.RetrievalEncoder = deterministicRetrievalEncoder{}
	options.Vectorizer = deterministicVectorizer{}
	options.Reranker = nil
	open := func(tenant string) *memory.PostgresFabric {
		fabric, err := memory.OpenPostgresFabric(ctx, memory.PostgresFabricOptions{FabricOptions: options,
			PostgresURL: services.postgresURL, ClusterID: "integration", TenantID: tenant, ProjectID: "project"})
		if err != nil {
			t.Fatal(err)
		}
		return fabric
	}
	first := open("tenant-a")
	defer first.Close()
	second := open("tenant-b")
	defer second.Close()
	event := memory.RawEvent{ID: "shared-event", Space: "project", ContextID: "context-1",
		Actor: "user", Content: "The observatory launch window is Tuesday morning.",
		OccurredAt: time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC), SourceRef: "source-a"}
	if _, err := first.AppendEvents(ctx, []memory.RawEvent{event},
		memory.IngestOptions{SemanticPolicy: memory.SemanticDurableOnly}); err != nil {
		t.Fatal(err)
	}
	result, err := first.Search(ctx, memory.SearchRequest{Space: "project", Query: "observatory Tuesday",
		MaxEvidence: 10, MaxContextTokens: 2000, IncludeDiagnostics: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Evidence) == 0 || result.Evidence[0].SourceEventIDs[0] != event.ID {
		t.Fatalf("unexpected search result: %#v", result)
	}
	isolated, err := second.Search(ctx, memory.SearchRequest{Space: "project", Query: "observatory Tuesday"})
	if err != nil {
		t.Fatal(err)
	}
	if len(isolated.Evidence) != 0 {
		t.Fatalf("tenant-b saw tenant-a evidence: %#v", isolated.Evidence)
	}
	commit, err := first.Remember(ctx, memory.MemoryRequest{Space: "project", ContextID: "context-1",
		SourceEventIDs: []string{event.ID}, Drafts: []memory.MemoryDraft{{Kind: memory.NodeClaim,
			ClaimType: memory.ClaimFact, Statement: "The launch window is Tuesday morning.",
			Subject: "observatory launch", Facet: memory.FacetState, AttributeKey: "window",
			Value: memory.ClaimValue{Kind: memory.ValueText, Text: "Tuesday morning"},
			Sources: []memory.SourceSpan{{EventID: event.ID, StartRune: 0,
				EndRune: len([]rune(event.Content)), Role: "support"}}}}})
	if err != nil || len(commit.MemoryIDs) != 1 {
		t.Fatalf("remember=%#v err=%v", commit, err)
	}
	if err := first.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	semanticResult, err := first.Search(ctx, memory.SearchRequest{Space: "project",
		Query: "launch window Tuesday morning", MaxEvidence: 10, MaxContextTokens: 2000})
	if err != nil || len(semanticResult.CurrentView) == 0 || semanticResult.CurrentView[0].ID != commit.MemoryIDs[0] {
		t.Fatalf("semantic CurrentView=%#v err=%v", semanticResult.CurrentView, err)
	}
	correctionEvent := event
	correctionEvent.ID = "corrected-event"
	correctionEvent.Content = "Correction: the observatory launch window is Wednesday morning."
	correctionEvent.OccurredAt = time.Date(2026, 8, 20, 1, 0, 0, 0, time.UTC)
	correctionEvent.SourceRef = "source-correction"
	if _, err := first.AppendEvents(ctx, []memory.RawEvent{correctionEvent},
		memory.IngestOptions{SemanticPolicy: memory.SemanticDurableOnly}); err != nil {
		t.Fatal(err)
	}
	correction, err := first.Remember(ctx, memory.MemoryRequest{Space: "project", ContextID: "context-1",
		SourceEventIDs: []string{correctionEvent.ID}, Mode: memory.WriteCorrection,
		Drafts: []memory.MemoryDraft{{Kind: memory.NodeClaim, ClaimType: memory.ClaimFact,
			Statement: "The launch window is Wednesday morning.", Subject: "observatory launch",
			Facet: memory.FacetState, AttributeKey: "window",
			Value:        memory.ClaimValue{Kind: memory.ValueText, Text: "Wednesday morning"},
			EvidenceMode: memory.EvidenceObserved, Sources: []memory.SourceSpan{{EventID: correctionEvent.ID,
				StartRune: 0, EndRune: len([]rune(correctionEvent.Content)), Role: "support"}}}}})
	if err != nil || len(correction.ResolutionIDs) != 1 {
		t.Fatalf("correction=%#v err=%v", correction, err)
	}
	snapshot, err := first.ExportDurableSnapshot(ctx, "project")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Events) != 2 || len(snapshot.Nodes) != 2 || len(snapshot.Conflicts) != 1 ||
		len(snapshot.Resolutions) != 1 || snapshot.Checksum == "" {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
	migrated := open("tenant-migrated")
	defer migrated.Close()
	report, err := migrated.ImportDurableSnapshot(ctx, snapshot, "memory-migration-fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrated.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	verified, err := migrated.ExportDurableSnapshot(ctx, "project")
	if err != nil || verified.Checksum != snapshot.Checksum || report.Checksum != snapshot.Checksum {
		t.Fatalf("migrated snapshot checksum=%s report=%#v err=%v", verified.Checksum, report, err)
	}
	sqliteOptions := options
	sqliteOptions.Dir = filepath.Join(t.TempDir(), "sqlite-fabric")
	sqlite, err := memory.OpenFabric(ctx, sqliteOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlite.Close()
	sqliteEvent := event
	sqliteEvent.ID = "sqlite-event"
	sqliteEvent.ContextID = "sqlite-context"
	if _, err := sqlite.AppendEvents(ctx, []memory.RawEvent{sqliteEvent},
		memory.IngestOptions{SemanticPolicy: memory.SemanticDurableOnly}); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.Remember(ctx, memory.MemoryRequest{Space: "project", ContextID: sqliteEvent.ContextID,
		SourceEventIDs: []string{sqliteEvent.ID}, Drafts: []memory.MemoryDraft{{Kind: memory.NodeClaim,
			ClaimType: memory.ClaimFact, Statement: "The migrated launch window is Tuesday morning.",
			Subject: "migrated observatory", Facet: memory.FacetState, AttributeKey: "window",
			Value: memory.ClaimValue{Kind: memory.ValueText, Text: "Tuesday morning"},
			Sources: []memory.SourceSpan{{EventID: sqliteEvent.ID, StartRune: 0,
				EndRune: len([]rune(sqliteEvent.Content)), Role: "support"}}}}}); err != nil {
		t.Fatal(err)
	}
	sqliteSnapshot, err := sqlite.ExportDurableSnapshot(ctx, "project")
	if err != nil {
		t.Fatal(err)
	}
	fromSQLite := open("tenant-from-sqlite")
	defer fromSQLite.Close()
	if _, err := fromSQLite.ImportDurableSnapshot(ctx, sqliteSnapshot, "sqlite-memory-migration", false); err != nil {
		t.Fatal(err)
	}
	if err := fromSQLite.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	fromSQLiteSnapshot, err := fromSQLite.ExportDurableSnapshot(ctx, "project")
	if err != nil || fromSQLiteSnapshot.Checksum != sqliteSnapshot.Checksum {
		t.Fatalf("SQLite migration checksum source=%s target=%s err=%v",
			sqliteSnapshot.Checksum, fromSQLiteSnapshot.Checksum, err)
	}
}

func TestTwoDaemonInstancesForwardAndFailOver(t *testing.T) {
	services := startServices(t)
	root := t.TempDir()
	t.Setenv("LUMINA_APP_ROOT", filepath.Join(root, "app-root"))
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	publicKeyPath := filepath.Join(root, "jwt-public.pem")
	if err := os.WriteFile(publicKeyPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: encoded}), 0o600); err != nil {
		t.Fatal(err)
	}
	newConfig := func(instanceID string) config.Config {
		cfg := config.NewConfigForCWD(root)
		cfg.SessionRuntimeBackend = "cluster"
		cfg.MemoryFabricStore = "postgres"
		cfg.ClusterID = "daemon-integration"
		cfg.InstanceID = instanceID
		cfg.ClusterListenAddr = "127.0.0.1:0"
		cfg.RedisURL = services.redisURL
		cfg.PostgresURL = services.postgresURL
		cfg.ClusterLeaseTTLSeconds = 6
		cfg.ClusterLeaseRenewSeconds = 2
		cfg.ClusterRPCTimeoutSeconds = 5
		cfg.ClusterShutdownGraceSeconds = 3
		cfg.JWTIssuer = "integration-issuer"
		cfg.JWTAudience = "integration-audience"
		cfg.JWTPublicKeyFile = publicKeyPath
		cfg.JWTJWKSURL = ""
		cfg.LongTermMemoryEnabled = false
		cfg.SkillsEnabled = false
		cfg.MCPEnabled = false
		cfg.ClusterConfigErrors = nil
		return cfg
	}
	tokenFor := func(tenant string) string {
		claims := jwt.MapClaims{"iss": "integration-issuer", "aud": "integration-audience",
			"sub": "integration-user", "tenant_id": tenant,
			"scope": "lumina:admin lumina:session:read lumina:session:write",
			"iat":   time.Now().Add(-time.Minute).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(),
			"exp": time.Now().Add(time.Hour).Unix(), "jti": uuid.NewString()}
		token, signErr := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(private)
		if signErr != nil {
			t.Fatal(signErr)
		}
		return token
	}

	ctxOne, cancelOne := context.WithCancel(context.Background())
	ctxTwo, cancelTwo := context.WithCancel(context.Background())
	endpointOne := filepath.Join(root, "backend-one.json")
	endpointTwo := filepath.Join(root, "backend-two.json")
	doneOne := make(chan error, 1)
	doneTwo := make(chan error, 1)
	go func() {
		doneOne <- backend.Serve(ctxOne, backend.DaemonOptions{Config: newConfig("instance-one"),
			EndpointPath: endpointOne})
	}()
	go func() {
		doneTwo <- backend.Serve(ctxTwo, backend.DaemonOptions{Config: newConfig("instance-two"),
			EndpointPath: endpointTwo})
	}()
	t.Cleanup(func() {
		cancelOne()
		cancelTwo()
		select {
		case <-doneOne:
		case <-time.After(5 * time.Second):
		}
		select {
		case <-doneTwo:
		case <-time.After(5 * time.Second):
		}
	})
	firstEndpoint := waitEndpoint(t, endpointOne)
	secondEndpoint := waitEndpoint(t, endpointTwo)
	first := dialCluster(t, firstEndpoint, tokenFor("tenant-a"))
	defer first.Close()
	second := dialCluster(t, secondEndpoint, tokenFor("tenant-a"))
	defer second.Close()

	createID := uuid.NewString()
	created := clusterRPC(t, first, createID, "session.create", map[string]any{"cwd": root})
	var snapshot backend.SessionSnapshot
	decodeRPCResult(t, created.Result, &snapshot)
	if snapshot.SessionID != createID {
		t.Fatalf("created session=%s, want deterministic UUID %s", snapshot.SessionID, createID)
	}
	remote := clusterRPC(t, second, uuid.NewString(), "session.snapshot",
		map[string]any{"session_id": snapshot.SessionID})
	var remoteSnapshot backend.SessionSnapshot
	decodeRPCResult(t, remote.Result, &remoteSnapshot)
	if remoteSnapshot.SessionID != snapshot.SessionID {
		t.Fatalf("remote gateway returned session %s", remoteSnapshot.SessionID)
	}
	mutationID := uuid.NewString()
	firstToggle := clusterRPC(t, second, mutationID, "session.yolo",
		map[string]any{"session_id": snapshot.SessionID})
	secondToggle := clusterRPC(t, second, mutationID, "session.yolo",
		map[string]any{"session_id": snapshot.SessionID})
	firstJSON, _ := json.Marshal(firstToggle.Result)
	secondJSON, _ := json.Marshal(secondToggle.Result)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("same UUID was executed twice: first=%s second=%s", firstJSON, secondJSON)
	}

	cancelOne()
	select {
	case err := <-doneOne:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("owner instance did not stop")
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		requestID := uuid.NewString()
		if response := tryClusterRPC(second, requestID, "session.snapshot",
			map[string]any{"session_id": snapshot.SessionID}); response.OK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replacement instance did not take ownership within 20 seconds")
		}
		time.Sleep(250 * time.Millisecond)
	}

	otherTenant := dialCluster(t, secondEndpoint, tokenFor("tenant-b"))
	defer otherTenant.Close()
	response := tryClusterRPC(otherTenant, uuid.NewString(), "session.resume",
		map[string]any{"session_id": snapshot.SessionID, "cwd": root})
	if response.OK {
		t.Fatal("tenant-b resumed tenant-a session")
	}
}

func waitEndpoint(t *testing.T, path string) backend.EndpointInfo {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			var endpoint backend.EndpointInfo
			if json.Unmarshal(data, &endpoint) == nil && endpoint.Port > 0 {
				return endpoint
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("endpoint was not written: %s", path)
	return backend.EndpointInfo{}
}

func dialCluster(t *testing.T, endpoint backend.EndpointInfo, token string) *websocket.Conn {
	t.Helper()
	headers := http.Header{"Authorization": []string{"Bearer " + token}}
	connection, _, err := websocket.DefaultDialer.Dial(
		fmt.Sprintf("ws://%s:%d/v1/ws", endpoint.Host, endpoint.Port), headers)
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

func clusterRPC(t *testing.T, connection *websocket.Conn, id, method string, params any) backend.RPCResponse {
	t.Helper()
	response := tryClusterRPC(connection, id, method, params)
	if !response.OK {
		t.Fatalf("%s failed: %#v", method, response.Error)
	}
	return response
}

func tryClusterRPC(connection *websocket.Conn, id, method string, params any) backend.RPCResponse {
	_ = connection.WriteJSON(map[string]any{"id": id, "method": method, "params": params})
	for {
		var raw map[string]json.RawMessage
		if connection.ReadJSON(&raw) != nil {
			return backend.RPCResponse{ID: id, Error: &backend.RPCError{Code: "read_failed", Message: "connection closed"}}
		}
		var responseID string
		_ = json.Unmarshal(raw["id"], &responseID)
		if responseID != id {
			continue
		}
		encoded, _ := json.Marshal(raw)
		var response backend.RPCResponse
		_ = json.Unmarshal(encoded, &response)
		return response
	}
}

func decodeRPCResult(t *testing.T, input any, output any) {
	t.Helper()
	encoded, err := json.Marshal(input)
	if err != nil || json.Unmarshal(encoded, output) != nil {
		t.Fatalf("decode RPC result: %v", err)
	}
}
