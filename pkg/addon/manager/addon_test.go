package manager

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	fakekube "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"open-cluster-management.io/addon-framework/pkg/addonfactory"
	"open-cluster-management.io/addon-framework/pkg/utils"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	"open-cluster-management.io/managed-serviceaccount/pkg/addon/manager/provisioner"
	"open-cluster-management.io/managed-serviceaccount/pkg/common"
)

func TestNewRegistrationOption(t *testing.T) {
	clusterName := "cluster1"
	fakeKubeClient := fakekube.NewSimpleClientset()

	registrationOptions := NewRegistrationOption(fakeKubeClient)
	assert.NotNil(t, registrationOptions.PermissionConfig, "permissionConfig is not specified")

	err := registrationOptions.PermissionConfig(newTestCluster(clusterName), newTestAddOn("addon", clusterName))
	assert.NoError(t, err)

	actions := fakeKubeClient.Actions()
	assert.Len(t, actions, 2)
	role := actions[0].(clienttesting.CreateAction).GetObject().(*rbacv1.Role)
	assert.Equal(t, clusterName, role.Namespace, "invalid role ns")
	assert.Equal(t, "managed-serviceaccount-addon-agent", role.Name, "invalid role name")
	rolebinding := actions[1].(clienttesting.CreateAction).GetObject().(*rbacv1.RoleBinding)
	assert.Equal(t, clusterName, rolebinding.Namespace, "invalid rolebinding ns")
	assert.Equal(t, "managed-serviceaccount-addon-agent", rolebinding.Name, "invalid rolebinding name")
}

func TestManifestAddonAgent(t *testing.T) {
	clusterName := "cluster1"
	addonName := "addon1"
	imageName := "imageName1"
	manifestNames := []string{
		addonName,
		"managed-serviceaccount",
		"open-cluster-management:managed-serviceaccount:addon-agent",
		"open-cluster-management:managed-serviceaccount:addon-agent",
		"open-cluster-management:managed-serviceaccount:addon-agent",
		"open-cluster-management:managed-serviceaccount:addon-agent",
		"managed-serviceaccount-addon-agent",
	}

	cases := []struct {
		name                  string
		getValuesFunc         []addonfactory.GetValuesFunc
		expectedManifestNames []string
	}{
		{
			name:                  "install",
			getValuesFunc:         []addonfactory.GetValuesFunc{GetDefaultValues(imageName, nil)},
			expectedManifestNames: manifestNames,
		},
		{
			name:                  "install all with image pull secret",
			getValuesFunc:         []addonfactory.GetValuesFunc{GetDefaultValues(imageName, newTestImagePullSecret())},
			expectedManifestNames: append(manifestNames, "open-cluster-management-image-pull-credentials"),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			agentFactory := NewAgentAddonFactory(common.AddonName, FS, "manifests/templates").
				WithGetValuesFuncs(c.getValuesFunc...)

			addOnAgent, err := agentFactory.BuildTemplateAgentAddon()
			assert.NoError(t, err)

			manifests, err := addOnAgent.Manifests(newTestCluster(clusterName), newTestAddOn(addonName, clusterName))
			assert.NoError(t, err)

			actual := []string{}
			for _, manifest := range manifests {
				obj, ok := manifest.(metav1.ObjectMetaAccessor)
				assert.True(t, ok, "invalid manifest")
				if ns := obj.GetObjectMeta().GetNamespace(); len(ns) > 0 {
					assert.Equalf(t, addonName, ns, "unexpected ns of manifest %q", obj.GetObjectMeta().GetName())
				}
				actual = append(actual, obj.GetObjectMeta().GetName())
			}
			assert.ElementsMatch(t, c.expectedManifestNames, actual)
		})
	}
}

func TestManifestAddonAgentDefaultModeDeployment(t *testing.T) {
	clusterName := "cluster1"
	addonName := "addon1"
	imageName := "imageName1"

	manifests := renderTestManifests(
		t,
		newTestCluster(clusterName),
		newTestAddOn(addonName, clusterName),
		GetDefaultValues(imageName, nil),
	)
	deployment := findDeployment(t, manifests)
	container := deployment.Spec.Template.Spec.Containers[0]

	assert.NotContains(t, deployment.Annotations, addonv1alpha1.HostedManifestLocationAnnotationKey)
	assert.Contains(t, container.Args, "--leader-elect=false")
	assert.Contains(t, container.Args, "--cluster-name="+clusterName)
	assert.Contains(t, container.Args, "--kubeconfig=/etc/hub/kubeconfig")
	assert.Contains(t, container.Args, "--lease-health-check=true")
	assert.NotContains(t, container.Args, "--spoke-kubeconfig=/etc/managed/kubeconfig")
	assert.NotContains(t, container.Args, "--lease-in-cluster-config=true")
	assertDeploymentContainerPort(t, deployment, "metrics", 38080)
	assertDeploymentSecretVolume(t, deployment, "hub-kubeconfig", "managed-serviceaccount-hub-kubeconfig")
	assertDeploymentMissingVolume(t, deployment, "managed-kubeconfig")
}

