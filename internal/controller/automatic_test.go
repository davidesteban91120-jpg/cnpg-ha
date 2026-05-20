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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	hav1alpha1 "github.com/davidesteban/cnpg-ha/api/v1alpha1"
	"github.com/davidesteban/cnpg-ha/internal/promotion"
	"github.com/davidesteban/cnpg-ha/internal/remoteclient"
)

const (
	siteB = "site-b"
	siteC = "site-c"
)

// autoFixture wires a reconciler in Automatic mode with site-a as the
// declared primary and site-b/site-c as replicas. The hub omits site-a's
// CNPG Cluster when primaryDown is true (observePrimary → unreachable).
type autoFixture struct {
	r     *HAClusterReconciler
	hub   client.Client
	rem   client.Client
	rec   *k8sevents.FakeRecorder
	haKey types.NamespacedName
}

func newAutoFixture(t *testing.T, threshold int32, primaryDown bool, replicas []*unstructured.Unstructured, repSpec []hav1alpha1.ReplicaSite) *autoFixture {
	t.Helper()
	scheme := buildPromoteScheme(t)
	secretRef := corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "kc"},
		Key:                  "kubeconfig",
	}
	if repSpec == nil {
		repSpec = []hav1alpha1.ReplicaSite{{
			Name:                siteB,
			KubeconfigSecretRef: secretRef,
			ClusterRef:          hav1alpha1.ClusterRef{Name: "pg-prod", Namespace: siteB},
			ReplicationEndpoint: "pg-prod-rw.site-b.svc.cluster.local",
		}}
	}
	ha := &hav1alpha1.HACluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-db", Namespace: "ops"},
		Spec: hav1alpha1.HAClusterSpec{
			Primary: hav1alpha1.PrimarySite{
				Name:                "site-a",
				ClusterRef:          hav1alpha1.ClusterRef{Name: "pg-prod", Namespace: "site-a"},
				ReplicationEndpoint: siteAEndpoint,
			},
			Replicas: repSpec,
			Failover: hav1alpha1.FailoverSpec{
				Mode:             hav1alpha1.FailoverModeAutomatic,
				FailureThreshold: threshold,
			},
		},
	}

	hubObjs := []client.Object{ha}
	if !primaryDown {
		hubObjs = append(hubObjs, newCNPGClusterForTest("site-a", nil, 1)) // primary, ready
	}
	hub := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(hubObjs...).
		WithStatusSubresource(&hav1alpha1.HACluster{}).Build()

	remObjs := make([]client.Object, 0, 2+len(replicas))
	// CNPG-managed -rw Services that FlipCiliumService targets on promotion.
	remObjs = append(remObjs, newServiceForTest(siteB), newServiceForTest(siteC))
	for _, o := range replicas {
		remObjs = append(remObjs, o)
	}
	rem := fake.NewClientBuilder().WithScheme(scheme).WithObjects(remObjs...).Build()

	cache := remoteclient.NewCache(scheme)
	cache.PutForTest("ops", secretRef, rem)
	rec := k8sevents.NewFakeRecorder(30)
	return &autoFixture{
		r:     &HAClusterReconciler{Client: hub, Scheme: scheme, RemoteClients: cache, Recorder: rec},
		hub:   hub,
		rem:   rem,
		rec:   rec,
		haKey: types.NamespacedName{Name: "prod-db", Namespace: "ops"},
	}
}

func (f *autoFixture) reconcile(t *testing.T) {
	t.Helper()
	if _, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: f.haKey}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func (f *autoFixture) currentPrimary(t *testing.T) string {
	t.Helper()
	ha := &hav1alpha1.HACluster{}
	if err := f.hub.Get(context.Background(), f.haKey, ha); err != nil {
		t.Fatalf("get HACluster: %v", err)
	}
	return ha.Status.CurrentPrimarySite
}

