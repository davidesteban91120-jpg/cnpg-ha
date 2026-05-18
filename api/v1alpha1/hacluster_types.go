/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package v1alpha1 contient les types Go de l'API ha.ha.cnpg.io/v1alpha1.
//
// Note Go pour nouveau venu : un package = un dossier. Tous les fichiers
// .go ici doivent déclarer `package v1alpha1`. Le suffixe v1alpha1 signale
// que l'API est expérimentale (peut changer sans rétrocompatibilité).
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FailoverMode contrôle si l'opérateur déclenche un failover automatiquement
// ou attend une action humaine.
//
// +kubebuilder:validation:Enum=Automatic;Manual
type FailoverMode string

const (
	// FailoverModeAutomatic : l'opérateur promeut un replica dès que le seuil
	// de défaillance du primary est atteint. À privilégier si la latence de
	// reprise est critique et le risque de split-brain maîtrisé (fencing OK).
	FailoverModeAutomatic FailoverMode = "Automatic"

	// FailoverModeManual : l'opérateur détecte la panne et expose l'état,
	// mais attend une annotation `ha.cnpg.io/promote: <site>` pour agir.
	// À privilégier pour les bases sensibles ou les bascules planifiées.
	FailoverModeManual FailoverMode = "Manual"
)

// PromotionPolicy détermine quel replica devient le nouveau primary.
//
// +kubebuilder:validation:Enum=MostAdvancedLSN;Ordered
type PromotionPolicy string

const (
	// PromotionPolicyMostAdvancedLSN : choisit le replica dont le LSN PostgreSQL
	// (Log Sequence Number) est le plus avancé — minimise la perte de données.
	PromotionPolicyMostAdvancedLSN PromotionPolicy = "MostAdvancedLSN"

	// PromotionPolicyOrdered : suit l'ordre déclaré dans spec.replicas
	// (utile pour respecter un site préféré, ex. site B avant site C).
	PromotionPolicyOrdered PromotionPolicy = "Ordered"
)

// RejoinPolicy controls what the operator does with a former primary that
// comes back online while another site is the current primary.
//
// +kubebuilder:validation:Enum=Manual;AutoReplica
type RejoinPolicy string

const (
	// RejoinPolicyManual : the operator fences the returning primary and
	// raises the SplitBrain condition. Converting it back to a replica is
	// left to a human (no silent data loss). Safe default.
	RejoinPolicyManual RejoinPolicy = "Manual"

	// RejoinPolicyAutoReplica : the operator rebuilds the returning primary
	// as a replica of the current primary. This discards any writes the
	// returning site accepted after the failover (diverged timeline) — fully
	// automatic but destructive by design.
	RejoinPolicyAutoReplica RejoinPolicy = "AutoReplica"
)

// SiteRole indique le rôle observé d'un site lors du dernier Reconcile.
//
// +kubebuilder:validation:Enum=Primary;Replica;Unknown
type SiteRole string

const (
	// SiteRolePrimary : le CR CNPG Cluster du site est en mode primary
	// (spec.replica absent ou enabled=false).
	SiteRolePrimary SiteRole = "Primary"

	// SiteRoleReplica : le CR CNPG Cluster du site est en mode replica
	// (spec.replica.enabled=true).
	SiteRoleReplica SiteRole = "Replica"

	// SiteRoleUnknown : le site n'a pas pu être joint ou observé.
	// Aucune conclusion ne peut être tirée sur son rôle.
	SiteRoleUnknown SiteRole = "Unknown"
)

// ClusterRef pointe vers un CR CNPG `Cluster` (postgresql.cnpg.io/v1).
//
// Note : on ne référence pas directement le CNPG Cluster en Go (pas d'import
// croisé pour ne pas alourdir les dépendances). On vérifie son existence au
// runtime via le client K8s.
type ClusterRef struct {
	// Name du CR CNPG Cluster.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace du CR CNPG Cluster.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
}

