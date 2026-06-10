# Managed Service Account

## Overview

The Managed Service Account addon is an Open Cluster Management (OCM) addon built on the [addon-framework](https://github.com/open-cluster-management-io/addon-framework). It synchronizes [ServiceAccount](https://kubernetes.io/docs/tasks/configure-pod-container/configure-service-account/) resources to managed clusters and collects their authentication tokens back to the hub cluster as secret resources.

## Use Cases

This addon is beneficial when you need to:

- Deploy service account resources to managed clusters without requiring a kubeconfig to each managed cluster
- Access the Kubernetes API of managed clusters from the hub cluster using valid authentication tokens  
- Standardize client identity by using the same service account across managed cluster API requests

## Architecture

The addon follows the standard OCM [addon architecture](https://open-cluster-management.io/concepts/addon/) with two main components:

For the full component model, reconciliation flows, hosted-mode runtime, and
deployment invariants, see [Managed ServiceAccount Architecture](docs/ARCHITECTURE.md).

**Addon Manager**
- Automatically installs the addon agent into managed clusters
- Manages required resources and dependencies

**Addon Agent**  
- Monitors the ManagedServiceAccount API resources
- Periodically projects service account tokens as secret resources to the hub cluster
- Handles token refresh according to the configured rotation policy

### Installation Methods

The managed service account addon supports 2 installation ways:

- **default (manager - agent)**: Full deployment with both addon manager and addon agent components
- **addontemplate (only agent)**: Lightweight deployment with only the addon agent component

Hosted mode (see [Hosted Mode](#hosted-mode)) is only rendered by the addon manager, so it requires the default `hubDeployMode: Deployment`. The `AddOnTemplate` mode disables the addon manager and will not deploy any hosted-mode workloads.

## Installation

### Prerequisites

- Open Cluster Management (OCM) registration (version >= 0.5.0)

### Installation Steps

Install the addon using Helm charts:

```shell
# Add the OCM Helm repository
helm repo add ocm https://open-cluster-management.io/helm-charts/
helm repo update

# Search for the managed-serviceaccount chart
helm search repo ocm/managed-serviceaccount

# Install the addon
helm install \
    -n open-cluster-management-addon --create-namespace \
    managed-serviceaccount ocm/managed-serviceaccount
```

### Verification

Confirm the installation was successful:

```shell
kubectl get managedclusteraddon -A | grep managed-serviceaccount
```

Expected output:
```
NAMESPACE        NAME                     AVAILABLE   DEGRADED   PROGRESSING
<your-cluster>   managed-serviceaccount   True        False      False
```

## Usage

### Creating a ManagedServiceAccount

Create a sample ManagedServiceAccount resource:

```shell
kubectl create -f - <<EOF
apiVersion: authentication.open-cluster-management.io/v1beta1
kind: ManagedServiceAccount
metadata:
  name: my-sample
  namespace: <your-cluster-name>
spec:
  rotation: {}
EOF
```

### Checking Status

After creation, the addon agent will process the ManagedServiceAccount and update its status:

```shell
kubectl get managedserviceaccount my-sample -n <your-cluster-name> -o yaml
```

Expected status output:
```yaml
status:
  conditions:
  - lastTransitionTime: "2021-12-09T09:08:15Z"
    message: ""
    reason: TokenReported
    status: "True"
    type: TokenReported
  - lastTransitionTime: "2021-12-09T09:08:15Z"
    message: ""
    reason: SecretCreated
    status: "True"
    type: SecretCreated
  expirationTimestamp: "2022-12-04T09:08:15Z"
  tokenSecretRef:
    lastRefreshTimestamp: "2021-12-09T09:08:15Z"
    name: my-sample
```

### Accessing the Service Account Token

The corresponding secret containing the service account token will be created in the same namespace:

```shell
kubectl -n <your-cluster-name> get secret my-sample
```

Expected output:
```
NAME        TYPE     DATA   AGE
my-sample   Opaque   2      2m23s
```

You can retrieve the token from the secret:

```shell
kubectl -n <your-cluster-name> get secret my-sample -o jsonpath='{.data.token}' | base64 -d
```

## Hub Manager Metrics

The Helm chart renders a metrics `Service` for the hub addon manager so that
Prometheus can scrape it. Both the `Service` and the optional `ServiceMonitor`
are only rendered when the hub manager itself is deployed; selecting
`hubDeployMode: AddOnTemplate` (without the `ClusterProfile` feature gate)
skips them along with the manager `Deployment`.

`metrics.enabled` defaults to `true`, so upgrading an existing install creates
the additive `managed-serviceaccount-addon-manager-metrics` `ClusterIP` Service
in the release namespace. The Service only exposes the manager pod's existing
metrics port and never changes the addon's behavior; the actual Prometheus
wiring (`metrics.serviceMonitor.enabled`) stays opt-in. Set
`metrics.enabled: false` to keep the pre-metrics behavior of rendering no
metrics resources.

| `values.yaml` key | Default | Purpose |
| --- | --- | --- |
| `metrics.enabled` | `true` | Render the `managed-serviceaccount-addon-manager-metrics` Service in the release namespace. Set to `false` to skip the Service (and the ServiceMonitor). |
| `metrics.port` | `38080` | Port number used for both the Service `port`/`targetPort` and the manager pod's `metrics` container port. |
| `metrics.serviceMonitor.enabled` | `false` | Render a `monitoring.coreos.com/v1` ServiceMonitor that targets the metrics Service. Requires the Prometheus Operator's `ServiceMonitor` CRD to already exist on the hub cluster; the chart does not install it. |
| `metrics.serviceMonitor.labels` | `{}` | Additional labels to place on the ServiceMonitor, for example `release: prometheus` when using the kube-prometheus-stack release selector. |

Example overrides:

```yaml
metrics:
  enabled: true
  port: 38080
  serviceMonitor:
    enabled: true
    labels:
      release: prometheus
```

## Hosted Mode

Hosted mode runs the addon agent on a hosting cluster while the managed cluster
only exposes a service account. The hub addon manager renders the hosted-mode
workloads, so the manager must run in the default `hubDeployMode: Deployment`;
selecting `AddOnTemplate` disables the addon manager and skips all hosted-mode
templates.

### Prerequisites

Three pieces of state must exist before the addon can roll out in hosted mode:

1. On the hub, annotate the `ManagedClusterAddOn` with the hosting cluster
   name so the addon framework places hosted manifests on that cluster, and
   pick an install namespace that is unique to this managed cluster on the
   hosting cluster (see the note below). The v1alpha1 API accepts
   `spec.installNamespace`; on clusters serving the v1beta1 API this value is
   stored as the
   `addon.open-cluster-management.io/v1alpha1-install-namespace` annotation:

    ```yaml
    apiVersion: addon.open-cluster-management.io/v1alpha1
    kind: ManagedClusterAddOn
    metadata:
      name: managed-serviceaccount
      namespace: <managed-cluster-name>
      annotations:
        addon.open-cluster-management.io/hosting-cluster-name: <hosting-cluster-name>
    spec:
      installNamespace: klusterlet-<managed-cluster-name>
    ```

   > **Hosting-cluster namespace must be unique per managed cluster.** The
   > hosted-mode templates render the agent Deployment, the kubeconfig
   > provisioner Deployment/Job, the rotating kubeconfig Secret, and their
   > ServiceAccount/Role/RoleBinding with fixed object names
   > (`managed-serviceaccount`, `managed-serviceaccount-addon-agent`,
   > `managed-serviceaccount-kubeconfig-provisioner`,
   > `managed-serviceaccount-kubeconfig-cleanup`,
   > `<addon-name>-managed-kubeconfig`, and the
   > `open-cluster-management:managed-serviceaccount:addon-agent` Role/Binding)
   > in `AddonInstallNamespace`, which comes from the addon's effective
   > install namespace (`spec.installNamespace` in v1alpha1, or the converted
   > install-namespace annotation/status in v1beta1). If two managed clusters
   > share a hosting cluster and both point this value at the same namespace,
   > their per-cluster `ManifestWork`s will contend for the same object
   > identities on the hosting cluster. Use a per-managed-cluster namespace
   > such as `klusterlet-<managed-cluster-name>` (the OCM hosted-mode
   > convention).

2. On the hosting cluster, create the bootstrap kubeconfig secret in a
   namespace named after the managed cluster (the value of
   `ExternalManagedKubeConfigNamespace`). By default the kubeconfig
   provisioner reads a secret named `external-managed-kubeconfig` with a
   single `kubeconfig` data key that points at the managed cluster's API
   server. The provisioner uses that kubeconfig only to mint a short-lived
   token for the managed service account; it never writes the bootstrap
   kubeconfig into the generated target secret.

   > **The bootstrap kubeconfig must be self-contained.** The provisioner
   > copies the cluster entry into the generated target kubeconfig, which is
   > then consumed by the agent pod on the hosting cluster, and it builds a
   > client from the kubeconfig's user inside the provisioner pod to mint the
   > token. Embed the CA inline via `certificate-authority-data` and embed any
   > user credentials inline via `client-certificate-data`, `client-key-data`,
   > or `token`; a file-based `certificate-authority`, `client-certificate`,
   > `client-key`, or `tokenFile` path would not exist in those pods and is
   > rejected at sync time so the provisioner never writes an unusable target
   > kubeconfig.
   >
   > The user is only ever consumed inside the provisioner pod to mint the
   > token and is never copied into the target kubeconfig, so it does not
   > have to be a static credential. An `auth-provider` entry for a client-go
   > provider that authenticates against a remote endpoint (for example
   > `oidc`) works, because those plugins are compiled into the provisioner.
   > An `exec` credential, or an `auth-provider` that shells out to a local
   > helper binary (for example `gke-gcloud-auth-plugin` or
   > `aws-iam-authenticator`), does not: that binary is absent from the
   > provisioner image, so the managed client build would otherwise fail at
   > connect time. An `exec` credential is rejected at sync time for that
   > reason, alongside file-based paths. An `auth-provider` that shells out
   > cannot be distinguished from a remote-endpoint one and is not rejected, so
   > prefer an inline `token`/`client-certificate-data` or a remote-endpoint
   > `auth-provider` for the bootstrap user.

3. On the managed cluster, grant the identity embedded in that bootstrap
   kubeconfig permission to mint tokens for the target service account.
   The provisioner calls
   `ServiceAccounts(<install-namespace>).CreateToken(<managed-serviceaccount-name>, ...)`,
   which exercises the `serviceaccounts/token` subresource on the
   ManagedServiceAccountName service account inside the addon install
   namespace on the managed cluster. The minimum RBAC for the bootstrap
   identity is therefore:

    ```yaml
    apiVersion: rbac.authorization.k8s.io/v1
    kind: Role
    metadata:
      # Namespace must match addon.Spec.InstallNamespace on the managed cluster.
      # With the recommended convention this is klusterlet-<managed-cluster-name>.
      namespace: klusterlet-<managed-cluster-name>
      name: managed-serviceaccount-kubeconfig-provisioner
    rules:
      - apiGroups: [""]
        resources: ["serviceaccounts/token"]
        # Defaults to "managed-serviceaccount"; override with the value of
        # ManagedServiceAccountName when customized.
        resourceNames: ["managed-serviceaccount"]
        verbs: ["create"]
    ---
    apiVersion: rbac.authorization.k8s.io/v1
    kind: RoleBinding
    metadata:
      namespace: klusterlet-<managed-cluster-name>
      name: managed-serviceaccount-kubeconfig-provisioner
    roleRef:
      apiGroup: rbac.authorization.k8s.io
      kind: Role
      name: managed-serviceaccount-kubeconfig-provisioner
    subjects:
      # Bind to whatever User, Group, or ServiceAccount the bootstrap
      # kubeconfig authenticates as on the managed cluster.
      - kind: User
        name: <bootstrap-identity>
        apiGroup: rbac.authorization.k8s.io
    ```

   The `managed-serviceaccount` ServiceAccount in the same namespace is
   rendered to the managed cluster by `serviceaccount.yaml`, so the
   bootstrap identity does not need to create it; it only needs to issue
   tokens against it. If `ManagedServiceAccountName` is overridden through
   `AddOnDeploymentConfig`, update `resourceNames` to match.

### AddOnDeploymentConfig variables

The hub manager renders the hosted-mode workloads from the variables below.
Each one can be overridden per managed cluster through
`AddOnDeploymentConfig.spec.customizedVariables`; any unset variable falls
back to the default.

| Variable | Default | Purpose |
| --- | --- | --- |
| `ExternalManagedKubeConfigNamespace` | managed cluster name | Hosting-cluster namespace that holds the bootstrap kubeconfig secret. |
| `ExternalManagedKubeConfigSecret` | `external-managed-kubeconfig` | Bootstrap kubeconfig secret name on the hosting cluster. Must contain a `kubeconfig` key. |
| `ManagedKubeConfigSecret` | `<addon-name>-managed-kubeconfig` | Hosting-cluster secret in the addon install namespace that the provisioner writes the rotating kubeconfig into and that the agent pod mounts at `/etc/managed/kubeconfig`. Renaming it also moves the provisioner's `--target-secret` and the matching RBAC `resourceNames`. |
| `ManagedServiceAccountName` | `managed-serviceaccount` | Service account on the managed cluster whose token is minted into the generated kubeconfig. |
| `ManagedKubeConfigTokenExpirationSeconds` | `3600` | Requested `TokenRequest` lifetime in seconds. |
| `ManagedKubeConfigRefreshBeforeSeconds` | `600` | Refresh the generated secret when the token expires within this many seconds. |
| `ManagedKubeConfigProvisionerSyncInterval` | `5m` | Reconcile interval for the provisioner loop (Go duration string). |
| `AgentMetricsServiceEnabled` | `false` | Set to `true` to render the addon agent metrics Service for this managed cluster. The Service is also rendered when `AgentServiceMonitorEnabled` is `true`. |
| `AgentServiceMonitorEnabled` | `false` | Set to `true` to render the addon agent ServiceMonitor for this managed cluster. The target cluster must already have the Prometheus Operator ServiceMonitor CRD. In hosted mode this renders on the hosting cluster. |
| `AgentServiceMonitorLabels` | `""` | Comma-separated `key=value` labels to place on the addon agent ServiceMonitor, for example `release=prometheus,team=platform`. |

Example: point the provisioner at a non-default bootstrap secret and lengthen
the token lifetime.

```yaml
apiVersion: addon.open-cluster-management.io/v1alpha1
kind: AddOnDeploymentConfig
metadata:
  name: managed-serviceaccount-hosted
  namespace: <managed-cluster-name>
spec:
  customizedVariables:
    - name: ExternalManagedKubeConfigSecret
      value: my-managed-kubeconfig
    - name: ManagedKubeConfigTokenExpirationSeconds
      value: "7200"
    - name: ManagedKubeConfigRefreshBeforeSeconds
      value: "900"
```

Reference the config from the `ManagedClusterAddOn`:

```yaml
spec:
  configs:
    - group: addon.open-cluster-management.io
      resource: addondeploymentconfigs
      namespace: <managed-cluster-name>
      name: managed-serviceaccount-hosted
```

### Internal CLI surface (not a public interface)

Hosted mode adds two pieces of binary CLI surface that exist solely so the
addon manager can wire the hosted-mode workloads. They are implementation
details, not a stable public interface: configure hosted mode through the
`AddOnDeploymentConfig` variables above, not by invoking these directly.

- **`msa managed-kubeconfig-provisioner`** is the subcommand the manager runs in
  the provisioner `Deployment` and the pre-delete cleanup `Job`. Its flags
  (`--cluster-name`, `--source-namespace`, `--source-secret`,
  `--target-namespace`, `--target-secret`, `--managed-serviceaccount-namespace`,
  `--managed-serviceaccount-name`, `--token-expiration-seconds`,
  `--refresh-before`, `--sync-interval`, `--cleanup`) are populated from the
  rendered manifests; the user-facing knobs map to the `AddOnDeploymentConfig`
  variables above (for example `--token-expiration-seconds` ←
  `ManagedKubeConfigTokenExpirationSeconds`). The command expects an in-cluster
  config and is not intended for manual use.
- **`msa agent --lease-in-cluster-config`** is set to `true` only by the hosted
  agent `Deployment`. It tells the agent to report its health lease using the
  agent pod's in-cluster (hosting-cluster) config instead of the spoke cluster
  config, which is required because in hosted mode the agent runs on the hosting
  cluster. It has no effect in default (non-hosted) mode and should not be set
  manually.

These names and flags may change between releases without notice; depend on the
`AddOnDeploymentConfig` variables instead.

## References

- Design: [https://github.com/open-cluster-management-io/enhancements/tree/main/enhancements/sig-architecture/19-projected-serviceaccount-token](https://github.com/open-cluster-management-io/enhancements/tree/main/enhancements/sig-architecture/19-projected-serviceaccount-token)
- Addon-Framework: [https://github.com/open-cluster-management-io/addon-framework](https://github.com/open-cluster-management-io/addon-framework)
