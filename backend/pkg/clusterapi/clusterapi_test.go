/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package clusterapi //nolint:testpackage

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"

	"github.com/kubernetes-sigs/headlamp/backend/pkg/kubeconfig"
)

const testKubeconfig = `apiVersion: v1
kind: Config
current-context: workload
clusters:
- name: workload-cluster
  cluster:
    server: https://workload.example.com
    certificate-authority-data: Y2EtZGF0YQ==
users:
- name: workload-user
  user:
    token: workload-token
contexts:
- name: workload
  context:
    cluster: workload-cluster
    user: workload-user
    namespace: default
`

func testCluster() Cluster {
	return Cluster{
		Root:      "in-cluster",
		Namespace: "default",
		Name:      "spoke-a",
		Conditions: []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Available"},
		},
		Phase:             "Provisioned",
		KubernetesVersion: "v1.35.0",
	}
}

func testSecret(name string, data []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      name,
		},
		Type: SecretType,
		Data: map[string][]byte{
			SecretDataKey: data,
		},
	}
}

func TestContextNameFromClusterKey(t *testing.T) {
	tests := []struct {
		clusterKey string
		want       string
	}{
		{
			clusterKey: "in-cluster/default/spoke-a",
			want:       "cluster-api-in-cluster--default--spoke-a--dbdb0aa95e5d",
		},
		{
			clusterKey: "store/minikube/default/spoke-a",
			want:       "cluster-api-store--minikube--default--spoke-a--4c4ba4f64db8",
		},
	}

	for _, tt := range tests {
		t.Run(tt.clusterKey, func(t *testing.T) {
			assert.Equal(t, tt.want, ContextNameFromClusterKey(tt.clusterKey))
		})
	}
}

func TestContextFromSecret(t *testing.T) {
	headlampContext, err := ContextFromSecret(testCluster(), testSecret("spoke-a-kubeconfig", []byte(testKubeconfig)))
	require.NoError(t, err)

	assert.Equal(t, "cluster-api-in-cluster--default--spoke-a--dbdb0aa95e5d", headlampContext.Name)
	assert.Equal(t, kubeconfig.ClusterAPI, headlampContext.Source)
	assert.Equal(t, "cluster-api/in-cluster/default/spoke-a", headlampContext.ClusterID)
	assert.Equal(t, "https://workload.example.com", headlampContext.Cluster.Server)
	assert.Equal(t, []byte("ca-data"), headlampContext.Cluster.CertificateAuthorityData)
	assert.Equal(t, "workload-token", headlampContext.AuthInfo.Token)
	assert.Equal(t, "default", headlampContext.KubeContext.Namespace)

	require.NotNil(t, headlampContext.ClusterAPI)
	assert.Equal(t, kubeconfig.ClusterAPICluster{
		Root:      "in-cluster",
		Namespace: "default",
		Name:      "spoke-a",
		Key:       "in-cluster/default/spoke-a",
	}, headlampContext.ClusterAPI.Cluster)
	assert.Equal(t, testCluster().Conditions, headlampContext.ClusterAPI.Conditions)
	assert.Equal(t, "Provisioned", headlampContext.ClusterAPI.Phase)
	assert.Equal(t, "v1.35.0", headlampContext.ClusterAPI.KubernetesVersion)
	assert.Equal(t, kubeconfig.ClusterAPIKubeconfigSecret{
		Namespace: "default",
		Name:      "spoke-a-kubeconfig",
		Key:       "default/spoke-a-kubeconfig",
	}, headlampContext.ClusterAPI.KubeconfigSecret)
}

func TestContextFromSecretValidatesStandardSecret(t *testing.T) {
	_, err := ContextFromSecret(testCluster(), nil)
	require.ErrorContains(t, err, "Secret is nil")

	secret := testSecret("wrong-kubeconfig", []byte(testKubeconfig))
	_, err = ContextFromSecret(testCluster(), secret)
	require.ErrorContains(t, err, "does not match expected name")

	secret = testSecret("spoke-a-kubeconfig", []byte(testKubeconfig))
	secret.Type = corev1.SecretTypeOpaque
	_, err = ContextFromSecret(testCluster(), secret)
	require.ErrorContains(t, err, "unsupported type")

	secret = testSecret("spoke-a-kubeconfig", nil)
	_, err = ContextFromSecret(testCluster(), secret)
	require.ErrorContains(t, err, "missing data key")
}

