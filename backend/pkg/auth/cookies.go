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

package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/kubernetes-sigs/headlamp/backend/pkg/kubeconfig"
)

type cookieConnectionKey struct{}

// WithCookieConnection binds authentication cookies to the selected connection.
func WithCookieConnection(r *http.Request, connection *kubeconfig.Context) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), cookieConnectionKey{}, connection))
}

func cookieConnection(r *http.Request) *kubeconfig.Context {
	connection, _ := r.Context().Value(cookieConnectionKey{}).(*kubeconfig.Context)
	return connection
}

// CookieConnectionCurrent reports whether a selected discovered connection still
// owns the cookie slot after an operation that could have blocked.
func CookieConnectionCurrent(r *http.Request, store kubeconfig.ContextStore) bool {
	connection := cookieConnection(r)

	revision := connection.RoutingRevision()
	if revision == "" {
		return true
	}

	current, err := store.GetContext(connection.Name)

	return err == nil && current.RoutingRevision() == revision
}

func connectionCookieScope(r *http.Request) string {
	connection := cookieConnection(r)

	revision := connection.RoutingRevision()
	if revision == "" {
		return ""
	}

	if connection.ImportRevision != "" {
		revision = connection.Name
	}

	scope := sha256.Sum256([]byte(revision))

	return hex.EncodeToString(scope[:16]) + ":"
}

func cookieClusterName(r *http.Request, cluster string) string {
	name := SanitizeClusterName(cluster)
	connection := cookieConnection(r)

	if name != "" && connection != nil {
		if connection.Discovery != nil && connection.RoutingRevision() != "" {
			return name + ".discovery." + strings.TrimSuffix(connectionCookieScope(r), ":")
		}

		if connection.ImportRevision != "" {
			return name + "." + connection.ImportRevision
		}
	}

	return name
}

// A noncredential marker is visible on both the proxy and encoded logout paths.
// Each login gets a fresh marker so a delayed logout can revoke exactly the
// sessions visible to the browser when it sent the request.
func discoveredSessionPrefix(r *http.Request, cluster, kind string) string {
	connection := cookieConnection(r)
	if connection == nil || connection.Discovery == nil || connection.RoutingRevision() == "" {
		return ""
	}

	key := sha256.Sum256([]byte(cluster))

	return fmt.Sprintf("headlamp-%s-session-%x-", kind, key[:12])
}

func setSessionMarker(w http.ResponseWriter, r *http.Request, name, baseURL string, maxAge int) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure follows the deployment's request scheme.
		Name: name, Value: "1", Path: "/" + strings.Trim(baseURL, "/"),
		HttpOnly: true, Secure: IsSecureContext(r), SameSite: http.SameSiteStrictMode, MaxAge: maxAge,
	})
}

func clearObservedSessions(w http.ResponseWriter, r *http.Request, prefix, baseURL string) {
	if prefix == "" {
		return
	}

	for _, cookie := range r.Cookies() {
		id, ok := strings.CutPrefix(cookie.Name, prefix)
		if ok && len(id) == 32 && cookie.Value == "1" {
			if _, err := hex.DecodeString(id); err == nil {
				setSessionMarker(w, r, cookie.Name, baseURL, -1)
			}
		}
	}
}

func tokenCookieChunks(w http.ResponseWriter, r *http.Request, cluster, kind, token, baseURL string,
	ttl int, chunk func(string) ([]string, error),
) ([]string, error) {
	scope := connectionCookieScope(r)

	prefix := discoveredSessionPrefix(r, cluster, kind)
	if prefix == "" {
		return chunk(scope + token)
	}

	var nonce [16]byte

	_, _ = rand.Read(nonce[:])
	id := hex.EncodeToString(nonce[:])

	chunks, err := chunk(scope + id + ":" + token)
	if err != nil {
		return nil, err
	}

	clearObservedSessions(w, r, prefix, baseURL)
	setSessionMarker(w, r, prefix+id, baseURL, ttl)

	return chunks, nil
}

func readCookieToken(r *http.Request, cluster, kind, value string) string {
	token, matches := strings.CutPrefix(value, connectionCookieScope(r))
	if !matches {
		return ""
	}

	prefix := discoveredSessionPrefix(r, cluster, kind)
	if prefix != "" {
		id, credential, found := strings.Cut(token, ":")
		if !found || len(id) != 32 {
			return ""
		}

		marker, err := r.Cookie(prefix + id)
		if err != nil || marker.Value != "1" {
			return ""
		}

		return credential
	}

	return token
}

