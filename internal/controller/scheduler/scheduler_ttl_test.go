//nolint:testpackage // The regression exercises the unexported scheduler health-checking loop.
package scheduler

import (
	"errors"
	"testing"
	"time"

	"github.com/cirruslabs/orchard/internal/controller/notifier"
	storepkg "github.com/cirruslabs/orchard/internal/controller/store"
	"github.com/cirruslabs/orchard/internal/controller/store/badger"
	v1 "github.com/cirruslabs/orchard/pkg/resource/v1"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestHealthCheckingLoopDeletesExpiredVM ensures that a VM whose TTL has
// been exceeded is deleted by a single health-checking loop iteration,
// exactly as if "orchard delete vm" had been called: both the VM record
// and its events are removed from the store.
func TestHealthCheckingLoopDeletesExpiredVM(t *testing.T) {
	logger := zap.NewNop().Sugar()

	store, err := badger.NewBadgerStore(t.TempDir(), true, logger)
	require.NoError(t, err)

	var expiredVM v1.VM
	expiredVM.Name = "expired-vm"
	expiredVM.UID = "expired-vm-uid"
	expiredVM.CreatedAt = time.Now().Add(-2 * time.Second)
	expiredVM.TTLSeconds = 1

	err = store.Update(func(txn storepkg.Transaction) error {
		if err := txn.SetVM(expiredVM); err != nil {
			return err
		}

		return txn.AppendEvents([]v1.Event{{
			Kind:      v1.EventKindLogLine,
			Timestamp: time.Now().Unix(),
			Payload:   "created",
		}}, "vms", expiredVM.UID)
	})
	require.NoError(t, err)

	scheduler, err := NewScheduler(store, notifier.NewNotifier(logger), time.Minute, logger)
	require.NoError(t, err)

	numVMs, err := scheduler.healthCheckingLoopIteration()
	require.NoError(t, err)
	require.Equal(t, 1, numVMs)

	err = store.View(func(txn storepkg.Transaction) error {
		_, err := txn.GetVM(expiredVM.Name)
		require.True(t, errors.Is(err, storepkg.ErrNotFound))

		events, err := txn.ListEvents("vms", expiredVM.UID)
		require.NoError(t, err)
		require.Empty(t, events)

		return nil
	})
	require.NoError(t, err)
}

// TestHealthCheckingLoopKeepsVMWithoutTTL ensures that a VM with no TTL
// set (TTLSeconds == 0) is never deleted by the scheduler, no matter its age.
func TestHealthCheckingLoopKeepsVMWithoutTTL(t *testing.T) {
	logger := zap.NewNop().Sugar()

	store, err := badger.NewBadgerStore(t.TempDir(), true, logger)
	require.NoError(t, err)

	var vm v1.VM
	vm.Name = "no-ttl-vm"
	vm.UID = "no-ttl-vm-uid"
	vm.CreatedAt = time.Now().Add(-24 * time.Hour)
	vm.TTLSeconds = 0

	err = store.Update(func(txn storepkg.Transaction) error {
		return txn.SetVM(vm)
	})
	require.NoError(t, err)

	scheduler, err := NewScheduler(store, notifier.NewNotifier(logger), time.Minute, logger)
	require.NoError(t, err)

	_, err = scheduler.healthCheckingLoopIteration()
	require.NoError(t, err)

	err = store.View(func(txn storepkg.Transaction) error {
		currentVM, err := txn.GetVM(vm.Name)
		require.NoError(t, err)
		require.Equal(t, vm.Name, currentVM.Name)

		return nil
	})
	require.NoError(t, err)
}

// TestHealthCheckingLoopKeepsVMYoungerThanTTL ensures that a VM younger
// than its configured TTL is left untouched.
func TestHealthCheckingLoopKeepsVMYoungerThanTTL(t *testing.T) {
	logger := zap.NewNop().Sugar()

	store, err := badger.NewBadgerStore(t.TempDir(), true, logger)
	require.NoError(t, err)

	var vm v1.VM
	vm.Name = "young-vm"
	vm.UID = "young-vm-uid"
	vm.CreatedAt = time.Now()
	vm.TTLSeconds = 3600

	err = store.Update(func(txn storepkg.Transaction) error {
		return txn.SetVM(vm)
	})
	require.NoError(t, err)

	scheduler, err := NewScheduler(store, notifier.NewNotifier(logger), time.Minute, logger)
	require.NoError(t, err)

	_, err = scheduler.healthCheckingLoopIteration()
	require.NoError(t, err)

	err = store.View(func(txn storepkg.Transaction) error {
		currentVM, err := txn.GetVM(vm.Name)
		require.NoError(t, err)
		require.Equal(t, vm.Name, currentVM.Name)

		return nil
	})
	require.NoError(t, err)
}