func TestContextFromSecretUsesSingleContextWithoutCurrentContext(t *testing.T) {
	kubeconfigWithoutCurrent := []byte(`apiVersion: v1
kind: Config
clusters:
- name: workload-cluster
  cluster:
    server: https://workload.example.com
users:
- name: workload-user
  user:
    token: workload-token
contexts:
- name: workload
  context:
    cluster: workload-cluster
    user: workload-user
`)

	headlampContext, err := ContextFromSecret(testCluster(), testSecret("spoke-a-kubeconfig", kubeconfigWithoutCurrent))
	require.NoError(t, err)

	assert.Equal(t, "https://workload.example.com", headlampContext.Cluster.Server)
}

func TestClusterFromUnstructured(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cluster.x-k8s.io/v1beta2",
		"kind":       "Cluster",
		"metadata": map[string]interface{}{
			"namespace": "default",
			"name":      "spoke-a",
			"labels": map[string]interface{}{
				"environment": "dev",
			},
		},
		"spec": map[string]interface{}{
			"topology": map[string]interface{}{
				"version": "v1.35.0",
			},
		},
		"status": map[string]interface{}{
			"phase": "Provisioned",
			"conditions": []interface{}{
				map[string]interface{}{
					"type":    "Ready",
					"status":  "True",
					"reason":  "Available",
					"message": "cluster is available",
				},
			},
		},
	}}

	cluster, err := ClusterFromUnstructured("in-cluster", obj)
	require.NoError(t, err)

	assert.Equal(t, "in-cluster", cluster.Root)
	assert.Equal(t, "default", cluster.Namespace)
	assert.Equal(t, "spoke-a", cluster.Name)
	assert.Equal(t, map[string]string{"environment": "dev"}, cluster.Labels)
	assert.Equal(t, "Provisioned", cluster.Phase)
	assert.Equal(t, "v1.35.0", cluster.KubernetesVersion)
	require.Len(t, cluster.Conditions, 1)
	assert.Equal(t, "Ready", cluster.Conditions[0].Type)
	assert.Equal(t, metav1.ConditionTrue, cluster.Conditions[0].Status)
}

func TestClusterFromUnstructuredRequiresV1Beta2(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cluster.x-k8s.io/v1beta1",
		"kind":       "Cluster",
		"metadata": map[string]interface{}{
			"namespace": "default",
			"name":      "spoke-a",
		},
	}}

	_, err := ClusterFromUnstructured("in-cluster", obj)
	require.ErrorContains(t, err, "unsupported Cluster API version")
}

func TestNewRunnerValidatesOptions(t *testing.T) {
	_, err := NewRunner(Options{})
	require.ErrorContains(t, err, "context store is required")

	_, err = NewRunner(Options{
		Store:         kubeconfig.NewContextStore(),
		LabelSelector: "headlamp.dev/ignore in (",
	})
	require.ErrorContains(t, err, "invalid cluster-api-label-selector")

	runner, err := NewRunner(Options{Store: kubeconfig.NewContextStore()})
	require.NoError(t, err)
	assert.Equal(t, DefaultRootReconcileInterval, runner.rootReconcileInterval)
	assert.Equal(t, DefaultNoCRDCacheTTL, runner.noCRDCacheTTL)
}

