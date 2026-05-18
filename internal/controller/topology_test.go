/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	k8sevents "k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	hav1alpha1 "github.com/davidesteban/cnpg-ha/api/v1alpha1"
	"github.com/davidesteban/cnpg-ha/internal/promotion"
	"github.com/davidesteban/cnpg-ha/internal/remoteclient"
)

const siteAEndpoint = "pg-prod-rw.site-a.svc.cluster.local"

// cnpgReplica builds a site-b CNPG Cluster with a spec.replica block + one
// externalCluster "src" pointing at host. enabled toggles primary/replica.
func cnpgReplica(host string, enabled bool) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(cnpgClusterGVK)
	u.SetNamespace("site-b")
	u.SetName("pg-prod")
	_ = unstructured.SetNestedField(u.Object, enabled, "spec", "replica", "enabled")
	_ = unstructured.SetNestedField(u.Object, "src", "spec", "replica", "source")
	_ = unstructured.SetNestedSlice(u.Object, []any{
		map[string]any{
			"name":                 "src",
			"connectionParameters": map[string]any{"host": host},
		},
	}, "spec", "externalClusters")
	_ = unstructured.SetNestedField(u.Object, int64(1), "status", "readyInstances")
	_ = unstructured.SetNestedField(u.Object, "Cluster in healthy state", "status", "phase")
	return u
}

func extHostOf(t *testing.T, u *unstructured.Unstructured) string {
	t.Helper()
	ext, _, _ := unstructured.NestedSlice(u.Object, "spec", "externalClusters")
	return ext[0].(map[string]any)["connectionParameters"].(map[string]any)["host"].(string)
}

