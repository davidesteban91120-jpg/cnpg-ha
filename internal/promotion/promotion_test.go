/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package promotion

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newSchemeWithCNPG returns a runtime.Scheme that knows about core/v1 +
// the CNPG Cluster GVK as Unstructured. Required so the fake client can
// store and serve both Services and CNPG Clusters.
func newSchemeWithCNPG(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	scheme.AddKnownTypeWithName(CNPGClusterGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(
		CNPGClusterGVK.GroupVersion().WithKind("ClusterList"),
		&unstructured.UnstructuredList{},
	)
	return scheme
}

// makeCluster builds an Unstructured CNPG Cluster with the given replica
// flag and starting annotations.
func makeCluster(ns, name string, replicaEnabled *bool, annotations map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(CNPGClusterGVK)
	u.SetNamespace(ns)
	u.SetName(name)
	if annotations != nil {
		u.SetAnnotations(annotations)
	}
	if replicaEnabled != nil {
		_ = unstructured.SetNestedField(u.Object, *replicaEnabled, "spec", "replica", "enabled")
	}
	return u
}

func makeService(ns, name string, annotations map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   ns,
			Name:        name,
			Annotations: annotations,
		},
	}
}

func TestFence(t *testing.T) {
	ctx := context.Background()
	scheme := newSchemeWithCNPG(t)

	tests := []struct {
		name        string
		annotations map[string]string
		wantValue   string
		wantNoOp    bool
	}{
		{
			name:        "no annotation → sets fenced=[\"*\"]",
			annotations: nil,
			wantValue:   FencedAllInstances,
		},
		{
			name:        "unrelated annotation present → still sets fence and keeps the other",
			annotations: map[string]string{"foo": "bar"},
			wantValue:   FencedAllInstances,
		},
		{
			name:        "already fenced → no-op",
			annotations: map[string]string{AnnotationFenced: FencedAllInstances},
			wantValue:   FencedAllInstances,
			wantNoOp:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := makeCluster("db", "pg-prod", nil, tt.annotations)
			cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

			if err := Fence(ctx, cli, Ref{Namespace: "db", Name: "pg-prod"}); err != nil {
				t.Fatalf("Fence: %v", err)
			}

			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(CNPGClusterGVK)
			if err := cli.Get(ctx, clientKey("db", "pg-prod"), got); err != nil {
				t.Fatalf("get after fence: %v", err)
			}
			if v := got.GetAnnotations()[AnnotationFenced]; v != tt.wantValue {
				t.Errorf("fence annotation: got %q, want %q", v, tt.wantValue)
			}
			if tt.annotations != nil {
				if v, ok := tt.annotations["foo"]; ok && got.GetAnnotations()["foo"] != v {
					t.Errorf("unrelated annotation lost")
				}
			}
		})
	}
}

func TestFenceClusterNotFound(t *testing.T) {
	scheme := newSchemeWithCNPG(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).Build()

	err := Fence(context.Background(), cli, Ref{Namespace: "db", Name: "missing"})
	if err == nil {
		t.Fatalf("Fence on missing cluster: want error, got nil")
	}
}

func TestPromote(t *testing.T) {
	ctx := context.Background()
	scheme := newSchemeWithCNPG(t)
	tBool, fBool := true, false

	tests := []struct {
		name           string
		replicaEnabled *bool
		wantEnabled    *bool // nil = expect field absent
	}{
		{name: "replica=true → flipped to false", replicaEnabled: &tBool, wantEnabled: &fBool},
		{name: "replica=false → no-op", replicaEnabled: &fBool, wantEnabled: &fBool},
		{name: "spec.replica absent → no-op (already primary)", replicaEnabled: nil, wantEnabled: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := makeCluster("db", "pg-prod", tt.replicaEnabled, nil)
			cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

			if err := Promote(ctx, cli, Ref{Namespace: "db", Name: "pg-prod"}); err != nil {
				t.Fatalf("Promote: %v", err)
			}

			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(CNPGClusterGVK)
			if err := cli.Get(ctx, clientKey("db", "pg-prod"), got); err != nil {
				t.Fatalf("get after promote: %v", err)
			}
			enabled, found, _ := unstructured.NestedBool(got.Object, "spec", "replica", "enabled")
			switch {
			case tt.wantEnabled == nil && found:
				t.Errorf("expected spec.replica.enabled absent, got %v", enabled)
			case tt.wantEnabled != nil && !found:
				t.Errorf("expected spec.replica.enabled=%v, got absent", *tt.wantEnabled)
			case tt.wantEnabled != nil && enabled != *tt.wantEnabled:
				t.Errorf("spec.replica.enabled: got %v, want %v", enabled, *tt.wantEnabled)
			}
		})
	}
}

