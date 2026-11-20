package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	storepkg "github.com/cirruslabs/orchard/internal/controller/store"
	"github.com/cirruslabs/orchard/internal/responder"
	v1 "github.com/cirruslabs/orchard/pkg/resource/v1"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func (tx *workerScopeTestTransaction) GetVM(name string) (*v1.VM, error) {
	for i := range tx.db.vms {
		if tx.db.vms[i].Name == name {
			vm := tx.db.vms[i]
			return &vm, nil
		}
	}
	return nil, storepkg.ErrNotFound
}

func (tx *workerScopeTestTransaction) SetVM(vm v1.VM) error {
	for i := range tx.db.vms {
		if tx.db.vms[i].Name == vm.Name {
			tx.db.vms[i] = vm
			return nil
		}
	}
	tx.db.vms = append(tx.db.vms, vm)
	return nil
}

func (tx *workerScopeTestTransaction) SetWorker(worker v1.Worker) error {
	tx.db.workers[worker.Name] = &worker
	return nil
}

func (tx *workerScopeTestTransaction) DeleteWorker(name string) error {
	delete(tx.db.workers, name)
	return nil
}

func (tx *workerScopeTestTransaction) AppendEvents(_ []v1.Event, _ ...string) error { return nil }

func workerScopeController() (*Controller, *workerScopeTestStore) {
	store := &workerScopeTestStore{
		vms: []v1.VM{
			{Meta: v1.Meta{Name: "own-vm"}, Worker: "worker-a"},
			{Meta: v1.Meta{Name: "other-vm"}, Worker: "worker-b"},
			{Meta: v1.Meta{Name: "unassigned-vm"}},
		},
		workers: map[string]*v1.Worker{
			"worker-a": {Meta: v1.Meta{Name: "worker-a"}},
			"worker-b": {Meta: v1.Meta{Name: "worker-b"}},
		},
	}
	return &Controller{store: store, logger: zap.NewNop().Sugar()}, store
}

func workerScopeCall(
	account *v1.ServiceAccount, method, name, body string, handler func(*gin.Context) responder.Responder,
) int {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, "/v1/x?command=id&port=22", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Params = gin.Params{{Key: "name", Value: name}}
	ctx.Set(ctxServiceAccountKey, account)
	handler(ctx).Respond(ctx)
	return recorder.Code
}

// A worker's own credential must not reach into VMs: exec, port-forward and the IP look-up go to
// whichever worker runs the VM, so one worker Mac would be root in every other worker's VMs. It
// has no use for creating or deleting VMs or deleting workers either.
func TestWorkerAccountRefusedOnRoutesNoWorkerNeeds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	controller, store := workerScopeController()
	account := workerScopeAccount("worker-a")

	for _, test := range []struct {
		name, method, target, body string
		handler                    func(*gin.Context) responder.Responder
	}{
		{"exec in another worker's VM", http.MethodGet, "other-vm", "", controller.execVM},
		{"exec in its own VM", http.MethodGet, "own-vm", "", controller.execVM},
		{"port-forward", http.MethodGet, "other-vm", "", controller.portForwardVM},
		{"ip", http.MethodGet, "other-vm", "", controller.ip},
		{"create VM", http.MethodPost, "", `{"name":"evil","image":"x"}`, controller.createVM},
		{"delete another worker's VM", http.MethodDelete, "other-vm", "", controller.deleteVM},
		{"delete its own VM", http.MethodDelete, "own-vm", "", controller.deleteVM},
		{"delete another worker", http.MethodDelete, "worker-b", "", controller.deleteWorker},
		{"delete itself", http.MethodDelete, "worker-a", "", controller.deleteWorker},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, http.StatusForbidden,
				workerScopeCall(account, test.method, test.target, test.body, test.handler))
		})
	}
	require.Len(t, store.vms, 3, "no VM was created or deleted")
	require.Len(t, store.workers, 2, "no worker was deleted")
}

