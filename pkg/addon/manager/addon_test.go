package manager

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakekube "k8s.io/client-go/kubernetes/fake"
	"open-cluster-management.io/addon-framework/pkg/addonfactory"
	"open-cluster-management.io/addon-framework/pkg/agent"
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

	role, err := fakeKubeClient.RbacV1().Roles(clusterName).Get(context.TODO(), permissionName, metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, clusterName, role.Namespace, "invalid role ns")
	assert.Equal(t, permissionName, role.Name, "invalid role name")
	roleBinding := getRoleBinding(t, fakeKubeClient, clusterName)
	assert.Equal(t, clusterName, roleBinding.Namespace, "invalid rolebinding ns")
	assert.Equal(t, permissionName, roleBinding.Name, "invalid rolebinding name")
}

func TestSetupPermission(t *testing.T) {
	clusterName := "cluster1"
	tokenUser := "system:serviceaccount:" + clusterName + ":" + common.AddonName + "-agent"
	addonGroup := "system:open-cluster-management:cluster:" + clusterName + ":addon:" + common.AddonName
	defaultUser := agent.DefaultUser(clusterName, common.AddonName, common.AgentName)

	userSubject := func(name string) rbacv1.Subject {
		return rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: name}
	}
	groupSubject := func(name string) rbacv1.Subject {
		return rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: name}
	}

	cases := []struct {
		name          string
		driver        string
		registrations []addonv1alpha1.RegistrationConfig
		existing      []runtime.Object
		wantSubjects  []rbacv1.Subject
		wantNotReady  bool
	}{
		{
			name:   "token driver binds registration subject and filters system:authenticated",
			driver: "token",
			registrations: []addonv1alpha1.RegistrationConfig{
				newKubeClientRegistration(tokenUser, []string{addonGroup, "system:authenticated"}),
			},
			wantSubjects: []rbacv1.Subject{userSubject(tokenUser), groupSubject(addonGroup)},
		},
		{
			name:         "csr driver binds default user",
			driver:       "csr",
			wantSubjects: []rbacv1.Subject{userSubject(defaultUser)},
		},
		{
			name:         "empty driver binds default user",
			driver:       "",
			wantSubjects: []rbacv1.Subject{userSubject(defaultUser)},
		},
		{
			name:   "token driver updates existing rolebinding subjects",
			driver: "token",
			registrations: []addonv1alpha1.RegistrationConfig{
				newKubeClientRegistration(tokenUser, nil),
			},
			existing: []runtime.Object{&rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: permissionName, Namespace: clusterName},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: permissionName},
				Subjects:   []rbacv1.Subject{userSubject(defaultUser)},
			}},
			wantSubjects: []rbacv1.Subject{userSubject(tokenUser)},
		},
		{
			name:         "token driver without registration subject is not ready",
			driver:       "token",
			wantNotReady: true,
		},
		{
			name:          "token driver with empty registration subject is not ready",
			driver:        "token",
			registrations: []addonv1alpha1.RegistrationConfig{newKubeClientRegistration("", nil)},
			wantNotReady:  true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeKubeClient := fakekube.NewSimpleClientset(c.existing...)
			addon := newTestAddOn(common.AddonName, clusterName)
			addon.Status.KubeClientDriver = c.driver
			addon.Status.Registrations = c.registrations

			err := NewRegistrationOption(fakeKubeClient).PermissionConfig(newTestCluster(clusterName), addon)
			if c.wantNotReady {
				var subjectErr *agent.SubjectNotReadyError
				assert.ErrorAs(t, err, &subjectErr)
				return
			}
			assert.NoError(t, err)
			roleBinding := getRoleBinding(t, fakeKubeClient, clusterName)
			assert.Equal(t, c.wantSubjects, roleBinding.Subjects)
		})
	}
}

func getRoleBinding(t *testing.T, client *fakekube.Clientset, namespace string) *rbacv1.RoleBinding {
	t.Helper()
	roleBinding, err := client.RbacV1().RoleBindings(namespace).Get(context.TODO(), permissionName, metav1.GetOptions{})
	assert.NoError(t, err)
	return roleBinding
}

func newKubeClientRegistration(user string, groups []string) addonv1alpha1.RegistrationConfig {
	return addonv1alpha1.RegistrationConfig{
		SignerName: certificatesv1.KubeAPIServerClientSignerName,
		Subject: addonv1alpha1.Subject{
			User:   user,
			Groups: groups,
		},
	}
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
