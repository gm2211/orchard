package controller

import (
	"testing"

	v1 "github.com/cirruslabs/orchard/pkg/resource/v1"
	"github.com/stretchr/testify/require"
)

func TestValidWorkerIssuedAccount(t *testing.T) {
	require.True(t, validWorkerIssuedAccount(&v1.ServiceAccount{
		Meta:  v1.Meta{Name: "grove-worker-mac-123"},
		Roles: []v1.ServiceAccountRole{v1.ServiceAccountRoleComputeWrite, v1.ServiceAccountRoleComputeConnect},
	}))
	require.False(t, validWorkerIssuedAccount(&v1.ServiceAccount{
		Meta:  v1.Meta{Name: "admin"},
		Roles: []v1.ServiceAccountRole{v1.ServiceAccountRoleComputeWrite, v1.ServiceAccountRoleComputeConnect},
	}))
	require.False(t, validWorkerIssuedAccount(&v1.ServiceAccount{
		Meta:  v1.Meta{Name: "grove-worker-escalation"},
		Roles: []v1.ServiceAccountRole{v1.ServiceAccountRoleAdminWrite, v1.ServiceAccountRoleComputeConnect},
	}))
	require.False(t, validWorkerIssuedAccount(&v1.ServiceAccount{
		Meta:  v1.Meta{Name: "grove-worker-missing-connect"},
		Roles: []v1.ServiceAccountRole{v1.ServiceAccountRoleComputeWrite},
	}))
}

func TestWorkerIssueRoleRoundTrips(t *testing.T) {
	role, err := v1.NewServiceAccountRole("service-account:issue-worker")
	require.NoError(t, err)
	require.Equal(t, v1.ServiceAccountRoleWorkerIssue, role)
	require.Contains(t, v1.AllServiceAccountRoles(), role)
}
