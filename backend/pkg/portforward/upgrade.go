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
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	socksproxy "golang.org/x/net/proxy"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	portconstants "k8s.io/apimachinery/pkg/util/portforward"
	"k8s.io/client-go/tools/portforward"

	"k8s.io/apimachinery/pkg/util/httpstream"               //nolint:staticcheck // Matches the client-go port-forward dialer.
	portspdy "k8s.io/apimachinery/pkg/util/httpstream/spdy" //nolint:staticcheck // Matches the client-go port-forward dialer.
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport/spdy"
)

type upgradeRoundTripFunc func(*http.Request) (*http.Response, error)

func (f upgradeRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func withPortForwardUpgradeContext(ctx context.Context, config *rest.Config) *rest.Config {
	copy := rest.CopyConfig(config)
	copy.Wrap(func(next http.RoundTripper) http.RoundTripper {
		return upgradeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			upgradeContext, cancel := context.WithTimeout(ctx, PortForwardReadinessTimeout)
			defer cancel()

			return next.RoundTrip(request.WithContext(upgradeContext))
		})
	})

	return copy
}

// The stock WebSocket transport exposes the socket only after an HTTP proxy's
// CONNECT response. Bind its initial TCP connection to the upgrade context so
// retiring a cluster also interrupts an outstanding proxy handshake.
type webSocketForwardDialer struct {
	endpoint *url.URL
	config   *rest.Config
	dialer   websocket.Dialer
}

func newWebSocketForwardDialer(endpoint *url.URL, config *rest.Config) (*webSocketForwardDialer, error) {
	tlsConfig, err := rest.TLSConfigFor(config)
	if err != nil {
		return nil, err
	}

	proxy := config.Proxy
	if proxy == nil {
		proxy = utilnet.NewProxierWithNoProxyCIDR(http.ProxyFromEnvironment)
	}

	return &webSocketForwardDialer{endpoint, config, websocket.Dialer{Proxy: proxy, TLSClientConfig: tlsConfig}}, nil
}

func (d *webSocketForwardDialer) Dial(protocols ...string) (httpstream.Connection, string, error) {
	dialer := d.dialer
	for _, protocol := range protocols {
		dialer.Subprotocols = append(dialer.Subprotocols, portconstants.WebsocketsSPDYTunnelingPrefix+protocol)
	}

	var ws *websocket.Conn

	upgrade := upgradeRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		conn, response, err := dialWebSocketForward(req, dialer, d.config.Dial)
		ws = conn

		return response, err
	})

	transport, err := rest.HTTPWrappersForConfig(d.config, upgrade)
	if err != nil {
		return nil, "", err
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, d.endpoint.String(), nil)
	if err != nil {
		return nil, "", err
	}

	response, err := transport.RoundTrip(req)
	if err != nil {
		return nil, "", err
	}

	_ = response.Body.Close()
	if ws == nil {
		return nil, "", &httpstream.UpgradeFailureError{Cause: fmt.Errorf("WebSocket upgrade returned %s", response.Status)}
	}

	selected := ws.Subprotocol()
	if !slices.Contains(dialer.Subprotocols, selected) {
		_ = ws.Close()
		return nil, "", &httpstream.UpgradeFailureError{Cause: fmt.Errorf("unexpected WebSocket protocol %q", selected)}
	}

	tunnel := portforward.NewTunnelingConnection("client", ws)

	conn, err := portspdy.NewClientConnectionWithPings(tunnel, portforward.PingPeriod)
	if err != nil {
		_ = tunnel.Close()
		return nil, "", err
	}

	return conn, strings.TrimPrefix(selected, portconstants.WebsocketsSPDYTunnelingPrefix), nil
}

