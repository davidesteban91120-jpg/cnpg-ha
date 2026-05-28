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
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	hav1alpha1 "github.com/davidesteban/cnpg-ha/api/v1alpha1"
	"github.com/davidesteban/cnpg-ha/internal/health"
	hametrics "github.com/davidesteban/cnpg-ha/internal/metrics"
	"github.com/davidesteban/cnpg-ha/internal/postgres"
	"github.com/davidesteban/cnpg-ha/internal/promotion"
	"github.com/davidesteban/cnpg-ha/internal/remoteclient"
)

const (
	// requeuePeriod is the periodic re-Reconcile interval used to detect
	// drift (a site going down between two watch events).
	requeuePeriod = 30 * time.Second

	// Condition types exposed on HACluster.status.conditions.
	conditionAvailable          = "Available"
	conditionDegraded           = "Degraded"
	conditionFailoverInProgress = "FailoverInProgress"
	conditionSplitBrain         = "SplitBrain"

	// annotationPromote, when present on an HACluster, instructs the
	// operator to promote the named site (must be one of Spec.Replicas[].Name)
	// to primary. Only honored when Spec.Failover.Mode == Manual.
	annotationPromote = "ha.cnpg.io/promote"

	// Event reasons emitted by the reconciler.
	eventReasonFailoverStarted         = "FailoverStarted"
	eventReasonFailoverCompleted       = "FailoverCompleted"
	eventReasonFailoverFailed          = "FailoverFailed"
	eventReasonPromoteRejected         = "PromoteRejected"
	eventReasonRejoinFenced            = "RejoinFenced"
	eventReasonRejoinReconfigured      = "RejoinReconfigured"
	eventReasonAutoFailoverNoCandidate = "AutoFailoverNoCandidate"
	eventReasonPrimaryUnhealthy        = "PrimaryUnhealthy"

	// Effective defaults applied when the spec leaves the field at zero
	// (envtest / hand-built objects bypass the CRD defaults).
	defaultFailureThreshold    = 3
	defaultHealthCheckInterval = 10 * time.Second

	// minStabilizationCooldown is the floor for the post-failover window
	// during which automatic failover is suspended (CNPG promotion restart
	// of the new primary typically takes ~10–30s).
	minStabilizationCooldown = 30 * time.Second

	// postgresProbeTimeout bounds each optional direct SQL probe so a slow
	// database endpoint cannot stall the Reconcile loop.
	postgresProbeTimeout = 5 * time.Second
)

// cnpgClusterGVK is the GVK of the CNPG Cluster CR. Single source of truth
// lives in internal/health; aliased here so existing call sites and tests
// keep one name.
var cnpgClusterGVK = health.CNPGClusterGVK

// HAClusterReconciler reconciles a HACluster object.
//
// Two responsibilities:
//
//  1. Observation: read every site's CNPG Cluster (local for the primary,
//     remote via kubeconfig Secret for replicas), populate Status.Sites
//     and Conditions (Available, Degraded).
//
//  2. Manual failover: when an HACluster carries the annotation
//     ha.cnpg.io/promote=<site> AND Spec.Failover.Mode == Manual, run the
//     promotion sequence on the targeted replica site:
//     Fence(old) → FlipCiliumService(old, RoleRemote) →
//     Promote(new) → FlipCiliumService(new, RoleLocal).
//     Each step is idempotent so a partial failure can be retried.
type HAClusterReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	RemoteClients  *remoteclient.Cache
	Recorder       events.EventRecorder
	PostgresProber postgres.Prober

	// failMu guards failCount. failCount holds, per HACluster, the number of
	// consecutive reconciles where the current primary was observed
	// unhealthy. It is intentionally in-memory only: on operator restart it
	// resets to 0 and re-converges (ARCHITECTURE §3, invariant 2).
	failMu    sync.Mutex
	failCount map[types.NamespacedName]int
}

// siteObservation is the controller-side view of a site for one Reconcile.
// It adapts health.SiteHealth and carries the site name; status mapping,
// decide/split-brain and target selection all work off this type.
type siteObservation struct {
	name       string
	reachable  bool   // the site's K8s API answered
	primary    bool   // CNPG Cluster is in primary mode (replica.enabled absent/false)
	ready      bool   // at least one ready instance
	phase      string // status.phase reported by CNPG
	reason     string // short explanation when not ready/unreachable
	timelineID int64  // status.timelineID — coarse advancement proxy (no true LSN)
	lsnKnown   bool
	lsn        string
	lsnValue   uint64
	lagSeconds *float64
}

// siteObsFromHealth adapts a health.SiteHealth into the controller's
// siteObservation, attaching the site name.
func siteObsFromHealth(name string, h health.SiteHealth) siteObservation {
	return siteObservation{
		name:       name,
		reachable:  h.Reachable,
		primary:    h.Primary,
		ready:      h.Ready,
		phase:      h.Phase,
		reason:     h.Reason,
		timelineID: h.TimelineID,
		lsnKnown:   h.LSNKnown,
		lsn:        h.LSN,
		lsnValue:   h.LSNValue,
		lagSeconds: h.LagSeconds,
	}
}

// +kubebuilder:rbac:groups=ha.cnpg.io,resources=haclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ha.cnpg.io,resources=haclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ha.cnpg.io,resources=haclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch;update;patch

