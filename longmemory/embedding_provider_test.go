//go:build cgo

package longmemory

import (
	"reflect"
	"testing"
)

func TestEmbeddingProviderCandidatesRespectInstalledRuntime(t *testing.T) {
	tests := []struct {
		name              string
		requested         string
		goos              string
		installedProvider string
		customRuntime     bool
		cudaAvailable     bool
		rocmAvailable     bool
		want              []string
	}{
		{name: "mac bundled coreml", goos: "darwin", installedProvider: "coreml", want: []string{"coreml", "cpu"}},
		{name: "linux bundled cuda", goos: "linux", installedProvider: "cuda", cudaAvailable: true, want: []string{"cuda", "cpu"}},
		{name: "bundled cpu does not pretend to include rocm", goos: "linux", installedProvider: "cpu", rocmAvailable: true, want: []string{"cpu"}},
		{name: "custom linux runtime can expose rocm", goos: "linux", customRuntime: true, rocmAvailable: true, want: []string{"rocm", "cpu"}},
		{name: "custom windows runtime can expose directml", goos: "windows", customRuntime: true, want: []string{"directml", "cpu"}},
		{name: "explicit provider is attempted before cpu", requested: "rocm", goos: "linux", installedProvider: "cpu", want: []string{"rocm", "cpu"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := embeddingProviderCandidatesFor(test.requested, test.goos, test.installedProvider, test.customRuntime, test.cudaAvailable, test.rocmAvailable)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("providers = %v, want %v", got, test.want)
			}
		})
	}
}
