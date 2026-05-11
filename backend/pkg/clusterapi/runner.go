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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	dynamicinformer "k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/kubernetes-sigs/headlamp/backend/pkg/kubeconfig"
	"github.com/kubernetes-sigs/headlamp/backend/pkg/logger"
)

const (
	inClusterRootID = "in-cluster"
	storeRootPrefix = "store/"
)

// Options controls Cluster API discovery.
type Options struct {
	Store                 kubeconfig.ContextStore
	LabelSelector         string
	RootReconcileInterval time.Duration
	NoCRDCacheTTL         time.Duration
	HubConfig             *rest.Config
	DiscoverFromStore     bool
}

// Runner watches v1beta2 Cluster API Cluster resources and syncs workload contexts.
type Runner struct {
	store                 kubeconfig.ContextStore
	rootReconcileInterval time.Duration
	noCRDCacheTTL         time.Duration
	labelSelector         labels.Selector
	hubConfig             *rest.Config
	discoverFromStore     bool

	clientForConfig func(*rest.Config) (rootClients, error)
	now             func() time.Time

	mu                sync.Mutex
	roots             map[string]*rootState
	clusters          map[string]clusterState
	clusterKeysByRoot map[string]map[string]struct{}
	noCRD             map[string]time.Time
}

type rootClients struct {
	dynamic    dynamic.Interface
	kubernetes kubernetes.Interface
}

type rootState struct {
	rootID          string
	serverURL       string
	fingerprint     string
	ctx             context.Context
	cancel          context.CancelFunc
	clusterInformer cache.SharedIndexInformer
	secretInformer  cache.SharedIndexInformer
}

type rootInformer struct {
	state          *rootState
	clusterFactory dynamicinformer.DynamicSharedInformerFactory
	secretFactory  informers.SharedInformerFactory
}

type clusterState struct {
	contextName string
}

// NewRunner validates options and returns a Cluster API discovery runner.
func NewRunner(opts Options) (*Runner, error) {
	if opts.Store == nil {
		return nil, errors.New("context store is required")
	}

	labelSelector, err := normalizeLabelSelector(opts.LabelSelector)
	if err != nil {
		return nil, err
	}

	rootReconcileInterval := opts.RootReconcileInterval
	if rootReconcileInterval <= 0 {
		rootReconcileInterval = DefaultRootReconcileInterval
	}

	noCRDCacheTTL := opts.NoCRDCacheTTL
	if noCRDCacheTTL <= 0 {
		noCRDCacheTTL = DefaultNoCRDCacheTTL
	}

	return &Runner{
		store:                 opts.Store,
		rootReconcileInterval: rootReconcileInterval,
		noCRDCacheTTL:         noCRDCacheTTL,
		labelSelector:         labelSelector,
		hubConfig:             opts.HubConfig,
		discoverFromStore:     opts.DiscoverFromStore,
		clientForConfig: func(config *rest.Config) (rootClients, error) {
			dynamicClient, err := dynamic.NewForConfig(config)
			if err != nil {
				return rootClients{}, err
			}

			kubeClient, err := kubernetes.NewForConfig(config)
			if err != nil {
				return rootClients{}, err
			}

			return rootClients{dynamic: dynamicClient, kubernetes: kubeClient}, nil
		},
		now:               time.Now,
		roots:             map[string]*rootState{},
		clusters:          map[string]clusterState{},
		clusterKeysByRoot: map[string]map[string]struct{}{},
		noCRD:             map[string]time.Time{},
	}, nil
}

// Run blocks until ctx is cancelled and reconciles long-lived root informers.
func (r *Runner) Run(ctx context.Context) {
	defer r.stopAllRoots()

	r.reconcileRoots(ctx)

	ticker := time.NewTicker(r.rootReconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reconcileRoots(ctx)
		}
	}
}

