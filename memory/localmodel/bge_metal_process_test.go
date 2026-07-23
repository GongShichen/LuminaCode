//go:build cgo

package localmodel

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestBGEMetalClientRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	modelDir := writeBGEMetalClientFixture(t, `#!/bin/sh
read request
printf '%s\n' '{"id":1,"embeddings":[{"dense":[0.25,0.75],"sparse":[{"token_id":7,"position":1,"weight":0.4},{"token_id":7,"position":2,"weight":0.8}],"multi":[{"token_id":7,"position":1,"weight":0.4,"values":[0.5,0.5]}]}],"error":""}'
`)
	client, err := newBGEMetalClient(modelDir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	encoded, err := client.Encode(context.Background(), []bgeInput{{
		ids:         []int64{0, 7, 2},
		attention:   []int64{1, 1, 1},
		specialMask: []int{1, 0, 1},
	}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != 1 || len(encoded[0].Dense) != 2 || encoded[0].Sparse[7] != 0.8 ||
		len(encoded[0].Multi) != 1 || encoded[0].Multi[0].TokenID != 7 {
		t.Fatalf("unexpected Metal encoding: %+v", encoded)
	}
}

func TestBGEMetalClientHonorsContextCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	modelDir := writeBGEMetalClientFixture(t, `#!/bin/sh
read request
exec sleep 30
`)
	client, err := newBGEMetalClient(modelDir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = client.Encode(ctx, []bgeInput{{
		ids:         []int64{0, 7, 2},
		attention:   []int64{1, 1, 1},
		specialMask: []int{1, 0, 1},
	}}, false)
	if err == nil {
		t.Fatal("Metal encoding ignored cancellation")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Metal cancellation took %s", elapsed)
	}
}

func writeBGEMetalClientFixture(t *testing.T, script string) string {
	t.Helper()
	root := t.TempDir()
	for _, path := range []string{
		filepath.Join(root, "metal", "config.json"),
		filepath.Join(root, "metal", "model.safetensors"),
		filepath.Join(root, "metal", "model.safetensors.index.json"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(root, "runtime", "bin", "lumina-bge-metal")
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	metallib := filepath.Join(root, "runtime", "bin", "mlx-swift_Cmlx.bundle",
		"Contents", "Resources", "default.metallib")
	if err := os.MkdirAll(filepath.Dir(metallib), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metallib, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}
