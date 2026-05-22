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
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	hav1alpha1 "github.com/davidesteban/cnpg-ha/api/v1alpha1"
	"github.com/davidesteban/cnpg-ha/internal/postgres"
)

type fakePostgresProber struct {
	cfg    postgres.Config
	result postgres.Result
	err    error
}

func (f *fakePostgresProber) Probe(_ context.Context, cfg postgres.Config) (postgres.Result, error) {
	f.cfg = cfg
	return f.result, f.err
}

func TestFindObservation(t *testing.T) {
	prim := siteObservation{name: "site-a", reachable: true}
	repB := siteObservation{name: siteB}
	repC := siteObservation{name: siteC}
	reps := []siteObservation{repB, repC}

	tests := []struct {
		name     string
		query    string
		wantName string // "" → expect nil
	}{
		{"matches primary", "site-a", "site-a"},
		{"matches a replica", siteC, siteC},
		{"unknown site → nil", "site-z", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findObservation(tt.query, prim, reps)
			switch {
			case tt.wantName == "" && got != nil:
				t.Errorf("expected nil, got %+v", *got)
			case tt.wantName != "" && got == nil:
				t.Errorf("expected %q, got nil", tt.wantName)
			case tt.wantName != "" && got.name != tt.wantName:
				t.Errorf("name: got %q, want %q", got.name, tt.wantName)
			}
		})
	}
}