func TestFlipCiliumService(t *testing.T) {
	ctx := context.Background()
	scheme := newSchemeWithCNPG(t)

	tests := []struct {
		name          string
		startAnn      map[string]string
		role          ServiceRole
		wantGlobal    string
		wantAffinity  string
		wantUnchanged bool
	}{
		{
			name:         "no annotations → RoleLocal sets global+local",
			startAnn:     nil,
			role:         RoleLocal,
			wantGlobal:   "true",
			wantAffinity: "local",
		},
		{
			name:         "local → flipped to remote",
			startAnn:     map[string]string{AnnotationCiliumGlobal: "true", AnnotationCiliumAffinity: "local"},
			role:         RoleRemote,
			wantGlobal:   "true",
			wantAffinity: "remote",
		},
		{
			name:          "already local + RoleLocal → no-op",
			startAnn:      map[string]string{AnnotationCiliumGlobal: "true", AnnotationCiliumAffinity: "local"},
			role:          RoleLocal,
			wantGlobal:    "true",
			wantAffinity:  "local",
			wantUnchanged: true,
		},
		{
			name:         "unrelated annotation preserved",
			startAnn:     map[string]string{"foo": "bar"},
			role:         RoleLocal,
			wantGlobal:   "true",
			wantAffinity: "local",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := makeService("db", "pg-prod-rw", tt.startAnn)
			cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).Build()

			if err := FlipCiliumService(ctx, cli, Ref{Namespace: "db", Name: "pg-prod"}, tt.role); err != nil {
				t.Fatalf("FlipCiliumService: %v", err)
			}

			got := &corev1.Service{}
			if err := cli.Get(ctx, clientKey("db", "pg-prod-rw"), got); err != nil {
				t.Fatalf("get after flip: %v", err)
			}
			if got.Annotations[AnnotationCiliumGlobal] != tt.wantGlobal {
				t.Errorf("global: got %q, want %q", got.Annotations[AnnotationCiliumGlobal], tt.wantGlobal)
			}
			if got.Annotations[AnnotationCiliumAffinity] != tt.wantAffinity {
				t.Errorf("affinity: got %q, want %q", got.Annotations[AnnotationCiliumAffinity], tt.wantAffinity)
			}
			if v, ok := tt.startAnn["foo"]; ok && got.Annotations["foo"] != v {
				t.Errorf("unrelated annotation lost")
			}
		})
	}
}

func TestFlipCiliumServiceMissingService(t *testing.T) {
	scheme := newSchemeWithCNPG(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).Build()

	err := FlipCiliumService(context.Background(), cli, Ref{Namespace: "db", Name: "pg-prod"}, RoleLocal)
	if err == nil {
		t.Fatalf("expected error when <cluster>-rw Service is missing")
	}
}

// clientKey is a tiny helper to keep test calls compact.
func clientKey(ns, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: ns, Name: name}
}

// makeReplicaCluster builds a CNPG replica Cluster named "pg-prod" with a
// spec.replica block and one externalCluster "src" pointing at host.
func makeReplicaCluster(ns, host string, enabled bool) *unstructured.Unstructured {
	const src = "src"
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(CNPGClusterGVK)
	u.SetNamespace(ns)
	u.SetName("pg-prod")
	_ = unstructured.SetNestedField(u.Object, enabled, "spec", "replica", "enabled")
	_ = unstructured.SetNestedField(u.Object, src, "spec", "replica", "source")
	_ = unstructured.SetNestedSlice(u.Object, []any{
		map[string]any{
			"name": src,
			"connectionParameters": map[string]any{
				"host": host,
				"user": "streaming_replica",
			},
		},
	}, "spec", "externalClusters")
	return u
}

func extHost(t *testing.T, u *unstructured.Unstructured) string {
	t.Helper()
	externals, _, _ := unstructured.NestedSlice(u.Object, "spec", "externalClusters")
	if len(externals) == 0 {
		t.Fatalf("no externalClusters")
	}
	m := externals[0].(map[string]any)
	conn := m["connectionParameters"].(map[string]any)
	return conn["host"].(string)
}

