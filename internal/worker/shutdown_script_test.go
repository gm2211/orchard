package worker //nolint:testpackage // exercises the unexported deleteVM directly

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cirruslabs/orchard/internal/worker/ondiskname"
	"github.com/cirruslabs/orchard/internal/worker/vmmanager"
	v1 "github.com/cirruslabs/orchard/pkg/resource/v1"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// shutdownScriptTestVM is a minimal vmmanager.VM fake that records the ordering
// and timing of RunShutdownScript relative to Stop and Delete.
type shutdownScriptTestVM struct {
	vmmanager.VM

	resource v1.VM
	running  bool

	// shutdownScriptDelay simulates how long the shutdown script takes to "run".
	shutdownScriptDelay time.Duration

	ranShutdownScript atomic.Bool
	stoppedBeforeRun  atomic.Bool
	deletedBeforeRun  atomic.Bool
	stopped           atomic.Bool
	deleted           atomic.Bool
}

func (vm *shutdownScriptTestVM) Resource() v1.VM {
	return vm.resource
}

func (vm *shutdownScriptTestVM) OnDiskName() ondiskname.OnDiskName {
	return ondiskname.NewFromResource(vm.resource)
}

func (vm *shutdownScriptTestVM) Running() bool {
	return vm.running
}

func (vm *shutdownScriptTestVM) RunShutdownScript(ctx context.Context) error {
	vm.ranShutdownScript.Store(true)

	if vm.stopped.Load() {
		vm.stoppedBeforeRun.Store(true)
	}
	if vm.deleted.Load() {
		vm.deletedBeforeRun.Store(true)
	}

	select {
	case <-time.After(vm.shutdownScriptDelay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (vm *shutdownScriptTestVM) Stop() <-chan error {
	vm.stopped.Store(true)

	result := make(chan error, 1)
	result <- nil

	return result
}

func (vm *shutdownScriptTestVM) Delete() error {
	vm.deleted.Store(true)

	return nil
}

func TestDeleteVMRunsShutdownScriptBeforeStoppingAndDeleting(t *testing.T) {
	worker := &Worker{vmm: vmmanager.New(), logger: zap.NewNop().Sugar()}

	vm := &shutdownScriptTestVM{
		resource: v1.VM{
			Meta:           v1.Meta{Name: "test-vm"},
			ShutdownScript: &v1.VMScript{ScriptContent: "echo bye"},
		},
		running: true,
	}
	worker.vmm.Put(vm.OnDiskName(), vm)

	require.NoError(t, worker.deleteVM(vm))
	require.True(t, vm.ranShutdownScript.Load(), "shutdown script must run")
	require.False(t, vm.stoppedBeforeRun.Load(), "shutdown script must run before the VM is stopped")
	require.False(t, vm.deletedBeforeRun.Load(), "shutdown script must run before the VM is deleted")
	require.True(t, vm.stopped.Load())
	require.True(t, vm.deleted.Load())
	require.False(t, worker.vmm.Exists(vm.OnDiskName()))
}

func TestDeleteVMSkipsShutdownScriptForNonRunningVM(t *testing.T) {
	worker := &Worker{vmm: vmmanager.New(), logger: zap.NewNop().Sugar()}

	vm := &shutdownScriptTestVM{
		resource: v1.VM{
			Meta:           v1.Meta{Name: "test-vm"},
			ShutdownScript: &v1.VMScript{ScriptContent: "echo bye"},
		},
		running: false,
	}
	worker.vmm.Put(vm.OnDiskName(), vm)

	require.NoError(t, worker.deleteVM(vm))
	require.False(t, vm.ranShutdownScript.Load(), "a VM that isn't running must not run its shutdown script")
	require.True(t, vm.stopped.Load())
	require.True(t, vm.deleted.Load())
}

func TestDeleteVMSkipsAbsentShutdownScript(t *testing.T) {
	worker := &Worker{vmm: vmmanager.New(), logger: zap.NewNop().Sugar()}

	vm := &shutdownScriptTestVM{
		resource: v1.VM{Meta: v1.Meta{Name: "test-vm"}},
		running:  true,
	}
	worker.vmm.Put(vm.OnDiskName(), vm)

	require.NoError(t, worker.deleteVM(vm))
	require.False(t, vm.ranShutdownScript.Load(), "a VM without a shutdown script must not run one")
	require.True(t, vm.deleted.Load())
}

// TestDeleteVMDoesNotBlockOnSlowShutdownScript proves that a shutdown script
// exceeding its timeout doesn't hold up VM deletion: RunShutdownScript itself
// keeps "running" for an hour, but deleteVM must still return promptly once
// the (1 second) timeout elapses.
func TestDeleteVMDoesNotBlockOnSlowShutdownScript(t *testing.T) {
	worker := &Worker{vmm: vmmanager.New(), logger: zap.NewNop().Sugar()}

	vm := &shutdownScriptTestVM{
		resource: v1.VM{
			Meta:                         v1.Meta{Name: "test-vm"},
			ShutdownScript:               &v1.VMScript{ScriptContent: "sleep 9999"},
			ShutdownScriptTimeoutSeconds: 1,
		},
		running:             true,
		shutdownScriptDelay: time.Hour,
	}
	worker.vmm.Put(vm.OnDiskName(), vm)

	done := make(chan error, 1)
	go func() { done <- worker.deleteVM(vm) }()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("deleteVM blocked on a shutdown script that exceeded its timeout")
	}

	require.True(t, vm.ranShutdownScript.Load())
	require.True(t, vm.stopped.Load())
	require.True(t, vm.deleted.Load())
}