func TestManifestAddonAgentHostedModeDeployment(t *testing.T) {
	clusterName := "cluster1"
	addonName := "addon1"
	imageName := "imageName1"
	addon := newTestHostedAddOn(addonName, clusterName, "hosting1")

	manifests := renderTestManifests(
		t,
		newTestCluster(clusterName),
		addon,
		GetDefaultValues(imageName, nil),
	)
	deployment := findDeployment(t, manifests)
	container := deployment.Spec.Template.Spec.Containers[0]

	assert.Equal(t,
		addonv1alpha1.HostedManifestLocationHostingValue,
		deployment.Annotations[addonv1alpha1.HostedManifestLocationAnnotationKey])
	assert.Contains(t, container.Args, "--kubeconfig=/etc/hub/kubeconfig")
	assert.Contains(t, container.Args, "--spoke-kubeconfig=/etc/managed/kubeconfig")
	assertDeploymentContainerPort(t, deployment, "metrics", 38080)
	assertDeploymentSecretVolume(t, deployment, "hub-kubeconfig", "managed-serviceaccount-hub-kubeconfig")
	assertDeploymentSecretVolume(t, deployment, "managed-kubeconfig", addonName+"-managed-kubeconfig")
	assertDeploymentVolumeMount(t, deployment, "hub-kubeconfig", "/etc/hub/")
	assertDeploymentVolumeMount(t, deployment, "managed-kubeconfig", "/etc/managed/")
}

func TestManifestAddonAgentMetricsDefaultDisabled(t *testing.T) {
	clusterName := "cluster1"
	addonName := "addon1"
	imageName := "imageName1"

	manifests := renderTestManifests(
		t,
		newTestCluster(clusterName),
		newTestAddOn(addonName, clusterName),
		GetDefaultValues(imageName, nil),
	)

	assertNoService(t, manifests, "managed-serviceaccount-addon-agent")
	assertNoServiceMonitor(t, manifests, "managed-serviceaccount-addon-agent")
}

func TestManifestAddonAgentMetricsServiceOptIn(t *testing.T) {
	clusterName := "cluster1"
	addonName := "addon1"
	imageName := "imageName1"

	manifests := renderTestManifests(
		t,
		newTestCluster(clusterName),
		newTestAddOn(addonName, clusterName),
		GetDefaultValues(imageName, nil),
		func(cluster *clusterv1.ManagedCluster, addon *addonv1alpha1.ManagedClusterAddOn) (addonfactory.Values, error) {
			return addonfactory.Values{
				"AgentMetricsServiceEnabled": "true",
			}, nil
		},
	)

	service := findService(t, manifests, "managed-serviceaccount-addon-agent", "")
	assertAgentMetricsService(t, service)
	assertNoServiceMonitor(t, manifests, "managed-serviceaccount-addon-agent")
}

func TestManifestAddonAgentServiceMonitorOptIn(t *testing.T) {
	clusterName := "cluster1"
	addonName := "addon1"
	imageName := "imageName1"

	manifests := renderTestManifests(
		t,
		newTestCluster(clusterName),
		newTestAddOn(addonName, clusterName),
		GetDefaultValues(imageName, nil),
		func(cluster *clusterv1.ManagedCluster, addon *addonv1alpha1.ManagedClusterAddOn) (addonfactory.Values, error) {
			return addonfactory.Values{
				"AgentServiceMonitorEnabled": "true",
			}, nil
		},
	)

	service := findService(t, manifests, "managed-serviceaccount-addon-agent", "")
	assertAgentMetricsService(t, service)

	serviceMonitor := findServiceMonitor(t, manifests, "managed-serviceaccount-addon-agent", "")
	assertAgentServiceMonitor(t, serviceMonitor)
	assert.Empty(t, serviceMonitor.GetLabels())
}

func TestManifestAddonAgentHostedModeMetricsLocation(t *testing.T) {
	clusterName := "cluster1"
	addonName := "addon1"
	imageName := "imageName1"
	addon := newTestHostedAddOn(addonName, clusterName, "hosting1")

	manifests := renderTestManifests(
		t,
		newTestCluster(clusterName),
		addon,
		GetDefaultValues(imageName, nil),
		func(cluster *clusterv1.ManagedCluster, addon *addonv1alpha1.ManagedClusterAddOn) (addonfactory.Values, error) {
			return addonfactory.Values{
				"AgentServiceMonitorEnabled": "true",
			}, nil
		},
	)

	service := findService(t, manifests, "managed-serviceaccount-addon-agent", addonv1alpha1.HostedManifestLocationHostingValue)
	assertAgentMetricsService(t, service)

	serviceMonitor := findServiceMonitor(t, manifests, "managed-serviceaccount-addon-agent", addonv1alpha1.HostedManifestLocationHostingValue)
	assertAgentServiceMonitor(t, serviceMonitor)
}

