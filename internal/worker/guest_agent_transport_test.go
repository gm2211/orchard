package worker //nolint:testpackage // exercises the unexported vmDialer and capabilities

import (
	"testing"

	"github.com/cirruslabs/orchard/internal/dialer"
	"github.com/cirruslabs/orchard/internal/worker/ondiskname"
	"github.com/cirruslabs/orchard/internal/worker/runtime"
	v1 "github.com/cirruslabs/orchard/pkg/resource/v1"
	"github.com/stretchr/testify/require"
)

func TestGuestAgentTransportOnlyForTart(t *testing.T) {
	odn := ondiskname.New("vm", "uid", 0)

	tartWorker := &Worker{runtime: runtime.NewTart()}
	WithGuestAgentTransport()(tartWorker)
	require.True(t, tartWorker.capabilities().Has(v1.WorkerCapabilityGuestAgentTransport))
	require.IsType(t, dialer.DialFunc(nil), tartWorker.vmDialer(odn))

	// Without the option the worker keeps dialing over the network
	networkWorker := &Worker{runtime: runtime.NewTart()}
	require.False(t, networkWorker.capabilities().Has(v1.WorkerCapabilityGuestAgentTransport))
	require.Nil(t, networkWorker.vmDialer(odn))

	// Synthetic VMs have no guest agent to relay through
	syntheticWorker := &Worker{runtime: runtime.NewSynthetic()}
	WithGuestAgentTransport()(syntheticWorker)
	require.False(t, syntheticWorker.capabilities().Has(v1.WorkerCapabilityGuestAgentTransport))
	require.Nil(t, syntheticWorker.vmDialer(odn))
}
