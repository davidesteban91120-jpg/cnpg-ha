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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	k8sevents "k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	hav1alpha1 "github.com/davidesteban/cnpg-ha/api/v1alpha1"
	"github.com/davidesteban/cnpg-ha/internal/promotion"
	"github.com/davidesteban/cnpg-ha/internal/remoteclient"
)

// buildPromoteScheme returns a scheme that knows core types, the HACluster
// API and the CNPG Cluster GVK (registered as Unstructured).
func buildPromoteScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := hav1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add hav1alpha1 scheme: %v", err)
	}
	s.AddKnownTypeWithName(cnpgClusterGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(
		cnpgClusterGVK.GroupVersion().WithKind("ClusterList"),
		&unstructured.UnstructuredList{},
	)
	return s
}

// promoteFixture is the canonical setup used by every test in this file:
// one HACluster named "prod-db" in namespace "ops", referencing one local
// primary site ("site-a" → db/pg-prod) and one remote replica site
// ("site-b" → db/pg-prod on a remote cluster).
type promoteFixture struct {
	scheme   *runtime.Scheme
	hub      client.Client
	remote   client.Client
	cache    *remoteclient.Cache
	recorder *k8sevents.FakeRecorder
	r        *HAClusterReconciler
	haKey    types.NamespacedName
}

type promoteFixtureOpts struct {
	annotation        string // value for ha.cnpg.io/promote; "" means no annotation
	mode              hav1alpha1.FailoverMode
	remoteReplicaCNPG *unstructured.Unstructured // override the remote replica CNPG (for unhealthy cases)
	omitOldPrimary    bool                       // don't seed the old primary CNPG Cluster / -rw Service (DR: site gone)
}

// newPromoteFixture builds an in-memory hub + remote fake client pair and
// the reconciler wired against them.
func newPromoteFixture(t *testing.T, opts promoteFixtureOpts) *promoteFixture {
	t.Helper()
	scheme := buildPromoteScheme(t)

	annotations := map[string]string{}
	if opts.annotation != "" {
		annotations[annotationPromote] = opts.annotation
	}
	mode := opts.mode
	if mode == "" {
		mode = hav1alpha1.FailoverModeManual
	}

	secretRef := corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "kc"},
		Key:                  "kubeconfig",
	}
	ha := &hav1alpha1.HACluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "prod-db",
			Namespace:   "ops",
			Annotations: annotations,
		},
		Spec: hav1alpha1.HAClusterSpec{
			Primary: hav1alpha1.PrimarySite{
				Name:       "site-a",
				ClusterRef: hav1alpha1.ClusterRef{Name: "pg-prod", Namespace: "db"},
			},
			Replicas: []hav1alpha1.ReplicaSite{{
				Name:                "site-b",
				KubeconfigSecretRef: secretRef,
				ClusterRef:          hav1alpha1.ClusterRef{Name: "pg-prod", Namespace: "db"},
			}},
			Failover: hav1alpha1.FailoverSpec{Mode: mode},
		},
	}

	// Hub: HACluster + (optionally) local primary CNPG Cluster + its -rw Service.
	hubObjects := []client.Object{ha}
	if !opts.omitOldPrimary {
		hubObjects = append(hubObjects,
			newCNPGClusterForTest("db", nil, 1),
			newServiceForTest("db"),
		)
	}
	hub := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(hubObjects...).
		WithStatusSubresource(&hav1alpha1.HACluster{}).
		Build()

	// Remote: replica CNPG (defaults to healthy replica) + its -rw Service.
	tBool := true
	remoteCNPG := opts.remoteReplicaCNPG
	if remoteCNPG == nil {
		remoteCNPG = newCNPGClusterForTest("db", &tBool, 1)
	}
	newSvc := newServiceForTest("db")
	remote := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(remoteCNPG, newSvc).
		Build()

	cache := remoteclient.NewCache(scheme)
	cache.PutForTest(ha.Namespace, secretRef, remote)

	recorder := k8sevents.NewFakeRecorder(20)
	r := &HAClusterReconciler{
		Client:        hub,
		Scheme:        scheme,
		RemoteClients: cache,
		Recorder:      recorder,
	}

	return &promoteFixture{
		scheme:   scheme,
		hub:      hub,
		remote:   remote,
		cache:    cache,
		recorder: recorder,
		r:        r,
		haKey:    types.NamespacedName{Name: ha.Name, Namespace: ha.Namespace},
	}
}