// PrimarySite décrit le site local / bootstrap déclaré au départ. Après une
// bascule, le primary courant est porté par status.currentPrimarySite.
type PrimarySite struct {
	// Name est l'identifiant logique du site local / bootstrap (ex. "site-a").
	// Cohérent avec ReplicaSite.Name — peut être la valeur initiale de
	// status.currentPrimarySite et sert de clé dans les métriques. Doit être
	// unique au sein du HACluster (ne pas réutiliser un nom de replica).
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// ClusterRef pointe vers le CR CNPG Cluster du site local / bootstrap.
	// Il sert de primary au démarrage, mais après une bascule le primary
	// courant peut être l'un des sites de Spec.Replicas.
	//
	// +required
	ClusterRef ClusterRef `json:"clusterRef"`

	// ReplicationEndpoint is the host (optionally host:port) that the OTHER
	// sites use to stream WAL from this site when it is the current primary.
	// Typically the CNPG read-write Service FQDN
	// (e.g. "pg-prod-rw.site-a.svc.cluster.local"); in a Cilium Cluster Mesh
	// deployment, the global service name.
	//
	// When set, after a failover the operator rewrites every other site's
	// spec.replica externalCluster host to the new primary's endpoint so
	// surviving replicas follow the promotion automatically. When empty,
	// topology reconfiguration is skipped (the operator only observes).
	//
	// +optional
	ReplicationEndpoint string `json:"replicationEndpoint,omitempty"`
}

// ReplicaSite décrit un cluster CNPG distant servant de replica.
type ReplicaSite struct {
	// Name est l'identifiant logique du site (ex. "site-b"). Doit être
	// stable et unique au sein du HACluster — sert de clé dans le status
	// et dans les métriques Prometheus.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// KubeconfigSecretRef pointe vers un Secret du cluster local qui contient
	// le kubeconfig pour se connecter au cluster distant. L'opérateur lit
	// le Secret au démarrage et le rafraîchit selon un TTL interne.
	//
	// Sécurité : la clé pointée NE DOIT JAMAIS être loggée. Voir
	// internal/remoteclient pour la logique de redaction.
	//
	// +required
	KubeconfigSecretRef corev1.SecretKeySelector `json:"kubeconfigSecretRef"`

	// ClusterRef pointe vers le CR CNPG Cluster dans le cluster distant.
	//
	// +required
	ClusterRef ClusterRef `json:"clusterRef"`

	// ReplicationEndpoint is the host (optionally host:port) that the OTHER
	// sites use to stream WAL from this site once it has been promoted to
	// primary. Same semantics as PrimarySite.ReplicationEndpoint. Required
	// for this site to be eligible as an automatic-reconfiguration target
	// after a failover; when empty, other sites cannot be re-pointed here.
	//
	// +optional
	ReplicationEndpoint string `json:"replicationEndpoint,omitempty"`
}

// FailoverSpec regroupe les paramètres de décision de bascule.
type FailoverSpec struct {
	// Mode : Automatic ou Manual. Voir FailoverMode.
	//
	// +kubebuilder:default=Manual
	// +optional
	Mode FailoverMode `json:"mode,omitempty"`

	// HealthCheckIntervalSeconds : période entre deux sondes du primary.
	// 10s est un bon point de départ — trop court = flapping, trop long
	// = RTO dégradé.
	//
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=300
	// +optional
	HealthCheckIntervalSeconds int32 `json:"healthCheckIntervalSeconds,omitempty"`

	// FailureThreshold : nombre de sondes consécutives en échec avant de
	// considérer le primary HS. Toujours > 1 pour éviter qu'un blip réseau
	// déclenche un failover.
	//
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=20
	// +optional
	FailureThreshold int32 `json:"failureThreshold,omitempty"`

	// PromotionPolicy : voir PromotionPolicy.
	//
	// +kubebuilder:default=MostAdvancedLSN
	// +optional
	PromotionPolicy PromotionPolicy `json:"promotionPolicy,omitempty"`

	// RejoinPolicy controls what happens to a former primary that comes
	// back while another site is the current primary. See RejoinPolicy.
	//
	// +kubebuilder:default=Manual
	// +optional
	RejoinPolicy RejoinPolicy `json:"rejoinPolicy,omitempty"`
}

