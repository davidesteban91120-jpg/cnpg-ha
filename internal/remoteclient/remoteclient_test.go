/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package remoteclient

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const miniKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: k
  cluster:
    server: https://10.0.0.1:6443
    insecure-skip-tls-verify: true
contexts:
- name: k
  context: {cluster: k, user: k}
current-context: k
users:
- name: k
  user: {token: t}
`

func secretRefFor() corev1.SecretKeySelector {
	return corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "kc"},
		Key:                  "kubeconfig",
	}
}

func hubWithSecret(t *testing.T, data string) (client.Client, *corev1.Secret) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "kc", Namespace: "ops"},
		Data:       map[string][]byte{"kubeconfig": []byte(data)},
	}
	hub := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sec).Build()
	return hub, sec
}

func TestGetOrCreate_RebuildOnRotation(t *testing.T) {
	ctx := context.Background()
	hub, sec := hubWithSecret(t, miniKubeconfig)
	c := NewCache(runtime.NewScheme())
	ref := secretRefFor()

	cli1, err := c.GetOrCreate(ctx, hub, "ops", ref)
	if err != nil {
		t.Fatalf("first GetOrCreate: %v", err)
	}

	cli1b, err := c.GetOrCreate(ctx, hub, "ops", ref)
	if err != nil {
		t.Fatalf("second GetOrCreate: %v", err)
	}
	if cli1b != cli1 {
		t.Errorf("unchanged Secret must be a cache hit (same client)")
	}

	// Rotate the kubeconfig — the fake client bumps resourceVersion.
	fresh := &corev1.Secret{}
	if err := hub.Get(ctx, client.ObjectKeyFromObject(sec), fresh); err != nil {
		t.Fatalf("get secret: %v", err)
	}
	fresh.Data["kubeconfig"] = []byte(miniKubeconfig + "\n# rotated\n")
	if err := hub.Update(ctx, fresh); err != nil {
		t.Fatalf("rotate secret: %v", err)
	}

	cli2, err := c.GetOrCreate(ctx, hub, "ops", ref)
	if err != nil {
		t.Fatalf("post-rotation GetOrCreate: %v", err)
	}
	if cli2 == cli1 {
		t.Errorf("rotated kubeconfig must rebuild the client (got the cached one)")
	}
}

func TestGetOrCreate_GracefulWhenSecretGoneButCached(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	hubNoSecret := fake.NewClientBuilder().WithScheme(scheme).Build()

	c := NewCache(runtime.NewScheme())
	ref := secretRefFor()
	seeded := fake.NewClientBuilder().WithScheme(scheme).Build()
	c.PutForTest("ops", ref, seeded)

	got, err := c.GetOrCreate(ctx, hubNoSecret, "ops", ref)
	if err != nil {
		t.Fatalf("graceful path must not error when a client is cached: %v", err)
	}
	if got != seeded {
		t.Errorf("expected the previously cached client to be served")
	}
}

func TestGetOrCreate_ErrorWhenNoCacheAndNoSecret(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	hub := fake.NewClientBuilder().WithScheme(scheme).Build()

	c := NewCache(runtime.NewScheme())
	if _, err := c.GetOrCreate(ctx, hub, "ops", secretRefFor()); err == nil {
		t.Fatalf("expected an error with no cached client and a missing Secret")
	}
}

func TestGetOrCreate_EmptyKey(t *testing.T) {
	ctx := context.Background()
	hub, _ := hubWithSecret(t, "")
	c := NewCache(runtime.NewScheme())

	_, err := c.GetOrCreate(ctx, hub, "ops", secretRefFor())
	if !errors.Is(err, ErrSecretKeyEmpty) {
		t.Fatalf("want ErrSecretKeyEmpty, got %v", err)
	}
}

func TestInvalidate(t *testing.T) {
	ctx := context.Background()
	hub, _ := hubWithSecret(t, miniKubeconfig)
	c := NewCache(runtime.NewScheme())
	ref := secretRefFor()

	cli1, err := c.GetOrCreate(ctx, hub, "ops", ref)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	c.Invalidate("ops", ref)
	cli2, err := c.GetOrCreate(ctx, hub, "ops", ref)
	if err != nil {
		t.Fatalf("GetOrCreate after invalidate: %v", err)
	}
	if cli2 == cli1 {
		t.Errorf("Invalidate must force a rebuild")
	}
}
