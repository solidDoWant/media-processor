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
resolveActivities mirrors cmd/worker/activities_resolver.go: it evaluates a
list of WORKER_ACTIVITIES tokens left-to-right against an initially empty
set, then returns the resolved set as a comma-joined string in the canonical
order of the supplied known-tokens list. Caller supplies a dict with keys
"tokens" (the raw token list, untrimmed) and "knownTokens" (the chart-side
known set, mirroring workflows/media/config.go's KnownActivities + WorkflowToken).

Token grammar:
  - "all"   sets the working set to every known token
  - "name"  adds that token to the set
  - "!name" removes that token from the set

Returns the empty string when the input has no tokens or every token is
removed; callers use that to detect an empty resolved set. Unknown tokens
fail rendering with a fail() call so configuration drift surfaces immediately
rather than producing silently-empty trigger lists.

The output is deliberately a comma-joined string (not a list) because Helm's
template language does not let a `define` return a list / dict directly; the
caller splits on "," to recover the resolved tokens.
*/}}
{{- define "media-processor.resolveActivities" -}}
{{- $tokens := index . "tokens" -}}
{{- $knownTokens := index . "knownTokens" -}}
{{- $set := dict -}}
{{- range $rawToken := $tokens -}}
  {{- $token := trim (toString $rawToken) -}}
  {{- if $token -}}
    {{- if eq $token "all" -}}
      {{- range $knownToken := $knownTokens -}}
        {{- $_ := set $set $knownToken true -}}
      {{- end -}}
    {{- else -}}
      {{- $negate := false -}}
      {{- $name := $token -}}
      {{- if hasPrefix "!" $name -}}
        {{- $negate = true -}}
        {{- $name = trim (substr 1 (len $name) $name) -}}
      {{- end -}}
      {{- if not (has $name $knownTokens) -}}
        {{- fail (printf "WORKER_ACTIVITIES: unknown token %q (known: %s)" $token (join ", " $knownTokens)) -}}
      {{- end -}}
      {{- if $negate -}}
        {{- $_ := unset $set $name -}}
      {{- else -}}
        {{- $_ := set $set $name true -}}
      {{- end -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- $resolved := list -}}
{{- range $knownToken := $knownTokens -}}
  {{- if hasKey $set $knownToken -}}
    {{- $resolved = append $resolved $knownToken -}}
  {{- end -}}
{{- end -}}
{{- join "," $resolved -}}
{{- end -}}

{{/*
triggerAuthentication renders the per-release keda.sh/v1alpha1
TriggerAuthentication that backs every Temporal trigger emitted by
media-processor.scaledjob. Caller supplies a dict with keys "rootContext"
(the chart root, post common.yaml merge so bjw-s helpers can resolve the
fullname / labels), "name" (the resolved authRef name), "apiKeySecret"
(dict {"name", "key"} pointing at the Secret carrying the KEDA-only API
key — either operator-supplied via secretKeyRef or chart-managed when
config.temporal.keda.apiKey.value is set), and "tlsSecrets" (a list of
{"parameter", "name", "key"} entries for the optional TLS material under
config.temporal.keda.tls.*).

Emitted only when at least one type: scaledjob controller exists; the
gating happens in the caller (templates/common.yaml) so the default
chart render stays byte-identical to pre-change.
*/}}
{{- define "media-processor.triggerAuthentication" -}}
{{- $rootContext := .rootContext -}}
{{- $name := .name -}}
{{- $apiKeySecret := .apiKeySecret -}}
{{- $tlsSecrets := .tlsSecrets | default list -}}
{{- $topLabels := include "bjw-s.common.lib.metadata.allLabels" $rootContext | fromYaml -}}
{{- $topAnnotations := include "bjw-s.common.lib.metadata.globalAnnotations" $rootContext | fromYaml -}}
---
apiVersion: keda.sh/v1alpha1
kind: TriggerAuthentication
metadata:
  name: {{ $name }}
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
  secretTargetRef:
    - parameter: apiKey
      name: {{ $apiKeySecret.name }}
      key: {{ $apiKeySecret.key }}
    {{- range $entry := $tlsSecrets }}
    - parameter: {{ $entry.parameter }}
      name: {{ $entry.name }}
      key: {{ $entry.key }}
    {{- end }}
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