func (r *Runner) reconcileRoots(ctx context.Context) {
	if err := ctx.Err(); err != nil {
		return
	}

	presentRoots := map[string]struct{}{}
	desiredRoots := map[string]*rest.Config{}
	storeRootsLoaded := true

	if r.hubConfig != nil {
		presentRoots[inClusterRootID] = struct{}{}
		desiredRoots[inClusterRootID] = r.hubConfig
	}

	if r.discoverFromStore {
		storeRootsLoaded = r.collectStoreSeedRoots(desiredRoots, presentRoots)
	}

	r.stopMissingRoots(presentRoots, storeRootsLoaded)

	rootIDs := make([]string, 0, len(desiredRoots))
	for rootID := range desiredRoots {
		rootIDs = append(rootIDs, rootID)
	}

	sort.Strings(rootIDs)

	for _, rootID := range rootIDs {
		r.reconcileRoot(ctx, rootID, desiredRoots[rootID])
	}
}

func (r *Runner) collectStoreSeedRoots(
	desiredRoots map[string]*rest.Config,
	presentRoots map[string]struct{},
) bool {
	contexts, err := r.store.GetContexts()
	if err != nil {
		logger.Log(logger.LevelWarn, nil, err, "cluster-api: failed to get seed contexts")

		return false
	}

	sort.Slice(contexts, func(i, j int) bool {
		return contexts[i].Name < contexts[j].Name
	})

	for _, headlampContext := range contexts {
		if headlampContext.Source == kubeconfig.ClusterAPI || headlampContext.Internal {
			continue
		}

		rootID := storeRootPrefix + headlampContext.Name
		presentRoots[rootID] = struct{}{}

		seedConfig, err := headlampContext.RESTConfig()
		if err != nil {
			logger.Log(logger.LevelWarn, map[string]string{"context": headlampContext.Name}, err,
				"cluster-api: failed to build seed rest config")

			continue
		}

		desiredRoots[rootID] = seedConfig
	}

	return true
}

func (r *Runner) reconcileRoot(ctx context.Context, rootID string, config *rest.Config) {
	if config == nil {
		return
	}

	if err := ctx.Err(); err != nil {
		return
	}

	serverURL := normalizeServerURL(config.Host)
	if r.hasNoCRD(serverURL) {
		r.stopRoot(rootID, true)

		return
	}

	fingerprint := restConfigFingerprint(config)
	if r.rootMatches(rootID, serverURL, fingerprint) {
		return
	}

	rootInformer, ok := r.newRootInformer(ctx, rootID, serverURL, fingerprint, config)
	if !ok {
		return
	}

	previous, current := r.activateRoot(rootInformer.state)
	if current {
		rootInformer.state.cancel()

		return
	}

	if previous != nil {
		previous.cancel()
	}

	go r.runRootInformers(rootInformer)
}

func (r *Runner) rootMatches(rootID, serverURL, fingerprint string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	current := r.roots[rootID]

	return current != nil && current.serverURL == serverURL && current.fingerprint == fingerprint
}

func (r *Runner) newRootInformer(
	ctx context.Context,
	rootID string,
	serverURL string,
	fingerprint string,
	config *rest.Config,
) (*rootInformer, bool) {
	clients, err := r.clientForConfig(rest.CopyConfig(config))
	if err != nil {
		logger.Log(logger.LevelWarn, map[string]string{"root": rootID, "server": config.Host}, err,
			"cluster-api: failed to create clients")

		return nil, false
	}

	rootCtx, cancel := context.WithCancel(ctx)
	clusterFactory := r.newClusterInformerFactory(clients.dynamic)
	clusterInformer := clusterFactory.ForResource(ClusterGVR).Informer()
	secretFactory := r.newSecretInformerFactory(clients.kubernetes)
	secretInformer := secretFactory.Core().V1().Secrets().Informer()
	state := &rootState{
		rootID:          rootID,
		serverURL:       serverURL,
		fingerprint:     fingerprint,
		ctx:             rootCtx,
		cancel:          cancel,
		clusterInformer: clusterInformer,
		secretInformer:  secretInformer,
	}

	if err := r.configureRootInformerHandlers(state); err != nil {
		cancel()
		logger.Log(logger.LevelWarn, map[string]string{"root": rootID, "server": config.Host}, err,
			"cluster-api: failed to configure root informer")

		return nil, false
	}

	return &rootInformer{
		state:          state,
		clusterFactory: clusterFactory,
		secretFactory:  secretFactory,
	}, true
}

