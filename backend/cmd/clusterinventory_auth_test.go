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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/kubernetes-sigs/headlamp/backend/pkg/auth"
	"github.com/kubernetes-sigs/headlamp/backend/pkg/kubeconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

const testImportRevisionKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestRejectedBackendCredentialsDoNotChangeContexts(t *testing.T) {
	t.Setenv("HEADLAMP_BACKEND_TOKEN", "desktop-token")

	for _, token := range []string{"", "wrong-token"} {
		t.Run("token="+token, func(t *testing.T) {
			cfg := newDrainAuthConfig(t, &kubeconfig.Context{Name: "unrelated"})
			cfg.UseInCluster = false
			cfg.EnableDynamicClusters = true
			cfg.KubeConfigDir = t.TempDir()
			cfg.KubeConfigPath = filepath.Join(cfg.KubeConfigDir, "missing")
			handler := createHeadlampHandler(newTestHandlerCtx(t), cfg)
			config := userKubeconfigFor(t, "user-context", "https://127.0.0.1:1")
			key := statelessContextKey("user-context", "user")
			reject := func() {
				req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
					"/clusters/user-context/api/v1/pods", nil)
				req.Header.Set(importRevisionKeyHeader, testImportRevisionKey)
				req.Header.Set("KUBECONFIG", base64.StdEncoding.EncodeToString(config))
				req.Header.Set("X-HEADLAMP-USER-ID", "user")

				if token != "" {
					req.Header.Set("X-HEADLAMP_BACKEND-TOKEN", token)
				}

				response := httptest.NewRecorder()
				handler.ServeHTTP(response, req)
				require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			}

			reject()

			_, err := cfg.KubeConfigStore.GetContext(key)
			assert.Error(t, err, "a rejected request must not register a context")
			require.NoError(t, cfg.KubeConfigStore.AddContextWithKeyAndTTL(&kubeconfig.Context{
				Name: "user-context", KubeContext: &api.Context{}, AuthInfo: &api.AuthInfo{},
			}, key, 250*time.Millisecond))
			reject()
			require.Eventually(t, func() bool {
				_, err := cfg.KubeConfigStore.GetContext(key)
				return err != nil
			}, 2*time.Second, 25*time.Millisecond, "a rejected request must not extend the context lifetime")
		})
	}
}

func TestInventoryRoutesRequireUserAuthentication(t *testing.T) {
	provider := newSigningOIDCTestServer(t)
	handler, cluster := newOIDCTestHandler(t, provider, func(c *HeadlampConfig) {
		ctx, err := c.KubeConfigStore.GetContext("oidc-char-test")
		require.NoError(t, err)

		ctx.Source = kubeconfig.ClusterInventory
		c.UnsafeUseServiceAccountToken = true
		c.CacheEnabled = true
	})

	for _, path := range []string{
		"api/v1/pods", "version", "helm/releases/list", "serviceproxy/default/api",
		"portforward/list", "drain-node-status",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/clusters/"+cluster+"/"+path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			require.Equal(t, http.StatusUnauthorized, response.Code, response.Body.String())
		})
	}
}

func TestClusterConnectionPreconditionPreservesLookupErrors(t *testing.T) {
	cfg := newDrainAuthConfig(t, &kubeconfig.Context{Name: "unrelated"})
	cfg.EnableDynamicClusters = true
	router := mux.NewRouter()
	handleClusterAPI(cfg, router)

	for _, tc := range []struct {
		name, revision, kubeconfig string
		status                     int
	}{
		{name: "unknown ordinary cluster", revision: unversionedDiscoveryConnection, status: http.StatusNotFound},
		{
			name: "invalid stateless kubeconfig", revision: unversionedDiscoveryConnection,
			kubeconfig: "invalid!", status: http.StatusBadRequest,
		},
		{name: "removed discovered connection", revision: "previous", status: http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/clusters/missing/api/v1/pods", nil)
			req.Header.Set(discoveryConnectionRevisionHeader, tc.revision)

			if tc.kubeconfig != "" {
				req.Header.Set(importRevisionKeyHeader, testImportRevisionKey)
				req.Header.Set("KUBECONFIG", tc.kubeconfig)
				req.Header.Set("X-HEADLAMP-USER-ID", "user")
			}

			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			require.Equal(t, tc.status, response.Code, response.Body.String())
		})
	}
}

