package controller

import (
	"errors"
	"net/http"
	"strings"
	"time"

	storepkg "github.com/cirruslabs/orchard/internal/controller/store"
	"github.com/cirruslabs/orchard/internal/responder"
	"github.com/cirruslabs/orchard/internal/simplename"
	v1 "github.com/cirruslabs/orchard/pkg/resource/v1"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func (controller *Controller) createServiceAccount(ctx *gin.Context) responder.Responder {
	if responder := controller.authorizeAny(ctx, v1.ServiceAccountRoleAdminWrite, v1.ServiceAccountRoleWorkerIssue); responder != nil {
		return responder
	}

	var serviceAccount v1.ServiceAccount

	if err := ctx.ShouldBindJSON(&serviceAccount); err != nil {
		return responder.JSON(http.StatusBadRequest, NewErrorResponse("invalid JSON was provided"))
	}

	// Validate service account name
	if serviceAccount.Name == "" {
		return responder.JSON(http.StatusPreconditionFailed,
			NewErrorResponse("service account name is empty"))
	} else if err := simplename.Validate(serviceAccount.Name); err != nil {
		return responder.JSON(http.StatusPreconditionFailed,
			NewErrorResponse("service account %v", err))
	}

	// Validate roles
	for _, role := range serviceAccount.Roles {
		_, err := v1.NewServiceAccountRole(string(role))
		if err != nil {
			return responder.JSON(http.StatusPreconditionFailed,
				NewErrorResponse("unsupported role \"%s\"", role))
		}
	}
	if !callerHasRole(ctx, v1.ServiceAccountRoleAdminWrite) && !validWorkerIssuedAccount(&serviceAccount) {
		return responder.JSON(http.StatusForbidden,
			NewErrorResponse("worker issuer may create only grove-worker-* accounts with compute:write and compute:connect roles"))
	}

	if serviceAccount.Token == "" {
		serviceAccount.Token = uuid.New().String()
	}

	serviceAccount.CreatedAt = time.Now()

	return controller.storeUpdate(func(txn storepkg.Transaction) responder.Responder {
		// Does the Service Account resource with this name already exists?
		_, err := txn.GetServiceAccount(serviceAccount.Name)
		if err != nil && !errors.Is(err, storepkg.ErrNotFound) {
			controller.logger.Errorf("failed to check if the service account exists in the DB: %v", err)

			return responder.Code(http.StatusInternalServerError)
		}
		if err == nil {
			return responder.JSON(http.StatusConflict,
				NewErrorResponse("service account with this name already exists"))
		}

		if err := txn.SetServiceAccount(&serviceAccount); err != nil {
			controller.logger.Errorf("failed to create the service account in the DB: %v", err)

			return responder.Code(http.StatusInternalServerError)
		}

		return responder.JSON(http.StatusOK, &serviceAccount)
	})
}

func callerHasRole(ctx *gin.Context, required v1.ServiceAccountRole) bool {
	untyped, ok := ctx.Get(ctxServiceAccountKey)
	if !ok {
		return false
	}
	account, ok := untyped.(*v1.ServiceAccount)
	if !ok {
		return false
	}
	for _, role := range account.Roles {
		if role == required {
			return true
		}
	}
	return false
}

func validWorkerIssuedAccount(account *v1.ServiceAccount) bool {
	if !strings.HasPrefix(account.Name, "grove-worker-") || len(account.Roles) != 2 {
		return false
	}
	var write, connect bool
	for _, role := range account.Roles {
		switch role {
		case v1.ServiceAccountRoleComputeWrite:
			write = true
		case v1.ServiceAccountRoleComputeConnect:
			connect = true
		default:
			return false
		}
	}
	return write && connect
}

func (controller *Controller) updateServiceAccount(ctx *gin.Context) responder.Responder {
	if responder := controller.authorize(ctx, v1.ServiceAccountRoleAdminWrite); responder != nil {
		return responder
	}

	var userServiceAccount v1.ServiceAccount

	if err := ctx.ShouldBindJSON(&userServiceAccount); err != nil {
		return responder.JSON(http.StatusBadRequest, NewErrorResponse("invalid JSON was provided"))
	}

	// Validate service account name
	if userServiceAccount.Name == "" {
		return responder.JSON(http.StatusPreconditionFailed,
			NewErrorResponse("service account name is empty"))
	} else if err := simplename.Validate(userServiceAccount.Name); err != nil {
		return responder.JSON(http.StatusPreconditionFailed,
			NewErrorResponse("service account %v", err))
	}

	// Validate roles
	for _, role := range userServiceAccount.Roles {
		_, err := v1.NewServiceAccountRole(string(role))
		if err != nil {
			return responder.JSON(http.StatusPreconditionFailed,
				NewErrorResponse("unsupported role \"%s\"", role))
		}
	}

	if userServiceAccount.Token == "" {
		return responder.JSON(http.StatusPreconditionFailed, NewErrorResponse("service account token is empty"))
	}

	return controller.storeUpdate(func(txn storepkg.Transaction) responder.Responder {
		dbServiceAccount, err := txn.GetServiceAccount(userServiceAccount.Name)
		if err != nil {
			return responder.Error(err)
		}

		dbServiceAccount.Token = userServiceAccount.Token
		dbServiceAccount.Roles = userServiceAccount.Roles

		if err := txn.SetServiceAccount(dbServiceAccount); err != nil {
			controller.logger.Errorf("failed to update service account in the DB: %v", err)

			return responder.Code(http.StatusInternalServerError)
		}

		return responder.JSON(http.StatusOK, &dbServiceAccount)
	})
}

func (controller *Controller) getServiceAccount(ctx *gin.Context) responder.Responder {
	if responder := controller.authorize(ctx, v1.ServiceAccountRoleAdminRead); responder != nil {
		return responder
	}

	name := ctx.Param("name")

	return controller.storeView(func(txn storepkg.Transaction) responder.Responder {
		serviceAccount, err := txn.GetServiceAccount(name)
		if err != nil {
			return responder.Error(err)
		}

		return responder.JSON(http.StatusOK, &serviceAccount)
	})
}

func (controller *Controller) listServiceAccounts(ctx *gin.Context) responder.Responder {
	if responder := controller.authorize(ctx, v1.ServiceAccountRoleAdminRead); responder != nil {
		return responder
	}

	return controller.storeView(func(txn storepkg.Transaction) responder.Responder {
		serviceAccounts, err := txn.ListServiceAccounts()
		if err != nil {
			return responder.Error(err)
		}

		return responder.JSON(http.StatusOK, &serviceAccounts)
	})
}

func (controller *Controller) deleteServiceAccount(ctx *gin.Context) responder.Responder {
	if responder := controller.authorize(ctx, v1.ServiceAccountRoleAdminWrite); responder != nil {
		return responder
	}

	name := ctx.Param("name")

	return controller.storeUpdate(func(txn storepkg.Transaction) responder.Responder {
		if err := txn.DeleteServiceAccount(name); err != nil {
			return responder.Error(err)
		}

		return responder.Code(http.StatusOK)
	})
}
