{{/*
Common names and labels.
*/}}
{{- define "patchproof.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "patchproof.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "patchproof.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "patchproof.labels" -}}
helm.sh/chart: {{ include "patchproof.chart" . }}
{{ include "patchproof.selectorLabels" . }}
app.kubernetes.io/part-of: patchproof
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}

{{- define "patchproof.selectorLabels" -}}
app.kubernetes.io/name: {{ include "patchproof.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Render an image reference pinned by digest. Uses the repository and digest from
the supplied image object; tags are intentionally unsupported.
*/}}
{{- define "patchproof.image" -}}
{{- $digest := required (printf "%s image digest is required (sha256:<64 hex>)" .name) .image.digest -}}
{{- if not (regexMatch "^sha256:[a-f0-9]{64}$" $digest) -}}
{{- fail (printf "%s image digest must match sha256:<64 lowercase hex>" .name) -}}
{{- end -}}
{{- printf "%s@%s" .image.repository $digest -}}
{{- end }}

{{- define "patchproof.configMapName" -}}
{{- printf "%s-config" (include "patchproof.fullname" .) }}
{{- end }}

{{- define "patchproof.controllerConfigMapName" -}}
{{- printf "%s-controller-config" (include "patchproof.fullname" .) }}
{{- end }}

{{- define "patchproof.apiServiceAccountName" -}}
{{- if .Values.api.serviceAccount.name }}
{{- .Values.api.serviceAccount.name }}
{{- else }}
{{- printf "%s-api" (include "patchproof.fullname" .) }}
{{- end }}
{{- end }}

{{- define "patchproof.controllerServiceAccountName" -}}
{{- if .Values.controller.serviceAccount.name }}
{{- .Values.controller.serviceAccount.name }}
{{- else }}
{{- printf "%s-controller" (include "patchproof.fullname" .) }}
{{- end }}
{{- end }}

{{- define "patchproof.clusterAgentServiceAccountName" -}}
{{- if .Values.clusterAgent.serviceAccount.name }}
{{- .Values.clusterAgent.serviceAccount.name }}
{{- else }}
{{- printf "%s-cluster-agent" (include "patchproof.fullname" .) }}
{{- end }}
{{- end }}

{{/*
Default topology spread and anti-affinity keep replicas off the same node and
zone where the scheduler allows it. Values may override either block.
*/}}
{{- define "patchproof.topologySpreadConstraints" -}}
{{- if .constraints }}
{{- toYaml .constraints }}
{{- else }}
- maxSkew: 1
  topologyKey: topology.kubernetes.io/zone
  whenUnsatisfiable: ScheduleAnyway
  labelSelector:
    matchLabels:
      {{- include "patchproof.selectorLabels" .root | nindent 6 }}
      app.kubernetes.io/component: {{ .component }}
- maxSkew: 1
  topologyKey: kubernetes.io/hostname
  whenUnsatisfiable: ScheduleAnyway
  labelSelector:
    matchLabels:
      {{- include "patchproof.selectorLabels" .root | nindent 6 }}
      app.kubernetes.io/component: {{ .component }}
{{- end }}
{{- end }}

{{- define "patchproof.podAntiAffinity" -}}
podAntiAffinity:
  preferredDuringSchedulingIgnoredDuringExecution:
    - weight: 100
      podAffinityTerm:
        topologyKey: kubernetes.io/hostname
        labelSelector:
          matchLabels:
            {{- include "patchproof.selectorLabels" .root | nindent 12 }}
            app.kubernetes.io/component: {{ .component }}
{{- end }}
