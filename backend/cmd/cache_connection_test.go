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
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/kubernetes-sigs/headlamp/backend/pkg/auth"
	inventorymetadata "github.com/kubernetes-sigs/headlamp/backend/pkg/clusterinventory/metadata"
	"github.com/kubernetes-sigs/headlamp/backend/pkg/k8cache"
	"github.com/kubernetes-sigs/headlamp/backend/pkg/kubeconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd/api"
)

func newCacheConnectionServer(
	t *testing.T, pod, token string, before func(*http.Request),
) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	podRequests := &atomic.Int32{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		allowed := r.Header.Get("Authorization") == "Bearer "+token
		if strings.Contains(r.Header.Get("Accept"), "as=PartialObjectMetadata") {
			if !allowed {
				http.Error(w, "denied", http.StatusForbidden)
				return
			}

			serveMetadataListWatch(w, r, nil)

			return
		}

		if before != nil {
			before(r)
		}

		switch {
		case strings.HasSuffix(r.URL.Path, "/selfsubjectaccessreviews"):
			writeAccessReview(w, allowed)
		case r.URL.Path == "/api/v1/pods":
			if !allowed {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"kind":"Status","status":"Failure","reason":"Forbidden"}`))

				return
			}

			podRequests.Add(1)

			_ = json.NewEncoder(w).Encode(corev1.PodList{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
				Items:    []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: pod}}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	return server, podRequests
}

// serveMetadataListWatch answers the metadata list that seeds a shared cache
// watch, then holds a watch open until the client disconnects.
func serveMetadataListWatch(w http.ResponseWriter, r *http.Request, onWatch func()) {
	w.Header().Set("Content-Type", "application/json")

	if r.URL.Query().Get("watch") == "true" {
		if onWatch != nil {
			onWatch()
		}

		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()

		return
	}

	_, _ = w.Write([]byte(`{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadataList",` +
		`"metadata":{"resourceVersion":"1"},"items":[]}`))
}

func writeAccessReview(w http.ResponseWriter, allowed bool) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(authorizationv1.SelfSubjectAccessReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "authorization.k8s.io/v1", Kind: "SelfSubjectAccessReview"},
		Status:   authorizationv1.SubjectAccessReviewStatus{Allowed: allowed},
	})
}

func cacheConnectionContext(t *testing.T, server *httptest.Server, source int) *kubeconfig.Context {
	t.Helper()

	kContext := &kubeconfig.Context{
		Name: "cache-switch", ClusterID: "stable-cluster-id", Source: source,
		KubeContext: &api.Context{Cluster: "cache-switch", AuthInfo: "cache-switch"},
		Cluster: &api.Cluster{
			Server: server.URL,
			CertificateAuthorityData: pem.EncodeToMemory(&pem.Block{
				Type: "CERTIFICATE", Bytes: server.Certificate().Raw,
			}),
		},
		AuthInfo: &api.AuthInfo{}, OidcConf: &kubeconfig.OidcConfig{ClientID: "headlamp"},
	}
	require.NoError(t, kContext.SetupProxy())

	return kContext
}

func cacheConnectionRouter(t *testing.T, kContext *kubeconfig.Context) (*HeadlampConfig, *mux.Router) {
	t.Helper()
	resetCacheMiddlewareTestState()
	t.Cleanup(resetCacheMiddlewareTestState)

	cfg := newDrainAuthConfig(t, kContext)
	cfg.CacheEnabled = true
	router := mux.NewRouter()
	handleClusterAPI(cfg, router)

	return cfg, router
}

func cacheConnectionRequest(t *testing.T, router http.Handler, method, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, "/clusters/cache-switch/api/v1/pods", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)

	return response
}

