/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package health probes the health of a single site by reading its CNPG
// Cluster CR. It manipulates the CR as unstructured JSON so cnpg-ha keeps
// no compile-time dependency on the CloudNativePG module.
//
// CNPG's Cluster.status does NOT expose a replication LSN nor a lag in
// seconds; status.timelineID is exposed as a coarse fallback when the
// optional direct PostgreSQL probe is not configured.
package health

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CNPGClusterGVK is the GroupVersionKind of the CNPG Cluster CR.
var CNPGClusterGVK = schema.GroupVersionKind{
	Group:   "postgresql.cnpg.io",
	Version: "v1",
	Kind:    "Cluster",
}

// SiteHealth is the observed health of one site's CNPG Cluster at a given
// reconcile. Zero value = unreachable/unknown.
type SiteHealth struct {
	// Reachable is true when the site's Kubernetes API answered the Get.
	Reachable bool
	// Primary is true when the CNPG Cluster is in primary mode
	// (spec.replica.enabled absent or false).
	Primary bool
	// Ready is true when status.readyInstances > 0.
	Ready bool
	// Phase mirrors status.phase (empty when unreachable).
	Phase string
	// Reason is a short human explanation, set only when the site is
	// unreachable or not ready.
	Reason string
	// TimelineID mirrors status.timelineID — a coarse "how advanced"
	// proxy (CNPG exposes no LSN/lag in Cluster.status). 0 when unknown.
	TimelineID int64
	// LSNKnown is true when the optional PostgreSQL probe successfully read a
	// WAL location from the site.
	LSNKnown bool
	// LSN is the PostgreSQL WAL location read by the optional SQL probe.
	LSN string
	// LSNValue is the parsed numeric representation of LSN for ordering.
	LSNValue uint64
	// LagSeconds is the replay lag read by the optional SQL probe. It is nil
	// when the probe is not configured or PostgreSQL cannot compute lag yet.
	LagSeconds *float64
}

// Probe reads the CNPG Cluster at ref through cli and returns its health.
// Any Get error yields Reachable=false with the error in Reason — Probe
// never returns an error itself, so a single unreachable site cannot fail
// the whole reconcile.
func Probe(ctx context.Context, cli client.Client, ref types.NamespacedName) SiteHealth {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(CNPGClusterGVK)
	if err := cli.Get(ctx, ref, u); err != nil {
		return SiteHealth{Reason: fmt.Sprintf("get cnpg cluster %s/%s: %v", ref.Namespace, ref.Name, err)}
	}
	h := parseCluster(u)
	h.Reachable = true
	return h
}

// parseCluster derives SiteHealth from a CNPG Cluster CR. Pure (no I/O):
// directly unit-testable without a Kubernetes client.
//
// Rules:
//   - Primary  = spec.replica.enabled absent or false.
//   - Ready    = status.readyInstances > 0.
//   - Reason   = non-empty only when not Ready.
//   - TimelineID = status.timelineID (0 if absent).
func parseCluster(u *unstructured.Unstructured) SiteHealth {
	replicaEnabled, found, _ := unstructured.NestedBool(u.Object, "spec", "replica", "enabled")
	h := SiteHealth{Primary: !found || !replicaEnabled}

	h.Phase, _, _ = unstructured.NestedString(u.Object, "status", "phase")
	h.TimelineID, _, _ = unstructured.NestedInt64(u.Object, "status", "timelineID")

	readyInstances, _, _ := unstructured.NestedInt64(u.Object, "status", "readyInstances")
	h.Ready = readyInstances > 0
	if !h.Ready {
		h.Reason = fmt.Sprintf("not ready (phase=%q, readyInstances=%d)", h.Phase, readyInstances)
	}
	return h
}
