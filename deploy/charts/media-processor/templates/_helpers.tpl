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
"workerName" (used in error messages so operators can find the offending
workers.<name>.activities entry when multiple workers are defined), "tokens"
(the raw token list, untrimmed), and "knownTokens" (the chart-side known
set, mirroring workflows/media/config.go's KnownActivities + WorkflowToken).

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
{{- $workerName := index . "workerName" | default "" -}}
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
        {{- fail (printf "workers.%s.activities: unknown token %q (known: %s)" $workerName $token (join ", " $knownTokens)) -}}
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
    {{ $k }}: {{ tpl $v $rootContext | quote }}
    {{- end }}
  {{- end }}
  {{- with $topAnnotations }}
  annotations:
    {{- range $k, $v := . }}
    {{ $k }}: {{ tpl $v $rootContext | quote }}
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
podDisruptionBudget renders one policy/v1 PodDisruptionBudget for a single
scaledjob controller. bjw-s renders PDBs through its own controller loop,
which the chart cannot reach for type: scaledjob workers (they are stripped
from the controllers map before bjw-s' generate pass runs), so the chart
emits PDBs for those workers itself.

Caller supplies a dict with keys "rootContext" (the chart root, post
common.yaml merge so bjw-s helpers resolve fullname / labels), "controllerName"
(the worker name, used both as the resource-name suffix and as the
app.kubernetes.io/controller selector value — matches the label
bjw-s.common.lib.pod.metadata.labels stamps onto the rendered Job pod
template), and "pdb" (the merged podDisruptionBudget block carrying any
combination of minAvailable / maxUnavailable from the chart default plus
operator overrides).

`hasKey` rather than `with` for minAvailable / maxUnavailable so an explicit
zero (the scaledjob chart default's maxUnavailable: 0 is exactly this case)
is preserved — Go template falsiness drops 0 with `with`, but PDB treats 0
as a valid value with distinct meaning.

Emitted only when the merged controller still carries a podDisruptionBudget
block; the gating happens in the caller (templates/common.yaml) so a user-
supplied podDisruptionBudget: null or .enabled: false produces no output.
*/}}
{{- define "media-processor.podDisruptionBudget" -}}
{{- $rootContext := .rootContext -}}
{{- $controllerName := .controllerName -}}
{{- $pdb := .pdb -}}
{{- $fullName := include "bjw-s.common.lib.chart.names.fullname" $rootContext -}}
{{- $resolvedName := printf "%s-%s" $fullName $controllerName | lower | trunc 63 | trimSuffix "-" -}}
{{- $topLabels := merge
    (dict "app.kubernetes.io/controller" $controllerName)
    (include "bjw-s.common.lib.metadata.allLabels" $rootContext | fromYaml)
-}}
{{- $topAnnotations := include "bjw-s.common.lib.metadata.globalAnnotations" $rootContext | fromYaml -}}
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: {{ $resolvedName }}
  namespace: {{ $rootContext.Release.Namespace }}
  {{- with $topLabels }}
  labels:
    {{- range $k, $v := . }}
    {{ $k }}: {{ tpl $v $rootContext | quote }}
    {{- end }}
  {{- end }}
  {{- with $topAnnotations }}
  annotations:
    {{- range $k, $v := . }}
    {{ $k }}: {{ tpl $v $rootContext | quote }}
    {{- end }}
  {{- end }}
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: {{ include "bjw-s.common.lib.chart.names.name" $rootContext | quote }}
      app.kubernetes.io/instance: {{ $rootContext.Release.Name | quote }}
      app.kubernetes.io/controller: {{ $controllerName | quote }}
  {{- /* hasKey + non-nil so explicit zero (the scaledjob chart default's
       maxUnavailable: 0 is exactly this case) survives, but a user-supplied
       `null` (e.g. `maxUnavailable: null` to clear the chart default before
       supplying a different field) drops the key from the rendered spec
       rather than emitting `maxUnavailable:` with no value. */ -}}
  {{- $hasMin := and (hasKey $pdb "minAvailable") (not (kindIs "invalid" (index $pdb "minAvailable"))) -}}
  {{- $hasMax := and (hasKey $pdb "maxUnavailable") (not (kindIs "invalid" (index $pdb "maxUnavailable"))) -}}
  {{- /* policy/v1 PodDisruptionBudget rejects spec with both fields set
       (apiserver: "minAvailable and maxUnavailable cannot be both set").
       The scaledjob chart default lands maxUnavailable: 0, so an operator
       override that adds minAvailable without explicitly nulling the
       default is a real footgun — fail at template time with a controller-
       named message instead of letting kubectl apply surface it. */ -}}
  {{- if and $hasMin $hasMax -}}
    {{- fail (printf "podDisruptionBudget for controller %q: minAvailable and maxUnavailable are mutually exclusive; set the unused field to null" $controllerName) -}}
  {{- end }}
  {{- if $hasMin }}
  minAvailable: {{ get $pdb "minAvailable" }}
  {{- end }}
  {{- if $hasMax }}
  maxUnavailable: {{ get $pdb "maxUnavailable" }}
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
"{prefix}-{token}" queues (queueTypes activity); the workflow token maps to
the prefix-only queue (queueTypes workflow). The metadata key names
(taskQueue, queueTypes, tlsServerName, unsafeSsl) and value casing
(lowercase) match the KEDA Temporal scaler's struct tags in
pkg/scalers/temporal_scaler.go — they are not chart-internal names. Caller
supplies a dict with the additional keys "temporalEndpoint",
"temporalNamespace", "taskQueuePrefix", "authRefName", "knownTokens",
"workersByName" (the chart-validated workers map keyed on controller name),
and "kedaTLS" (a dict with optional "serverName" and "unsafeSsl" entries
already filtered for whether keda.tls.enabled is true upstream) so the
helper can resolve each controller's tokens via
media-processor.resolveActivities and emit scaler-side TLS metadata
when configured. Per-controller keda.targetQueueSize /
keda.activationTargetQueueSize override the chart defaults (5 / 0).
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
{{- $kedaTLS := .kedaTLS | default dict -}}
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
  {{- $resolved := include "media-processor.resolveActivities" (dict
        "workerName" $name
        "tokens" $rawTokens
        "knownTokens" $knownTokens
      ) -}}
  {{- if not $resolved -}}
    {{- fail (printf "scaledjob controller %q resolved to an empty activity set" $name) -}}
  {{- end -}}
  {{- $resolvedTokens := splitList "," $resolved -}}

  {{- /* Default targetQueueSize / activationTargetQueueSize per trigger.
       Operator overrides land via resources.controllers.<name>.keda.*. */ -}}
  {{- $targetQueueSize := index $kedaCfg "targetQueueSize" | default 5 -}}
  {{- $activationTargetQueueSize := index $kedaCfg "activationTargetQueueSize" | default 0 -}}

  {{- /* Build the trigger list. Activity tokens emit activity-typed
       triggers on "{prefix}-{token}"; the workflow token emits a
       workflow-typed trigger on the prefix-only queue. Metadata keys
       (taskQueue, queueTypes) and lowercase values match the KEDA
       Temporal scaler struct tags in pkg/scalers/temporal_scaler.go —
       getQueueTypes() switches on lowercase "workflow"/"activity" only.
       Order matches knownTokens order (resolver already canonicalises). */ -}}
  {{- $triggers := list -}}
  {{- range $token := $resolvedTokens -}}
    {{- $taskQueue := "" -}}
    {{- $queueTypes := "" -}}
    {{- if eq $token "workflow" -}}
      {{- $taskQueue = $taskQueuePrefix -}}
      {{- $queueTypes = "workflow" -}}
    {{- else -}}
      {{- $taskQueue = printf "%s-%s" $taskQueuePrefix $token -}}
      {{- $queueTypes = "activity" -}}
    {{- end -}}
    {{- $metadata := dict
      "endpoint" $temporalEndpoint
      "namespace" $temporalNamespace
      "taskQueue" $taskQueue
      "queueTypes" $queueTypes
      "targetQueueSize" ($targetQueueSize | toString)
      "activationTargetQueueSize" ($activationTargetQueueSize | toString)
    -}}
    {{- /* Scaler-side TLS metadata. Only emitted when keda.tls.enabled is
         true and the operator supplied the corresponding override.
         tlsServerName overrides SNI / cert-verification hostname; unsafeSsl
         disables host verification. Both are per-trigger metadata fields
         in the Temporal scaler (not authParams), so they live alongside
         taskQueue / queueTypes rather than on the TriggerAuthentication. */ -}}
    {{- if $kedaTLS.serverName -}}
      {{- $_ := set $metadata "tlsServerName" $kedaTLS.serverName -}}
    {{- end -}}
    {{- if $kedaTLS.unsafeSsl -}}
      {{- $_ := set $metadata "unsafeSsl" "true" -}}
    {{- end -}}
    {{- $trigger := dict
      "type" "temporal"
      "metadata" $metadata
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
    {{ $k }}: {{ tpl $v $rootContext | quote }}
    {{- end }}
  {{- end }}
  {{- with $topAnnotations }}
  annotations:
    {{- range $k, $v := . }}
    {{ $k }}: {{ tpl $v $rootContext | quote }}
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
        endpoint: {{ $trigger.metadata.endpoint | quote }}
        namespace: {{ $trigger.metadata.namespace | quote }}
        taskQueue: {{ $trigger.metadata.taskQueue | quote }}
        queueTypes: {{ $trigger.metadata.queueTypes | quote }}
        targetQueueSize: {{ $trigger.metadata.targetQueueSize | quote }}
        activationTargetQueueSize: {{ $trigger.metadata.activationTargetQueueSize | quote }}
        {{- if hasKey $trigger.metadata "tlsServerName" }}
        tlsServerName: {{ $trigger.metadata.tlsServerName | quote }}
        {{- end }}
        {{- if hasKey $trigger.metadata "unsafeSsl" }}
        unsafeSsl: {{ $trigger.metadata.unsafeSsl | quote }}
        {{- end }}
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