//nolint:funlen // Exercises credential selection through the proxy.
func TestInventoryProxyUsesEachUsersToken(t *testing.T) {
	var received []string

	apiServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = append(received, r.Header.Get("Authorization"))
		assert.Empty(t, r.Header.Get("Cookie"))

		if r.Header.Get("Authorization") == "Bearer user-b" {
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	t.Cleanup(apiServer.Close)

	cfg := newDrainAuthConfig(t, &kubeconfig.Context{
		Name: "inventory", Source: kubeconfig.ClusterInventory,
		KubeContext: &api.Context{Cluster: "inventory", AuthInfo: "inventory"},
		Cluster:     &api.Cluster{Server: apiServer.URL, InsecureSkipTLSVerify: true},
		AuthInfo:    &api.AuthInfo{}, OidcConf: &kubeconfig.OidcConfig{ClientID: "headlamp"},
	})
	router := mux.NewRouter()
	handleClusterAPI(cfg, router)

	for _, tc := range []struct {
		name, authorization, cookie, protocol string
		status                                int
	}{
		{name: "anonymous", status: 401},
		{name: "user a", authorization: "Bearer user-a", status: 200},
		{name: "user b RBAC denial", authorization: "Bearer user-b", status: 403},
		{name: "cookie", cookie: "user-a", status: 200},
		{name: "header wins over stale cookie", authorization: "Bearer user-a", cookie: "user-b", status: 200},
		{name: "non-bearer header without cookie", authorization: "Basic dXNlcjpwYXNz", status: 401},
		{name: "cookie replaces non-bearer header", authorization: "Basic dXNlcjpwYXNz", cookie: "user-a", status: 200},
		{name: "websocket", protocol: "user-a", status: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/clusters/inventory/api/v1/pods", nil)
			if tc.authorization != "" {
				req.Header.Set("Authorization", tc.authorization)
			}

			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{
					Name: "headlamp-auth-inventory.0", Value: tc.cookie,
					Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
				})
			}

			if tc.protocol != "" {
				req.Header.Set("Upgrade", "websocket")
				req.Header.Set("Sec-WebSocket-Protocol",
					"base64url.bearer.authorization.k8s.io."+base64.RawURLEncoding.EncodeToString([]byte(tc.protocol)))
			}

			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			require.Equal(t, tc.status, response.Code, response.Body.String())
		})
	}

	require.Equal(t, []string{
		"Bearer user-a", "Bearer user-b", "Bearer user-a", "Bearer user-a", "Bearer user-a", "Bearer user-a",
	}, received)
}

