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
	// direct PostgreSQL probe.
	ReplicaLagSeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: subsystem + "_replica_lag_seconds",
		Help: "PostgreSQL replay lag in seconds observed through the optional direct probe.",
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
