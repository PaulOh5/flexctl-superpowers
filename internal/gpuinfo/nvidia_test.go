package gpuinfo_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/flexctlagent"
	"github.com/paul/flexctl/internal/gpuinfo"
)

func TestParseNvidiaSMI_Happy(t *testing.T) {
	out := `0, NVIDIA GeForce RTX 4090, 24576 MiB
1, NVIDIA H100 80GB HBM3, 81920 MiB
`
	got, err := gpuinfo.ParseNvidiaSMI(out)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, flexctlagent.GPU{Index: 0, Model: "NVIDIA GeForce RTX 4090", VRAMMB: 24576}, got[0])
	require.Equal(t, flexctlagent.GPU{Index: 1, Model: "NVIDIA H100 80GB HBM3", VRAMMB: 81920}, got[1])
}

func TestParseNvidiaSMI_EmptyOK(t *testing.T) {
	got, err := gpuinfo.ParseNvidiaSMI("")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestParseNvidiaSMI_BadLineFails(t *testing.T) {
	_, err := gpuinfo.ParseNvidiaSMI("not, a valid line")
	require.Error(t, err)
}

func TestNvidiaDetector_NoBinaryGracefulEmpty(t *testing.T) {
	// PATH 가 비어 있으면 nvidia-smi를 못 찾고 빈 슬라이스 반환 (에러 X)
	d := gpuinfo.NewNvidiaDetector(gpuinfo.WithExecLookPath(func(_ string) (string, error) {
		return "", errors.New("not found")
	}))
	got := d.Detect(context.Background())
	require.Empty(t, got)
}
