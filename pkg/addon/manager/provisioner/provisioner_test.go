package provisioner

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakekube "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestSyncReturnsClearErrorWhenSourceSecretMissing(t *testing.T) {
	hostingClient := fakekube.NewSimpleClientset()
	p := newTestProvisioner(hostingClient, fakekube.NewSimpleClientset(), nil)

	err := p.Sync(context.Background())

	if !assert.Error(t, err) {
		return
	}
	assert.Contains(t, err.Error(), "source managed kubeconfig secret source-ns/external-managed-kubeconfig")
}

func TestSyncCreatesManagedKubeconfigSecretFromServiceAccountToken(t *testing.T) {
	now := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)
	sourceKubeconfig := testKubeconfig(t, "https://managed.example.com", []byte("ca-1"))
	hostingClient := fakekube.NewSimpleClientset(newSourceSecret(sourceKubeconfig))
	managedClient := fakekube.NewSimpleClientset(&corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "managed-serviceaccount",
			Namespace: "addon-ns",
		},
	})
	expires := now.Add(time.Hour)
	stubTokenRequest(t, managedClient, "token-1", expires)
	p := newTestProvisioner(hostingClient, managedClient, func(o *Provisioner) {
		o.Now = func() time.Time { return now }
	})

	err := p.Sync(context.Background())

	mustNoError(t, err)
	secret, err := hostingClient.CoreV1().Secrets("addon-ns").Get(context.Background(), "target-kubeconfig", metav1.GetOptions{})
	mustNoError(t, err)
	assert.Equal(t, corev1.SecretTypeOpaque, secret.Type)
	assert.Equal(t, expires.Format(time.RFC3339), secret.Annotations[TokenExpirationAnnotation])
	assert.Equal(t, sourceKubeconfigHash(sourceKubeconfig), secret.Annotations[SourceKubeconfigHashAnnotation])

	kubeconfig, err := clientcmd.Load(secret.Data[KubeconfigSecretKey])
	mustNoError(t, err)
	assert.Equal(t, "https://managed.example.com", kubeconfig.Clusters["managed"].Server)
	assert.Equal(t, []byte("ca-1"), kubeconfig.Clusters["managed"].CertificateAuthorityData)
	assert.Equal(t, "token-1", kubeconfig.AuthInfos["managed-serviceaccount"].Token)
	assert.Equal(t, "managed", kubeconfig.CurrentContext)
}

func TestSyncSkipsRefreshWhenTokenIsStillValidAndSourceClusterInfoUnchanged(t *testing.T) {
	now := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)
	sourceKubeconfig := testKubeconfig(t, "https://managed.example.com", []byte("ca-1"))
	expires := now.Add(2 * time.Hour)
	hostingClient := fakekube.NewSimpleClientset(
		newSourceSecret(sourceKubeconfig),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "target-kubeconfig",
				Namespace: "addon-ns",
				Annotations: map[string]string{
					TokenExpirationAnnotation:      expires.Format(time.RFC3339),
					SourceKubeconfigHashAnnotation: sourceKubeconfigHash(sourceKubeconfig),
				},
			},
			Data: map[string][]byte{
				KubeconfigSecretKey: []byte("existing"),
			},
		},
	)
	managedClient := fakekube.NewSimpleClientset()
	p := newTestProvisioner(hostingClient, managedClient, func(o *Provisioner) {
		o.Now = func() time.Time { return now }
		o.ManagedClientFactory = func([]byte) (ManagedClient, error) {
			t.Fatalf("managed client factory should not be called when target secret is still fresh")
			return nil, nil
		}
	})

	err := p.Sync(context.Background())

	mustNoError(t, err)
	assertNoAction(t, hostingClient.Actions(), "update", "secrets")
	assertNoAction(t, managedClient.Actions(), "create", "serviceaccounts/token")
}

func TestSyncRefreshesWhenTokenIsExpiring(t *testing.T) {
	now := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)
	sourceKubeconfig := testKubeconfig(t, "https://managed.example.com", []byte("ca-1"))
	hostingClient := fakekube.NewSimpleClientset(
		newSourceSecret(sourceKubeconfig),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "target-kubeconfig",
				Namespace: "addon-ns",
				Annotations: map[string]string{
					TokenExpirationAnnotation:      now.Add(5 * time.Minute).Format(time.RFC3339),
					SourceKubeconfigHashAnnotation: sourceKubeconfigHash(sourceKubeconfig),
				},
			},
			Data: map[string][]byte{
				KubeconfigSecretKey: []byte("existing"),
			},
		},
	)
	managedClient := fakekube.NewSimpleClientset()
	expires := now.Add(time.Hour)
	stubTokenRequest(t, managedClient, "token-2", expires)
	p := newTestProvisioner(hostingClient, managedClient, func(o *Provisioner) {
		o.Now = func() time.Time { return now }
	})

	err := p.Sync(context.Background())

	mustNoError(t, err)
	secret, err := hostingClient.CoreV1().Secrets("addon-ns").Get(context.Background(), "target-kubeconfig", metav1.GetOptions{})
	mustNoError(t, err)
	kubeconfig, err := clientcmd.Load(secret.Data[KubeconfigSecretKey])
	mustNoError(t, err)
	assert.Equal(t, "token-2", kubeconfig.AuthInfos["managed-serviceaccount"].Token)
	assert.Equal(t, expires.Format(time.RFC3339), secret.Annotations[TokenExpirationAnnotation])
	assertAction(t, hostingClient.Actions(), "update", "secrets")
}