func (r *Runner) configureRootInformerHandlers(state *rootState) error {
	if _, err := state.clusterInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			r.handleClusterUpsert(state, obj)
		},
		UpdateFunc: func(_, newObj interface{}) {
			r.handleClusterUpsert(state, newObj)
		},
		DeleteFunc: func(obj interface{}) {
			r.handleClusterDelete(state, obj)
		},
	}); err != nil {
		return fmt.Errorf("add Cluster event handler: %w", err)
	}

	if _, err := state.secretInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			r.handleSecretUpsert(state, obj)
		},
		UpdateFunc: func(_, newObj interface{}) {
			r.handleSecretUpsert(state, newObj)
		},
		DeleteFunc: func(obj interface{}) {
			r.handleSecretDelete(state, obj)
		},
	}); err != nil {
		return fmt.Errorf("add Secret event handler: %w", err)
	}

	if err := state.clusterInformer.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		r.handleRootWatchError(state, err)
	}); err != nil {
		return fmt.Errorf("set Cluster watch error handler: %w", err)
	}

	return nil
}

func (r *Runner) newClusterInformerFactory(client dynamic.Interface) dynamicinformer.DynamicSharedInformerFactory {
	return dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		client,
		0,
		metav1.NamespaceAll,
		r.clusterInformerOptions(),
	)
}

func (r *Runner) clusterInformerOptions() func(*metav1.ListOptions) {
	if r.labelSelector == nil {
		return nil
	}

	selector := r.labelSelector.String()

	return func(options *metav1.ListOptions) {
		options.LabelSelector = selector
	}
}

func (r *Runner) newSecretInformerFactory(client kubernetes.Interface) informers.SharedInformerFactory {
	return informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithNamespace(metav1.NamespaceAll))
}

func (r *Runner) activateRoot(state *rootState) (*rootState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	previous := r.roots[state.rootID]
	if previous != nil && previous.serverURL == state.serverURL && previous.fingerprint == state.fingerprint {
		return nil, true
	}

	r.roots[state.rootID] = state

	return previous, false
}

func (r *Runner) runRootInformers(rootInformer *rootInformer) {
	state := rootInformer.state

	rootInformer.clusterFactory.Start(state.ctx.Done())
	rootInformer.secretFactory.Start(state.ctx.Done())

	if !cache.WaitForCacheSync(state.ctx.Done(), state.clusterInformer.HasSynced, state.secretInformer.HasSynced) {
		return
	}

	r.completeRootSyncFromCache(state)

	<-state.ctx.Done()
}

func (r *Runner) handleClusterUpsert(state *rootState, obj interface{}) {
	cluster, ok := r.clusterFromObject(state, obj)
	if !ok {
		return
	}

	clusterKey := ClusterKey(state.rootID, cluster.Namespace, cluster.Name)
	if !r.clusterMatchesSelector(cluster) {
		r.pruneCluster(state, clusterKey)

		return
	}

	if !r.recordRootCluster(state, clusterKey) {
		return
	}

	secret, ok := secretFromCache(state, cluster.Namespace, KubeconfigSecretName(cluster.Name))
	if !ok {
		logger.Log(logger.LevelInfo, map[string]string{"cluster": clusterKey}, nil,
			"cluster-api: kubeconfig Secret not found")

		return
	}

	r.syncCluster(state.ctx, state, clusterKey, *cluster, secret)
}

func (r *Runner) handleClusterDelete(state *rootState, obj interface{}) {
	cluster, ok := r.clusterFromObject(state, obj)
	if !ok {
		return
	}

	r.pruneCluster(state, ClusterKey(state.rootID, cluster.Namespace, cluster.Name))
}

func (r *Runner) handleSecretUpsert(state *rootState, obj interface{}) {
	secret, ok := secretFromObject(obj)
	if !ok || !isClusterAPIKubeconfigSecret(secret) {
		return
	}

	clusterName, ok := clusterNameFromSecret(secret.Name)
	if !ok {
		return
	}

	cluster, ok := clusterFromCache(state, secret.Namespace, clusterName)
	if !ok {
		return
	}

	r.handleClusterUpsert(state, cluster)
}

