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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	k8sevents "k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	hav1alpha1 "github.com/davidesteban/cnpg-ha/api/v1alpha1"
	"github.com/davidesteban/cnpg-ha/internal/remoteclient"
)

// kubeconfigFromRest serializes the envtest rest.Config into a kubeconfig.
// Storing it in a Secret lets a "replica site" resolve, via the real
// remoteclient path, back to the same envtest API server (a different
// namespace stands in for a different cluster).
func kubeconfigFromRest(cfg *rest.Config) ([]byte, error) {
	c := clientcmdapi.NewConfig()
	c.Clusters["envtest"] = &clientcmdapi.Cluster{
		Server:                   cfg.Host,
		CertificateAuthorityData: cfg.CAData,
		InsecureSkipTLSVerify:    len(cfg.CAData) == 0,
	}
	c.AuthInfos["envtest"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientKeyData:         cfg.KeyData,
		Token:                 cfg.BearerToken,
	}
	c.Contexts["envtest"] = &clientcmdapi.Context{Cluster: "envtest", AuthInfo: "envtest"}
	c.CurrentContext = "envtest"
	return clientcmd.Write(*c)
}

func mkNamespace(ctx context.Context, name string) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := k8sClient.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
}

// mkCNPGCluster creates a ready CNPG Cluster CR (readyInstances=1,
// timelineID=1). replicaEnabled=nil ⇒ spec.replica absent (primary mode).
func mkCNPGCluster(ctx context.Context, ns string, replicaEnabled *bool) {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(cnpgClusterGVK)
	u.SetNamespace(ns)
	u.SetName("pg-prod")
	if replicaEnabled != nil {
		Expect(unstructured.SetNestedField(u.Object, *replicaEnabled, "spec", "replica", "enabled")).To(Succeed())
		Expect(unstructured.SetNestedField(u.Object, "src", "spec", "replica", "source")).To(Succeed())
		Expect(unstructured.SetNestedSlice(u.Object, []any{
			map[string]any{"name": "src", "connectionParameters": map[string]any{"host": "old"}},
		}, "spec", "externalClusters")).To(Succeed())
	}
	Expect(unstructured.SetNestedField(u.Object, "Cluster in healthy state", "status", "phase")).To(Succeed())
	Expect(unstructured.SetNestedField(u.Object, int64(1), "status", "readyInstances")).To(Succeed())
	Expect(unstructured.SetNestedField(u.Object, int64(1), "status", "timelineID")).To(Succeed())
	Expect(k8sClient.Create(ctx, u)).To(Succeed())
}

func getCNPGCluster(ctx context.Context, ns string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(cnpgClusterGVK)
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "pg-prod"}, u)).To(Succeed())
	return u
}

func mkRWService(ctx context.Context, ns string) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "pg-prod-rw"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 5432}}},
	}
	Expect(k8sClient.Create(ctx, svc)).To(Succeed())
}

