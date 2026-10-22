{{/*
Expand the name of the chart.
*/}}
{{- define "iam-realm-provisioner.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to
this (by the DNS naming spec).
*/}}
{{- define "iam-realm-provisioner.fullname" -}}
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

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "iam-realm-provisioner.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels applied to every resource.
*/}}
{{- define "iam-realm-provisioner.labels" -}}
helm.sh/chart: {{ include "iam-realm-provisioner.chart" . }}
{{ include "iam-realm-provisioner.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels — used by Deployment, Service, HPA, PDB, ServiceMonitor,
and every CronJob (so NetworkPolicy egress rules cover CronJob pods too,
since they select on these same labels).
*/}}
{{- define "iam-realm-provisioner.selectorLabels" -}}
app.kubernetes.io/name: {{ include "iam-realm-provisioner.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use.
*/}}
{{- define "iam-realm-provisioner.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "iam-realm-provisioner.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Image tag: prefer .Values.image.tag; fall back to chart appVersion. One
image carries both binaries (server + reconciler) — the Deployment and all
eight CronJobs share this same tag.
*/}}
{{- define "iam-realm-provisioner.imageTag" -}}
{{- .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{/*
Name of the Secret holding application secrets: either a pre-existing,
externally-managed Secret (.Values.existingSecret), or the one this chart
renders itself from .Values.secretValues (see templates/secret.yaml).
Shared by the server Deployment and every reconciler CronJob.
*/}}
{{- define "iam-realm-provisioner.secretName" -}}
{{- default (printf "%s-secrets" (include "iam-realm-provisioner.fullname" .)) .Values.existingSecret }}
{{- end }}