func (r *Runner) handleSecretDelete(state *rootState, obj interface{}) {
	secret, ok := secretFromObject(obj)
	if !ok || !isClusterAPIKubeconfigSecret(secret) {
		return
	}

	clusterName, ok := clusterNameFromSecret(secret.Name)
	if !ok {
		return
	}

	clusterKey := ClusterKey(state.rootID, secret.Namespace, clusterName)
	r.pruneClusterContext(state, clusterKey)
}

func (r *Runner) handleRootWatchError(state *rootState, err error) {
	if isNoCRDError(err) {
		r.markRootNoCRD(state)
		logger.Log(logger.LevelInfo, map[string]string{"root": state.rootID, "server": state.serverURL}, nil,
			"cluster-api: v1beta2 Cluster CRD is not available")

		return
	}

	logger.Log(logger.LevelWarn, map[string]string{"root": state.rootID, "server": state.serverURL}, err,
		"cluster-api: Cluster watch error")
}

func (r *Runner) syncCluster(
	ctx context.Context,
	state *rootState,
	clusterKey string,
	cluster Cluster,
	secret *corev1.Secret,
) {
	if err := ctx.Err(); err != nil {
		return
	}

	if !r.isCurrentRoot(state) {
		return
	}

	headlampContext, err := ContextFromSecret(cluster, secret)
	if err != nil {
		logger.Log(logger.LevelWarn, map[string]string{"cluster": clusterKey}, err,
			"cluster-api: failed to convert kubeconfig Secret")

		return
	}

	if err := headlampContext.SetupProxy(); err != nil {
		logger.Log(logger.LevelWarn, map[string]string{"cluster": clusterKey}, err,
			"cluster-api: failed to setup proxy")

		return
	}

	if !r.isCurrentRoot(state) {
		return
	}

	if err := r.store.AddContext(headlampContext); err != nil {
		logger.Log(logger.LevelWarn, map[string]string{"cluster": clusterKey}, err,
			"cluster-api: failed to add context")

		return
	}

	r.recordSyncedCluster(state, clusterKey, headlampContext.Name)
}

func (r *Runner) completeRootSyncFromCache(state *rootState) {
	seen := map[string]struct{}{}

	var clusters []Cluster

	for _, obj := range state.clusterInformer.GetIndexer().List() {
		cluster, ok := r.clusterFromObject(state, obj)
		if !ok || !r.clusterMatchesSelector(cluster) {
			continue
		}

		clusterKey := ClusterKey(state.rootID, cluster.Namespace, cluster.Name)
		seen[clusterKey] = struct{}{}

		clusters = append(clusters, *cluster)
	}

	if !r.replaceRootClusters(state, seen) {
		return
	}

	for _, cluster := range clusters {
		secret, ok := secretFromCache(state, cluster.Namespace, KubeconfigSecretName(cluster.Name))
		if !ok {
			continue
		}

		r.syncCluster(state.ctx, state, ClusterKey(state.rootID, cluster.Namespace, cluster.Name), cluster, secret)
	}
}

func (r *Runner) replaceRootClusters(state *rootState, seen map[string]struct{}) bool {
	r.mu.Lock()

	if r.roots[state.rootID] != state {
		r.mu.Unlock()
		return false
	}

	previous := r.clusterKeysByRoot[state.rootID]
	r.clusterKeysByRoot[state.rootID] = seen

	var contextNames []string

	for clusterKey := range previous {
		if _, ok := seen[clusterKey]; ok {
			continue
		}

		contextNames = append(contextNames, r.pruneClusterContextLocked(clusterKey)...)
	}
	r.mu.Unlock()

	r.removeContexts(contextNames)

	return true
}

func (r *Runner) recordRootCluster(state *rootState, clusterKey string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.roots[state.rootID] != state {
		return false
	}

	if r.clusterKeysByRoot[state.rootID] == nil {
		r.clusterKeysByRoot[state.rootID] = map[string]struct{}{}
	}

	r.clusterKeysByRoot[state.rootID][clusterKey] = struct{}{}

	return true
}

func (r *Runner) recordSyncedCluster(state *rootState, clusterKey string, contextName string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.roots[state.rootID] != state {
		return
	}

	r.clusters[clusterKey] = clusterState{contextName: contextName}
}

