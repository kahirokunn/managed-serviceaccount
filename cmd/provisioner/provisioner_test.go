package provisioner

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"

	managerprovisioner "open-cluster-management.io/managed-serviceaccount/pkg/addon/manager/provisioner"
)

func TestNewProvisionerCommand(t *testing.T) {
	cmd := NewProvisioner()

	assert.Equal(t, "managed-kubeconfig-provisioner", cmd.Use)
	assert.NotEmpty(t, cmd.Short)
	assert.NotNil(t, cmd.Run)

	for _, name := range []string{
		"cluster-name",
		"source-namespace",
		"source-secret",
		"target-namespace",
		"target-secret",
		"managed-serviceaccount-namespace",
		"managed-serviceaccount-name",
		"token-expiration-seconds",
		"refresh-before",
		"sync-interval",
		"cleanup",
	} {
		assert.NotNil(t, cmd.Flags().Lookup(name), "flag %q must be registered", name)
	}
}

func TestAddFlagsAppliesExpectedDefaults(t *testing.T) {
	opts := NewProvisionerOptions()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	opts.AddFlags(fs)

	assert.NoError(t, fs.Parse(nil))

	assert.Equal(t, "", opts.ClusterName)
	assert.Equal(t, "", opts.SourceNamespace)
	assert.Equal(t, managerprovisioner.DefaultExternalManagedKubeConfigSecret, opts.SourceSecret)
	assert.Equal(t, "", opts.TargetNamespace)
	assert.Equal(t, "", opts.TargetSecret)
	assert.Equal(t, "", opts.ManagedServiceAccountNamespace)
	assert.Equal(t, managerprovisioner.DefaultManagedServiceAccountName, opts.ManagedServiceAccountName)
	assert.Equal(t, managerprovisioner.DefaultTokenExpirationSeconds, opts.TokenExpirationSeconds)
	assert.Equal(t, managerprovisioner.DefaultRefreshBefore, opts.RefreshBefore)
	assert.Equal(t, managerprovisioner.DefaultSyncInterval, opts.SyncInterval)
	assert.False(t, opts.Cleanup)
}

func TestAddFlagsParsesProvidedValues(t *testing.T) {
	opts := NewProvisionerOptions()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	opts.AddFlags(fs)

	args := []string{
		"--cluster-name=cluster-1",
		"--source-namespace=source-ns",
		"--source-secret=source-kubeconfig",
		"--target-namespace=target-ns",
		"--target-secret=target-kubeconfig",
		"--managed-serviceaccount-namespace=msa-ns",
		"--managed-serviceaccount-name=msa-name",
		"--token-expiration-seconds=7200",
		"--refresh-before=15m",
		"--sync-interval=2m",
		"--cleanup=true",
	}
	assert.NoError(t, fs.Parse(args))

	assert.Equal(t, "cluster-1", opts.ClusterName)
	assert.Equal(t, "source-ns", opts.SourceNamespace)
	assert.Equal(t, "source-kubeconfig", opts.SourceSecret)
	assert.Equal(t, "target-ns", opts.TargetNamespace)
	assert.Equal(t, "target-kubeconfig", opts.TargetSecret)
	assert.Equal(t, "msa-ns", opts.ManagedServiceAccountNamespace)
	assert.Equal(t, "msa-name", opts.ManagedServiceAccountName)
	assert.Equal(t, int64(7200), opts.TokenExpirationSeconds)
	assert.Equal(t, 15*time.Minute, opts.RefreshBefore)
	assert.Equal(t, 2*time.Minute, opts.SyncInterval)
	assert.True(t, opts.Cleanup)
}

func TestRunReconcileCleanupInvokesCleanupAndReturns(t *testing.T) {
	opts := validOptions()
	opts.Cleanup = true
	stub := &stubReconciler{}

	err := opts.runReconcile(context.Background(), stub)

	assert.NoError(t, err)
	assert.Equal(t, int64(1), atomic.LoadInt64(&stub.cleanupCalls))
	assert.Equal(t, int64(0), atomic.LoadInt64(&stub.syncCalls))
}

