package manager

import (
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakekube "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"open-cluster-management.io/addon-framework/pkg/addonfactory"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
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
			agentFactory := addonfactory.NewAgentAddonFactory(common.AddonName, FS, "manifests/templates").
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
		true,
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
		true,
		GetDefaultValues(imageName, nil),
	)
	deployment := findDeployment(t, manifests)
	container := deployment.Spec.Template.Spec.Containers[0]

	assert.Equal(t,
		addonv1alpha1.HostedManifestLocationHostingValue,
		deployment.Annotations[addonv1alpha1.HostedManifestLocationAnnotationKey])
	assert.Contains(t, container.Args, "--kubeconfig=/etc/hub/kubeconfig")
	assert.Contains(t, container.Args, "--spoke-kubeconfig=/etc/managed/kubeconfig")
	assertDeploymentSecretVolume(t, deployment, "hub-kubeconfig", "managed-serviceaccount-hub-kubeconfig")
	assertDeploymentSecretVolume(t, deployment, "managed-kubeconfig", addonName+"-managed-kubeconfig")
	assertDeploymentVolumeMount(t, deployment, "hub-kubeconfig", "/etc/hub/")
	assertDeploymentVolumeMount(t, deployment, "managed-kubeconfig", "/etc/managed/")
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
		true,
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
		true,
		GetDefaultValues(imageName, newTestImagePullSecret()),
	)

	assert.Equal(t, "", hostedLocation(findServiceAccount(t, manifests, "managed-serviceaccount", "")), "managed agent serviceaccount should stay on managed cluster")
	assert.Equal(t, addonv1alpha1.HostedManifestLocationHostingValue,
		hostedLocation(findServiceAccount(t, manifests, "managed-serviceaccount", addonv1alpha1.HostedManifestLocationHostingValue)),
		"hosting agent serviceaccount should stay on hosting cluster")
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

	assert.Equal(t, addonv1alpha1.HostedManifestLocationNoneValue,
		hostedLocation(findClusterRole(t, manifests, "open-cluster-management:managed-serviceaccount:addon-agent")))
	assert.Equal(t, addonv1alpha1.HostedManifestLocationNoneValue,
		hostedLocation(findClusterRoleBinding(t, manifests, "open-cluster-management:managed-serviceaccount:addon-agent")))
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
		true,
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

func TestManifestAddonAgentHostedModeCleanupHook(t *testing.T) {
	clusterName := "cluster1"
	addonName := "addon1"
	imageName := "imageName1"
	addon := newTestHostedAddOn(addonName, clusterName, "hosting1")

	manifests := renderTestManifests(
		t,
		newTestCluster(clusterName),
		addon,
		true,
		GetDefaultValues(imageName, nil),
	)

	job := findJob(t, manifests, "managed-serviceaccount-kubeconfig-cleanup")
	assert.Equal(t, addonv1alpha1.HostedManifestLocationHostingValue, hostedLocation(job))
	assert.Contains(t, job.Annotations, addonv1alpha1.AddonPreDeleteHookAnnotationKey)
	args := job.Spec.Template.Spec.Containers[0].Args
	assert.Contains(t, args, "--cleanup")
	assert.Contains(t, args, "--target-secret="+addonName+"-managed-kubeconfig")
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
		true,
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
	hostedModeEnabled bool,
	getValuesFuncs ...addonfactory.GetValuesFunc) []runtime.Object {
	t.Helper()

	agentFactory := addonfactory.NewAgentAddonFactory(common.AddonName, FS, "manifests/templates").
		WithGetValuesFuncs(getValuesFuncs...).
		WithAgentRegistrationOption(NewRegistrationOption(fakekube.NewSimpleClientset()))
	if hostedModeEnabled {
		agentFactory = agentFactory.WithAgentHostedModeEnabledOption()
	}

	addOnAgent, err := agentFactory.BuildTemplateAgentAddon()
	assert.NoError(t, err)

	manifests, err := addOnAgent.Manifests(cluster, addon)
	assert.NoError(t, err)

	return manifests
}

func findDeployment(t *testing.T, manifests []runtime.Object) *appsv1.Deployment {
	t.Helper()

	for _, manifest := range manifests {
		deployment, ok := manifest.(*appsv1.Deployment)
		if ok && deployment.Name == "managed-serviceaccount-addon-agent" {
			return deployment
		}
	}

	t.Fatalf("deployment %q not found", "managed-serviceaccount-addon-agent")
	return nil
}

func findDeploymentByName(t *testing.T, manifests []runtime.Object, name string) *appsv1.Deployment {
	t.Helper()

	for _, manifest := range manifests {
		deployment, ok := manifest.(*appsv1.Deployment)
		if ok && deployment.Name == name {
			return deployment
		}
	}

	t.Fatalf("deployment %q not found", name)
	return nil
}

func findJob(t *testing.T, manifests []runtime.Object, name string) *batchv1.Job {
	t.Helper()

	for _, manifest := range manifests {
		job, ok := manifest.(*batchv1.Job)
		if ok && job.Name == name {
			return job
		}
	}

	t.Fatalf("job %q not found", name)
	return nil
}

func findSecret(t *testing.T, manifests []runtime.Object, name, location string) *corev1.Secret {
	t.Helper()

	for _, manifest := range manifests {
		secret, ok := manifest.(*corev1.Secret)
		if ok && secret.Name == name && hostedLocation(secret) == location {
			return secret
		}
	}

	t.Fatalf("secret %q with hosted location %q not found", name, location)
	return nil
}

func findServiceAccount(t *testing.T, manifests []runtime.Object, name, location string) *corev1.ServiceAccount {
	t.Helper()

	for _, manifest := range manifests {
		serviceAccount, ok := manifest.(*corev1.ServiceAccount)
		if ok && serviceAccount.Name == name && hostedLocation(serviceAccount) == location {
			return serviceAccount
		}
	}

	t.Fatalf("serviceaccount %q with hosted location %q not found", name, location)
	return nil
}

func findRole(t *testing.T, manifests []runtime.Object, name, location string) *rbacv1.Role {
	t.Helper()

	for _, manifest := range manifests {
		role, ok := manifest.(*rbacv1.Role)
		if ok && role.Name == name && hostedLocation(role) == location {
			return role
		}
	}

	t.Fatalf("role %q with hosted location %q not found", name, location)
	return nil
}

func findRoleBinding(t *testing.T, manifests []runtime.Object, name, location string) *rbacv1.RoleBinding {
	t.Helper()

	for _, manifest := range manifests {
		roleBinding, ok := manifest.(*rbacv1.RoleBinding)
		if ok && roleBinding.Name == name && hostedLocation(roleBinding) == location {
			return roleBinding
		}
	}

	t.Fatalf("rolebinding %q with hosted location %q not found", name, location)
	return nil
}

func findClusterRole(t *testing.T, manifests []runtime.Object, name string) *rbacv1.ClusterRole {
	t.Helper()

	for _, manifest := range manifests {
		clusterRole, ok := manifest.(*rbacv1.ClusterRole)
		if ok && clusterRole.Name == name {
			return clusterRole
		}
	}

	t.Fatalf("clusterrole %q not found", name)
	return nil
}

func findClusterRoleBinding(t *testing.T, manifests []runtime.Object, name string) *rbacv1.ClusterRoleBinding {
	t.Helper()

	for _, manifest := range manifests {
		clusterRoleBinding, ok := manifest.(*rbacv1.ClusterRoleBinding)
		if ok && clusterRoleBinding.Name == name {
			return clusterRoleBinding
		}
	}

	t.Fatalf("clusterrolebinding %q not found", name)
	return nil
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
