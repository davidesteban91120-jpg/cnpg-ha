/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package health

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// makeCluster builds an unstructured CNPG Cluster with the requested spec
// and status fields. replicaEnabled=nil ⇒ spec.replica absent.
func makeCluster(replicaEnabled *bool, phase string, readyInstances, timelineID int64) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	if replicaEnabled != nil {
		_ = unstructured.SetNestedField(u.Object, *replicaEnabled, "spec", "replica", "enabled")
	}
	if phase != "" {
		_ = unstructured.SetNestedField(u.Object, phase, "status", "phase")
	}
	_ = unstructured.SetNestedField(u.Object, readyInstances, "status", "readyInstances")
	if timelineID != 0 {
		_ = unstructured.SetNestedField(u.Object, timelineID, "status", "timelineID")
	}
	return u
}

func TestParseCluster(t *testing.T) {
	tBool, fBool := true, false
	tests := []struct {
		name           string
		u              *unstructured.Unstructured
		wantPrimary    bool
		wantReady      bool
		wantPhase      string
		wantTimeline   int64
		wantReasonHasf bool
	}{
		{
			name:         "spec.replica absent → primary, ready, timeline read",
			u:            makeCluster(nil, "Cluster in healthy state", 1, 3),
			wantPrimary:  true,
			wantReady:    true,
			wantPhase:    "Cluster in healthy state",
			wantTimeline: 3,
		},
		{
			name:         "spec.replica.enabled=false → primary",
			u:            makeCluster(&fBool, "Cluster in healthy state", 2, 1),
			wantPrimary:  true,
			wantReady:    true,
			wantPhase:    "Cluster in healthy state",
			wantTimeline: 1,
		},
		{
			name:         "spec.replica.enabled=true → replica",
			u:            makeCluster(&tBool, "Cluster in healthy state", 1, 1),
			wantPrimary:  false,
			wantReady:    true,
			wantPhase:    "Cluster in healthy state",
			wantTimeline: 1,
		},
		{
			name:           "readyInstances=0 → not ready, reason set",
			u:              makeCluster(nil, "Setting up primary", 0, 0),
			wantPrimary:    true,
			wantReady:      false,
			wantPhase:      "Setting up primary",
			wantReasonHasf: true,
		},
		{
			name:           "phase absent + ready=0 → reason mentions empty phase",
			u:              makeCluster(&tBool, "", 0, 0),
			wantPrimary:    false,
			wantReady:      false,
			wantReasonHasf: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := parseCluster(tt.u)
			if h.Primary != tt.wantPrimary {
				t.Errorf("Primary: got %v, want %v", h.Primary, tt.wantPrimary)
			}
			if h.Ready != tt.wantReady {
				t.Errorf("Ready: got %v, want %v", h.Ready, tt.wantReady)
			}
			if h.Phase != tt.wantPhase {
				t.Errorf("Phase: got %q, want %q", h.Phase, tt.wantPhase)
			}
			if h.TimelineID != tt.wantTimeline {
				t.Errorf("TimelineID: got %d, want %d", h.TimelineID, tt.wantTimeline)
			}
			if (h.Reason != "") != tt.wantReasonHasf {
				t.Errorf("Reason=%q, want hasReason=%v", h.Reason, tt.wantReasonHasf)
			}
		})
	}
}

func newSchemeWithCNPG(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(CNPGClusterGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(
		CNPGClusterGVK.GroupVersion().WithKind("ClusterList"),
		&unstructured.UnstructuredList{},
	)
	return s
}

func TestProbeReachable(t *testing.T) {
	scheme := newSchemeWithCNPG(t)
	cluster := makeCluster(nil, "Cluster in healthy state", 1, 2)
	cluster.SetGroupVersionKind(CNPGClusterGVK)
	cluster.SetNamespace("site-a")
	cluster.SetName("pg-prod")
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	h := Probe(context.Background(), cli, types.NamespacedName{Namespace: "site-a", Name: "pg-prod"})
	if !h.Reachable || !h.Primary || !h.Ready || h.TimelineID != 2 {
		t.Errorf("unexpected health: %+v", h)
	}
}

func TestProbeNotFound(t *testing.T) {
	scheme := newSchemeWithCNPG(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).Build()

	h := Probe(context.Background(), cli, types.NamespacedName{Namespace: "x", Name: "missing"})
	if h.Reachable {
		t.Errorf("expected unreachable, got %+v", h)
	}
	if h.Reason == "" {
		t.Errorf("expected a Reason explaining the failed Get")
	}
}
