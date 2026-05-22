/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package postgres probes PostgreSQL itself for WAL progress. It is kept
// separate from internal/health so Kubernetes-object health and SQL probes
// can evolve independently.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

const (
	// DefaultPort is the PostgreSQL default TCP port.
	DefaultPort int32 = 5432

	defaultDatabase = "postgres"
	defaultSSLMode  = "require"
)

const probeQuery = `
WITH state AS (
	SELECT pg_is_in_recovery() AS in_recovery
)
SELECT
	in_recovery,
	CASE
		WHEN in_recovery THEN COALESCE(pg_last_wal_replay_lsn(), '0/0'::pg_lsn)
		ELSE pg_current_wal_lsn()
	END::text AS current_lsn,
	CASE
		WHEN in_recovery AND pg_last_xact_replay_timestamp() IS NOT NULL
			THEN EXTRACT(EPOCH FROM clock_timestamp() - pg_last_xact_replay_timestamp())::double precision
		WHEN in_recovery THEN NULL
		ELSE 0
	END AS replay_lag_seconds
FROM state`

// Config describes a single PostgreSQL probe target.
type Config struct {
	Host     string
	Port     int32
	Database string
	User     string
	Password string
	SSLMode  string
}

// Result is the WAL state returned by a PostgreSQL probe.
type Result struct {
	InRecovery bool
	LSN        string
	LSNValue   uint64
	LagSeconds *float64
}

// Prober is implemented by PostgreSQL WAL-location probes.
type Prober interface {
	Probe(ctx context.Context, cfg Config) (Result, error)
}

// SQLProber connects to PostgreSQL and reads WAL progress through SQL.
type SQLProber struct{}

// Probe connects to PostgreSQL and returns the current WAL location. On a
// standby this is pg_last_wal_replay_lsn(); on a primary this is
// pg_current_wal_lsn().
func (SQLProber) Probe(ctx context.Context, cfg Config) (Result, error) {
	conn, err := pgx.Connect(ctx, connString(cfg))
	if err != nil {
		return Result{}, fmt.Errorf("connect postgres: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	var got Result
	var lag sql.NullFloat64
	if err := conn.QueryRow(ctx, probeQuery).Scan(&got.InRecovery, &got.LSN, &lag); err != nil {
		return Result{}, fmt.Errorf("query wal progress: %w", err)
	}
	if lag.Valid {
		got.LagSeconds = &lag.Float64
	}
	lsnValue, err := ParseLSN(got.LSN)
	if err != nil {
		return Result{}, fmt.Errorf("parse lsn %q: %w", got.LSN, err)
	}
	got.LSNValue = lsnValue
	return got, nil
}

// ParseLSN converts a PostgreSQL LSN string (for example "0/16B6C50") into
// its monotonic uint64 representation.
func ParseLSN(s string) (uint64, error) {
	hi, lo, ok := strings.Cut(s, "/")
	if !ok || hi == "" || lo == "" {
		return 0, fmt.Errorf("expected <hex>/<hex>")
	}
	hiN, err := strconv.ParseUint(hi, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("parse high word: %w", err)
	}
	loN, err := strconv.ParseUint(lo, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("parse low word: %w", err)
	}
	return (hiN << 32) + loN, nil
}

func connString(cfg Config) string {
	port := cfg.Port
	if port == 0 {
		port = DefaultPort
	}
	db := cfg.Database
	if db == "" {
		db = defaultDatabase
	}
	sslMode := cfg.SSLMode
	if sslMode == "" {
		sslMode = defaultSSLMode
	}

	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(cfg.User, cfg.Password),
		Host:   normalizeHostPort(cfg.Host, port),
		Path:   db,
	}
	q := u.Query()
	q.Set("sslmode", sslMode)
	u.RawQuery = q.Encode()
	return u.String()
}

func normalizeHostPort(host string, port int32) string {
	if h, p, err := net.SplitHostPort(host); err == nil {
		return net.JoinHostPort(h, p)
	}
	if h, p, ok := strings.Cut(host, ":"); ok && h != "" && p != "" && !strings.Contains(p, ":") {
		return net.JoinHostPort(h, p)
	}
	return net.JoinHostPort(host, strconv.Itoa(int(port)))
}