// Reconcile observes every site and, when an explicit promotion annotation
// is present, runs the failover sequence before refreshing the status.
func (r *HAClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var ha hav1alpha1.HACluster
	if err := r.Get(ctx, req.NamespacedName, &ha); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("reconciling", "primary", ha.Spec.Primary.ClusterRef, "replicas", len(ha.Spec.Replicas))

	// Last primary the operator itself committed (survives across reconciles).
	// Used as the source of truth for topology reconciliation when the live
	// observation is ambiguous (e.g. a returning old primary causes a
	// transient split-brain where decideCurrentPrimary can't pick one).
	priorPrimary := ha.Status.CurrentPrimarySite

	// 1. Observe the primary (local client).
	primaryObs := r.observePrimary(ctx, &ha)

	// 2. Observe each replica (remote client via the kubeconfig Secret).
	replicaObs := make([]siteObservation, 0, len(ha.Spec.Replicas))
	for _, rep := range ha.Spec.Replicas {
		replicaObs = append(replicaObs, r.observeReplica(ctx, &ha, rep))
	}

	// 3. Honor a manual promotion request, if any (mode=Manual only).
	// Local name avoids shadowing the `promotion` package imported above.
	promoEffect, promoteErr := r.handlePromoteAnnotation(ctx, &ha, primaryObs, replicaObs)

	// 3bis. Automatic failover (mode=Automatic only): if the current primary
	// has been unhealthy for failureThreshold consecutive reconciles, promote
	// a replica without any annotation.
	if promoteErr == nil {
		autoEffect, err := r.handleAutomaticFailover(ctx, &ha, primaryObs, replicaObs)
		if err != nil {
			promoteErr = err
		} else if autoEffect.didPromote() {
			promoEffect = autoEffect
		}
	}
	applyPromotionEffect(promoEffect, &primaryObs, replicaObs)

	// 4. Decide the current primary from the post-promotion observations.
	currentPrimary, available := decideCurrentPrimary(primaryObs, replicaObs)
	splitBrain := detectSplitBrain(primaryObs, replicaObs)

	// 5. Update the status.
	ha.Status.ObservedGeneration = ha.Generation
	// Keep the last accepted primary when no healthy primary is observed.
	// Availability is carried by conditions; the failover state machine still
	// needs a stable "old primary" across thresholded reconciles.
	if currentPrimary != "" {
		ha.Status.CurrentPrimarySite = currentPrimary
	} else {
		ha.Status.CurrentPrimarySite = priorPrimary
	}
	ha.Status.Sites = buildSiteStatuses(primaryObs, replicaObs, metav1.Now())
	r.setConditions(&ha, available, splitBrain, primaryObs, replicaObs)

	r.publishMetrics(&ha, currentPrimary, len(splitBrain) > 0, primaryObs, replicaObs)

	if err := r.Status().Update(ctx, &ha); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}

	if promoteErr != nil {
		// Surface promotion failures to controller-runtime so it requeues
		// with backoff. The annotation stays in place — next reconcile retries.
		return ctrl.Result{}, promoteErr
	}

	// 6. Converge the topology of the other sites toward the current primary:
	// re-point surviving replicas, and handle a returning old primary per
	// RejoinPolicy. Runs only when promotion didn't fail this round.
	effectivePrimary := currentPrimary
	if effectivePrimary == "" {
		effectivePrimary = priorPrimary
	}
	if err := r.reconcileReplicaTopology(ctx, &ha, effectivePrimary, primaryObs, replicaObs); err != nil {
		return ctrl.Result{}, err
	}

	// In Automatic mode the failure counter must advance at the configured
	// cadence, so requeue every healthCheckIntervalSeconds rather than the
	// slower default observe period.
	requeue := requeuePeriod
	if ha.Spec.Failover.Mode == hav1alpha1.FailoverModeAutomatic {
		requeue = effectiveHealthInterval(ha.Spec.Failover)
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// handleAutomaticFailover promotes a replica without an annotation when the
// current primary has been observed unhealthy for failureThreshold
// consecutive reconciles. Active only when spec.failover.mode == Automatic.
//
// Safety guards (ARCHITECTURE §7.1):
//   - never acts while a split-brain is observed (ambiguous);
//   - acts only after a sustained failure (threshold), not a transient blip;
//   - aborts cleanly when no healthy replica candidate exists.
//
// Observation buffers are read-only here. On success the function returns a
// non-zero promotionEffect that the caller (Reconcile) applies via
// applyPromotionEffect so the rest of the loop sees the new primary.
func (r *HAClusterReconciler) handleAutomaticFailover(
	ctx context.Context,
	ha *hav1alpha1.HACluster,
	primaryObs siteObservation,
	replicaObs []siteObservation,
) (promotionEffect, error) {
	log := logf.FromContext(ctx)
	key := types.NamespacedName{Namespace: ha.Namespace, Name: ha.Name}

	if ha.Spec.Failover.Mode != hav1alpha1.FailoverModeAutomatic {
		return promotionEffect{}, nil
	}
	if sb := detectSplitBrain(primaryObs, replicaObs); len(sb) > 0 {
		log.Info("split-brain observed, automatic failover suspended", "sites", sb)
		r.resetFailCount(key)
		return promotionEffect{}, nil
	}

	// Post-failover stabilization. A just-promoted primary briefly reports
	// unhealthy while CNPG performs the promotion restart/switchover. Without
	// a cooldown the failure counter would re-trigger and cascade through
	// every replica (flapping). Suppress automatic failover — observe only —
	// until the new primary has had time to settle. Uses the persisted
	// LastFailoverTime so the cooldown also holds across an operator restart.
	if lf := ha.Status.LastFailoverTime; lf != nil {
		cooldown := stabilizationCooldown(ha.Spec.Failover)
		if since := metav1.Now().Sub(lf.Time); since < cooldown {
			log.Info("post-failover stabilization, automatic failover suspended",
				"sinceFailover", since.Truncate(time.Second), "cooldown", cooldown)
			r.resetFailCount(key)
			return promotionEffect{}, nil
		}
	}

	curName := ha.Status.CurrentPrimarySite
	if curName == "" {
		curName = ha.Spec.Primary.Name
	}
	cur := findObservation(curName, primaryObs, replicaObs)
	healthy := cur != nil && cur.reachable && cur.primary && cur.ready
	if healthy {
		r.resetFailCount(key)
		return promotionEffect{}, nil
	}

	threshold := effectiveFailureThreshold(ha.Spec.Failover)
	n := r.bumpFailCount(key)
	if n < threshold {
		msg := fmt.Sprintf("primary %q unhealthy (%d/%d) — automatic failover pending", curName, n, threshold)
		log.Info(msg)
		r.event(ha, corev1.EventTypeWarning, eventReasonPrimaryUnhealthy, msg)
		return promotionEffect{}, nil
	}

	target, _, ok := chooseTarget(ha, replicaObs, curName)
	if !ok {
		msg := fmt.Sprintf("primary %q down but no healthy replica to promote — staying unavailable", curName)
		log.Info(msg)
		r.event(ha, corev1.EventTypeWarning, eventReasonAutoFailoverNoCandidate, msg)
		return promotionEffect{}, nil
	}

	r.event(ha, corev1.EventTypeNormal, eventReasonFailoverStarted,
		fmt.Sprintf("promoting site %q to primary (automatic, primary %q unhealthy %d/%d)", target.Name, curName, n, threshold))
	r.setFailoverInProgress(ha, true, target.Name)

	start := time.Now()
	if err := r.runPromotion(ctx, ha, curName, target); err != nil {
		r.event(ha, corev1.EventTypeWarning, eventReasonFailoverFailed,
			fmt.Sprintf("automatic promotion of %q failed: %v", target.Name, err))
		r.setFailoverInProgress(ha, false, "")
		return promotionEffect{}, fmt.Errorf("automatic failover to %q: %w", target.Name, err)
	}
	hametrics.ObserveFailoverDuration(ha.Namespace, ha.Name, "automatic", time.Since(start).Seconds())

	ha.Status.CurrentPrimarySite = target.Name
	now := metav1.Now()
	ha.Status.LastFailoverTime = &now
	r.setFailoverInProgress(ha, false, "")
	r.event(ha, corev1.EventTypeNormal, eventReasonFailoverCompleted,
		fmt.Sprintf("site %q is now primary (automatic)", target.Name))
	hametrics.IncFailover(ha.Namespace, ha.Name, "automatic")
	r.resetFailCount(key)

	return promotionEffect{demoted: curName, promoted: target.Name}, nil
}

// chooseTarget selects the replica to promote during an automatic failover,
// honoring spec.failover.promotionPolicy. A candidate is eligible when it is
// reachable, ready, still a CNPG replica, and not the failed primary itself.
//
// Policy:
//   - Ordered: first eligible replica in spec order.
//   - MostAdvancedLSN: eligible replica with the highest PostgreSQL LSN when
//     the optional postgresProbe is configured. Candidates with an observed
//     LSN beat candidates without one. If no candidate has an LSN, the
//     function falls back to status.timelineID and keeps spec-order ties.
//
// Any unset/unknown policy falls back to Ordered (deterministic).
func chooseTarget(
	ha *hav1alpha1.HACluster, replicaObs []siteObservation, failedPrimary string,
) (hav1alpha1.ReplicaSite, int, bool) {
	mostAdvanced := ha.Spec.Failover.PromotionPolicy == hav1alpha1.PromotionPolicyMostAdvancedLSN
	best := -1
	for i, rep := range ha.Spec.Replicas {
		if rep.Name == failedPrimary {
			continue
		}
		o := replicaObs[i]
		if !o.reachable || !o.ready || o.primary {
			continue
		}
		if best == -1 {
			best = i
			if !mostAdvanced {
				break // Ordered: first eligible wins
			}
			continue
		}
		if moreAdvanced(replicaObs[i], replicaObs[best]) {
			best = i
		}
	}
	if best == -1 {
		return hav1alpha1.ReplicaSite{}, 0, false
	}
	return ha.Spec.Replicas[best], best, true
}

func moreAdvanced(candidate, current siteObservation) bool {
	if candidate.lsnKnown != current.lsnKnown {
		return candidate.lsnKnown
	}
	if candidate.lsnKnown {
		return candidate.lsnValue > current.lsnValue
	}
	return candidate.timelineID > current.timelineID
}

// reconcileReplicaTopology makes every site other than effectivePrimary
// follow it:
//
//   - a reachable site still in CNPG-replica mode is re-pointed at the
//     current primary's ReplicationEndpoint (idempotent — a no-op when it
//     already streams from there);
//   - a reachable site observed as CNPG-primary that is NOT effectivePrimary
//     is a stray / returning old primary. Per spec.failover.rejoinPolicy:
//     Manual (default) fences it (the SplitBrain condition already flags it),
//     AutoReplica rebuilds it as a replica of effectivePrimary (destructive,
//     CNPG performs the resync).
//
// No-ops (returns nil) when there is no known primary or it has no
// ReplicationEndpoint set — topology reconfiguration is strictly opt-in.
func (r *HAClusterReconciler) reconcileReplicaTopology(
	ctx context.Context,
	ha *hav1alpha1.HACluster,
	effectivePrimary string,
	primaryObs siteObservation,
	replicaObs []siteObservation,
) error {
	log := logf.FromContext(ctx)

	if effectivePrimary == "" {
		return nil
	}
	endpoint := replicationEndpointFor(ha, effectivePrimary)
	if endpoint == "" {
		log.V(1).Info("no replicationEndpoint for current primary, skipping topology reconcile",
			"primary", effectivePrimary)
		return nil
	}

	rejoin := ha.Spec.Failover.RejoinPolicy
	if rejoin == "" {
		rejoin = hav1alpha1.RejoinPolicyManual
	}

	all := append([]siteObservation{primaryObs}, replicaObs...)
	for _, obs := range all {
		if obs.name == effectivePrimary {
			continue
		}
		cli, ref, ok := r.siteClientAndRef(ctx, ha, obs.name)
		if !ok {
			continue
		}

		// Authoritative re-read. The observation buffer passed in was taken
		// BEFORE this reconcile's promotion writes and is deliberately
		// mutated by handle{Promote,AutomaticFailover} so the status reflects
		// the post-failover reality. It must therefore NOT drive the
		// fence-vs-reconfigure decision: a just-demoted old primary still
		// has spec.replica.enabled=false on the API server, and only a
		// fresh read tells us so. Trusting the mutated buffer would treat it
		// as a surviving replica and silently rebuild it — bypassing
		// rejoinPolicy=Manual (a data-safety guard).
		fresh := siteObsFromHealth(obs.name, health.Probe(ctx, cli,
			types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}))
		if !fresh.reachable {
			continue
		}

		switch {
		case fresh.primary:
			// Stray / returning primary while another site is primary.
			if rejoin == hav1alpha1.RejoinPolicyAutoReplica {
				if err := promotion.Reconfigure(ctx, cli, ref, endpoint); err != nil {
					return fmt.Errorf("auto-rejoin %q as replica: %w", obs.name, err)
				}
				r.event(ha, corev1.EventTypeWarning, eventReasonRejoinReconfigured,
					fmt.Sprintf("returning primary %q rebuilt as replica of %q (data discarded)", obs.name, effectivePrimary))
				continue
			}
			if err := promotion.Fence(ctx, cli, ref); err != nil {
				return fmt.Errorf("fence returning primary %q: %w", obs.name, err)
			}
			r.event(ha, corev1.EventTypeWarning, eventReasonRejoinFenced,
				fmt.Sprintf("returning primary %q fenced (rejoinPolicy=Manual); resolve split-brain manually", obs.name))

		default:
			// Surviving replica: ensure it streams from the current primary.
			if err := promotion.Reconfigure(ctx, cli, ref, endpoint); err != nil {
				return fmt.Errorf("repoint replica %q to %q: %w", obs.name, effectivePrimary, err)
			}
		}
	}
	return nil
}

