{{/* Common labels applied to every Kaalm object. */}}
{{- define "kaalm.labels" -}}
app.kubernetes.io/name: kaalm
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{/* Guard: both Deployments have a hard floor of 2 replicas. */}}
{{- define "kaalm.replicaFloor" -}}
{{- if lt (int .) 2 -}}
{{- fail "kaalm: replicas must be >= 2 (controller and gateway both require a floor of 2 for availability)" -}}
{{- end -}}
{{- end -}}

{{/*
Convert a human-readable size (a plain integer, or one with a Ki/Mi/Gi binary
suffix) into a byte count for the gateway's int64 flags.
*/}}
{{- define "kaalm.bytes" -}}
{{- $v := . | toString -}}
{{- if hasSuffix "Gi" $v -}}
{{- mul (trimSuffix "Gi" $v | int64) 1073741824 -}}
{{- else if hasSuffix "Mi" $v -}}
{{- mul (trimSuffix "Mi" $v | int64) 1048576 -}}
{{- else if hasSuffix "Ki" $v -}}
{{- mul (trimSuffix "Ki" $v | int64) 1024 -}}
{{- else -}}
{{- $v | int64 -}}
{{- end -}}
{{- end -}}
