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

package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/gorilla/mux"
	"github.com/kubernetes-sigs/headlamp/backend/pkg/auth"
	"github.com/kubernetes-sigs/headlamp/backend/pkg/cache"
	"github.com/kubernetes-sigs/headlamp/backend/pkg/k8cache"
	"github.com/kubernetes-sigs/headlamp/backend/pkg/kubeconfig"
	"github.com/kubernetes-sigs/headlamp/backend/pkg/logger"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	importRevisionKeyHeader = "X-Headlamp-Import-Key"
	importedRevisionPrefix  = "import-"
	importedDigestLength    = 32
)

// The browser persists this private key alongside its imports. Replicas can verify
// the same revision without publishing a digest of the kubeconfig credentials.
func importedConnectionRevision(connection *kubeconfig.Context, key string) (string, error) {
	if key == "" {
		return "", nil
	} // Unversioned clients predate import revisions.

	secret, err := hex.DecodeString(key)
	if err != nil || len(secret) != sha256.Size {
		return "", errors.New("invalid import revision key")
	}

	digest, err := k8cache.ConnectionDigest(connection)
	if err != nil {
		return "", err
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(digest))

	return importedRevisionPrefix + hex.EncodeToString(mac.Sum(nil))[:importedDigestLength], nil
}

func validateImportedRevision(userID, key string, connection *kubeconfig.Context) error {
	expected := importedUserRevision(userID)
	if expected == "" {
		return nil
	}

	expected = expected[:len(importedRevisionPrefix)+importedDigestLength]

	current, err := importedConnectionRevision(connection, key)
	if err != nil {
		return err
	}

	if expected != current {
		return errClusterConnectionChanged
	}

	return nil
}

func importedUserRevision(userID string) string {
	_, revision, found := strings.Cut(userID, "--"+importedRevisionPrefix)

	digest, incarnation, scoped := strings.Cut(revision, "-")
	if !found || len(digest) != importedDigestLength {
		return ""
	}

	if _, err := hex.DecodeString(digest); err != nil {
		return ""
	}

	if scoped && (incarnation == "" || len(incarnation) > 48 ||
		strings.ContainsFunc(incarnation, func(r rune) bool {
			return (r < '0' || r > '9') && (r < 'a' || r > 'f') && r != '-'
		})) {
		return ""
	}

	return importedRevisionPrefix + revision
}

const (
	statelessContextKeySep = "\x00"
	retiredImportPrefix    = "stateless-retired\x00"
)

func statelessContextKey(clusterName, userID string) string {
	if userID == "" {
		return clusterName
	}

	return clusterName + statelessContextKeySep + userID
}

// MarshalCustomObject marshals the runtime.Unknown object into a CustomObject.
func MarshalCustomObject(info runtime.Object, contextName string) (kubeconfig.CustomObject, error) {
	// Convert the runtime.Unknown object to a byte slice
	unknownBytes, err := json.Marshal(info)
	if err != nil {
		logger.Log(logger.LevelError, map[string]string{logFieldCluster: contextName},
			err, "unmarshaling context data")

		return kubeconfig.CustomObject{}, err
	}

	// Now, decode the byte slice into CustomObject
	var customObj kubeconfig.CustomObject

	err = json.Unmarshal(unknownBytes, &customObj)
	if err != nil {
		logger.Log(logger.LevelError, map[string]string{logFieldCluster: contextName},
			err, "unmarshaling into CustomObject")

		return kubeconfig.CustomObject{}, err
	}

	return customObj, nil
}