const (
	// chunkSize is the size of each token chunk, less than 4KB because of the size limit.
	chunkSize = 3800
	// maxIDTokenChunks bounds client-controlled chunk-count metadata.
	maxIDTokenChunks   = 16
	maxAuthTokenChunks = 32
)

// GetCookiePath returns the full cookie path including baseURL.
func GetCookiePath(baseURL, cluster string) string {
	if baseURL != "" {
		baseURL = "/" + strings.Trim(baseURL, "/")
		return baseURL + "/clusters/" + cluster
	}

	return "/clusters/" + cluster
}

// EncodeClusterForURL returns a path-safe representation of a cluster name.
func EncodeClusterForURL(cluster string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cluster))
}

// DecodeClusterFromURL decodes an encoded cluster segment.
func DecodeClusterFromURL(segment string) (string, error) {
	cluster, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return "", fmt.Errorf("decode cluster URL segment: %w", err)
	}

	if len(cluster) == 0 {
		return "", errors.New("cluster URL segment is empty")
	}

	if !utf8.Valid(cluster) {
		return "", errors.New("cluster URL segment is not valid UTF-8")
	}

	return string(cluster), nil
}

// GetOIDCLogoutCookiePath returns the path for the restricted ID-token cookies.
func GetOIDCLogoutCookiePath(baseURL, cluster string) string {
	trimmedBaseURL := strings.Trim(baseURL, "/")
	if trimmedBaseURL != "" {
		trimmedBaseURL = "/" + trimmedBaseURL
	}

	return trimmedBaseURL + "/encoded-clusters/" + EncodeClusterForURL(cluster) + "/oidc-logout"
}

// SanitizeClusterName ensures cluster names are safe for use in cookie names.
func SanitizeClusterName(cluster string) string {
	// Only allow alphanumeric characters, hyphens, and underscores
	reg := regexp.MustCompile(`[^a-zA-Z0-9\-_]`)
	sanitized := reg.ReplaceAllString(cluster, "")

	// Limit length to prevent issues
	if len(sanitized) > 50 {
		sanitized = sanitized[:50]
	}

	return sanitized
}

// GetForwardedProto extracts and normalizes the client protocol from the X-Forwarded-Proto header.
// If multiple comma-separated protocols are present (e.g. from multi-tier reverse proxies),
// the first (client-facing) protocol is returned in lowercase. Only "http" and "https" are recognized.
func GetForwardedProto(r *http.Request) string {
	if r == nil {
		return ""
	}

	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		return ""
	}

	if before, _, found := strings.Cut(proto, ","); found {
		proto = before
	}

	proto = strings.ToLower(strings.TrimSpace(proto))
	if proto == "http" || proto == "https" {
		return proto
	}

	return ""
}

// IsSecureContext determines if we should use secure cookies.
func IsSecureContext(r *http.Request) bool {
	// Check if request came over HTTPS
	if r.TLS != nil {
		return true
	}

	// Check X-Forwarded-Proto header (for reverse proxies)
	if GetForwardedProto(r) == "https" {
		return true
	}

	// Check if we're in localhost/development (allow insecure for dev)
	host := r.Host
	if strings.HasPrefix(host, "localhost") || strings.HasPrefix(host, "127.0.0.1") {
		return false
	}

	return false
}

// SetTokenCookie sets an authentication cookie for a specific cluster.
func SetTokenCookie(
	w http.ResponseWriter,
	r *http.Request,
	cluster, token, baseURL string,
	sessionTTL int,
) error {
	// Validate inputs
	if cluster == "" || token == "" {
		return nil
	}

	sanitizedCluster := cookieClusterName(r, cluster)
	if sanitizedCluster == "" {
		return nil
	}

	secure := IsSecureContext(r)
	cookiePath := GetCookiePath(baseURL, cluster)

	chunks, sizeErr := tokenCookieChunks(w, r, cluster, "auth", token, baseURL, sessionTTL, boundedAuthTokenChunks)
	for i, chunk := range chunks {
		// G124: Secure is set from IsSecureContext so localhost development still works;
		// HttpOnly and SameSite are set unconditionally.
		cookie := &http.Cookie{ //nolint:gosec // Secure attribute is dynamically set based on request context
			Name:     fmt.Sprintf("headlamp-auth-%s.%d", sanitizedCluster, i),
			Value:    chunk,
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteStrictMode,
			Path:     cookiePath,
			MaxAge:   sessionTTL,
		}

		http.SetCookie(w, cookie)
	}

	if len(chunks) < maxAuthTokenChunks {
		clearAuthTokenChunkCookie(w, secure, cookiePath, sanitizedCluster, len(chunks))
	}

	return sizeErr
}