func TestInventoryOIDCLoginForwardsTheSignedInUser(t *testing.T) {
	provider := newSigningOIDCTestServer(t)

	var forwardedToken string

	apiServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwardedToken = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(apiServer.Close)

	handler, cluster := newOIDCTestHandler(t, provider, func(c *HeadlampConfig) {
		ctx, err := c.KubeConfigStore.GetContext("oidc-char-test")
		require.NoError(t, err)

		ctx.Source = kubeconfig.ClusterInventory
		ctx.Cluster = &api.Cluster{Server: apiServer.URL, InsecureSkipTLSVerify: true}
		c.OidcUseCookie = true
		c.SessionTTL = 3600
	})
	state := extractState(t, driveOIDCStart(t, handler, cluster))
	callback := callOIDCCallback(t, handler, "state="+state+"&code=code")
	require.Equal(t, http.StatusSeeOther, callback.Code, callback.Body.String())

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/clusters/"+cluster+"/api/v1/pods", nil)
	for _, cookie := range callback.Result().Cookies() {
		req.AddCookie(cookie)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Len(t, strings.Split(forwardedToken, "."), 3)
}

func TestOIDCLoginExpiresWhenTheConnectionIsReplaced(t *testing.T) {
	provider := newSigningOIDCTestServer(t)

	var store kubeconfig.ContextStore

	handler, cluster := newOIDCTestHandler(t, provider, func(config *HeadlampConfig) {
		store = config.KubeConfigStore
		connection, err := store.GetContext("oidc-char-test")
		require.NoError(t, err)

		connection.Discovery = &kubeconfig.DiscoveryOrigin{ConnectionRevision: "original"}
	})
	state := extractState(t, driveOIDCStart(t, handler, cluster))
	original, err := store.GetContext(cluster)
	require.NoError(t, err)

	replacement := original.Copy()
	replacement.Discovery.ConnectionRevision = "replacement"
	require.NoError(t, store.AddContext(replacement))

	callback := callOIDCCallback(t, handler, "state="+state+"&code=code")
	require.Equal(t, http.StatusConflict, callback.Code, callback.Body.String())
	require.Empty(t, callback.Header().Values("Set-Cookie"))
}

//nolint:funlen // Drive login, callback and logout through the same HTTP handler.
func TestImportedOIDCUsesItsOwnConnection(t *testing.T) {
	provider := newSigningOIDCTestServer(t)
	global := setupOIDCProviderWithEndSessionEndpoint()(t)

	var config *HeadlampConfig

	handler, cluster := newOIDCTestHandler(t, provider, func(c *HeadlampConfig) {
		config = c
		c.UseInCluster = true
		c.EnableDynamicClusters = true
		c.OidcUseCookie = true
		c.SessionTTL = 600
		connection, err := c.KubeConfigStore.GetContext("oidc-char-test")
		require.NoError(t, err)

		connection.OidcConf.IdpIssuerURL = global
	})
	kubeconfigData, err := clientcmd.Write(api.Config{
		Clusters: map[string]*api.Cluster{cluster: {Server: "https://k8s.example.com"}},
		Contexts: map[string]*api.Context{cluster: {Cluster: cluster, AuthInfo: cluster}},
		AuthInfos: map[string]*api.AuthInfo{cluster: {AuthProvider: &api.AuthProviderConfig{
			Name: "oidc", Config: map[string]string{
				"idp-issuer-url": provider.URL(), "client-id": "test-client-id",
			},
		}}},
		CurrentContext: cluster,
	})
	require.NoError(t, err)

	raw := base64.StdEncoding.EncodeToString(kubeconfigData)
	connections, _, err := kubeconfig.LoadContextsFromBase64String(raw, kubeconfig.DynamicCluster)
	require.NoError(t, err)
	require.Len(t, connections, 1)

	fingerprint, err := importedConnectionRevision(&connections[0], testImportRevisionKey)
	require.NoError(t, err)

	revision := fingerprint + "-1"
	user := "browser--" + revision
	request := func(method, path string, cookies []*http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), method, path, nil)
		if method == http.MethodPost {
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set(importRevisionKeyHeader, testImportRevisionKey)
			r.Header.Set("KUBECONFIG", raw)
			r.Header.Set("X-HEADLAMP-USER-ID", user)
			r.Header.Set(discoveryConnectionRevisionHeader, revision)
		}

		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}

		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)

		return response
	}
	redirectURL := func(response *httptest.ResponseRecorder) *url.URL {
		t.Helper()
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())

		var result struct{ RedirectURL string }
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		target, err := url.Parse(result.RedirectURL)
		require.NoError(t, err)

		return target
	}

	login := "/oidc?cluster=" + cluster + "&popup=true"
	target := redirectURL(request(http.MethodPost, login, nil))
	require.Equal(t, provider.URL()+"/auth", target.Scheme+"://"+target.Host+target.Path)
	require.NotContains(t, target.String(), user)

	callback := driveOIDCCallback(t, handler, extractState(t, target))
	require.Equal(t, http.StatusSeeOther, callback.Code, callback.Body.String())
	require.Contains(t, callback.Header().Get("Location"), discoveryConnectionRevisionParam+"="+revision)
	require.Contains(t, callback.Header().Get("Location"), "popup=true")
	cookies := callback.Result().Cookies()
	require.NotEmpty(t, cookies)

	desktopState := extractState(t, redirectURL(request(http.MethodPost, login, nil)))
	desktopRequest := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/oidc-callback?state="+url.QueryEscape(desktopState)+"&code="+url.QueryEscape(testAuthCode), nil)
	desktopRequest.Header.Set("Accept", "application/json")

	// An in-progress login pins its imported context even when unrelated
	// requests update the cache's idle expiry.
	require.NoError(t, config.KubeConfigStore.UpdateTTL(statelessContextKey(cluster, user), time.Nanosecond))

	desktopCallback := httptest.NewRecorder()
	handler.ServeHTTP(desktopCallback, desktopRequest)
	require.Equal(t, http.StatusNoContent, desktopCallback.Code)
	require.Empty(t, desktopCallback.Header().Get("Location"))
	require.NotEmpty(t, desktopCallback.Result().Cookies())

	logoutPath := auth.GetOIDCLogoutCookiePath("", cluster)
	transition := redirectURL(request(http.MethodPost, logoutPath, cookies))
	require.NotContains(t, transition.String(), user)
	follow := request(http.MethodGet, transition.String(), cookies)
	require.Equal(t, http.StatusFound, follow.Code, follow.Body.String())
	require.Contains(t, follow.Header().Get("Location"), provider.URL()+"/logout?")
	require.NotContains(t, follow.Header().Get("Location"), global)

	pending := extractState(t, redirectURL(request(http.MethodPost, login, nil)))
	require.NoError(t, config.removeImportedConnection(t.Context(), statelessContextKey(cluster, user), revision))
	staleCallback := driveOIDCCallback(t, handler, pending)
	require.Equal(t, http.StatusConflict, staleCallback.Code, staleCallback.Body.String())
	require.Empty(t, staleCallback.Header().Values("Set-Cookie"))
	require.Equal(t, http.StatusConflict, request(http.MethodPost, logoutPath, cookies).Code)
}