// HAClusterSpec décrit l'état désiré d'un HACluster.
type HAClusterSpec struct {
	// Primary : site qui sert actuellement les écritures.
	//
	// +required
	Primary PrimarySite `json:"primary"`

	// Replicas : sites distants prêts à être promus.
	//
	// +required
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=name
	Replicas []ReplicaSite `json:"replicas"`

	// Failover : paramètres de décision de bascule.
	//
	// +optional
	Failover FailoverSpec `json:"failover,omitempty"`
}

// SiteStatus expose l'observation d'un site (primary ou replica) lors du
// dernier Reconcile. Sert à l'inspection rapide via kubectl et de base à
// la future logique de promotion (qui consulte l'état de chaque candidat).
type SiteStatus struct {
	// Name est l'identifiant logique du site, cohérent avec
	// spec.primary.name ou spec.replicas[].name.
	//
	// +required
	Name string `json:"name"`

	// Role observé du site lors du dernier Reconcile.
	//
	// +required
	Role SiteRole `json:"role"`

	// Reachable indique si l'API K8s du site a répondu au dernier Reconcile.
	Reachable bool `json:"reachable"`

	// Ready indique si le CR CNPG Cluster du site a au moins une instance
	// ready (status.readyInstances > 0).
	Ready bool `json:"ready"`

	// Phase reprend status.phase du CR CNPG Cluster (ex. "Cluster in healthy
	// state"). Vide si le site est unreachable.
	//
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message porte un détail libre — typiquement la raison d'un échec
	// (kubeconfig invalide, Get K8s en erreur, readyInstances=0). Vide
	// quand le site est reachable et ready.
	//
	// +optional
	Message string `json:"message,omitempty"`

	// LastObservedTime est le timestamp du Reconcile qui a produit cette
	// observation. Mis à jour à chaque passage, même sans changement d'état.
	//
	// +optional
	LastObservedTime *metav1.Time `json:"lastObservedTime,omitempty"`
}

// HAClusterStatus décrit l'état observé d'un HACluster.
type HAClusterStatus struct {
	// CurrentPrimarySite : nom du dernier site accepté comme primary par
	// l'opérateur. Lors d'un failover, ce champ est mis à jour APRÈS la
	// promotion effective côté CNPG (jamais avant). Si ce site devient
	// temporairement unhealthy, le champ reste renseigné et l'indisponibilité
	// est exposée via les conditions.
	//
	// +optional
	CurrentPrimarySite string `json:"currentPrimarySite,omitempty"`

	// LastFailoverTime : timestamp du dernier failover réussi.
	//
	// +optional
	LastFailoverTime *metav1.Time `json:"lastFailoverTime,omitempty"`

	// ObservedGeneration : metadata.generation prise en compte lors du
	// dernier Reconcile. Permet de savoir si le status reflète le spec courant.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Sites : observation détaillée par site (primary + replicas).
	// Permet l'inspection rapide via
	// `kubectl get hacluster -o jsonpath='{.status.sites}'` et sert de
	// base à la logique de promotion.
	//
	// +listType=map
	// +listMapKey=name
	// +optional
	Sites []SiteStatus `json:"sites,omitempty"`

	// Conditions : état des aspects fonctionnels du HACluster.
	// Types attendus : "Available", "Progressing", "Degraded", "FailoverInProgress".
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=hac;hacluster
// +kubebuilder:printcolumn:name="Primary",type=string,JSONPath=`.status.currentPrimarySite`
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=`.status.conditions[?(@.type=="Available")].status`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.failover.mode`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// HACluster orchestre la bascule multi-cluster d'un groupe de Clusters CNPG.
type HACluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata standard.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec définit l'état désiré.
	// +required
	Spec HAClusterSpec `json:"spec"`

	// status reflète l'état observé.
	// +optional
	Status HAClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// HAClusterList contient une liste de HACluster.
type HAClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []HACluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HACluster{}, &HAClusterList{})
}
