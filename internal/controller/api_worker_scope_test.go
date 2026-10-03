package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	storepkg "github.com/cirruslabs/orchard/internal/controller/store"
	v1 "github.com/cirruslabs/orchard/pkg/resource/v1"
	"github.com/cirruslabs/orchard/rpc"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc/metadata"
)

type workerScopeTestStore struct {
	storepkg.Store
	vms             []v1.VM
	serviceAccounts map[string]*v1.ServiceAccount
}

func (s *workerScopeTestStore) View(cb func(storepkg.Transaction) error) error {
	return cb(&workerScopeTestTransaction{db: s})
}

func (s *workerScopeTestStore) Update(cb func(storepkg.Transaction) error) error {
	return cb(&workerScopeTestTransaction{db: s})
}

type workerScopeTestTransaction struct {
	storepkg.Transaction
	db *workerScopeTestStore
}

func (tx *workerScopeTestTransaction) ListVMs() ([]v1.VM, error) {
	return tx.db.vms, nil
}

func (tx *workerScopeTestTransaction) GetServiceAccount(name string) (*v1.ServiceAccount, error) {
	account, ok := tx.db.serviceAccounts[name]
	if !ok {
		return nil, storepkg.ErrNotFound
	}
	copy := *account
	return &copy, nil
}

func (tx *workerScopeTestTransaction) SetServiceAccount(account *v1.ServiceAccount) error {
	copy := *account
	tx.db.serviceAccounts[copy.Name] = &copy
	return nil
}

func workerScopeAccount(workerName string) *v1.ServiceAccount {
	return &v1.ServiceAccount{
		Token:      "test-token",
		WorkerName: workerName,
		Roles:      []v1.ServiceAccountRole{v1.ServiceAccountRoleComputeWrite, v1.ServiceAccountRoleComputeConnect},
		Meta:       v1.Meta{Name: "grove-worker-test"},
	}
}

func TestListVMsScopesWorkerAccountRegardlessOfQueryFilters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	controller := &Controller{
		store: &workerScopeTestStore{vms: []v1.VM{
			{Meta: v1.Meta{Name: "own-vm"}, Worker: "worker-a"},
			{Meta: v1.Meta{Name: "other-vm"}, Worker: "worker-b"},
			{Meta: v1.Meta{Name: "unassigned-vm"}},
		}}, logger: zap.NewNop().Sugar(),
	}
	account := workerScopeAccount("worker-a")

	for _, test := range []struct {
		name       string
		query      string
		wantStatus int
		wantNames  []string
	}{
		{name: "defaults to assigned worker", wantStatus: http.StatusOK, wantNames: []string{"own-vm"}},
		{name: "caller filter cannot request another worker", query: "filter=worker%3Dworker-b", wantStatus: http.StatusOK},
		{name: "caller filters intersect with assigned worker", query: "filter=worker%3Dworker-a", wantStatus: http.StatusOK, wantNames: []string{"own-vm"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/vms?"+test.query, nil)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = request
			ctx.Set(ctxServiceAccountKey, account)

			controller.listVMs(ctx).Respond(ctx)

			require.Equal(t, test.wantStatus, recorder.Code)
			var got []v1.VM
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &got))
			var gotNames []string
			for _, vm := range got {
				gotNames = append(gotNames, vm.Name)
			}
			require.Equal(t, test.wantNames, gotNames)
		})
	}
}

func TestListVMsRequiresExplicitWorkerBindingAndPreservesBroadRead(t *testing.T) {
	gin.SetMode(gin.TestMode)
	controller := &Controller{
		store: &workerScopeTestStore{vms: []v1.VM{
			{Meta: v1.Meta{Name: "own-vm"}, Worker: "worker-a"},
			{Meta: v1.Meta{Name: "other-vm"}, Worker: "worker-b"},
			{Meta: v1.Meta{Name: "unassigned-vm"}},
		}}, logger: zap.NewNop().Sugar(),
	}

	for _, test := range []struct {
		name       string
		account    *v1.ServiceAccount
		wantStatus int
		wantNames  []string
	}{
		{name: "missing binding denied", account: workerScopeAccount(""), wantStatus: http.StatusUnauthorized},
		{name: "compute read remains broad", account: &v1.ServiceAccount{Roles: []v1.ServiceAccountRole{v1.ServiceAccountRoleComputeRead}}, wantStatus: http.StatusOK, wantNames: []string{"own-vm", "other-vm", "unassigned-vm"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/vms", nil)
			ctx.Set(ctxServiceAccountKey, test.account)

			controller.listVMs(ctx).Respond(ctx)

			require.Equal(t, test.wantStatus, recorder.Code)
			if test.wantStatus != http.StatusOK {
				return
			}
			var got []v1.VM
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &got))
			var gotNames []string
			for _, vm := range got {
				gotNames = append(gotNames, vm.Name)
			}
			require.Equal(t, test.wantNames, gotNames)
		})
	}
}