func TestManifestAddonAgentServiceMonitorLabelsFromAddOnDeploymentConfig(t *testing.T) {
	clusterName := "cluster1"
	addonName := "addon1"
	imageName := "imageName1"
	addon := newTestAddOn(addonName, clusterName)
	config := &addonv1alpha1.AddOnDeploymentConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "metrics-config",
			Namespace: clusterName,
		},
		Spec: addonv1alpha1.AddOnDeploymentConfigSpec{
			CustomizedVariables: []addonv1alpha1.CustomizedVariable{
				{Name: "AgentServiceMonitorEnabled", Value: "true"},
				{Name: "AgentServiceMonitorLabels", Value: "release=prometheus,team=platform"},
			},
		},
	}
	addon.Status.ConfigReferences = []addonv1alpha1.ConfigReference{
		{
			ConfigGroupResource: addonv1alpha1.ConfigGroupResource{
				Group:    utils.AddOnDeploymentConfigGVR.Group,
				Resource: utils.AddOnDeploymentConfigGVR.Resource,
			},
			DesiredConfig: &addonv1alpha1.ConfigSpecHash{
				ConfigReferent: addonv1alpha1.ConfigReferent{
					Namespace: config.Namespace,
					Name:      config.Name,
				},
				SpecHash: "hash",
			},
		},
	}

	manifests := renderTestManifests(
		t,
		newTestCluster(clusterName),
		addon,
		GetDefaultValues(imageName, nil),
		addonfactory.GetAddOnDeploymentConfigValues(
			fakeAddOnDeploymentConfigGetter{config: config},
			ToAddOnDeploymentConfigValues,
		),
	)

	serviceMonitor := findServiceMonitor(t, manifests, "managed-serviceaccount-addon-agent", "")
	assertAgentServiceMonitor(t, serviceMonitor)
	assert.Equal(t, map[string]string{
		"release": "prometheus",
		"team":    "platform",
	}, serviceMonitor.GetLabels())
}

func TestManifestAddonAgentHostedModeManagedKubeConfigSecretOverride(t *testing.T) {
	clusterName := "cluster1"
	addonName := "addon1"
	imageName := "imageName1"
	managedKubeConfigSecret := "custom-managed-kubeconfig"
	addon := newTestHostedAddOn(addonName, clusterName, "hosting1")

	manifests := renderTestManifests(
		t,
		newTestCluster(clusterName),
		addon,
		GetDefaultValues(imageName, nil),
		func(cluster *clusterv1.ManagedCluster, addon *addonv1alpha1.ManagedClusterAddOn) (addonfactory.Values, error) {
			return addonfactory.Values{
				"ManagedKubeConfigSecret": managedKubeConfigSecret,
			}, nil
		},
	)
	deployment := findDeployment(t, manifests)

	assertDeploymentSecretVolume(t, deployment, "managed-kubeconfig", managedKubeConfigSecret)
}

func TestManifestAddonAgentHostedModeManifestLocations(t *testing.T) {
	clusterName := "cluster1"
	addonName := "addon1"
	imageName := "imageName1"
	addon := newTestHostedAddOn(addonName, clusterName, "hosting1")

	manifests := renderTestManifests(
		t,
		newTestCluster(clusterName),
		addon,
		GetDefaultValues(imageName, newTestImagePullSecret()),
	)

	assert.Equal(t, "", hostedLocation(findServiceAccount(t, manifests, "managed-serviceaccount", "")))
	assert.Equal(t, addonv1alpha1.HostedManifestLocationHostingValue,
		hostedLocation(findServiceAccount(t, manifests, "managed-serviceaccount", addonv1alpha1.HostedManifestLocationHostingValue)))
	assert.Equal(t, "", hostedLocation(findRole(t, manifests, "open-cluster-management:managed-serviceaccount:addon-agent", "")))
	assert.Equal(t, "", hostedLocation(findRoleBinding(t, manifests, "open-cluster-management:managed-serviceaccount:addon-agent", "")))
	assert.Equal(t, addonv1alpha1.HostedManifestLocationHostingValue,
		hostedLocation(findRole(t, manifests, "open-cluster-management:managed-serviceaccount:addon-agent", addonv1alpha1.HostedManifestLocationHostingValue)))
	assert.Equal(t, addonv1alpha1.HostedManifestLocationHostingValue,
		hostedLocation(findRoleBinding(t, manifests, "open-cluster-management:managed-serviceaccount:addon-agent", addonv1alpha1.HostedManifestLocationHostingValue)))

	assert.Equal(t, addonv1alpha1.HostedManifestLocationHostingValue, hostedLocation(findDeployment(t, manifests)))
	assert.Equal(t, addonv1alpha1.HostedManifestLocationHostingValue,
		hostedLocation(findSecret(t, manifests, "open-cluster-management-image-pull-credentials", addonv1alpha1.HostedManifestLocationHostingValue)))
	assert.Equal(t, addonv1alpha1.HostedManifestLocationHostingValue,
		hostedLocation(findDeploymentByName(t, manifests, "managed-serviceaccount-kubeconfig-provisioner")))
	assert.Equal(t, addonv1alpha1.HostedManifestLocationHostingValue,
		hostedLocation(findServiceAccount(t, manifests, "managed-serviceaccount-kubeconfig-provisioner", addonv1alpha1.HostedManifestLocationHostingValue)))
	assert.Equal(t, addonv1alpha1.HostedManifestLocationHostingValue,
		hostedLocation(findRole(t, manifests, "managed-serviceaccount-kubeconfig-provisioner", addonv1alpha1.HostedManifestLocationHostingValue)))
	assert.Equal(t, addonv1alpha1.HostedManifestLocationHostingValue,
		hostedLocation(findRoleBinding(t, manifests, "managed-serviceaccount-kubeconfig-provisioner", addonv1alpha1.HostedManifestLocationHostingValue)))
	assert.Equal(t, addonv1alpha1.HostedManifestLocationHostingValue,
		hostedLocation(findRoleBinding(t, manifests, "managed-serviceaccount-kubeconfig-provisioner-source", addonv1alpha1.HostedManifestLocationHostingValue)))

	assert.Equal(t, addonv1alpha1.HostedManifestLocationManagedValue,
		hostedLocation(findClusterRole(t, manifests, "open-cluster-management:managed-serviceaccount:addon-agent")))
	assert.Equal(t, addonv1alpha1.HostedManifestLocationManagedValue,
		hostedLocation(findClusterRoleBinding(t, manifests, "open-cluster-management:managed-serviceaccount:addon-agent")))
}

