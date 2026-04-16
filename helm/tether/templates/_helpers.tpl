{{/*
Expand the name of the chart.
*/}}
{{- define "tether.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "tether.operator.labels" -}}
app.kubernetes.io/name: tether-operator
app.kubernetes.io/part-of: tether
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{- define "tether.proxy.labels" -}}
app.kubernetes.io/name: tether-proxy
app.kubernetes.io/part-of: tether
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{- define "tether.proxy.tlsSecretName" -}}
{{- if .Values.proxy.tls.existingSecret -}}
  {{ .Values.proxy.tls.existingSecret }}
{{- else -}}
  tether-proxy-tls
{{- end -}}
{{- end }}