func TestWorkerRPCWatchCannotCrossWorkerBinding(t *testing.T) {
	account := workerScopeAccount("worker-a")
	controller := &Controller{store: &workerScopeTestStore{serviceAccounts: map[string]*v1.ServiceAccount{account.Name: account}}}

	for _, test := range []struct {
		name      string
		worker    string
		account   *v1.ServiceAccount
		wantAllow bool
	}{
		{name: "own worker", worker: "worker-a", account: account, wantAllow: true},
		{name: "other worker", worker: "worker-b", account: account},
		{name: "role mutation cannot remove binding", worker: "worker-a", account: &v1.ServiceAccount{
			Token: account.Token, WorkerName: "worker-a",
			Roles: []v1.ServiceAccountRole{v1.ServiceAccountRoleComputeWrite, v1.ServiceAccountRoleWorkerIssue},
			Meta:  account.Meta,
		}},
		{name: "role mutation cannot widen binding", worker: "worker-b", account: &v1.ServiceAccount{
			Token: account.Token, WorkerName: "worker-a",
			Roles: []v1.ServiceAccountRole{v1.ServiceAccountRoleComputeWrite, v1.ServiceAccountRoleWorkerIssue},
			Meta:  account.Meta,
		}},
		{name: "legacy Grove account without binding", worker: "worker-a", account: workerScopeAccount("")},
		{name: "altered unbound Grove account stays denied", worker: "worker-a", account: &v1.ServiceAccount{
			Token: account.Token,
			Roles: []v1.ServiceAccountRole{v1.ServiceAccountRoleComputeWrite, v1.ServiceAccountRoleWorkerIssue},
			Meta:  account.Meta,
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
				rpc.MetadataServiceAccountNameKey, test.account.Name,
				rpc.MetadataServiceAccountTokenKey, test.account.Token,
				rpc.MetadataWorkerNameKey, test.worker,
			))
			controller.store.(*workerScopeTestStore).serviceAccounts[test.account.Name] = test.account
			require.Equal(t, test.wantAllow, controller.authorizeGRPCWorkerWatch(ctx, test.worker))
		})
	}

	legacyNonGrove := &v1.ServiceAccount{Token: "legacy", Roles: []v1.ServiceAccountRole{v1.ServiceAccountRoleComputeWrite}, Meta: v1.Meta{Name: "legacy-worker"}}
	controller.store.(*workerScopeTestStore).serviceAccounts[legacyNonGrove.Name] = legacyNonGrove
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		rpc.MetadataServiceAccountNameKey, legacyNonGrove.Name,
		rpc.MetadataServiceAccountTokenKey, legacyNonGrove.Token,
		rpc.MetadataWorkerNameKey, "legacy-worker-name",
	))
	require.True(t, controller.authorizeGRPCWorkerWatch(ctx, "legacy-worker-name"), "preserve non-Grove compute:write Watch compatibility")
}

func TestRPCWatchRejectsCrossWorkerBeforeWebSocketUpgrade(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := workerScopeAccount("worker-a")
	controller := &Controller{store: &workerScopeTestStore{serviceAccounts: map[string]*v1.ServiceAccount{account.Name: account}}}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/rpc/watch?workerName=worker-b", nil)
	ctx.Set(ctxServiceAccountKey, account)

	controller.rpcWatch(ctx).Respond(ctx)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestServiceAccountUpdatePreservesWorkerBindingWhenRotatingToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := workerScopeAccount("worker-a")
	db := &workerScopeTestStore{serviceAccounts: map[string]*v1.ServiceAccount{account.Name: account}}
	controller := &Controller{store: db}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/v1/service-accounts", strings.NewReader(`{"name":"grove-worker-test","token":"rotated","roles":["compute:write","compute:connect"]}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set(ctxServiceAccountKey, &v1.ServiceAccount{Roles: []v1.ServiceAccountRole{v1.ServiceAccountRoleAdminWrite}})

	controller.updateServiceAccount(ctx).Respond(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "worker-a", db.serviceAccounts[account.Name].WorkerName)
}