func TestWorkerAccountActsOnlyOnItsOwnVMsAndWorker(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := workerScopeAccount("worker-a")

	for _, test := range []struct {
		name, method, target, body string
		handler                    func(*Controller) func(*gin.Context) responder.Responder
		want                       int
	}{
		{"state of its own VM", http.MethodPut, "own-vm", `{"status":"running"}`,
			func(c *Controller) func(*gin.Context) responder.Responder { return c.updateVMState }, http.StatusOK},
		{"state of another worker's VM", http.MethodPut, "other-vm", `{"status":"failed"}`,
			func(c *Controller) func(*gin.Context) responder.Responder { return c.updateVMState }, http.StatusForbidden},
		{"state of an unassigned VM", http.MethodPut, "unassigned-vm", `{"status":"failed"}`,
			func(c *Controller) func(*gin.Context) responder.Responder { return c.updateVMState }, http.StatusForbidden},
		{"spec of another worker's VM", http.MethodPut, "other-vm", `{"startup_script":{"script_content":"id"}}`,
			func(c *Controller) func(*gin.Context) responder.Responder { return c.updateVMSpec }, http.StatusForbidden},
		{"events of its own VM", http.MethodPost, "own-vm", `[]`,
			func(c *Controller) func(*gin.Context) responder.Responder { return c.appendVMEvents }, http.StatusOK},
		{"events of another worker's VM", http.MethodPost, "other-vm", `[]`,
			func(c *Controller) func(*gin.Context) responder.Responder { return c.appendVMEvents }, http.StatusForbidden},
		{"registers itself", http.MethodPost, "", `{"name":"worker-a"}`,
			func(c *Controller) func(*gin.Context) responder.Responder { return c.createWorker }, http.StatusOK},
		{"registers as another worker", http.MethodPost, "", `{"name":"worker-b"}`,
			func(c *Controller) func(*gin.Context) responder.Responder { return c.createWorker }, http.StatusForbidden},
		{"registers a new name", http.MethodPost, "", `{"name":"worker-c"}`,
			func(c *Controller) func(*gin.Context) responder.Responder { return c.createWorker }, http.StatusForbidden},
		{"heartbeat for itself", http.MethodPut, "worker-a", `{"name":"worker-a"}`,
			func(c *Controller) func(*gin.Context) responder.Responder { return c.updateWorker }, http.StatusOK},
		{"pauses another worker", http.MethodPut, "worker-b", `{"name":"worker-b","schedulingPaused":true}`,
			func(c *Controller) func(*gin.Context) responder.Responder { return c.updateWorker }, http.StatusForbidden},
		// The handler updates the worker the body names, whatever the path says.
		{"pauses another worker under its own path", http.MethodPut, "worker-a", `{"name":"worker-b","schedulingPaused":true}`,
			func(c *Controller) func(*gin.Context) responder.Responder { return c.updateWorker }, http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			controller, store := workerScopeController()
			got := workerScopeCall(account, test.method, test.target, test.body, test.handler(controller))
			require.Equal(t, test.want, got)
			require.False(t, store.workers["worker-b"].SchedulingPaused)
			for _, vm := range store.vms {
				if vm.Name != "own-vm" {
					require.Empty(t, vm.Status, "VM %s was changed", vm.Name)
				}
			}
		})
	}
}

// The control plane's account (compute:read among its roles) and a plain compute:write account
// that was never bound to a worker keep the whole API.
func TestUnboundAccountsKeepTheWholeAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	controlPlane := &v1.ServiceAccount{Meta: v1.Meta{Name: "grove"}, Roles: []v1.ServiceAccountRole{
		v1.ServiceAccountRoleComputeRead, v1.ServiceAccountRoleComputeWrite, v1.ServiceAccountRoleComputeConnect,
	}}
	legacy := &v1.ServiceAccount{Meta: v1.Meta{Name: "ci"}, Roles: []v1.ServiceAccountRole{v1.ServiceAccountRoleComputeWrite}}
	for _, account := range []*v1.ServiceAccount{controlPlane, legacy} {
		require.False(t, workerRestricted(account), account.Name)
		controller, store := workerScopeController()
		require.Equal(t, http.StatusOK, workerScopeCall(account, http.MethodPut, "other-vm", `{"status":"failed"}`, controller.updateVMState))
		require.Equal(t, http.StatusOK, workerScopeCall(account, http.MethodDelete, "worker-b", "", controller.deleteWorker))
		require.NotContains(t, store.workers, "worker-b")
	}
	// A bound account an administrator later gave other roles is still a worker's, and one with no
	// binding under the worker prefix owns nothing.
	require.True(t, workerRestricted(&v1.ServiceAccount{WorkerName: "worker-a", Roles: []v1.ServiceAccountRole{v1.ServiceAccountRoleComputeWrite}}))
	require.True(t, workerRestricted(workerScopeAccount("")))
	controller, _ := workerScopeController()
	require.Equal(t, http.StatusForbidden, workerScopeCall(workerScopeAccount(""), http.MethodPut, "own-vm", `{"status":"failed"}`, controller.updateVMState))
}
