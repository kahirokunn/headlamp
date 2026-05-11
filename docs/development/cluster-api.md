---
title: Cluster API discovery
sidebar_position: 7
---

Headlamp can discover workload clusters from
[Cluster API](https://cluster-api.sigs.k8s.io/) management clusters and add
them to the cluster list automatically.

This discovery path currently supports only
`cluster.x-k8s.io/v1beta2` `Cluster` resources. It does not watch v1beta1
Cluster API resources.

## Kubeconfig Secret format

For each discovered Cluster, Headlamp reads the standard Cluster API workload
kubeconfig Secret:

- Namespace: the same namespace as the `Cluster`
- Name: `<cluster-name>-kubeconfig`
- Type: `cluster.x-k8s.io/secret`
- Data key: `value`

The Secret value must contain a kubeconfig. Headlamp uses the kubeconfig's
`current-context`; if `current-context` is empty, the kubeconfig must contain
exactly one context.

## Backend configuration

Enable discovery with the backend flag:

```bash
--enable-cluster-api
```

The same option can be configured with an environment variable:

```bash
HEADLAMP_CONFIG_ENABLE_CLUSTER_API=true
```

Optional settings:

| Flag | Environment variable | Default | Description |
|------|----------------------|---------|-------------|
| `--cluster-api-label-selector` | `HEADLAMP_CONFIG_CLUSTER_API_LABEL_SELECTOR` | `""` | Kubernetes label selector used to filter Cluster resources |
| `--cluster-api-root-reconcile-interval` | `HEADLAMP_CONFIG_CLUSTER_API_ROOT_RECONCILE_INTERVAL` | `5m` | How often Headlamp reconciles discovery roots |
| `--cluster-api-no-crd-cache-ttl` | `HEADLAMP_CONFIG_CLUSTER_API_NO_CRD_CACHE_TTL` | `2h` | How long Headlamp waits before retrying an API server without the v1beta2 Cluster CRD |

When Headlamp runs in-cluster, it watches Cluster API resources from the
in-cluster API server. When Headlamp runs outside the cluster, it treats the
configured kubeconfig contexts as discovery roots and watches Cluster API
resources from those API servers.

## Local development

Build the backend:

```bash
npm run backend:build
```

Start the backend with Cluster API discovery enabled:

```bash
KUBECONFIG="$WORK/hub.kubeconfig" \
HEADLAMP_BACKEND_TOKEN=headlamp \
./backend/headlamp-server -dev -listen-addr=localhost \
  --enable-cluster-api \
  --cluster-api-label-selector='!headlamp.dev/ignore' \
  --cluster-api-root-reconcile-interval=10s \
  --cluster-api-no-crd-cache-ttl=30s
```

Start the frontend in another terminal:

```bash
npm run frontend:start
```

Then open `http://localhost:3000`. Cluster API entries are shown in the
cluster table with the origin `Cluster API`.

## Helm configuration

For in-cluster deployments, enable discovery with Helm values:

```yaml
config:
  clusterAPI:
    enabled: true
    labelSelector: "!headlamp.dev/ignore"
    rootReconcileInterval: 30s
    noCRDCacheTTL: 10m
```

## RBAC

The Headlamp service account needs permission to watch Cluster API Cluster
resources and read kubeconfig Secrets.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: headlamp-cluster-api-discovery
rules:
  - apiGroups: ["cluster.x-k8s.io"]
    resources: ["clusters"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "list", "watch"]
```

Where possible, use namespace-scoped `Role` objects for Secret access. Cluster
API kubeconfig Secrets contain workload cluster credentials.

## Local verification

Use your Cluster API provider or test environment to install the v1beta2 CRDs
and create a workload `Cluster`. Then create the standard kubeconfig Secret in
the same namespace:

```bash
kubectl -n capi-system create secret generic spoke-a-kubeconfig \
  --type cluster.x-k8s.io/secret \
  --from-file=value="$WORK/spoke-a.kubeconfig"
```

Check the backend config response:

```bash
curl -s http://localhost:4466/config \
  -H 'X-HEADLAMP_BACKEND-TOKEN: headlamp' \
  | jq '.clusters[] | select(.meta_data.source == "cluster_api") | .meta_data'
```

Expected metadata includes:

- `source: "cluster_api"`
- `clusterID: "cluster-api/<root>/<namespace>/<name>"`
- `clusterAPI.cluster`
- `clusterAPI.conditions`
- `clusterAPI.phase`
- `clusterAPI.kubernetesVersion`
- `clusterAPI.kubeconfigSecret`

The opt-in E2E smoke test also exercises proxying to discovered clusters when
`HEADLAMP_CLUSTER_API_E2E=true` is set.
