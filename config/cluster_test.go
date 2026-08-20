package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClusterConfigFailsClosedAndProjectDefaultsCannotSetTrustRoots(t *testing.T) {
	cfg := Config{SessionRuntimeBackend: "cluster", MemoryFabricStore: "sqlite",
		ClusterLeaseTTLSeconds: 15, ClusterLeaseRenewSeconds: 5, ClusterRPCTimeoutSeconds: 30,
		ClusterShutdownGraceSeconds: 30, JWTClockLeewaySeconds: 30}
	if err := cfg.ValidateClusterConfig(); err == nil {
		t.Fatal("incomplete cluster configuration was accepted")
	}
	path := filepath.Join(t.TempDir(), "project-defaults.json")
	if err := os.WriteFile(path, []byte(`{
  "session_runtime_backend": "cluster",
  "memory_fabric_store": "postgres",
  "redis_url": "redis://project-secret",
  "postgres_url": "postgres://project-secret",
  "jwt_issuer": "project-issuer",
  "jwt_jwks_url": "https://project.invalid/jwks"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	base := Config{SessionRuntimeBackend: "local", MemoryFabricStore: "sqlite"}
	applyLuminaDefaults(&base, path, t.TempDir(), "", false)
	if base.SessionRuntimeBackend != "local" || base.MemoryFabricStore != "sqlite" ||
		base.RedisURL != "" || base.PostgresURL != "" || base.JWTIssuer != "" || base.JWTJWKSURL != "" {
		t.Fatalf("project defaults changed cluster trust configuration: %#v", base)
	}
}
