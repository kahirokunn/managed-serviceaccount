package provisioner

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/client-go/kubernetes"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// so external source kubeconfigs using those auth providers keep working.
	_ "k8s.io/client-go/plugin/pkg/client/auth" //nolint:revive // required for auth plugins
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	managerprovisioner "open-cluster-management.io/managed-serviceaccount/pkg/addon/manager/provisioner"
)

type ProvisionerOptions struct {
	ClusterName string

	SourceNamespace string
	SourceSecret    string
	TargetNamespace string
	TargetSecret    string

	ManagedServiceAccountNamespace string
	ManagedServiceAccountName      string
	TokenExpirationSeconds         int64
	RefreshBefore                  time.Duration
	SyncInterval                   time.Duration

	Cleanup bool
}

func NewProvisioner() *cobra.Command {
	opts := NewProvisionerOptions()

	cmd := &cobra.Command{
		Use:   "managed-kubeconfig-provisioner",
		Short: "Provision a least-privilege managed cluster kubeconfig for hosted mode",
		Run: func(cmd *cobra.Command, args []string) {
			// A canceled context is the normal SIGTERM shutdown path, not a failure.
			if err := opts.Run(ctrl.SetupSignalHandler()); err != nil && !errors.Is(err, context.Canceled) {
				klog.Fatal(err)
			}
		},
	}

	opts.AddFlags(cmd.Flags())
	return cmd
}

func NewProvisionerOptions() *ProvisionerOptions {
	return &ProvisionerOptions{}
}

func (o *ProvisionerOptions) AddFlags(flags *pflag.FlagSet) {
	flags.StringVar(&o.ClusterName, "cluster-name", "", "The managed cluster name.")
	flags.StringVar(&o.SourceNamespace, "source-namespace", "", "The namespace containing the external managed kubeconfig secret. Defaults to --cluster-name.")
	flags.StringVar(&o.SourceSecret, "source-secret", managerprovisioner.DefaultExternalManagedKubeConfigSecret, "The external managed kubeconfig secret name.")
	flags.StringVar(&o.TargetNamespace, "target-namespace", "", "The namespace where the generated managed kubeconfig secret is stored.")
	flags.StringVar(&o.TargetSecret, "target-secret", "", "The generated managed kubeconfig secret name.")
	flags.StringVar(&o.ManagedServiceAccountNamespace, "managed-serviceaccount-namespace", "", "The managed cluster namespace containing the agent service account. Defaults to --target-namespace.")
	flags.StringVar(&o.ManagedServiceAccountName, "managed-serviceaccount-name", managerprovisioner.DefaultManagedServiceAccountName, "The managed cluster service account used by the agent.")
	flags.Int64Var(&o.TokenExpirationSeconds, "token-expiration-seconds", managerprovisioner.DefaultTokenExpirationSeconds, "Requested TokenRequest expiration seconds.")
	flags.DurationVar(&o.RefreshBefore, "refresh-before", managerprovisioner.DefaultRefreshBefore, "Refresh the generated kubeconfig when the token expires within this duration.")
	flags.DurationVar(&o.SyncInterval, "sync-interval", managerprovisioner.DefaultSyncInterval, "How often to reconcile the generated kubeconfig secret.")
	flags.BoolVar(&o.Cleanup, "cleanup", false, "Delete the generated managed kubeconfig secret and exit.")
}

type reconciler interface {
	Sync(ctx context.Context) error
	Cleanup(ctx context.Context) error
}

const (
	initialErrorBackoff = time.Second
	maxErrorBackoff     = time.Minute
)

func (o *ProvisionerOptions) Run(ctx context.Context) error {
	klog.SetOutput(os.Stdout)
	klog.InitFlags(flag.CommandLine)

	if len(o.SourceNamespace) == 0 {
		o.SourceNamespace = o.ClusterName
	}
	if !o.Cleanup {
		if len(o.SourceNamespace) == 0 {
			return fmt.Errorf("--cluster-name or --source-namespace is required")
		}
		if o.SyncInterval <= 0 {
			return fmt.Errorf("--sync-interval must be a positive duration, got %s", o.SyncInterval)
		}
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("failed to load hosting cluster in-cluster config: %w", err)
	}
	hostingClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("failed to build hosting cluster client: %w", err)
	}

	p := &managerprovisioner.Provisioner{
		HostingClient:                  hostingClient,
		SourceNamespace:                o.SourceNamespace,
		SourceSecret:                   o.SourceSecret,
		TargetNamespace:                o.TargetNamespace,
		TargetSecret:                   o.TargetSecret,
		ManagedServiceAccountNamespace: o.ManagedServiceAccountNamespace,
		ManagedServiceAccountName:      o.ManagedServiceAccountName,
		TokenExpirationSeconds:         o.TokenExpirationSeconds,
		RefreshBefore:                  o.RefreshBefore,
	}
	if err := p.Complete(); err != nil {
		return err
	}
	return o.runReconcile(ctx, p)
}

func (o *ProvisionerOptions) runReconcile(ctx context.Context, p reconciler) error {
	if o.Cleanup {
		return p.Cleanup(ctx)
	}

	// Back off exponentially on failure so a transient error doesn't delay the
	// first success by a full --sync-interval.
	errorBackoff := initialErrorBackoff
	for {
		var wait time.Duration
		if err := p.Sync(ctx); err != nil {
			klog.ErrorS(err, "failed to provision managed kubeconfig")
			wait = min(errorBackoff, o.SyncInterval)
			errorBackoff = min(errorBackoff*2, maxErrorBackoff)
		} else {
			errorBackoff = initialErrorBackoff
			wait = o.SyncInterval
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
