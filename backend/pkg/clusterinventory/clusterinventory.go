/*
Copyright 2025 The Kubernetes Authors.

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

package clusterinventory

import (
	"context"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd/api"

	"github.com/kubernetes-sigs/headlamp/backend/pkg/kubeconfig"
	"github.com/kubernetes-sigs/headlamp/backend/pkg/logger"
	apisv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	ciaclient "sigs.k8s.io/cluster-inventory-api/client/clientset/versioned"
	ciascredentials "sigs.k8s.io/cluster-inventory-api/pkg/credentials"
)

const clusterInventoryPrefix = "cluster-inventory-"

// WatchAndSync loads the ClusterProfile access-provider config, lists and watches
// ClusterProfile resources across all namespaces, and syncs them into the given
// context store as Headlamp contexts. It runs until the context is cancelled.
// When running in-cluster, the in-cluster rest.Config is used to talk to the hub
// (same cluster) where ClusterProfiles are stored. Each ClusterProfile is turned
// into a context using the configured exec plugins (e.g. kubeconfig-secretreader) for auth.
func WatchAndSync(ctx context.Context, store kubeconfig.ContextStore, providerFile string) {
	if providerFile == "" {
		logger.Log(logger.LevelWarn, nil, nil, "cluster-inventory: provider file path is empty; skipping ClusterProfile sync")
		return
	}

	credProvider, err := ciascredentials.NewFromFile(providerFile)
	if err != nil {
		logger.Log(logger.LevelError, map[string]string{"path": providerFile}, err, "cluster-inventory: failed to load provider file")
		return
	}

	hubConfig, err := rest.InClusterConfig()
	if err != nil {
		logger.Log(logger.LevelError, nil, err, "cluster-inventory: failed to get in-cluster config")
		return
	}

	cic, err := ciaclient.NewForConfig(hubConfig)
	if err != nil {
		logger.Log(logger.LevelError, nil, err, "cluster-inventory: failed to create ClusterProfile client")
		return
	}

	cpInterface := cic.ApisV1alpha1().ClusterProfiles(metav1.NamespaceAll)

	// Track context names we added so we can remove them on delete.
	mu := sync.Mutex{}
	contextNamesByKey := make(map[string]string) // key = namespace/name

	syncOne := func(cp *apisv1alpha1.ClusterProfile) {
		key := cp.Namespace + "/" + cp.Name
		ctxName := contextNameForClusterProfile(cp)

		spokeConfig, err := credProvider.BuildConfigFromCP(cp)
		if err != nil {
			logger.Log(logger.LevelWarn, map[string]string{"clusterprofile": key}, err, "cluster-inventory: failed to build config for ClusterProfile")
			return
		}

		headlampContext, err := restConfigToContext(spokeConfig, ctxName, key)
		if err != nil {
			logger.Log(logger.LevelWarn, map[string]string{"clusterprofile": key}, err, "cluster-inventory: failed to build context")
			return
		}

		if err := headlampContext.SetupProxy(); err != nil {
			logger.Log(logger.LevelWarn, map[string]string{"clusterprofile": key}, err, "cluster-inventory: failed to setup proxy")
			return
		}

		if err := store.AddContext(headlampContext); err != nil {
			logger.Log(logger.LevelWarn, map[string]string{"clusterprofile": key}, err, "cluster-inventory: failed to add context")
			return
		}

		mu.Lock()
		contextNamesByKey[key] = ctxName
		mu.Unlock()
	}

	removeOne := func(key string) {
		mu.Lock()
		ctxName, ok := contextNamesByKey[key]
		if ok {
			delete(contextNamesByKey, key)
		}
		mu.Unlock()
		if !ok {
			return
		}
		if err := store.RemoveContext(ctxName); err != nil {
			logger.Log(logger.LevelWarn, map[string]string{"context": ctxName}, err, "cluster-inventory: failed to remove context")
		}
	}

	// Initial list and sync
	list, err := cpInterface.List(ctx, metav1.ListOptions{})
	if err != nil {
		logger.Log(logger.LevelError, nil, err, "cluster-inventory: failed to list ClusterProfiles")
		return
	}
	for i := range list.Items {
		syncOne(&list.Items[i])
	}

	// Watch for changes
	watcher, err := cpInterface.Watch(ctx, metav1.ListOptions{})
	if err != nil {
		logger.Log(logger.LevelError, nil, err, "cluster-inventory: failed to watch ClusterProfiles")
		return
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.ResultChan():
			if !ok {
				logger.Log(logger.LevelWarn, nil, nil, "cluster-inventory: watch channel closed")
				return
			}
			switch event.Type {
			case watch.Added, watch.Modified:
				if cp, ok := event.Object.(*apisv1alpha1.ClusterProfile); ok {
					syncOne(cp)
				}
			case watch.Deleted:
				if cp, ok := event.Object.(*apisv1alpha1.ClusterProfile); ok {
					removeOne(cp.Namespace + "/" + cp.Name)
				}
			}
		}
	}
}

func contextNameForClusterProfile(cp *apisv1alpha1.ClusterProfile) string {
	name := clusterInventoryPrefix + cp.Namespace + "-" + cp.Name
	return makeDNSFriendly(name)
}

func makeDNSFriendly(name string) string {
	name = strings.ReplaceAll(name, "/", "--")
	name = strings.ReplaceAll(name, " ", "__")
	return name
}

// restConfigToContext builds a Headlamp kubeconfig.Context from a rest.Config
// produced by cluster-inventory-api credentials.BuildConfigFromCP (exec plugin).
func restConfigToContext(restConfig *rest.Config, contextName, clusterID string) (*kubeconfig.Context, error) {
	cluster := &api.Cluster{
		Server:                   restConfig.Host,
		CertificateAuthorityData: restConfig.TLSClientConfig.CAData,
		InsecureSkipTLSVerify:    restConfig.TLSClientConfig.Insecure,
	}
	if restConfig.TLSClientConfig.CAFile != "" {
		cluster.CertificateAuthority = restConfig.TLSClientConfig.CAFile
	}

	authInfo := &api.AuthInfo{}
	if restConfig.ExecProvider != nil {
		authInfo.Exec = &api.ExecConfig{
			APIVersion:         restConfig.ExecProvider.APIVersion,
			Command:            restConfig.ExecProvider.Command,
			Args:               restConfig.ExecProvider.Args,
			Env:                restConfig.ExecProvider.Env,
			InteractiveMode:    api.NeverExecInteractiveMode,
			ProvideClusterInfo: restConfig.ExecProvider.ProvideClusterInfo,
			Config:             restConfig.ExecProvider.Config,
		}
	} else if restConfig.BearerToken != "" {
		authInfo.Token = restConfig.BearerToken
	}

	kubeContext := &api.Context{
		Cluster:  contextName,
		AuthInfo: contextName,
	}

	return &kubeconfig.Context{
		Name:           contextName,
		KubeContext:    kubeContext,
		Cluster:        cluster,
		AuthInfo:       authInfo,
		Source:         kubeconfig.ClusterInventory,
		KubeConfigPath: "",
		ClusterID:      "cluster-inventory/" + clusterID,
	}, nil
}
