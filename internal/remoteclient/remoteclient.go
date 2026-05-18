/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package remoteclient gère la création et la mise en cache de clients
// Kubernetes pour les clusters distants référencés par un HACluster.
//
// Chaque site "replica" déclare un Secret contenant un kubeconfig. Ce package
// charge ce kubeconfig, construit un client.Client controller-runtime, et le
// cache pour éviter de reconstruire la config à chaque Reconcile.
package remoteclient

import (
	"context"
	"errors"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrSecretKeyEmpty est renvoyée quand la clé pointée dans le Secret est
// présente mais vide. Distinct de "Secret introuvable" pour faciliter
// le diagnostic côté operateur.
var ErrSecretKeyEmpty = errors.New("kubeconfig secret key is empty")

// cachedClient is a built remote client together with the resourceVersion
// of the kubeconfig Secret it was built from. A change of resourceVersion
// means the kubeconfig rotated and the client must be rebuilt.
type cachedClient struct {
	cli             client.Client
	resourceVersion string
}

// Cache stores remote Kubernetes clients keyed by the source kubeconfig
// Secret (namespace/name#key).
//
// Thread-safe. There is no TTL: instead, every GetOrCreate reads the source
// Secret (cheap — it lives in the local hub cluster) and rebuilds the
// client whenever the Secret's resourceVersion changed. A rotated
// kubeconfig is therefore picked up on the next reconcile, not only on a
// manager restart.
type Cache struct {
	mu      sync.RWMutex
	clients map[string]cachedClient
	scheme  *runtime.Scheme
}

// NewCache construit un Cache vide. Le scheme doit déjà connaître les types
// que le client distant manipulera (Secrets, CNPG Cluster, etc.). En pratique,
// passer le scheme du manager local suffit puisque les CRD sont identiques
// de chaque côté.
func NewCache(scheme *runtime.Scheme) *Cache {
	return &Cache{
		clients: make(map[string]cachedClient),
		scheme:  scheme,
	}
}

// GetOrCreate returns a client for the cluster described by the kubeconfig
// stored in the referenced Secret.
//
// It reads the Secret from hubClient (the cluster where the operator runs)
// and returns the cached client only when the Secret's resourceVersion is
// unchanged; otherwise it rebuilds. If the Secret cannot be read but a
// client is already cached, the stale client is returned rather than
// failing the whole reconcile (graceful degradation on a transient hub
// read error).
func (c *Cache) GetOrCreate(
	ctx context.Context,
	hubClient client.Client,
	namespace string,
	secretRef corev1.SecretKeySelector,
) (client.Client, error) {
	key := cacheKey(namespace, secretRef)

	kubeconfig, rv, err := c.loadKubeconfig(ctx, hubClient, namespace, secretRef)
	if err != nil {
		// Can't verify freshness. Keep serving a previously built client if
		// we have one; only fail when we have nothing to fall back to.
		c.mu.RLock()
		ent, ok := c.clients[key]
		c.mu.RUnlock()
		if ok {
			return ent.cli, nil
		}
		return nil, err
	}

	c.mu.RLock()
	ent, ok := c.clients[key]
	c.mu.RUnlock()
	if ok && ent.resourceVersion == rv {
		return ent.cli, nil
	}

	cli, err := c.clientFromKubeconfig(kubeconfig, namespace, secretRef.Name)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.clients[key] = cachedClient{cli: cli, resourceVersion: rv}
	c.mu.Unlock()
	return cli, nil
}

// Invalidate retire un client du cache. À appeler quand on détecte qu'un
// kubeconfig est obsolète (ex. erreur d'auth persistante) en plus du
// rafraîchissement automatique par resourceVersion.
func (c *Cache) Invalidate(namespace string, secretRef corev1.SecretKeySelector) {
	c.mu.Lock()
	delete(c.clients, cacheKey(namespace, secretRef))
	c.mu.Unlock()
}

// PutForTest preseeds the cache with a client for the given secretRef key.
// Test-only: production code goes through GetOrCreate. The entry carries an
// empty resourceVersion; GetOrCreate's graceful-degradation path returns it
// whenever the source Secret is absent (the usual unit-test setup).
func (c *Cache) PutForTest(namespace string, secretRef corev1.SecretKeySelector, cli client.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clients[cacheKey(namespace, secretRef)] = cachedClient{cli: cli}
}

// loadKubeconfig reads the Secret and returns the kubeconfig bytes plus the
// Secret's resourceVersion (the cache freshness key).
func (c *Cache) loadKubeconfig(
	ctx context.Context,
	hubClient client.Client,
	namespace string,
	secretRef corev1.SecretKeySelector,
) (kubeconfig []byte, resourceVersion string, err error) {
	var secret corev1.Secret
	nn := types.NamespacedName{Namespace: namespace, Name: secretRef.Name}
	if err := hubClient.Get(ctx, nn, &secret); err != nil {
		// Sécurité : ne pas inclure le contenu du Secret dans le message.
		return nil, "", fmt.Errorf("get kubeconfig secret %s/%s: %w", namespace, secretRef.Name, err)
	}

	kubeconfigBytes, ok := secret.Data[secretRef.Key]
	if !ok || len(kubeconfigBytes) == 0 {
		return nil, "", fmt.Errorf("secret %s/%s key %q: %w", namespace, secretRef.Name, secretRef.Key, ErrSecretKeyEmpty)
	}
	return kubeconfigBytes, secret.ResourceVersion, nil
}

// clientFromKubeconfig builds a controller-runtime client from raw
// kubeconfig bytes.
func (c *Cache) clientFromKubeconfig(kubeconfig []byte, namespace, name string) (client.Client, error) {
	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig from secret %s/%s: %w", namespace, name, err)
	}
	cli, err := client.New(restConfig, client.Options{Scheme: c.scheme})
	if err != nil {
		return nil, fmt.Errorf("build client from kubeconfig %s/%s: %w", namespace, name, err)
	}
	return cli, nil
}

func cacheKey(namespace string, secretRef corev1.SecretKeySelector) string {
	return namespace + "/" + secretRef.Name + "#" + secretRef.Key
}