func TestManifestAddonAgentHostedModeLeaseRBAC(t *testing.T) {
	clusterName := "cluster1"
	addonName := "addon1"
	imageName := "imageName1"
	addon := newTestHostedAddOn(addonName, clusterName, "hosting1")

	manifests := renderTestManifests(
		t,
		newTestCluster(clusterName),
		addon,
		GetDefaultValues(imageName, nil),
	)

	// Hosted-mode agent writes the lease on the hosting cluster, so the
	// hosting Role/RoleBinding must grant lease perms in the install namespace.
	deployment := findDeployment(t, manifests)
	assert.Contains(t, deployment.Spec.Template.Spec.Containers[0].Args, "--lease-health-check=true")
	assert.Contains(t, deployment.Spec.Template.Spec.Containers[0].Args, "--lease-in-cluster-config=true")

	role := findRole(t, manifests, "open-cluster-management:managed-serviceaccount:addon-agent", addonv1alpha1.HostedManifestLocationHostingValue)
	assert.Equal(t, addonName, role.Namespace)
	assertRule(t, role.Rules, []string{"coordination.k8s.io"}, []string{"leases"}, []string{"create"}, nil)
	assertRule(t, role.Rules, []string{"coordination.k8s.io"}, []string{"leases"}, []string{"get", "update", "patch"}, []string{common.AddonName})

	binding := findRoleBinding(t, manifests, "open-cluster-management:managed-serviceaccount:addon-agent", addonv1alpha1.HostedManifestLocationHostingValue)
	assert.Equal(t, addonName, binding.Namespace)
	assert.Equal(t, "Role", binding.RoleRef.Kind)
	assert.Equal(t, "open-cluster-management:managed-serviceaccount:addon-agent", binding.RoleRef.Name)
	assert.Len(t, binding.Subjects, 1)
	assert.Equal(t, "ServiceAccount", binding.Subjects[0].Kind)
	assert.Equal(t, "managed-serviceaccount", binding.Subjects[0].Name)
	assert.Equal(t, addonName, binding.Subjects[0].Namespace)
}

func TestManifestAddonAgentHostedModeExternalManagedKubeConfigOverrides(t *testing.T) {
	clusterName := "cluster1"
	addonName := "addon1"
	imageName := "imageName1"
	addon := newTestHostedAddOn(addonName, clusterName, "hosting1")

	manifests := renderTestManifests(
		t,
		newTestCluster(clusterName),
		addon,
		GetDefaultValues(imageName, nil),
		func(cluster *clusterv1.ManagedCluster, addon *addonv1alpha1.ManagedClusterAddOn) (addonfactory.Values, error) {
			return addonfactory.Values{
				"ExternalManagedKubeConfigNamespace": "custom-source-ns",
				"ExternalManagedKubeConfigSecret":    "custom-source-secret",
				"ManagedKubeConfigSecret":            "custom-target-secret",
			}, nil
		},
	)

	provisioner := findDeploymentByName(t, manifests, "managed-serviceaccount-kubeconfig-provisioner")
	args := provisioner.Spec.Template.Spec.Containers[0].Args
	assert.Contains(t, args, "--source-namespace=custom-source-ns")
	assert.Contains(t, args, "--source-secret=custom-source-secret")
	assert.Contains(t, args, "--target-secret=custom-target-secret")

	targetRole := findRole(t, manifests, "managed-serviceaccount-kubeconfig-provisioner", addonv1alpha1.HostedManifestLocationHostingValue)
	assert.Equal(t, addonName, targetRole.Namespace)
	assertRule(t, targetRole.Rules, []string{""}, []string{"secrets"}, []string{"get", "update", "patch", "delete"}, []string{"custom-target-secret"})
	assertRule(t, targetRole.Rules, []string{""}, []string{"secrets"}, []string{"create"}, nil)

	sourceRole := findRole(t, manifests, "managed-serviceaccount-kubeconfig-provisioner-source", addonv1alpha1.HostedManifestLocationHostingValue)
	assert.Equal(t, "custom-source-ns", sourceRole.Namespace)
	assertRule(t, sourceRole.Rules, []string{""}, []string{"secrets"}, []string{"get"}, []string{"custom-source-secret"})

	sourceBinding := findRoleBinding(t, manifests, "managed-serviceaccount-kubeconfig-provisioner-source", addonv1alpha1.HostedManifestLocationHostingValue)
	assert.Equal(t, "custom-source-ns", sourceBinding.Namespace)
	assert.Len(t, sourceBinding.Subjects, 1)
	assert.Equal(t, "managed-serviceaccount-kubeconfig-provisioner", sourceBinding.Subjects[0].Name)
	assert.Equal(t, addonName, sourceBinding.Subjects[0].Namespace)
}