// GetTokenFromCookie retrieves an authentication cookie for a specific cluster.
func GetTokenFromCookie(r *http.Request, cluster string) (string, error) {
	sanitizedCluster := cookieClusterName(r, cluster)
	if sanitizedCluster == "" {
		return "", errors.New("invalid cluster name")
	}

	// check for chunked cookies first
	var token strings.Builder

	for i := 0; i < maxAuthTokenChunks; i++ {
		cookie, err := r.Cookie(fmt.Sprintf("headlamp-auth-%s.%d", sanitizedCluster, i))
		if err != nil {
			break
		}

		if cookie.Value == "" {
			break
		}

		token.WriteString(cookie.Value)
	}

	if token.Len() > 0 {
		return readCookieToken(r, cluster, "auth", token.String()), nil
	}

	return "", nil
}

// ClearTokenCookie clears an authentication cookie for a specific cluster.
func ClearTokenCookie(w http.ResponseWriter, r *http.Request, cluster, baseURL string) {
	clearObservedSessions(w, r, discoveredSessionPrefix(r, cluster, "auth"), baseURL)

	sanitizedCluster := cookieClusterName(r, cluster)
	if sanitizedCluster == "" {
		return
	}

	secure := IsSecureContext(r)

	// clear chunked cookies
	for i := 0; ; i++ {
		cookieName := fmt.Sprintf("headlamp-auth-%s.%d", sanitizedCluster, i)

		_, err := r.Cookie(cookieName)
		if err != nil {
			// No more cookies for this cluster
			break
		}

		// G124: Secure is set from IsSecureContext so localhost development still works;
		// HttpOnly and SameSite are set unconditionally.
		cookie := &http.Cookie{ //nolint:gosec // Secure attribute is dynamically set based on request context
			Name:     cookieName,
			Value:    "",
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteStrictMode,
			Path:     GetCookiePath(baseURL, cluster),
			MaxAge:   -1,
		}
		http.SetCookie(w, cookie)
	}
}

// ClearTokenCookieBounded invalidates chunk zero and clears other visible chunks.
// Unsent suffixes expire naturally and cannot be read without chunk zero.
func ClearTokenCookieBounded(w http.ResponseWriter, r *http.Request, cluster, baseURL string) {
	clearObservedSessions(w, r, discoveredSessionPrefix(r, cluster, "auth"), baseURL)

	sanitizedCluster := cookieClusterName(r, cluster)
	if sanitizedCluster == "" {
		return
	}

	secure := IsSecureContext(r)
	cookiePath := GetCookiePath(baseURL, cluster)

	for i := 0; i < maxAuthTokenChunks; i++ {
		if _, err := r.Cookie(fmt.Sprintf("headlamp-auth-%s.%d", sanitizedCluster, i)); i > 0 && err != nil {
			continue
		}

		clearAuthTokenChunkCookie(w, secure, cookiePath, sanitizedCluster, i)
	}
}

func boundedAuthTokenChunks(token string) ([]string, error) {
	chunks := splitToken(token, chunkSize)
	if len(chunks) > maxAuthTokenChunks {
		return nil, fmt.Errorf("authentication token exceeds the %d-cookie storage limit", maxAuthTokenChunks)
	}

	return chunks, nil
}

func clearAuthTokenChunkCookie(w http.ResponseWriter, secure bool, cookiePath, sanitizedCluster string, index int) {
	// G124: Secure is set from IsSecureContext so localhost development still works;
	// HttpOnly and SameSite are set unconditionally.
	cookie := &http.Cookie{ //nolint:gosec // Secure attribute is dynamically set based on request context
		Name:     fmt.Sprintf("headlamp-auth-%s.%d", sanitizedCluster, index),
		Value:    "",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		Path:     cookiePath,
		MaxAge:   -1,
	}
	http.SetCookie(w, cookie)
}

