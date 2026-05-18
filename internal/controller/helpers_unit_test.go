/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"testing"
	"time"

	hav1alpha1 "github.com/davidesteban/cnpg-ha/api/v1alpha1"
)

func TestFindObservation(t *testing.T) {
	prim := siteObservation{name: "site-a", reachable: true}
	repB := siteObservation{name: siteB}
	repC := siteObservation{name: "site-c"}
	reps := []siteObservation{repB, repC}

	tests := []struct {
		name     string
		query    string
		wantName string // "" → expect nil
	}{
		{"matches primary", "site-a", "site-a"},
		{"matches a replica", "site-c", "site-c"},
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

func makeHAForLookup() *hav1alpha1.HACluster {
	return &hav1alpha1.HACluster{
		Spec: hav1alpha1.HAClusterSpec{
			Primary: hav1alpha1.PrimarySite{
				Name:                "site-a",
				ReplicationEndpoint: "ep-a",
			},
			Replicas: []hav1alpha1.ReplicaSite{
				{Name: siteB, ReplicationEndpoint: "ep-b"},
				{Name: "site-c"}, // no endpoint
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
		{"replica without endpoint", "site-c", ""},
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

func TestChooseTarget(t *testing.T) {
	ha := &hav1alpha1.HACluster{
		Spec: hav1alpha1.HAClusterSpec{
			Replicas: []hav1alpha1.ReplicaSite{
				{Name: siteB},
				{Name: "site-c"},
			},
		},
	}
	healthy := func(n string) siteObservation {
		return siteObservation{name: n, reachable: true, ready: true, primary: false}
	}

	t.Run("Ordered picks first healthy replica", func(t *testing.T) {
		obs := []siteObservation{healthy(siteB), healthy("site-c")}
		rep, idx, ok := chooseTarget(ha, obs, "site-a")
		if !ok || rep.Name != siteB || idx != 0 {
			t.Errorf("got (%q,%d,%v), want (site-b,0,true)", rep.Name, idx, ok)
		}
	})

	t.Run("skips the failed primary even if healthy", func(t *testing.T) {
		// site-b is the failed primary → must be skipped, site-c chosen.
		obs := []siteObservation{healthy(siteB), healthy("site-c")}
		rep, idx, ok := chooseTarget(ha, obs, siteB)
		if !ok || rep.Name != "site-c" || idx != 1 {
			t.Errorf("got (%q,%d,%v), want (site-c,1,true)", rep.Name, idx, ok)
		}
	})

	t.Run("skips unreachable / not-ready / already-primary", func(t *testing.T) {
		obs := []siteObservation{
			{name: siteB, reachable: false},                               // unreachable
			{name: "site-c", reachable: true, ready: true, primary: true}, // already primary
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
			Replicas: []hav1alpha1.ReplicaSite{{Name: siteB}, {Name: "site-c"}},
			Failover: hav1alpha1.FailoverSpec{PromotionPolicy: hav1alpha1.PromotionPolicyMostAdvancedLSN},
		},
	}
	withTL := func(n string, tl int64) siteObservation {
		return siteObservation{name: n, reachable: true, ready: true, primary: false, timelineID: tl}
	}

	t.Run("MostAdvancedLSN picks the highest timeline", func(t *testing.T) {
		obs := []siteObservation{withTL(siteB, 2), withTL("site-c", 5)}
		rep, idx, ok := chooseTarget(mostAdvanced, obs, "site-a")
		if !ok || rep.Name != "site-c" || idx != 1 {
			t.Errorf("got (%q,%d,%v), want (site-c,1,true)", rep.Name, idx, ok)
		}
	})

	t.Run("MostAdvancedLSN tie → earlier spec order wins", func(t *testing.T) {
		obs := []siteObservation{withTL(siteB, 4), withTL("site-c", 4)}
		rep, idx, ok := chooseTarget(mostAdvanced, obs, "site-a")
		if !ok || rep.Name != siteB || idx != 0 {
			t.Errorf("got (%q,%d,%v), want (site-b,0,true) on tie", rep.Name, idx, ok)
		}
	})

	t.Run("MostAdvancedLSN ignores ineligible higher-timeline sites", func(t *testing.T) {
		// site-c has a higher timeline but is not ready → site-b chosen.
		obs := []siteObservation{
			withTL(siteB, 1),
			{name: "site-c", reachable: true, ready: false, primary: false, timelineID: 9},
		}
		rep, _, ok := chooseTarget(mostAdvanced, obs, "site-a")
		if !ok || rep.Name != siteB {
			t.Errorf("got (%q,%v), want site-b (ineligible site-c skipped)", rep.Name, ok)
		}
	})
}
