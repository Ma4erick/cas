{{/*
Expand the name of the chart.
*/}}
{{- define "cas.name" -}}
{{- .Chart.Name }}
{{- end }}

{{/*
Full name — release + chart name.
*/}}
{{- define "cas.fullname" -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "cas.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{ include "cas.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "cas.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cas.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