func TestReconfigure(t *testing.T) {
	ctx := context.Background()
	scheme := newSchemeWithCNPG(t)
	const newHost = "pg-prod-rw.site-b.svc.cluster.local"

	t.Run("replica drifted host → repointed + enabled stays true", func(t *testing.T) {
		c := makeReplicaCluster("site-c", "pg-prod-rw.site-a.svc.cluster.local", true)
		cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c).Build()

		if err := Reconfigure(ctx, cli, Ref{Namespace: "site-c", Name: "pg-prod"}, newHost); err != nil {
			t.Fatalf("Reconfigure: %v", err)
		}
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(CNPGClusterGVK)
		if err := cli.Get(ctx, clientKey("site-c", "pg-prod"), got); err != nil {
			t.Fatalf("get: %v", err)
		}
		if h := extHost(t, got); h != newHost {
			t.Errorf("host: got %q, want %q", h, newHost)
		}
		enabled, _, _ := unstructured.NestedBool(got.Object, "spec", "replica", "enabled")
		if !enabled {
			t.Errorf("spec.replica.enabled should be true")
		}
	})

	t.Run("former primary (replica.enabled=false) → demoted to replica of newHost", func(t *testing.T) {
		c := makeReplicaCluster("site-a", "old", false)
		cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c).Build()

		if err := Reconfigure(ctx, cli, Ref{Namespace: "site-a", Name: "pg-prod"}, newHost); err != nil {
			t.Fatalf("Reconfigure: %v", err)
		}
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(CNPGClusterGVK)
		_ = cli.Get(ctx, clientKey("site-a", "pg-prod"), got)
		enabled, _, _ := unstructured.NestedBool(got.Object, "spec", "replica", "enabled")
		if !enabled {
			t.Errorf("former primary should be flipped to replica (enabled=true)")
		}
		if h := extHost(t, got); h != newHost {
			t.Errorf("host: got %q, want %q", h, newHost)
		}
	})

	t.Run("already pointing at newHost + enabled → no-op", func(t *testing.T) {
		c := makeReplicaCluster("site-c", newHost, true)
		cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c).Build()
		rvBefore := c.GetResourceVersion()

		if err := Reconfigure(ctx, cli, Ref{Namespace: "site-c", Name: "pg-prod"}, newHost); err != nil {
			t.Fatalf("Reconfigure: %v", err)
		}
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(CNPGClusterGVK)
		_ = cli.Get(ctx, clientKey("site-c", "pg-prod"), got)
		if got.GetResourceVersion() != rvBefore {
			t.Errorf("expected no Update (idempotent), resourceVersion changed %s → %s", rvBefore, got.GetResourceVersion())
		}
	})

	t.Run("empty source host → error", func(t *testing.T) {
		c := makeReplicaCluster("site-c", "x", true)
		cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c).Build()
		if err := Reconfigure(ctx, cli, Ref{Namespace: "site-c", Name: "pg-prod"}, ""); err == nil {
			t.Fatalf("want error on empty source host")
		}
	})

	t.Run("no externalClusters → error", func(t *testing.T) {
		c := makeCluster("site-c", "pg-prod", boolp(true), nil)
		cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c).Build()
		if err := Reconfigure(ctx, cli, Ref{Namespace: "site-c", Name: "pg-prod"}, newHost); err == nil {
			t.Fatalf("want error when no externalClusters to repoint")
		}
	})
}

func boolp(b bool) *bool { return &b }

func TestExternalClusterIndex(t *testing.T) {
	ext := func(name string) any {
		return map[string]any{"name": name, "connectionParameters": map[string]any{"host": "h"}}
	}

	tests := []struct {
		name      string
		externals []any
		source    string
		wantIdx   int
		wantErr   bool
	}{
		{"empty source + single external → 0", []any{ext("only")}, "", 0, false},
		{"empty source + several externals → ambiguous error", []any{ext("a"), ext("b")}, "", 0, true},
		{"named source found", []any{ext("a"), ext("b")}, "b", 1, false},
		{"named source not found → error", []any{ext("a")}, "missing", 0, true},
		{"non-object entries are skipped, still not found → error", []any{"junk", ext("a")}, "z", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx, err := externalClusterIndex(tt.externals, tt.source)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got idx=%d nil", idx)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if idx != tt.wantIdx {
				t.Errorf("idx: got %d, want %d", idx, tt.wantIdx)
			}
		})
	}
}

func TestPromoteClusterNotFound(t *testing.T) {
	scheme := newSchemeWithCNPG(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).Build()
	if err := Promote(context.Background(), cli, Ref{Namespace: "db", Name: "missing"}); err == nil {
		t.Fatalf("Promote on missing cluster: want error, got nil")
	}
}

func TestReconfigureClusterNotFound(t *testing.T) {
	scheme := newSchemeWithCNPG(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).Build()
	err := Reconfigure(context.Background(), cli, Ref{Namespace: "db", Name: "missing"}, "host")
	if err == nil {
		t.Fatalf("Reconfigure on missing cluster: want error, got nil")
	}
}

func TestReconfigureAmbiguousSource(t *testing.T) {
	ctx := context.Background()
	scheme := newSchemeWithCNPG(t)

	// spec.replica.source unset + two externalClusters → ambiguous.
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(CNPGClusterGVK)
	u.SetNamespace("site-c")
	u.SetName("pg-prod")
	_ = unstructured.SetNestedField(u.Object, true, "spec", "replica", "enabled")
	_ = unstructured.SetNestedSlice(u.Object, []any{
		map[string]any{"name": "a", "connectionParameters": map[string]any{"host": "x"}},
		map[string]any{"name": "b", "connectionParameters": map[string]any{"host": "y"}},
	}, "spec", "externalClusters")

	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(u).Build()
	if err := Reconfigure(ctx, cli, Ref{Namespace: "site-c", Name: "pg-prod"}, "newhost"); err == nil {
		t.Fatalf("want ambiguous-source error")
	}
}
