package provisioner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/clock"
)

const (
	KubeconfigSecretKey = "kubeconfig"
	TokenExpirationKey  = "expirationTimestamp"

	TokenExpirationAnnotation                = "authentication.open-cluster-management.io/token-expiration"
	SourceKubeconfigHashAnnotation           = "authentication.open-cluster-management.io/source-kubeconfig-hash"
	ManagedServiceAccountNamespaceAnnotation = "authentication.open-cluster-management.io/managed-serviceaccount-namespace"
	ManagedServiceAccountNameAnnotation      = "authentication.open-cluster-management.io/managed-serviceaccount-name"
	TokenExpirationSecondsAnnotation         = "authentication.open-cluster-management.io/token-expiration-seconds"

	DefaultExternalManagedKubeConfigSecret = "external-managed-kubeconfig"
	DefaultManagedServiceAccountName       = "managed-serviceaccount"
	DefaultTokenExpirationSeconds          = int64(3600)
	DefaultRefreshBefore                   = 10 * time.Minute
	DefaultSyncInterval                    = 5 * time.Minute
)

type ManagedClientFactory func(sourceKubeconfig []byte) (kubernetes.Interface, error)

type Provisioner struct {
	HostingClient kubernetes.Interface

	SourceNamespace string
	SourceSecret    string
	TargetNamespace string
	TargetSecret    string

	ManagedServiceAccountNamespace string
	ManagedServiceAccountName      string
	TokenExpirationSeconds         int64
	RefreshBefore                  time.Duration

	ManagedClientFactory ManagedClientFactory
	Clock                clock.Clock
}

func NewManagedClient(sourceKubeconfig []byte) (kubernetes.Interface, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(sourceKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to build managed client config from source kubeconfig: %w", err)
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create managed client from source kubeconfig: %w", err)
	}
	return client, nil
}

func (p *Provisioner) Sync(ctx context.Context) error {
	source, err := p.HostingClient.CoreV1().Secrets(p.SourceNamespace).Get(ctx, p.SourceSecret, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get source managed kubeconfig secret: %w", err)
	}

	sourceKubeconfig, ok := source.Data[KubeconfigSecretKey]
	if !ok || len(sourceKubeconfig) == 0 {
		return fmt.Errorf("source managed kubeconfig secret missing %q data", KubeconfigSecretKey)
	}

	sourceConfig, err := clientcmd.Load(sourceKubeconfig)
	if err != nil {
		return fmt.Errorf("failed to load source managed kubeconfig: %w", err)
	}

	cluster, err := portableSourceCluster(sourceConfig)
	if err != nil {
		return err
	}

	sourceHash, err := sourceKubeconfigHashFromCluster(cluster)
	if err != nil {
		return err
	}
	identity := p.identityAnnotations(sourceHash)

	existing, err := p.HostingClient.CoreV1().Secrets(p.TargetNamespace).Get(ctx, p.TargetSecret, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		existing = nil
	case err != nil:
		return fmt.Errorf("failed to get target managed kubeconfig secret: %w", err)
	case p.targetSecretFresh(existing, identity):
		return nil
	}

	managedClient, err := p.ManagedClientFactory(sourceKubeconfig)
	if err != nil {
		return err
	}

	tokenRequest, err := managedClient.CoreV1().ServiceAccounts(p.ManagedServiceAccountNamespace).CreateToken(
		ctx,
		p.ManagedServiceAccountName,
		&authenticationv1.TokenRequest{
			Spec: authenticationv1.TokenRequestSpec{
				ExpirationSeconds: &p.TokenExpirationSeconds,
			},
		},
		metav1.CreateOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to request token for managed serviceaccount: %w", err)
	}
	if len(tokenRequest.Status.Token) == 0 {
		return fmt.Errorf("token request for managed serviceaccount returned an empty token")
	}

	kubeconfig, err := buildManagedKubeconfig(cluster, p.ManagedServiceAccountNamespace, p.ManagedServiceAccountName, tokenRequest.Status.Token)
	if err != nil {
		return err
	}

	expiration := tokenRequest.Status.ExpirationTimestamp.Time.UTC().Format(time.RFC3339)
	identity[TokenExpirationAnnotation] = expiration
	desiredData := map[string][]byte{
		KubeconfigSecretKey: kubeconfig,
		TokenExpirationKey:  []byte(expiration),
	}

	secret := p.buildTargetSecret(existing, identity, desiredData)
	if existing == nil {
		if _, err := p.HostingClient.CoreV1().Secrets(p.TargetNamespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("failed to create target managed kubeconfig secret: %w", err)
		}
		return nil
	}
	if _, err := p.HostingClient.CoreV1().Secrets(p.TargetNamespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("failed to update target managed kubeconfig secret: %w", err)
	}
	return nil
}

