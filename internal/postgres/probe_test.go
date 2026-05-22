/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package postgres

import "testing"

func TestParseLSN(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    uint64
		wantErr bool
	}{
		{name: "zero", in: "0/0", want: 0},
		{name: "low word", in: "0/16B6C50", want: 0x16B6C50},
		{name: "high and low words", in: "1/0000000A", want: 0x10000000A},
		{name: "max words", in: "FFFFFFFF/FFFFFFFF", want: 0xFFFFFFFFFFFFFFFF},
		{name: "missing slash", in: "16B6C50", wantErr: true},
		{name: "bad high word", in: "x/16B6C50", wantErr: true},
		{name: "bad low word", in: "0/nope", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseLSN(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseLSN(%q) succeeded, want error", tt.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLSN(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseLSN(%q): got %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestNormalizeHostPort(t *testing.T) {
	tests := []struct {
		name string
		host string
		port int32
		want string
	}{
		{name: "host only", host: "pg-rw.site-a.svc", port: 5432, want: "pg-rw.site-a.svc:5432"},
		{name: "host with port", host: "pg-rw.site-a.svc:6432", port: 5432, want: "pg-rw.site-a.svc:6432"},
		{name: "ipv6 host with port", host: "[2001:db8::1]:5432", port: 6432, want: "[2001:db8::1]:5432"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeHostPort(tt.host, tt.port); got != tt.want {
				t.Errorf("normalizeHostPort(%q, %d): got %q, want %q", tt.host, tt.port, got, tt.want)
			}
		})
	}
}