func TestInventoryMultiplexerRejectsMissingCredentialsOnReusedConnection(t *testing.T) {
	store := kubeconfig.NewContextStore()
	require.NoError(t, store.AddContext(&kubeconfig.Context{
		Name: "inventory", Source: kubeconfig.ClusterInventory,
		OidcConf: &kubeconfig.OidcConfig{ClientID: "headlamp"},
	}))
	m := NewMultiplexer(store, true)
	message := Message{ClusterID: "inventory", UserID: "user-a", Path: "/api/v1/pods"}
	m.connections[connectionKey{message.ClusterID, message.Path, message.UserID, message.Query}] = &Connection{}

	for _, token := range []*string{nil, new(string)} {
		_, err := m.getOrCreateConnection(message, nil, token, nil)
		require.ErrorContains(t, err, "user authentication is required")
	}

	require.NoError(t, store.AddContext(&kubeconfig.Context{Name: "inventory", Source: kubeconfig.KubeConfig}))

	_, err := m.getOrCreateConnection(message, nil, nil, nil)
	require.NoError(t, err)
}

func TestOIDCRoutesUseTheSameAuthenticationForEachSource(t *testing.T) {
	for _, source := range []int{
		kubeconfig.ClusterInventory, kubeconfig.DiscoveredCluster, kubeconfig.KubeConfig, kubeconfig.DynamicCluster,
	} {
		t.Run(fmt.Sprint(source), func(t *testing.T) {
			apiServer, _ := newCacheConnectionServer(t, "pod", "user", nil)
			_, router := cacheConnectionRouter(t, cacheConnectionContext(t, apiServer, source))
			anonymous := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/clusters/cache-switch/api/v1/pods", nil)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, anonymous)
			require.Equal(t, http.StatusUnauthorized, response.Code)
			require.Equal(t, http.StatusOK, cacheConnectionRequest(t, router, http.MethodGet, "user").Code)
		})
	}
}

func TestRenameRejectsContextsWithoutPersistentOrigin(t *testing.T) {
	for _, source := range []int{
		kubeconfig.ClusterInventory, kubeconfig.DiscoveredCluster, kubeconfig.KubeConfig, kubeconfig.DynamicCluster,
	} {
		cfg := newDrainAuthConfig(t, &kubeconfig.Context{Name: "memory", Source: source})

		for _, stateless := range []bool{false, true} {
			body := fmt.Sprintf(`{"newClusterName":"renamed","source":"kubeconfig","stateless":%t}`, stateless)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/cluster/memory", strings.NewReader(body))
			req = mux.SetURLVars(req, map[string]string{"name": "memory"})
			response := httptest.NewRecorder()
			cfg.renameCluster(response, req)
			require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())

			_, err := cfg.KubeConfigStore.GetContext("memory")
			require.NoError(t, err, "a rejected rename must not remove the generated connection")
		}
	}
}

func TestInventoryLegacyDrainStatusRequiresUserAuthentication(t *testing.T) {
	provider := newSigningOIDCTestServer(t)
	handler, cluster := newOIDCTestHandler(t, provider, func(c *HeadlampConfig) {
		ctx, err := c.KubeConfigStore.GetContext("oidc-char-test")
		require.NoError(t, err)

		ctx.Source = kubeconfig.ClusterInventory
	})

	for _, tc := range []struct {
		name, token string
		status      int
	}{
		{name: "anonymous", status: http.StatusUnauthorized},
		// No drain was recorded, so a signed-in user reaches the handler's cache lookup.
		{name: "signed in user", token: "user-a", status: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
				"/drain-node-status?cluster="+cluster+"&nodeName=node-1", nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}

			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			require.Equal(t, tc.status, response.Code, response.Body.String())
		})
	}
}

// userKubeconfigFor returns a kubeconfig that authenticates with its own basic-auth credentials.
func userKubeconfigFor(t *testing.T, name, server string) []byte {
	t.Helper()

	data, err := clientcmd.Write(api.Config{
		Clusters:       map[string]*api.Cluster{name: {Server: server, InsecureSkipTLSVerify: true}},
		AuthInfos:      map[string]*api.AuthInfo{name: {Username: "local-user", Password: "local-pass"}},
		Contexts:       map[string]*api.Context{name: {Cluster: name, AuthInfo: name}},
		CurrentContext: name,
	})
	require.NoError(t, err)

	return data
}