func (p *Provisioner) buildTargetSecret(existing *corev1.Secret, annotations map[string]string, data map[string][]byte) *corev1.Secret {
	var secret *corev1.Secret
	if existing != nil {
		secret = existing.DeepCopy()
	} else {
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      p.TargetSecret,
				Namespace: p.TargetNamespace,
			},
		}
	}

	secret.Type = corev1.SecretTypeOpaque
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	for k, v := range annotations {
		secret.Annotations[k] = v
	}
	secret.Data = data
	return secret
}

func (p *Provisioner) Cleanup(ctx context.Context) error {
	if err := p.HostingClient.CoreV1().Secrets(p.TargetNamespace).Delete(ctx, p.TargetSecret, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete target managed kubeconfig secret: %w", err)
	}
	return nil
}

// Complete defaults and validates the provisioner fields. It must be called
// once before Sync or Cleanup.
func (p *Provisioner) Complete() error {
	if p.HostingClient == nil {
		return fmt.Errorf("hosting client is required")
	}
	if len(p.SourceSecret) == 0 {
		p.SourceSecret = DefaultExternalManagedKubeConfigSecret
	}
	if len(p.TargetNamespace) == 0 {
		return fmt.Errorf("target namespace is required")
	}
	if len(p.TargetSecret) == 0 {
		return fmt.Errorf("target secret is required")
	}
	if p.SourceNamespace == p.TargetNamespace && p.SourceSecret == p.TargetSecret {
		return fmt.Errorf("target managed kubeconfig secret %s/%s must differ from the source secret %s/%s",
			p.TargetNamespace, p.TargetSecret, p.SourceNamespace, p.SourceSecret)
	}
	if len(p.ManagedServiceAccountNamespace) == 0 {
		p.ManagedServiceAccountNamespace = p.TargetNamespace
	}
	if len(p.ManagedServiceAccountName) == 0 {
		p.ManagedServiceAccountName = DefaultManagedServiceAccountName
	}
	if p.TokenExpirationSeconds == 0 {
		p.TokenExpirationSeconds = DefaultTokenExpirationSeconds
	}
	if p.RefreshBefore == 0 {
		p.RefreshBefore = DefaultRefreshBefore
	}
	if p.TokenExpirationSeconds < 0 {
		return fmt.Errorf("token expiration seconds must be positive, got %d", p.TokenExpirationSeconds)
	}
	if p.RefreshBefore < 0 {
		return fmt.Errorf("refresh before must be a positive duration, got %s", p.RefreshBefore)
	}
	if tokenLifetime := time.Duration(p.TokenExpirationSeconds) * time.Second; p.RefreshBefore >= tokenLifetime {
		return fmt.Errorf("refresh before (%s) must be less than the token lifetime (%s)", p.RefreshBefore, tokenLifetime)
	}
	if p.ManagedClientFactory == nil {
		p.ManagedClientFactory = NewManagedClient
	}
	if p.Clock == nil {
		p.Clock = clock.RealClock{}
	}
	return nil
}

