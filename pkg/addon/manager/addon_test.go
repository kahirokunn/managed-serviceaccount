package manager

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakekube "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"open-cluster-management.io/addon-framework/pkg/addonfactory"
	"open-cluster-management.io/addon-framework/pkg/agent"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	addonv1beta1 "open-cluster-management.io/api/addon/v1beta1"
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
	assert.Len(t, actions, 4)
	role := createdRole(t, actions)
	assert.Equal(t, clusterName, role.Namespace, "invalid role ns")
	assert.Equal(t, "managed-serviceaccount-addon-agent", role.Name, "invalid role name")
	rolebinding := createdRoleBinding(t, actions)
	assert.Equal(t, clusterName, rolebinding.Namespace, "invalid rolebinding ns")
	assert.Equal(t, "managed-serviceaccount-addon-agent", rolebinding.Name, "invalid rolebinding name")
}

// TestSetupPermission_tokenDriver_bindsRegistrationSubject documents GitHub issue #279: with klusterlet token
// registration, the hub RoleBinding must use the subject published in ManagedClusterAddOn status (the
// system:serviceaccount:... identity), not the CSR-style OCM user.
func TestSetupPermission_tokenDriver_bindsRegistrationSubject(t *testing.T) {
	clusterName := "cluster1"
	addonName := common.AddonName
	expectedSubjectUser := "system:serviceaccount:" + clusterName + ":" + addonName + "-agent"

	fakeKubeClient := fakekube.NewSimpleClientset()
	permissionConfig := NewRegistrationOption(fakeKubeClient).PermissionConfig

	addon := newTestAddOn(addonName, clusterName)
	addon.Status.KubeClientDriver = TokenDriver
	addon.Status.Registrations = []addonv1alpha1.RegistrationConfig{
		newKubeClientRegistration(t, expectedSubjectUser, nil),
	}

	err := permissionConfig(newTestCluster(clusterName), addon)
	assert.NoError(t, err)

	roleBinding := createdRoleBinding(t, fakeKubeClient.Actions())
	assert.Len(t, roleBinding.Subjects, 1, "expected exactly one subject on RoleBinding")
	assert.Equal(t, rbacv1.UserKind, roleBinding.Subjects[0].Kind)
	assert.Equal(t, expectedSubjectUser, roleBinding.Subjects[0].Name,
		"token registration must bind the hub kube client subject user, not the CSR registration user")
}

func TestSetupPermission_tokenDriver_filtersSystemAuthenticatedGroup(t *testing.T) {
	clusterName := "cluster1"
	fakeKubeClient := fakekube.NewSimpleClientset()
	addon := newTestAddOn(common.AddonName, clusterName)
	addon.Status.KubeClientDriver = TokenDriver
	addon.Status.Registrations = []addonv1alpha1.RegistrationConfig{
		newKubeClientRegistration(t,
			"system:serviceaccount:"+clusterName+":"+common.AddonName+"-agent",
			[]string{
				"system:open-cluster-management:cluster:" + clusterName + ":addon:" + common.AddonName,
				"system:authenticated",
			},
		),
	}

	err := NewRegistrationOption(fakeKubeClient).PermissionConfig(newTestCluster(clusterName), addon)
	assert.NoError(t, err)

	roleBinding := createdRoleBinding(t, fakeKubeClient.Actions())
	assert.Equal(t, []rbacv1.Subject{
		{
			Kind:     rbacv1.UserKind,
			APIGroup: rbacv1.GroupName,
			Name:     "system:serviceaccount:" + clusterName + ":" + common.AddonName + "-agent",
		},
		{
			Kind:     rbacv1.GroupKind,
			APIGroup: rbacv1.GroupName,
			Name:     "system:open-cluster-management:cluster:" + clusterName + ":addon:" + common.AddonName,
		},
	}, roleBinding.Subjects)
}

func TestSetupPermission_csrOrEmptyDriver_bindsDefaultUser(t *testing.T) {
	clusterName := "cluster1"

	for _, driver := range []string{"", CSRDriver} {
		t.Run("driver="+driver, func(t *testing.T) {
			fakeKubeClient := fakekube.NewSimpleClientset()
			addon := newTestAddOn(common.AddonName, clusterName)
			addon.Status.KubeClientDriver = driver

			err := NewRegistrationOption(fakeKubeClient).PermissionConfig(newTestCluster(clusterName), addon)
			assert.NoError(t, err)

			roleBinding := createdRoleBinding(t, fakeKubeClient.Actions())
			assert.Equal(t, []rbacv1.Subject{
				{
					Kind:     rbacv1.UserKind,
					APIGroup: rbacv1.GroupName,
					Name:     agent.DefaultUser(clusterName, common.AddonName, common.AgentName),
				},
			}, roleBinding.Subjects)
		})
	}
}

