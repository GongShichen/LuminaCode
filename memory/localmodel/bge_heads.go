package localmodel

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type bgeHeadSpec struct {
	archive      string
	prefix       string
	weightFile   string
	weightBytes  int64
	weightSHA256 string
	biasFile     string
	biasBytes    int64
	biasSHA256   string
	weightShape  string
	biasShape    string
}

var bgeHeadSpecs = []bgeHeadSpec{
	{archive: "colbert_linear.pt", prefix: "colbert_linear/", weightFile: "colbert.weight.fp16",
		weightBytes: 2097152, weightSHA256: "024db35eb6602bb68974bfbe01e4b062f2bf62310baa2fb63a7b0497c71a0c43",
		biasFile: "colbert.bias.fp16", biasBytes: 2048,
		biasSHA256:  "53d66308968ded5691d7c82e25dce1b5d131a35a7c644083b2b2920d25af9f2a",
		weightShape: "1024x1024", biasShape: "1024"},
	{archive: "sparse_linear.pt", prefix: "sparse_linear/", weightFile: "sparse.weight.fp16",
		weightBytes: 2048, weightSHA256: "9ff696ed5c8e9b35babce11a06ab132aa74de9d87c8d229bedd2f001dc10b873",
		biasFile: "sparse.bias.fp16", biasBytes: 2,
		biasSHA256:  "626077bf00373688d21329e599f837ee4ab2c4b348869331f05648ed3f2c38ee",
		weightShape: "1x1024", biasShape: "1"},
}

// PrepareBGEHeads extracts plain FP16 tensors from the fixed PyTorch ZIP
// archives. It deliberately supports only the locked BGE-M3 head layout.
func PrepareBGEHeads(modelDir string) error {
	modelDir = filepath.Clean(ExpandPath(modelDir))
	headDir := filepath.Join(modelDir, "heads")
	if err := os.MkdirAll(headDir, 0o700); err != nil {
		return err
	}
	for _, spec := range bgeHeadSpecs {
		archivePath := filepath.Join(modelDir, spec.archive)
		reader, err := zip.OpenReader(archivePath)
		if err != nil {
			return fmt.Errorf("open %s: %w", spec.archive, err)
		}
		err = errors.Join(
			extractLockedTensor(reader.File, spec.prefix+"data/0", filepath.Join(headDir, spec.weightFile),
				spec.weightBytes, spec.weightSHA256),
			extractLockedTensor(reader.File, spec.prefix+"data/1", filepath.Join(headDir, spec.biasFile),
				spec.biasBytes, spec.biasSHA256),
		)
		closeErr := reader.Close()
		if err = errors.Join(err, closeErr); err != nil {
			return fmt.Errorf("extract %s: %w", spec.archive, err)
		}
	}
	manifest := "{\n" +
		"  \"format\": \"float16-le\",\n" +
		"  \"colbert_weight_shape\": \"1024x1024\",\n" +
		"  \"colbert_bias_shape\": \"1024\",\n" +
		"  \"sparse_weight_shape\": \"1x1024\",\n" +
		"  \"sparse_bias_shape\": \"1\"\n" +
		"}\n"
	return writePrivateAtomic(filepath.Join(headDir, "manifest.json"), []byte(manifest))
}

func VerifyBGEHeads(modelDir string) error {
	headDir := filepath.Join(filepath.Clean(ExpandPath(modelDir)), "heads")
	for _, spec := range bgeHeadSpecs {
		if err := verifyLockedTensor(filepath.Join(headDir, spec.weightFile), spec.weightBytes, spec.weightSHA256); err != nil {
			return err
		}
		if err := verifyLockedTensor(filepath.Join(headDir, spec.biasFile), spec.biasBytes, spec.biasSHA256); err != nil {
			return err
		}
	}
	return nil
}

func extractLockedTensor(files []*zip.File, name, destination string, size int64, expectedSHA string) error {
	for _, file := range files {
		if file.Name != name {
			continue
		}
		if int64(file.UncompressedSize64) != size {
			return fmt.Errorf("%s size is %d, want %d", name, file.UncompressedSize64, size)
		}
		source, err := file.Open()
		if err != nil {
			return err
		}
		temporary := destination + ".new"
		target, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			_ = source.Close()
			return err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(io.MultiWriter(target, hash), source)
		err = errors.Join(copyErr, target.Sync(), target.Close(), source.Close())
		if err != nil {
			_ = os.Remove(temporary)
			return err
		}
		if actual := hex.EncodeToString(hash.Sum(nil)); actual != expectedSHA {
			_ = os.Remove(temporary)
			return fmt.Errorf("%s SHA-256 is %s, want %s", name, actual, expectedSHA)
		}
		if err := os.Chmod(temporary, 0o600); err != nil {
			_ = os.Remove(temporary)
			return err
		}
		return os.Rename(temporary, destination)
	}
	return fmt.Errorf("tensor %s not found", name)
}

func verifyLockedTensor(path string, size int64, expectedSHA string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != size {
		return fmt.Errorf("%s size is %d, want %d", path, info.Size(), size)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if actual := hex.EncodeToString(hash.Sum(nil)); !strings.EqualFold(actual, expectedSHA) {
		return fmt.Errorf("%s SHA-256 is %s, want %s", path, actual, expectedSHA)
	}
	return nil
}

func writePrivateAtomic(path string, content []byte) error {
	temporary := path + ".new"
	if err := os.WriteFile(temporary, content, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(temporary, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