// replicationEndpointFor returns the ReplicationEndpoint declared for the
// named site, or "" when the site is unknown or has none set.
func replicationEndpointFor(ha *hav1alpha1.HACluster, siteName string) string {
	if siteName == ha.Spec.Primary.Name {
		return ha.Spec.Primary.ReplicationEndpoint
	}
	for _, rep := range ha.Spec.Replicas {
		if rep.Name == siteName {
			return rep.ReplicationEndpoint
		}
	}
	return ""
}

// siteClientAndRef resolves the K8s client and CNPG Cluster Ref for a site:
// the local client for the declared primary site, a cached remote client
// (built from the kubeconfig Secret) for replica sites. ok=false when the
// site is unknown or its remote client cannot be built.
func (r *HAClusterReconciler) siteClientAndRef(
	ctx context.Context, ha *hav1alpha1.HACluster, siteName string,
) (client.Client, promotion.Ref, bool) {
	if siteName == ha.Spec.Primary.Name {
		return r.Client, promotion.Ref{
			Namespace: ha.Spec.Primary.ClusterRef.Namespace,
			Name:      ha.Spec.Primary.ClusterRef.Name,
		}, true
	}
	for _, rep := range ha.Spec.Replicas {
		if rep.Name != siteName {
			continue
		}
		cli, err := r.RemoteClients.GetOrCreate(ctx, r.Client, ha.Namespace, rep.KubeconfigSecretRef)
		if err != nil {
			return nil, promotion.Ref{}, false
		}
		return cli, promotion.Ref{
			Namespace: rep.ClusterRef.Namespace,
			Name:      rep.ClusterRef.Name,
		}, true
	}
	return nil, promotion.Ref{}, false
}

