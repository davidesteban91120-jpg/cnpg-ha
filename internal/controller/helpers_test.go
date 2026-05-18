/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	hav1alpha1 "github.com/davidesteban/cnpg-ha/api/v1alpha1"
)

// fixedNow renvoie un metav1.Time stable pour des tests reproductibles.
func fixedNow() metav1.Time {
	return metav1.NewTime(time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC))
}

// findCondition cherche une condition par type dans une slice de conditions.
// Retourne nil si non trouvée.
func findCondition(conditions []metav1.Condition, t string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == t {
			return &conditions[i]
		}
	}
	return nil
}

func TestToSiteStatus(t *testing.T) {
	now := fixedNow()
	tests := []struct {
		name string
		obs  siteObservation
		want hav1alpha1.SiteStatus
	}{
		{
			name: "unreachable → role Unknown, message preserve",
			obs:  siteObservation{name: "site-a", reachable: false, reason: "kubeconfig load failed: dial tcp"},
			want: hav1alpha1.SiteStatus{
				Name:             "site-a",
				Role:             hav1alpha1.SiteRoleUnknown,
				Reachable:        false,
				Ready:            false,
				Message:          "kubeconfig load failed: dial tcp",
				LastObservedTime: &now,
			},
		},
		{
			name: "reachable + primary + ready → role Primary",
			obs: siteObservation{
				name:      "site-a",
				reachable: true,
				primary:   true,
				ready:     true,
				phase:     "Cluster in healthy state",
			},
			want: hav1alpha1.SiteStatus{
				Name:             "site-a",
				Role:             hav1alpha1.SiteRolePrimary,
				Reachable:        true,
				Ready:            true,
				Phase:            "Cluster in healthy state",
				LastObservedTime: &now,
			},
		},
		{
			name: "reachable + non-primary + ready → role Replica",
			obs: siteObservation{
				name:      "site-b",
				reachable: true,
				primary:   false,
				ready:     true,
				phase:     "Cluster in healthy state",
			},
			want: hav1alpha1.SiteStatus{
				Name:             "site-b",
				Role:             hav1alpha1.SiteRoleReplica,
				Reachable:        true,
				Ready:            true,
				Phase:            "Cluster in healthy state",
				LastObservedTime: &now,
			},
		},
		{
			name: "reachable mais not-ready (primary fence) → role conserve, message non vide",
			obs: siteObservation{
				name:      "site-a",
				reachable: true,
				primary:   true,
				ready:     false,
				phase:     "Cluster in healthy state",
				reason:    "not ready (phase=\"Cluster in healthy state\", readyInstances=0)",
			},
			want: hav1alpha1.SiteStatus{
				Name:             "site-a",
				Role:             hav1alpha1.SiteRolePrimary,
				Reachable:        true,
				Ready:            false,
				Phase:            "Cluster in healthy state",
				Message:          "not ready (phase=\"Cluster in healthy state\", readyInstances=0)",
				LastObservedTime: &now,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toSiteStatus(tt.obs, now)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("toSiteStatus mismatch:\n  got  = %+v\n  want = %+v", got, tt.want)
			}
		})
	}
}

func TestBuildSiteStatuses(t *testing.T) {
	now := fixedNow()
	primary := siteObservation{name: "site-a", reachable: true, primary: true, ready: true}
	replicas := []siteObservation{
		{name: "site-b", reachable: true, primary: false, ready: true},
		{name: "site-c", reachable: false, reason: "kubeconfig load failed"},
	}

	got := buildSiteStatuses(primary, replicas, now)

	if len(got) != 3 {
		t.Fatalf("expected 3 SiteStatus, got %d", len(got))
	}
	wantOrder := []string{"site-a", "site-b", "site-c"}
	for i, name := range wantOrder {
		if got[i].Name != name {
			t.Errorf("position %d: got name=%q, want %q", i, got[i].Name, name)
		}
	}
	if got[0].Role != hav1alpha1.SiteRolePrimary {
		t.Errorf("site-a: role=%q, want Primary", got[0].Role)
	}
	if got[1].Role != hav1alpha1.SiteRoleReplica {
		t.Errorf("site-b: role=%q, want Replica", got[1].Role)
	}
	if got[2].Role != hav1alpha1.SiteRoleUnknown {
		t.Errorf("site-c: role=%q, want Unknown", got[2].Role)
	}
}

