/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package metrics declares the Prometheus collectors exported by cnpg-ha.
//
// The collectors are package-level globals: a metrics registry is the
// canonical justified exception to the "no mutable globals" rule
// (CONVENTION §4). They are registered once into controller-runtime's
// shared registry via MustRegister, so they are served on the manager's
// existing metrics endpoint alongside the controller-runtime defaults
// (which we deliberately do not duplicate).
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const subsystem = "cnpg_ha"

var (
	// CurrentPrimarySite is 1 for the site currently serving writes and 0
	// for every other site of the HACluster.
	CurrentPrimarySite = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: subsystem + "_current_primary_site",
		Help: "1 if the site is the current primary of the HACluster, else 0.",
	}, []string{"hacluster", "namespace", "site"})

	// SiteReachable is 1 when the site's Kubernetes API answered during the
	// last reconcile, else 0.
	SiteReachable = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: subsystem + "_site_reachable",
		Help: "1 if the site's Kubernetes API was reachable at the last reconcile, else 0.",
	}, []string{"hacluster", "namespace", "site"})

	// SiteReady is 1 when the site's CNPG Cluster reported at least one
	// ready instance at the last reconcile, else 0.
	SiteReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: subsystem + "_site_ready",
		Help: "1 if the site's CNPG Cluster had a ready instance at the last reconcile, else 0.",
	}, []string{"hacluster", "namespace", "site"})

	// ReplicaLagSeconds is the replay lag observed through the optional
	// direct PostgreSQL probe. NOTE: this is the clock-based metric
	// (clock_timestamp() - pg_last_xact_replay_timestamp()) and therefore
	// keeps growing on an idle primary even though the replica is fully
	// caught up at the WAL level. Prefer ReplicaLagBytes for dashboards.
	ReplicaLagSeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: subsystem + "_replica_lag_seconds",
		Help: "PostgreSQL replay lag in seconds (clock_timestamp - pg_last_xact_replay_timestamp); inflated by primary idleness, prefer cnpg_ha_replica_lag_bytes for true behindness.",
	}, []string{"hacluster", "namespace", "site"})

	// ReplicaLagBytes is the WAL distance the site is behind the current
	// primary in bytes, computed from the per-site LSNs published by the
	// optional direct PostgreSQL probe. It is 0 on the current primary,
	// 0 on a caught-up replica regardless of primary traffic, and only
	// grows when there is actual streaming or apply backlog. The metric
	// is cleared when no LSN is known for either side (probe disabled
	// or transient connection failure).
	ReplicaLagBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: subsystem + "_replica_lag_bytes",
		Help: "WAL bytes the site is behind the current primary, computed from per-site LSN gaps; 0 on the primary and on caught-up replicas.",
	}, []string{"hacluster", "namespace", "site"})

	// SplitBrain is 1 when more than one site was observed as CNPG-primary
	// and ready (writes may diverge), else 0.
	SplitBrain = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: subsystem + "_split_brain",
		Help: "1 if multiple sites were observed as CNPG-primary and ready, else 0.",
	}, []string{"hacluster", "namespace"})

	// FailoverTotal counts completed promotions, labelled by trigger mode
	// ("manual" or "automatic").
	FailoverTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: subsystem + "_failover_total",
		Help: "Number of completed failovers, by trigger mode.",
	}, []string{"hacluster", "namespace", "mode"})

	// FailoverDurationSeconds measures successful promotion duration, labelled
	// by trigger mode ("manual" or "automatic").
	FailoverDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    subsystem + "_failover_duration_seconds",
		Help:    "Duration in seconds of successful failover promotion sequences.",
		Buckets: prometheus.DefBuckets,
	}, []string{"hacluster", "namespace", "mode"})
)

// MustRegister registers every cnpg-ha collector into controller-runtime's
// shared registry so they are served on the manager's metrics endpoint.
// Call once from main before the manager starts. Panics on double
// registration (a programming error).
func MustRegister() {
	ctrlmetrics.Registry.MustRegister(
		CurrentPrimarySite,
		SiteReachable,
		SiteReady,
		ReplicaLagSeconds,
		ReplicaLagBytes,
		SplitBrain,
		FailoverTotal,
		FailoverDurationSeconds,
	)
}

// boolGauge maps a bool to the 1/0 Prometheus convention.
func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// SetSite publishes the per-site gauges for one reconcile pass.
func SetSite(haNamespace, haName, site string, isPrimary, reachable, ready bool) {
	CurrentPrimarySite.WithLabelValues(haName, haNamespace, site).Set(boolGauge(isPrimary))
	SiteReachable.WithLabelValues(haName, haNamespace, site).Set(boolGauge(reachable))
	SiteReady.WithLabelValues(haName, haNamespace, site).Set(boolGauge(ready))
}

// SetReplicaLag publishes the replay lag gauge for a site.
func SetReplicaLag(haNamespace, haName, site string, seconds float64) {
	ReplicaLagSeconds.WithLabelValues(haName, haNamespace, site).Set(seconds)
}

// ClearReplicaLag removes a stale replay lag gauge when the probe no longer
// reports a value for a site.
func ClearReplicaLag(haNamespace, haName, site string) {
	ReplicaLagSeconds.DeleteLabelValues(haName, haNamespace, site)
}

// SetReplicaLagBytes publishes the WAL byte gap for a site relative to the
// current primary. Callers pass 0 on the current primary and on caught-up
// replicas (a negative gap is impossible after the uint64 underflow guard
// in the controller).
func SetReplicaLagBytes(haNamespace, haName, site string, bytes float64) {
	ReplicaLagBytes.WithLabelValues(haName, haNamespace, site).Set(bytes)
}

// ClearReplicaLagBytes removes a stale WAL gap gauge when the primary's
// LSN or the site's LSN is unknown (probe disabled, transient failure,
// site unreachable).
func ClearReplicaLagBytes(haNamespace, haName, site string) {
	ReplicaLagBytes.DeleteLabelValues(haName, haNamespace, site)
}

// SetSplitBrain publishes the split-brain gauge for one HACluster.
func SetSplitBrain(haNamespace, haName string, splitBrain bool) {
	SplitBrain.WithLabelValues(haName, haNamespace).Set(boolGauge(splitBrain))
}

// IncFailover records a completed failover. mode is "manual" or "automatic".
func IncFailover(haNamespace, haName, mode string) {
	FailoverTotal.WithLabelValues(haName, haNamespace, mode).Inc()
}

// ObserveFailoverDuration records the duration of a successful failover.
// mode is "manual" or "automatic".
func ObserveFailoverDuration(haNamespace, haName, mode string, seconds float64) {
	FailoverDurationSeconds.WithLabelValues(haName, haNamespace, mode).Observe(seconds)
}