// healthyReplica = a site-b CNPG replica (reachable + ready) that already
// streams from site-a, with the externalClusters block topology
// reconciliation needs (so a steady-state Reconfigure is a no-op).
func healthyReplica() *unstructured.Unstructured {
	return cnpgReplica(siteAEndpoint, true)
}

func TestAutomaticFailover_UsesStatusCurrentPrimaryAsOldPrimary(t *testing.T) {
	ctx := context.Background()
	secretRef := corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "kc"},
		Key:                  "kubeconfig",
	}
	siteBEndpoint := "pg-prod-rw.site-b.svc.cluster.local"
	siteCEndpoint := "pg-prod-rw.site-c.svc.cluster.local"
	repSpec := []hav1alpha1.ReplicaSite{
		{
			Name:                siteB,
			KubeconfigSecretRef: secretRef,
			ClusterRef:          hav1alpha1.ClusterRef{Name: "pg-prod", Namespace: siteB},
			ReplicationEndpoint: siteBEndpoint,
		},
		{
			Name:                siteC,
			KubeconfigSecretRef: secretRef,
			ClusterRef:          hav1alpha1.ClusterRef{Name: "pg-prod", Namespace: siteC},
			ReplicationEndpoint: siteCEndpoint,
		},
	}

	// site-b is the previously promoted primary, now unhealthy. site-c is a
	// healthy replica already following site-b, so topology reconcile is a
	// no-op before the threshold is reached.
	tb := true
	siteBPrimaryUnhealthy := newCNPGClusterForTest(siteB, nil, 0)
	siteCReplica := newCNPGClusterForTest(siteC, &tb, 1)
	if err := unstructured.SetNestedField(siteCReplica.Object, "src", "spec", "replica", "source"); err != nil {
		t.Fatalf("set replica source: %v", err)
	}
	if err := unstructured.SetNestedSlice(siteCReplica.Object, []any{
		map[string]any{
			"name":                 "src",
			"connectionParameters": map[string]any{"host": siteBEndpoint},
		},
	}, "spec", "externalClusters"); err != nil {
		t.Fatalf("set externalClusters: %v", err)
	}

	f := newAutoFixture(t, 2, true, []*unstructured.Unstructured{siteBPrimaryUnhealthy, siteCReplica}, repSpec)
	ha := &hav1alpha1.HACluster{}
	if err := f.hub.Get(ctx, f.haKey, ha); err != nil {
		t.Fatalf("get HACluster: %v", err)
	}
	ha.Status.CurrentPrimarySite = siteB
	if err := f.hub.Status().Update(ctx, ha); err != nil {
		t.Fatalf("status update: %v", err)
	}

	f.reconcile(t) // 1/2: remember site-b, don't fall back to spec.primary.
	if cp := f.currentPrimary(t); cp != siteB {
		t.Fatalf("currentPrimary after first failed probe: got %q, want %q", cp, siteB)
	}
	f.reconcile(t) // 2/2: promote site-c, demote/fence site-b.

	if cp := f.currentPrimary(t); cp != siteC {
		t.Fatalf("currentPrimary after failover: got %q, want %q", cp, siteC)
	}

	oldPrimary := &unstructured.Unstructured{}
	oldPrimary.SetGroupVersionKind(cnpgClusterGVK)
	if err := f.rem.Get(ctx, types.NamespacedName{Namespace: siteB, Name: "pg-prod"}, oldPrimary); err != nil {
		t.Fatalf("get old primary: %v", err)
	}
	if oldPrimary.GetAnnotations()[promotion.AnnotationFenced] != promotion.FencedAllInstances {
		t.Errorf("old current primary site-b should be fenced; annotations=%v", oldPrimary.GetAnnotations())
	}

	newPrimary := &unstructured.Unstructured{}
	newPrimary.SetGroupVersionKind(cnpgClusterGVK)
	if err := f.rem.Get(ctx, types.NamespacedName{Namespace: siteC, Name: "pg-prod"}, newPrimary); err != nil {
		t.Fatalf("get new primary: %v", err)
	}
	enabled, _, _ := unstructured.NestedBool(newPrimary.Object, "spec", "replica", "enabled")
	if enabled {
		t.Errorf("site-c should be promoted (spec.replica.enabled=false)")
	}
}