func TestDecideCurrentPrimary(t *testing.T) {
	healthyPrimary := siteObservation{name: "site-a", reachable: true, primary: true, ready: true}
	downPrimary := siteObservation{name: "site-a", reachable: true, primary: true, ready: false}
	unreachablePrimary := siteObservation{name: "site-a", reachable: false}
	healthyReplicaB := siteObservation{name: "site-b", reachable: true, primary: false, ready: true}
	promotedReplicaB := siteObservation{name: "site-b", reachable: true, primary: true, ready: true}
	promotedReplicaC := siteObservation{name: "site-c", reachable: true, primary: true, ready: true}

	tests := []struct {
		name      string
		primary   siteObservation
		replicas  []siteObservation
		wantName  string
		wantAvail bool
	}{
		{
			name:      "primary sain → primary gagne",
			primary:   healthyPrimary,
			replicas:  []siteObservation{healthyReplicaB},
			wantName:  "site-a",
			wantAvail: true,
		},
		{
			name:      "primary down, exactement un replica promu → replica gagne",
			primary:   downPrimary,
			replicas:  []siteObservation{promotedReplicaB},
			wantName:  "site-b",
			wantAvail: true,
		},
		{
			name:      "primary down, deux replicas promus → split-brain, indisponible",
			primary:   unreachablePrimary,
			replicas:  []siteObservation{promotedReplicaB, promotedReplicaC},
			wantName:  "",
			wantAvail: false,
		},
		{
			name:      "primary down, aucun replica promu → indisponible",
			primary:   downPrimary,
			replicas:  []siteObservation{healthyReplicaB},
			wantName:  "",
			wantAvail: false,
		},
		{
			name:      "primary sain + replica primary aussi → split-brain (plus de \"primary win\" silencieux)",
			primary:   healthyPrimary,
			replicas:  []siteObservation{promotedReplicaB},
			wantName:  "",
			wantAvail: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotAvail := decideCurrentPrimary(tt.primary, tt.replicas)
			if gotName != tt.wantName || gotAvail != tt.wantAvail {
				t.Errorf("decideCurrentPrimary: got (%q, %v), want (%q, %v)",
					gotName, gotAvail, tt.wantName, tt.wantAvail)
			}
		})
	}
}