.spec.triggers is computed from the controller's resolved activity tokens —
one Temporal trigger per resolved task queue. Activity tokens map to
"{prefix}-{token}" queues (queueType Activity); the workflow token maps to
the prefix-only queue (queueType Workflow). Caller supplies a dict with
the additional keys "temporalEndpoint", "temporalNamespace", "taskQueuePrefix",
"authRefName", "knownTokens", and "workersByName" (the chart-validated
workers map keyed on controller name) so the helper can resolve each
controller's tokens via media-processor.resolveActivities. Per-controller
keda.targetQueueSize / keda.activationTargetQueueSize override the chart
defaults (5 / 0).
*/}}
{{- define "media-processor.scaledjob" -}}
{{- $rootContext := .rootContext -}}
{{- $controllers := .controllers -}}
{{- $temporalEndpoint := .temporalEndpoint -}}
{{- $temporalNamespace := .temporalNamespace -}}
{{- $taskQueuePrefix := .taskQueuePrefix -}}
{{- $authRefName := .authRefName -}}
{{- $knownTokens := .knownTokens -}}
{{- $workersByName := .workersByName -}}
{{- $fullName := include "bjw-s.common.lib.chart.names.fullname" $rootContext -}}
{{- range $name, $ctrl := $controllers }}
  {{- /* Honor controller.enabled so a user-disabled scaledjob controller
       produces no output, mirroring bjw-s.common.lib.controller.enabledControllers
       for the deployment path. Tpl-evaluate the value to also handle the
       chart's own '{{ … }}' string-template form (the deployment-typed
       defaults already use that pattern via the post-merge enabled-template
       eval in common.yaml). */ -}}
  {{- $enabled := true -}}
  {{- if hasKey $ctrl "enabled" -}}
    {{- $enabled = eq (tpl (get $ctrl "enabled" | toString) $rootContext) "true" -}}
  {{- end -}}
  {{- if $enabled -}}
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
  {{- $jobCfg := index $controllerObject "job" | default dict -}}

  {{- /* Resolve the controller's activity tokens via the same left-to-right
       grammar as cmd/worker/activities_resolver.go. The workersByName map is
       chart-validated upstream (templates/common.yaml partitioning), so a
       missing entry would mean the partitioning logic and the workers map
       have drifted apart — fail loudly rather than emitting an empty trigger
       list that would silently break scaling. */ -}}
  {{- $workerEntry := index $workersByName $name -}}
  {{- if not $workerEntry -}}
    {{- fail (printf "scaledjob controller %q has no entry in workers" $name) -}}
  {{- end -}}
  {{- $rawTokens := index $workerEntry "activities" -}}
  {{- $resolved := include "media-processor.resolveActivities" (dict "tokens" $rawTokens "knownTokens" $knownTokens) -}}
  {{- if not $resolved -}}
    {{- fail (printf "scaledjob controller %q resolved to an empty activity set" $name) -}}
  {{- end -}}
  {{- $resolvedTokens := splitList "," $resolved -}}

  {{- /* Default targetQueueSize / activationTargetQueueSize per trigger.
       Operator overrides land via resources.controllers.<name>.keda.*. */ -}}
  {{- $targetQueueSize := index $kedaCfg "targetQueueSize" | default 5 -}}
  {{- $activationTargetQueueSize := index $kedaCfg "activationTargetQueueSize" | default 0 -}}

  {{- /* Build the trigger list. Activity tokens emit Activity-typed triggers
       on "{prefix}-{token}"; the workflow token emits a Workflow-typed
       trigger on the prefix-only queue. Order matches knownTokens order
       (resolver already canonicalises). */ -}}
  {{- $triggers := list -}}
  {{- range $token := $resolvedTokens -}}
    {{- $queueName := "" -}}
    {{- $queueType := "" -}}
    {{- if eq $token "workflow" -}}
      {{- $queueName = $taskQueuePrefix -}}
      {{- $queueType = "Workflow" -}}
    {{- else -}}
      {{- $queueName = printf "%s-%s" $taskQueuePrefix $token -}}
      {{- $queueType = "Activity" -}}
    {{- end -}}
    {{- $trigger := dict
      "type" "temporal"
      "metadata" (dict
        "endpoint" $temporalEndpoint
        "namespace" $temporalNamespace
        "queueName" $queueName
        "queueType" $queueType
        "targetQueueSize" ($targetQueueSize | toString)
        "activationTargetQueueSize" ($activationTargetQueueSize | toString)
      )
      "authenticationRef" (dict "name" $authRefName)
    -}}
    {{- $triggers = append $triggers $trigger -}}
  {{- end }}
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
  {{- /* hasKey rather than `with` so an explicit zero (e.g.
       successfulJobsHistoryLimit: 0 or backoffLimit: 0) is preserved —
       Go template falsiness drops 0 with `with`, but KEDA / Kubernetes
       JobSpec accept 0 as a valid value with distinct meaning. */ -}}
  {{- if hasKey $kedaCfg "pollingInterval" }}
  pollingInterval: {{ get $kedaCfg "pollingInterval" }}
  {{- end }}
  {{- if hasKey $kedaCfg "successfulJobsHistoryLimit" }}
  successfulJobsHistoryLimit: {{ get $kedaCfg "successfulJobsHistoryLimit" }}
  {{- end }}
  {{- if hasKey $kedaCfg "failedJobsHistoryLimit" }}
  failedJobsHistoryLimit: {{ get $kedaCfg "failedJobsHistoryLimit" }}
  {{- end }}
  {{- if hasKey $kedaCfg "maxReplicaCount" }}
  maxReplicaCount: {{ get $kedaCfg "maxReplicaCount" }}
  {{- end }}
  {{- with $kedaCfg.scalingStrategy }}
  scalingStrategy: {{ . | toYaml | nindent 4 }}
  {{- end }}
  triggers:
    {{- range $trigger := $triggers }}
    - type: {{ $trigger.type }}
      metadata:
        endpoint: {{ $trigger.metadata.endpoint }}
        namespace: {{ $trigger.metadata.namespace }}
        queueName: {{ $trigger.metadata.queueName }}
        queueType: {{ $trigger.metadata.queueType }}
        targetQueueSize: {{ $trigger.metadata.targetQueueSize | quote }}
        activationTargetQueueSize: {{ $trigger.metadata.activationTargetQueueSize | quote }}
      authenticationRef:
        name: {{ $trigger.authenticationRef.name }}
    {{- end }}
  jobTargetRef:
    {{- if hasKey $jobCfg "parallelism" }}
    parallelism: {{ get $jobCfg "parallelism" }}
    {{- end }}
    {{- if hasKey $jobCfg "completions" }}
    completions: {{ get $jobCfg "completions" }}
    {{- end }}
    {{- if hasKey $jobCfg "activeDeadlineSeconds" }}
    activeDeadlineSeconds: {{ get $jobCfg "activeDeadlineSeconds" }}
    {{- end }}
    {{- if hasKey $jobCfg "backoffLimit" }}
    backoffLimit: {{ get $jobCfg "backoffLimit" }}
    {{- end }}
    {{- if hasKey $jobCfg "ttlSecondsAfterFinished" }}
    ttlSecondsAfterFinished: {{ get $jobCfg "ttlSecondsAfterFinished" }}
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
{{- end -}}