func TestManifestAddonAgentHostedModeProvisionerTimingDefaults(t *testing.T) {
	clusterName := "cluster1"
	addonName := "addon1"
	imageName := "imageName1"
	addon := newTestHostedAddOn(addonName, clusterName, "hosting1")

	manifests := renderTestManifests(
		t,
		newTestCluster(clusterName),
		addon,
		GetDefaultValues(imageName, nil),
	)

	prov := findDeploymentByName(t, manifests, "managed-serviceaccount-kubeconfig-provisioner")
	args := prov.Spec.Template.Spec.Containers[0].Args
	assert.Contains(t, args, fmt.Sprintf("--token-expiration-seconds=%d", provisioner.DefaultTokenExpirationSeconds))
	assert.Contains(t, args, fmt.Sprintf("--refresh-before=%ds", int64(provisioner.DefaultRefreshBefore.Seconds())))
	assert.Contains(t, args, fmt.Sprintf("--sync-interval=%s", provisioner.DefaultSyncInterval))
}

func TestManifestAddonAgentHostedModeProvisionerTimingOverrides(t *testing.T) {
	clusterName := "cluster1"
	addonName := "addon1"
	imageName := "imageName1"
	addon := newTestHostedAddOn(addonName, clusterName, "hosting1")

	manifests := renderTestManifests(
		t,
		newTestCluster(clusterName),
		addon,
		GetDefaultValues(imageName, nil),
		func(cluster *clusterv1.ManagedCluster, addon *addonv1alpha1.ManagedClusterAddOn) (addonfactory.Values, error) {
			return addonfactory.Values{
				"ManagedKubeConfigTokenExpirationSeconds":  int64(7200),
				"ManagedKubeConfigRefreshBeforeSeconds":    int64(900),
				"ManagedKubeConfigProvisionerSyncInterval": "30s",
			}, nil
		},
	)

	prov := findDeploymentByName(t, manifests, "managed-serviceaccount-kubeconfig-provisioner")
	args := prov.Spec.Template.Spec.Containers[0].Args
	assert.Contains(t, args, "--token-expiration-seconds=7200")
	assert.Contains(t, args, "--refresh-before=900s")
	assert.Contains(t, args, "--sync-interval=30s")

	assert.NotContains(t, args, fmt.Sprintf("--token-expiration-seconds=%d", provisioner.DefaultTokenExpirationSeconds))
	assert.NotContains(t, args, fmt.Sprintf("--refresh-before=%ds", int64(provisioner.DefaultRefreshBefore.Seconds())))
	assert.NotContains(t, args, fmt.Sprintf("--sync-interval=%s", provisioner.DefaultSyncInterval))
}

func TestManifestAddonAgentDefaultModeManagedServiceAccountNameOverride(t *testing.T) {
	clusterName := "cluster1"
	addonName := "addon1"
	imageName := "imageName1"
	customName := "custom-msa"

	manifests := renderTestManifests(
		t,
		newTestCluster(clusterName),
		newTestAddOn(addonName, clusterName),
		GetDefaultValues(imageName, nil),
		func(cluster *clusterv1.ManagedCluster, addon *addonv1alpha1.ManagedClusterAddOn) (addonfactory.Values, error) {
			return addonfactory.Values{
				"ManagedServiceAccountName": customName,
			}, nil
		},
	)

	sa := findServiceAccount(t, manifests, customName, "")
	assert.Equal(t, addonName, sa.Namespace)

	binding := findRoleBinding(t, manifests, "open-cluster-management:managed-serviceaccount:addon-agent", "")
	assert.Len(t, binding.Subjects, 1)
	assert.Equal(t, customName, binding.Subjects[0].Name)
	assert.Equal(t, addonName, binding.Subjects[0].Namespace)

	clusterBinding := findClusterRoleBinding(t, manifests, "open-cluster-management:managed-serviceaccount:addon-agent")
	assert.Len(t, clusterBinding.Subjects, 1)
	assert.Equal(t, customName, clusterBinding.Subjects[0].Name)
	assert.Equal(t, addonName, clusterBinding.Subjects[0].Namespace)

	deployment := findDeployment(t, manifests)
	assert.Equal(t, customName, deployment.Spec.Template.Spec.ServiceAccountName)
}

