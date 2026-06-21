{{/* Expand the name of the chart. */}}
{{- define "bigfleet-proxmox.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* A fully qualified app name. */}}
{{- define "bigfleet-proxmox.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "bigfleet-proxmox.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "bigfleet-proxmox.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "bigfleet-proxmox.labels" -}}
helm.sh/chart: {{ include "bigfleet-proxmox.chart" . }}
{{ include "bigfleet-proxmox.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "bigfleet-proxmox.selectorLabels" -}}
app.kubernetes.io/name: {{ include "bigfleet-proxmox.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "bigfleet-proxmox.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "bigfleet-proxmox.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "bigfleet-proxmox.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}
