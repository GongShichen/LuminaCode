package agent

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"LuminaCode/memory"
)

const fabricVectorArtifactContract = "local-content-embedding"

var fabricVectorArtifactLocks sync.Map

func (v fabricVectorizer) embedContentWithArtifacts(ctx context.Context, texts []string,
	purpose memory.VectorPurpose) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if v.encoder == nil || v.encoder.Dimensions() <= 0 {
		return nil, fmt.Errorf("memory embedding artifact cache has no usable embedder")
	}
	results := make([][]float32, len(texts))
	keyIndices := make(map[string][]int, len(texts))
	keyText := make(map[string]string, len(texts))
	keys := make([]string, 0, len(texts))
	for index, text := range texts {
		key := fabricVectorArtifactKey(v.encoder.Model(), v.encoder.Dimensions(), purpose, text)
		if _, exists := keyIndices[key]; !exists {
			keys = append(keys, key)
			keyText[key] = text
		}
		keyIndices[key] = append(keyIndices[key], index)
	}
	missing := v.loadVectorArtifacts(keys, keyIndices, results)
	if len(missing) == 0 {
		return results, nil
	}

	lockKey := filepath.Clean(v.artifactDir) + "\x00" + v.encoder.Model()
	rawLock, _ := fabricVectorArtifactLocks.LoadOrStore(lockKey, &sync.Mutex{})
	lock := rawLock.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	// A parallel case may have filled the same artifacts while this request
	// waited for the shared local model. Recheck before running inference.
	missing = v.loadVectorArtifacts(missing, keyIndices, results)
	if len(missing) == 0 {
		return results, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	missingTexts := make([]string, len(missing))
	for index, key := range missing {
		missingTexts[index] = keyText[key]
	}
	vectors, err := v.encoder.EmbedDense(ctx, missingTexts, purpose)
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(missing) {
		return nil, fmt.Errorf("memory embedder returned %d vectors for %d uncached texts", len(vectors), len(missing))
	}
	for index, key := range missing {
		vector := vectors[index]
		if err := validateFabricVector(vector, v.encoder.Dimensions()); err != nil {
			return nil, err
		}
		for _, resultIndex := range keyIndices[key] {
			results[resultIndex] = append([]float32(nil), vector...)
		}
		// The index remains correct if the optional cache cannot be written.
		_ = writeFabricVectorArtifact(v.vectorArtifactPath(key), vector)
	}
	return results, nil
}

func (v fabricVectorizer) loadVectorArtifacts(keys []string, keyIndices map[string][]int,
	results [][]float32) []string {
	missing := make([]string, 0, len(keys))
	for _, key := range keys {
		vector, err := readFabricVectorArtifact(v.vectorArtifactPath(key), v.encoder.Dimensions())
		if err != nil {
			missing = append(missing, key)
			continue
		}
		for _, index := range keyIndices[key] {
			results[index] = append([]float32(nil), vector...)
		}
	}
	return missing
}

func (v fabricVectorizer) vectorArtifactPath(key string) string {
	return filepath.Join(v.artifactDir, key[:2], key+".bin")
}

func fabricVectorArtifactKey(model string, dimensions int, purpose memory.VectorPurpose, text string) string {
	digest := sha256.New()
	for _, value := range []string{fabricVectorArtifactContract, strings.TrimSpace(model),
		strconv.Itoa(dimensions), string(purpose), text} {
		_, _ = digest.Write([]byte(value))
		_, _ = digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func readFabricVectorArtifact(path string, dimensions int) ([]float32, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	const headerSize = 8
	if len(encoded) != headerSize+dimensions*4 || string(encoded[:4]) != "LMEM" ||
		int(binary.LittleEndian.Uint32(encoded[4:8])) != dimensions {
		return nil, fmt.Errorf("invalid memory embedding artifact %s", path)
	}
	vector := make([]float32, dimensions)
	for index := range vector {
		vector[index] = math.Float32frombits(binary.LittleEndian.Uint32(encoded[headerSize+index*4:]))
	}
	if err := validateFabricVector(vector, dimensions); err != nil {
		return nil, err
	}
	return vector, nil
}

func writeFabricVectorArtifact(path string, vector []float32) error {
	if err := validateFabricVector(vector, len(vector)); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	encoded := make([]byte, 8+len(vector)*4)
	copy(encoded[:4], "LMEM")
	binary.LittleEndian.PutUint32(encoded[4:8], uint32(len(vector)))
	for index, value := range vector {
		binary.LittleEndian.PutUint32(encoded[8+index*4:], math.Float32bits(value))
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".embedding-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer func() { _ = os.Remove(name) }()
	_ = temporary.Chmod(0o600)
	_, err = temporary.Write(encoded)
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	// Content embeddings are deterministic, local and fully rebuildable. The
	// temp-file rename prevents readers from observing partial bytes; forcing a
	// disk flush for every vector would add hundreds of fsyncs to an import.
	return os.Chmod(path, 0o600)
}

func validateFabricVector(vector []float32, dimensions int) error {
	if dimensions <= 0 || len(vector) != dimensions {
		return fmt.Errorf("memory embedding artifact dimensions=%d, want %d", len(vector), dimensions)
	}
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("memory embedding artifact contains a non-finite value")
		}
	}
	return nil
}