func TestClusterUpsertSyncsKubeconfigSecret(t *testing.T) {
	store := kubeconfig.NewContextStore()
	runner := newRunnerForTest(t, store)
	state := newRootStateForTest(t, runner)
	cluster := testClusterResource(nil)
	secret := testSecret("spoke-a-kubeconfig", []byte(testKubeconfig))

	require.NoError(t, state.secretInformer.GetStore().Add(secret))
	runner.handleClusterUpsert(state, cluster)

	headlampContext := requireClusterContext(t, store)
	assert.Equal(t, "https://workload.example.com", headlampContext.Cluster.Server)
	assert.Equal(t, kubeconfig.ClusterAPI, headlampContext.Source)
	require.NotNil(t, headlampContext.ClusterAPI)
	assert.Equal(t, "Provisioned", headlampContext.ClusterAPI.Phase)
}

func TestSecretUpsertSyncsExistingCluster(t *testing.T) {
	store := kubeconfig.NewContextStore()
	runner := newRunnerForTest(t, store)
	state := newRootStateForTest(t, runner)
	cluster := testClusterResource(nil)
	secret := testSecret("spoke-a-kubeconfig", []byte(testKubeconfig))

	require.NoError(t, state.clusterInformer.GetStore().Add(cluster))
	runner.handleClusterUpsert(state, cluster)

	_, err := store.GetContext(ContextName("in-cluster", "default", "spoke-a"))
	require.Error(t, err)

	require.NoError(t, state.secretInformer.GetStore().Add(secret))
	runner.handleSecretUpsert(state, secret)

	requireClusterContext(t, store)
}

func TestSecretDeletePrunesContext(t *testing.T) {
	store := kubeconfig.NewContextStore()
	runner := newRunnerForTest(t, store)
	state := newRootStateForTest(t, runner)
	cluster := testClusterResource(nil)
	secret := testSecret("spoke-a-kubeconfig", []byte(testKubeconfig))

	require.NoError(t, state.secretInformer.GetStore().Add(secret))
	runner.handleClusterUpsert(state, cluster)
	requireClusterContext(t, store)

	runner.handleSecretDelete(state, secret)

	_, err := store.GetContext(ContextName("in-cluster", "default", "spoke-a"))
	require.Error(t, err)
}

func TestClusterDeletePrunesContextOutsideRunnerLock(t *testing.T) {
	store := &removeLockDetectingStore{ContextStore: kubeconfig.NewContextStore()}
	runner := newRunnerForTest(t, store)
	store.runner = runner
	state := newRootStateForTest(t, runner)
	clusterKey := ClusterKey(state.rootID, "default", "spoke-a")
	contextName := ContextNameFromClusterKey(clusterKey)

	require.NoError(t, store.AddContext(&kubeconfig.Context{Name: contextName}))

	runner.mu.Lock()
	runner.clusterKeysByRoot[state.rootID] = map[string]struct{}{clusterKey: {}}
	runner.clusters[clusterKey] = clusterState{contextName: contextName}
	runner.mu.Unlock()

	runner.handleClusterDelete(state, testClusterResource(nil))

	assert.False(t, store.removeWhileLocked.Load())

	_, err := store.GetContext(contextName)
	require.Error(t, err)
}

func TestCompleteRootSyncPrunesMissingClusters(t *testing.T) {
	store := kubeconfig.NewContextStore()
	runner := newRunnerForTest(t, store)
	state := newRootStateForTest(t, runner)
	oldKey := ClusterKey(state.rootID, "default", "old")
	oldContextName := ContextNameFromClusterKey(oldKey)

	require.NoError(t, store.AddContext(&kubeconfig.Context{Name: oldContextName}))
	runner.mu.Lock()
	runner.clusterKeysByRoot[state.rootID] = map[string]struct{}{oldKey: {}}
	runner.clusters[oldKey] = clusterState{contextName: oldContextName}
	runner.mu.Unlock()

	require.NoError(t, state.clusterInformer.GetStore().Add(testClusterResource(nil)))
	require.NoError(t, state.secretInformer.GetStore().Add(testSecret("spoke-a-kubeconfig", []byte(testKubeconfig))))

	runner.completeRootSyncFromCache(state)

	_, err := store.GetContext(oldContextName)
	require.Error(t, err)
	requireClusterContext(t, store)
}

