{{/*
snakeKeys recursively rewrites a dict's keys from camelCase to snake_case via Sprig's snakecase.
Non-map values pass through unchanged. Used to convert a values.yaml-style dict (camelCase keys)
into the snake_case shape that the Temporal Go SDK envconfig TOML decoder expects.

Returns YAML; the caller round-trips through fromYaml to get the dict back.

Limitation: every nested map's keys are rewritten. Subtrees whose keys are user-supplied identifiers
(e.g. grpc_meta header names) must NOT be passed through this helper — attach them after recursion.
*/}}
{{- define "media-processor.snakeKeys" -}}
{{- $out := dict -}}
{{- range $k, $v := . -}}
  {{- $sk := snakecase $k -}}
  {{- if kindIs "map" $v -}}
    {{- $_ := set $out $sk (include "media-processor.snakeKeys" $v | fromYaml) -}}
  {{- else -}}
    {{- $_ := set $out $sk $v -}}
  {{- end -}}
{{- end -}}
{{- toYaml $out -}}
{{- end -}}

{{/*
scaledjob renders one keda.sh/v1alpha1 ScaledJob per input controller. Caller
supplies a dict with keys "rootContext" (the chart root, post common.yaml
merge so .Values.global is populated) and "controllers" (a map of controller
name -> merged controller values, the partition stashed by common.yaml).

The Job pod template inside each ScaledJob is produced by the same bjw-s
helpers the Job/Deployment classes use (bjw-s.common.lib.pod.spec,
bjw-s.common.lib.pod.metadata.labels, bjw-s.common.lib.pod.metadata.annotations),
so env, volume mounts, securityContext, image, and the
app.kubernetes.io/controller pod-template label match what a Deployment for
the same controller would have produced.

KEDA-level fields (.spec.maxReplicaCount, .spec.pollingInterval, …) lift from
controller.keda.* onto the ScaledJob's top-level spec. Job-level fields
(backoffLimit, parallelism, …) lift from controller.job.* onto
.spec.jobTargetRef.spec.

.spec.triggers is intentionally an empty list — the Temporal triggers and the
TriggerAuthentication land in the follow-up sub-issue that depends on this one.
*/}}
{{- define "media-processor.scaledjob" -}}
{{- $rootContext := .rootContext -}}
{{- $controllers := .controllers -}}
{{- $fullName := include "bjw-s.common.lib.chart.names.fullname" $rootContext -}}
{{- range $name, $ctrl := $controllers }}
  {{- /* Build a synthetic controller object that bjw-s.common.lib.pod.spec /
       pod.metadata.labels / pod.metadata.annotations expect: a dict with the
       controller values plus identifier (and a Job-style restartPolicy default
       so the rendered pod template would run in a Job context). */ -}}
  {{- $controllerObject := mustDeepCopy $ctrl -}}
  {{- $_ := set $controllerObject "identifier" $name -}}
  {{- $pod := index $controllerObject "pod" | default dict -}}
  {{- if not (hasKey $pod "restartPolicy") -}}
    {{- $_ := set $pod "restartPolicy" "Never" -}}
  {{- end -}}
  {{- $_ := set $controllerObject "pod" $pod -}}

  {{- $resolvedName := printf "%s-%s" $fullName $name | lower | trunc 63 | trimSuffix "-" -}}

  {{- $topLabels := merge
    (dict "app.kubernetes.io/controller" $name)
    (index $controllerObject "labels" | default dict)
    (include "bjw-s.common.lib.metadata.allLabels" $rootContext | fromYaml)
  -}}
  {{- $topAnnotations := merge
    (index $controllerObject "annotations" | default dict)
    (include "bjw-s.common.lib.metadata.globalAnnotations" $rootContext | fromYaml)
  -}}

  {{- $podLabels := include "bjw-s.common.lib.pod.metadata.labels" (dict "rootContext" $rootContext "controllerObject" $controllerObject) -}}
  {{- $podAnnotations := include "bjw-s.common.lib.pod.metadata.annotations" (dict "rootContext" $rootContext "controllerObject" $controllerObject) -}}
  {{- $podSpec := include "bjw-s.common.lib.pod.spec" (dict "rootContext" $rootContext "controllerObject" $controllerObject) -}}

  {{- $kedaCfg := index $controllerObject "keda" | default dict -}}
  {{- $jobCfg := index $controllerObject "job" | default dict }}
---
apiVersion: keda.sh/v1alpha1
kind: ScaledJob
metadata:
  name: {{ $resolvedName }}
  namespace: {{ $rootContext.Release.Namespace }}
  {{- with $topLabels }}
  labels:
    {{- range $k, $v := . }}
    {{ printf "%s: %s" $k (tpl $v $rootContext | toYaml) }}
    {{- end }}
  {{- end }}
  {{- with $topAnnotations }}
  annotations:
    {{- range $k, $v := . }}
    {{ printf "%s: %s" $k (tpl $v $rootContext | toYaml) }}
    {{- end }}
  {{- end }}
spec:
  {{- with $kedaCfg.pollingInterval }}
  pollingInterval: {{ . }}
  {{- end }}
  {{- with $kedaCfg.successfulJobsHistoryLimit }}
  successfulJobsHistoryLimit: {{ . }}
  {{- end }}
  {{- with $kedaCfg.failedJobsHistoryLimit }}
  failedJobsHistoryLimit: {{ . }}
  {{- end }}
  {{- with $kedaCfg.maxReplicaCount }}
  maxReplicaCount: {{ . }}
  {{- end }}
  {{- with $kedaCfg.scalingStrategy }}
  scalingStrategy: {{ . | toYaml | nindent 4 }}
  {{- end }}
  triggers: []
  jobTargetRef:
    {{- with $jobCfg.parallelism }}
    parallelism: {{ . }}
    {{- end }}
    {{- with $jobCfg.completions }}
    completions: {{ . }}
    {{- end }}
    {{- with $jobCfg.activeDeadlineSeconds }}
    activeDeadlineSeconds: {{ . }}
    {{- end }}
    {{- with $jobCfg.backoffLimit }}
    backoffLimit: {{ . }}
    {{- end }}
    {{- with $jobCfg.ttlSecondsAfterFinished }}
    ttlSecondsAfterFinished: {{ . }}
    {{- end }}
    template:
      metadata:
        {{- with $podAnnotations }}
        annotations: {{ . | nindent 10 }}
        {{- end }}
        {{- with $podLabels }}
        labels: {{ . | nindent 10 }}
        {{- end }}
      spec: {{ $podSpec | nindent 8 }}
{{- end -}}
{{- end -}}