func TestDetectSplitBrain(t *testing.T) {
	healthyPrimary := siteObservation{name: "site-a", reachable: true, primary: true, ready: true}
	downPrimary := siteObservation{name: "site-a", reachable: true, primary: true, ready: false}
	unreachablePrimary := siteObservation{name: "site-a", reachable: false}
	replicaB := siteObservation{name: "site-b", reachable: true, primary: false, ready: true}
	promotedReplicaB := siteObservation{name: "site-b", reachable: true, primary: true, ready: true}
	promotedReplicaC := siteObservation{name: "site-c", reachable: true, primary: true, ready: true}

	tests := []struct {
		name     string
		primary  siteObservation
		replicas []siteObservation
		want     []string
	}{
		{
			name:     "primary sain + replica replica → pas de split-brain",
			primary:  healthyPrimary,
			replicas: []siteObservation{replicaB},
			want:     nil,
		},
		{
			name:     "primary sain + replica promu → split-brain [site-a, site-b]",
			primary:  healthyPrimary,
			replicas: []siteObservation{promotedReplicaB},
			want:     []string{"site-a", "site-b"},
		},
		{
			name:     "primary down + deux replicas promus → split-brain [site-b, site-c]",
			primary:  downPrimary,
			replicas: []siteObservation{promotedReplicaB, promotedReplicaC},
			want:     []string{"site-b", "site-c"},
		},
		{
			name:     "primary unreachable + un seul replica primary+ready → pas de split-brain",
			primary:  unreachablePrimary,
			replicas: []siteObservation{promotedReplicaB, replicaB},
			want:     nil,
		},
		{
			name:     "tout en panne → nil (rien à comparer)",
			primary:  downPrimary,
			replicas: []siteObservation{{name: "site-b", reachable: true, primary: false, ready: false}},
			want:     nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectSplitBrain(tt.primary, tt.replicas)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("detectSplitBrain: got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSetConditions(t *testing.T) {
	tests := []struct {
		name             string
		available        bool
		splitBrain       []string
		primary          siteObservation
		replicas         []siteObservation
		currentPrimary   string
		wantAvailable    metav1.ConditionStatus
		wantAvailReas    string
		wantDegraded     metav1.ConditionStatus
		wantDegrReas     string
		wantSplitBrain   metav1.ConditionStatus
		wantSplitReason  string
		wantSplitContain string // substring expected in the message; empty = no check
	}{
		{
			name:            "tout sain → Available=True, Degraded=False, SplitBrain=False",
			available:       true,
			primary:         siteObservation{name: "site-a", reachable: true, primary: true, ready: true},
			replicas:        []siteObservation{{name: "site-b", reachable: true, primary: false, ready: true}},
			currentPrimary:  "site-a",
			wantAvailable:   metav1.ConditionTrue,
			wantAvailReas:   "PrimaryReady",
			wantDegraded:    metav1.ConditionFalse,
			wantDegrReas:    "AllSitesHealthy",
			wantSplitBrain:  metav1.ConditionFalse,
			wantSplitReason: "NoConflict",
		},
		{
			name:            "primary down + replica unreachable → Available=False, Degraded=SitesUnreachable",
			available:       false,
			primary:         siteObservation{name: "site-a", reachable: true, primary: true, ready: false},
			replicas:        []siteObservation{{name: "site-b", reachable: false}},
			wantAvailable:   metav1.ConditionFalse,
			wantAvailReas:   "PrimaryNotReady",
			wantDegraded:    metav1.ConditionTrue,
			wantDegrReas:    "SitesUnreachable",
			wantSplitBrain:  metav1.ConditionFalse,
			wantSplitReason: "NoConflict",
		},
		{
			name:            "primary OK + replica not-ready → Available=True, Degraded=SitesNotReady",
			available:       true,
			primary:         siteObservation{name: "site-a", reachable: true, primary: true, ready: true},
			replicas:        []siteObservation{{name: "site-b", reachable: true, primary: false, ready: false}},
			currentPrimary:  "site-a",
			wantAvailable:   metav1.ConditionTrue,
			wantAvailReas:   "PrimaryReady",
			wantDegraded:    metav1.ConditionTrue,
			wantDegrReas:    "SitesNotReady",
			wantSplitBrain:  metav1.ConditionFalse,
			wantSplitReason: "NoConflict",
		},
		{
			name:            "primary unreachable seul → Degraded=SitesUnreachable, pas SitesNotReady",
			available:       false,
			primary:         siteObservation{name: "site-a", reachable: false},
			replicas:        []siteObservation{{name: "site-b", reachable: true, primary: false, ready: true}},
			wantAvailable:   metav1.ConditionFalse,
			wantAvailReas:   "PrimaryNotReady",
			wantDegraded:    metav1.ConditionTrue,
			wantDegrReas:    "SitesUnreachable",
			wantSplitBrain:  metav1.ConditionFalse,
			wantSplitReason: "NoConflict",
		},
		{
			name:             "split-brain explicite → SplitBrain=True avec liste des sites",
			available:        false,
			splitBrain:       []string{"site-a", "site-c"},
			primary:          siteObservation{name: "site-a", reachable: true, primary: true, ready: true},
			replicas:         []siteObservation{{name: "site-c", reachable: true, primary: true, ready: true}},
			wantAvailable:    metav1.ConditionFalse,
			wantAvailReas:    "PrimaryNotReady",
			wantDegraded:     metav1.ConditionFalse,
			wantDegrReas:     "AllSitesHealthy",
			wantSplitBrain:   metav1.ConditionTrue,
			wantSplitReason:  "MultiplePrimariesObserved",
			wantSplitContain: "site-a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ha := &hav1alpha1.HACluster{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Status:     hav1alpha1.HAClusterStatus{CurrentPrimarySite: tt.currentPrimary},
			}
			r := &HAClusterReconciler{}
			r.setConditions(ha, tt.available, tt.splitBrain, tt.primary, tt.replicas)

			avail := findCondition(ha.Status.Conditions, conditionAvailable)
			if avail == nil {
				t.Fatalf("Available condition manquante")
			}
			if avail.Status != tt.wantAvailable || avail.Reason != tt.wantAvailReas {
				t.Errorf("Available: got (%v, %q), want (%v, %q)",
					avail.Status, avail.Reason, tt.wantAvailable, tt.wantAvailReas)
			}

			degr := findCondition(ha.Status.Conditions, conditionDegraded)
			if degr == nil {
				t.Fatalf("Degraded condition manquante")
			}
			if degr.Status != tt.wantDegraded || degr.Reason != tt.wantDegrReas {
				t.Errorf("Degraded: got (%v, %q), want (%v, %q)",
					degr.Status, degr.Reason, tt.wantDegraded, tt.wantDegrReas)
			}

			split := findCondition(ha.Status.Conditions, conditionSplitBrain)
			if split == nil {
				t.Fatalf("SplitBrain condition manquante")
			}
			if split.Status != tt.wantSplitBrain || split.Reason != tt.wantSplitReason {
				t.Errorf("SplitBrain: got (%v, %q), want (%v, %q)",
					split.Status, split.Reason, tt.wantSplitBrain, tt.wantSplitReason)
			}
			if tt.wantSplitContain != "" && !strings.Contains(split.Message, tt.wantSplitContain) {
				t.Errorf("SplitBrain message should contain %q, got %q", tt.wantSplitContain, split.Message)
			}
		})
	}
}