// setKeyInCache sets the context in the cache with the given key.
func (c *HeadlampConfig) setKeyInCache(key string, connection kubeconfig.Context) error {
	// Concurrent requests for the same key must not both see it missing and each add a context,
	// as the later add replaces the first context and discards its already set up proxy.
	c.statelessCacheMu.Lock()
	defer c.statelessCacheMu.Unlock()

	_, userID, _ := strings.Cut(key, statelessContextKeySep)
	revision := importedUserRevision(userID)

	if revision != "" {
		if _, err := c.Cache.Get(context.Background(), retiredImportPrefix+key); err == nil {
			return errClusterConnectionChanged
		}
	}

	// check context is present
	_, err := c.KubeConfigStore.GetContext(key)
	if err != nil && err.Error() == "key not found" {
		// To ensure stateless clusters are not visible to other users, they are marked as internal clusters.
		// They are stored in the proxy cache and accessed through the /config endpoint.
		connection.Internal = true
		connection.ImportRevision = revision

		if err = c.KubeConfigStore.AddContextWithKeyAndTTL(&connection, key, ContextCacheTTL); err != nil {
			logger.Log(logger.LevelError, map[string]string{"key": key},
				err, "adding context to cache")

			return err
		}
	} else {
		if err = c.KubeConfigStore.UpdateTTL(key, ContextUpdateCacheTTL); err != nil {
			logger.Log(logger.LevelError, map[string]string{"key": key},
				err, "updating context ttl")

			return err
		}
	}

	return nil
}

// Handles stateless cluster requests if kubeconfig is set and dynamic clusters are enabled.
// It returns context key which is used to store the context in the cache.
func (c *HeadlampConfig) handleStatelessReq(r *http.Request, kubeConfig string) (string, error) {
	key, context, err := resolveStatelessRequestContext(r, kubeConfig)
	if err != nil {
		return "", err
	}

	if err := c.setKeyInCache(key, *context); err != nil {
		return "", err
	}

	return key, nil
}

func resolveStatelessRequestContext(r *http.Request, kubeConfig string) (string, *kubeconfig.Context, error) {
	userID := r.Header.Get("X-HEADLAMP-USER-ID")

	targetClusterName := mux.Vars(r)["clusterName"]

	contexts, contextLoadErrors, err := kubeconfig.LoadContextsFromBase64String(kubeConfig, kubeconfig.DynamicCluster)
	if len(contextLoadErrors) > 0 {
		for _, contextError := range contextLoadErrors {
			logger.Log(logger.LevelError, nil, contextError.Error, "loading contexts from kubeconfig")
		}

		if err != nil {
			logger.Log(logger.LevelError, nil, err, "loading contexts from kubeconfig")

			return "", nil, err
		}

		if len(contexts) == 0 {
			return "", nil, fmt.Errorf("failed to load any valid contexts from kubeconfig")
		}
	}

	if len(contexts) == 0 {
		logger.Log(logger.LevelError, nil, nil, "no contexts found in kubeconfig")
		return "", nil, fmt.Errorf("no contexts found in kubeconfig")
	}

	return selectStatelessContext(targetClusterName, userID, r.Header.Get(importRevisionKeyHeader), contexts)
}

func selectStatelessContext(targetClusterName, userID, revisionKey string, contexts []kubeconfig.Context,
) (string, *kubeconfig.Context, error) {
	var (
		selected         *kubeconfig.Context
		revisionMismatch bool
	)

	for _, context := range contexts {
		effectiveContextName := context.Name

		info := context.KubeContext.Extensions["headlamp_info"]
		if info != nil {
			customObj, err := MarshalCustomObject(info, context.Name)
			if err != nil {
				logger.Log(logger.LevelError, map[string]string{logFieldCluster: context.Name},
					err, "marshaling custom object")

				return "", nil, err
			}

			if customObj.CustomName != "" {
				effectiveContextName = customObj.CustomName
			}
		}

		if effectiveContextName != targetClusterName {
			continue
		}

		if err := validateImportedRevision(userID, revisionKey, &context); err != nil {
			if errors.Is(err, errClusterConnectionChanged) {
				revisionMismatch = true
				continue
			}

			return "", nil, err
		}

		if selected == nil {
			selected = &context
		}
	}

	if selected != nil {
		return statelessContextKey(targetClusterName, userID), selected, nil
	}

	if revisionMismatch {
		return "", nil, errClusterConnectionChanged
	}

	return "", nil, fmt.Errorf("context %q not found in supplied kubeconfig", targetClusterName)
}