// SetIDTokenCookie sets an ID token cookie for a specific cluster.
// This is used for OIDC RP-initiated logout (id_token_hint parameter).
// The cookie path is restricted to the cluster's oidc-logout endpoint.
// Large ID tokens are split into chunks to avoid exceeding per-cookie size limits (~4KB).
func SetIDTokenCookie(
	w http.ResponseWriter,
	r *http.Request,
	cluster, idToken, baseURL string,
	sessionTTL int,
) error {
	if cluster == "" || idToken == "" {
		return nil
	}

	sanitizedCluster := cookieClusterName(r, cluster)
	if sanitizedCluster == "" {
		return nil
	}

	secure := IsSecureContext(r)

	cookiePath := GetOIDCLogoutCookiePath(baseURL, cluster)

	chunks, sizeErr := tokenCookieChunks(w, r, cluster, "id", idToken, baseURL, sessionTTL, boundedIDTokenChunks)

	for i, chunk := range chunks {
		// G124: Secure is set from IsSecureContext so localhost development still works;
		// HttpOnly and SameSite are set unconditionally.
		cookie := &http.Cookie{ //nolint:gosec // Secure attribute is dynamically set based on request context
			Name:     fmt.Sprintf("headlamp-id-token-%s.%d", sanitizedCluster, i),
			Value:    chunk,
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteStrictMode,
			Path:     cookiePath,
			MaxAge:   sessionTTL,
		}

		http.SetCookie(w, cookie)
	}

	if len(chunks) < maxIDTokenChunks {
		clearIDTokenChunkCookie(w, secure, cookiePath, sanitizedCluster, len(chunks))
	}

	return sizeErr
}

func boundedIDTokenChunks(idToken string) ([]string, error) {
	chunks := splitToken(idToken, chunkSize)
	if len(chunks) > maxIDTokenChunks {
		return nil, fmt.Errorf("ID token exceeds the %d-cookie storage limit", maxIDTokenChunks)
	}

	return chunks, nil
}

// GetIDTokenFromCookie retrieves the ID token cookie for a specific cluster.
// Supports chunked cookies for large ID tokens.
func GetIDTokenFromCookie(r *http.Request, cluster string) (string, error) {
	sanitizedCluster := cookieClusterName(r, cluster)
	if sanitizedCluster == "" {
		return "", errors.New("invalid cluster name")
	}

	// Check for chunked cookies
	var idToken strings.Builder

	for i := 0; i < maxIDTokenChunks; i++ {
		cookie, err := r.Cookie(fmt.Sprintf("headlamp-id-token-%s.%d", sanitizedCluster, i))
		if err != nil {
			break
		}

		if cookie.Value == "" {
			break
		}

		idToken.WriteString(cookie.Value)
	}

	if token := readCookieToken(r, cluster, "id", idToken.String()); token != "" {
		return token, nil
	}

	return "", http.ErrNoCookie
}

// ClearIDTokenCookie clears the ID token cookie for a specific cluster.
// Chunk zero is always invalidated, even when the request path hides the cookies.
func ClearIDTokenCookie(w http.ResponseWriter, r *http.Request, cluster, baseURL string) {
	clearObservedSessions(w, r, discoveredSessionPrefix(r, cluster, "id"), baseURL)

	sanitizedCluster := cookieClusterName(r, cluster)
	if sanitizedCluster == "" {
		return
	}

	secure := IsSecureContext(r)

	cookiePath := GetOIDCLogoutCookiePath(baseURL, cluster)

	for index := 0; index < maxIDTokenChunks; index++ {
		if _, err := r.Cookie(fmt.Sprintf("headlamp-id-token-%s.%d", sanitizedCluster, index)); index > 0 && err != nil {
			continue
		}

		clearIDTokenChunkCookie(w, secure, cookiePath, sanitizedCluster, index)
	}
}

func clearIDTokenChunkCookie(
	w http.ResponseWriter,
	secure bool,
	cookiePath, sanitizedCluster string,
	index int,
) {
	// G124: Secure is set from IsSecureContext so localhost development still works;
	// HttpOnly and SameSite are set unconditionally.
	cookie := &http.Cookie{ //nolint:gosec // Secure attribute is dynamically set based on request context
		Name:     fmt.Sprintf("headlamp-id-token-%s.%d", sanitizedCluster, index),
		Value:    "",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		Path:     cookiePath,
		MaxAge:   -1,
	}
	http.SetCookie(w, cookie)
}

// splitToken splits a token into chunks of a given size.
func splitToken(token string, size int) []string {
	var chunks []string

	for i := 0; i < len(token); i += size {
		end := i + size
		if end > len(token) {
			end = len(token)
		}

		chunks = append(chunks, token[i:end])
	}

	return chunks
}
