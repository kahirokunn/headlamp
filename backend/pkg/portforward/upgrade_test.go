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

package portforward_test

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/kubernetes-sigs/headlamp/backend/pkg/cache"
	"github.com/kubernetes-sigs/headlamp/backend/pkg/kubeconfig"
	"github.com/kubernetes-sigs/headlamp/backend/pkg/portforward"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"               //nolint:staticcheck // Matches the client-go port-forward dialer.
	portspdy "k8s.io/apimachinery/pkg/util/httpstream/spdy" //nolint:staticcheck // Matches the client-go port-forward dialer.
	api "k8s.io/client-go/tools/clientcmd/api"
	clientforward "k8s.io/client-go/tools/portforward"
)

type stalledForward struct {
	name, prefix                string
	imported, proxy, permission bool
}

func TestReplacementCancelsStalledForwardUpgrade(t *testing.T) {
	for _, test := range []stalledForward{
		{name: "spdy"},
		{name: "websocket", prefix: "/proxy"},
		{name: "imported-spdy", imported: true},
		{name: "http-proxy-connect", proxy: true},
		{name: "websocket-proxy-connect", prefix: "/proxy", proxy: true},
		{name: "permission-review", permission: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			testReplacementCancelsUpgrade(t, test)
		})
	}
}

//nolint:funlen // Set up a stalled access review, proxy tunnel, or protocol upgrade.
func testReplacementCancelsUpgrade(t *testing.T, scenario stalledForward) {
	t.Helper()

	entered := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isPermission := strings.HasSuffix(r.URL.Path, "/selfsubjectaccessreviews")
		switch {
		case r.Method == http.MethodConnect || (scenario.permission && isPermission) ||
			strings.HasSuffix(r.URL.Path, "/portforward"):
			if isPermission {
				_, _ = io.Copy(io.Discard, r.Body)
			}

			close(entered)

			select {
			case <-r.Context().Done():
				close(canceled)
			case <-release:
				w.WriteHeader(http.StatusBadGateway)
			}
		case strings.HasSuffix(r.URL.Path, "/selfsubjectreviews"):
			writeForwardTestIdentity(w)
		case isPermission:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"apiVersion": "authorization.k8s.io/v1",
				"kind":       "SelfSubjectAccessReview",
				"status":     map[string]bool{"allowed": true},
			})
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	defer close(release)

	const cluster = "workload"

	original := &kubeconfig.Context{
		Name:        cluster,
		Discovery:   &kubeconfig.DiscoveryOrigin{ConnectionRevision: "original"},
		Cluster:     &api.Cluster{Server: upstream.URL + scenario.prefix},
		KubeContext: &api.Context{Cluster: cluster, AuthInfo: cluster},
		AuthInfo:    &api.AuthInfo{Token: "test-token"},
	}
	if scenario.proxy {
		original.Cluster.ProxyURL = upstream.URL
	}

	store := kubeconfig.NewContextStore()
	forwardCache := cache.New[interface{}]()
	portforward.WatchConnections(store, forwardCache)

	connectionKey := cluster
	if scenario.imported {
		connectionKey = cluster + "\x00browser--import"
		original.Discovery = nil
		original.ImportRevision = "import-original"
		require.NoError(t, store.AddContextWithKeyAndTTL(original, connectionKey, time.Second))
	} else {
		require.NoError(t, store.AddContext(original))
	}

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/portforward",
		strings.NewReader(`{"id":"pending","pod":"test","namespace":"default","targetPort":"80"}`))
	r = mux.SetURLVars(r, map[string]string{"clusterName": cluster})
	r.Header.Set("Authorization", "Bearer test-token")

	result := make(chan int, 1)

	go func() {
		w := httptest.NewRecorder()
		portforward.RequestHandlerForStoredContext(original, store, forwardCache, false, false, connectionKey, w, r)

		result <- w.Code
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("upgrade attempt did not start")
	}

	if scenario.imported {
		require.NoError(t, store.UpdateTTL(connectionKey, time.Nanosecond))
		_, err := store.GetContext(connectionKey)
		require.NoError(t, err)
		require.NoError(t, store.RemoveContext(connectionKey))
	} else {
		replacement := original.Copy()
		replacement.Discovery.ConnectionRevision = "replacement"
		require.NoError(t, store.AddContext(replacement))
	}

	select {
	case code := <-result:
		require.Equal(t, http.StatusConflict, code)
	case <-time.After(2 * time.Second):
		t.Fatal("port-forward start did not return")
	}

	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("the original API upgrade stayed open after replacement")
	}
}

