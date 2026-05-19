package provisioner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	KubeconfigSecretKey = "kubeconfig"
	TokenExpirationKey  = "expirationTimestamp"

	TokenExpirationAnnotation      = "authentication.open-cluster-management.io/token-expiration"
	SourceKubeconfigHashAnnotation = "authentication.open-cluster-management.io/source-kubeconfig-hash"

	DefaultExternalManagedKubeConfigSecret = "external-managed-kubeconfig"
	DefaultManagedServiceAccountName       = "managed-serviceaccount"
	DefaultTokenExpirationSeconds          = int64(3600)
	DefaultRefreshBefore                   = 10 * time.Minute
)

type ManagedClient interface {
	CoreV1() corev1client.CoreV1Interface
}

type ManagedClientFactory func(sourceKubeconfig []byte) (ManagedClient, error)

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
	Now                  func() time.Time
}

func NewManagedClient(sourceKubeconfig []byte) (ManagedClient, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(sourceKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("build managed client config from source kubeconfig: %w", err)
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create managed client from source kubeconfig: %w", err)
	}
	return client, nil
}

func (p *Provisioner) Sync(ctx context.Context) error {
	if err := p.complete(); err != nil {
		return err
	}

	source, err := p.HostingClient.CoreV1().Secrets(p.SourceNamespace).Get(ctx, p.SourceSecret, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("source managed kubeconfig secret %s/%s not found", p.SourceNamespace, p.SourceSecret)
		}
		return fmt.Errorf("get source managed kubeconfig secret %s/%s: %w", p.SourceNamespace, p.SourceSecret, err)
	}

	sourceKubeconfig, ok := source.Data[KubeconfigSecretKey]
	if !ok || len(sourceKubeconfig) == 0 {
		return fmt.Errorf("source managed kubeconfig secret %s/%s missing %q data", p.SourceNamespace, p.SourceSecret, KubeconfigSecretKey)
	}

	sourceConfig, err := clientcmd.Load(sourceKubeconfig)
	if err != nil {
		return fmt.Errorf("load source managed kubeconfig from secret %s/%s: %w", p.SourceNamespace, p.SourceSecret, err)
	}
	sourceHash, err := sourceKubeconfigHashFromConfig(sourceConfig)
	if err != nil {
		return err
	}

	existing, err := p.HostingClient.CoreV1().Secrets(p.TargetNamespace).Get(ctx, p.TargetSecret, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		existing = nil
	case err != nil:
		return fmt.Errorf("get target managed kubeconfig secret %s/%s: %w", p.TargetNamespace, p.TargetSecret, err)
	case p.targetSecretFresh(existing, sourceHash):
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
		return fmt.Errorf("request token for managed serviceaccount %s/%s: %w",
			p.ManagedServiceAccountNamespace, p.ManagedServiceAccountName, err)
	}
	if len(tokenRequest.Status.Token) == 0 {
		return fmt.Errorf("token request for managed serviceaccount %s/%s returned an empty token",
			p.ManagedServiceAccountNamespace, p.ManagedServiceAccountName)
	}

	kubeconfig, err := buildManagedKubeconfig(sourceConfig, p.ManagedServiceAccountNamespace, p.ManagedServiceAccountName, tokenRequest.Status.Token)
	if err != nil {
		return err
	}

	expiration := tokenRequest.Status.ExpirationTimestamp.Time.UTC().Format(time.RFC3339)
	desiredAnnotations := map[string]string{
		TokenExpirationAnnotation:      expiration,
		SourceKubeconfigHashAnnotation: sourceHash,
	}
	desiredData := map[string][]byte{
		KubeconfigSecretKey: kubeconfig,
		TokenExpirationKey:  []byte(expiration),
	}

	if existing == nil {
		required := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        p.TargetSecret,
				Namespace:   p.TargetNamespace,
				Annotations: desiredAnnotations,
			},
			Type: corev1.SecretTypeOpaque,
			Data: desiredData,
		}
		if _, err := p.HostingClient.CoreV1().Secrets(p.TargetNamespace).Create(ctx, required, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create target managed kubeconfig secret %s/%s: %w", p.TargetNamespace, p.TargetSecret, err)
		}
		return nil
	}

	updated := existing.DeepCopy()
	updated.Type = corev1.SecretTypeOpaque
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	for k, v := range desiredAnnotations {
		updated.Annotations[k] = v
	}
	updated.Data = desiredData
	if _, err := p.HostingClient.CoreV1().Secrets(p.TargetNamespace).Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update target managed kubeconfig secret %s/%s: %w", p.TargetNamespace, p.TargetSecret, err)
	}
	return nil
}

func (p *Provisioner) Cleanup(ctx context.Context) error {
	if err := p.complete(); err != nil {
		return err
	}

	err := p.HostingClient.CoreV1().Secrets(p.TargetNamespace).Delete(ctx, p.TargetSecret, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete target managed kubeconfig secret %s/%s: %w", p.TargetNamespace, p.TargetSecret, err)
	}
	return nil
}

func (p *Provisioner) complete() error {
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
	if p.ManagedClientFactory == nil {
		p.ManagedClientFactory = NewManagedClient
	}
	if p.Now == nil {
		p.Now = time.Now
	}
	return nil
}

func (p *Provisioner) targetSecretFresh(secret *corev1.Secret, sourceHash string) bool {
	if secret == nil || secret.Annotations == nil {
		return false
	}
	if secret.Annotations[SourceKubeconfigHashAnnotation] != sourceHash {
		return false
	}

	expiration, err := time.Parse(time.RFC3339, secret.Annotations[TokenExpirationAnnotation])
	if err != nil {
		return false
	}
	return expiration.After(p.Now().UTC().Add(p.RefreshBefore))
}

func buildManagedKubeconfig(sourceConfig *clientcmdapi.Config, namespace, serviceAccountName, token string) ([]byte, error) {
	cluster, err := currentCluster(sourceConfig)
	if err != nil {
		return nil, err
	}

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
		return nil, fmt.Errorf("write managed serviceaccount kubeconfig: %w", err)
	}
	return kubeconfig, nil
}

func currentCluster(config *clientcmdapi.Config) (*clientcmdapi.Cluster, error) {
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
	return cluster, nil
}

func sourceKubeconfigHashFromConfig(config *clientcmdapi.Config) (string, error) {
	cluster, err := currentCluster(config)
	if err != nil {
		return "", err
	}

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
		return "", fmt.Errorf("write source managed kubeconfig hash input: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