func newCNPGClusterForTest(ns string, replicaEnabled *bool, readyInstances int64) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(cnpgClusterGVK)
	u.SetNamespace(ns)
	u.SetName("pg-prod")
	if replicaEnabled != nil {
		_ = unstructured.SetNestedField(u.Object, *replicaEnabled, "spec", "replica", "enabled")
	}
	_ = unstructured.SetNestedField(u.Object, readyInstances, "status", "readyInstances")
	_ = unstructured.SetNestedField(u.Object, "Cluster in healthy state", "status", "phase")
	return u
}

// newServiceForTest builds the CNPG-managed read-write Service ("pg-prod-rw")
// in ns — the object FlipCiliumService targets.
func newServiceForTest(ns string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      "pg-prod-rw",
		},
	}
}

// drainEvents collects every queued event from a FakeRecorder.
func drainEvents(rec *k8sevents.FakeRecorder) []string {
	out := []string{}
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func eventsContaining(events []string, needle string) []string {
	matched := []string{}
	for _, e := range events {
		if strings.Contains(e, needle) {
			matched = append(matched, e)
		}
	}
	return matched
}

func TestReconcile_NoAnnotation_NoPromotion(t *testing.T) {
	f := newPromoteFixture(t, promoteFixtureOpts{}) // no annotation

	if _, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: f.haKey}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Old primary must not be fenced.
	oldCNPG := &unstructured.Unstructured{}
	oldCNPG.SetGroupVersionKind(cnpgClusterGVK)
	if err := f.hub.Get(context.Background(), types.NamespacedName{Namespace: "db", Name: "pg-prod"}, oldCNPG); err != nil {
		t.Fatalf("get old cnpg: %v", err)
	}
	if v := oldCNPG.GetAnnotations()[promotion.AnnotationFenced]; v != "" {
		t.Errorf("old primary unexpectedly fenced: %q", v)
	}

	// No promotion-related event should have been emitted.
	events := drainEvents(f.recorder)
	for _, e := range events {
		if strings.Contains(e, eventReasonFailoverStarted) ||
			strings.Contains(e, eventReasonFailoverCompleted) ||
			strings.Contains(e, eventReasonPromoteRejected) {
			t.Errorf("unexpected promotion event: %q", e)
		}
	}
}

