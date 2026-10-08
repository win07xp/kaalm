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

{{/*
kaalm.labelList renders a label map as a comma-separated key=value list, in
key order, for controller flags that take one.
*/}}
{{- define "kaalm.labelList" -}}
{{- $pairs := list -}}
{{- range $k, $v := . -}}
{{- $pairs = append $pairs (printf "%s=%s" $k $v) -}}
{{- end -}}
{{- join "," $pairs -}}
{{- end -}}

{{/*
The CA Bundle sources shared by the workload Bundle (kaalm-ca) and the
operator Bundle (kaalm-ca-system): the kaalm-ca Secret first, then
trustManager.extraSources verbatim.
*/}}
{{- define "kaalm.caBundleSources" -}}
- secret:
    name: kaalm-ca
    key: tls.crt
{{- with .Values.trustManager.extraSources }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Non-empty when the deprecated controller.probeCA bundle needs its own
projection into the controller: it is set and does not name the same
ConfigMap key as gateway.upstreamCA, which the controller already projects.
*/}}
{{- define "kaalm.controllerProbeCAProjected" -}}
{{- $p := .Values.controller.probeCA }}
{{- $u := .Values.gateway.upstreamCA }}
{{- if and $p.configMap (not (and (eq $p.configMap $u.configMap) (eq $p.key $u.key))) -}}
true
{{- end }}
{{- end }}

{{/*
kaalm.seconds takes (list NAME VALUE) and converts VALUE, a Go duration in
whole hours, minutes, or seconds (5s, 1m, 1m30s, or 0), into seconds. Any
other value fails the render and names NAME, because the Pod grace period
derived from it is a whole number of seconds.
*/}}
{{- define "kaalm.seconds" -}}
{{- $name := index . 0 -}}
{{- $v := index . 1 | toString -}}
{{- if or (eq $v "") (not (regexMatch "^(0|([0-9]+h)?([0-9]+m)?([0-9]+s)?)$" $v)) -}}
{{- fail (printf "kaalm: %s must be a duration in whole hours, minutes, or seconds, such as 5s, 1m, or 1m30s (got %q)" $name $v) -}}
{{- end -}}
{{- $h := regexFind "[0-9]+h" $v | trimSuffix "h" | default "0" | int -}}
{{- $m := regexFind "[0-9]+m" $v | trimSuffix "m" | default "0" | int -}}
{{- $s := regexFind "[0-9]+s" $v | trimSuffix "s" | default "0" | int -}}
{{- add (mul $h 3600) (mul $m 60) $s -}}
{{- end -}}

{{/*
kaalm.terminationGracePeriod takes (dict "name" "gateway.shutdown" "shutdown"
.Values.gateway.shutdown) and returns drainDelay + timeout + 5 seconds. The
extra 5 seconds cover the work after the timeout (the health listener's
shutdown and, on the gateway, the final budget publish), so Kubernetes never
kills a Pod mid-sequence.
*/}}
{{- define "kaalm.terminationGracePeriod" -}}
{{- $delay := include "kaalm.seconds" (list (printf "%s.drainDelay" .name) .shutdown.drainDelay) | int -}}
{{- $timeout := include "kaalm.seconds" (list (printf "%s.timeout" .name) .shutdown.timeout) | int -}}
{{- add $delay $timeout 5 -}}
{{- end -}}