// A user's own kubeconfig shadows a global connection with the same name, so the
// authentication gate must judge the connection the request will actually use.
func TestTokenEndpointsSelectImportedConnection(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("token management must not call Kubernetes")
	}))
	t.Cleanup(server.Close)

	const cluster = "imported"

	global := &kubeconfig.Context{
		Name: cluster, Source: kubeconfig.DiscoveredCluster,
		KubeContext: &api.Context{Cluster: cluster, AuthInfo: cluster},
		Cluster:     &api.Cluster{Server: server.URL, InsecureSkipTLSVerify: true},
		AuthInfo:    &api.AuthInfo{},
		Discovery:   &kubeconfig.DiscoveryOrigin{ConnectionRevision: "global"},
	}
	cfg := newDrainAuthConfig(t, global)
	cfg.EnableDynamicClusters = true
	router := mux.NewRouter()
	handleClusterAPI(cfg, router)

	userKubeconfig := base64.StdEncoding.EncodeToString(userKubeconfigFor(t, cluster, server.URL))

	for _, shadowed := range []bool{true, false} {
		for _, endpoint := range []string{
			"/clusters/" + cluster + "/set-token",
			"/encoded-clusters/" + auth.EncodeClusterForURL(cluster) + "/clear-token",
		} {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, endpoint,
				strings.NewReader(`{"token":"private"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(importRevisionKeyHeader, testImportRevisionKey)
			req.Header.Set("KUBECONFIG", userKubeconfig)
			req.Header.Set("X-HEADLAMP-USER-ID", "browser-user")
			req.Header.Set(discoveryConnectionRevisionHeader, unversionedDiscoveryConnection)

			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			require.Equal(t, http.StatusOK, response.Code, "shadowed=%t: %s", shadowed, response.Body.String())
		}

		if shadowed {
			require.NoError(t, cfg.KubeConfigStore.RemoveContext(cluster))
		}
	}

	_, err := cfg.KubeConfigStore.GetContext(statelessContextKey(cluster, "browser-user"))
	require.NoError(t, err)
}

//nolint:funlen // Exercise both imported routes through the HTTP router.
func TestReplacementImportKeepsItsOwnRouteAndCookies(t *testing.T) {
	const cluster = "same-name"

	reached := make(chan string, 8)
	makeAPI := func(endpoint string) *httptest.Server {
		return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Empty(t, r.Header.Get(importRevisionKeyHeader))
			assert.Empty(t, r.Header.Get("KUBECONFIG"))

			reached <- endpoint + ":" + r.Header.Get("Authorization")

			_, _ = w.Write([]byte("{}"))
		}))
	}
	original := makeAPI("A")
	t.Cleanup(original.Close)

	replacement := makeAPI("B")
	t.Cleanup(replacement.Close)

	config := newDrainAuthConfig(t, &kubeconfig.Context{Name: cluster})
	config.EnableDynamicClusters = true
	router := mux.NewRouter()
	handleClusterAPI(config, router)

	kubeconfigA := base64.StdEncoding.EncodeToString(userKubeconfigFor(t, cluster, original.URL))
	kubeconfigB := base64.StdEncoding.EncodeToString(userKubeconfigFor(t, cluster, replacement.URL))
	parsed, errs := parseClusterFromKubeConfig([]string{kubeconfigA, kubeconfigB}, testImportRevisionKey)
	require.Empty(t, errs)
	require.Len(t, parsed, 2)
	require.Equal(t, 0, *parsed[0].KubeconfigIndex)
	require.Equal(t, 1, *parsed[1].KubeconfigIndex)
	revisionA, revisionB := parsed[0].ImportRevision, parsed[1].ImportRevision
	require.NotEqual(t, revisionA, revisionB)

	request := func(method, path, raw, revision string, cookies []*http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), method, "/clusters/"+cluster+path,
			strings.NewReader(`{"token":"token-for-A"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(importRevisionKeyHeader, testImportRevisionKey)
		r.Header.Set("KUBECONFIG", raw)
		r.Header.Set("X-HEADLAMP-USER-ID", "browser-user--"+revision)
		r.Header.Set(discoveryConnectionRevisionHeader, revision)

		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}

		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)

		return w
	}

	save := request(http.MethodPost, "/set-token", kubeconfigA, revisionA, nil)
	require.Equal(t, http.StatusOK, save.Code, save.Body.String())
	cookies := save.Result().Cookies()
	require.NotEmpty(t, cookies)

	readA := request(http.MethodGet, "/api/v1/pods", kubeconfigA, revisionA, cookies)
	require.Equal(t, http.StatusOK, readA.Code, readA.Body.String())
	require.Len(t, reached, 1)
	require.Equal(t, "A:Bearer token-for-A", <-reached)

	readB := request(http.MethodGet, "/api/v1/pods", kubeconfigB, revisionB, cookies)
	require.Equal(t, http.StatusOK, readB.Code, readB.Body.String())
	require.Len(t, reached, 1)
	require.Equal(t, "B:Basic bG9jYWwtdXNlcjpsb2NhbC1wYXNz", <-reached)

	stale := request(http.MethodDelete, "/api/v1/pods/work", kubeconfigB, revisionA, cookies)
	require.Equal(t, http.StatusConflict, stale.Code, stale.Body.String())
	require.Empty(t, reached)
}

