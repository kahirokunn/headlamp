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

package clusterapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/kubernetes-sigs/headlamp/backend/pkg/kubeconfig"
)

const (
	// DefaultRootReconcileInterval is the default interval for reconciling Cluster API roots.
	DefaultRootReconcileInterval = 5 * time.Minute
	// DefaultNoCRDCacheTTL is the default TTL for API servers that do not have the Cluster CRD.
	DefaultNoCRDCacheTTL = 2 * time.Hour

	SecretType    corev1.SecretType = "cluster.x-k8s.io/secret" //nolint:gosec
	SecretDataKey string            = "value"

	contextPrefix      = "cluster-api-"
	clusterAPIIDPrefix = "cluster-api/"
	kubeconfigSuffix   = "-kubeconfig"
)

// ClusterGVR is the v1beta2 Cluster API Cluster resource this package supports.
var ClusterGVR = schema.GroupVersionResource{
	Group:    "cluster.x-k8s.io",
	Version:  "v1beta2",
	Resource: "clusters",
}

// Cluster contains the Cluster API fields Headlamp needs to create a context.
type Cluster struct {
	Root              string
	Namespace         string
	Name              string
	Labels            map[string]string
	Conditions        []metav1.Condition
	Phase             string
	KubernetesVersion string
}

type clusterObject struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              clusterSpec   `json:"spec,omitempty"`
	Status            clusterStatus `json:"status,omitempty"`
}

type clusterSpec struct {
	Topology *clusterTopology `json:"topology,omitempty"`
}

type clusterTopology struct {
	Version string `json:"version,omitempty"`
}

type clusterStatus struct {
	Conditions        []metav1.Condition `json:"conditions,omitempty"`
	Phase             string             `json:"phase,omitempty"`
	KubernetesVersion string             `json:"kubernetesVersion,omitempty"`
	Version           string             `json:"version,omitempty"`
}

// KubeconfigSecretName returns the standard Cluster API workload kubeconfig Secret name.
func KubeconfigSecretName(clusterName string) string {
	return clusterName + kubeconfigSuffix
}

// ClusterKey returns the stable identity of a Cluster API Cluster under a discovery root.
func ClusterKey(root, namespace, name string) string {
	return root + "/" + namespace + "/" + name
}

// SecretKey returns the stable identity of a Kubernetes Secret.
func SecretKey(namespace, name string) string {
	return namespace + "/" + name
}

// ClusterIDFromKey returns the /config clusterID for a Cluster API context.
func ClusterIDFromKey(clusterKey string) string {
	return clusterAPIIDPrefix + clusterKey
}

// ContextNameFromClusterKey returns Headlamp's generated context name for a Cluster.
func ContextNameFromClusterKey(clusterKey string) string {
	return contextPrefix +
		kubeconfig.MakeDNSFriendly(clusterKey) +
		"--" +
		clusterKeyHashSuffix(clusterKey)
}

// ContextName returns Headlamp's generated context name for a Cluster.
func ContextName(root, namespace, name string) string {
	return ContextNameFromClusterKey(ClusterKey(root, namespace, name))
}

func clusterKeyHashSuffix(clusterKey string) string {
	sum := sha256.Sum256([]byte(clusterKey))

	return hex.EncodeToString(sum[:6])
}

// ClusterFromUnstructured extracts supported v1beta2 Cluster fields.
func ClusterFromUnstructured(root string, obj *unstructured.Unstructured) (*Cluster, error) {
	if obj == nil {
		return nil, errors.New("cluster is nil")
	}

	if obj.GroupVersionKind().Group != ClusterGVR.Group || obj.GroupVersionKind().Version != ClusterGVR.Version {
		return nil, fmt.Errorf("unsupported Cluster API version %q", obj.GroupVersionKind().GroupVersion().String())
	}

	var cluster clusterObject
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &cluster); err != nil {
		return nil, fmt.Errorf("convert Cluster API Cluster: %w", err)
	}

	kubernetesVersion := cluster.Status.KubernetesVersion
	if kubernetesVersion == "" {
		kubernetesVersion = cluster.Status.Version
	}

	if kubernetesVersion == "" && cluster.Spec.Topology != nil {
		kubernetesVersion = cluster.Spec.Topology.Version
	}

	return &Cluster{
		Root:              root,
		Namespace:         cluster.Namespace,
		Name:              cluster.Name,
		Labels:            copyLabels(cluster.Labels),
		Conditions:        append([]metav1.Condition(nil), cluster.Status.Conditions...),
		Phase:             cluster.Status.Phase,
		KubernetesVersion: kubernetesVersion,
	}, nil
}

