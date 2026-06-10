package provisioner

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	fakekube "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clock "k8s.io/utils/clock/testing"
)

func TestSyncReturnsClearErrorWhenSourceSecretMissing(t *testing.T) {
	hostingClient := fakekube.NewSimpleClientset()
	p := newTestProvisioner(hostingClient, fakekube.NewSimpleClientset(), nil)

	err := p.Sync(context.Background())

	assert.ErrorContains(t, err, "failed to get source managed kubeconfig secret")
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
		o.Clock = clock.NewFakeClock(now)
	})

	err := p.Sync(context.Background())

	assert.NoError(t, err)
	secret, err := hostingClient.CoreV1().Secrets("addon-ns").Get(context.Background(), "target-kubeconfig", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, corev1.SecretTypeOpaque, secret.Type)
	assert.Equal(t, expires.Format(time.RFC3339), secret.Annotations[TokenExpirationAnnotation])
	assert.Equal(t, sourceKubeconfigHash(sourceKubeconfig), secret.Annotations[SourceKubeconfigHashAnnotation])
	assert.Equal(t, "addon-ns", secret.Annotations[ManagedServiceAccountNamespaceAnnotation])
	assert.Equal(t, "managed-serviceaccount", secret.Annotations[ManagedServiceAccountNameAnnotation])
	assert.Equal(t, "3600", secret.Annotations[TokenExpirationSecondsAnnotation])

	kubeconfig, err := clientcmd.Load(secret.Data[KubeconfigSecretKey])
	assert.NoError(t, err)
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
					TokenExpirationAnnotation:                expires.Format(time.RFC3339),
					SourceKubeconfigHashAnnotation:           sourceKubeconfigHash(sourceKubeconfig),
					ManagedServiceAccountNamespaceAnnotation: "addon-ns",
					ManagedServiceAccountNameAnnotation:      "managed-serviceaccount",
					TokenExpirationSecondsAnnotation:         "3600",
				},
			},
			Data: map[string][]byte{
				KubeconfigSecretKey: []byte("existing"),
				TokenExpirationKey:  []byte(expires.Format(time.RFC3339)),
			},
		},
	)
	managedClient := fakekube.NewSimpleClientset()
	p := newTestProvisioner(hostingClient, managedClient, func(o *Provisioner) {
		o.Clock = clock.NewFakeClock(now)
		o.ManagedClientFactory = func([]byte) (kubernetes.Interface, error) {
			t.Fatalf("managed client factory should not be called when target secret is still fresh")
			return nil, nil
		}
	})

	err := p.Sync(context.Background())

	assert.NoError(t, err)
	assertNoAction(t, hostingClient.Actions(), "update", "secrets")
	assertNoAction(t, managedClient.Actions(), "create", "serviceaccounts/token")
}

func TestSyncRefreshesWhenManagedServiceAccountIdentityChanges(t *testing.T) {
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
					TokenExpirationAnnotation:                expires.Format(time.RFC3339),
					SourceKubeconfigHashAnnotation:           sourceKubeconfigHash(sourceKubeconfig),
					ManagedServiceAccountNamespaceAnnotation: "addon-ns",
					ManagedServiceAccountNameAnnotation:      "managed-serviceaccount",
					TokenExpirationSecondsAnnotation:         "3600",
				},
			},
			Data: map[string][]byte{
				KubeconfigSecretKey: []byte("existing"),
				TokenExpirationKey:  []byte(expires.Format(time.RFC3339)),
			},
		},
	)
	managedClient := fakekube.NewSimpleClientset()
	newExpires := now.Add(time.Hour)
	stubTokenRequestFor(t, managedClient, "other-ns", "renamed-sa", 3600, "token-rename", newExpires)
	p := newTestProvisioner(hostingClient, managedClient, func(o *Provisioner) {
		o.Clock = clock.NewFakeClock(now)
		o.ManagedServiceAccountNamespace = "other-ns"
		o.ManagedServiceAccountName = "renamed-sa"
	})

	err := p.Sync(context.Background())

	assert.NoError(t, err)
	secret, err := hostingClient.CoreV1().Secrets("addon-ns").Get(context.Background(), "target-kubeconfig", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, "other-ns", secret.Annotations[ManagedServiceAccountNamespaceAnnotation])
	assert.Equal(t, "renamed-sa", secret.Annotations[ManagedServiceAccountNameAnnotation])
	kubeconfig, err := clientcmd.Load(secret.Data[KubeconfigSecretKey])
	assert.NoError(t, err)
	assert.Equal(t, "token-rename", kubeconfig.AuthInfos["renamed-sa"].Token)
	assert.Equal(t, "other-ns", kubeconfig.Contexts["managed"].Namespace)
	assertAction(t, hostingClient.Actions(), "update", "secrets")
}