//nolint:funlen // Test retirement, late rehydration, and a fresh incarnation through the router.
func TestRetiringAnImportCancelsOnlyItsConnection(t *testing.T) {
	const alias, browser = "workload", "browser"

	cfg := newDrainAuthConfig(t, &kubeconfig.Context{Name: alias})
	cfg.EnableDynamicClusters = true
	router := mux.NewRouter()
	cfg.addClusterSetupRoute(router)
	handleClusterAPI(cfg, router)

	add := func(host, incarnation string) (string, string, *kubeconfig.Context, string) {
		raw := base64.StdEncoding.EncodeToString(userKubeconfigFor(t, alias, "https://"+host+".example.invalid"))
		contexts, errs, err := kubeconfig.LoadContextsFromBase64String(raw, kubeconfig.DynamicCluster)
		require.NoError(t, err)
		require.Empty(t, errs)
		require.Len(t, contexts, 1)
		revision, err := importedConnectionRevision(&contexts[0], testImportRevisionKey)
		require.NoError(t, err)

		userID := browser + "--" + revision + "-" + incarnation
		key := statelessContextKey(alias, userID)
		require.NoError(t, cfg.setKeyInCache(key, contexts[0]))
		connection, err := cfg.KubeConfigStore.GetContext(key)
		require.NoError(t, err)

		return userID, key, connection, raw
	}
	oldID, oldKey, old, originalRaw := add("original", "1")
	newID, newKey, replacement, _ := add("replacement", "2")
	ctxA, releaseA, err := cfg.KubeConfigStore.RetainContext(oldKey, old, time.Second)
	require.NoError(t, err)
	t.Cleanup(releaseA)

	ctxB, releaseB, err := cfg.KubeConfigStore.RetainContext(newKey, replacement, time.Second)
	require.NoError(t, err)
	t.Cleanup(releaseB)

	retire := func(userID string) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
			"/stateless-connections/"+auth.EncodeClusterForURL(alias)+"/retire", nil)
		req.Header.Set("X-HEADLAMP-USER-ID", userID)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	}
	retire("other-" + oldID)
	require.NoError(t, ctxA.Err())
	require.NoError(t, cfg.KubeConfigStore.UpdateTTL(oldKey, time.Nanosecond))
	_, err = cfg.KubeConfigStore.GetContext(oldKey)
	require.NoError(t, err)

	retire(oldID)
	retire(oldID)
	require.Error(t, ctxA.Err())
	require.NoError(t, ctxB.Err())
	require.NotEqual(t, oldID, newID)

	_, err = cfg.KubeConfigStore.GetContext(oldKey)
	require.Error(t, err)
	_, err = cfg.KubeConfigStore.GetContext(newKey)
	require.NoError(t, err)

	request := func(userID string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
			"/clusters/"+alias+"/set-token", strings.NewReader(`{"token":"test"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(importRevisionKeyHeader, testImportRevisionKey)
		req.Header.Set("KUBECONFIG", originalRaw)
		req.Header.Set("X-HEADLAMP-USER-ID", userID)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		return w
	}
	stale := request(oldID)
	require.Equal(t, http.StatusConflict, stale.Code, stale.Body.String())
	require.Contains(t, stale.Body.String(), "Discovered cluster connection changed")

	_, err = cfg.KubeConfigStore.GetContext(oldKey)
	require.Error(t, err)

	// An intentional re-import can use the same connection settings under a new incarnation.
	restored := request(strings.TrimSuffix(oldID, "-1") + "-3")
	require.Equal(t, http.StatusOK, restored.Code, restored.Body.String())
}

func TestSelectImportWithSharedEffectiveName(t *testing.T) {
	load := func(name string) kubeconfig.Context {
		raw := userKubeconfigFor(t, name, "https://"+name+".example.invalid")
		encoded := base64.StdEncoding.EncodeToString(raw)
		contexts, errs, err := kubeconfig.LoadContextsFromBase64String(encoded, kubeconfig.DynamicCluster)
		require.NoError(t, err)
		require.Empty(t, errs)

		c := contexts[0]
		c.KubeContext.Extensions = map[string]runtime.Object{
			"headlamp_info": &runtime.Unknown{Raw: []byte(`{"customName":"shared"}`)},
		}

		return c
	}

	contexts := []kubeconfig.Context{load("first"), load("second")}
	parsed, errs := parseCustomNameClusters(contexts, testImportRevisionKey)
	require.Empty(t, parsed)
	require.Len(t, errs, 1)
	require.ErrorContains(t, errs[0], "shared")

	sibling := load("other")
	sibling.KubeContext.Extensions = nil
	parsed, errs = parseCustomNameClusters(append(contexts, sibling), testImportRevisionKey)
	require.Len(t, errs, 1)
	require.Len(t, parsed, 1)
	require.Equal(t, "other", parsed[0].Name)

	for _, selected := range contexts {
		revision, err := importedConnectionRevision(&selected, testImportRevisionKey)
		require.NoError(t, err)
		_, routed, err := selectStatelessContext("shared", "browser--"+revision, testImportRevisionKey, contexts)
		require.NoError(t, err)
		require.Equal(t, selected.Cluster.Server, routed.Cluster.Server)
	}

	_, _, err := selectStatelessContext("shared", "browser--import-"+strings.Repeat("0", 32),
		testImportRevisionKey, contexts)
	require.ErrorIs(t, err, errClusterConnectionChanged)
}

func TestImportedRevisionIgnoresOtherContextsInTheRecord(t *testing.T) {
	config := api.Config{
		Clusters: map[string]*api.Cluster{
			"alpha": {Server: "https://alpha.example.invalid"},
			"beta":  {Server: "https://beta.example.invalid"},
		},
		AuthInfos: map[string]*api.AuthInfo{
			"alpha": {Token: "alpha-token"},
			"beta":  {Token: "beta-token"},
		},
		Contexts: map[string]*api.Context{
			"alpha": {Cluster: "alpha", AuthInfo: "alpha"},
			"beta":  {Cluster: "beta", AuthInfo: "beta", Namespace: "workloads"},
		},
		CurrentContext: "beta",
	}
	revision := func(key string) string {
		raw, err := clientcmd.Write(config)
		require.NoError(t, err)

		clusters, errs := parseClusterFromKubeConfig([]string{base64.StdEncoding.EncodeToString(raw)}, key)
		require.Empty(t, errs)

		for _, cluster := range clusters {
			if cluster.Name == "beta" {
				return cluster.ImportRevision
			}
		}

		t.Fatal("beta is missing from parsed kubeconfig")

		return ""
	}

	original := revision(testImportRevisionKey)
	require.NotEmpty(t, original)
	require.Empty(t, revision(""))
	require.NotEqual(t, original, revision(strings.Repeat("ab", 32)))
	delete(config.Contexts, "alpha")
	delete(config.AuthInfos, "alpha")
	delete(config.Clusters, "alpha")
	require.Equal(t, original, revision(testImportRevisionKey))

	config.AuthInfos["beta"].Token = "replacement-token"

	require.NotEqual(t, original, revision(testImportRevisionKey))
}

func TestLegacyDrainStatusSurvivesMissingUnversionedContext(t *testing.T) {
	const cluster, node = "local", "node-1"

	config := newDrainAuthConfig(t, &kubeconfig.Context{Name: cluster})
	require.NoError(t, config.KubeConfigStore.RemoveContext(cluster))
	require.NoError(t, config.Cache.Set(t.Context(), drainCacheKey(node, cluster, nil), "pending"))

	router := mux.NewRouter()
	router.Handle("/drain-node-status", config.authenticatedHandler(http.HandlerFunc(config.handleNodeDrainStatus)))

	for _, tc := range []struct {
		revision string
		want     int
	}{
		{unversionedDiscoveryConnection, http.StatusOK},
		{"superseded", http.StatusConflict},
	} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
			"/drain-node-status?cluster="+cluster+"&nodeName="+node, nil)
		req.Header.Set(discoveryConnectionRevisionHeader, tc.revision)

		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		require.Equal(t, tc.want, response.Code, response.Body.String())
	}
}

//nolint:funlen // HTTP and WebSocket credential integration.
func TestStatelessConnectionShadowsSameNamedOIDCConnection(t *testing.T) {
	t.Setenv("HEADLAMP_BACKEND_TOKEN", "desktop-token")

	var received []string

	apiServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = append(received, r.Header.Get("Authorization"))
	}))
	t.Cleanup(apiServer.Close)

	cfg := newDrainAuthConfig(t, &kubeconfig.Context{
		Name: "inventory", Source: kubeconfig.ClusterInventory,
		KubeContext: &api.Context{Cluster: "inventory", AuthInfo: "inventory"},
		Cluster:     &api.Cluster{Server: apiServer.URL, InsecureSkipTLSVerify: true},
		AuthInfo:    &api.AuthInfo{}, OidcConf: &kubeconfig.OidcConfig{ClientID: "headlamp"},
	})
	cfg.EnableDynamicClusters = true
	cfg.UseInCluster = false

	router := mux.NewRouter()
	handleClusterAPI(cfg, router)

	userKubeconfig := userKubeconfigFor(t, "inventory", apiServer.URL)

	const userAuthorization = "Basic bG9jYWwtdXNlcjpsb2NhbC1wYXNz"

	do := func(mutate func(*http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/clusters/inventory/api/v1/pods", nil)
		req.Header.Set("X-HEADLAMP_BACKEND-TOKEN", "desktop-token")
		mutate(req)

		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)

		return response
	}

	t.Run("anonymous request to the global connection is rejected", func(t *testing.T) {
		require.Equal(t, http.StatusUnauthorized, do(func(*http.Request) {}).Code)
	})

	t.Run("stateless request uses its own credentials", func(t *testing.T) {
		received = nil
		response := do(func(req *http.Request) {
			req.Header.Set(importRevisionKeyHeader, testImportRevisionKey)
			req.Header.Set("KUBECONFIG", base64.StdEncoding.EncodeToString(userKubeconfig))
			req.Header.Set("X-HEADLAMP-USER-ID", "user-a")
		})
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.Equal(t, []string{userAuthorization}, received)
	})

	t.Run("websocket resolves the cached stateless connection once", func(t *testing.T) {
		received = nil
		response := do(func(req *http.Request) {
			req.Header.Set("Upgrade", "websocket")
			req.Header.Del("X-HEADLAMP_BACKEND-TOKEN")
			req.Header.Set("Sec-WebSocket-Protocol", "base64url.headlamp.authorization.k8s.io.user-a, "+
				"base64url.headlamp.backend.authorization.k8s.io."+base64.RawURLEncoding.EncodeToString([]byte("desktop-token")))
		})
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.Equal(t, []string{userAuthorization}, received)
	})
}

//nolint:funlen // HTTP identity and cache lifecycle cases.
func TestStatelessRenamePreservesOtherConnections(t *testing.T) {
	t.Setenv("HEADLAMP_BACKEND_TOKEN", "desktop-token")

	for _, tc := range []struct {
		name, userID, backendToken string
		cached, expired, stateless bool
		status                     int
	}{
		{"cached", "user-a", "desktop-token", true, false, true, http.StatusCreated},
		{"other user", "user-b", "desktop-token", true, false, true, http.StatusCreated},
		{"not connected", "user-a", "desktop-token", false, false, true, http.StatusCreated},
		{"expired", "user-a", "desktop-token", true, true, true, http.StatusCreated},
		{"missing user ID", "", "desktop-token", true, false, true, http.StatusBadRequest},
		{"missing backend token", "user-a", "", true, false, true, http.StatusForbidden},
		{"invalid backend token", "user-a", "invalid", true, false, true, http.StatusForbidden},
		{"generated connection", "user-a", "desktop-token", true, false, false, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			global := &kubeconfig.Context{
				Name: "inventory", Source: kubeconfig.ClusterInventory,
				KubeContext: &api.Context{}, Cluster: &api.Cluster{Server: "https://global.example.com"},
			}
			cfg := newDrainAuthConfig(t, global)
			cfg.UseInCluster = false

			for _, user := range []string{"user-a", "user-b"} {
				if user == "user-a" && !tc.cached {
					continue
				}

				ttl := time.Minute
				if user == "user-a" && tc.expired {
					ttl = -time.Second
				}

				require.NoError(t, cfg.KubeConfigStore.AddContextWithKeyAndTTL(&kubeconfig.Context{
					Internal: true, KubeContext: &api.Context{}, Cluster: &api.Cluster{Server: "https://user.example.com"},
				}, statelessContextKey(global.Name, user), ttl))
			}

			router := mux.NewRouter()
			router.Handle("/cluster/{name}", cfg.authenticatedHandler(http.HandlerFunc(cfg.renameCluster))).Methods("PUT")

			req, err := makeJSONReq(http.MethodPut, "/cluster/inventory", RenameClusterRequest{
				NewClusterName: "my-workload", Stateless: tc.stateless,
			})
			require.NoError(t, err)
			req.Header.Set("X-HEADLAMP-USER-ID", tc.userID)
			req.Header.Set("X-HEADLAMP_BACKEND-TOKEN", tc.backendToken)

			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			require.Equal(t, tc.status, response.Code, response.Body.String())

			stored, err := cfg.KubeConfigStore.GetContext(global.Name)
			require.NoError(t, err)
			require.Same(t, global, stored)

			for _, user := range []string{"user-a", "user-b"} {
				_, err := cfg.KubeConfigStore.GetContext(statelessContextKey(global.Name, user))

				removed := tc.status == http.StatusCreated && user == tc.userID
				if removed || (user == "user-a" && (!tc.cached || tc.expired)) {
					require.Error(t, err, "cached connection for %s must be absent", user)
				} else {
					require.NoError(t, err, "cached connection for %s must remain", user)
				}
			}
		})
	}
}
