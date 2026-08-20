package backend

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"LuminaCode/cluster"
	"LuminaCode/config"

	"github.com/golang-jwt/jwt/v5"
)

type principalContextKey struct{}

type JWTAuthenticator struct {
	issuer       string
	audience     string
	tenantClaim  string
	scopeClaim   string
	leeway       time.Duration
	jwksURL      string
	httpClient   *http.Client
	staticKey    crypto.PublicKey
	mu           sync.RWMutex
	keys         map[string]crypto.PublicKey
	refreshedAt  time.Time
	refreshEvery time.Duration
	maxStale     time.Duration
}

func NewJWTAuthenticator(ctx context.Context, cfg config.Config) (*JWTAuthenticator, error) {
	if !cfg.UsesClusterRuntime() {
		return nil, nil
	}
	if err := cfg.ValidateClusterConfig(); err != nil {
		return nil, err
	}
	authenticator := &JWTAuthenticator{
		issuer: cfg.JWTIssuer, audience: cfg.JWTAudience,
		tenantClaim: cfg.JWTTenantClaim, scopeClaim: cfg.JWTScopeClaim,
		leeway:  time.Duration(cfg.JWTClockLeewaySeconds) * time.Second,
		jwksURL: strings.TrimSpace(cfg.JWTJWKSURL), httpClient: &http.Client{Timeout: 10 * time.Second},
		keys: map[string]crypto.PublicKey{}, refreshEvery: 15 * time.Minute, maxStale: time.Hour,
	}
	if path := strings.TrimSpace(cfg.JWTPublicKeyFile); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read JWT public key: %w", err)
		}
		key, err := parsePublicKeyPEM(data)
		if err != nil {
			return nil, err
		}
		authenticator.staticKey = key
	}
	if authenticator.jwksURL != "" {
		if err := authenticator.refresh(ctx); err != nil {
			return nil, err
		}
	}
	return authenticator, nil
}

func (a *JWTAuthenticator) Middleware(next http.Handler) http.Handler {
	if a == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		principal, err := a.Authenticate(r.Context(), r.Header.Get("Authorization"))
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal)))
	})
}

func (a *JWTAuthenticator) Authenticate(ctx context.Context, authorization string) (cluster.Principal, error) {
	prefix := "Bearer "
	if !strings.HasPrefix(authorization, prefix) {
		return cluster.Principal{}, errors.New("bearer token is required")
	}
	encoded := strings.TrimSpace(strings.TrimPrefix(authorization, prefix))
	parser := jwt.NewParser(jwt.WithValidMethods([]string{"RS256", "ES256", "EdDSA"}),
		jwt.WithIssuer(a.issuer), jwt.WithAudience(a.audience), jwt.WithLeeway(a.leeway), jwt.WithExpirationRequired())
	token, err := parser.Parse(encoded, func(token *jwt.Token) (any, error) {
		if a.staticKey != nil {
			return a.staticKey, nil
		}
		kid, _ := token.Header["kid"].(string)
		return a.key(ctx, kid)
	})
	if err != nil || !token.Valid {
		return cluster.Principal{}, errors.New("invalid JWT")
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return cluster.Principal{}, errors.New("invalid JWT claims")
	}
	subject, _ := claims.GetSubject()
	if strings.TrimSpace(subject) == "" {
		return cluster.Principal{}, errors.New("JWT subject is required")
	}
	tenantID := claimString(claims[a.tenantClaim])
	if tenantID == "" {
		return cluster.Principal{}, errors.New("JWT tenant is required")
	}
	principal := cluster.Principal{TenantID: tenantID, Subject: subject,
		TokenID: claimString(claims["jti"]), Scopes: map[string]struct{}{}}
	for _, scope := range claimStrings(claims[a.scopeClaim]) {
		principal.Scopes[scope] = struct{}{}
	}
	return principal, nil
}

func PrincipalFromContext(ctx context.Context) cluster.Principal {
	if principal, ok := ctx.Value(principalContextKey{}).(cluster.Principal); ok {
		return principal
	}
	return cluster.LocalPrincipal()
}