func TestManifestAddonAgentHostedModeManagedServiceAccountNameOverride(t *testing.T) {
	clusterName := "cluster1"
	addonName := "addon1"
	imageName := "imageName1"
	customName := "custom-msa"
	addon := newTestHostedAddOn(addonName, clusterName, "hosting1")

	manifests := renderTestManifests(
		t,
		newTestCluster(clusterName),
		addon,
		GetDefaultValues(imageName, nil),
		func(cluster *clusterv1.ManagedCluster, addon *addonv1alpha1.ManagedClusterAddOn) (addonfactory.Values, error) {
			return addonfactory.Values{
				"ManagedServiceAccountName": customName,
			}, nil
		},
	)

	sa := findServiceAccount(t, manifests, customName, "")
	assert.Equal(t, addonName, sa.Namespace)

	binding := findRoleBinding(t, manifests, "open-cluster-management:managed-serviceaccount:addon-agent", "")
	assert.Len(t, binding.Subjects, 1)
	assert.Equal(t, customName, binding.Subjects[0].Name)
	assert.Equal(t, addonName, binding.Subjects[0].Namespace)

	clusterBinding := findClusterRoleBinding(t, manifests, "open-cluster-management:managed-serviceaccount:addon-agent")
	assert.Len(t, clusterBinding.Subjects, 1)
	assert.Equal(t, customName, clusterBinding.Subjects[0].Name)
	assert.Equal(t, addonName, clusterBinding.Subjects[0].Namespace)

	provisioner := findDeploymentByName(t, manifests, "managed-serviceaccount-kubeconfig-provisioner")
	assert.Contains(t, provisioner.Spec.Template.Spec.Containers[0].Args, "--managed-serviceaccount-name="+customName)

	// The hosted-mode agent pod runs as the hosting SA, not the overridden managed-cluster SA.
	deployment := findDeployment(t, manifests)
	assert.Equal(t, "managed-serviceaccount", deployment.Spec.Template.Spec.ServiceAccountName)
	hostingSA := findServiceAccount(t, manifests, "managed-serviceaccount", addonv1alpha1.HostedManifestLocationHostingValue)
	assert.Equal(t, addonName, hostingSA.Namespace)
}

func TestManifestAddonAgentHostedModeCleanupHook(t *testing.T) {
	clusterName := "cluster1"
	addonName := "addon1"
	imageName := "imageName1"
	addon := newTestHostedAddOn(addonName, clusterName, "hosting1")

	manifests := renderTestManifests(
		t,
		newTestCluster(clusterName),
		addon,
		GetDefaultValues(imageName, nil),
	)

	job := findJob(t, manifests, "managed-serviceaccount-kubeconfig-cleanup")
	assert.Equal(t, addonv1alpha1.HostedManifestLocationHostingValue, hostedLocation(job))
	assert.Contains(t, job.Annotations, addonv1alpha1.AddonPreDeleteHookAnnotationKey)
	args := job.Spec.Template.Spec.Containers[0].Args
	assert.Contains(t, args, "--cleanup")
	assert.Contains(t, args, "--target-secret="+addonName+"-managed-kubeconfig")
	// The cleanup job must pass the same source kubeconfig coordinates as the
	// provisioner Deployment so the source/target collision guard (which prevents
	// deleting the source bootstrap secret) also fires during uninstall.
	assert.Contains(t, args, "--source-namespace="+clusterName)
	assert.Contains(t, args, "--source-secret=external-managed-kubeconfig")
}

func TestManifestAddonAgentHostedModeNamespaces(t *testing.T) {
	clusterName := "cluster1"
	addonName := "addon1"
	imageName := "imageName1"
	addon := newTestHostedAddOn(addonName, clusterName, "hosting1")

	manifests := renderTestManifests(
		t,
		newTestCluster(clusterName),
		addon,
		GetDefaultValues(imageName, nil),
	)
	namespaces := findNamespaces(manifests)

	assert.Len(t, namespaces, 2)
	hostingNamespaces := 0
	managedNamespaces := 0
	for _, namespace := range namespaces {
		assert.Equal(t, addonName, namespace.Name)
		if namespace.Annotations[addonv1alpha1.HostedManifestLocationAnnotationKey] == addonv1alpha1.HostedManifestLocationHostingValue {
			hostingNamespaces++
		} else {
			managedNamespaces++
		}
	}
	assert.Equal(t, 1, hostingNamespaces)
	assert.Equal(t, 1, managedNamespaces)
}

func renderTestManifests(t *testing.T,
	cluster *clusterv1.ManagedCluster,
	addon *addonv1alpha1.ManagedClusterAddOn,
	getValuesFuncs ...addonfactory.GetValuesFunc) []runtime.Object {
	t.Helper()

	// Mirror the production factory wiring: hosted mode is always enabled and the
	// addon's hosting annotation decides whether manifests render in hosted mode.
	agentFactory := NewAgentAddonFactory(common.AddonName, FS, "manifests/templates").
		WithGetValuesFuncs(getValuesFuncs...).
		WithAgentRegistrationOption(NewRegistrationOption(fakekube.NewSimpleClientset())).
		WithAgentHostedModeEnabledOption()

	addOnAgent, err := agentFactory.BuildTemplateAgentAddon()
	assert.NoError(t, err)

	manifests, err := addOnAgent.Manifests(cluster, addon)
	assert.NoError(t, err)

	return manifests
}

type fakeAddOnDeploymentConfigGetter struct {
	config *addonv1alpha1.AddOnDeploymentConfig
}

func (g fakeAddOnDeploymentConfigGetter) Get(ctx context.Context, namespace, name string) (*addonv1alpha1.AddOnDeploymentConfig, error) {
	if g.config == nil {
		return nil, fmt.Errorf("addon deployment config %s/%s not found", namespace, name)
	}
	if g.config.Namespace != namespace || g.config.Name != name {
		return nil, fmt.Errorf("addon deployment config %s/%s not found", namespace, name)
	}
	return g.config, nil
}

