package controller

import (
	"context"
	"strings"

	"github.com/cirruslabs/orchard/internal/responder"
	v1 "github.com/cirruslabs/orchard/pkg/resource/v1"
	"github.com/cirruslabs/orchard/rpc"
	mapset "github.com/deckarep/golang-set/v2"
	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/metadata"
)

func serviceAccountFromContext(ctx *gin.Context) *v1.ServiceAccount {
	value, ok := ctx.Get(ctxServiceAccountKey)
	if !ok {
		return nil
	}
	account, _ := value.(*v1.ServiceAccount)
	return account
}

func serviceAccountHasRole(account *v1.ServiceAccount, role v1.ServiceAccountRole) bool {
	if account == nil {
		return false
	}
	return mapset.NewSet[v1.ServiceAccountRole](account.Roles...).Contains(role)
}

// workerAccountOwnsName grants worker-issued accounts access only to the worker identity
// explicitly stored by the trusted worker issuer. Request parameters never define this scope.
func workerAccountOwnsName(account *v1.ServiceAccount, workerName string) bool {
	return account != nil && workerName != "" && validWorkerIssuedAccount(account) && account.WorkerName == workerName
}

func vmListFilters(account *v1.ServiceAccount, filters []v1.Filter) ([]v1.Filter, bool) {
	if serviceAccountHasRole(account, v1.ServiceAccountRoleComputeRead) {
		return filters, true
	}
	if account == nil || !workerAccountOwnsName(account, account.WorkerName) {
		return nil, false
	}
	return append(filters, v1.Filter{Path: "worker", Value: account.WorkerName}), true
}

func (controller *Controller) authorizeVMList(ctx *gin.Context, filters []v1.Filter) ([]v1.Filter, responder.Responder) {
	if controller.insecureAuthDisabled {
		return filters, nil
	}
	scopedFilters, ok := vmListFilters(serviceAccountFromContext(ctx), filters)
	if !ok {
		return nil, responder.JSON(401, NewErrorResponse("compute:read or a worker-bound compute:write/compute:connect account is required"))
	}
	return scopedFilters, nil
}

func (controller *Controller) authorizeWorkerWatch(ctx *gin.Context, workerName string) bool {
	if controller.insecureAuthDisabled {
		return true
	}
	account := serviceAccountFromContext(ctx)
	if serviceAccountHasRole(account, v1.ServiceAccountRoleComputeRead) {
		return true
	}
	return workerAccountOwnsName(account, workerName)
}

func (controller *Controller) authorizeGRPCWorkerWatch(ctx context.Context, workerName string) bool {
	if controller.insecureAuthDisabled {
		return true
	}
	names := metadata.ValueFromIncomingContext(ctx, rpc.MetadataServiceAccountNameKey)
	tokens := metadata.ValueFromIncomingContext(ctx, rpc.MetadataServiceAccountTokenKey)
	workers := metadata.ValueFromIncomingContext(ctx, rpc.MetadataWorkerNameKey)
	if len(names) != 1 || len(tokens) != 1 || len(workers) != 1 || workers[0] != workerName {
		return false
	}
	account, err := controller.fetchServiceAccount(names[0], tokens[0])
	if err != nil || !serviceAccountHasRole(account, v1.ServiceAccountRoleComputeWrite) {
		return false
	}
	if serviceAccountHasRole(account, v1.ServiceAccountRoleComputeRead) {
		return true
	}
	if account.WorkerName != "" {
		// A persisted binding is always security-relevant, even if an administrator
		// later changes the account's roles. Only the original worker role set may
		// use that binding; an explicit compute:read grant remains global above.
		if !validWorkerIssuerRoleSet(account) {
			return false
		}
		return workerAccountOwnsName(account, workerName)
	}
	// Preserve legacy non-Grove compute:write Watch clients that predate scoped
	// worker accounts. Grove-issued accounts without a binding are denied below.
	if strings.HasPrefix(account.Name, "grove-worker-") {
		return false
	}
	return true
}