func TestClusterLabelSelectorPrunesContext(t *testing.T) {
	store := kubeconfig.NewContextStore()
	runner, err := NewRunner(Options{
		Store:         store,
		LabelSelector: "environment=prod",
	})
	require.NoError(t, err)
	state := newRootStateForTest(t, runner)
	clusterKey := ClusterKey(state.rootID, "default", "spoke-a")
	contextName := ContextNameFromClusterKey(clusterKey)

	require.NoError(t, store.AddContext(&kubeconfig.Context{Name: contextName}))
	runner.mu.Lock()
	runner.clusterKeysByRoot[state.rootID] = map[string]struct{}{clusterKey: {}}
	runner.clusters[clusterKey] = clusterState{contextName: contextName}
	runner.mu.Unlock()

	runner.handleClusterUpsert(state, testClusterResource(map[string]string{"environment": "dev"}))

	_, err = store.GetContext(contextName)
	require.Error(t, err)
}

func TestNoCRDWatchErrorStopsRootAndCachesServer(t *testing.T) {
	store := kubeconfig.NewContextStore()
	runner := newRunnerForTest(t, store)
	state := newRootStateForTest(t, runner)
	state.serverURL = "https://hub.example.com"
	clusterKey := ClusterKey(state.rootID, "default", "spoke-a")
	contextName := ContextNameFromClusterKey(clusterKey)

	require.NoError(t, store.AddContext(&kubeconfig.Context{Name: contextName}))
	runner.mu.Lock()
	runner.clusterKeysByRoot[state.rootID] = map[string]struct{}{clusterKey: {}}
	runner.clusters[clusterKey] = clusterState{contextName: contextName}
	runner.mu.Unlock()

	runner.handleRootWatchError(state, &meta.NoKindMatchError{
		GroupKind: schema.GroupKind{Group: "cluster.x-k8s.io", Kind: "Cluster"},
	})

	assert.True(t, runner.hasNoCRD("https://hub.example.com"))

	_, err := store.GetContext(contextName)
	require.Error(t, err)
}

type removeLockDetectingStore struct {
	kubeconfig.ContextStore
	runner            *Runner
	removeWhileLocked atomic.Bool
}

func (s *removeLockDetectingStore) RemoveContext(name string) error {
	if s.runner != nil {
		if s.runner.mu.TryLock() {
			s.runner.mu.Unlock()
		} else {
			s.removeWhileLocked.Store(true)
		}
	}

	return s.ContextStore.RemoveContext(name)
}

func newRunnerForTest(t *testing.T, store kubeconfig.ContextStore) *Runner {
	t.Helper()

	runner, err := NewRunner(Options{Store: store})
	require.NoError(t, err)

	return runner
}

func newRootStateForTest(t *testing.T, runner *Runner) *rootState {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	state := &rootState{
		rootID: inClusterRootID,
		ctx:    ctx,
		cancel: cancel,
		clusterInformer: cache.NewSharedIndexInformer(
			&cache.ListWatch{},
			&unstructured.Unstructured{},
			0,
			cache.Indexers{},
		),
		secretInformer: cache.NewSharedIndexInformer(
			&cache.ListWatch{},
			&corev1.Secret{},
			0,
			cache.Indexers{},
		),
	}

	runner.mu.Lock()
	runner.roots[inClusterRootID] = state
	runner.mu.Unlock()

	return state
}

func requireClusterContext(t *testing.T, store kubeconfig.ContextStore) *kubeconfig.Context {
	t.Helper()

	headlampContext, err := store.GetContext(ContextName("in-cluster", "default", "spoke-a"))
	require.NoError(t, err)

	return headlampContext
}

func testClusterResource(labels map[string]string) *unstructured.Unstructured {
	labelMap := map[string]interface{}{}
	for key, value := range labels {
		labelMap[key] = value
	}

	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cluster.x-k8s.io/v1beta2",
		"kind":       "Cluster",
		"metadata": map[string]interface{}{
			"namespace": "default",
			"name":      "spoke-a",
			"labels":    labelMap,
		},
		"status": map[string]interface{}{
			"phase": "Provisioned",
		},
	}}
}
