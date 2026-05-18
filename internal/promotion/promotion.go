/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package promotion executes the steps that turn a replica CNPG Cluster into
// the new primary of an HACluster. Each exported function is idempotent so
// that the surrounding Reconcile can retry safely after a crash or partial
// failure.
//
// The package operates on raw API primitives (unstructured CNPG Cluster,
// native Service annotations) and does not depend on the CNPG Go module.
package promotion

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// AnnotationFenced is the CNPG annotation that fences instances of a
	// Cluster. The value `["*"]` fences every pod, preventing any of them
	// from accepting writes during a promotion.
	AnnotationFenced = "cnpg.io/fencedInstances"

	// FencedAllInstances is the value of AnnotationFenced that fences every
	// instance of the Cluster.
	FencedAllInstances = `["*"]`

	// AnnotationCiliumGlobal turns a Service into a Cilium Cluster Mesh
	// global service mirrored across all peered clusters.
	AnnotationCiliumGlobal = "service.cilium.io/global"

	// AnnotationCiliumAffinity controls which cluster's endpoints Cilium
	// prefers when routing to the global Service. See ServiceRole.
	AnnotationCiliumAffinity = "service.cilium.io/affinity"
)

// CNPGClusterGVK is the GroupVersionKind of the CNPG Cluster CR. The
// package uses unstructured access so callers do not need to import the
// CNPG module.
var CNPGClusterGVK = schema.GroupVersionKind{
	Group:   "postgresql.cnpg.io",
	Version: "v1",
	Kind:    "Cluster",
}

// Ref identifies a CNPG Cluster CR by namespace and name. The client used
// to manipulate it is passed separately so that operations can target the
// local cluster or any remote cluster transparently.
type Ref struct {
	Namespace string
	Name      string
}

// ServiceRole expresses the role the operator wants Cilium to give to the
// CNPG read-write Service of a site after a promotion.
type ServiceRole int

const (
	// RoleLocal marks the Service as the active primary in the Global mesh.
	// Cilium prefers this cluster's endpoints for write traffic.
	RoleLocal ServiceRole = iota

	// RoleRemote keeps the Service in the Global mesh but lets Cilium prefer
	// remote-cluster endpoints. Used to drain in-flight sessions on the
	// former primary without abruptly killing them.
	RoleRemote
)

func (r ServiceRole) affinityValue() string {
	if r == RoleRemote {
		return "remote"
	}
	return "local"
}