func TestEffectiveFailureThreshold(t *testing.T) {
	tests := []struct {
		name string
		in   int32
		want int
	}{
		{"zero → default 3", 0, defaultFailureThreshold},
		{"one (< min) → default 3", 1, defaultFailureThreshold},
		{"valid 2 kept", 2, 2},
		{"valid 7 kept", 7, 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := effectiveFailureThreshold(hav1alpha1.FailoverSpec{FailureThreshold: tt.in})
			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestEffectiveHealthInterval(t *testing.T) {
	tests := []struct {
		name string
		in   int32
		want time.Duration
	}{
		{"zero → default 10s", 0, defaultHealthCheckInterval},
		{"negative → default 10s", -5, defaultHealthCheckInterval},
		{"valid 1s kept", 1, 1 * time.Second},
		{"valid 45s kept", 45, 45 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := effectiveHealthInterval(hav1alpha1.FailoverSpec{HealthCheckIntervalSeconds: tt.in})
			if got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestStabilizationCooldown(t *testing.T) {
	tests := []struct {
		name string
		in   int32
		want time.Duration
	}{
		{"default interval uses minimum", 0, minStabilizationCooldown},
		{"short interval uses minimum", 5, minStabilizationCooldown},
		{"long interval uses three intervals", 20, 60 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stabilizationCooldown(hav1alpha1.FailoverSpec{HealthCheckIntervalSeconds: tt.in})
			if got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func makeHAForLookup() *hav1alpha1.HACluster {
	return &hav1alpha1.HACluster{
		Spec: hav1alpha1.HAClusterSpec{
			Primary: hav1alpha1.PrimarySite{
				Name:                "site-a",
				ReplicationEndpoint: "ep-a",
			},
			Replicas: []hav1alpha1.ReplicaSite{
				{Name: siteB, ReplicationEndpoint: "ep-b"},
				{Name: siteC}, // no endpoint
			},
		},
	}
}

func TestReplicationEndpointFor(t *testing.T) {
	ha := makeHAForLookup()
	tests := []struct {
		name string
		site string
		want string
	}{
		{"primary site endpoint", "site-a", "ep-a"},
		{"replica with endpoint", siteB, "ep-b"},
		{"replica without endpoint", siteC, ""},
		{"unknown site", "site-z", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := replicationEndpointFor(ha, tt.site); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCurrentPrimaryForPromotion(t *testing.T) {
	base := &hav1alpha1.HACluster{
		Spec: hav1alpha1.HAClusterSpec{
			Primary: hav1alpha1.PrimarySite{Name: "site-a"},
		},
	}
	healthyPrimary := siteObservation{name: "site-a", reachable: true, primary: true, ready: true}
	healthyReplica := siteObservation{name: siteB, reachable: true, primary: false, ready: true}

	t.Run("status current primary wins", func(t *testing.T) {
		ha := base.DeepCopy()
		ha.Status.CurrentPrimarySite = siteB
		got, err := currentPrimaryForPromotion(ha, healthyPrimary, []siteObservation{healthyReplica})
		if err != nil || got != siteB {
			t.Fatalf("got (%q,%v), want (%q,nil)", got, err, siteB)
		}
	})

	t.Run("observed primary wins before status is initialized", func(t *testing.T) {
		got, err := currentPrimaryForPromotion(base, healthyPrimary, []siteObservation{healthyReplica})
		if err != nil || got != "site-a" {
			t.Fatalf("got (%q,%v), want (site-a,nil)", got, err)
		}
	})

	t.Run("fallback to spec primary when no primary is observed", func(t *testing.T) {
		got, err := currentPrimaryForPromotion(base,
			siteObservation{name: "site-a", reachable: true, primary: true, ready: false},
			[]siteObservation{healthyReplica})
		if err != nil || got != "site-a" {
			t.Fatalf("got (%q,%v), want (site-a,nil)", got, err)
		}
	})

	t.Run("split-brain is rejected", func(t *testing.T) {
		_, err := currentPrimaryForPromotion(base, healthyPrimary,
			[]siteObservation{{name: siteB, reachable: true, primary: true, ready: true}})
		if err == nil {
			t.Fatalf("expected split-brain error")
		}
	})
}

func TestProbePostgresAddsLSNObservation(t *testing.T) {
	ctx := context.Background()
	scheme := buildPromoteScheme(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-probe", Namespace: "db"},
		Data: map[string][]byte{
			"user":     []byte("probe"),
			"password": []byte("secret"),
		},
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	lag := 0.75
	prober := &fakePostgresProber{result: postgres.Result{
		LSN:        "0/16B6C50",
		LSNValue:   0x16B6C50,
		LagSeconds: &lag,
	}}
	r := &HAClusterReconciler{PostgresProber: prober}
	obs := siteObservation{name: siteB, reachable: true, ready: true}

	r.probePostgres(ctx, &obs, cli, hav1alpha1.ClusterRef{Namespace: "db", Name: "pg-prod"},
		"pg-prod-rw.db.svc.cluster.local", &hav1alpha1.PostgresProbe{
			Database: "app",
			SSLMode:  "require",
			UserSecretRef: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "pg-probe"},
				Key:                  "user",
			},
			PasswordSecretRef: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "pg-probe"},
				Key:                  "password",
			},
		})

	if !obs.lsnKnown || obs.lsn != "0/16B6C50" || obs.lsnValue != 0x16B6C50 {
		t.Fatalf("postgres probe did not populate LSN: %+v", obs)
	}
	if obs.lagSeconds == nil || *obs.lagSeconds != lag {
		t.Fatalf("lagSeconds: got %v, want %v", obs.lagSeconds, lag)
	}
	if prober.cfg.Host != "pg-prod-rw.db.svc.cluster.local" ||
		prober.cfg.Database != "app" ||
		prober.cfg.User != "probe" ||
		prober.cfg.Password != "secret" {
		t.Errorf("unexpected postgres config: %+v", prober.cfg)
	}
}

func TestProbePostgresBranches(t *testing.T) {
	ctx := context.Background()
	scheme := buildPromoteScheme(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-probe", Namespace: "db"},
		Data:       map[string][]byte{"user": []byte("probe"), "password": []byte("secret")},
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	spec := &hav1alpha1.PostgresProbe{
		UserSecretRef: corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "pg-probe"},
			Key:                  "user",
		},
		PasswordSecretRef: corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "pg-probe"},
			Key:                  "password",
		},
	}

	t.Run("nil spec leaves observation untouched", func(t *testing.T) {
		r := &HAClusterReconciler{PostgresProber: &fakePostgresProber{}}
		obs := siteObservation{name: siteB, reachable: true, ready: true}
		r.probePostgres(ctx, &obs, cli, hav1alpha1.ClusterRef{Namespace: "db"}, "pg", nil)
		if obs.lsnKnown {
			t.Fatalf("nil spec should not probe: %+v", obs)
		}
	})

	t.Run("missing endpoint appends skip reason", func(t *testing.T) {
		r := &HAClusterReconciler{PostgresProber: &fakePostgresProber{}}
		obs := siteObservation{name: siteB, reachable: true, ready: true, reason: "existing"}
		r.probePostgres(ctx, &obs, cli, hav1alpha1.ClusterRef{Namespace: "db"}, "", spec)
		if !strings.Contains(obs.reason, "existing; postgres probe skipped: endpoint is empty") {
			t.Fatalf("unexpected reason: %q", obs.reason)
		}
	})

	t.Run("probe error appends failure reason", func(t *testing.T) {
		r := &HAClusterReconciler{PostgresProber: &fakePostgresProber{err: errors.New("boom")}}
		obs := siteObservation{name: siteB, reachable: true, ready: true}
		r.probePostgres(ctx, &obs, cli, hav1alpha1.ClusterRef{Namespace: "db"}, "pg", spec)
		if obs.lsnKnown || !strings.Contains(obs.reason, "postgres probe failed: boom") {
			t.Fatalf("unexpected observation: %+v", obs)
		}
	})
}

func TestSecretValue(t *testing.T) {
	ctx := context.Background()
	scheme := buildPromoteScheme(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-probe", Namespace: "db"},
		Data:       map[string][]byte{"user": []byte("probe")},
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	got, err := secretValue(ctx, cli, "db", corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "pg-probe"},
		Key:                  "user",
	})
	if err != nil || got != "probe" {
		t.Fatalf("secretValue: got (%q,%v), want (probe,nil)", got, err)
	}

	if _, err := secretValue(ctx, cli, "db", corev1.SecretKeySelector{}); err == nil {
		t.Fatalf("empty selector should fail")
	}
	if _, err := secretValue(ctx, cli, "db", corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "pg-probe"},
		Key:                  "missing",
	}); err == nil {
		t.Fatalf("missing key should fail")
	}
}

func TestChooseTarget(t *testing.T) {
	ha := &hav1alpha1.HACluster{
		Spec: hav1alpha1.HAClusterSpec{
			Replicas: []hav1alpha1.ReplicaSite{
				{Name: siteB},
				{Name: siteC},
			},
		},
	}
	healthy := func(n string) siteObservation {
		return siteObservation{name: n, reachable: true, ready: true, primary: false}
	}

	t.Run("Ordered picks first healthy replica", func(t *testing.T) {
		obs := []siteObservation{healthy(siteB), healthy(siteC)}
		rep, idx, ok := chooseTarget(ha, obs, "site-a")
		if !ok || rep.Name != siteB || idx != 0 {
			t.Errorf("got (%q,%d,%v), want (site-b,0,true)", rep.Name, idx, ok)
		}
	})

	t.Run("skips the failed primary even if healthy", func(t *testing.T) {
		// site-b is the failed primary → must be skipped, site-c chosen.
		obs := []siteObservation{healthy(siteB), healthy(siteC)}
		rep, idx, ok := chooseTarget(ha, obs, siteB)
		if !ok || rep.Name != siteC || idx != 1 {
			t.Errorf("got (%q,%d,%v), want (site-c,1,true)", rep.Name, idx, ok)
		}
	})

	t.Run("skips unreachable / not-ready / already-primary", func(t *testing.T) {
		obs := []siteObservation{
			{name: siteB, reachable: false},                            // unreachable
			{name: siteC, reachable: true, ready: true, primary: true}, // already primary
		}
		if _, _, ok := chooseTarget(ha, obs, "site-a"); ok {
			t.Errorf("expected no candidate")
		}
	})

	t.Run("no replicas at all → no candidate", func(t *testing.T) {
		empty := &hav1alpha1.HACluster{}
		if _, _, ok := chooseTarget(empty, nil, "site-a"); ok {
			t.Errorf("expected ok=false with no replicas")
		}
	})

	mostAdvanced := &hav1alpha1.HACluster{
		Spec: hav1alpha1.HAClusterSpec{
			Replicas: []hav1alpha1.ReplicaSite{{Name: siteB}, {Name: siteC}},
			Failover: hav1alpha1.FailoverSpec{PromotionPolicy: hav1alpha1.PromotionPolicyMostAdvancedLSN},
		},
	}
	withTL := func(n string, tl int64) siteObservation {
		return siteObservation{name: n, reachable: true, ready: true, primary: false, timelineID: tl}
	}
	withLSN := func(n string, lsn string, lsnValue uint64, tl int64) siteObservation {
		return siteObservation{
			name: n, reachable: true, ready: true, primary: false,
			timelineID: tl, lsnKnown: true, lsn: lsn, lsnValue: lsnValue,
		}
	}

	t.Run("MostAdvancedLSN picks the highest PostgreSQL LSN", func(t *testing.T) {
		obs := []siteObservation{
			withLSN(siteB, "0/20", 0x20, 9),
			withLSN(siteC, "0/30", 0x30, 1),
		}
		rep, idx, ok := chooseTarget(mostAdvanced, obs, "site-a")
		if !ok || rep.Name != siteC || idx != 1 {
			t.Errorf("got (%q,%d,%v), want (site-c,1,true)", rep.Name, idx, ok)
		}
	})

	t.Run("MostAdvancedLSN prefers known LSN over timeline fallback", func(t *testing.T) {
		obs := []siteObservation{
			withTL(siteB, 99),
			withLSN(siteC, "0/10", 0x10, 1),
		}
		rep, idx, ok := chooseTarget(mostAdvanced, obs, "site-a")
		if !ok || rep.Name != siteC || idx != 1 {
			t.Errorf("got (%q,%d,%v), want (site-c,1,true)", rep.Name, idx, ok)
		}
	})

	t.Run("MostAdvancedLSN falls back to highest timeline when LSN is unknown", func(t *testing.T) {
		obs := []siteObservation{withTL(siteB, 2), withTL(siteC, 5)}
		rep, idx, ok := chooseTarget(mostAdvanced, obs, "site-a")
		if !ok || rep.Name != siteC || idx != 1 {
			t.Errorf("got (%q,%d,%v), want (site-c,1,true)", rep.Name, idx, ok)
		}
	})

	t.Run("MostAdvancedLSN tie → earlier spec order wins", func(t *testing.T) {
		obs := []siteObservation{withTL(siteB, 4), withTL(siteC, 4)}
		rep, idx, ok := chooseTarget(mostAdvanced, obs, "site-a")
		if !ok || rep.Name != siteB || idx != 0 {
			t.Errorf("got (%q,%d,%v), want (site-b,0,true) on tie", rep.Name, idx, ok)
		}
	})

	t.Run("MostAdvancedLSN ignores ineligible higher-timeline sites", func(t *testing.T) {
		// site-c has a higher timeline but is not ready → site-b chosen.
		obs := []siteObservation{
			withTL(siteB, 1),
			{name: siteC, reachable: true, ready: false, primary: false, timelineID: 9},
		}
		rep, _, ok := chooseTarget(mostAdvanced, obs, "site-a")
		if !ok || rep.Name != siteB {
			t.Errorf("got (%q,%v), want site-b (ineligible site-c skipped)", rep.Name, ok)
		}
	})
}
