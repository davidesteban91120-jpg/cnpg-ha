/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestSetSite(t *testing.T) {
	CurrentPrimarySite.Reset()
	SiteReachable.Reset()
	SiteReady.Reset()

	SetSite("db", "prod-db", "site-a", true, true, true)
	SetSite("db", "prod-db", "site-b", false, true, false)

	if v := testutil.ToFloat64(CurrentPrimarySite.WithLabelValues("prod-db", "db", "site-a")); v != 1 {
		t.Errorf("current_primary_site site-a: got %v, want 1", v)
	}
	if v := testutil.ToFloat64(CurrentPrimarySite.WithLabelValues("prod-db", "db", "site-b")); v != 0 {
		t.Errorf("current_primary_site site-b: got %v, want 0", v)
	}
	if v := testutil.ToFloat64(SiteReady.WithLabelValues("prod-db", "db", "site-b")); v != 0 {
		t.Errorf("site_ready site-b: got %v, want 0", v)
	}
	if v := testutil.ToFloat64(SiteReachable.WithLabelValues("prod-db", "db", "site-b")); v != 1 {
		t.Errorf("site_reachable site-b: got %v, want 1", v)
	}
}

func TestSetSplitBrain(t *testing.T) {
	SplitBrain.Reset()
	SetSplitBrain("db", "prod-db", true)
	if v := testutil.ToFloat64(SplitBrain.WithLabelValues("prod-db", "db")); v != 1 {
		t.Errorf("split_brain: got %v, want 1", v)
	}
	SetSplitBrain("db", "prod-db", false)
	if v := testutil.ToFloat64(SplitBrain.WithLabelValues("prod-db", "db")); v != 0 {
		t.Errorf("split_brain after clear: got %v, want 0", v)
	}
}

func TestIncFailover(t *testing.T) {
	FailoverTotal.Reset()
	IncFailover("db", "prod-db", "automatic")
	IncFailover("db", "prod-db", "automatic")
	IncFailover("db", "prod-db", "manual")

	const want = `
# HELP cnpg_ha_failover_total Number of completed failovers, by trigger mode.
# TYPE cnpg_ha_failover_total counter
cnpg_ha_failover_total{hacluster="prod-db",mode="automatic",namespace="db"} 2
cnpg_ha_failover_total{hacluster="prod-db",mode="manual",namespace="db"} 1
`
	if err := testutil.CollectAndCompare(FailoverTotal, strings.NewReader(want)); err != nil {
		t.Errorf("failover_total mismatch:\n%v", err)
	}
}
