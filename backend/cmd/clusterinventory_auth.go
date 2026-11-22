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

package main

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	"github.com/kubernetes-sigs/headlamp/backend/pkg/auth"
	"github.com/kubernetes-sigs/headlamp/backend/pkg/cache"
	"github.com/kubernetes-sigs/headlamp/backend/pkg/kubeconfig"
)

var errUserAuthRequired = errors.New("user authentication is required for this OIDC connection")

// authenticatedHandler validates the desktop credential before resolving a
// connection, since stateless resolution can register a context or renew its TTL.
func (c *HeadlampConfig) authenticatedHandler(next http.Handler) http.Handler {
	return auth.NewBackendTokenMiddleware(c.UseInCluster)(c.requireUserAuthentication(next))
}

// requestClusterName returns the cluster a request targets: the {clusterName}
// route variable, or the cluster query of the legacy drain status route.
func requestClusterName(r *http.Request, pathTemplate string) string {
	if clusterName := mux.Vars(r)["clusterName"]; clusterName != "" {
		return clusterName
	}

	if strings.HasSuffix(pathTemplate, "/drain-node-status") {
		return r.URL.Query().Get("cluster")
	}

	return ""
}

// resolveRequestContext returns the connection the request will use. A user's own
// kubeconfig shadows a global connection with the same name, so the lookup follows
// the handlers' resolution. The result is attached to the returned request: the
// WebSocket user-id subprotocol is consumed on first use and must not be resolved twice.
// It returns the lookup error when the cluster cannot be resolved.
func (c *HeadlampConfig) resolveRequestContext(
	r *http.Request,
	clusterName string,
) (*http.Request, *kubeconfig.Context, error) {
	if mux.Vars(r)["clusterName"] == "" {
		// The legacy drain status route names its cluster in the query.
		kContext, err := c.KubeConfigStore.GetContext(clusterName)
		resolved := resolvedClusterContext{key: clusterName, context: kContext}

		return r.WithContext(context.WithValue(r.Context(), resolvedClusterContextKey{}, resolved)), kContext, err
	}

	contextKey, err := c.getContextKeyForRequest(r)
	if err != nil {
		return r, nil, err
	}

	contextKey, kContext, err := c.getContextWithWebSocketFallback(r, contextKey)
	if err != nil {
		return r, nil, err
	}

	resolved := resolvedClusterContext{key: contextKey, context: kContext}

	return r.WithContext(context.WithValue(r.Context(), resolvedClusterContextKey{}, resolved)), kContext, nil
}

// requireUserAuthentication runs before cluster handlers and the response
// cache. Kubernetes validates the user's token and applies that user's RBAC.
func (c *HeadlampConfig) requireUserAuthentication(next http.Handler) http.Handler {
	refresh := c.OIDCTokenRefreshMiddleware(next)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathTemplate, _ := mux.CurrentRoute(r).GetPathTemplate()

		clusterName := requestClusterName(r, pathTemplate)
		if clusterName == "" {
			next.ServeHTTP(w, r)
			return
		}

		if strings.HasSuffix(pathTemplate, "/clusters/{clusterName}/set-token") {
			request, connection, err := c.resolveRequestContext(r, clusterName)
			if !validateDiscoveryConnectionRevision(w, request, connection, err) {
				return
			}

			next.ServeHTTP(w, auth.WithCookieConnection(request, connection))

			return
		}

		r, kContext, resolveErr := c.resolveRequestContext(r, clusterName)
		checkedConnection := kContext

		// The legacy status cache can still answer for a removed unversioned
		// connection. Keep its original key and never look up a replacement.
		if mux.Vars(r)["clusterName"] == "" && errors.Is(resolveErr, cache.ErrNotFound) {
			checkedConnection = &kubeconfig.Context{}
			resolveErr = nil
		}

		if !validateDiscoveryConnectionRevision(w, r, checkedConnection, resolveErr) {
			return
		}

		r = auth.WithCookieConnection(r, kContext)
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			processWebSocketProtocolHeader(r)
		}

		c.normalizeClusterCredentials(r, clusterName, kContext)

		if kContext.RequiresUserAuthentication() && requestBearerToken(r) == "" {
			http.Error(w, errUserAuthRequired.Error(), http.StatusUnauthorized)
			return
		}

		refresh.ServeHTTP(w, r)
	})
}

func requestBearerToken(r *http.Request) string {
	authorization := r.Header.Get("Authorization")

	parts := strings.Fields(authorization)
	if len(parts) < 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}

	return auth.BearerTokenValue(authorization)
}

// normalizeClusterCredentials makes cache authorization and cluster handlers use
// the same selected credential, including auth-proxy and cookie authentication.
func (c *HeadlampConfig) normalizeClusterCredentials(r *http.Request, cluster string, k *kubeconfig.Context) {
	if c.shouldUseUnsafeServiceAccountTokenForContext(k) {
		clearRequestAuthorization(r)
	} else {
		token := auth.BearerTokenValue(c.proxyAuthToken(r))

		// Prefer the request's Authorization header, using the cookie as a fallback.
		if token == "" {
			token = c.requestTokenForContext(r, cluster, k)
		}

		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
	}

	clearConfiguredProxyTokenHeader(r, c.ProxyAuthTokenHeader)
	r.Header.Del("Cookie")
}