func dialWebSocketForward(req *http.Request, dialer websocket.Dialer,
	tcpDial func(context.Context, string, string) (net.Conn, error),
) (*websocket.Conn, *http.Response, error) {
	var stop func() bool
	defer func() {
		if stop != nil {
			stop()
		}
	}()

	dialer.NetDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dial := tcpDial
		if dial == nil {
			dial = (&net.Dialer{}).DialContext
		}

		conn, err := dial(ctx, network, addr)
		if err == nil {
			stop = context.AfterFunc(ctx, func() { _ = conn.Close() })
		}

		return conn, err
	}

	target := *req.URL
	switch target.Scheme {
	case "https":
		target.Scheme = "wss"
	case "http":
		target.Scheme = "ws"
	default:
		return nil, nil, fmt.Errorf("unsupported port-forward scheme %q", target.Scheme)
	}

	conn, response, err := dialer.DialContext(req.Context(), target.String(), req.Header)
	if errors.Is(err, websocket.ErrBadHandshake) {
		if response != nil {
			// Authentication wrappers inspect HTTP rejections before the dialer decides on fallback.
			return nil, response, nil
		}

		return nil, nil, &httpstream.UpgradeFailureError{Cause: err}
	}

	return conn, response, err
}

const (
	portForwardSPDYVersion = "SPDY/3.1"
	portForwardPingPeriod  = 5 * time.Second
)

type portForwardSPDYTransport struct {
	tlsConfig *tls.Config
	proxy     func(*http.Request) (*url.URL, error)
	conn      net.Conn
}

func newPortForwardSPDYTransport(config *rest.Config) (http.RoundTripper, spdy.Upgrader, error) {
	tlsConfig, err := rest.TLSConfigFor(config)
	if err != nil {
		return nil, nil, err
	}

	proxy := config.Proxy
	if proxy == nil {
		proxy = http.ProxyFromEnvironment
	}

	upgrader := &portForwardSPDYTransport{tlsConfig: tlsConfig, proxy: proxy}
	transport, err := rest.HTTPWrappersForConfig(config, upgrader)

	return transport, upgrader, err
}

func (p *portForwardSPDYTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", portForwardSPDYVersion)

	conn, err := p.dial(request)
	if err != nil {
		return nil, err
	}

	// The stock SPDY round tripper waits for the upgrade response on a
	// raw socket. The forwarder's stop channel is only used after that.
	stop := context.AfterFunc(request.Context(), func() { _ = conn.Close() })
	defer stop()

	reader := bufio.NewReader(conn)

	response, err := http.ReadResponse(reader, request)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	if response.StatusCode != http.StatusSwitchingProtocols ||
		!strings.Contains(strings.ToLower(response.Header.Get("Connection")), "upgrade") ||
		!strings.Contains(strings.ToLower(response.Header.Get("Upgrade")), strings.ToLower(portForwardSPDYVersion)) {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, maxPortForwardBodyBytes))
		_ = conn.Close()
		_ = response.Body.Close()
		response.Body = io.NopCloser(bytes.NewReader(detail))

		return response, nil
	}

	p.conn = &bufferedUpgradeConnection{Conn: conn, reader: reader}

	return response, nil
}

func (p *portForwardSPDYTransport) dial(request *http.Request) (net.Conn, error) {
	proxy, err := p.proxy(request)
	if err != nil {
		return nil, err
	}

	if proxy != nil && (proxy.Scheme == "" || proxy.Scheme == "http" || proxy.Scheme == "https") {
		return p.dialHTTPProxy(request, proxy)
	}

	var dial func(context.Context, string, string) (net.Conn, error)

	if proxy != nil {
		if proxy.Scheme != "socks5" {
			return nil, fmt.Errorf("unsupported port-forward proxy scheme %q", proxy.Scheme)
		}

		socks, err := socksproxy.FromURL(proxy, &net.Dialer{})
		if err != nil {
			return nil, err
		}

		contextDialer, ok := socks.(socksproxy.ContextDialer)
		if !ok {
			return nil, errors.New("SOCKS5 proxy does not support context dialing")
		}

		dial = contextDialer.DialContext
	}

	return p.dialAndWrite(request, dial)
}