// ContextFromSecret converts a standard Cluster API kubeconfig Secret into a Headlamp context.
func ContextFromSecret(cluster Cluster, secret *corev1.Secret) (*kubeconfig.Context, error) {
	if err := validateCluster(cluster); err != nil {
		return nil, err
	}

	if err := validateSecret(cluster, secret); err != nil {
		return nil, err
	}

	config, err := clientcmd.Load(secret.Data[SecretDataKey])
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig from Cluster API Secret: %w", err)
	}

	contextName, kubeContext, err := selectedContext(config)
	if err != nil {
		return nil, err
	}

	clusterConfig, ok := config.Clusters[kubeContext.Cluster]
	if !ok || clusterConfig == nil {
		return nil, fmt.Errorf("kubeconfig context %q references missing cluster %q", contextName, kubeContext.Cluster)
	}

	authInfo, err := selectedAuthInfo(config, kubeContext)
	if err != nil {
		return nil, err
	}

	clusterKey := ClusterKey(cluster.Root, cluster.Namespace, cluster.Name)
	generatedName := ContextNameFromClusterKey(clusterKey)

	return &kubeconfig.Context{
		Name:        generatedName,
		KubeContext: kubeContext.DeepCopy(),
		Cluster:     clusterConfig.DeepCopy(),
		AuthInfo:    authInfo.DeepCopy(),
		Source:      kubeconfig.ClusterAPI,
		ClusterID:   ClusterIDFromKey(clusterKey),
		ClusterAPI:  metadataFromClusterAndSecret(cluster, secret, clusterKey),
	}, nil
}

func validateCluster(cluster Cluster) error {
	if strings.TrimSpace(cluster.Root) == "" {
		return errors.New("Cluster API root is empty")
	}

	if strings.TrimSpace(cluster.Namespace) == "" {
		return errors.New("Cluster API namespace is empty")
	}

	if strings.TrimSpace(cluster.Name) == "" {
		return errors.New("Cluster API cluster name is empty")
	}

	return nil
}

func validateSecret(cluster Cluster, secret *corev1.Secret) error {
	if secret == nil {
		return errors.New("Cluster API kubeconfig Secret is nil")
	}

	if secret.Namespace != cluster.Namespace {
		return fmt.Errorf("Cluster API kubeconfig Secret namespace %q does not match cluster namespace %q",
			secret.Namespace, cluster.Namespace)
	}

	expectedName := KubeconfigSecretName(cluster.Name)
	if secret.Name != expectedName {
		return fmt.Errorf("Cluster API kubeconfig Secret name %q does not match expected name %q",
			secret.Name, expectedName)
	}

	if secret.Type != SecretType {
		return fmt.Errorf("Cluster API kubeconfig Secret %s has unsupported type %q",
			SecretKey(secret.Namespace, secret.Name), secret.Type)
	}

	if len(secret.Data[SecretDataKey]) == 0 {
		return fmt.Errorf("Cluster API kubeconfig Secret %s is missing data key %q",
			SecretKey(secret.Namespace, secret.Name), SecretDataKey)
	}

	return nil
}

func selectedContext(config *clientcmdapi.Config) (string, *clientcmdapi.Context, error) {
	if config == nil {
		return "", nil, errors.New("kubeconfig is nil")
	}

	if config.CurrentContext != "" {
		kubeContext := config.Contexts[config.CurrentContext]
		if kubeContext == nil {
			return "", nil, fmt.Errorf("kubeconfig current context %q is missing", config.CurrentContext)
		}

		return config.CurrentContext, kubeContext, nil
	}

	if len(config.Contexts) != 1 {
		return "", nil, errors.New("kubeconfig must have current-context or exactly one context")
	}

	for name, kubeContext := range config.Contexts {
		if kubeContext == nil {
			return "", nil, fmt.Errorf("kubeconfig context %q is nil", name)
		}

		return name, kubeContext, nil
	}

	return "", nil, errors.New("kubeconfig has no contexts")
}

func selectedAuthInfo(config *clientcmdapi.Config, kubeContext *clientcmdapi.Context) (*clientcmdapi.AuthInfo, error) {
	if kubeContext.AuthInfo == "" {
		return &clientcmdapi.AuthInfo{}, nil
	}

	authInfo, ok := config.AuthInfos[kubeContext.AuthInfo]
	if !ok || authInfo == nil {
		return nil, fmt.Errorf("kubeconfig context references missing auth info %q", kubeContext.AuthInfo)
	}

	return authInfo, nil
}

func metadataFromClusterAndSecret(
	cluster Cluster,
	secret *corev1.Secret,
	clusterKey string,
) *kubeconfig.ClusterAPIMetadata {
	return &kubeconfig.ClusterAPIMetadata{
		Cluster: kubeconfig.ClusterAPICluster{
			Root:      cluster.Root,
			Namespace: cluster.Namespace,
			Name:      cluster.Name,
			Key:       clusterKey,
		},
		Conditions:        append([]metav1.Condition(nil), cluster.Conditions...),
		Phase:             cluster.Phase,
		KubernetesVersion: cluster.KubernetesVersion,
		KubeconfigSecret: kubeconfig.ClusterAPIKubeconfigSecret{
			Namespace: secret.Namespace,
			Name:      secret.Name,
			Key:       SecretKey(secret.Namespace, secret.Name),
		},
	}
}

func copyLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}

	return out
}
