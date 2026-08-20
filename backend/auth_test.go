package backend

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"LuminaCode/config"

	"github.com/golang-jwt/jwt/v5"
)

func TestJWTAuthenticatorValidatesClaimsAlgorithmsAndScopes(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "jwt-public.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: encoded}), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{SessionRuntimeBackend: "cluster", MemoryFabricStore: "postgres",
		ClusterID: "test", InstanceID: "instance", RedisURL: "redis://unused", PostgresURL: "postgres://unused",
		ClusterLeaseTTLSeconds: 15, ClusterLeaseRenewSeconds: 5, ClusterRPCTimeoutSeconds: 30,
		ClusterShutdownGraceSeconds: 30, JWTIssuer: "issuer", JWTAudience: "audience",
		JWTPublicKeyFile: path, JWTTenantClaim: "tenant_id", JWTScopeClaim: "scope",
		JWTClockLeewaySeconds: 30}
	authenticator, err := NewJWTAuthenticator(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claims := jwt.MapClaims{"iss": "issuer", "aud": "audience", "sub": "subject", "tenant_id": "tenant-a",
		"scope": []string{"lumina:session:read", "lumina:session:write"}, "jti": "token-id",
		"nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix()}
	token, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(private)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := authenticator.Authenticate(context.Background(), "Bearer "+token)
	if err != nil {
		t.Fatal(err)
	}
	if principal.TenantID != "tenant-a" || principal.Subject != "subject" || principal.TokenID != "token-id" ||
		!principal.HasScope("lumina:session:write") {
		t.Fatalf("unexpected principal: %#v", principal)
	}
	badClaims := claims
	badClaims["tenant_id"] = ""
	badToken, _ := jwt.NewWithClaims(jwt.SigningMethodEdDSA, badClaims).SignedString(private)
	if _, err := authenticator.Authenticate(context.Background(), "Bearer "+badToken); err == nil {
		t.Fatal("JWT without tenant was accepted")
	}
	hsToken, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("secret"))
	if _, err := authenticator.Authenticate(context.Background(), "Bearer "+hsToken); err == nil {
		t.Fatal("HS256 JWT was accepted")
	}
	if authorizeRPC(principal, "backend.shutdown") == nil {
		t.Fatal("non-admin principal was allowed to shut down an instance")
	}
}