func TestSyncRefreshesWhenSourceClusterInfoChanges(t *testing.T) {
	now := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)
	sourceKubeconfig := testKubeconfig(t, "https://managed-new.example.com", []byte("ca-2"))
	hostingClient := fakekube.NewSimpleClientset(
		newSourceSecret(sourceKubeconfig),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "target-kubeconfig",
				Namespace: "addon-ns",
				Annotations: map[string]string{
					TokenExpirationAnnotation:      now.Add(2 * time.Hour).Format(time.RFC3339),
					SourceKubeconfigHashAnnotation: "old-hash",
				},
			},
			Data: map[string][]byte{
				KubeconfigSecretKey: []byte("existing"),
			},
		},
	)
	managedClient := fakekube.NewSimpleClientset()
	expires := now.Add(time.Hour)
	stubTokenRequest(t, managedClient, "token-3", expires)
	p := newTestProvisioner(hostingClient, managedClient, func(o *Provisioner) {
		o.Now = func() time.Time { return now }
	})

	err := p.Sync(context.Background())

	mustNoError(t, err)
	secret, err := hostingClient.CoreV1().Secrets("addon-ns").Get(context.Background(), "target-kubeconfig", metav1.GetOptions{})
	mustNoError(t, err)
	kubeconfig, err := clientcmd.Load(secret.Data[KubeconfigSecretKey])
	mustNoError(t, err)
	assert.Equal(t, "https://managed-new.example.com", kubeconfig.Clusters["managed"].Server)
	assert.Equal(t, []byte("ca-2"), kubeconfig.Clusters["managed"].CertificateAuthorityData)
	assert.Equal(t, "token-3", kubeconfig.AuthInfos["managed-serviceaccount"].Token)
	assert.Equal(t, sourceKubeconfigHash(sourceKubeconfig), secret.Annotations[SourceKubeconfigHashAnnotation])
}

func TestCleanupIgnoresMissingTargetSecret(t *testing.T) {
	hostingClient := fakekube.NewSimpleClientset()
	p := newTestProvisioner(hostingClient, fakekube.NewSimpleClientset(), nil)

	err := p.Cleanup(context.Background())

	mustNoError(t, err)
}

func newTestProvisioner(hostingClient *fakekube.Clientset, managedClient *fakekube.Clientset, mutate func(*Provisioner)) *Provisioner {
	p := &Provisioner{
		HostingClient:                  hostingClient,
		SourceNamespace:                "source-ns",
		SourceSecret:                   "external-managed-kubeconfig",
		TargetNamespace:                "addon-ns",
		TargetSecret:                   "target-kubeconfig",
		ManagedServiceAccountNamespace: "addon-ns",
		ManagedServiceAccountName:      "managed-serviceaccount",
		TokenExpirationSeconds:         3600,
		RefreshBefore:                  30 * time.Minute,
		ManagedClientFactory: func([]byte) (ManagedClient, error) {
			return managedClient, nil
		},
		Now: time.Now,
	}
	if mutate != nil {
		mutate(p)
	}
	return p
}

func newSourceSecret(kubeconfig []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "external-managed-kubeconfig",
			Namespace: "source-ns",
		},
		Data: map[string][]byte{
			KubeconfigSecretKey: kubeconfig,
		},
	}
}

func testKubeconfig(t *testing.T, server string, ca []byte) []byte {
	t.Helper()

	data, err := clientcmd.Write(clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			"managed": {
				Server:                   server,
				CertificateAuthorityData: ca,
			},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"source": {
				Token: "source-token",
			},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"managed": {
				Cluster:  "managed",
				AuthInfo: "source",
			},
		},
		CurrentContext: "managed",
	})
	mustNoError(t, err)
	return data
}

func stubTokenRequest(t *testing.T, client *fakekube.Clientset, token string, expires time.Time) {
	t.Helper()

	client.PrependReactor("create", "serviceaccounts/token", func(action clienttesting.Action) (bool, runtime.Object, error) {
		createAction := action.(clienttesting.CreateAction)
		assert.Equal(t, "addon-ns", action.GetNamespace())
		assert.Equal(t, "managed-serviceaccount", action.(clienttesting.CreateActionImpl).Name)
		request := createAction.GetObject().(*authenticationv1.TokenRequest)
		assert.Equal(t, int64(3600), *request.Spec.ExpirationSeconds)
		return true, &authenticationv1.TokenRequest{
			Status: authenticationv1.TokenRequestStatus{
				Token:               token,
				ExpirationTimestamp: metav1.NewTime(expires),
			},
		}, nil
	})
}

func assertNoAction(t *testing.T, actions []clienttesting.Action, verb, resource string) {
	t.Helper()

	for _, action := range actions {
		if action.Matches(verb, resource) {
			t.Fatalf("unexpected action %s %s: %#v", verb, resource, action)
		}
	}
}

func assertAction(t *testing.T, actions []clienttesting.Action, verb, resource string) {
	t.Helper()

	for _, action := range actions {
		if action.Matches(verb, resource) {
			return
		}
	}
	t.Fatalf("expected action %s %s in %#v", verb, resource, actions)
}

func mustNoError(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