func TestSyncRefreshesWhenTokenExpirationSecondsChanges(t *testing.T) {
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
					TokenExpirationAnnotation:                expires.Format(time.RFC3339),
					SourceKubeconfigHashAnnotation:           sourceKubeconfigHash(sourceKubeconfig),
					ManagedServiceAccountNamespaceAnnotation: "addon-ns",
					ManagedServiceAccountNameAnnotation:      "managed-serviceaccount",
					TokenExpirationSecondsAnnotation:         "3600",
				},
			},
			Data: map[string][]byte{
				KubeconfigSecretKey: []byte("existing"),
				TokenExpirationKey:  []byte(expires.Format(time.RFC3339)),
			},
		},
	)
	managedClient := fakekube.NewSimpleClientset()
	newExpires := now.Add(4 * time.Hour)
	stubTokenRequestFor(t, managedClient, "addon-ns", "managed-serviceaccount", 14400, "token-longer", newExpires)
	p := newTestProvisioner(hostingClient, managedClient, func(o *Provisioner) {
		o.Clock = clock.NewFakeClock(now)
		o.TokenExpirationSeconds = 14400
	})

	err := p.Sync(context.Background())

	assert.NoError(t, err)
	secret, err := hostingClient.CoreV1().Secrets("addon-ns").Get(context.Background(), "target-kubeconfig", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, "14400", secret.Annotations[TokenExpirationSecondsAnnotation])
	assert.Equal(t, newExpires.Format(time.RFC3339), secret.Annotations[TokenExpirationAnnotation])
	assertAction(t, hostingClient.Actions(), "update", "secrets")
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
		o.Clock = clock.NewFakeClock(now)
	})

	err := p.Sync(context.Background())

	assert.NoError(t, err)
	secret, err := hostingClient.CoreV1().Secrets("addon-ns").Get(context.Background(), "target-kubeconfig", metav1.GetOptions{})
	assert.NoError(t, err)
	kubeconfig, err := clientcmd.Load(secret.Data[KubeconfigSecretKey])
	assert.NoError(t, err)
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
		o.Clock = clock.NewFakeClock(now)
	})

	err := p.Sync(context.Background())

	assert.NoError(t, err)
	secret, err := hostingClient.CoreV1().Secrets("addon-ns").Get(context.Background(), "target-kubeconfig", metav1.GetOptions{})
	assert.NoError(t, err)
	kubeconfig, err := clientcmd.Load(secret.Data[KubeconfigSecretKey])
	assert.NoError(t, err)
	assert.Equal(t, "https://managed-new.example.com", kubeconfig.Clusters["managed"].Server)
	assert.Equal(t, []byte("ca-2"), kubeconfig.Clusters["managed"].CertificateAuthorityData)
	assert.Equal(t, "token-3", kubeconfig.AuthInfos["managed-serviceaccount"].Token)
	assert.Equal(t, sourceKubeconfigHash(sourceKubeconfig), secret.Annotations[SourceKubeconfigHashAnnotation])
}

func TestSyncRefreshesWhenTargetSecretDataIsMissing(t *testing.T) {
	now := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)
	sourceKubeconfig := testKubeconfig(t, "https://managed.example.com", []byte("ca-1"))
	hostingClient := fakekube.NewSimpleClientset(
		newSourceSecret(sourceKubeconfig),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "target-kubeconfig",
				Namespace: "addon-ns",
				Annotations: map[string]string{
					TokenExpirationAnnotation:      now.Add(2 * time.Hour).Format(time.RFC3339),
					SourceKubeconfigHashAnnotation: sourceKubeconfigHash(sourceKubeconfig),
				},
			},
			Data: map[string][]byte{},
		},
	)
	managedClient := fakekube.NewSimpleClientset()
	expires := now.Add(time.Hour)
	stubTokenRequest(t, managedClient, "token-repair", expires)
	p := newTestProvisioner(hostingClient, managedClient, func(o *Provisioner) {
		o.Clock = clock.NewFakeClock(now)
	})

	err := p.Sync(context.Background())

	assert.NoError(t, err)
	secret, err := hostingClient.CoreV1().Secrets("addon-ns").Get(context.Background(), "target-kubeconfig", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotEmpty(t, secret.Data[KubeconfigSecretKey])
	assert.Equal(t, expires.Format(time.RFC3339), string(secret.Data[TokenExpirationKey]))
	kubeconfig, err := clientcmd.Load(secret.Data[KubeconfigSecretKey])
	assert.NoError(t, err)
	assert.Equal(t, "token-repair", kubeconfig.AuthInfos["managed-serviceaccount"].Token)
	assertAction(t, hostingClient.Actions(), "update", "secrets")
}

