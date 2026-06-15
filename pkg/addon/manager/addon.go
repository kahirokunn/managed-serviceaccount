package manager

import (
	"context"
	"embed"
	"encoding/base64"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"open-cluster-management.io/addon-framework/pkg/addonfactory"
	"open-cluster-management.io/addon-framework/pkg/agent"
	addonutils "open-cluster-management.io/addon-framework/pkg/utils"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	addonv1beta1 "open-cluster-management.io/api/addon/v1beta1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	"open-cluster-management.io/managed-serviceaccount/pkg/common"
)

// Kube client registration driver values (ManagedClusterAddOn.status.kubeClientDriver).
const (
	TokenDriver = "token"
	CSRDriver   = "csr"
)

//go:embed manifests/templates
var FS embed.FS

func GetDefaultValues(image string, imagePullSecret *corev1.Secret) addonfactory.GetValuesFunc {
	return func(cluster *clusterv1.ManagedCluster, addon *addonv1alpha1.ManagedClusterAddOn) (addonfactory.Values, error) {
		manifestConfig := struct {
			ClusterName         string
			Image               string
			ImagePullSecretData string
		}{
			ClusterName: cluster.Name,
			Image:       image,
		}

		if imagePullSecret != nil {
			manifestConfig.ImagePullSecretData = base64.StdEncoding.EncodeToString(imagePullSecret.Data[corev1.DockerConfigJsonKey])
		}

		return addonfactory.StructToValues(manifestConfig), nil
	}
}

func NewRegistrationOption(nativeClient kubernetes.Interface) *agent.RegistrationOption {
	return &agent.RegistrationOption{
		CSRConfigurations: agent.KubeClientSignerConfigurations(common.AddonName, common.AgentName),
		CSRApproveCheck:   agent.ApprovalAllCSRs,
		PermissionConfig:  setupPermission(nativeClient),
	}
}

func setupPermission(nativeClient kubernetes.Interface) agent.PermissionConfigFunc {
	return func(cluster *clusterv1.ManagedCluster, addon *addonv1alpha1.ManagedClusterAddOn) error {
		namespace := cluster.Name

		var subjects []rbacv1.Subject
		if addon.Status.KubeClientDriver == TokenDriver {
			subjects = rbacSubjectsFromKubeClientRegistration(addon)
			if len(subjects) == 0 {
				return &agent.SubjectNotReadyError{}
			}
		} else {
			subjects = []rbacv1.Subject{
				{
					Kind:     rbacv1.UserKind,
					APIGroup: rbacv1.GroupName,
					Name:     agent.DefaultUser(cluster.Name, common.AddonName, common.AgentName),
				},
			}
		}

		role := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "managed-serviceaccount-addon-agent",
				Namespace: namespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "addon.open-cluster-management.io/v1alpha1",
						Kind:               "ManagedClusterAddOn",
						UID:                addon.UID,
						Name:               addon.Name,
						BlockOwnerDeletion: ptr.To(true),
					},
				},
			},
			Rules: []rbacv1.PolicyRule{
				{
					APIGroups: []string{""},
					Verbs:     []string{"get", "list", "watch", "create", "update"},
					Resources: []string{"secrets"},
				},
				{
					APIGroups: []string{"authentication.open-cluster-management.io"},
					Verbs:     []string{"get", "list", "watch"},
					Resources: []string{"managedserviceaccounts"},
				},
				{
					APIGroups: []string{"authentication.open-cluster-management.io"},
					Verbs:     []string{"get", "update", "patch"},
					Resources: []string{"managedserviceaccounts/status"},
				},
			},
		}
		roleBinding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "managed-serviceaccount-addon-agent",
				Namespace: namespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "addon.open-cluster-management.io/v1alpha1",
						Kind:               "ManagedClusterAddOn",
						UID:                addon.UID,
						Name:               addon.Name,
						BlockOwnerDeletion: ptr.To(true),
					},
				},
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "Role",
				Name:     "managed-serviceaccount-addon-agent",
			},
			Subjects: subjects,
		}

		if _, _, err := addonutils.ApplyRole(context.TODO(), nativeClient.RbacV1(), role); err != nil {
			return err
		}
		if _, _, err := addonutils.ApplyRoleBinding(context.TODO(), nativeClient.RbacV1(), roleBinding); err != nil {
			return err
		}
		return nil
	}
}

// rbacSubjectsFromKubeClientRegistration mirrors addon-framework kubeClient registration subject
// handling while omitting system:authenticated so this addon's hub RBAC stays narrow.
func rbacSubjectsFromKubeClientRegistration(addon *addonv1alpha1.ManagedClusterAddOn) []rbacv1.Subject {
	var subject *addonv1beta1.KubeClientSubject
	for i := range addon.Status.Registrations {
		registration := addonv1beta1.RegistrationConfig{}
		if err := addonv1beta1.Convert_v1alpha1_RegistrationConfig_To_v1beta1_RegistrationConfig(
			&addon.Status.Registrations[i],
			&registration,
			nil,
		); err != nil {
			return nil
		}
		if registration.Type == addonv1beta1.KubeClient && registration.KubeClient != nil {
			subject = &registration.KubeClient.Subject
			break
		}
	}
	if subject == nil || (subject.User == "" && len(subject.Groups) == 0) {
		return nil
	}

	var subjects []rbacv1.Subject
	if subject.User != "" {
		subjects = append(subjects, rbacv1.Subject{
			Kind:     rbacv1.UserKind,
			APIGroup: rbacv1.GroupName,
			Name:     subject.User,
		})
	}
	for _, group := range subject.Groups {
		if group == "system:authenticated" {
			continue
		}
		subjects = append(subjects, rbacv1.Subject{
			Kind:     rbacv1.GroupKind,
			APIGroup: rbacv1.GroupName,
			Name:     group,
		})
	}
	return subjects
}