func TestSetupPermission_updatesExistingRoleBindingSubjects(t *testing.T) {
	clusterName := "cluster1"
	bindingName := "managed-serviceaccount-addon-agent"
	tokenUser := "system:serviceaccount:" + clusterName + ":" + common.AddonName + "-agent"

	existingRoleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bindingName,
			Namespace: clusterName,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     bindingName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:     rbacv1.UserKind,
				APIGroup: rbacv1.GroupName,
				Name:     agent.DefaultUser(clusterName, common.AddonName, common.AgentName),
			},
		},
	}
	fakeKubeClient := fakekube.NewSimpleClientset(existingRoleBinding)
	addon := newTestAddOn(common.AddonName, clusterName)
	addon.Status.KubeClientDriver = TokenDriver
	addon.Status.Registrations = []addonv1alpha1.RegistrationConfig{
		newKubeClientRegistration(t, tokenUser, nil),
	}

	err := NewRegistrationOption(fakeKubeClient).PermissionConfig(newTestCluster(clusterName), addon)
	assert.NoError(t, err)

	roleBinding, err := fakeKubeClient.RbacV1().RoleBindings(clusterName).Get(
		context.TODO(),
		bindingName,
		metav1.GetOptions{},
	)
	assert.NoError(t, err)
	assert.Equal(t, []rbacv1.Subject{
		{
			Kind:     rbacv1.UserKind,
			APIGroup: rbacv1.GroupName,
			Name:     tokenUser,
		},
	}, roleBinding.Subjects)

	foundUpdate := false
	for _, a := range fakeKubeClient.Actions() {
		if a.GetVerb() == "update" && a.GetResource().Resource == "rolebindings" {
			foundUpdate = true
			break
		}
	}
	assert.True(t, foundUpdate, "expected existing RoleBinding subjects to be updated")
}

// TestSetupPermission_tokenDriver_subjectNotReady verifies we wait until the spoke has published
// registration subject before applying hub RBAC.
func TestSetupPermission_tokenDriver_subjectNotReady(t *testing.T) {
	clusterName := "cluster1"
	addonName := common.AddonName

	t.Run("no kube client registration entry", func(t *testing.T) {
		fakeKubeClient := fakekube.NewSimpleClientset()
		permissionConfig := NewRegistrationOption(fakeKubeClient).PermissionConfig
		addon := newTestAddOn(addonName, clusterName)
		addon.Status.KubeClientDriver = TokenDriver

		err := permissionConfig(newTestCluster(clusterName), addon)
		var subjectErr *agent.SubjectNotReadyError
		assert.True(t, errors.As(err, &subjectErr), "expected SubjectNotReadyError while token subject is unset, got %v", err)
		assertNoCreateActions(t, fakeKubeClient.Actions())
	})

	t.Run("kube client registration with empty subject", func(t *testing.T) {
		fakeKubeClient := fakekube.NewSimpleClientset()
		permissionConfig := NewRegistrationOption(fakeKubeClient).PermissionConfig
		addon := newTestAddOn(addonName, clusterName)
		addon.Status.KubeClientDriver = TokenDriver
		addon.Status.Registrations = []addonv1alpha1.RegistrationConfig{
			newKubeClientRegistration(t, "", nil),
		}

		err := permissionConfig(newTestCluster(clusterName), addon)
		var subjectErr *agent.SubjectNotReadyError
		assert.True(t, errors.As(err, &subjectErr), "expected SubjectNotReadyError for empty registration subject, got %v", err)
		assertNoCreateActions(t, fakeKubeClient.Actions())
	})
}

func assertNoCreateActions(t *testing.T, actions []clienttesting.Action) {
	t.Helper()
	for _, a := range actions {
		if _, ok := a.(clienttesting.CreateAction); ok {
			t.Fatalf("expected no CreateAction writes, saw %#v", a)
		}
	}
}

func createdRole(t *testing.T, actions []clienttesting.Action) *rbacv1.Role {
	t.Helper()
	for _, a := range actions {
		if create, ok := a.(clienttesting.CreateAction); ok {
			if role, ok := create.GetObject().(*rbacv1.Role); ok {
				return role
			}
		}
	}
	t.Fatalf("expected a Role create action")
	return nil
}

func createdRoleBinding(t *testing.T, actions []clienttesting.Action) *rbacv1.RoleBinding {
	t.Helper()
	for _, a := range actions {
		if create, ok := a.(clienttesting.CreateAction); ok {
			if roleBinding, ok := create.GetObject().(*rbacv1.RoleBinding); ok {
				return roleBinding
			}
		}
	}
	t.Fatalf("expected a RoleBinding create action")
	return nil
}

func newKubeClientRegistration(t *testing.T, user string, groups []string) addonv1alpha1.RegistrationConfig {
	t.Helper()

	registration := addonv1alpha1.RegistrationConfig{}
	err := addonv1beta1.Convert_v1beta1_RegistrationConfig_To_v1alpha1_RegistrationConfig(
		&addonv1beta1.RegistrationConfig{
			Type: addonv1beta1.KubeClient,
			KubeClient: &addonv1beta1.KubeClientConfig{
				Subject: addonv1beta1.KubeClientSubject{
					BaseSubject: addonv1beta1.BaseSubject{
						User:   user,
						Groups: groups,
					},
				},
			},
		},
		&registration,
		nil,
	)
	assert.NoError(t, err)
	return registration
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