// retireImportedConnection drops the previous browser-owned connection after a
// replacement or deletion. A lease keeps it alive during ordinary cache expiry.
func (c *HeadlampConfig) retireImportedConnection(w http.ResponseWriter, r *http.Request) {
	name, err := auth.DecodeClusterFromURL(mux.Vars(r)["encodedName"])
	userID := r.Header.Get("X-HEADLAMP-USER-ID")

	revision := importedUserRevision(userID)
	if err != nil || name == "" || revision == "" {
		http.Error(w, "invalid imported connection", http.StatusBadRequest)
		return
	}

	key := statelessContextKey(name, userID)
	if err := c.removeImportedConnection(r.Context(), key, revision); err != nil {
		if errors.Is(err, cache.ErrNotFound) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "could not retire imported connection", http.StatusInternalServerError)
		}

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (c *HeadlampConfig) removeImportedConnection(ctx context.Context, key, revision string) error {
	c.statelessCacheMu.Lock()
	defer c.statelessCacheMu.Unlock()

	if revision != "" {
		connection, err := c.KubeConfigStore.GetContext(key)
		if !errors.Is(err, cache.ErrNotFound) &&
			(err != nil || !connection.Internal || connection.ImportRevision != revision) {
			return cache.ErrNotFound
		}

		if err := c.Cache.SetWithTTL(ctx, retiredImportPrefix+key, struct{}{}, ContextCacheTTL); err != nil {
			return err
		}
	}

	return c.KubeConfigStore.RemoveContext(key)
}

type ambiguousImportContext struct {
	Name            string `json:"name"`
	KubeconfigIndex int    `json:"kubeconfigIndex"`
}

func (c *ambiguousImportContext) Error() string {
	return fmt.Sprintf("multiple kubeconfig contexts have the Headlamp name %q", c.Name)
}

// parseKubeConfig parses the kubeconfig and returns a list of contexts and errors.
// Input is a list of base64 encoded kubeconfigs. Output is a list of clusters.
// Input: {"kubeconfigs": ["base64 encoded kubeconfig 1", "base64 encoded kubeconfig 2"]}
// Output: {"clusters": [{"name": "cluster 1", "server": "https://cluster1.server.com",
// "authType": "token"}, {"name": "cluster 2", "server": "https://cluster2.server.com", "authType": "token"}]}.
func (c *HeadlampConfig) parseKubeConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Create a variable to store the decoded JSON data
	var kubeconfigReq KubeconfigRequest

	// Decode the JSON request body into the kubeconfigReq variable
	if err := decodeRequestBody(w, r, &kubeconfigReq); err != nil {
		// Handle the error, return a bad request response
		logger.Log(logger.LevelError, nil, err, "decoding config")

		writeDecodeError(w, err, "Invalid JSON request body")

		return
	}

	if len(kubeconfigReq.Kubeconfigs) == 0 {
		http.Error(w, "kubeconfigs is required", http.StatusBadRequest)

		return
	}

	contexts, setupErrors := parseClusterFromKubeConfig(kubeconfigReq.Kubeconfigs, r.Header.Get(importRevisionKeyHeader))

	var ambiguous []*ambiguousImportContext

	for _, err := range setupErrors {
		var duplicate *ambiguousImportContext
		if errors.As(err, &duplicate) {
			ambiguous = append(ambiguous, duplicate)
		}
	}

	if len(setupErrors) > 0 && len(contexts) == 0 {
		// Only fail the request when NO clusters could be parsed at all.
		// Partial failures (some entries invalid, others valid) still return the valid clusters.
		logger.Log(logger.LevelError, nil, setupErrors, "setting up contexts from kubeconfig")

		if len(ambiguous) != 0 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": ambiguous[0].Error()})
		} else {
			http.Error(w, "setting up contexts from kubeconfig", http.StatusBadRequest)
		}

		return
	}

	result := struct {
		clientConfig
		AmbiguousContexts []*ambiguousImportContext `json:"ambiguousContexts,omitempty"`
	}{clientConfig: clientConfig{
		Clusters:                contexts,
		IsDynamicClusterEnabled: c.EnableDynamicClusters,
		AllowKubeconfigChanges:  c.AllowKubeconfigChanges,
		DefaultLightTheme:       c.DefaultLightTheme,
		DefaultDarkTheme:        c.DefaultDarkTheme,
		ForceTheme:              c.ForceTheme,
		OidcAutoLogin:           c.OidcAutoLogin,
	}, AmbiguousContexts: ambiguous}

	if err := json.NewEncoder(w).Encode(&result); err != nil {
		logger.Log(logger.LevelError, nil, err, "encoding config")

		http.Error(w, "Invalid JSON request body", http.StatusBadRequest)
	}
}