// findManifest returns the first manifest of type T matching the predicate, or
// fails the test with desc if none matches.
func findManifest[T runtime.Object](t *testing.T, manifests []runtime.Object, desc string, match func(T) bool) T {
	t.Helper()

	for _, manifest := range manifests {
		if obj, ok := manifest.(T); ok && match(obj) {
			return obj
		}
	}

	t.Fatalf("%s not found", desc)
	var zero T
	return zero
}

// assertNoManifest fails the test if any manifest of type T matches the predicate.
func assertNoManifest[T runtime.Object](t *testing.T, manifests []runtime.Object, desc string, match func(T) bool) {
	t.Helper()

	for _, manifest := range manifests {
		if obj, ok := manifest.(T); ok && match(obj) {
			t.Fatalf("%s should not be rendered", desc)
		}
	}
}

func findDeployment(t *testing.T, manifests []runtime.Object) *appsv1.Deployment {
	t.Helper()
	return findDeploymentByName(t, manifests, "managed-serviceaccount-addon-agent")
}

func findDeploymentByName(t *testing.T, manifests []runtime.Object, name string) *appsv1.Deployment {
	t.Helper()
	return findManifest(t, manifests, fmt.Sprintf("deployment %q", name),
		func(d *appsv1.Deployment) bool { return d.Name == name })
}

func findJob(t *testing.T, manifests []runtime.Object, name string) *batchv1.Job {
	t.Helper()
	return findManifest(t, manifests, fmt.Sprintf("job %q", name),
		func(j *batchv1.Job) bool { return j.Name == name })
}

func findSecret(t *testing.T, manifests []runtime.Object, name, location string) *corev1.Secret {
	t.Helper()
	return findManifest(t, manifests, fmt.Sprintf("secret %q with hosted location %q", name, location),
		func(s *corev1.Secret) bool { return s.Name == name && hostedLocation(s) == location })
}

func findService(t *testing.T, manifests []runtime.Object, name, location string) *corev1.Service {
	t.Helper()
	return findManifest(t, manifests, fmt.Sprintf("service %q with hosted location %q", name, location),
		func(s *corev1.Service) bool { return s.Name == name && hostedLocation(s) == location })
}

func assertNoService(t *testing.T, manifests []runtime.Object, name string) {
	t.Helper()
	assertNoManifest(t, manifests, fmt.Sprintf("service %q", name),
		func(s *corev1.Service) bool { return s.Name == name })
}

func findServiceMonitor(t *testing.T, manifests []runtime.Object, name, location string) *unstructured.Unstructured {
	t.Helper()
	return findManifest(t, manifests, fmt.Sprintf("servicemonitor %q with hosted location %q", name, location),
		func(sm *unstructured.Unstructured) bool {
			return sm.GetKind() == "ServiceMonitor" && sm.GetName() == name && hostedLocation(sm) == location
		})
}

func assertNoServiceMonitor(t *testing.T, manifests []runtime.Object, name string) {
	t.Helper()
	assertNoManifest(t, manifests, fmt.Sprintf("servicemonitor %q", name),
		func(sm *unstructured.Unstructured) bool {
			return sm.GetKind() == "ServiceMonitor" && sm.GetName() == name
		})
}

func findServiceAccount(t *testing.T, manifests []runtime.Object, name, location string) *corev1.ServiceAccount {
	t.Helper()
	return findManifest(t, manifests, fmt.Sprintf("serviceaccount %q with hosted location %q", name, location),
		func(sa *corev1.ServiceAccount) bool { return sa.Name == name && hostedLocation(sa) == location })
}

func findRole(t *testing.T, manifests []runtime.Object, name, location string) *rbacv1.Role {
	t.Helper()
	return findManifest(t, manifests, fmt.Sprintf("role %q with hosted location %q", name, location),
		func(r *rbacv1.Role) bool { return r.Name == name && hostedLocation(r) == location })
}

func findRoleBinding(t *testing.T, manifests []runtime.Object, name, location string) *rbacv1.RoleBinding {
	t.Helper()
	return findManifest(t, manifests, fmt.Sprintf("rolebinding %q with hosted location %q", name, location),
		func(rb *rbacv1.RoleBinding) bool { return rb.Name == name && hostedLocation(rb) == location })
}

func findClusterRole(t *testing.T, manifests []runtime.Object, name string) *rbacv1.ClusterRole {
	t.Helper()
	return findManifest(t, manifests, fmt.Sprintf("clusterrole %q", name),
		func(cr *rbacv1.ClusterRole) bool { return cr.Name == name })
}

func findClusterRoleBinding(t *testing.T, manifests []runtime.Object, name string) *rbacv1.ClusterRoleBinding {
	t.Helper()
	return findManifest(t, manifests, fmt.Sprintf("clusterrolebinding %q", name),
		func(crb *rbacv1.ClusterRoleBinding) bool { return crb.Name == name })
}

func hostedLocation(obj metav1.Object) string {
	return obj.GetAnnotations()[addonv1alpha1.HostedManifestLocationAnnotationKey]
}

