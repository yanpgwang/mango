package temporal

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/sandbox"
	"github.com/yanpgwang/mango/internal/sandbox/sandboxtest"
	"go.temporal.io/sdk/activity"
	temporalsdk "go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

type releaseMemoryReconciler struct {
	mounts    []sandbox.MemoryStoreMount
	wroteBack bool
}

func (*releaseMemoryReconciler) Reconcile(context.Context, string, sandbox.Sandbox) error {
	return nil
}

func (r *releaseMemoryReconciler) MemoryStoreMountsForRelease(
	context.Context,
	string,
) ([]sandbox.MemoryStoreMount, error) {
	return append([]sandbox.MemoryStoreMount(nil), r.mounts...), nil
}

func (r *releaseMemoryReconciler) WritebackForRelease(
	context.Context,
	string,
	sandbox.Sandbox,
) error {
	r.wroteBack = true
	return nil
}

type releaseMemoryLease struct {
	box      sandbox.Sandbox
	spec     sandbox.Spec
	released bool
}

func (l *releaseMemoryLease) Acquire(
	_ context.Context,
	_ string,
	spec sandbox.Spec,
) (sandbox.Sandbox, error) {
	l.spec = spec
	return l.box, nil
}

func (l *releaseMemoryLease) Release(context.Context, string) error {
	l.released = true
	return nil
}

func TestSandboxCleanupWorkflow_ReleasesSessionSandbox(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	released := ""
	env.RegisterActivityWithOptions(
		func(_ context.Context, in ReleaseSandboxInput) error {
			released = in.SessionID
			return nil
		},
		activity.RegisterOptions{Name: ActivityReleaseSandbox},
	)

	env.ExecuteWorkflow(
		SandboxCleanupWorkflow,
		ReleaseSandboxInput{SessionID: "sesn_cleanup"},
	)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, "sesn_cleanup", released)
}

func TestSandboxCleanupWorkflow_RetriesTransientReleaseFailure(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	attempts := 0
	env.RegisterActivityWithOptions(
		func(context.Context, ReleaseSandboxInput) error {
			attempts++
			if attempts < 3 {
				return errors.New("provider temporarily unavailable")
			}
			return nil
		},
		activity.RegisterOptions{Name: ActivityReleaseSandbox},
	)

	env.ExecuteWorkflow(
		SandboxCleanupWorkflow,
		ReleaseSandboxInput{SessionID: "sesn_retry"},
	)

	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, 3, attempts)
}

type permanentReleaseLease struct{}

func (permanentReleaseLease) Acquire(
	context.Context,
	string,
	sandbox.Spec,
) (sandbox.Sandbox, error) {
	return nil, nil
}

func (permanentReleaseLease) Release(context.Context, string) error {
	return sandbox.Permanent(errors.New("sandbox belongs to another provider"))
}

func TestReleaseSandbox_MapsPermanentProviderErrorToNonRetryable(t *testing.T) {
	activities := NewActivities(nil, nil, nil, permanentReleaseLease{}, nil)
	err := activities.ReleaseSandbox(
		context.Background(),
		ReleaseSandboxInput{SessionID: "sesn_permanent"},
	)
	var applicationError *temporalsdk.ApplicationError
	require.ErrorAs(t, err, &applicationError)
	require.True(t, applicationError.NonRetryable())
	require.Equal(t, sandboxPermanentErrorType, applicationError.Type())
}

func TestReleaseSandbox_FlushesMemoryBeforeProviderRelease(t *testing.T) {
	ctx := context.Background()
	box := sandboxtest.Inert(t)
	lease := &releaseMemoryLease{box: box}
	reconciler := &releaseMemoryReconciler{mounts: []sandbox.MemoryStoreMount{{
		Identity: "sesrsc_memory", StoreID: "memstore_memory",
		RuntimePath: "/mnt/memory/project", Access: domain.MemoryAccessReadWrite,
	}}}
	activities := NewActivities(
		nil, newFakeSource(nil), nil, lease, &testIDGen{},
	).WithSandboxResourceReconciler(reconciler)

	err := activities.ReleaseSandbox(ctx, ReleaseSandboxInput{SessionID: "sess_memory"})
	require.NoError(t, err)
	require.True(t, reconciler.wroteBack)
	require.True(t, lease.released)
	require.Equal(t, reconciler.mounts, lease.spec.MemoryStores)
}