func TestAutomaticFailover(t *testing.T) {
	t.Run("counter increments, no failover before threshold", func(t *testing.T) {
		f := newAutoFixture(t, 3, true, []*unstructured.Unstructured{healthyReplica()}, nil)

		f.reconcile(t) // 1/3
		f.reconcile(t) // 2/3
		if cp := f.currentPrimary(t); cp != "" {
			t.Errorf("must not fail over before threshold; currentPrimary=%q", cp)
		}
		evs := drainEvents(f.rec)
		if len(eventsContaining(evs, eventReasonPrimaryUnhealthy)) == 0 {
			t.Errorf("expected PrimaryUnhealthy events, got %v", evs)
		}
		if len(eventsContaining(evs, eventReasonFailoverCompleted)) != 0 {
			t.Errorf("must not have failed over yet: %v", evs)
		}
	})

	t.Run("fails over at threshold (Ordered → first healthy replica)", func(t *testing.T) {
		f := newAutoFixture(t, 2, true, []*unstructured.Unstructured{healthyReplica()}, nil)

		f.reconcile(t) // 1/2
		if f.currentPrimary(t) != "" {
			t.Fatalf("premature failover")
		}
		f.reconcile(t) // 2/2 → promote site-b

		if cp := f.currentPrimary(t); cp != siteB {
			t.Errorf("currentPrimary: got %q, want site-b", cp)
		}
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(cnpgClusterGVK)
		_ = f.rem.Get(context.Background(), types.NamespacedName{Namespace: siteB, Name: "pg-prod"}, got)
		enabled, _, _ := unstructured.NestedBool(got.Object, "spec", "replica", "enabled")
		if enabled {
			t.Errorf("site-b should be promoted (spec.replica.enabled=false)")
		}
		if len(eventsContaining(drainEvents(f.rec), eventReasonFailoverCompleted)) == 0 {
			t.Errorf("expected FailoverCompleted event")
		}
	})

	t.Run("no healthy replica → AutoFailoverNoCandidate, no crash", func(t *testing.T) {
		// site-b present but NOT ready (readyInstances=0).
		tb := true
		notReady := newCNPGClusterForTest(siteB, &tb, 0)
		f := newAutoFixture(t, 2, true, []*unstructured.Unstructured{notReady}, nil)

		f.reconcile(t)
		f.reconcile(t) // threshold reached, but no candidate

		if cp := f.currentPrimary(t); cp != "" {
			t.Errorf("must stay unavailable; currentPrimary=%q", cp)
		}
		if len(eventsContaining(drainEvents(f.rec), eventReasonAutoFailoverNoCandidate)) == 0 {
			t.Errorf("expected AutoFailoverNoCandidate event")
		}
	})

	t.Run("split-brain observed → automatic failover suspended", func(t *testing.T) {
		secretRef := corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "kc"},
			Key:                  "kubeconfig",
		}
		// Two replicas both observed as CNPG-primary + ready → split-brain.
		repSpec := []hav1alpha1.ReplicaSite{
			{Name: siteB, KubeconfigSecretRef: secretRef, ClusterRef: hav1alpha1.ClusterRef{Name: "pg-prod", Namespace: siteB}},
			{Name: siteC, KubeconfigSecretRef: secretRef, ClusterRef: hav1alpha1.ClusterRef{Name: "pg-prod", Namespace: siteC}},
		}
		// remote client serves both site-b and site-c CNPG as primaries.
		bPrim := newCNPGClusterForTest(siteB, nil, 1)
		cPrim := newCNPGClusterForTest(siteC, nil, 1)
		f := newAutoFixture(t, 2, true, []*unstructured.Unstructured{bPrim, cPrim}, repSpec)

		f.reconcile(t)
		f.reconcile(t)
		f.reconcile(t)

		if cp := f.currentPrimary(t); cp != "" {
			t.Errorf("must not auto-fail-over under split-brain; currentPrimary=%q", cp)
		}
		evs := drainEvents(f.rec)
		if len(eventsContaining(evs, eventReasonFailoverCompleted)) != 0 {
			t.Errorf("must not promote under split-brain: %v", evs)
		}
	})

	t.Run("healthy primary resets the counter (no failover)", func(t *testing.T) {
		f := newAutoFixture(t, 2, false, []*unstructured.Unstructured{healthyReplica()}, nil)

		f.reconcile(t)
		f.reconcile(t)
		f.reconcile(t)
		if cp := f.currentPrimary(t); cp != "site-a" {
			t.Errorf("healthy primary should remain; got %q", cp)
		}
	})

	t.Run("mode=Manual → handleAutomaticFailover is a no-op", func(t *testing.T) {
		f := newAutoFixture(t, 2, true, []*unstructured.Unstructured{healthyReplica()}, nil)
		ha := &hav1alpha1.HACluster{}
		if err := f.hub.Get(context.Background(), f.haKey, ha); err != nil {
			t.Fatalf("get: %v", err)
		}
		ha.Spec.Failover.Mode = hav1alpha1.FailoverModeManual
		if err := f.hub.Update(context.Background(), ha); err != nil {
			t.Fatalf("update: %v", err)
		}

		f.reconcile(t)
		f.reconcile(t)
		f.reconcile(t)
		if cp := f.currentPrimary(t); cp != "" {
			t.Errorf("Manual mode must not auto-fail-over; currentPrimary=%q", cp)
		}
	})
}