// handlePromoteAnnotation runs the manual failover sequence when the
// HACluster carries the ha.cnpg.io/promote annotation. Returns nil when
// there is nothing to do (annotation absent) or when the promotion has
// completed successfully. Returns an error to trigger a requeue when the
// promotion is in progress or has failed mid-way.
//
// On success the function returns a non-zero promotionEffect that the caller
// applies to keep observations in sync with the post-failover reality so the
// same Reconcile reports the correct status.
func (r *HAClusterReconciler) handlePromoteAnnotation(
	ctx context.Context,
	ha *hav1alpha1.HACluster,
	primaryObs siteObservation,
	replicaObs []siteObservation,
) (promotionEffect, error) {
	log := logf.FromContext(ctx)

	target, ok := ha.Annotations[annotationPromote]
	if !ok {
		return promotionEffect{}, nil
	}

	if ha.Spec.Failover.Mode != hav1alpha1.FailoverModeManual {
		msg := fmt.Sprintf("annotation %s rejected: spec.failover.mode is %q, must be Manual", annotationPromote, ha.Spec.Failover.Mode)
		log.Info(msg)
		r.event(ha, corev1.EventTypeWarning, eventReasonPromoteRejected, msg)
		return promotionEffect{}, r.clearPromoteAnnotation(ctx, ha)
	}

	rep, replicaIdx, found := findReplica(ha, target)
	if !found {
		msg := fmt.Sprintf("annotation %s=%q rejected: no replica with that name", annotationPromote, target)
		log.Info(msg)
		r.event(ha, corev1.EventTypeWarning, eventReasonPromoteRejected, msg)
		return promotionEffect{}, r.clearPromoteAnnotation(ctx, ha)
	}

	targetObs := replicaObs[replicaIdx]
	if !targetObs.reachable || !targetObs.ready || targetObs.primary {
		msg := fmt.Sprintf("annotation %s=%q rejected: target not a healthy replica (reachable=%v ready=%v primary=%v reason=%q)",
			annotationPromote, target, targetObs.reachable, targetObs.ready, targetObs.primary, targetObs.reason)
		log.Info(msg)
		r.event(ha, corev1.EventTypeWarning, eventReasonPromoteRejected, msg)
		return promotionEffect{}, r.clearPromoteAnnotation(ctx, ha)
	}

	oldPrimaryName, err := currentPrimaryForPromotion(ha, primaryObs, replicaObs)
	if err != nil {
		msg := fmt.Sprintf("annotation %s=%q rejected: %v", annotationPromote, target, err)
		log.Info(msg)
		r.event(ha, corev1.EventTypeWarning, eventReasonPromoteRejected, msg)
		return promotionEffect{}, r.clearPromoteAnnotation(ctx, ha)
	}

	r.event(ha, corev1.EventTypeNormal, eventReasonFailoverStarted,
		fmt.Sprintf("promoting site %q to primary (manual)", target))
	r.setFailoverInProgress(ha, true, target)

	start := time.Now()
	if err := r.runPromotion(ctx, ha, oldPrimaryName, rep); err != nil {
		r.event(ha, corev1.EventTypeWarning, eventReasonFailoverFailed,
			fmt.Sprintf("promotion of %q failed: %v", target, err))
		r.setFailoverInProgress(ha, false, "")
		return promotionEffect{}, fmt.Errorf("manual promotion to %q: %w", target, err)
	}
	hametrics.ObserveFailoverDuration(ha.Namespace, ha.Name, "manual", time.Since(start).Seconds())

	if err := r.clearPromoteAnnotation(ctx, ha); err != nil {
		return promotionEffect{}, fmt.Errorf("clear promote annotation: %w", err)
	}

	ha.Status.CurrentPrimarySite = target
	now := metav1.Now()
	ha.Status.LastFailoverTime = &now
	r.setFailoverInProgress(ha, false, "")
	r.event(ha, corev1.EventTypeNormal, eventReasonFailoverCompleted,
		fmt.Sprintf("site %q is now primary", target))
	hametrics.IncFailover(ha.Namespace, ha.Name, "manual")

	return promotionEffect{demoted: oldPrimaryName, promoted: replicaObs[replicaIdx].name}, nil
}