func TestReplacementClosesEstablishedForward(t *testing.T) {
	t.Run("SPDY", func(t *testing.T) { testEstablishedForward(t, "") })
	t.Run("WebSocket", func(t *testing.T) { testEstablishedForward(t, "/proxy") })
}

//nolint:funlen // Send real bytes through the established tunnel, then replace its owning connection.
func testEstablishedForward(t *testing.T, prefix string) {
	t.Helper()

	closed := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/selfsubjectreviews") {
			writeForwardTestIdentity(w)
			return
		}

		if strings.HasSuffix(r.URL.Path, "/selfsubjectaccessreviews") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"apiVersion": "authorization.k8s.io/v1",
				"kind":       "SelfSubjectAccessReview",
				"status":     map[string]bool{"allowed": true},
			})

			return
		}

		conn := acceptForward(t, w, r, prefix != "")
		if conn == nil {
			return
		}

		select {
		case <-conn.CloseChan():
			close(closed)
		case <-t.Context().Done():
			_ = conn.Close()
		}
	}))
	t.Cleanup(upstream.Close)

	const cluster = "workload"

	original := &kubeconfig.Context{
		Name:        cluster,
		Discovery:   &kubeconfig.DiscoveryOrigin{ConnectionRevision: "original"},
		Cluster:     &api.Cluster{Server: upstream.URL + prefix},
		KubeContext: &api.Context{Cluster: cluster, AuthInfo: cluster},
		AuthInfo:    &api.AuthInfo{Token: "test-token"},
	}
	store := kubeconfig.NewContextStore()
	forwardCache := cache.New[interface{}]()
	portforward.WatchConnections(store, forwardCache)
	require.NoError(t, store.AddContext(original))
	t.Cleanup(func() { _ = store.RemoveContext(cluster) })

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/portforward",
		strings.NewReader(`{"id":"established","pod":"test","namespace":"default","targetPort":"80"}`))
	r = mux.SetURLVars(r, map[string]string{"clusterName": cluster})
	r.Header.Set("Authorization", "Bearer test-token")

	w := httptest.NewRecorder()
	portforward.RequestHandlerForStoredContext(original, store, forwardCache, false, false, cluster, w, r)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var forward struct {
		Port string `json:"port"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &forward))
	address := net.JoinHostPort("127.0.0.1", forward.Port)
	dialer := &net.Dialer{Timeout: time.Second}
	listener, err := dialer.DialContext(t.Context(), "tcp", address)
	require.NoError(t, err)

	t.Cleanup(func() { _ = listener.Close() })
	require.NoError(t, listener.SetDeadline(time.Now().Add(2*time.Second)))

	const payload = "bytes through the tunnel"

	_, err = io.WriteString(listener, payload)
	require.NoError(t, err)

	echo := make([]byte, len(payload))
	_, err = io.ReadFull(listener, echo)
	require.NoError(t, err)
	require.Equal(t, payload, string(echo))

	select {
	case <-closed:
		t.Fatal("tunnel closed before replacing its connection")
	default:
	}

	replacement := original.Copy()
	replacement.Discovery.ConnectionRevision = "replacement"
	require.NoError(t, store.AddContext(replacement))

	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the established upstream connection remained open")
	}

	require.Eventually(t, func() bool {
		conn, err := dialer.DialContext(t.Context(), "tcp", address)
		if err != nil {
			return true
		}

		_ = conn.Close()

		return false
	}, time.Second, 10*time.Millisecond)
}

func acceptForward(t *testing.T, w http.ResponseWriter, r *http.Request, useWebSocket bool) httpstream.Connection {
	t.Helper()

	if useWebSocket {
		upgrader := websocket.Upgrader{Subprotocols: []string{"SPDY/3.1+portforward.k8s.io"}}

		socket, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return nil
		}

		conn, err := portspdy.NewServerConnection(clientforward.NewTunnelingConnection("server", socket), echoForwardStream)
		if err != nil {
			t.Error(err)

			_ = socket.Close()

			return nil
		}

		return conn
	}

	if _, err := httpstream.Handshake(r, w, []string{"portforward.k8s.io"}); err != nil {
		t.Error(err)
		return nil
	}

	return portspdy.NewResponseUpgrader().UpgradeResponse(w, r, echoForwardStream)
}

func echoForwardStream(stream httpstream.Stream, replySent <-chan struct{}) error {
	go func() {
		<-replySent

		if stream.Headers().Get(corev1.StreamType) == corev1.StreamTypeData {
			_, _ = io.Copy(stream, stream)
		}

		_ = stream.Close()
	}()

	return nil
}

func writeForwardTestIdentity(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview",
		"status":{"userInfo":{"username":"forward-user"}}}`)
}
