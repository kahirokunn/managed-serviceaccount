package manager

import (
	"context"
	"embed"
	"encoding/base64"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"open-cluster-management.io/addon-framework/pkg/addonfactory"
	"open-cluster-management.io/addon-framework/pkg/agent"
	addonutils "open-cluster-management.io/addon-framework/pkg/utils"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	"open-cluster-management.io/managed-serviceaccount/pkg/common"
)

const permissionName = "managed-serviceaccount-addon-agent"

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
		if addon.Status.KubeClientDriver == "token" {
			subjects = buildSubjectsFromRegistration(addon, certificatesv1.KubeAPIServerClientSignerName)
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

		owner := metav1.OwnerReference{
			APIVersion:         addonv1alpha1.GroupVersion.String(),
			Kind:               "ManagedClusterAddOn",
			UID:                addon.UID,
			Name:               addon.Name,
			BlockOwnerDeletion: ptr.To(true),
		}
		role := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{
				Name:            permissionName,
				Namespace:       namespace,
				OwnerReferences: []metav1.OwnerReference{owner},
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
				Name:            permissionName,
				Namespace:       namespace,
				OwnerReferences: []metav1.OwnerReference{owner},
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "Role",
				Name:     permissionName,
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

// buildSubjectsFromRegistration extracts RBAC subjects from the addon registration status, returning nil
// when no matching registration or subject is found. Unlike utils.BindKubeClientRole, it drops the
// system:authenticated group so the Role is not bound to every authenticated hub user.
func buildSubjectsFromRegistration(addon *addonv1alpha1.ManagedClusterAddOn, signerName string) []rbacv1.Subject {
	var subject *addonv1alpha1.Subject
	for _, registration := range addon.Status.Registrations {
		if registration.SignerName == signerName {
			subject = &registration.Subject
			break
		}
	}

	if subject == nil || equality.Semantic.DeepEqual(*subject, addonv1alpha1.Subject{}) {
		return nil
	}

	subjects := []rbacv1.Subject{}
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