// currentPrimaryForPromotion returns the site to demote before a manual
// promotion. The persisted status is the operator's source of truth after an
// earlier failover; observed state is used only when status has not been
// initialized yet. Split-brain is rejected because a manual promotion must not
// guess which writer to fence.
func currentPrimaryForPromotion(
	ha *hav1alpha1.HACluster,
	primaryObs siteObservation,
	replicaObs []siteObservation,
) (string, error) {
	if splitBrain := detectSplitBrain(primaryObs, replicaObs); len(splitBrain) > 0 {
		return "", fmt.Errorf("split-brain observed among sites %v", splitBrain)
	}
	if ha.Status.CurrentPrimarySite != "" {
		return ha.Status.CurrentPrimarySite, nil
	}
	if observed, ok := decideCurrentPrimary(primaryObs, replicaObs); ok {
		return observed, nil
	}
	return ha.Spec.Primary.Name, nil
}

// promotionEffect describes the role change a successful promotion has
// applied. Returned by handlePromoteAnnotation / handleAutomaticFailover so
// the caller — Reconcile — can update the in-memory observation buffers in
// one place, rather than each handler silently mutating its inputs.
//
// The zero value (both fields empty) means "no promotion happened this
// reconcile", and applyPromotionEffect is a no-op for it.
type promotionEffect struct {
	demoted  string // name of the site that lost its primary role
	promoted string // name of the site that gained the primary role
}

// didPromote reports whether the effect represents an actual role change.
func (e promotionEffect) didPromote() bool { return e.promoted != "" }

// applyPromotionEffect mirrors the post-promotion topology in the in-memory
// observation buffers so the rest of the Reconcile (decideCurrentPrimary,
// detectSplitBrain, buildSiteStatuses, setConditions, publishMetrics) sees
// the new primary immediately, without waiting for the next observation
// pass.
//
// Important: reconcileReplicaTopology deliberately re-Probes the API
// server and does NOT trust these mutated observations. A just-demoted old
// primary still has spec.replica.enabled=false on the API server and only
// a fresh read tells us so — trusting the mutated buffer would silently
// rebuild it as a replica, bypassing rejoinPolicy=Manual. See the comment
// in reconcileReplicaTopology for the data-safety reasoning.
func applyPromotionEffect(e promotionEffect, primaryObs *siteObservation, replicaObs []siteObservation) {
	if !e.didPromote() {
		return
	}
	setObservationRole(e.demoted, false, primaryObs, replicaObs)
	setObservationRole(e.promoted, true, primaryObs, replicaObs)
}

// setObservationRole flips the .primary (and .ready, when demoting) of the
// site identified by name. Internal helper of applyPromotionEffect.
func setObservationRole(siteName string, primary bool, primaryObs *siteObservation, replicaObs []siteObservation) {
	set := func(o *siteObservation) {
		o.primary = primary
		if !primary {
			o.ready = false
		}
	}
	if primaryObs.name == siteName {
		set(primaryObs)
		return
	}
	for i := range replicaObs {
		if replicaObs[i].name == siteName {
			set(&replicaObs[i])
			return
		}
	}
}

// runPromotion executes the steps that move write traffic from the declared
// current primary to the chosen replica site.
//
// The steps targeting the OLD primary (Fence, Cilium flip to remote) are
// best-effort: in a disaster-recovery failover the old site is typically
// gone, so its CNPG Cluster / -rw Service no longer exist. A NotFound there
// means the old primary physically cannot accept writes — there is nothing
// to fence — so we log and continue instead of aborting the failover.
//
// The steps targeting the NEW primary (Promote, Cilium flip to local) stay
// strict: if the target replica is unreachable the failover genuinely fails.
func (r *HAClusterReconciler) runPromotion(
	ctx context.Context,
	ha *hav1alpha1.HACluster,
	oldPrimaryName string,
	target hav1alpha1.ReplicaSite,
) error {
	log := logf.FromContext(ctx)

	if oldPrimaryName == "" {
		return fmt.Errorf("old primary site is empty")
	}
	if oldPrimaryName == target.Name {
		return fmt.Errorf("old primary site %q and target site are identical", oldPrimaryName)
	}

	oldCli, oldRef, ok := r.siteClientAndRef(ctx, ha, oldPrimaryName)
	if !ok {
		return fmt.Errorf("resolve old primary site %q", oldPrimaryName)
	}
	newRef := promotion.Ref{
		Namespace: target.ClusterRef.Namespace,
		Name:      target.ClusterRef.Name,
	}

	newCli, err := r.RemoteClients.GetOrCreate(ctx, r.Client, ha.Namespace, target.KubeconfigSecretRef)
	if err != nil {
		return fmt.Errorf("load remote client for %q: %w", target.Name, err)
	}

	if err := promotion.Fence(ctx, oldCli, oldRef); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("fence old primary: %w", err)
		}
		log.Info("old primary CNPG Cluster not found, skipping fence (DR failover)", "ref", oldRef)
	}
	if err := promotion.FlipCiliumService(ctx, oldCli, oldRef, promotion.RoleRemote); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("flip old primary service to remote: %w", err)
		}
		log.Info("old primary -rw Service not found, skipping flip (DR failover)", "ref", oldRef)
	}
	if err := promotion.Promote(ctx, newCli, newRef); err != nil {
		return fmt.Errorf("promote target: %w", err)
	}
	if err := promotion.FlipCiliumService(ctx, newCli, newRef, promotion.RoleLocal); err != nil {
		return fmt.Errorf("flip target service to local: %w", err)
	}
	return nil
}

// findReplica returns the ReplicaSite matching name, its index in
// ha.Spec.Replicas, and ok=true if it exists.
func findReplica(ha *hav1alpha1.HACluster, name string) (hav1alpha1.ReplicaSite, int, bool) {
	for i, rep := range ha.Spec.Replicas {
		if rep.Name == name {
			return rep, i, true
		}
	}
	return hav1alpha1.ReplicaSite{}, 0, false
}

