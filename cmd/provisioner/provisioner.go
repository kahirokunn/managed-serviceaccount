package provisioner

import (
	"context"
	"flag"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	managerprovisioner "open-cluster-management.io/managed-serviceaccount/pkg/addon/manager/provisioner"
)

type ManagedKubeconfigProvisionerOptions struct {
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

func NewManagedKubeconfigProvisioner() *cobra.Command {
	opts := NewManagedKubeconfigProvisionerOptions()

	cmd := &cobra.Command{
		Use:   "managed-kubeconfig-provisioner",
		Short: "Provision a least-privilege managed cluster kubeconfig for hosted mode",
		Run: func(cmd *cobra.Command, args []string) {
			if err := opts.Run(context.Background()); err != nil {
				klog.Fatal(err)
			}
		},
	}

	opts.AddFlags(cmd.Flags())
	return cmd
}

func NewManagedKubeconfigProvisionerOptions() *ManagedKubeconfigProvisionerOptions {
	return &ManagedKubeconfigProvisionerOptions{
		SourceSecret:              managerprovisioner.DefaultExternalManagedKubeConfigSecret,
		ManagedServiceAccountName: managerprovisioner.DefaultManagedServiceAccountName,
		TokenExpirationSeconds:    managerprovisioner.DefaultTokenExpirationSeconds,
		RefreshBefore:             managerprovisioner.DefaultRefreshBefore,
		SyncInterval:              5 * time.Minute,
	}
}

func (o *ManagedKubeconfigProvisionerOptions) AddFlags(flags *pflag.FlagSet) {
	flags.StringVar(&o.ClusterName, "cluster-name", "", "The managed cluster name.")
	flags.StringVar(&o.SourceNamespace, "source-namespace", "", "The namespace containing the external managed kubeconfig secret. Defaults to --cluster-name.")
	flags.StringVar(&o.SourceSecret, "source-secret", managerprovisioner.DefaultExternalManagedKubeConfigSecret, "The external managed kubeconfig secret name.")
	flags.StringVar(&o.TargetNamespace, "target-namespace", "", "The namespace where the generated managed kubeconfig secret is stored.")
	flags.StringVar(&o.TargetSecret, "target-secret", "", "The generated managed kubeconfig secret name.")
	flags.StringVar(&o.ManagedServiceAccountNamespace, "managed-serviceaccount-namespace", "", "The managed cluster namespace containing the agent service account. Defaults to --target-namespace.")
	flags.StringVar(&o.ManagedServiceAccountName, "managed-serviceaccount-name", managerprovisioner.DefaultManagedServiceAccountName, "The managed cluster service account used by the agent.")
	flags.Int64Var(&o.TokenExpirationSeconds, "token-expiration-seconds", managerprovisioner.DefaultTokenExpirationSeconds, "Requested TokenRequest expiration seconds.")
	flags.DurationVar(&o.RefreshBefore, "refresh-before", managerprovisioner.DefaultRefreshBefore, "Refresh the generated kubeconfig when the token expires within this duration.")
	flags.DurationVar(&o.SyncInterval, "sync-interval", 5*time.Minute, "How often to reconcile the generated kubeconfig secret.")
	flags.BoolVar(&o.Cleanup, "cleanup", false, "Delete the generated managed kubeconfig secret and exit.")
}

func (o *ManagedKubeconfigProvisionerOptions) Run(ctx context.Context) error {
	klog.SetOutput(os.Stdout)
	klog.InitFlags(flag.CommandLine)

	if len(o.SourceNamespace) == 0 {
		o.SourceNamespace = o.ClusterName
	}
	if len(o.ManagedServiceAccountNamespace) == 0 {
		o.ManagedServiceAccountNamespace = o.TargetNamespace
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	hostingClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
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
	if o.Cleanup {
		return p.Cleanup(ctx)
	}

	for {
		if err := p.Sync(ctx); err != nil {
			klog.ErrorS(err, "failed to provision managed kubeconfig")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(o.SyncInterval):
		}
	}
}