// identityAnnotations records the inputs the generated kubeconfig was minted
// for, so targetSecretFresh re-mints the token whenever any of them changes.
func (p *Provisioner) identityAnnotations(sourceHash string) map[string]string {
	return map[string]string{
		SourceKubeconfigHashAnnotation:           sourceHash,
		ManagedServiceAccountNamespaceAnnotation: p.ManagedServiceAccountNamespace,
		ManagedServiceAccountNameAnnotation:      p.ManagedServiceAccountName,
		TokenExpirationSecondsAnnotation:         strconv.FormatInt(p.TokenExpirationSeconds, 10),
	}
}

func (p *Provisioner) targetSecretFresh(secret *corev1.Secret, identity map[string]string) bool {
	if secret == nil {
		return false
	}
	for key, want := range identity {
		if secret.Annotations[key] != want {
			return false
		}
	}
	if len(secret.Data[KubeconfigSecretKey]) == 0 || len(secret.Data[TokenExpirationKey]) == 0 {
		return false
	}

	expiration, err := time.Parse(time.RFC3339, secret.Annotations[TokenExpirationAnnotation])
	if err != nil {
		return false
	}
	return expiration.After(p.Clock.Now().UTC().Add(p.RefreshBefore))
}

func buildManagedKubeconfig(cluster *clientcmdapi.Cluster, namespace, serviceAccountName, token string) ([]byte, error) {
	config := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			"managed": cluster.DeepCopy(),
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			serviceAccountName: {
				Token: token,
			},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"managed": {
				Cluster:   "managed",
				AuthInfo:  serviceAccountName,
				Namespace: namespace,
			},
		},
		CurrentContext: "managed",
	}

	kubeconfig, err := clientcmd.Write(config)
	if err != nil {
		return nil, fmt.Errorf("failed to write managed serviceaccount kubeconfig: %w", err)
	}
	return kubeconfig, nil
}

// portableSourceCluster returns the current context's cluster, rejecting file
// or exec references that won't resolve once consumed from the provisioner pod.
func portableSourceCluster(config *clientcmdapi.Config) (*clientcmdapi.Cluster, error) {
	if config == nil {
		return nil, fmt.Errorf("source managed kubeconfig is empty")
	}
	contextName := config.CurrentContext
	if len(contextName) == 0 && len(config.Contexts) == 1 {
		for name := range config.Contexts {
			contextName = name
		}
	}
	context := config.Contexts[contextName]
	if context == nil {
		return nil, fmt.Errorf("source managed kubeconfig current context %q not found", contextName)
	}
	cluster := config.Clusters[context.Cluster]
	if cluster == nil {
		return nil, fmt.Errorf("source managed kubeconfig cluster %q not found", context.Cluster)
	}
	authInfo := config.AuthInfos[context.AuthInfo]
	if authInfo == nil {
		return nil, fmt.Errorf("source managed kubeconfig user %q not found", context.AuthInfo)
	}

	if len(cluster.CertificateAuthority) > 0 {
		return nil, fmt.Errorf("source managed kubeconfig cluster references certificate-authority file %q, which is not portable",
			cluster.CertificateAuthority)
	}
	if authInfo.Exec != nil {
		return nil, fmt.Errorf("source managed kubeconfig user references exec credential plugin %q, which is not portable",
			authInfo.Exec.Command)
	}
	for _, ref := range []struct{ path, field string }{
		{authInfo.ClientCertificate, "client-certificate"},
		{authInfo.ClientKey, "client-key"},
		{authInfo.TokenFile, "token"},
	} {
		if len(ref.path) > 0 {
			return nil, fmt.Errorf("source managed kubeconfig user references %s file %q, which is not portable",
				ref.field, ref.path)
		}
	}
	return cluster, nil
}

// Hash the canonical cluster form so formatting-only changes don't churn the token.
func sourceKubeconfigHashFromCluster(cluster *clientcmdapi.Cluster) (string, error) {
	sanitized := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			"managed": cluster.DeepCopy(),
		},
		Contexts: map[string]*clientcmdapi.Context{
			"managed": {
				Cluster: "managed",
			},
		},
		CurrentContext: "managed",
	}
	data, err := clientcmd.Write(sanitized)
	if err != nil {
		return "", fmt.Errorf("failed to write source managed kubeconfig hash input: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