func TestDiscoveredDrainRejectsChangedConnection(t *testing.T) {
	var requests atomic.Int32

	upstream, _ := newCacheConnectionServer(t, "pod", "user", func(*http.Request) { requests.Add(1) })
	connection := cacheConnectionContext(t, upstream, kubeconfig.ClusterInventory)
	connection.Discovery = &kubeconfig.DiscoveryOrigin{ConnectionRevision: "replacement"}
	cfg := newDrainAuthConfig(t, connection)
	cfg.KubeConfigDir = t.TempDir()
	cfg.KubeConfigPath = filepath.Join(cfg.KubeConfigDir, "missing")
	router := createHeadlampHandler(t.Context(), cfg)

	for _, path := range []string{"/clusters/cache-switch/drain-node", "/drain-node"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
				path+"?"+discoveryConnectionRevisionParam+"=original",
				strings.NewReader(`{"cluster":"cache-switch","nodeName":"node-a"}`))
			original := *connection
			original.Discovery = &kubeconfig.DiscoveryOrigin{ConnectionRevision: "original"}
			cookies := httptest.NewRecorder()
			require.NoError(t, auth.SetTokenCookie(cookies, auth.WithCookieConnection(req, &original),
				original.Name, "user", "", 60))

			for _, cookie := range cookies.Result().Cookies() {
				req.AddCookie(cookie)
			}

			req.Header.Set("Content-Type", "application/json")

			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)

			require.Equal(t, http.StatusConflict, res.Code)
			require.Zero(t, requests.Load())
		})
	}
}