// clearPromoteAnnotation removes the manual promote annotation from the
// HACluster via a JSON merge patch. Patching avoids the full-object update
// race when the controller is rapidly reconciling.
func (r *HAClusterReconciler) clearPromoteAnnotation(ctx context.Context, ha *hav1alpha1.HACluster) error {
	if _, ok := ha.Annotations[annotationPromote]; !ok {
		return nil
	}
	patch := client.RawPatch(types.MergePatchType,
		fmt.Appendf(nil, `{"metadata":{"annotations":{"%s":null}}}`, annotationPromote))
	if err := r.Patch(ctx, ha, patch); err != nil {
		return fmt.Errorf("patch HACluster %s/%s: %w", ha.Namespace, ha.Name, err)
	}
	delete(ha.Annotations, annotationPromote)
	return nil
}

func (r *HAClusterReconciler) setFailoverInProgress(ha *hav1alpha1.HACluster, inProgress bool, target string) {
	cond := metav1.Condition{
		Type:               conditionFailoverInProgress,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: ha.Generation,
	}
	if inProgress {
		cond.Status = metav1.ConditionTrue
		if ha.Spec.Failover.Mode == hav1alpha1.FailoverModeAutomatic {
			cond.Reason = "AutomaticPromotion"
			cond.Message = fmt.Sprintf("promoting site %q (automatic)", target)
		} else {
			cond.Reason = "ManualPromotion"
			cond.Message = fmt.Sprintf("promoting site %q (manual)", target)
		}
	} else {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "Idle"
		cond.Message = "no failover in progress"
	}
	meta.SetStatusCondition(&ha.Status.Conditions, cond)
}

func (r *HAClusterReconciler) event(ha *hav1alpha1.HACluster, eventType, reason, message string) {
	if r.Recorder == nil {
		return
	}
	// New events API (k8s.io/client-go/tools/events). "related" is nil and
	// "action" reuses reason (a short CamelCase verb-ish token is acceptable
	// for the action field). note="%s" + message keeps the rendered event
	// text identical to the old record API.
	r.Recorder.Eventf(ha, nil, eventType, reason, reason, "%s", message)
}

// publishMetrics mirrors the just-computed status into the Prometheus
// collectors: one current_primary/reachable/ready triplet per site, the
// time-based and byte-based replica lag gauges, plus the split-brain
// gauge. Side-effect only — metrics globals are the justified exception
// to the no-mutable-globals rule (CONVENTION §4).
//
// The byte-lag is derived from per-site LSNs already collected by the
// optional direct PostgreSQL probe: we locate the current primary's LSN
// in this reconcile pass and publish max(0, primaryLSN - siteLSN) for
// every site. The uint64 underflow guard keeps a transiently-fenced
// or just-promoted replica (whose LSN may briefly exceed the recorded
// primary LSN) at 0 rather than 2^64, which would otherwise spike the
// dashboard. Sites without a known LSN — probe disabled, unreachable,
// or transient failure — have their byte-lag gauge cleared so stale
// values do not survive across reconciles.
func (r *HAClusterReconciler) publishMetrics(
	ha *hav1alpha1.HACluster, currentPrimary string, splitBrain bool,
	primaryObs siteObservation, replicaObs []siteObservation,
) {
	all := append([]siteObservation{primaryObs}, replicaObs...)

	var primaryLSN uint64
	var primaryLSNKnown bool
	for _, obs := range all {
		if obs.name == currentPrimary && obs.lsnKnown {
			primaryLSN = obs.lsnValue
			primaryLSNKnown = true
			break
		}
	}

	for _, obs := range all {
		hametrics.SetSite(ha.Namespace, ha.Name, obs.name,
			obs.name == currentPrimary, obs.reachable, obs.ready)
		if obs.lagSeconds != nil {
			hametrics.SetReplicaLag(ha.Namespace, ha.Name, obs.name, *obs.lagSeconds)
		} else {
			hametrics.ClearReplicaLag(ha.Namespace, ha.Name, obs.name)
		}

		switch {
		case !primaryLSNKnown || !obs.lsnKnown:
			hametrics.ClearReplicaLagBytes(ha.Namespace, ha.Name, obs.name)
		case obs.name == currentPrimary || obs.lsnValue >= primaryLSN:
			hametrics.SetReplicaLagBytes(ha.Namespace, ha.Name, obs.name, 0)
		default:
			hametrics.SetReplicaLagBytes(ha.Namespace, ha.Name, obs.name, float64(primaryLSN-obs.lsnValue))
		}

		if obs.lsnKnown {
			hametrics.SetSiteCurrentLSNBytes(ha.Namespace, ha.Name, obs.name, float64(obs.lsnValue))
		} else {
			hametrics.ClearSiteCurrentLSNBytes(ha.Namespace, ha.Name, obs.name)
		}
	}
	hametrics.SetSplitBrain(ha.Namespace, ha.Name, splitBrain)
}

// bumpFailCount increments and returns the consecutive-failure counter for
// key. resetFailCount clears it. Both are safe for concurrent reconciles.
func (r *HAClusterReconciler) bumpFailCount(key types.NamespacedName) int {
	r.failMu.Lock()
	defer r.failMu.Unlock()
	if r.failCount == nil {
		r.failCount = map[types.NamespacedName]int{}
	}
	r.failCount[key]++
	return r.failCount[key]
}

func (r *HAClusterReconciler) resetFailCount(key types.NamespacedName) {
	r.failMu.Lock()
	defer r.failMu.Unlock()
	delete(r.failCount, key)
}

// effectiveFailureThreshold / effectiveHealthInterval apply the CRD defaults
// when the field is left at zero (hand-built objects in tests, or an old
// stored object), so behavior matches a defaulted spec.
func effectiveFailureThreshold(f hav1alpha1.FailoverSpec) int {
	if f.FailureThreshold < 2 {
		return defaultFailureThreshold
	}
	return int(f.FailureThreshold)
}

func effectiveHealthInterval(f hav1alpha1.FailoverSpec) time.Duration {
	if f.HealthCheckIntervalSeconds < 1 {
		return defaultHealthCheckInterval
	}
	return time.Duration(f.HealthCheckIntervalSeconds) * time.Second
}

// stabilizationCooldown is how long automatic failover stays suspended
// after a failover, letting the freshly promoted primary finish CNPG's
// promotion restart before its health can trigger another failover.
// max(30s, 3 × healthCheckInterval): long enough for the restart, short
// enough that a genuinely failed new primary is still recovered promptly.
func stabilizationCooldown(f hav1alpha1.FailoverSpec) time.Duration {
	if c := 3 * effectiveHealthInterval(f); c > minStabilizationCooldown {
		return c
	}
	return minStabilizationCooldown
}