// TestAutomaticFailover_OldPrimaryFencedNotReconfigured is a regression
// guard. After an automatic failover, the demoted old primary is still
// CNPG-primary (spec.replica.enabled=false) on the API server but the
// in-memory observation buffer was mutated to primary=false for status.
// reconcileReplicaTopology must classify it from a FRESH read, so under
// rejoinPolicy=Manual it gets FENCED — not silently rebuilt as a replica
// (which would bypass the Manual data-safety guard).
func TestAutomaticFailover_OldPrimaryFencedNotReconfigured(t *testing.T) {
	ctx := context.Background()
	scheme := buildPromoteScheme(t)
	secretRef := corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "kc"},
		Key:                  "kubeconfig",
	}

	ha := &hav1alpha1.HACluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-db", Namespace: "ops"},
		Spec: hav1alpha1.HAClusterSpec{
			Primary: hav1alpha1.PrimarySite{
				Name:                "site-a",
				ClusterRef:          hav1alpha1.ClusterRef{Name: "pg-prod", Namespace: "site-a"},
				ReplicationEndpoint: siteAEndpoint,
			},
			Replicas: []hav1alpha1.ReplicaSite{{
				Name:                siteB,
				KubeconfigSecretRef: secretRef,
				ClusterRef:          hav1alpha1.ClusterRef{Name: "pg-prod", Namespace: siteB},
				ReplicationEndpoint: "pg-prod-rw.site-b.svc.cluster.local",
			}},
			Failover: hav1alpha1.FailoverSpec{
				Mode:             hav1alpha1.FailoverModeAutomatic,
				FailureThreshold: 2,
				RejoinPolicy:     hav1alpha1.RejoinPolicyManual,
			},
		},
	}

	// site-a: present + reachable, CNPG-primary (spec.replica absent) but
	// UNHEALTHY (readyInstances=0) → triggers the failover counter while
	// remaining reachable for the topology reconcile.
	siteACNPG := newCNPGClusterForTest("site-a", nil, 0)
	hub := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ha, siteACNPG, newServiceForTest("site-a")).
		WithStatusSubresource(&hav1alpha1.HACluster{}).Build()

	remote := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(healthyReplica(), newServiceForTest(siteB)).Build()

	cache := remoteclient.NewCache(scheme)
	cache.PutForTest("ops", secretRef, remote)
	rec := k8sevents.NewFakeRecorder(40)
	r := &HAClusterReconciler{Client: hub, Scheme: scheme, RemoteClients: cache, Recorder: rec}
	key := types.NamespacedName{Name: "prod-db", Namespace: "ops"}

	for i := range 2 { // 1/2 then 2/2 → auto-failover
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("Reconcile #%d: %v", i+1, err)
		}
	}

	gotHA := &hav1alpha1.HACluster{}
	if err := hub.Get(ctx, key, gotHA); err != nil {
		t.Fatalf("get HACluster: %v", err)
	}
	if gotHA.Status.CurrentPrimarySite != siteB {
		t.Fatalf("expected failover to site-b, currentPrimary=%q", gotHA.Status.CurrentPrimarySite)
	}

	siteA := &unstructured.Unstructured{}
	siteA.SetGroupVersionKind(cnpgClusterGVK)
	if err := hub.Get(ctx, types.NamespacedName{Namespace: "site-a", Name: "pg-prod"}, siteA); err != nil {
		t.Fatalf("get site-a cnpg: %v", err)
	}

	// THE regression assertion: the old primary must NOT have been turned
	// into a replica behind rejoinPolicy=Manual.
	enabled, found, _ := unstructured.NestedBool(siteA.Object, "spec", "replica", "enabled")
	if found && enabled {
		t.Errorf("old primary was silently reconfigured as replica (spec.replica.enabled=true) — Manual guard bypassed")
	}
	if siteA.GetAnnotations()[promotion.AnnotationFenced] != promotion.FencedAllInstances {
		t.Errorf("old primary should be fenced under rejoinPolicy=Manual; annotations=%v", siteA.GetAnnotations())
	}

	evs := drainEvents(rec)
	if len(eventsContaining(evs, eventReasonRejoinFenced)) == 0 {
		t.Errorf("expected RejoinFenced event, got %v", evs)
	}
	if len(eventsContaining(evs, eventReasonRejoinReconfigured)) != 0 {
		t.Errorf("must NOT emit RejoinReconfigured under Manual policy: %v", evs)
	}
}