var _ = Describe("HACluster Controller (envtest, real API server)", func() {
	ctx := context.Background()

	newReconciler := func() (*HAClusterReconciler, *k8sevents.FakeRecorder) {
		rec := k8sevents.NewFakeRecorder(50)
		return &HAClusterReconciler{
			Client:        k8sClient,
			Scheme:        k8sClient.Scheme(),
			RemoteClients: remoteclient.NewCache(k8sClient.Scheme()),
			Recorder:      rec,
		}, rec
	}

	Context("observation", func() {
		It("populates status.currentPrimarySite and conditions through the API server", func() {
			mkNamespace(ctx, "obs-a")
			mkNamespace(ctx, "obs-b")
			mkCNPGCluster(ctx, "obs-a", nil) // primary, ready
			tb := true
			mkCNPGCluster(ctx, "obs-b", &tb) // replica, ready

			kc, err := kubeconfigFromRest(cfg)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "obs-a", Name: "site-b-kc"},
				Data:       map[string][]byte{"kubeconfig": kc},
			})).To(Succeed())

			ha := &hav1alpha1.HACluster{
				ObjectMeta: metav1.ObjectMeta{Name: "obs", Namespace: "obs-a"},
				Spec: hav1alpha1.HAClusterSpec{
					Primary: hav1alpha1.PrimarySite{
						Name:       "site-a",
						ClusterRef: hav1alpha1.ClusterRef{Name: "pg-prod", Namespace: "obs-a"},
					},
					Replicas: []hav1alpha1.ReplicaSite{{
						Name: siteB,
						KubeconfigSecretRef: corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "site-b-kc"},
							Key:                  "kubeconfig",
						},
						ClusterRef: hav1alpha1.ClusterRef{Name: "pg-prod", Namespace: "obs-b"},
					}},
					Failover: hav1alpha1.FailoverSpec{Mode: hav1alpha1.FailoverModeManual},
				},
			}
			Expect(k8sClient.Create(ctx, ha)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ha) })

			r, _ := newReconciler()
			_, err = r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "obs", Namespace: "obs-a"},
			})
			Expect(err).NotTo(HaveOccurred())

			got := &hav1alpha1.HACluster{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "obs", Namespace: "obs-a"}, got)).To(Succeed())
			Expect(got.Status.CurrentPrimarySite).To(Equal("site-a"))

			avail := findCondition(got.Status.Conditions, conditionAvailable)
			Expect(avail).NotTo(BeNil())
			Expect(string(avail.Status)).To(Equal("True"))
			Expect(avail.Reason).To(Equal("PrimaryReady"))

			By("site-b observed as a ready Replica")
			var sb *hav1alpha1.SiteStatus
			for i := range got.Status.Sites {
				if got.Status.Sites[i].Name == siteB {
					sb = &got.Status.Sites[i]
				}
			}
			Expect(sb).NotTo(BeNil())
			Expect(sb.Role).To(Equal(hav1alpha1.SiteRoleReplica))
			Expect(sb.Ready).To(BeTrue())
		})
	})

	Context("manual failover", func() {
		It("promotes the annotated replica end-to-end via the API server", func() {
			mkNamespace(ctx, "mf-a")
			mkNamespace(ctx, "mf-b")
			mkCNPGCluster(ctx, "mf-a", nil) // primary, ready
			tb := true
			mkCNPGCluster(ctx, "mf-b", &tb) // replica, ready
			mkRWService(ctx, "mf-a")
			mkRWService(ctx, "mf-b")

			kc, err := kubeconfigFromRest(cfg)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "mf-a", Name: "site-b-kc"},
				Data:       map[string][]byte{"kubeconfig": kc},
			})).To(Succeed())

			ha := &hav1alpha1.HACluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "mf",
					Namespace:   "mf-a",
					Annotations: map[string]string{annotationPromote: siteB},
				},
				Spec: hav1alpha1.HAClusterSpec{
					Primary: hav1alpha1.PrimarySite{
						Name:       "site-a",
						ClusterRef: hav1alpha1.ClusterRef{Name: "pg-prod", Namespace: "mf-a"},
					},
					Replicas: []hav1alpha1.ReplicaSite{{
						Name: siteB,
						KubeconfigSecretRef: corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "site-b-kc"},
							Key:                  "kubeconfig",
						},
						ClusterRef: hav1alpha1.ClusterRef{Name: "pg-prod", Namespace: "mf-b"},
					}},
					Failover: hav1alpha1.FailoverSpec{Mode: hav1alpha1.FailoverModeManual},
				},
			}
			Expect(k8sClient.Create(ctx, ha)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ha) })

			r, rec := newReconciler()
			_, err = r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "mf", Namespace: "mf-a"},
			})
			Expect(err).NotTo(HaveOccurred())

			By("target CNPG Cluster promoted (spec.replica.enabled=false)")
			newPrimary := getCNPGCluster(ctx, "mf-b")
			enabled, _, _ := unstructured.NestedBool(newPrimary.Object, "spec", "replica", "enabled")
			Expect(enabled).To(BeFalse())

			By("old primary fenced")
			oldPrimary := getCNPGCluster(ctx, "mf-a")
			Expect(oldPrimary.GetAnnotations()["cnpg.io/fencedInstances"]).To(Equal(`["*"]`))

			By("HACluster status + annotation reflect the failover")
			got := &hav1alpha1.HACluster{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "mf", Namespace: "mf-a"}, got)).To(Succeed())
			Expect(got.Annotations).NotTo(HaveKey(annotationPromote))
			Expect(got.Status.CurrentPrimarySite).To(Equal(siteB))
			Expect(got.Status.LastFailoverTime).NotTo(BeNil())

			By("a FailoverCompleted event was recorded")
			var sawCompleted bool
			for done := false; !done; {
				select {
				case e := <-rec.Events:
					if strings.Contains(e, eventReasonFailoverCompleted) {
						sawCompleted = true
					}
				default:
					done = true
				}
			}
			Expect(sawCompleted).To(BeTrue())
		})
	})
})