// websocketConnContextKey extracts the user identity from websocket subprotocols
// and builds the same cache key shape used by HTTP stateless requests.
func websocketConnContextKey(r *http.Request, clusterName string) string {
	// Expected number of submatches in the regular expression
	const expectedSubmatches = 2

	var userID string
	// Define a regular expression pattern for base64url.headlamp.authorization.k8s.io
	pattern := `base64url\.headlamp\.authorization\.k8s\.io\.([a-zA-Z0-9_-]+)`

	// Compile the regular expression
	re := regexp.MustCompile(pattern)

	// Find the match in the header value
	matches := re.FindStringSubmatch(r.Header.Get("Sec-Websocket-Protocol"))

	// Check if a match is found
	if len(matches) >= expectedSubmatches {
		userID = matches[1]
	}

	// Remove the Headlamp identity subprotocol so it is not forwarded upstream.
	// Split on commas and trim space so the rewrite does not depend on ", " spacing.
	protocols := strings.Split(r.Header.Get("Sec-Websocket-Protocol"), ",")

	var updatedProtocols []string

	for _, protocol := range protocols {
		protocol = strings.TrimSpace(protocol)
		if protocol == "" {
			continue
		}

		if !strings.HasPrefix(protocol, "base64url.headlamp.authorization.k8s.io.") {
			updatedProtocols = append(updatedProtocols, protocol)
		}
	}

	if len(updatedProtocols) > 0 {
		r.Header.Set("Sec-Websocket-Protocol", strings.Join(updatedProtocols, ", "))
	} else {
		r.Header.Del("Sec-Websocket-Protocol")
	}

	if userID == "" {
		return clusterName
	}

	return statelessContextKey(clusterName, userID)
}

// getContextKeyForRequest handles every requests. It returns context key
// which is used to store the context in the cache. The context key is
// unique for each user. It is found in the "X-HEADLAMP-USER-ID" parameter.
// For stateless clusters it is combination of cluster name and user id.
// For normal clusters it is just the cluster name.
func (c *HeadlampConfig) getContextKeyForRequest(r *http.Request) (string, error) {
	if resolved, ok := r.Context().Value(resolvedClusterContextKey{}).(resolvedClusterContext); ok {
		return resolved.key, nil
	}

	var contextKey string

	clusterName := mux.Vars(r)["clusterName"]

	// checking if kubeConfig exists, if not check if the request headers for kubeConfig information
	kubeConfig := r.Header.Get("KUBECONFIG")

	if kubeConfig != "" && c.EnableDynamicClusters {
		// if kubeConfig is set and dynamic clusters are enabled then handle stateless cluster requests
		key, err := c.handleStatelessReq(r, kubeConfig)
		if err != nil {
			logger.Log(logger.LevelError, nil, err, "handling stateless request")

			return "", err
		}

		contextKey = key
	} else {
		contextKey = clusterName
	}

	// This means the connection is from websocket so there won't be kubeconfig header.
	// We get the value of X-HEADLAMP-USER-ID from the parameter and append it to the cluster name
	// to get the context key. This is to ensure that the context key is unique for each user.
	if r.Header.Get("Upgrade") == "websocket" {
		contextKey = websocketConnContextKey(r, clusterName)
	}

	return contextKey, nil
}