func (r *Runner) clusterMatchesSelector(cluster *Cluster) bool {
	return r.labelSelector == nil || r.labelSelector.Matches(labels.Set(cluster.Labels))
}

func (r *Runner) clusterFromObject(state *rootState, obj interface{}) (*Cluster, bool) {
	unstructuredCluster, ok := clusterUnstructuredFromObject(obj)
	if !ok {
		logger.Log(logger.LevelWarn, map[string]string{"root": state.rootID}, nil,
			"cluster-api: ignored non-Cluster informer event")

		return nil, false
	}

	cluster, err := ClusterFromUnstructured(state.rootID, unstructuredCluster)
	if err != nil {
		logger.Log(logger.LevelWarn, map[string]string{"root": state.rootID}, err,
			"cluster-api: failed to read Cluster")

		return nil, false
	}

	return cluster, true
}

func (r *Runner) pruneCluster(state *rootState, clusterKey string) {
	r.mu.Lock()

	if r.roots[state.rootID] != state {
		r.mu.Unlock()
		return
	}

	delete(r.clusterKeysByRoot[state.rootID], clusterKey)
	contextNames := r.pruneClusterContextLocked(clusterKey)
	r.mu.Unlock()

	r.removeContexts(contextNames)
}

func (r *Runner) pruneClusterContext(state *rootState, clusterKey string) {
	r.mu.Lock()

	if r.roots[state.rootID] != state {
		r.mu.Unlock()
		return
	}

	contextNames := r.pruneClusterContextLocked(clusterKey)
	r.mu.Unlock()

	r.removeContexts(contextNames)
}

func (r *Runner) stopMissingRoots(presentRoots map[string]struct{}, storeRootsLoaded bool) {
	r.mu.Lock()

	cancels := make([]context.CancelFunc, 0, len(r.roots))

	var contextNames []string

	for rootID, state := range r.roots {
		if _, ok := presentRoots[rootID]; ok {
			continue
		}

		if rootID != inClusterRootID && (!storeRootsLoaded || !strings.HasPrefix(rootID, storeRootPrefix)) {
			continue
		}

		cancels = append(cancels, state.cancel)

		delete(r.roots, rootID)
		contextNames = append(contextNames, r.pruneRootLocked(rootID)...)
	}

	r.mu.Unlock()

	r.removeContexts(contextNames)

	for _, cancel := range cancels {
		cancel()
	}
}

func (r *Runner) stopRoot(rootID string, prune bool) {
	var (
		cancel       context.CancelFunc
		contextNames []string
	)

	r.mu.Lock()
	if state := r.roots[rootID]; state != nil {
		cancel = state.cancel

		delete(r.roots, rootID)
	}

	if prune {
		contextNames = r.pruneRootLocked(rootID)
	}
	r.mu.Unlock()

	r.removeContexts(contextNames)

	if cancel != nil {
		cancel()
	}
}

func (r *Runner) stopAllRoots() {
	r.mu.Lock()

	cancels := make([]context.CancelFunc, 0, len(r.roots))
	for rootID, state := range r.roots {
		cancels = append(cancels, state.cancel)

		delete(r.roots, rootID)
	}
	r.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
}

func (r *Runner) pruneRootLocked(rootID string) []string {
	contextNames := make([]string, 0, len(r.clusterKeysByRoot[rootID]))

	for clusterKey := range r.clusterKeysByRoot[rootID] {
		contextNames = append(contextNames, r.pruneClusterContextLocked(clusterKey)...)
	}

	delete(r.clusterKeysByRoot, rootID)

	return contextNames
}

func (r *Runner) pruneClusterContextLocked(clusterKey string) []string {
	state, ok := r.clusters[clusterKey]
	if !ok {
		return nil
	}

	delete(r.clusters, clusterKey)

	return []string{state.contextName}
}

func (r *Runner) removeContexts(contextNames []string) {
	for _, contextName := range contextNames {
		if err := r.store.RemoveContext(contextName); err != nil {
			logger.Log(logger.LevelWarn, map[string]string{"context": contextName}, err,
				"cluster-api: failed to prune context")
		}
	}
}

