package framework

import (
	"flag"
	"os"
	"path/filepath"

	"k8s.io/klog/v2"
)

var e2eContext = &E2EContext{}

type E2EContext struct {
	HubKubeConfig                      string
	SpokeKubeConfig                    string
	AgentKubeConfig                    string
	TestCluster                        string
	ExternalManagedKubeConfigNamespace string
	ExternalManagedKubeConfigSecret    string
}

func ParseFlags() {
	registerFlags()
	flag.Parse()
	defaultFlags()
	validateFlags()
}

func registerFlags() {
	flag.StringVar(&e2eContext.HubKubeConfig,
		"hub-kubeconfig",
		os.Getenv("KUBECONFIG"),
		"Path to kubeconfig of the hub cluster.")
	flag.StringVar(&e2eContext.SpokeKubeConfig,
		"spoke-kubeconfig",
		"",
		"Path to kubeconfig of the managed/spoke cluster. Defaults to --hub-kubeconfig.")
	flag.StringVar(&e2eContext.AgentKubeConfig,
		"agent-kubeconfig",
		"",
		"Path to kubeconfig of the cluster where the addon agent deployment runs. Defaults to --spoke-kubeconfig.")
	flag.StringVar(&e2eContext.TestCluster,
		"test-cluster",
		"",
		"The target cluster to run the e2e suite.")
	flag.StringVar(&e2eContext.ExternalManagedKubeConfigNamespace,
		"external-managed-kubeconfig-namespace",
		"",
		"Namespace of the hosted-mode external managed kubeconfig source secret, propagated through AddOnDeploymentConfig when set.")
	flag.StringVar(&e2eContext.ExternalManagedKubeConfigSecret,
		"external-managed-kubeconfig-secret",
		"",
		"Name of the hosted-mode external managed kubeconfig source secret, propagated through AddOnDeploymentConfig when set.")
}

func defaultFlags() {
	if len(e2eContext.HubKubeConfig) == 0 {
		home := os.Getenv("HOME")
		if len(home) > 0 {
			e2eContext.HubKubeConfig = filepath.Join(home, ".kube", "config")
		}
	}
	if len(e2eContext.SpokeKubeConfig) == 0 {
		e2eContext.SpokeKubeConfig = e2eContext.HubKubeConfig
	}
	if len(e2eContext.AgentKubeConfig) == 0 {
		e2eContext.AgentKubeConfig = e2eContext.SpokeKubeConfig
	}
}

func validateFlags() {
	if len(e2eContext.HubKubeConfig) == 0 {
		klog.Fatalf("--hub-kubeconfig is required")
	}
	if len(e2eContext.TestCluster) == 0 {
		klog.Fatalf("--test-cluster is required")
	}
}