// TestAutomaticFailover_StabilizationCooldown guards against failover
// cascade (flapping): right after a failover the freshly promoted primary
// is transiently unhealthy while CNPG performs the promotion restart. The
// operator must NOT trigger another failover during the cooldown window.
func TestAutomaticFailover_StabilizationCooldown(t *testing.T) {
	ctx := context.Background()
	// primaryDown=true → declared primary unhealthy; a healthy replica exists.
	f := newAutoFixture(t, 2, true, []*unstructured.Unstructured{healthyReplica()}, nil)

	// Simulate "a failover just happened": LastFailoverTime = now.
	ha := &hav1alpha1.HACluster{}
	if err := f.hub.Get(ctx, f.haKey, ha); err != nil {
		t.Fatalf("get: %v", err)
	}
	now := metav1.Now()
	ha.Status.LastFailoverTime = &now
	if err := f.hub.Status().Update(ctx, ha); err != nil {
		t.Fatalf("status update: %v", err)
	}

	for range 3 {
		if _, err := f.r.Reconcile(ctx, ctrl.Request{NamespacedName: f.haKey}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}

	if cp := f.currentPrimary(t); cp != "" {
		t.Errorf("must not fail over during stabilization cooldown; currentPrimary=%q", cp)
	}
	evs := drainEvents(f.rec)
	for _, bad := range []string{eventReasonFailoverStarted, eventReasonFailoverCompleted, eventReasonPrimaryUnhealthy} {
		if len(eventsContaining(evs, bad)) != 0 {
			t.Errorf("cooldown must suppress %s; events=%v", bad, evs)
		}
	}
}