func TestSyncRejectsNonPortableSourceKubeconfig(t *testing.T) {
	now := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		kubeconfig []byte
	}{
		{
			name:       "certificate-authority file",
			kubeconfig: testKubeconfigWithCAFile(t, "https://managed.example.com", "/etc/ssl/source-ca.crt"),
		},
		{
			name:       "client-certificate file",
			kubeconfig: testKubeconfigWithAuthInfo(t, "https://managed.example.com", []byte("ca-1"), clientcmdapi.AuthInfo{ClientCertificate: "/etc/creds/client.crt", ClientKeyData: []byte("key")}),
		},
		{
			name:       "client-key file",
			kubeconfig: testKubeconfigWithAuthInfo(t, "https://managed.example.com", []byte("ca-1"), clientcmdapi.AuthInfo{ClientCertificateData: []byte("crt"), ClientKey: "/etc/creds/client.key"}),
		},
		{
			name:       "token file",
			kubeconfig: testKubeconfigWithAuthInfo(t, "https://managed.example.com", []byte("ca-1"), clientcmdapi.AuthInfo{TokenFile: "/var/run/secrets/token"}),
		},
		{
			name: "exec credential plugin",
			kubeconfig: testKubeconfigWithAuthInfo(t, "https://managed.example.com", []byte("ca-1"), clientcmdapi.AuthInfo{
				Exec: &clientcmdapi.ExecConfig{Command: "gke-gcloud-auth-plugin", APIVersion: "client.authentication.k8s.io/v1beta1"},
			}),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hostingClient := fakekube.NewSimpleClientset(newSourceSecret(tc.kubeconfig))
			p := newTestProvisioner(hostingClient, fakekube.NewSimpleClientset(), func(o *Provisioner) {
				o.Clock = clock.NewFakeClock(now)
				o.ManagedClientFactory = func([]byte) (kubernetes.Interface, error) {
					t.Fatalf("managed client factory should not be called for a non-portable source kubeconfig")
					return nil, nil
				}
			})

			err := p.Sync(context.Background())

			assert.ErrorContains(t, err, "not portable")
			assertNoAction(t, hostingClient.Actions(), "create", "secrets")
			assertNoAction(t, hostingClient.Actions(), "update", "secrets")
		})
	}
}

func TestCompleteRejectsInvalidConfig(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Provisioner)
		wantErr string
	}{
		{
			name:    "missing hosting client",
			mutate:  func(o *Provisioner) { o.HostingClient = nil },
			wantErr: "hosting client is required",
		},
		{
			name:    "empty target namespace",
			mutate:  func(o *Provisioner) { o.TargetNamespace = "" },
			wantErr: "target namespace is required",
		},
		{
			name:    "empty target secret",
			mutate:  func(o *Provisioner) { o.TargetSecret = "" },
			wantErr: "target secret is required",
		},
		{
			name:    "negative token expiration seconds",
			mutate:  func(o *Provisioner) { o.TokenExpirationSeconds = -1 },
			wantErr: "token expiration seconds must be positive",
		},
		{
			name:    "negative refresh before",
			mutate:  func(o *Provisioner) { o.RefreshBefore = -time.Minute },
			wantErr: "refresh before must be a positive duration",
		},
		{
			name:    "refresh before not shorter than token lifetime",
			mutate:  func(o *Provisioner) { o.TokenExpirationSeconds = 3600; o.RefreshBefore = time.Hour },
			wantErr: "must be less than the token lifetime",
		},
		{
			name: "target secret collides with source",
			mutate: func(o *Provisioner) {
				o.TargetNamespace = "source-ns"
				o.TargetSecret = "external-managed-kubeconfig"
			},
			wantErr: "must differ from the source secret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestProvisioner(fakekube.NewSimpleClientset(), fakekube.NewSimpleClientset(), tc.mutate)
			assert.ErrorContains(t, p.Complete(), tc.wantErr)
		})
	}
}

