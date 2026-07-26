{{- define "kshows.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "kshows.fullname" -}}
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

{{- define "kshows.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "kshows.labels" -}}
helm.sh/chart: {{ include "kshows.chart" . }}
{{ include "kshows.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: kshows
{{- end }}

{{- define "kshows.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kshows.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "kshows.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kshows.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
ClusterRole and its binding are cluster-scoped, so their names must not
collide between releases in different namespaces.
*/}}
{{- define "kshows.clusterRoleName" -}}
{{- printf "%s-%s" (include "kshows.fullname" .) .Release.Namespace | trunc 63 | trimSuffix "-" }}
{{- end }}
