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

package portforward

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/kubernetes-sigs/headlamp/backend/pkg/cache"
	"github.com/kubernetes-sigs/headlamp/backend/pkg/kubeconfig"
	"github.com/kubernetes-sigs/headlamp/backend/pkg/logger"
)

const (
	storeKeyPrefix = "PORT_FORWARD_"
	// storeKeySeparator separates the cluster key from the rest of a store key,
	// so one cluster's prefix never matches another cluster whose name starts
	// with the same text (e.g. "prod" and "prod-eu").
	storeKeySeparator = "\x00"
)

// storeKeyClusterPrefix returns the store key prefix for a cluster key.
// The cluster key is escaped so it never contains storeKeySeparator: stateless
// context keys are "<cluster>\x00<userID>", so without escaping, a base
// cluster's prefix would also match its users' port forwards.
func storeKeyClusterPrefix(clusterKey string) string {
	return storeKeyPrefix + url.QueryEscape(clusterKey) + storeKeySeparator
}

type forwardConnection struct {
	cache           cache.Cache[interface{}]
	contextStoreKey string
}

var (
	forwardStoreMu     sync.Mutex
	connectionForwards = make(map[forwardConnection]map[string]portForward)
)

// portforwardKeyGenerator generates a unique key
// based on the cluster name, id,service name, and pod name.
func portforwardKeyGenerator(p portForward) string {
	clusterKey := p.contextKey()
	if p.ID != "" {
		return storeKeyClusterPrefix(clusterKey) + p.ID
	}

	key := storeKeyClusterPrefix(clusterKey)

	if p.Service != "" {
		key += p.Service
	} else if p.Pod != "" {
		key += p.Pod
	}

	return key
}

func (p portForward) contextKey() string {
	if p.cacheKey != "" {
		return p.cacheKey
	}

	return p.Cluster
}

// portforwardstore stores a port forward in the cache.
func portforwardstore(forwardCache cache.Cache[interface{}], p portForward) {
	storeForwardSnapshot(forwardCache, p, false)
}

func storeForwardSnapshot(forwardCache cache.Cache[interface{}], p portForward, starting bool) bool {
	forwardStoreMu.Lock()
	defer forwardStoreMu.Unlock()

	if session := p.session; session != nil {
		if session.removed || (session.Err() != nil && p.Status != STOPPED) {
			return false
		}

		if session.connectionCurrent != nil && !session.connectionCurrent() {
			session.cancel()
			return false
		}
	}

	key := portforwardKeyGenerator(p)

	current, getErr := forwardCache.Get(context.Background(), key)

	saved, exists := current.(portForward)
	if p.session != nil && !starting && (getErr != nil || !exists || saved.session != p.session) {
		return false
	}

	if getErr == nil && exists && saved.session != p.session && (!starting || saved.Status != STOPPED) {
		return false
	}

	err := forwardCache.Set(context.Background(), key, p)
	if err != nil {
		logger.Log(logger.LevelError, nil, err, "storing portforward")
		return false
	}

	if p.session == nil || p.session.connectionCurrent == nil {
		untrackConnectionForward(forwardCache, key, p.session)
		return true
	}

	group := forwardConnection{forwardCache, p.session.contextStoreKey}
	if connectionForwards[group] == nil {
		connectionForwards[group] = make(map[string]portForward)
	}

	connectionForwards[group][key] = p

	return true
}

func untrackConnectionForward(forwardCache cache.Cache[interface{}], key string, session *forwardSession) {
	if session == nil {
		return
	}

	group := forwardConnection{forwardCache, session.contextStoreKey}
	if forward := connectionForwards[group][key]; forward.session == session {
		delete(connectionForwards[group], key)

		if len(connectionForwards[group]) == 0 {
			delete(connectionForwards, group)
		}
	}
}

func connectionForwardsSnapshot(group forwardConnection) []portForward {
	forwardStoreMu.Lock()
	defer forwardStoreMu.Unlock()

	forwards := make([]portForward, 0, len(connectionForwards[group]))
	for _, forward := range connectionForwards[group] {
		forwards = append(forwards, forward)
	}

	return forwards
}