func TestCleanupIgnoresMissingTargetSecret(t *testing.T) {
	hostingClient := fakekube.NewSimpleClientset()
	p := newTestProvisioner(hostingClient, fakekube.NewSimpleClientset(), nil)

	err := p.Cleanup(context.Background())

	assert.NoError(t, err)
}

func TestCleanupDeletesExistingTargetSecret(t *testing.T) {
	hostingClient := fakekube.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "target-kubeconfig",
			Namespace: "addon-ns",
		},
		Data: map[string][]byte{
			KubeconfigSecretKey: []byte("existing"),
			TokenExpirationKey:  []byte("2026-05-13T01:00:00Z"),
		},
	})
	p := newTestProvisioner(hostingClient, fakekube.NewSimpleClientset(), nil)

	err := p.Cleanup(context.Background())

	assert.NoError(t, err)
	_, err = hostingClient.CoreV1().Secrets("addon-ns").Get(context.Background(), "target-kubeconfig", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "expected target secret to be deleted, got error %v", err)
	assertAction(t, hostingClient.Actions(), "delete", "secrets")
}

func TestCompleteAppliesDefaults(t *testing.T) {
	p := &Provisioner{
		HostingClient:   fakekube.NewSimpleClientset(),
		TargetNamespace: "addon-ns",
		TargetSecret:    "target-kubeconfig",
	}

	assert.NoError(t, p.Complete())

	assert.Equal(t, DefaultExternalManagedKubeConfigSecret, p.SourceSecret)
	assert.Equal(t, "addon-ns", p.ManagedServiceAccountNamespace)
	assert.Equal(t, DefaultManagedServiceAccountName, p.ManagedServiceAccountName)
	assert.Equal(t, DefaultTokenExpirationSeconds, p.TokenExpirationSeconds)
	assert.Equal(t, DefaultRefreshBefore, p.RefreshBefore)
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
		ManagedClientFactory: func([]byte) (kubernetes.Interface, error) {
			return managedClient, nil
		},
	}
	if mutate != nil {
		mutate(p)
	}
	_ = p.Complete()
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
	return testKubeconfigWithAuthInfo(t, server, ca, clientcmdapi.AuthInfo{Token: "source-token"})
}

func testKubeconfigWithCAFile(t *testing.T, server, caFile string) []byte {
	t.Helper()
	return writeTestKubeconfig(t, clientcmdapi.Cluster{
		Server:               server,
		CertificateAuthority: caFile,
	}, clientcmdapi.AuthInfo{Token: "source-token"})
}

func testKubeconfigWithAuthInfo(t *testing.T, server string, ca []byte, authInfo clientcmdapi.AuthInfo) []byte {
	t.Helper()
	return writeTestKubeconfig(t, clientcmdapi.Cluster{
		Server:                   server,
		CertificateAuthorityData: ca,
	}, authInfo)
}

func writeTestKubeconfig(t *testing.T, cluster clientcmdapi.Cluster, authInfo clientcmdapi.AuthInfo) []byte {
	t.Helper()

	data, err := clientcmd.Write(clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{"managed": &cluster},
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{"source": &authInfo},
		Contexts:       map[string]*clientcmdapi.Context{"managed": {Cluster: "managed", AuthInfo: "source"}},
		CurrentContext: "managed",
	})
	assert.NoError(t, err)
	return data
}

func stubTokenRequest(t *testing.T, client *fakekube.Clientset, token string, expires time.Time) {
	t.Helper()
	stubTokenRequestFor(t, client, "addon-ns", "managed-serviceaccount", 3600, token, expires)
}

func stubTokenRequestFor(t *testing.T, client *fakekube.Clientset, namespace, name string, expirationSeconds int64, token string, expires time.Time) {
	t.Helper()

	client.PrependReactor("create", "serviceaccounts/token", func(action clienttesting.Action) (bool, runtime.Object, error) {
		createAction := action.(clienttesting.CreateAction)
		assert.Equal(t, namespace, action.GetNamespace())
		assert.Equal(t, name, action.(clienttesting.CreateActionImpl).Name)
		request := createAction.GetObject().(*authenticationv1.TokenRequest)
		assert.Equal(t, expirationSeconds, *request.Spec.ExpirationSeconds)
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

func sourceKubeconfigHash(kubeconfig []byte) string {
	config, err := clientcmd.Load(kubeconfig)
	if err != nil {
		panic(err)
	}
	context := config.Contexts[config.CurrentContext]
	hash, err := sourceKubeconfigHashFromCluster(config.Clusters[context.Cluster])
	if err != nil {
		panic(err)
	}
	return hash
}