func (r *Runner) hasNoCRD(serverURL string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	expiresAt, ok := r.noCRD[serverURL]
	if !ok {
		return false
	}

	if !r.now().Before(expiresAt) {
		delete(r.noCRD, serverURL)

		return false
	}

	return true
}

func (r *Runner) markRootNoCRD(state *rootState) {
	var (
		cancel       context.CancelFunc
		contextNames []string
	)

	r.mu.Lock()
	if r.roots[state.rootID] == state {
		r.noCRD[state.serverURL] = r.now().Add(r.noCRDCacheTTL)
		cancel = state.cancel
		delete(r.roots, state.rootID)
		contextNames = r.pruneRootLocked(state.rootID)
	}
	r.mu.Unlock()

	r.removeContexts(contextNames)

	if cancel != nil {
		cancel()
	}
}

func (r *Runner) isCurrentRoot(state *rootState) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.roots[state.rootID] == state
}

func normalizeLabelSelector(selector string) (labels.Selector, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return nil, nil
	}

	parsed, err := labels.Parse(selector)
	if err != nil {
		return nil, fmt.Errorf("invalid cluster-api-label-selector: %w", err)
	}

	return parsed, nil
}

func clusterUnstructuredFromObject(obj interface{}) (*unstructured.Unstructured, bool) {
	switch typed := obj.(type) {
	case *unstructured.Unstructured:
		return typed, true
	case cache.DeletedFinalStateUnknown:
		return clusterUnstructuredFromObject(typed.Obj)
	case *cache.DeletedFinalStateUnknown:
		return clusterUnstructuredFromObject(typed.Obj)
	default:
		return nil, false
	}
}

func secretFromObject(obj interface{}) (*corev1.Secret, bool) {
	switch typed := obj.(type) {
	case *corev1.Secret:
		return typed, true
	case cache.DeletedFinalStateUnknown:
		return secretFromObject(typed.Obj)
	case *cache.DeletedFinalStateUnknown:
		return secretFromObject(typed.Obj)
	default:
		return nil, false
	}
}

func clusterFromCache(state *rootState, namespace, name string) (*unstructured.Unstructured, bool) {
	key := namespace + "/" + name

	obj, exists, err := state.clusterInformer.GetStore().GetByKey(key)
	if err != nil || !exists {
		return nil, false
	}

	return clusterUnstructuredFromObject(obj)
}

func secretFromCache(state *rootState, namespace, name string) (*corev1.Secret, bool) {
	key := namespace + "/" + name

	obj, exists, err := state.secretInformer.GetStore().GetByKey(key)
	if err != nil || !exists {
		return nil, false
	}

	secret, ok := secretFromObject(obj)
	if !ok {
		return nil, false
	}

	return secret.DeepCopy(), true
}

func isClusterAPIKubeconfigSecret(secret *corev1.Secret) bool {
	return secret.Type == SecretType && strings.HasSuffix(secret.Name, kubeconfigSuffix)
}

func clusterNameFromSecret(secretName string) (string, bool) {
	if !strings.HasSuffix(secretName, kubeconfigSuffix) {
		return "", false
	}

	clusterName := strings.TrimSuffix(secretName, kubeconfigSuffix)

	return clusterName, clusterName != ""
}

func normalizeServerURL(host string) string {
	parsed, err := url.Parse(host)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return strings.TrimRight(host, "/")
	}

	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawQuery = ""
	parsed.Fragment = ""

	return strings.TrimRight(parsed.String(), "/")
}

func restConfigFingerprint(config *rest.Config) string {
	fingerprintHash := sha256.New()

	writeRestConfigFingerprint(fingerprintHash, config)
	writeTLSConfigFingerprint(fingerprintHash, config)
	writeImpersonateFingerprint(fingerprintHash, config)
	writeExecFingerprint(fingerprintHash, config.ExecProvider)

	return hex.EncodeToString(fingerprintHash.Sum(nil))
}

func writeRestConfigFingerprint(fingerprintHash hash.Hash, config *rest.Config) {
	writeHashString(fingerprintHash, config.Host)
	writeHashString(fingerprintHash, config.APIPath)
	writeHashString(fingerprintHash, config.Username)
	writeHashString(fingerprintHash, config.Password)
	writeHashString(fingerprintHash, config.BearerToken)
	writeHashString(fingerprintHash, config.BearerTokenFile)
}

