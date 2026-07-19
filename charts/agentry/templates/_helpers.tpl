{{/* Common labels applied to every Agentry object. */}}
{{- define "agentry.labels" -}}
app.kubernetes.io/name: agentry
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{/* Guard: both Deployments have a hard floor of 2 replicas. */}}
{{- define "agentry.replicaFloor" -}}
{{- if lt (int .) 2 -}}
{{- fail "agentry: replicas must be >= 2 (controller and gateway both require a floor of 2 for availability)" -}}
{{- end -}}
{{- end -}}