// Fence sets the CNPG fencing annotation on the Cluster pointed at by ref,
// preventing any instance from accepting writes. Must always run before
// Promote on the destination site, otherwise the former primary may still
// accept writes during the transition window.
//
// Idempotent: returns nil immediately if the annotation already matches.
func Fence(ctx context.Context, cli client.Client, ref Ref) error {
	u := newCNPGCluster()
	if err := cli.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, u); err != nil {
		return fmt.Errorf("get cnpg cluster %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	ann := u.GetAnnotations()
	if ann[AnnotationFenced] == FencedAllInstances {
		return nil
	}
	if ann == nil {
		ann = map[string]string{}
	}
	ann[AnnotationFenced] = FencedAllInstances
	u.SetAnnotations(ann)
	if err := cli.Update(ctx, u); err != nil {
		return fmt.Errorf("fence cnpg cluster %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	return nil
}

// Promote turns the replica Cluster pointed at by ref into a primary by
// setting spec.replica.enabled=false. Has no effect if the Cluster is
// already a primary (spec.replica.enabled is absent or already false).
//
// CNPG itself handles the actual PostgreSQL promotion once the field flips.
func Promote(ctx context.Context, cli client.Client, ref Ref) error {
	u := newCNPGCluster()
	if err := cli.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, u); err != nil {
		return fmt.Errorf("get cnpg cluster %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	enabled, found, err := unstructured.NestedBool(u.Object, "spec", "replica", "enabled")
	if err != nil {
		return fmt.Errorf("read spec.replica.enabled %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	if !found || !enabled {
		return nil
	}
	if err := unstructured.SetNestedField(u.Object, false, "spec", "replica", "enabled"); err != nil {
		return fmt.Errorf("set spec.replica.enabled %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	if err := cli.Update(ctx, u); err != nil {
		return fmt.Errorf("promote cnpg cluster %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	return nil
}

// Reconfigure makes the CNPG Cluster at ref a replica that streams from
// sourceHost. It sets spec.replica.enabled=true and rewrites the host of
// the externalCluster referenced by spec.replica.source (or the single
// externalCluster when source is unset) to sourceHost.
//
// Used after a failover so surviving replicas — and a former primary that
// rejoins under RejoinPolicy=AutoReplica — follow the new primary. CNPG
// itself performs the actual resync (pg_rewind or re-bootstrap) once the
// spec changes; this function only rewrites intent.
//
// Idempotent: returns nil without an Update when the Cluster is already a
// replica pointing at sourceHost. Returns an error when there is no
// externalCluster to repoint (the Cluster was never set up for replication).
func Reconfigure(ctx context.Context, cli client.Client, ref Ref, sourceHost string) error {
	if sourceHost == "" {
		return fmt.Errorf("reconfigure cnpg cluster %s/%s: empty source host", ref.Namespace, ref.Name)
	}
	u := newCNPGCluster()
	if err := cli.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, u); err != nil {
		return fmt.Errorf("get cnpg cluster %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	externals, _, err := unstructured.NestedSlice(u.Object, "spec", "externalClusters")
	if err != nil {
		return fmt.Errorf("read spec.externalClusters %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	if len(externals) == 0 {
		return fmt.Errorf("reconfigure cnpg cluster %s/%s: no spec.externalClusters to repoint", ref.Namespace, ref.Name)
	}

	source, _, _ := unstructured.NestedString(u.Object, "spec", "replica", "source")
	idx, err := externalClusterIndex(externals, source)
	if err != nil {
		return fmt.Errorf("reconfigure cnpg cluster %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	ext, ok := externals[idx].(map[string]any)
	if !ok {
		return fmt.Errorf("reconfigure cnpg cluster %s/%s: externalClusters[%d] is not an object", ref.Namespace, ref.Name, idx)
	}
	conn, _ := ext["connectionParameters"].(map[string]any)
	if conn == nil {
		conn = map[string]any{}
	}

	enabled, _, _ := unstructured.NestedBool(u.Object, "spec", "replica", "enabled")
	if enabled && conn["host"] == sourceHost {
		return nil
	}

	conn["host"] = sourceHost
	ext["connectionParameters"] = conn
	externals[idx] = ext
	if err := unstructured.SetNestedSlice(u.Object, externals, "spec", "externalClusters"); err != nil {
		return fmt.Errorf("set spec.externalClusters %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	if err := unstructured.SetNestedField(u.Object, true, "spec", "replica", "enabled"); err != nil {
		return fmt.Errorf("set spec.replica.enabled %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	if err := cli.Update(ctx, u); err != nil {
		return fmt.Errorf("reconfigure cnpg cluster %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	return nil
}

// externalClusterIndex returns the index of the externalCluster named
// source. When source is empty it requires exactly one externalCluster and
// returns 0; ambiguity (empty source, several externalClusters) is an error.
func externalClusterIndex(externals []any, source string) (int, error) {
	if source == "" {
		if len(externals) != 1 {
			return 0, fmt.Errorf("spec.replica.source unset and %d externalClusters (ambiguous)", len(externals))
		}
		return 0, nil
	}
	for i, e := range externals {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := m["name"].(string); name == source {
			return i, nil
		}
	}
	return 0, fmt.Errorf("spec.replica.source %q has no matching externalCluster", source)
}

// FlipCiliumService updates the Cilium Cluster Mesh annotations on the
// CNPG-managed read-write Service of the site. By CNPG convention this
// Service is named <ref.Name>-rw and lives in the same namespace as the
// Cluster.
//
// role=RoleLocal:  service.cilium.io/global=true, affinity=local
// role=RoleRemote: service.cilium.io/global=true, affinity=remote
//
// Both calls keep the Service in the Global mesh so clients keep resolving
// the same name; only endpoint affinity changes.
func FlipCiliumService(ctx context.Context, cli client.Client, ref Ref, role ServiceRole) error {
	svcName := ref.Name + "-rw"
	var svc corev1.Service
	if err := cli.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: svcName}, &svc); err != nil {
		return fmt.Errorf("get service %s/%s: %w", ref.Namespace, svcName, err)
	}
	wantAffinity := role.affinityValue()
	if svc.Annotations[AnnotationCiliumGlobal] == "true" &&
		svc.Annotations[AnnotationCiliumAffinity] == wantAffinity {
		return nil
	}
	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	svc.Annotations[AnnotationCiliumGlobal] = "true"
	svc.Annotations[AnnotationCiliumAffinity] = wantAffinity
	if err := cli.Update(ctx, &svc); err != nil {
		return fmt.Errorf("update service %s/%s: %w", ref.Namespace, svcName, err)
	}
	return nil
}

func newCNPGCluster() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(CNPGClusterGVK)
	return u
}