func TestReconcile_PromoteHappyPath(t *testing.T) {
	ctx := context.Background()
	f := newPromoteFixture(t, promoteFixtureOpts{annotation: "site-b"})

	if _, err := f.r.Reconcile(ctx, ctrl.Request{NamespacedName: f.haKey}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// HACluster: annotation cleared, CurrentPrimarySite=site-b, LastFailoverTime set.
	gotHA := &hav1alpha1.HACluster{}
	if err := f.hub.Get(ctx, f.haKey, gotHA); err != nil {
		t.Fatalf("get HACluster: %v", err)
	}
	if _, ok := gotHA.Annotations[annotationPromote]; ok {
		t.Errorf("promote annotation still present after success")
	}
	if gotHA.Status.CurrentPrimarySite != "site-b" {
		t.Errorf("CurrentPrimarySite: got %q, want site-b", gotHA.Status.CurrentPrimarySite)
	}
	if gotHA.Status.LastFailoverTime == nil {
		t.Errorf("LastFailoverTime should be set after a successful promotion")
	}
	if c := findCondition(gotHA.Status.Conditions, conditionFailoverInProgress); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("FailoverInProgress should be False after success, got %+v", c)
	}

	// Old primary: fenced + Service flipped to RoleRemote.
	gotOld := &unstructured.Unstructured{}
	gotOld.SetGroupVersionKind(cnpgClusterGVK)
	if err := f.hub.Get(ctx, types.NamespacedName{Namespace: "db", Name: "pg-prod"}, gotOld); err != nil {
		t.Fatalf("get old cnpg: %v", err)
	}
	if v := gotOld.GetAnnotations()[promotion.AnnotationFenced]; v != promotion.FencedAllInstances {
		t.Errorf("old primary fence annotation: got %q, want %q", v, promotion.FencedAllInstances)
	}
	gotOldSvc := &corev1.Service{}
	if err := f.hub.Get(ctx, types.NamespacedName{Namespace: "db", Name: "pg-prod-rw"}, gotOldSvc); err != nil {
		t.Fatalf("get old service: %v", err)
	}
	if gotOldSvc.Annotations[promotion.AnnotationCiliumGlobal] != "true" {
		t.Errorf("old service global: got %q, want true", gotOldSvc.Annotations[promotion.AnnotationCiliumGlobal])
	}
	if gotOldSvc.Annotations[promotion.AnnotationCiliumAffinity] != "remote" {
		t.Errorf("old service affinity: got %q, want remote", gotOldSvc.Annotations[promotion.AnnotationCiliumAffinity])
	}

	// New primary: promoted (replica.enabled=false) + Service flipped to RoleLocal.
	gotNew := &unstructured.Unstructured{}
	gotNew.SetGroupVersionKind(cnpgClusterGVK)
	if err := f.remote.Get(ctx, types.NamespacedName{Namespace: "db", Name: "pg-prod"}, gotNew); err != nil {
		t.Fatalf("get new cnpg: %v", err)
	}
	enabled, _, _ := unstructured.NestedBool(gotNew.Object, "spec", "replica", "enabled")
	if enabled {
		t.Errorf("new primary still has spec.replica.enabled=true")
	}
	gotNewSvc := &corev1.Service{}
	if err := f.remote.Get(ctx, types.NamespacedName{Namespace: "db", Name: "pg-prod-rw"}, gotNewSvc); err != nil {
		t.Fatalf("get new service: %v", err)
	}
	if gotNewSvc.Annotations[promotion.AnnotationCiliumAffinity] != "local" {
		t.Errorf("new service affinity: got %q, want local", gotNewSvc.Annotations[promotion.AnnotationCiliumAffinity])
	}

	// Events: at least FailoverStarted and FailoverCompleted.
	events := drainEvents(f.recorder)
	if len(eventsContaining(events, eventReasonFailoverStarted)) == 0 {
		t.Errorf("missing FailoverStarted event in %v", events)
	}
	if len(eventsContaining(events, eventReasonFailoverCompleted)) == 0 {
		t.Errorf("missing FailoverCompleted event in %v", events)
	}
}