// Publish the session before the listener starts so a concurrent removal
// can cancel both a running and a starting forward.
func publishStartingForward(forwardCache cache.Cache[interface{}], forward *portForward) bool {
	snapshot := *forward

	snapshot.Status = RECONNECTING
	if !storeForwardSnapshot(forwardCache, snapshot, true) {
		return false
	}

	if forward.session.connectionCurrent == nil || forward.session.connectionCurrent() {
		return true
	}

	_ = deleteForward(forwardCache, snapshot)

	return false
}

// WatchConnections closes forwarders whose selected connection is no longer present.
func WatchConnections(store kubeconfig.ContextStore, forwardCache cache.Cache[interface{}]) {
	store.AddContextKeyListener(func(key string) {
		for _, forward := range connectionForwardsSnapshot(forwardConnection{forwardCache, key}) {
			if !forward.session.connectionCurrent() {
				_ = deleteForward(forwardCache, forward)
			}
		}
	})
}

func stopForward(forward portForward) {
	if forward.session != nil {
		forward.session.cancel()
	}

	safeCloseChan(forward.closeChan)
}

func deleteForward(forwardCache cache.Cache[interface{}], forward portForward) error {
	forwardStoreMu.Lock()
	defer forwardStoreMu.Unlock()

	if session := forward.session; session != nil {
		session.removed = true
	}

	stopForward(forward)

	key := portforwardKeyGenerator(forward)
	untrackConnectionForward(forwardCache, key, forward.session)

	if current, err := forwardCache.Get(context.Background(), key); err == nil {
		if entry, ok := current.(portForward); ok && entry.session != forward.session {
			return nil
		}
	}

	return forwardCache.Delete(context.Background(), key)
}

// stopOrDeletePortForward stops or deletes a port forward by its cluster and id.
// It takes three parameters: cluster is the name of the cluster, id is the unique identifier of the port forward,
// isStopRequest is a boolean value indicating whether to stop or delete the port forward.
// It returns an error value indicating whether the operation is successful or not.
func stopOrDeletePortForward(cache cache.Cache[interface{}], cluster string, id string, isStopRequest bool) error {
	portforward, err := getPortForwardByID(cache, cluster, id)
	if err != nil {
		logger.Log(logger.LevelError, map[string]string{"cluster": cluster, "id": id},
			err, "getting portforward")

		return err
	}

	if !isStopRequest {
		return deleteForward(cache, portforward)
	}

	stopForward(portforward)
	portforward.Status = STOPPED
	portforwardstore(cache, portforward)

	return nil
}

// getPortForwardList returns a list of port forwards by its cluster name.
func getPortForwardList(cache cache.Cache[interface{}], cluster string) []portForward {
	portforwards, err := cache.GetAll(context.Background(), func(key string) bool {
		return strings.HasPrefix(key, storeKeyClusterPrefix(cluster))
	})
	if err != nil {
		logger.Log(logger.LevelError, map[string]string{"cluster": cluster},
			err, "getting portforward list")

		return nil
	}

	portForwards := []portForward{}

	for _, v := range portforwards {
		if forward, ok := v.(portForward); ok && forward.contextKey() == cluster {
			portForwards = append(portForwards, forward)
		}
	}

	return portForwards
}

// getPortForwardByID returns a port forward by its cluster name and id.
func getPortForwardByID(cache cache.Cache[interface{}], cluster string, id string) (portForward, error) {
	cacheValue, err := cache.Get(context.Background(), storeKeyClusterPrefix(cluster)+id)
	if err != nil {
		return portForward{}, fmt.Errorf("failed to get portforward from cache: %w", err)
	}

	pf, ok := cacheValue.(portForward)
	if !ok {
		return portForward{}, fmt.Errorf("failed to convert cache value to portforward")
	}

	return pf, nil
}
