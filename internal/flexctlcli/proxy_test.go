package flexctlcli_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/flexctlcli"
)

func TestProxy_NormalizesHost(t *testing.T) {
	require.Equal(t, "paul-vllm", flexctlcli.NormalizeProxyHost("paul-vllm.flex"))
	require.Equal(t, "paul-vllm", flexctlcli.NormalizeProxyHost("paul-vllm"))
}