// findObservation returns the observation for siteName among the primary
// and replica observations, or nil when absent.
func findObservation(name string, primary siteObservation, replicas []siteObservation) *siteObservation {
	if primary.name == name {
		return &primary
	}
	for i := range replicas {
		if replicas[i].name == name {
			return &replicas[i]
		}
	}
	return nil
}

// observePrimary probes the declared primary site's CNPG Cluster via the
// local client (it lives in the operator's own cluster, or via the local
// kubeconfig under `make run`).
func (r *HAClusterReconciler) observePrimary(ctx context.Context, ha *hav1alpha1.HACluster) siteObservation {
	log := logf.FromContext(ctx)
	ref := types.NamespacedName{
		Namespace: ha.Spec.Primary.ClusterRef.Namespace,
		Name:      ha.Spec.Primary.ClusterRef.Name,
	}
	obs := siteObsFromHealth(ha.Spec.Primary.Name, health.Probe(ctx, r.Client, ref))
	r.probePostgres(ctx, &obs, r.Client, ha.Spec.Primary.ClusterRef, ha.Spec.Primary.ReplicationEndpoint, ha.Spec.Primary.PostgresProbe)
	log.V(1).Info("primary site observed",
		"site", obs.name,
		"reachable", obs.reachable,
		"primary", obs.primary,
		"ready", obs.ready,
		"phase", obs.phase,
		"lsn", obs.lsn,
		"reason", obs.reason,
	)
	return obs
}

// observeReplica probes a replica site's CNPG Cluster via its remote client
// (built from the kubeconfig Secret). A kubeconfig load failure yields an
// unreachable observation rather than an error, so one bad site cannot fail
// the whole reconcile — but the cause is logged at Error level so operators
// see it without having to kubectl-describe the HACluster status.
func (r *HAClusterReconciler) observeReplica(ctx context.Context, ha *hav1alpha1.HACluster, rep hav1alpha1.ReplicaSite) siteObservation {
	log := logf.FromContext(ctx)
	cli, err := r.RemoteClients.GetOrCreate(ctx, r.Client, ha.Namespace, rep.KubeconfigSecretRef)
	if err != nil {
		log.Error(err, "kubeconfig load failed, treating replica site as unreachable",
			"site", rep.Name,
			"namespace", ha.Namespace,
			"secret", rep.KubeconfigSecretRef.Name,
			"key", rep.KubeconfigSecretRef.Key,
		)
		return siteObservation{name: rep.Name, reason: fmt.Sprintf("kubeconfig load failed: %v", err)}
	}
	ref := types.NamespacedName{
		Namespace: rep.ClusterRef.Namespace,
		Name:      rep.ClusterRef.Name,
	}
	obs := siteObsFromHealth(rep.Name, health.Probe(ctx, cli, ref))
	r.probePostgres(ctx, &obs, cli, rep.ClusterRef, rep.ReplicationEndpoint, rep.PostgresProbe)
	log.V(1).Info("replica site observed",
		"site", obs.name,
		"reachable", obs.reachable,
		"primary", obs.primary,
		"ready", obs.ready,
		"phase", obs.phase,
		"lsn", obs.lsn,
		"reason", obs.reason,
	)
	return obs
}

func (r *HAClusterReconciler) probePostgres(
	ctx context.Context,
	obs *siteObservation,
	cli client.Client,
	ref hav1alpha1.ClusterRef,
	replicationEndpoint string,
	spec *hav1alpha1.PostgresProbe,
) {
	if spec == nil || !obs.reachable || !obs.ready {
		return
	}

	cfg, err := r.postgresProbeConfig(ctx, cli, ref.Namespace, replicationEndpoint, spec)
	if err != nil {
		obs.reason = appendReason(obs.reason, fmt.Sprintf("postgres probe skipped: %v", err))
		return
	}

	prober := r.PostgresProber
	if prober == nil {
		prober = postgres.SQLProber{}
	}
	probeCtx, cancel := context.WithTimeout(ctx, postgresProbeTimeout)
	defer cancel()

	got, err := prober.Probe(probeCtx, cfg)
	if err != nil {
		obs.reason = appendReason(obs.reason, fmt.Sprintf("postgres probe failed: %v", err))
		return
	}
	obs.lsnKnown = true
	obs.lsn = got.LSN
	obs.lsnValue = got.LSNValue
	obs.lagSeconds = got.LagSeconds
}

func (r *HAClusterReconciler) postgresProbeConfig(
	ctx context.Context,
	cli client.Client,
	namespace string,
	replicationEndpoint string,
	spec *hav1alpha1.PostgresProbe,
) (postgres.Config, error) {
	endpoint := spec.Endpoint
	if endpoint == "" {
		endpoint = replicationEndpoint
	}
	if endpoint == "" {
		return postgres.Config{}, fmt.Errorf("endpoint is empty")
	}

	user, err := secretValue(ctx, cli, namespace, spec.UserSecretRef)
	if err != nil {
		return postgres.Config{}, fmt.Errorf("read user secret: %w", err)
	}
	password, err := secretValue(ctx, cli, namespace, spec.PasswordSecretRef)
	if err != nil {
		return postgres.Config{}, fmt.Errorf("read password secret: %w", err)
	}
	return postgres.Config{
		Host:     endpoint,
		Port:     spec.Port,
		Database: spec.Database,
		User:     user,
		Password: password,
		SSLMode:  spec.SSLMode,
	}, nil
}

func secretValue(ctx context.Context, cli client.Client, namespace string, sel corev1.SecretKeySelector) (string, error) {
	if sel.Name == "" || sel.Key == "" {
		return "", fmt.Errorf("secret name and key are required")
	}
	var secret corev1.Secret
	if err := cli.Get(ctx, types.NamespacedName{Namespace: namespace, Name: sel.Name}, &secret); err != nil {
		return "", fmt.Errorf("get secret %s/%s: %w", namespace, sel.Name, err)
	}
	value, ok := secret.Data[sel.Key]
	if !ok {
		return "", fmt.Errorf("secret %s/%s has no key %q", namespace, sel.Name, sel.Key)
	}
	return string(value), nil
}

func appendReason(existing, extra string) string {
	if existing == "" {
		return extra
	}
	return strings.Join([]string{existing, extra}, "; ")
}