func TestRunReconcileCleanupPropagatesError(t *testing.T) {
	opts := validOptions()
	opts.Cleanup = true
	stub := &stubReconciler{cleanupErr: errors.New("cleanup failed")}

	err := opts.runReconcile(context.Background(), stub)

	assert.ErrorContains(t, err, "cleanup failed")
}

func TestRunReconcileLoopSyncsUntilContextCancelled(t *testing.T) {
	opts := validOptions()
	opts.SyncInterval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	stub := &stubReconciler{
		onSync: func(count int64) error {
			if count >= 3 {
				cancel()
			}
			return nil
		},
	}

	err := opts.runReconcile(ctx, stub)

	assert.ErrorIs(t, err, context.Canceled)
	assert.GreaterOrEqual(t, atomic.LoadInt64(&stub.syncCalls), int64(3))
	assert.Equal(t, int64(0), atomic.LoadInt64(&stub.cleanupCalls))
}

func TestRunReconcileLoopContinuesAfterSyncError(t *testing.T) {
	opts := validOptions()
	opts.SyncInterval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	stub := &stubReconciler{
		onSync: func(count int64) error {
			if count >= 2 {
				cancel()
				return nil
			}
			return fmt.Errorf("transient sync error %d", count)
		},
	}

	err := opts.runReconcile(ctx, stub)

	assert.ErrorIs(t, err, context.Canceled)
	assert.GreaterOrEqual(t, atomic.LoadInt64(&stub.syncCalls), int64(2))
}

func TestRunReconcileLoopRetriesQuicklyAfterTransientError(t *testing.T) {
	// a transient failure must retry on the error backoff, not the full --sync-interval
	opts := validOptions()
	opts.SyncInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	stub := &stubReconciler{
		onSync: func(count int64) error {
			if count == 1 {
				return errors.New("transient")
			}
			cancel()
			return nil
		},
	}

	start := time.Now()
	err := opts.runReconcile(ctx, stub)
	elapsed := time.Since(start)

	assert.ErrorIs(t, err, context.Canceled)
	assert.GreaterOrEqual(t, atomic.LoadInt64(&stub.syncCalls), int64(2))
	assert.Less(t, elapsed, 30*time.Second)
}

func TestRunReconcileLoopReturnsImmediatelyWhenContextAlreadyCancelled(t *testing.T) {
	opts := validOptions()
	opts.SyncInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stub := &stubReconciler{}

	done := make(chan error, 1)
	go func() {
		done <- opts.runReconcile(ctx, stub)
	}()

	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("runReconcile did not return after context cancellation")
	}

	// Sync runs once before the cancellation check; Cleanup must not be invoked in sync mode.
	assert.Equal(t, int64(1), atomic.LoadInt64(&stub.syncCalls))
	assert.Equal(t, int64(0), atomic.LoadInt64(&stub.cleanupCalls))
}

func validOptions() *ProvisionerOptions {
	return &ProvisionerOptions{
		ClusterName:                    "cluster-1",
		SourceNamespace:                "source-ns",
		SourceSecret:                   managerprovisioner.DefaultExternalManagedKubeConfigSecret,
		TargetNamespace:                "addon-ns",
		TargetSecret:                   "target-kubeconfig",
		ManagedServiceAccountNamespace: "addon-ns",
		ManagedServiceAccountName:      managerprovisioner.DefaultManagedServiceAccountName,
		TokenExpirationSeconds:         managerprovisioner.DefaultTokenExpirationSeconds,
		RefreshBefore:                  managerprovisioner.DefaultRefreshBefore,
		SyncInterval:                   5 * time.Minute,
	}
}

type stubReconciler struct {
	syncCalls    int64
	cleanupCalls int64
	cleanupErr   error
	onSync       func(count int64) error
}

func (s *stubReconciler) Sync(ctx context.Context) error {
	count := atomic.AddInt64(&s.syncCalls, 1)
	if s.onSync == nil {
		return nil
	}
	return s.onSync(count)
}

func (s *stubReconciler) Cleanup(ctx context.Context) error {
	atomic.AddInt64(&s.cleanupCalls, 1)
	return s.cleanupErr
}
