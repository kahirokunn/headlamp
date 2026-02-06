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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd/api"

	"github.com/kubernetes-sigs/headlamp/backend/pkg/kubeconfig"
	apisv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
)

func Test_contextNameForClusterProfile(t *testing.T) {
	tests := []struct {
		name string
		cp   *apisv1alpha1.ClusterProfile
		want string
	}{
		{
			name: "simple namespace and name",
			cp: &apisv1alpha1.ClusterProfile{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "spoke-1"},
			},
			want: "cluster-inventory-default-spoke-1",
		},
		{
			name: "hyphenated namespace and name",
			cp: &apisv1alpha1.ClusterProfile{
				ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "my-spoke-cluster"},
			},
			want: "cluster-inventory-kube-system-my-spoke-cluster",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := contextNameForClusterProfile(tt.cp)
			if got != tt.want {
				t.Errorf("contextNameForClusterProfile() = %q, want %q", got, tt.want)
			}
		})
	}
}

func Test_makeDNSFriendly(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"slash to double dash", "a/b", "a--b"},
		{"space to double underscore", "a b", "a__b"},
		{"unchanged", "simple", "simple"},
		{"mixed", "a/b c", "a--b__c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := makeDNSFriendly(tt.in)
			if got != tt.want {
				t.Errorf("makeDNSFriendly(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func Test_restConfigToContext(t *testing.T) {
	t.Run("with ExecProvider", func(t *testing.T) {
		restConfig := &rest.Config{
			Host: "https://example.com:6443",
			TLSClientConfig: rest.TLSClientConfig{
				CAData:   []byte("fake-ca-data"),
				Insecure: false,
			},
			// Command is a placeholder; in production this would be the path to an exec plugin
			// (e.g. kubeconfig-secretreader-plugin) configured in the provider file.
			ExecProvider: &api.ExecConfig{
				APIVersion:         "client.authentication.k8s.io/v1",
				Command:            "/usr/local/bin/kubeconfig-secretreader-plugin",
				ProvideClusterInfo: true,
			},
		}
		ctx, err := restConfigToContext(restConfig, "test-ctx", "ns/name")
		if err != nil {
			t.Fatalf("restConfigToContext: %v", err)
		}
		if ctx.Name != "test-ctx" {
			t.Errorf("Name = %q, want test-ctx", ctx.Name)
		}
		if ctx.Cluster.Server != "https://example.com:6443" {
			t.Errorf("Cluster.Server = %q", ctx.Cluster.Server)
		}
		if string(ctx.Cluster.CertificateAuthorityData) != "fake-ca-data" {
			t.Errorf("Cluster.CAData mismatch")
		}
		if ctx.AuthInfo.Exec == nil {
			t.Error("AuthInfo.Exec is nil")
		}
		if ctx.AuthInfo.Exec.Command != "/usr/local/bin/kubeconfig-secretreader-plugin" {
			t.Errorf("Exec.Command = %q", ctx.AuthInfo.Exec.Command)
		}
		if ctx.Source != kubeconfig.ClusterInventory {
			t.Errorf("Source = %v, want ClusterInventory", ctx.Source)
		}
		if ctx.ClusterID != "cluster-inventory/ns/name" {
			t.Errorf("ClusterID = %q", ctx.ClusterID)
		}
	})

	t.Run("with BearerToken only", func(t *testing.T) {
		restConfig := &rest.Config{
			Host:        "https://example.com",
			BearerToken: "secret-token",
		}
		ctx, err := restConfigToContext(restConfig, "token-ctx", "default/c1")
		if err != nil {
			t.Fatalf("restConfigToContext: %v", err)
		}
		if ctx.AuthInfo.Token != "secret-token" {
			t.Errorf("AuthInfo.Token = %q", ctx.AuthInfo.Token)
		}
		if ctx.AuthInfo.Exec != nil {
			t.Error("AuthInfo.Exec should be nil when only BearerToken is set")
		}
	})
}

func TestWatchAndSync_emptyProviderFile_returnsEarly(t *testing.T) {
	store := kubeconfig.NewContextStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately so WatchAndSync returns quickly if it proceeds
	WatchAndSync(ctx, store, "")
	contexts, _ := store.GetContexts()
	if len(contexts) != 0 {
		t.Errorf("expected no contexts when provider file is empty, got %d", len(contexts))
	}
}

func TestWatchAndSync_invalidProviderFile_returnsEarly(t *testing.T) {
	store := kubeconfig.NewContextStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	WatchAndSync(ctx, store, "/nonexistent/path/provider-config.json")
	contexts, _ := store.GetContexts()
	if len(contexts) != 0 {
		t.Errorf("expected no contexts when provider file is invalid, got %d", len(contexts))
	}
}