// decideCurrentPrimary chooses the single site currently acting as the HA
// primary, given the observations of the declared primary site and each
// replica site.
//
// Returns ("", false) when no unique primary can be picked:
//   - no site is CNPG-primary AND ready, or
//   - several sites are CNPG-primary AND ready (split-brain — see
//     detectSplitBrain for the list of offending sites).
//
// Considering every primary+ready site — not just the declared primary —
// is intentional: it surfaces split-brain instead of silently letting the
// declared primary win.
func decideCurrentPrimary(primary siteObservation, replicas []siteObservation) (string, bool) {
	candidates := primaryReadyCandidates(primary, replicas)
	if len(candidates) == 1 {
		return candidates[0], true
	}
	return "", false
}

// detectSplitBrain returns the list of site names that all claim to be
// CNPG-primary AND ready. A return value with len > 1 is a split-brain:
// more than one site accepts writes, leading to divergence as soon as a
// client commits anywhere.
//
// Returns nil when at most one site qualifies (the normal case).
func detectSplitBrain(primary siteObservation, replicas []siteObservation) []string {
	candidates := primaryReadyCandidates(primary, replicas)
	if len(candidates) <= 1 {
		return nil
	}
	return candidates
}

// primaryReadyCandidates lists every site (declared primary first, then
// replicas in spec order) whose CNPG Cluster is observed as primary
// (spec.replica.enabled absent or false) AND ready AND reachable.
func primaryReadyCandidates(primary siteObservation, replicas []siteObservation) []string {
	var names []string
	if primary.reachable && primary.primary && primary.ready {
		names = append(names, primary.name)
	}
	for _, r := range replicas {
		if r.reachable && r.primary && r.ready {
			names = append(names, r.name)
		}
	}
	return names
}

// toSiteStatus converts an internal siteObservation into a SiteStatus that
// the API can expose. now is passed in so a single timestamp value is shared
// by every site observed in the same Reconcile (observation consistency).
func toSiteStatus(obs siteObservation, now metav1.Time) hav1alpha1.SiteStatus {
	role := hav1alpha1.SiteRoleUnknown
	if obs.reachable {
		if obs.primary {
			role = hav1alpha1.SiteRolePrimary
		} else {
			role = hav1alpha1.SiteRoleReplica
		}
	}
	return hav1alpha1.SiteStatus{
		Name:                       obs.name,
		Role:                       role,
		Reachable:                  obs.reachable,
		Ready:                      obs.ready,
		Phase:                      obs.phase,
		Message:                    obs.reason,
		LastObservedTime:           &now,
		CurrentLSN:                 obs.lsn,
		ReplicationLagMilliseconds: lagMilliseconds(obs.lagSeconds),
	}
}

func lagMilliseconds(seconds *float64) *int64 {
	if seconds == nil {
		return nil
	}
	ms := int64(*seconds * 1000)
	return &ms
}

// buildSiteStatuses aggregates the primary and replica observations into a
// single ordered list: primary first, then replicas in spec order.
func buildSiteStatuses(primary siteObservation, replicas []siteObservation, now metav1.Time) []hav1alpha1.SiteStatus {
	out := make([]hav1alpha1.SiteStatus, 0, 1+len(replicas))
	out = append(out, toSiteStatus(primary, now))
	for _, r := range replicas {
		out = append(out, toSiteStatus(r, now))
	}
	return out
}

func (r *HAClusterReconciler) setConditions(ha *hav1alpha1.HACluster, available bool, splitBrain []string, primary siteObservation, replicas []siteObservation) {
	now := metav1.Now()

	avail := metav1.Condition{
		Type:               conditionAvailable,
		Status:             metav1.ConditionFalse,
		LastTransitionTime: now,
		Reason:             "PrimaryNotReady",
		Message:            "no healthy CNPG-primary observed",
		ObservedGeneration: ha.Generation,
	}
	if available {
		avail.Status = metav1.ConditionTrue
		avail.Reason = "PrimaryReady"
		avail.Message = fmt.Sprintf("primary site %q is CNPG-primary and ready", ha.Status.CurrentPrimarySite)
	}
	meta.SetStatusCondition(&ha.Status.Conditions, avail)

	split := metav1.Condition{
		Type:               conditionSplitBrain,
		Status:             metav1.ConditionFalse,
		LastTransitionTime: now,
		Reason:             "NoConflict",
		Message:            "at most one site observed as CNPG-primary and ready",
		ObservedGeneration: ha.Generation,
	}
	if len(splitBrain) > 1 {
		split.Status = metav1.ConditionTrue
		split.Reason = "MultiplePrimariesObserved"
		split.Message = fmt.Sprintf("multiple sites observed as CNPG-primary and ready: %v — writes may diverge", splitBrain)
	}
	meta.SetStatusCondition(&ha.Status.Conditions, split)

	degraded := metav1.Condition{
		Type:               conditionDegraded,
		Status:             metav1.ConditionFalse,
		LastTransitionTime: now,
		Reason:             "AllSitesHealthy",
		Message:            "all sites reachable and ready",
		ObservedGeneration: ha.Generation,
	}
	unreachable := []string{}
	unready := []string{}
	collect := func(s siteObservation) {
		switch {
		case !s.reachable:
			unreachable = append(unreachable, s.name)
		case !s.ready:
			unready = append(unready, s.name)
		}
	}
	collect(primary)
	for _, rep := range replicas {
		collect(rep)
	}
	switch {
	case len(unreachable) > 0:
		degraded.Status = metav1.ConditionTrue
		degraded.Reason = "SitesUnreachable"
		degraded.Message = fmt.Sprintf("unreachable sites: %v (unready: %v)", unreachable, unready)
	case len(unready) > 0:
		degraded.Status = metav1.ConditionTrue
		degraded.Reason = "SitesNotReady"
		degraded.Message = fmt.Sprintf("sites reachable but not ready: %v", unready)
	}
	meta.SetStatusCondition(&ha.Status.Conditions, degraded)
}

// SetupWithManager sets up the controller with the Manager.
func (r *HAClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.RemoteClients == nil {
		r.RemoteClients = remoteclient.NewCache(mgr.GetScheme())
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&hav1alpha1.HACluster{}).
		Named("hacluster").
		Complete(r)
}
