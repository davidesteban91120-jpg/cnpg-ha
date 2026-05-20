{{/*
Expand the name of the chart.
*/}}
{{- define "cnpg-ha.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified app name. Truncated at 63 chars (DNS label limit).
*/}}
{{- define "cnpg-ha.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Chart name+version label value.
*/}}
{{- define "cnpg-ha.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels applied to every resource. Keep `control-plane: controller-manager`
for parity with the Kustomize selectors and existing Service/ServiceMonitor matchers.
*/}}
{{- define "cnpg-ha.labels" -}}
helm.sh/chart: {{ include "cnpg-ha.chart" . }}
{{ include "cnpg-ha.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: cnpg-ha
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
Selector labels — stable across upgrades, used for Deployment selector
and Service/ServiceMonitor/NetworkPolicy matchers. Never add chart version here.
*/}}
{{- define "cnpg-ha.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cnpg-ha.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
control-plane: controller-manager
{{- end -}}

{{/*
Common annotations applied to every resource.
*/}}
{{- define "cnpg-ha.annotations" -}}
{{- with .Values.commonAnnotations }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
ServiceAccount name (respects create=true/false + name override).
*/}}
{{- define "cnpg-ha.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "cnpg-ha.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Pod anti-affinity preset. Renders only the inner block (the keys under
`podAntiAffinity:`); the caller is responsible for the `affinity:` /
`podAntiAffinity:` wrappers.
*/}}
{{- define "cnpg-ha.podAntiAffinityPreset" -}}
{{- $preset := .Values.podAntiAffinityPreset | default "none" -}}
{{- if eq $preset "soft" -}}
preferredDuringSchedulingIgnoredDuringExecution:
  - weight: 1
    podAffinityTerm:
      topologyKey: kubernetes.io/hostname
      labelSelector:
        matchLabels:
          {{- include "cnpg-ha.selectorLabels" . | nindent 10 }}
{{- else if eq $preset "hard" -}}
requiredDuringSchedulingIgnoredDuringExecution:
  - topologyKey: kubernetes.io/hostname
    labelSelector:
      matchLabels:
        {{- include "cnpg-ha.selectorLabels" . | nindent 8 }}
{{- end -}}
{{- end -}}

{{/*
Manager image reference. Uses .Chart.AppVersion when image.tag is empty.
*/}}
{{- define "cnpg-ha.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/*
Args passed to /manager. Single source of truth so `helm template` shows
exactly what the binary will receive. Order is deterministic for diff stability.
*/}}
{{- define "cnpg-ha.managerArgs" -}}
- --health-probe-bind-address={{ .Values.health.bindAddress }}
{{- if .Values.manager.leaderElect }}
- --leader-elect
{{- end }}
{{- if .Values.metrics.enabled }}
- --metrics-bind-address={{ .Values.metrics.bindAddress }}
- --metrics-secure={{ .Values.metrics.secure }}
{{- else }}
- --metrics-bind-address=0
{{- end }}
{{- if .Values.manager.enableHTTP2 }}
- --enable-http2
{{- end }}
{{- if .Values.webhook.enabled }}
- --webhook-cert-path={{ .Values.webhook.certPath }}
- --webhook-cert-name={{ .Values.webhook.certName }}
- --webhook-cert-key={{ .Values.webhook.certKey }}
{{- end }}
- --zap-log-level={{ .Values.log.level }}
- --zap-encoder={{ .Values.log.encoder }}
- --zap-time-encoding={{ .Values.log.timeEncoding }}
- --zap-stacktrace-level={{ .Values.log.stacktraceLevel }}
{{- if .Values.log.devel }}
- --zap-devel
{{- end }}
{{- range .Values.extraArgs }}
- {{ . }}
{{- end }}
{{- end -}}

{{/*
CRD keep annotation — emitted only when `crds.keep=true` so `helm uninstall`
does not delete the CRD (and the HACluster objects with it).
*/}}
{{- define "cnpg-ha.crdAnnotations" -}}
{{- if .Values.crds.keep }}
"helm.sh/resource-policy": keep
{{- end }}
{{- end -}}