//nolint:funlen // Exercises drain submission and polling across two mock clusters.
func TestDiscoveredDrainKeepsStatusWithItsConnection(t *testing.T) {
	originalStarted := make(chan struct{})
	original, _ := newCacheConnectionServer(t, "pod", "user", func(r *http.Request) {
		if r.URL.Path == "/api/v1/nodes/node-a" {
			close(originalStarted)
			<-r.Context().Done()
		}
	})
	replacement := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/api/v1/nodes/node-a":
			_, _ = w.Write([]byte(`{"kind":"Node","apiVersion":"v1","metadata":{"name":"node-a"}}`))
		case "/api/v1/pods":
			_, _ = w.Write([]byte(`{"kind":"PodList","apiVersion":"v1","items":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(replacement.Close)

	originalConnection := cacheConnectionContext(t, original, kubeconfig.ClusterInventory)
	originalConnection.Discovery = &kubeconfig.DiscoveryOrigin{ConnectionRevision: "original"}
	cfg, router := cacheConnectionRouter(t, originalConnection)
	cfg.ServerCtx = t.Context()

	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), method, "/clusters/cache-switch"+path,
			strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer user")
		r.Header.Set("Content-Type", "application/json")

		response := httptest.NewRecorder()
		router.ServeHTTP(response, r)

		return response
	}

	const (
		payload    = `{"cluster":"cache-switch","nodeName":"node-a"}`
		statusPath = "/drain-node-status?nodeName=node-a"
	)

	require.Equal(t, http.StatusOK, request(http.MethodPost, "/drain-node", payload).Code)

	select {
	case <-originalStarted:
	case <-time.After(time.Second):
		t.Fatal("original drain did not start")
	}

	originalStatus := request(http.MethodGet, statusPath, "")
	require.Equal(t, http.StatusOK, originalStatus.Code)
	require.Contains(t, originalStatus.Body.String(), drainStatusInProgress)

	replacementConnection := cacheConnectionContext(t, replacement, kubeconfig.ClusterInventory)
	replacementConnection.Discovery = &kubeconfig.DiscoveryOrigin{ConnectionRevision: "replacement"}
	require.NoError(t, cfg.KubeConfigStore.AddContext(replacementConnection))

	require.Equal(t, http.StatusConflict, request(http.MethodGet,
		statusPath+"&"+discoveryConnectionRevisionParam+"=original", "").Code)
	require.Equal(t, http.StatusNotFound, request(http.MethodGet, statusPath, "").Code)

	require.Equal(t, http.StatusOK, request(http.MethodPost, "/drain-node", payload).Code)
	require.Eventually(t, func() bool {
		response := request(http.MethodGet, statusPath+"&"+discoveryConnectionRevisionParam+"=replacement", "")

		return response.Code == http.StatusOK && strings.Contains(response.Body.String(), `"id":"success"`)
	}, time.Second, 10*time.Millisecond)
}

func TestDiscoveredRequestsUseTheExpectedConnection(t *testing.T) {
	first, _ := newCacheConnectionServer(t, "first-pod", "user", func(r *http.Request) {
		assert.False(t, r.URL.Query().Has(discoveryConnectionRevisionParam))
	})
	second, secondRequests := newCacheConnectionServer(t, "second-pod", "user", nil)
	firstContext := cacheConnectionContext(t, first, kubeconfig.ClusterInventory)
	firstContext.Discovery = &kubeconfig.DiscoveryOrigin{ConnectionRevision: "first"}
	secondContext := cacheConnectionContext(t, second, kubeconfig.ClusterInventory)
	secondContext.Discovery = &kubeconfig.DiscoveryOrigin{ConnectionRevision: "second"}
	cfg, router := cacheConnectionRouter(t, firstContext)

	request := func(method string, upgrade bool) *httptest.ResponseRecorder {
		url := "/clusters/cache-switch/api/v1/pods?" + discoveryConnectionRevisionParam + "=first&limit=1"
		req := httptest.NewRequestWithContext(t.Context(), method, url, nil)
		req.Header.Set("Authorization", "Bearer user")

		if upgrade {
			req.Header.Set("Upgrade", "websocket")
		}

		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)

		return response
	}

	before := request(http.MethodGet, false)
	require.Equal(t, http.StatusOK, before.Code, before.Body.String())
	require.Contains(t, before.Body.String(), "first-pod")

	// The browser keeps using its first configuration snapshot while the route changes.
	require.NoError(t, cfg.KubeConfigStore.AddContext(secondContext))

	conflict := request(http.MethodGet, false)
	require.Equal(t, http.StatusConflict, conflict.Code)
	require.Equal(t, "application/json", conflict.Header().Get("Content-Type"))
	require.JSONEq(t, `{"message":"Discovered cluster connection changed"}`, conflict.Body.String())
	require.Equal(t, http.StatusConflict, request(http.MethodGet, true).Code)

	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete} {
		require.Equal(t, http.StatusConflict, request(method, false).Code)
	}

	require.Zero(t, secondRequests.Load())

	// Clients that omit the revision still see the current upstream.
	unguarded := cacheConnectionRequest(t, router, http.MethodGet, "user")
	require.Equal(t, http.StatusOK, unguarded.Code, unguarded.Body.String())
	require.Contains(t, unguarded.Body.String(), "second-pod")

	require.NoError(t, cfg.KubeConfigStore.AddContext(firstContext))

	after := request(http.MethodGet, false)
	require.Equal(t, http.StatusOK, after.Code, after.Body.String())
	require.Contains(t, after.Body.String(), "first-pod")
}

func TestUnversionedRequestsCannotUseADiscoveredReplacement(t *testing.T) {
	first, _ := newCacheConnectionServer(t, "first-pod", "user", func(r *http.Request) {
		assert.False(t, r.URL.Query().Has(discoveryConnectionRevisionParam))
		assert.Empty(t, r.Header.Get(discoveryConnectionRevisionHeader))
	})
	second, secondRequests := newCacheConnectionServer(t, "second-pod", "user", nil)
	cfg, router := cacheConnectionRouter(t, cacheConnectionContext(t, first, kubeconfig.KubeConfig))

	request := func(method string, upgrade bool) *httptest.ResponseRecorder {
		path := "/clusters/cache-switch/api/v1/pods?limit=1"
		if upgrade {
			path += "&" + discoveryConnectionRevisionParam + "=" + unversionedDiscoveryConnection
		}

		req := httptest.NewRequestWithContext(t.Context(), method, path, nil)
		req.Header.Set("Authorization", "Bearer user")

		if upgrade {
			req.Header.Set("Upgrade", "websocket")
		} else {
			req.Header.Set(discoveryConnectionRevisionHeader, unversionedDiscoveryConnection)
		}

		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)

		return response
	}

	ordinary := request(http.MethodGet, false)
	require.Equal(t, http.StatusOK, ordinary.Code, ordinary.Body.String())
	require.Contains(t, ordinary.Body.String(), "first-pod")

	replacement := cacheConnectionContext(t, second, kubeconfig.ClusterInventory)
	replacement.Discovery = &kubeconfig.DiscoveryOrigin{ConnectionRevision: "replacement"}
	require.NoError(t, cfg.KubeConfigStore.AddContext(replacement))

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete} {
		require.Equal(t, http.StatusConflict, request(method, false).Code)
	}

	require.Equal(t, http.StatusConflict, request(http.MethodGet, true).Code)
	require.Zero(t, secondRequests.Load())
}

func TestDiscoveredConnectionDoesNotReusePreviousLogin(t *testing.T) {
	first, _ := newCacheConnectionServer(t, "first-pod", "first-user", nil)

	var secondRequests atomic.Int32

	second, _ := newCacheConnectionServer(t, "second-pod", "second-user", func(*http.Request) { secondRequests.Add(1) })
	original := cacheConnectionContext(t, first, kubeconfig.ClusterInventory)
	original.Discovery = &kubeconfig.DiscoveryOrigin{ConnectionRevision: "first"}
	replacement := cacheConnectionContext(t, second, kubeconfig.ClusterInventory)
	replacement.Discovery = &kubeconfig.DiscoveryOrigin{ConnectionRevision: "second"}
	cfg, router := cacheConnectionRouter(t, original)

	login := func(token, revision string) []*http.Cookie {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/clusters/cache-switch/set-token",
			strings.NewReader(`{"token":"`+token+`"}`))
		req.Header.Set(discoveryConnectionRevisionHeader, revision)

		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		result := response.Result()

		t.Cleanup(func() { _ = result.Body.Close() })

		return result.Cookies()
	}
	list := func(revision string, cookies []*http.Cookie) int {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/clusters/cache-switch/api/v1/pods", nil)
		req.Header.Set(discoveryConnectionRevisionHeader, revision)

		for _, cookie := range cookies {
			if cookie.MaxAge >= 0 {
				req.AddCookie(cookie)
			}
		}

		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)

		return response.Code
	}

	originalCookies := login("first-user", "first")
	require.Equal(t, http.StatusOK, list("first", originalCookies))
	require.NoError(t, cfg.KubeConfigStore.AddContext(replacement))
	require.Equal(t, http.StatusUnauthorized, list("second", originalCookies))
	require.Zero(t, secondRequests.Load())
	require.Equal(t, http.StatusOK, list("second", login("second-user", "second")))
}

//nolint:funlen // Exercises the same cookie jar across login, replacement, logout, and restoration.
func TestDiscoveredConnectionLogoutInvalidatesPreviousSession(t *testing.T) {
	const cluster = "cache-switch"

	first, _ := newCacheConnectionServer(t, "first-pod", "first-user", nil)
	second, requests := newCacheConnectionServer(t, "second-pod", "second-user", nil)
	original := cacheConnectionContext(t, first, kubeconfig.ClusterInventory)
	original.Discovery = &kubeconfig.DiscoveryOrigin{ConnectionRevision: "original"}
	replacement := cacheConnectionContext(t, second, kubeconfig.ClusterInventory)
	replacement.Discovery = &kubeconfig.DiscoveryOrigin{ConnectionRevision: "replacement"}
	cfg, router := cacheConnectionRouter(t, original)
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)

	request := func(method, path, revision, body string) *httptest.ResponseRecorder {
		t.Helper()

		req := httptest.NewRequestWithContext(t.Context(), method, "http://headlamp.local"+path, strings.NewReader(body))
		req.Header.Set(discoveryConnectionRevisionHeader, revision)
		req.Header.Set("Content-Type", "application/json")

		for _, cookie := range jar.Cookies(req.URL) {
			req.AddCookie(cookie)
		}

		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)

		result := response.Result()
		defer func() { _ = result.Body.Close() }()

		jar.SetCookies(req.URL, result.Cookies())

		return response
	}

	const list = "/clusters/" + cluster + "/api/v1/pods"
	require.Equal(t, http.StatusOK, request(http.MethodPost,
		"/clusters/"+cluster+"/set-token", "original", `{"token":"first-user"}`).Code)
	require.Equal(t, http.StatusOK, request(http.MethodGet, list, "original", "").Code)

	oidcPath := auth.GetOIDCLogoutCookiePath("", cluster)
	idReq := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://headlamp.local"+oidcPath, nil)
	idReq = auth.WithCookieConnection(idReq, original)
	idResponse := httptest.NewRecorder()
	require.NoError(t, auth.SetIDTokenCookie(idResponse, idReq, cluster, "old-id-token", "", 60))

	idResult := idResponse.Result()
	defer func() { _ = idResult.Body.Close() }()

	jar.SetCookies(idReq.URL, idResult.Cookies())

	require.NoError(t, cfg.KubeConfigStore.AddContext(replacement))
	assert.Equal(t, http.StatusConflict, request(http.MethodGet, list, "original", "").Code)
	require.Zero(t, requests.Load())

	clear := "/encoded-clusters/" + auth.EncodeClusterForURL(cluster) + "/clear-token"
	require.Equal(t, http.StatusOK, request(http.MethodPost, clear, "replacement", "").Code)
	require.NoError(t, cfg.KubeConfigStore.AddContext(original))
	require.Equal(t, http.StatusUnauthorized, request(http.MethodGet, list, "original", "").Code)

	for _, cookie := range jar.Cookies(idReq.URL) {
		idReq.AddCookie(cookie)
	}

	token, err := auth.GetIDTokenFromCookie(idReq, cluster)
	require.ErrorIs(t, err, http.ErrNoCookie)
	require.Empty(t, token)
}

type observedTokenBody struct {
	io.Reader
	started chan struct{}
	once    sync.Once
}

func (b *observedTokenBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	return b.Reader.Read(p)
}

func writeTokenTestBody(t *testing.T, writer *io.PipeWriter) {
	t.Helper()

	written := make(chan error, 1)

	go func() {
		_, err := io.WriteString(writer, `{"token":"first-user"}`)
		_ = writer.Close()

		written <- err
	}()

	select {
	case err := <-written:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("request did not consume the token body")
	}
}

func TestPendingTokenRequestDoesNotWriteAfterConnectionReplacement(t *testing.T) {
	for _, operation := range []string{"set", "clear"} {
		t.Run(operation, func(t *testing.T) {
			first, _ := newCacheConnectionServer(t, "first-pod", "first-user", nil)
			second, _ := newCacheConnectionServer(t, "second-pod", "second-user", nil)
			original := cacheConnectionContext(t, first, kubeconfig.ClusterInventory)
			original.Discovery = &kubeconfig.DiscoveryOrigin{ConnectionRevision: "original"}
			replacement := cacheConnectionContext(t, second, kubeconfig.ClusterInventory)
			replacement.Discovery = &kubeconfig.DiscoveryOrigin{ConnectionRevision: "replacement"}
			cfg, router := cacheConnectionRouter(t, original)

			path := "/clusters/cache-switch/set-token"
			if operation == "clear" {
				path = "/encoded-clusters/" + auth.EncodeClusterForURL("cache-switch") + "/clear-token"
			}

			reader, writer := io.Pipe()

			t.Cleanup(func() { _ = writer.Close() })

			body := &observedTokenBody{Reader: reader, started: make(chan struct{})}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, body)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(discoveryConnectionRevisionHeader, "original")

			response := httptest.NewRecorder()
			done := make(chan struct{})

			go func() {
				defer close(done)

				router.ServeHTTP(response, req)
			}()

			select {
			case <-body.started:
			case <-time.After(time.Second):
				t.Fatal("request did not reach token handler")
			}

			require.NoError(t, cfg.KubeConfigStore.AddContext(replacement))

			writeTokenTestBody(t, writer)

			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("request did not finish")
			}

			require.Equal(t, http.StatusConflict, response.Code)
			require.Empty(t, response.Header().Values("Set-Cookie"))
		})
	}
}

func TestCacheConnectionReplacement(t *testing.T) {
	for _, source := range []int{kubeconfig.ClusterInventory, kubeconfig.DiscoveredCluster, kubeconfig.KubeConfig} {
		t.Run(fmt.Sprint(source), func(t *testing.T) {
			oldServer, _ := newCacheConnectionServer(t, "old-pod", "old-user", nil)
			newServer, newRequests := newCacheConnectionServer(t, "new-pod", "new-user", nil)
			oldContext := cacheConnectionContext(t, oldServer, source)
			cfg, router := cacheConnectionRouter(t, oldContext)
			setupKubeConfigStoreWatcher(cfg.KubeConfigStore)

			require.Equal(t, http.StatusOK, cacheConnectionRequest(t, router, http.MethodGet, "old-user").Code)
			requireCachedResource(t, router, "/api/v1/pods", "old-user", "")

			newContext := cacheConnectionContext(t, newServer, source)
			require.NoError(t, cfg.KubeConfigStore.AddContext(newContext))

			first := cacheConnectionRequest(t, router, http.MethodGet, "new-user")
			require.Equal(t, http.StatusOK, first.Code, first.Body.String())
			require.Contains(t, first.Body.String(), "new-pod")
			require.NotContains(t, first.Body.String(), "old-pod")
			require.Empty(t, first.Header().Get("X-HEADLAMP-CACHE"))

			denied := cacheConnectionRequest(t, router, http.MethodGet, "old-user")
			require.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())
			require.NotContains(t, denied.Body.String(), "new-pod")
			requireCachedResource(t, router, "/api/v1/pods", "new-user", "")

			requestsAfterHit := newRequests.Load()

			oldScope, err := k8cache.ContextCacheKey(oldContext.Name, oldContext)
			require.NoError(t, err)
			oldKey, err := k8cache.GenerateKey(&url.URL{Path: "/api/v1/pods"}, oldScope)
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				_, err := k8sResponseCache.Get(t.Context(), oldKey)
				return err != nil
			}, 3*time.Second, 10*time.Millisecond, "context notifications should purge the old connection")
			requireCachedResource(t, router, "/api/v1/pods", "new-user", "")
			require.Equal(t, requestsAfterHit, newRequests.Load())
		})
	}
}

func TestCacheConnectionReplacementDuringRequest(t *testing.T) {
	for _, phase := range []string{"authorization", "response", "mutation"} {
		t.Run(phase, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})

			var unblock sync.Once
			defer unblock.Do(func() { close(release) })

			oldServer, _ := newCacheConnectionServer(t, "old-pod", "shared-user", func(r *http.Request) {
				block := phase == "authorization" && strings.HasSuffix(r.URL.Path, "/selfsubjectaccessreviews") ||
					phase == "response" && r.URL.Path == "/api/v1/pods" ||
					phase == "mutation" && r.Method == http.MethodPost
				if block {
					close(started)
					<-release
				}
			})
			newServer, newRequests := newCacheConnectionServer(t, "new-pod", "shared-user", nil)
			cfg, router := cacheConnectionRouter(t, cacheConnectionContext(t, oldServer, kubeconfig.ClusterInventory))

			method := http.MethodGet
			if phase == "mutation" {
				method = http.MethodPost
			}

			finished := make(chan *httptest.ResponseRecorder, 1)
			go func() { finished <- cacheConnectionRequest(t, router, method, "shared-user") }()

			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("old request did not reach the synchronization point")
			}

			require.NoError(t, cfg.KubeConfigStore.AddContext(cacheConnectionContext(t, newServer, kubeconfig.ClusterInventory)))
			fresh := cacheConnectionRequest(t, router, http.MethodGet, "shared-user")
			require.Equal(t, http.StatusOK, fresh.Code, fresh.Body.String())
			require.Contains(t, fresh.Body.String(), "new-pod")

			unblock.Do(func() { close(release) })

			select {
			case old := <-finished:
				require.Equal(t, http.StatusOK, old.Code, old.Body.String())
				require.Contains(t, old.Body.String(), "old-pod", "an in-flight request must keep its original connection")
			case <-time.After(5 * time.Second):
				t.Fatal("old request did not finish")
			}

			cached := requireCachedResource(t, router, "/api/v1/pods", "shared-user", "")
			assert.Contains(t, cached.Body.String(), "new-pod")
			assert.NotContains(t, cached.Body.String(), "old-pod")

			requestsAfterHit := newRequests.Load()
			response := cacheConnectionRequest(t, router, http.MethodGet, "shared-user")
			require.Equal(t, "true", response.Header().Get("X-HEADLAMP-CACHE"))
			assert.Equal(t, requestsAfterHit, newRequests.Load())
		})
	}
}

func TestCacheConnectionTLSChange(t *testing.T) {
	server, requests := newCacheConnectionServer(t, "private-pod", "user", nil)
	kContext := cacheConnectionContext(t, server, kubeconfig.ClusterInventory)
	cfg, router := cacheConnectionRouter(t, kContext)
	require.Equal(t, http.StatusOK, cacheConnectionRequest(t, router, http.MethodGet, "user").Code)

	replacement := kContext.Copy()
	replacement.Cluster.TLSServerName = "different-cluster.invalid"
	require.NoError(t, replacement.SetupProxy())
	require.NoError(t, cfg.KubeConfigStore.AddContext(replacement))

	response := cacheConnectionRequest(t, router, http.MethodGet, "user")
	assert.Equal(t, http.StatusBadGateway, response.Code, response.Body.String())
	assert.NotContains(t, response.Body.String(), "private-pod")
	assert.Empty(t, response.Header().Get("X-HEADLAMP-CACHE"))
	assert.EqualValues(t, 1, requests.Load())
}

func TestCacheConnectionRevisionChangeWithSameEndpoint(t *testing.T) {
	server, requests := newCacheConnectionServer(t, "pod", "user", nil)
	connection := cacheConnectionContext(t, server, kubeconfig.ClusterInventory)
	connection.Discovery = &kubeconfig.DiscoveryOrigin{ConnectionRevision: "original"}
	cfg, router := cacheConnectionRouter(t, connection)
	require.Equal(t, http.StatusOK, cacheConnectionRequest(t, router, http.MethodGet, "user").Code)
	requireCachedResource(t, router, "/api/v1/pods", "user", "")
	requests.Store(0)

	replacement := connection.Copy()
	replacement.Discovery = &kubeconfig.DiscoveryOrigin{ConnectionRevision: "replacement"}
	require.NoError(t, replacement.SetupProxy())
	require.NoError(t, cfg.KubeConfigStore.AddContext(replacement))

	response := cacheConnectionRequest(t, router, http.MethodGet, "user")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Empty(t, response.Header().Get("X-HEADLAMP-CACHE"))
	require.EqualValues(t, 1, requests.Load())
}

func TestCacheConnectionMetadataUpdate(t *testing.T) {
	server, requests := newCacheConnectionServer(t, "pod", "user", nil)
	kContext := cacheConnectionContext(t, server, kubeconfig.ClusterInventory)
	cfg, router := cacheConnectionRouter(t, kContext)
	require.Equal(t, http.StatusOK, cacheConnectionRequest(t, router, http.MethodGet, "user").Code)

	requireCachedResource(t, router, "/api/v1/pods", "user", "")
	requests.Store(0)

	replacement := kContext.Copy()
	replacement.ClusterInventory = &inventorymetadata.Metadata{Version: &inventorymetadata.Version{Kubernetes: "v1.35.0"}}
	require.NoError(t, replacement.SetupProxy())
	require.NoError(t, cfg.KubeConfigStore.AddContext(replacement))

	response := cacheConnectionRequest(t, router, http.MethodGet, "user")
	assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, "true", response.Header().Get("X-HEADLAMP-CACHE"))
	assert.Zero(t, requests.Load())
}

func TestCacheConnectionConfiguredCredentialsChange(t *testing.T) {
	server, _ := newCacheConnectionServer(t, "private-pod", "allowed", nil)
	kContext := cacheConnectionContext(t, server, kubeconfig.KubeConfig)
	kContext.AuthInfo.Token = "allowed"
	require.NoError(t, kContext.SetupProxy())
	cfg, router := cacheConnectionRouter(t, kContext)

	require.Equal(t, http.StatusOK, cacheResourceRequest(t, router, "/api/v1/pods", "", "", "").Code)

	replacement := kContext.Copy()
	replacement.AuthInfo.Token = "denied"
	require.NoError(t, replacement.SetupProxy())
	require.NoError(t, cfg.KubeConfigStore.AddContext(replacement))

	response := cacheResourceRequest(t, router, "/api/v1/pods", "", "", "")
	assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	assert.NotContains(t, response.Body.String(), "private-pod")
}

func TestCacheConnectionIdentityFailureBypassesCache(t *testing.T) {
	resetCacheMiddlewareTestState()
	t.Cleanup(resetCacheMiddlewareTestState)

	kContext := &kubeconfig.Context{
		Name: "cache-switch", Cluster: &api.Cluster{Server: "https://example.com"},
		AuthInfo: &api.AuthInfo{Exec: &api.ExecConfig{
			Config: &runtime.Unknown{Raw: []byte(`{"credential":"secret"`)},
		}},
	}
	_, err := k8cache.ContextCacheKey(kContext.Name, kContext)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret")

	cfg := newDrainAuthConfig(t, kContext)
	cfg.CacheEnabled = true

	var forwarded int

	router := mux.NewRouter()
	router.PathPrefix("/clusters/{clusterName}/{api:.*}").Handler(CacheMiddleWare(cfg)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, resolved, err := cfg.getContextWithWebSocketFallback(r, kContext.Name)
			assert.NoError(t, err)
			assert.Same(t, kContext, resolved)

			forwarded++
			_, _ = w.Write([]byte("uncached"))
		}),
	))

	for range 2 {
		response := cacheConnectionRequest(t, router, http.MethodGet, "user")
		assert.Equal(t, http.StatusOK, response.Code)
		assert.Equal(t, "uncached", response.Body.String())
		assert.Empty(t, response.Header().Get("X-HEADLAMP-CACHE"))
	}

	assert.Equal(t, 2, forwarded)
	entries, err := k8sResponseCache.GetAll(t.Context(), nil)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestDiscoveredPortForwardRejectsChangedConnection(t *testing.T) {
	var requests atomic.Int32

	upstream, _ := newCacheConnectionServer(t, "pod", "another-user", func(*http.Request) { requests.Add(1) })
	connection := cacheConnectionContext(t, upstream, kubeconfig.ClusterInventory)
	connection.Discovery = &kubeconfig.DiscoveryOrigin{ConnectionRevision: "replacement"}
	cfg := newDrainAuthConfig(t, connection)
	cfg.KubeConfigDir = t.TempDir()
	cfg.KubeConfigPath = filepath.Join(cfg.KubeConfigDir, "missing")
	router := createHeadlampHandler(t.Context(), cfg)

	for _, request := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/portforward", `{"namespace":"default","pod":"pod","targetPort":"80","port":"8080"}`},
		{http.MethodDelete, "/portforward", `{"id":"shared-id","stopOrDelete":true}`},
		{http.MethodGet, "/portforward/list", ""},
	} {
		req := httptest.NewRequestWithContext(t.Context(), request.method,
			"/clusters/cache-switch"+request.path+"?"+discoveryConnectionRevisionParam+"=original",
			strings.NewReader(request.body))
		req.Header.Set("Authorization", "Bearer user")
		req.Header.Set("Content-Type", "application/json")

		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)

		require.Equal(t, http.StatusConflict, res.Code)
		require.Contains(t, res.Body.String(), "Discovered cluster connection changed")
	}

	require.Zero(t, requests.Load())
}

func TestPortForwardRejectsReplacementAfterResolving(t *testing.T) {
	var originalRequests, replacementRequests atomic.Int32

	original, _ := newCacheConnectionServer(t, "pod", "another-user", func(*http.Request) { originalRequests.Add(1) })
	replacement, _ := newCacheConnectionServer(t, "pod", "another-user",
		func(*http.Request) { replacementRequests.Add(1) })
	originalConnection := cacheConnectionContext(t, original, kubeconfig.ClusterInventory)
	originalConnection.Discovery = &kubeconfig.DiscoveryOrigin{ConnectionRevision: "original"}
	replacementConnection := cacheConnectionContext(t, replacement, kubeconfig.ClusterInventory)
	replacementConnection.Discovery = &kubeconfig.DiscoveryOrigin{ConnectionRevision: "replacement"}
	cfg := newDrainAuthConfig(t, originalConnection)

	start := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, cfg.KubeConfigStore.AddContext(replacementConnection))
		cfg.portForwardRequestHandler(w, r)
	})
	router := mux.NewRouter()
	router.Handle("/clusters/{clusterName}/portforward",
		cfg.authenticatedHandler(start),
	).Methods(http.MethodPost)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/clusters/cache-switch/portforward?"+discoveryConnectionRevisionParam+"=original",
		strings.NewReader(`{"namespace":"default","pod":"pod","targetPort":"80","port":"8080"}`))
	req.Header.Set("Authorization", "Bearer user")
	req.Header.Set("Content-Type", "application/json")

	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	require.Equal(t, http.StatusConflict, res.Code)
	require.Contains(t, res.Body.String(), "Discovered cluster connection changed")
	require.Zero(t, originalRequests.Load())
	require.Zero(t, replacementRequests.Load())
}
