{{/*
Names. They are prefixed with the release (or fullnameOverride), so more than
one release, or this chart used as a subchart, does not collide on the
cluster-scoped objects (ClusterRoles, the admission policy).
*/}}
{{- define "raspberrypi-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "raspberrypi-operator.fullname" -}}
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

{{- define "raspberrypi-operator.managerName" -}}
{{- printf "%s-manager" (include "raspberrypi-operator.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "raspberrypi-operator.agentName" -}}
{{- printf "%s-agent" (include "raspberrypi-operator.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "raspberrypi-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "raspberrypi-operator.labels" -}}
helm.sh/chart: {{ include "raspberrypi-operator.chart" . }}
app.kubernetes.io/name: {{ include "raspberrypi-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "raspberrypi-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "raspberrypi-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "raspberrypi-operator.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}

{{/* The user name the API server gives the agent's service account. */}}
{{- define "raspberrypi-operator.agentUser" -}}
{{- printf "system:serviceaccount:%s:%s" .Release.Namespace (include "raspberrypi-operator.agentName" .) }}
{{- end }}