func TestReconcile_PromoteRejected_AutomaticMode(t *testing.T) {
	ctx := context.Background()
	f := newPromoteFixture(t, promoteFixtureOpts{
		annotation: "site-b",
		mode:       hav1alpha1.FailoverModeAutomatic,
	})

	if _, err := f.r.Reconcile(ctx, ctrl.Request{NamespacedName: f.haKey}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Annotation cleared; no promotion executed.
	gotHA := &hav1alpha1.HACluster{}
	if err := f.hub.Get(ctx, f.haKey, gotHA); err != nil {
		t.Fatalf("get HACluster: %v", err)
	}
	if _, ok := gotHA.Annotations[annotationPromote]; ok {
		t.Errorf("promote annotation kept despite rejection")
	}
	gotOld := &unstructured.Unstructured{}
	gotOld.SetGroupVersionKind(cnpgClusterGVK)
	_ = f.hub.Get(ctx, types.NamespacedName{Namespace: "db", Name: "pg-prod"}, gotOld)
	if gotOld.GetAnnotations()[promotion.AnnotationFenced] != "" {
		t.Errorf("old primary should not be fenced when annotation rejected")
	}

	events := drainEvents(f.recorder)
	if len(eventsContaining(events, eventReasonPromoteRejected)) == 0 {
		t.Errorf("expected PromoteRejected event, got %v", events)
	}
}

func TestReconcile_PromoteRejected_UnknownTarget(t *testing.T) {
	ctx := context.Background()
	f := newPromoteFixture(t, promoteFixtureOpts{annotation: "site-z"})

	if _, err := f.r.Reconcile(ctx, ctrl.Request{NamespacedName: f.haKey}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	gotHA := &hav1alpha1.HACluster{}
	if err := f.hub.Get(ctx, f.haKey, gotHA); err != nil {
		t.Fatalf("get HACluster: %v", err)
	}
	if _, ok := gotHA.Annotations[annotationPromote]; ok {
		t.Errorf("annotation should be cleared on invalid target")
	}
	events := drainEvents(f.recorder)
	matches := eventsContaining(events, eventReasonPromoteRejected)
	if len(matches) == 0 {
		t.Errorf("expected PromoteRejected event, got %v", events)
	} else if !strings.Contains(matches[0], "site-z") {
		t.Errorf("event should mention target name; got %q", matches[0])
	}
}

func TestReconcile_PromoteRejected_TargetNotReady(t *testing.T) {
	ctx := context.Background()
	tBool := true
	// Replica exists but readyInstances=0 → observation says not ready.
	notReady := newCNPGClusterForTest("db", &tBool, 0)
	f := newPromoteFixture(t, promoteFixtureOpts{
		annotation:        "site-b",
		remoteReplicaCNPG: notReady,
	})

	if _, err := f.r.Reconcile(ctx, ctrl.Request{NamespacedName: f.haKey}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	gotHA := &hav1alpha1.HACluster{}
	if err := f.hub.Get(ctx, f.haKey, gotHA); err != nil {
		t.Fatalf("get HACluster: %v", err)
	}
	if _, ok := gotHA.Annotations[annotationPromote]; ok {
		t.Errorf("annotation should be cleared when target is unhealthy")
	}

	// New primary CNPG should still be a replica (no Promote attempted).
	gotNew := &unstructured.Unstructured{}
	gotNew.SetGroupVersionKind(cnpgClusterGVK)
	if err := f.remote.Get(ctx, types.NamespacedName{Namespace: "db", Name: "pg-prod"}, gotNew); err != nil {
		t.Fatalf("get new cnpg: %v", err)
	}
	enabled, _, _ := unstructured.NestedBool(gotNew.Object, "spec", "replica", "enabled")
	if !enabled {
		t.Errorf("Promote should not run when target not ready")
	}

	events := drainEvents(f.recorder)
	if len(eventsContaining(events, eventReasonPromoteRejected)) == 0 {
		t.Errorf("expected PromoteRejected event, got %v", events)
	}
}

// TestReconcile_PromoteSucceedsWhenOldPrimaryGone covers the disaster-
// recovery path: the old primary site is completely gone (its CNPG Cluster
// and -rw Service no longer exist). Fencing it is impossible but also
// unnecessary — it cannot accept writes — so the failover must still
// succeed instead of aborting on a NotFound.
func TestReconcile_PromoteSucceedsWhenOldPrimaryGone(t *testing.T) {
	ctx := context.Background()
	f := newPromoteFixture(t, promoteFixtureOpts{
		annotation:     "site-b",
		omitOldPrimary: true,
	})

	if _, err := f.r.Reconcile(ctx, ctrl.Request{NamespacedName: f.haKey}); err != nil {
		t.Fatalf("Reconcile returned error (failover must not abort when old primary is gone): %v", err)
	}

	gotHA := &hav1alpha1.HACluster{}
	if err := f.hub.Get(ctx, f.haKey, gotHA); err != nil {
		t.Fatalf("get HACluster: %v", err)
	}
	if _, ok := gotHA.Annotations[annotationPromote]; ok {
		t.Errorf("promote annotation should be cleared after a successful DR failover")
	}
	if gotHA.Status.CurrentPrimarySite != "site-b" {
		t.Errorf("CurrentPrimarySite: got %q, want site-b", gotHA.Status.CurrentPrimarySite)
	}
	if gotHA.Status.LastFailoverTime == nil {
		t.Errorf("LastFailoverTime should be set")
	}

	// Target must be promoted (spec.replica.enabled=false) despite the
	// missing old primary.
	gotNew := &unstructured.Unstructured{}
	gotNew.SetGroupVersionKind(cnpgClusterGVK)
	if err := f.remote.Get(ctx, types.NamespacedName{Namespace: "db", Name: "pg-prod"}, gotNew); err != nil {
		t.Fatalf("get new cnpg: %v", err)
	}
	enabled, _, _ := unstructured.NestedBool(gotNew.Object, "spec", "replica", "enabled")
	if enabled {
		t.Errorf("target should have been promoted (spec.replica.enabled=false)")
	}

	events := drainEvents(f.recorder)
	if len(eventsContaining(events, eventReasonFailoverFailed)) != 0 {
		t.Errorf("must NOT emit FailoverFailed; events=%v", events)
	}
	if len(eventsContaining(events, eventReasonFailoverCompleted)) == 0 {
		t.Errorf("expected FailoverCompleted event, got %v", events)
	}
}
