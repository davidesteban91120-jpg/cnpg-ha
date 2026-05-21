/*
Copyright 2026.

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

// Package v1alpha1 contains the Go types for the ha.cnpg.io/v1alpha1 API.
//
// Go reminder: one package == one directory. Every .go file in here must
// declare `package v1alpha1`. The v1alpha1 suffix flags the API as
// experimental — breaking changes are allowed without a conversion webhook.
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FailoverMode controls whether the operator promotes a replica
// automatically or waits for a human action.
//
// +kubebuilder:validation:Enum=Automatic;Manual
type FailoverMode string

const (
	// FailoverModeAutomatic: the operator promotes a replica as soon as the
	// primary failure threshold is reached. Pick this when recovery latency
	// is critical and the split-brain risk is acceptable (fencing in place).
	FailoverModeAutomatic FailoverMode = "Automatic"

	// FailoverModeManual: the operator detects the outage and surfaces the
	// state, but waits for an `ha.cnpg.io/promote: <site>` annotation before
	// acting. Pick this for sensitive databases or scheduled switchovers.
	FailoverModeManual FailoverMode = "Manual"
)

// PromotionPolicy decides which replica becomes the new primary.
//
// +kubebuilder:validation:Enum=MostAdvancedLSN;Ordered
type PromotionPolicy string

const (
	// PromotionPolicyMostAdvancedLSN: pick the replica whose PostgreSQL LSN
	// (Log Sequence Number) is furthest ahead — minimises data loss.
	PromotionPolicyMostAdvancedLSN PromotionPolicy = "MostAdvancedLSN"

	// PromotionPolicyOrdered: follow the order declared in spec.replicas
	// (useful to honour a preferred site, e.g. site B before site C).
	PromotionPolicyOrdered PromotionPolicy = "Ordered"
)

// RejoinPolicy controls what the operator does with a former primary that
// comes back online while another site is the current primary.
//
// +kubebuilder:validation:Enum=Manual;AutoReplica
type RejoinPolicy string

const (
	// RejoinPolicyManual: the operator fences the returning primary and
	// raises the SplitBrain condition. Converting it back to a replica is
	// left to a human (no silent data loss). Safe default.
	RejoinPolicyManual RejoinPolicy = "Manual"

	// RejoinPolicyAutoReplica: the operator rebuilds the returning primary
	// as a replica of the current primary. This discards any writes the
	// returning site accepted after the failover (diverged timeline) — fully
	// automatic but destructive by design.
	RejoinPolicyAutoReplica RejoinPolicy = "AutoReplica"
)

// SiteRole is the role a site was observed in during the last Reconcile.
//
// +kubebuilder:validation:Enum=Primary;Replica;Unknown
type SiteRole string

const (
	// SiteRolePrimary: the site's CNPG Cluster CR is in primary mode
	// (spec.replica missing or enabled=false).
	SiteRolePrimary SiteRole = "Primary"

	// SiteRoleReplica: the site's CNPG Cluster CR is in replica mode
	// (spec.replica.enabled=true).
	SiteRoleReplica SiteRole = "Replica"

	// SiteRoleUnknown: the site could not be reached or observed. No
	// conclusion can be drawn about its role.
	SiteRoleUnknown SiteRole = "Unknown"
)

// ClusterRef points to a CNPG `Cluster` CR (postgresql.cnpg.io/v1).
//
// Note: the CNPG Cluster is not imported as a Go type (to keep the
// dependency graph light). Its existence is checked at runtime through
// the Kubernetes client.
type ClusterRef struct {
	// Name of the CNPG Cluster CR.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace of the CNPG Cluster CR.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
}

// PrimarySite describes the local / bootstrap site declared at install
// time. After a failover the current primary is carried by
// status.currentPrimarySite, not by this field.
type PrimarySite struct {
	// Name is the logical identifier of the local / bootstrap site
	// (e.g. "site-a"). Same semantics as ReplicaSite.Name — it may be the
	// initial value of status.currentPrimarySite and is used as a key in
	// the metrics. Must be unique within the HACluster (do not reuse a
	// replica name).
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// ClusterRef points to the CNPG Cluster CR for the local / bootstrap
	// site. It serves as the primary at startup; after a failover the
	// current primary may be any of the sites listed in Spec.Replicas.
	//
	// +required
	ClusterRef ClusterRef `json:"clusterRef"`

	// ReplicationEndpoint is the host (optionally host:port) that the OTHER
	// sites use to stream WAL from this site when it is the current primary.
	// Typically the CNPG read-write Service FQDN
	// (e.g. "pg-prod-rw.site-a.svc.cluster.local"); in a Cilium Cluster Mesh
	// deployment, the global service name.
	//
	// When set, after a failover the operator rewrites every other site's
	// spec.replica externalCluster host to the new primary's endpoint so
	// surviving replicas follow the promotion automatically. When empty,
	// topology reconfiguration is skipped (the operator only observes).
	//
	// +optional
	ReplicationEndpoint string `json:"replicationEndpoint,omitempty"`
}

// ReplicaSite describes a remote CNPG cluster acting as a replica.
type ReplicaSite struct {
	// Name is the logical identifier of the site (e.g. "site-b"). Must be
	// stable and unique within the HACluster — it is used as a key in the
	// status and in Prometheus metrics.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// KubeconfigSecretRef points to a Secret in the local cluster that
	// holds the kubeconfig used to connect to the remote cluster. The
	// operator reads the Secret at startup and refreshes it on an internal
	// TTL.
	//
	// Security: the key referenced here MUST NEVER be logged. See
	// internal/remoteclient for the redaction logic.
	//
	// +required
	KubeconfigSecretRef corev1.SecretKeySelector `json:"kubeconfigSecretRef"`

	// ClusterRef points to the CNPG Cluster CR inside the remote cluster.
	//
	// +required
	ClusterRef ClusterRef `json:"clusterRef"`

	// ReplicationEndpoint is the host (optionally host:port) that the OTHER
	// sites use to stream WAL from this site once it has been promoted to
	// primary. Same semantics as PrimarySite.ReplicationEndpoint. Required
	// for this site to be eligible as an automatic-reconfiguration target
	// after a failover; when empty, other sites cannot be re-pointed here.
	//
	// +optional
	ReplicationEndpoint string `json:"replicationEndpoint,omitempty"`
}

// FailoverSpec groups the failover decision parameters.
type FailoverSpec struct {
	// Mode: Automatic or Manual. See FailoverMode.
	//
	// +kubebuilder:default=Manual
	// +optional
	Mode FailoverMode `json:"mode,omitempty"`

	// HealthCheckIntervalSeconds: period between two primary probes.
	// 10 s is a good starting point — too short causes flapping, too long
	// degrades the RTO.
	//
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=300
	// +optional
	HealthCheckIntervalSeconds int32 `json:"healthCheckIntervalSeconds,omitempty"`

	// FailureThreshold: number of consecutive failed probes before the
	// primary is considered down. Always greater than 1 so a transient
	// network blip cannot trigger a failover.
	//
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=20
	// +optional
	FailureThreshold int32 `json:"failureThreshold,omitempty"`

	// PromotionPolicy: see PromotionPolicy.
	//
	// +kubebuilder:default=MostAdvancedLSN
	// +optional
	PromotionPolicy PromotionPolicy `json:"promotionPolicy,omitempty"`

	// RejoinPolicy controls what happens to a former primary that comes
	// back while another site is the current primary. See RejoinPolicy.
	//
	// +kubebuilder:default=Manual
	// +optional
	RejoinPolicy RejoinPolicy `json:"rejoinPolicy,omitempty"`
}

// HAClusterSpec describes the desired state of an HACluster.
type HAClusterSpec struct {
	// Primary: site currently serving writes.
	//
	// +required
	Primary PrimarySite `json:"primary"`

	// Replicas: remote sites ready to be promoted.
	//
	// +required
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=name
	Replicas []ReplicaSite `json:"replicas"`

	// Failover: failover decision parameters.
	//
	// +optional
	Failover FailoverSpec `json:"failover,omitempty"`
}

// SiteStatus exposes the observation of a site (primary or replica)
// captured during the last Reconcile. It supports quick inspection via
// kubectl and is the basis for the promotion logic that consults the
// state of each candidate.
type SiteStatus struct {
	// Name is the logical identifier of the site, consistent with
	// spec.primary.name or spec.replicas[].name.
	//
	// +required
	Name string `json:"name"`

	// Role observed for the site during the last Reconcile.
	//
	// +required
	Role SiteRole `json:"role"`

	// Reachable indicates whether the site's K8s API responded during the
	// last Reconcile.
	Reachable bool `json:"reachable"`

	// Ready indicates whether the site's CNPG Cluster CR has at least one
	// ready instance (status.readyInstances > 0).
	Ready bool `json:"ready"`

	// Phase mirrors status.phase of the CNPG Cluster CR (e.g. "Cluster in
	// healthy state"). Empty if the site is unreachable.
	//
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message carries a free-form detail — typically the reason for a
	// failure (invalid kubeconfig, K8s Get error, readyInstances=0). Empty
	// when the site is reachable and ready.
	//
	// +optional
	Message string `json:"message,omitempty"`

	// LastObservedTime is the timestamp of the Reconcile that produced
	// this observation. Updated on every pass, even without a state change.
	//
	// +optional
	LastObservedTime *metav1.Time `json:"lastObservedTime,omitempty"`
}

// HAClusterStatus describes the observed state of an HACluster.
type HAClusterStatus struct {
	// CurrentPrimarySite: name of the last site accepted as primary by the
	// operator. On a failover, this field is updated AFTER the CNPG-side
	// promotion has happened (never before). If that site becomes
	// transiently unhealthy, the field stays set and the unavailability is
	// surfaced through the conditions.
	//
	// +optional
	CurrentPrimarySite string `json:"currentPrimarySite,omitempty"`

	// LastFailoverTime: timestamp of the last successful failover.
	//
	// +optional
	LastFailoverTime *metav1.Time `json:"lastFailoverTime,omitempty"`

	// ObservedGeneration: metadata.generation processed by the last
	// Reconcile. Tells callers whether the status reflects the current spec.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Sites: per-site detailed observation (primary + replicas). Enables
	// quick inspection via
	// `kubectl get hacluster -o jsonpath='{.status.sites}'` and underpins
	// the promotion logic.
	//
	// +listType=map
	// +listMapKey=name
	// +optional
	Sites []SiteStatus `json:"sites,omitempty"`

	// Conditions: state of the functional aspects of the HACluster.
	// Expected types: "Available", "Progressing", "Degraded",
	// "FailoverInProgress".
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=hac;hacluster
// +kubebuilder:printcolumn:name="Primary",type=string,JSONPath=`.status.currentPrimarySite`
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=`.status.conditions[?(@.type=="Available")].status`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.failover.mode`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// HACluster orchestrates the cross-cluster failover of a group of CNPG
// Clusters.
type HACluster struct {
	metav1.TypeMeta `json:",inline"`

	// Standard metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// Spec defines the desired state.
	// +required
	Spec HAClusterSpec `json:"spec"`

	// Status reflects the observed state.
	// +optional
	Status HAClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// HAClusterList contains a list of HACluster.
type HAClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []HACluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HACluster{}, &HAClusterList{})
}