func TestReconcileReplicaTopology(t *testing.T) {
	ctx := context.Background()

	secretRef := corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "kc"},
		Key:                  "kubeconfig",
	}

	newHA := func(rejoin hav1alpha1.RejoinPolicy, endpoint string) *hav1alpha1.HACluster {
		return &hav1alpha1.HACluster{
			ObjectMeta: metav1.ObjectMeta{Name: "prod-db", Namespace: "ops"},
			Spec: hav1alpha1.HAClusterSpec{
				Primary: hav1alpha1.PrimarySite{
					Name:                "site-a",
					ClusterRef:          hav1alpha1.ClusterRef{Name: "pg-prod", Namespace: "site-a"},
					ReplicationEndpoint: endpoint,
				},
				Replicas: []hav1alpha1.ReplicaSite{{
					Name:                "site-b",
					KubeconfigSecretRef: secretRef,
					ClusterRef:          hav1alpha1.ClusterRef{Name: "pg-prod", Namespace: "site-b"},
					ReplicationEndpoint: "pg-prod-rw.site-b.svc.cluster.local",
				}},
				Failover: hav1alpha1.FailoverSpec{Mode: hav1alpha1.FailoverModeManual, RejoinPolicy: rejoin},
			},
			Status: hav1alpha1.HAClusterStatus{CurrentPrimarySite: "site-a"},
		}
	}

	t.Run("surviving replica re-pointed to current primary endpoint", func(t *testing.T) {
		scheme := buildPromoteScheme(t)
		ha := newHA(hav1alpha1.RejoinPolicyManual, siteAEndpoint)

		siteACNPG := newCNPGClusterForTest("site-a", nil, 1) // primary, ready
		hub := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(ha, siteACNPG).
			WithStatusSubresource(&hav1alpha1.HACluster{}).Build()

		siteBCNPG := cnpgReplica("stale-old-host", true) // replica, drifted
		remote := fake.NewClientBuilder().WithScheme(scheme).WithObjects(siteBCNPG).Build()

		cache := remoteclient.NewCache(scheme)
		cache.PutForTest("ops", secretRef, remote)
		r := &HAClusterReconciler{Client: hub, Scheme: scheme, RemoteClients: cache, Recorder: k8sevents.NewFakeRecorder(20)}

		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "prod-db", Namespace: "ops"}}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(cnpgClusterGVK)
		if err := remote.Get(ctx, types.NamespacedName{Namespace: "site-b", Name: "pg-prod"}, got); err != nil {
			t.Fatalf("get site-b: %v", err)
		}
		if h := extHostOf(t, got); h != siteAEndpoint {
			t.Errorf("site-b externalCluster host: got %q, want %q", h, siteAEndpoint)
		}
	})

	t.Run("returning primary fenced under RejoinPolicy=Manual", func(t *testing.T) {
		scheme := buildPromoteScheme(t)
		ha := newHA(hav1alpha1.RejoinPolicyManual, siteAEndpoint)

		siteACNPG := newCNPGClusterForTest("site-a", nil, 1)
		hub := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(ha, siteACNPG).
			WithStatusSubresource(&hav1alpha1.HACluster{}).Build()

		// site-b came back as a PRIMARY (replica.enabled=false) + ready.
		siteBCNPG := cnpgReplica("whatever", false)
		remote := fake.NewClientBuilder().WithScheme(scheme).WithObjects(siteBCNPG).Build()

		cache := remoteclient.NewCache(scheme)
		cache.PutForTest("ops", secretRef, remote)
		rec := k8sevents.NewFakeRecorder(20)
		r := &HAClusterReconciler{Client: hub, Scheme: scheme, RemoteClients: cache, Recorder: rec}

		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "prod-db", Namespace: "ops"}}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(cnpgClusterGVK)
		_ = remote.Get(ctx, types.NamespacedName{Namespace: "site-b", Name: "pg-prod"}, got)
		if got.GetAnnotations()[promotion.AnnotationFenced] != promotion.FencedAllInstances {
			t.Errorf("returning primary should be fenced; annotations=%v", got.GetAnnotations())
		}
		if len(eventsContaining(drainEvents(rec), eventReasonRejoinFenced)) == 0 {
			t.Errorf("expected RejoinFenced event")
		}
	})

	t.Run("returning primary rebuilt as replica under RejoinPolicy=AutoReplica", func(t *testing.T) {
		scheme := buildPromoteScheme(t)
		ha := newHA(hav1alpha1.RejoinPolicyAutoReplica, siteAEndpoint)

		siteACNPG := newCNPGClusterForTest("site-a", nil, 1)
		hub := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(ha, siteACNPG).
			WithStatusSubresource(&hav1alpha1.HACluster{}).Build()

		siteBCNPG := cnpgReplica("stale", false) // primary mode, has externalClusters
		remote := fake.NewClientBuilder().WithScheme(scheme).WithObjects(siteBCNPG).Build()

		cache := remoteclient.NewCache(scheme)
		cache.PutForTest("ops", secretRef, remote)
		rec := k8sevents.NewFakeRecorder(20)
		r := &HAClusterReconciler{Client: hub, Scheme: scheme, RemoteClients: cache, Recorder: rec}

		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "prod-db", Namespace: "ops"}}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(cnpgClusterGVK)
		_ = remote.Get(ctx, types.NamespacedName{Namespace: "site-b", Name: "pg-prod"}, got)
		enabled, _, _ := unstructured.NestedBool(got.Object, "spec", "replica", "enabled")
		if !enabled {
			t.Errorf("AutoReplica: site-b should be flipped to replica (enabled=true)")
		}
		if h := extHostOf(t, got); h != siteAEndpoint {
			t.Errorf("AutoReplica: site-b host got %q, want %q", h, siteAEndpoint)
		}
		if len(eventsContaining(drainEvents(rec), eventReasonRejoinReconfigured)) == 0 {
			t.Errorf("expected RejoinReconfigured event")
		}
	})

	t.Run("no ReplicationEndpoint → topology untouched", func(t *testing.T) {
		scheme := buildPromoteScheme(t)
		ha := newHA(hav1alpha1.RejoinPolicyManual, "") // no endpoint

		siteACNPG := newCNPGClusterForTest("site-a", nil, 1)
		hub := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(ha, siteACNPG).
			WithStatusSubresource(&hav1alpha1.HACluster{}).Build()

		siteBCNPG := cnpgReplica("untouched-host", true)
		remote := fake.NewClientBuilder().WithScheme(scheme).WithObjects(siteBCNPG).Build()

		cache := remoteclient.NewCache(scheme)
		cache.PutForTest("ops", secretRef, remote)
		r := &HAClusterReconciler{Client: hub, Scheme: scheme, RemoteClients: cache, Recorder: k8sevents.NewFakeRecorder(20)}

		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "prod-db", Namespace: "ops"}}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(cnpgClusterGVK)
		_ = remote.Get(ctx, types.NamespacedName{Namespace: "site-b", Name: "pg-prod"}, got)
		if h := extHostOf(t, got); h != "untouched-host" {
			t.Errorf("host should be untouched without ReplicationEndpoint; got %q", h)
		}
	})
}