// Take ownership of the socket before the first write, including the proxy CONNECT.
func (p *portForwardSPDYTransport) dialAndWrite(request *http.Request,
	dial func(context.Context, string, string) (net.Conn, error),
) (net.Conn, error) {
	config := p.tlsConfig
	if config == nil {
		config = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}

	var stop func() bool

	transport := &http.Transport{
		TLSClientConfig: config,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			raw, err := dial(ctx, network, address)
			if err == nil {
				stop = context.AfterFunc(request.Context(), func() { _ = raw.Close() })
			}

			return raw, err
		},
	}

	dialer, err := portspdy.NewRoundTripperWithConfig(portspdy.RoundTripperConfig{UpgradeTransport: transport})
	if err != nil {
		return nil, err
	}

	conn, err := dialer.Dial(request)

	if stop != nil {
		stop()
	}

	return conn, err
}

func (p *portForwardSPDYTransport) dialHTTPProxy(request *http.Request, proxy *url.URL) (net.Conn, error) {
	target := request.URL

	port := target.Port()
	if port == "" {
		port = "80"
		if target.Scheme == "https" {
			port = "443"
		}
	}

	address := net.JoinHostPort(target.Hostname(), port)

	proxyAddress := *proxy
	if proxyAddress.Scheme == "" {
		proxyAddress.Scheme = "http"
	}

	proxyAddress.Path = ""
	proxyAddress.RawPath = ""
	proxyAddress.RawQuery = ""
	proxyAddress.Opaque = address
	proxyRequest := &http.Request{Method: http.MethodConnect, URL: &proxyAddress, Host: address}
	proxyRequest = proxyRequest.WithContext(request.Context())

	if proxy.User != nil {
		password, _ := proxy.User.Password()
		proxyRequest.Header = make(http.Header)
		proxyRequest.SetBasicAuth(proxy.User.Username(), password)
		proxyRequest.Header.Set("Proxy-Authorization", proxyRequest.Header.Get("Authorization"))
		proxyRequest.Header.Del("Authorization")
	}

	raw, err := p.dialAndWrite(proxyRequest, nil)
	if err != nil {
		return nil, err
	}

	return finishPortForwardProxyTunnel(request, raw, p.tlsConfig)
}

func finishPortForwardProxyTunnel(request *http.Request, raw net.Conn, targetTLS *tls.Config) (net.Conn, error) {
	stop := context.AfterFunc(request.Context(), func() { _ = raw.Close() })
	defer stop()

	reader := bufio.NewReader(raw)

	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = raw.Close()
		_ = response.Body.Close()

		return nil, fmt.Errorf("proxy CONNECT returned %s", response.Status)
	}

	var tunnel net.Conn = &bufferedUpgradeConnection{Conn: raw, reader: reader}

	if request.URL.Scheme == "https" {
		config := &tls.Config{MinVersion: tls.VersionTLS12}
		if targetTLS != nil {
			config = targetTLS.Clone()
		}

		if config.ServerName == "" {
			config.ServerName = request.URL.Hostname()
		}

		tlsTunnel := tls.Client(tunnel, config)
		if err := tlsTunnel.HandshakeContext(request.Context()); err != nil {
			_ = tlsTunnel.Close()
			return nil, err
		}

		tunnel = tlsTunnel
	}

	if err := request.Write(tunnel); err != nil {
		_ = tunnel.Close()
		return nil, err
	}

	return tunnel, nil
}

func (p *portForwardSPDYTransport) NewConnection(response *http.Response) (httpstream.Connection, error) {
	if p.conn == nil {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, maxPortForwardBodyBytes))

		return nil, fmt.Errorf("unable to upgrade connection: %s %s", response.Status, strings.TrimSpace(string(detail)))
	}

	conn, err := portspdy.NewClientConnectionWithPings(p.conn, portForwardPingPeriod)
	if err != nil {
		_ = p.conn.Close()
	}

	return conn, err
}

type bufferedUpgradeConnection struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedUpgradeConnection) Read(data []byte) (int, error) {
	return c.reader.Read(data)
}