func authorizeRPC(principal cluster.Principal, method string) *RPCError {
	required := "lumina:session:read"
	switch {
	case method == "backend.shutdown" || method == "cluster.instance.drain" || method == "storage.cleanup":
		required = "lumina:admin"
	case method == "backend.status" || method == "session.list" || method == "session.snapshot" ||
		method == "session.events" || method == "session.tokens" || method == "runtime.describe" ||
		method == "storage.status" || method == "team.list" || method == "team.snapshot" ||
		method == "team.status" || method == "team.artifacts" || method == "team.timeline" ||
		method == "team.dialogue" || method == "team.summary" || method == "team.detail" ||
		method == "memory.search" || method == "memory.doctor" || method == "slash.list" ||
		method == "skills.list" || method == "mcp.list":
		required = "lumina:session:read"
	default:
		required = "lumina:session:write"
	}
	if principal.HasScope(required) || principal.HasScope("lumina:admin") {
		return nil
	}
	return &RPCError{Code: "forbidden", Message: "missing scope " + required}
}

func (a *JWTAuthenticator) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	a.mu.RLock()
	key := a.keys[kid]
	refreshed := a.refreshedAt
	a.mu.RUnlock()
	if key != nil && time.Since(refreshed) < a.refreshEvery {
		return key, nil
	}
	if err := a.refresh(ctx); err != nil {
		if key != nil && time.Since(refreshed) <= a.maxStale {
			return key, nil
		}
		return nil, err
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	key = a.keys[kid]
	if key == nil {
		return nil, fmt.Errorf("JWT key %q not found", kid)
	}
	return key, nil
}

func (a *JWTAuthenticator) refresh(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.jwksURL, nil)
	if err != nil {
		return err
	}
	response, err := a.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("fetch JWKS: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch JWKS: HTTP %d", response.StatusCode)
	}
	var document struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		return err
	}
	keys := make(map[string]crypto.PublicKey, len(document.Keys))
	for _, raw := range document.Keys {
		kid, key, err := parseJWK(raw)
		if err != nil {
			return err
		}
		keys[kid] = key
	}
	if len(keys) == 0 {
		return errors.New("JWKS contains no supported keys")
	}
	a.mu.Lock()
	a.keys = keys
	a.refreshedAt = time.Now()
	a.mu.Unlock()
	return nil
}

func parseJWK(raw json.RawMessage) (string, crypto.PublicKey, error) {
	var key struct {
		KID string `json:"kid"`
		KTY string `json:"kty"`
		CRV string `json:"crv"`
		N   string `json:"n"`
		E   string `json:"e"`
		X   string `json:"x"`
		Y   string `json:"y"`
	}
	if err := json.Unmarshal(raw, &key); err != nil {
		return "", nil, err
	}
	if key.KID == "" {
		return "", nil, errors.New("JWKS key is missing kid")
	}
	decode := func(value string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(value) }
	switch key.KTY {
	case "RSA":
		n, err := decode(key.N)
		if err != nil {
			return "", nil, err
		}
		e, err := decode(key.E)
		if err != nil || len(e) == 0 || len(e) > 4 {
			return "", nil, errors.New("invalid RSA exponent")
		}
		exponent := 0
		for _, value := range e {
			exponent = exponent<<8 | int(value)
		}
		return key.KID, &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exponent}, nil
	case "EC":
		if key.CRV != "P-256" {
			return "", nil, fmt.Errorf("unsupported EC curve %q", key.CRV)
		}
		x, err := decode(key.X)
		if err != nil {
			return "", nil, err
		}
		y, err := decode(key.Y)
		if err != nil {
			return "", nil, err
		}
		return key.KID, &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}, nil
	case "OKP":
		if key.CRV != "Ed25519" {
			return "", nil, fmt.Errorf("unsupported OKP curve %q", key.CRV)
		}
		x, err := decode(key.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return "", nil, errors.New("invalid Ed25519 key")
		}
		return key.KID, ed25519.PublicKey(x), nil
	default:
		return "", nil, fmt.Errorf("unsupported JWK type %q", key.KTY)
	}
}

func parsePublicKeyPEM(data []byte) (crypto.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("invalid JWT public key PEM")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		if rsaKey, rsaErr := x509.ParsePKCS1PublicKey(block.Bytes); rsaErr == nil {
			return rsaKey, nil
		}
		return nil, err
	}
	switch key.(type) {
	case *rsa.PublicKey, *ecdsa.PublicKey, ed25519.PublicKey:
		return key, nil
	default:
		return nil, fmt.Errorf("unsupported JWT public key %T", key)
	}
}

func claimString(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

func claimStrings(value any) []string {
	switch typed := value.(type) {
	case string:
		return strings.Fields(typed)
	case []any:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := claimString(item); text != "" {
				result = append(result, text)
			}
		}
		return result
	case []string:
		return typed
	default:
		return nil
	}
}
