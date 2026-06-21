{{/* Expand the name of the chart. */}}
{{- define "bigfleet-upcloud.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* A fully qualified app name. */}}
{{- define "bigfleet-upcloud.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "bigfleet-upcloud.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "bigfleet-upcloud.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "bigfleet-upcloud.labels" -}}
helm.sh/chart: {{ include "bigfleet-upcloud.chart" . }}
{{ include "bigfleet-upcloud.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "bigfleet-upcloud.selectorLabels" -}}
app.kubernetes.io/name: {{ include "bigfleet-upcloud.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "bigfleet-upcloud.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "bigfleet-upcloud.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "bigfleet-upcloud.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}