func writeTLSConfigFingerprint(fingerprintHash hash.Hash, config *rest.Config) {
	writeHashString(fingerprintHash, config.ServerName)
	writeHashString(fingerprintHash, config.CAFile)
	writeHashString(fingerprintHash, config.CertFile)
	writeHashString(fingerprintHash, config.KeyFile)
	writeHashString(fingerprintHash, fmt.Sprintf("%t", config.Insecure))
	writeHashBytes(fingerprintHash, config.CAData)
	writeHashBytes(fingerprintHash, config.CertData)
	writeHashBytes(fingerprintHash, config.KeyData)
}

func writeImpersonateFingerprint(fingerprintHash hash.Hash, config *rest.Config) {
	writeHashString(fingerprintHash, config.Impersonate.UserName)

	for _, group := range config.Impersonate.Groups {
		writeHashString(fingerprintHash, group)
	}

	extraKeys := make([]string, 0, len(config.Impersonate.Extra))
	for key := range config.Impersonate.Extra {
		extraKeys = append(extraKeys, key)
	}

	sort.Strings(extraKeys)

	for _, key := range extraKeys {
		writeHashString(fingerprintHash, key)

		for _, value := range config.Impersonate.Extra[key] {
			writeHashString(fingerprintHash, value)
		}
	}
}

func writeExecFingerprint(fingerprintHash hash.Hash, execProvider *clientcmdapi.ExecConfig) {
	if execProvider == nil {
		return
	}

	writeHashString(fingerprintHash, execProvider.APIVersion)
	writeHashString(fingerprintHash, execProvider.Command)
	writeHashString(fingerprintHash, execProvider.InstallHint)
	writeHashString(fingerprintHash, fmt.Sprintf("%t", execProvider.ProvideClusterInfo))

	for _, arg := range execProvider.Args {
		writeHashString(fingerprintHash, arg)
	}

	for _, env := range execProvider.Env {
		writeHashString(fingerprintHash, env.Name)
		writeHashString(fingerprintHash, env.Value)
	}

	writeExecConfigFingerprint(fingerprintHash, execProvider.Config)
}

func writeExecConfigFingerprint(fingerprintHash hash.Hash, config k8sruntime.Object) {
	if config == nil {
		return
	}

	writeHashString(fingerprintHash, fmt.Sprintf("%T", config))

	configJSON, err := json.Marshal(config)
	if err != nil {
		writeHashString(fingerprintHash, fmt.Sprintf("%#v", config))

		return
	}

	writeHashBytes(fingerprintHash, configJSON)
}

func writeHashString(fingerprintHash hash.Hash, value string) {
	_, _ = fingerprintHash.Write([]byte(value))
	_, _ = fingerprintHash.Write([]byte{0})
}

func writeHashBytes(fingerprintHash hash.Hash, value []byte) {
	_, _ = fingerprintHash.Write(value)
	_, _ = fingerprintHash.Write([]byte{0})
}

func isNoCRDError(err error) bool {
	if err == nil {
		return false
	}

	if meta.IsNoMatchError(err) {
		return true
	}

	if apierrors.IsNotFound(err) {
		return isClusterNotFound(err)
	}

	message := err.Error()

	return strings.Contains(message, "no matches for kind") &&
		strings.Contains(message, "Cluster") &&
		strings.Contains(message, ClusterGVR.Group)
}

func isClusterNotFound(err error) bool {
	statusErr := &apierrors.StatusError{}
	if errors.As(err, &statusErr) && statusDetailsMatchClusters(statusErr.ErrStatus.Details) {
		return true
	}

	message := err.Error()

	return strings.Contains(message, "clusters") &&
		(strings.Contains(message, "cluster.x-k8s.io") || strings.Contains(message, "Cluster"))
}

func statusDetailsMatchClusters(details *metav1.StatusDetails) bool {
	if details == nil || details.Group != ClusterGVR.Group {
		return false
	}

	return details.Kind == "Cluster" || details.Kind == "clusters" || details.Name == "clusters"
}
