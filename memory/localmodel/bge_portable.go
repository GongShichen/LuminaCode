//go:build !cgo

package localmodel

import (
	"context"
	"fmt"
)

type LocalBGEEncoder struct{}

func NewLocalBGEEncoder(string) (*LocalBGEEncoder, error) {
	return nil, fmt.Errorf("BGE-M3 retrieval requires a cgo-enabled LuminaCode build")
}

func SharedLocalBGEEncoder(string) (*LocalBGEEncoder, error) {
	return nil, fmt.Errorf("BGE-M3 retrieval requires a cgo-enabled LuminaCode build")
}

func (*LocalBGEEncoder) Model() string         { return BGEModelName }
func (*LocalBGEEncoder) Revision() string      { return BGEModelRevision }
func (*LocalBGEEncoder) TokenizerHash() string { return "" }
func (*LocalBGEEncoder) Encode(context.Context, []string, BGEInputKind) ([]BGEEmbedding, error) {
	return nil, fmt.Errorf("BGE-M3 retrieval requires a cgo-enabled LuminaCode build")
}
func (*LocalBGEEncoder) EncodeChannels(context.Context, []string, BGEInputKind) ([]BGEEmbedding, error) {
	return nil, fmt.Errorf("BGE-M3 retrieval requires a cgo-enabled LuminaCode build")
}
func (*LocalBGEEncoder) Split(string, int, int) ([]string, error) {
	return nil, fmt.Errorf("BGE-M3 retrieval requires a cgo-enabled LuminaCode build")
}
func (*LocalBGEEncoder) SplitMany(string, []BGESplitSpec) ([][]string, error) {
	return nil, fmt.Errorf("BGE-M3 retrieval requires a cgo-enabled LuminaCode build")
}
func (*LocalBGEEncoder) Close() error { return nil }
