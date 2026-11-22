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

package kubeconfig

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"time"

	"github.com/kubernetes-sigs/headlamp/backend/pkg/cache"
	"github.com/kubernetes-sigs/headlamp/backend/pkg/logger"
)

// ContextChangeListener is a function that is called when contexts change.
type ContextChangeListener func()

// ContextStore is an interface for storing and retrieving contexts.
type ContextStore interface {
	AddContext(headlampContext *Context) error
	GetContexts() ([]*Context, error)
	GetContext(name string) (*Context, error)
	RemoveContext(name string) error
	AddContextWithKeyAndTTL(headlampContext *Context, key string, ttl time.Duration) error
	UpdateTTL(key string, ttl time.Duration) error
	RetainContext(key string, expected *Context, idleTTL time.Duration) (context.Context, func(), error)
	AddListener(listener ContextChangeListener)
	AddContextKeyListener(listener func(string))
	GetContextKeys() ([]string, error)
}

type contextLease struct {
	context.Context
	cancel context.CancelFunc
	users  int
}

type contextStore struct {
	leaseMu   sync.Mutex
	leases    map[string]*contextLease
	cache     cache.Cache[*Context]
	listeners []func(string)
	mu        sync.RWMutex
}

// NewContextStore creates a new ContextStore.
func NewContextStore() ContextStore {
	c := cache.New[*Context]()

	cs := &contextStore{
		cache:  c,
		leases: make(map[string]*contextLease),
	}

	c.SetOnEvicted(func(key string, value *Context) {
		cs.notifyListeners(key)
	})

	return cs
}

func (c *contextStore) AddListener(listener ContextChangeListener) {
	if listener != nil {
		c.AddContextKeyListener(func(string) { listener() })
	}
}

// AddContextKeyListener reports the storage key that was added, replaced, removed or expired.
func (c *contextStore) AddContextKeyListener(listener func(string)) {
	if listener == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.listeners = append(c.listeners, listener)
}

func (c *contextStore) notifyListeners(key string) {
	c.mu.RLock()
	listeners := make([]func(string), len(c.listeners))
	copy(listeners, c.listeners)
	c.mu.RUnlock()

	for _, listener := range listeners {
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Log(logger.LevelError, nil, r, "contextStore: ContextChangeListener panicked")
				}
			}()

			listener(key)
		}()
	}
}

// AddContext adds a context to the store.
func (c *contextStore) AddContext(headlampContext *Context) error {
	name := headlampContext.Name

	if headlampContext.KubeContext != nil && headlampContext.KubeContext.Extensions["headlamp_info"] != nil {
		info := headlampContext.KubeContext.Extensions["headlamp_info"]
		// Convert the runtime.Unknown object to a byte slice
		unknownBytes, err := json.Marshal(info)
		if err != nil {
			return err
		}

		// Now, decode the byte slice into your desired struct
		var customObj CustomObject

		err = json.Unmarshal(unknownBytes, &customObj)
		if err != nil {
			return err
		}

		// If the custom name is set, use it as the context name
		if customObj.CustomName != "" {
			name = customObj.CustomName
		}
	}

	// Keep the stored context identifier consistent with the cache key.
	headlampContext.Name = name

	return c.setContext(headlampContext, name, 0)
}

// GetContexts returns all contexts in the store.
func (c *contextStore) GetContexts() ([]*Context, error) {
	contexts := []*Context{}

	contextMap, err := c.cache.GetAll(context.Background(), nil)
	if err != nil {
		return nil, err
	}

	keys := make([]string, 0, len(contextMap))
	for key := range contextMap {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	for _, key := range keys {
		contexts = append(contexts, contextMap[key])
	}

	return contexts, nil
}

// GetContextKeys returns all context keys in the store.
func (c *contextStore) GetContextKeys() ([]string, error) {
	var keys []string

	contextMap, err := c.cache.GetAll(context.Background(), nil)
	if err != nil {
		return nil, err
	}

	for key := range contextMap {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	return keys, nil
}

// GetContext returns a context from the store.
func (c *contextStore) GetContext(name string) (*Context, error) {
	context, err := c.cache.Get(context.Background(), name)
	if err != nil {
		return nil, err
	}

	return context, nil
}

// RemoveContext removes a context from the store.
func (c *contextStore) RemoveContext(name string) error {
	c.leaseMu.Lock()
	err := c.cache.Delete(context.Background(), name)
	c.cancelLease(name)
	c.leaseMu.Unlock()

	if err == nil {
		c.notifyListeners(name)
	}

	return err
}

// AddContextWithKeyAndTTL adds a context to the store with a ttl.
func (c *contextStore) AddContextWithKeyAndTTL(headlampContext *Context, key string, ttl time.Duration) error {
	// Keep the stored context identifier consistent with the cache key.
	headlampContext.Name = key

	return c.setContext(headlampContext, key, ttl)
}

func (c *contextStore) setContext(value *Context, key string, ttl time.Duration) error {
	c.leaseMu.Lock()

	retained := c.leases[key] != nil
	if retained {
		previous, _ := c.GetContext(key)

		retained = sameRetainedConnection(previous, value)
		if retained {
			ttl = 0
		}
	}

	err := c.cache.SetWithTTL(context.Background(), key, value, ttl)
	if err == nil && !retained {
		// Waiters awakened by cancellation must see the replacement on their next lookup.
		c.cancelLease(key)
	}
	c.leaseMu.Unlock()

	if err == nil {
		c.notifyListeners(key)
	}

	return err
}

func sameRetainedConnection(first, second *Context) bool {
	revision := first.RoutingRevision()

	return first == second || (revision != "" && revision == second.RoutingRevision())
}

// ImportedContextIdleTTL is the cache lifetime after the last active use.
const ImportedContextIdleTTL = 20 * time.Second

func (c *contextStore) cancelLease(key string) {
	if lease := c.leases[key]; lease != nil {
		delete(c.leases, key)
		lease.cancel()
	}
}

// UpdateTTL updates the ttl of an unretained context.
func (c *contextStore) UpdateTTL(key string, ttl time.Duration) error {
	c.leaseMu.Lock()
	defer c.leaseMu.Unlock()

	if c.leases[key] != nil {
		return nil
	}

	return c.cache.UpdateTTL(context.Background(), key, ttl)
}

// RetainContext suspends cache expiry until the last user releases the lease.
// Removing or replacing the context cancels every lease for that entry.
func (c *contextStore) RetainContext(
	key string, expected *Context, idleTTL time.Duration,
) (context.Context, func(), error) {
	c.leaseMu.Lock()
	defer c.leaseMu.Unlock()

	current, err := c.GetContext(key)
	if err != nil || !sameRetainedConnection(current, expected) {
		return nil, nil, cache.ErrNotFound
	}

	lease := c.leases[key]
	if lease == nil {
		if err := c.cache.UpdateTTL(context.Background(), key, 0); err != nil {
			return nil, nil, err
		}

		ctx, cancel := context.WithCancel(context.Background())
		lease = &contextLease{Context: ctx, cancel: cancel}
		c.leases[key] = lease
	}

	lease.users++

	return lease.Context, sync.OnceFunc(func() {
		c.leaseMu.Lock()
		defer c.leaseMu.Unlock()

		if c.leases[key] != lease {
			return
		}

		lease.users--
		if lease.users == 0 {
			c.cancelLease(key)
			_ = c.cache.UpdateTTL(context.Background(), key, idleTTL)
		}
	}), nil
}