func assertRule(t *testing.T, rules []rbacv1.PolicyRule, apiGroups, resources, verbs, resourceNames []string) {
	t.Helper()

	for _, rule := range rules {
		if assert.ObjectsAreEqual(apiGroups, rule.APIGroups) &&
			assert.ObjectsAreEqual(resources, rule.Resources) &&
			assert.ObjectsAreEqual(verbs, rule.Verbs) &&
			assert.ObjectsAreEqual(resourceNames, rule.ResourceNames) {
			return
		}
	}

	t.Fatalf("rule not found: apiGroups=%v resources=%v verbs=%v resourceNames=%v in %#v", apiGroups, resources, verbs, resourceNames, rules)
}

func assertAgentMetricsService(t *testing.T, service *corev1.Service) {
	t.Helper()

	assert.Equal(t, "managed-serviceaccount-addon-agent", service.Name)
	assert.Equal(t, map[string]string{"addon-agent": "managed-serviceaccount"}, service.Labels)
	assert.Equal(t, map[string]string{"addon-agent": "managed-serviceaccount"}, service.Spec.Selector)
	if assert.Len(t, service.Spec.Ports, 1) {
		assert.Equal(t, "metrics", service.Spec.Ports[0].Name)
		assert.Equal(t, int32(38080), service.Spec.Ports[0].Port)
		assert.Equal(t, intstr.FromString("metrics"), service.Spec.Ports[0].TargetPort)
		assert.Equal(t, corev1.ProtocolTCP, service.Spec.Ports[0].Protocol)
	}
}

func assertAgentServiceMonitor(t *testing.T, serviceMonitor *unstructured.Unstructured) {
	t.Helper()

	assert.Equal(t, "monitoring.coreos.com/v1", serviceMonitor.GetAPIVersion())
	assert.Equal(t, "ServiceMonitor", serviceMonitor.GetKind())
	endpoints, ok, err := unstructured.NestedSlice(serviceMonitor.Object, "spec", "endpoints")
	assert.NoError(t, err)
	if assert.True(t, ok, "ServiceMonitor endpoints should be set") && assert.Len(t, endpoints, 1) {
		endpoint, ok := endpoints[0].(map[string]interface{})
		if assert.True(t, ok, "endpoint should be an object") {
			assert.Equal(t, "/metrics", endpoint["path"])
			assert.Equal(t, "http", endpoint["scheme"])
			assert.Equal(t, "metrics", endpoint["port"])
		}
	}

	selector, ok, err := unstructured.NestedStringMap(serviceMonitor.Object, "spec", "selector", "matchLabels")
	assert.NoError(t, err)
	if assert.True(t, ok, "ServiceMonitor selector should be set") {
		assert.Equal(t, map[string]string{"addon-agent": "managed-serviceaccount"}, selector)
	}
}

func findNamespaces(manifests []runtime.Object) []*corev1.Namespace {
	namespaces := []*corev1.Namespace{}
	for _, manifest := range manifests {
		namespace, ok := manifest.(*corev1.Namespace)
		if ok {
			namespaces = append(namespaces, namespace)
		}
	}
	return namespaces
}

func assertDeploymentSecretVolume(t *testing.T, deployment *appsv1.Deployment, volumeName, secretName string) {
	t.Helper()

	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Name != volumeName {
			continue
		}
		if assert.NotNil(t, volume.Secret, "volume %q should use a secret", volumeName) {
			assert.Equal(t, secretName, volume.Secret.SecretName)
		}
		return
	}
	t.Fatalf("volume %q not found", volumeName)
}

func assertDeploymentMissingVolume(t *testing.T, deployment *appsv1.Deployment, volumeName string) {
	t.Helper()

	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Name == volumeName {
			t.Fatalf("volume %q should not be rendered", volumeName)
		}
	}
}

func assertDeploymentVolumeMount(t *testing.T, deployment *appsv1.Deployment, volumeName, mountPath string) {
	t.Helper()

	for _, mount := range deployment.Spec.Template.Spec.Containers[0].VolumeMounts {
		if mount.Name == volumeName {
			assert.Equal(t, mountPath, mount.MountPath)
			assert.True(t, mount.ReadOnly)
			return
		}
	}
	t.Fatalf("volume mount %q not found", volumeName)
}

func assertDeploymentContainerPort(t *testing.T, deployment *appsv1.Deployment, portName string, port int32) {
	t.Helper()

	for _, containerPort := range deployment.Spec.Template.Spec.Containers[0].Ports {
		if containerPort.Name == portName {
			assert.Equal(t, port, containerPort.ContainerPort)
			assert.Equal(t, corev1.ProtocolTCP, containerPort.Protocol)
			return
		}
	}
	t.Fatalf("container port %q not found", portName)
}

func newTestImagePullSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "test",
		},
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte("test"),
		},
	}
}

func newTestCluster(name string) *clusterv1.ManagedCluster {
	return &clusterv1.ManagedCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
}

func newTestHostedAddOn(name, namespace, hostingClusterName string) *addonv1alpha1.ManagedClusterAddOn {
	addon := newTestAddOn(name, namespace)
	addon.Annotations = map[string]string{
		addonv1alpha1.HostingClusterNameAnnotationKey: hostingClusterName,
	}
	return addon
}

func newTestAddOn(name, namespace string) *addonv1alpha1.ManagedClusterAddOn {
	return &addonv1alpha1.ManagedClusterAddOn{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: addonv1alpha1.ManagedClusterAddOnSpec{
			InstallNamespace: name,
		},
	}
}
