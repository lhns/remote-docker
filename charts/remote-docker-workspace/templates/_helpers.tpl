{{/*
Standard Helm helpers.
*/}}

{{- define "remote-docker-workspace.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "remote-docker-workspace.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "remote-docker-workspace.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "remote-docker-workspace.labels" -}}
helm.sh/chart: {{ include "remote-docker-workspace.chart" . }}
{{ include "remote-docker-workspace.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: workspace
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
A volumeClaimTemplate is immutable, labels included, so these carry nothing
that moves: no chart version, no appVersion, no commonLabels. One that moves
makes the next upgrade forbidden until the StatefulSet is orphan-deleted.
*/}}
{{- define "remote-docker-workspace.volumeClaimLabels" -}}
{{ include "remote-docker-workspace.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: workspace
{{- end -}}

{{- define "remote-docker-workspace.selectorLabels" -}}
app.kubernetes.io/name: {{ include "remote-docker-workspace.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
The headless Service, which is the StatefulSet's serviceName and gives each pod
its own DNS name. Truncated first so the suffix survives a long release name.
*/}}
{{- define "remote-docker-workspace.headlessName" -}}
{{- printf "%s-headless" (include "remote-docker-workspace.fullname" . | trunc 54 | trimSuffix "-") -}}
{{- end -}}

{{/*
The address an enrolment invite names: publicURL; with several replicas, the
pod's own name, since a token is redeemable only where it was minted and
nothing in front of the pods can name one; else the ingress's. $(POD_NAME) is
expanded by the kubelet from the variable rendered before it.
*/}}
{{- define "remote-docker-workspace.publicURL" -}}
{{- if .Values.publicURL -}}
{{- .Values.publicURL -}}
{{- else if gt (int .Values.replicas) 1 -}}
ssh://$(POD_NAME).{{ include "remote-docker-workspace.headlessName" . }}.{{ .Release.Namespace }}.svc:2222
{{- else if and .Values.ingress.enabled .Values.ingress.host -}}
{{- ternary "wss" "ws" .Values.ingress.tls.enabled }}://{{ .Values.ingress.host }}/
{{- end -}}
{{- end -}}

{{/*
Refusals that would otherwise surface as a broken render, or as an upgrade the
API server forbids with a message naming no remedy.
*/}}
{{- define "remote-docker-workspace.checks" -}}
{{- /* --reuse-values renders with the OLD chart's defaults, so values this
      chart added are missing rather than defaulted. */ -}}
{{- if not (and (hasKey .Values "replicas") (hasKey .Values "podAntiAffinity") (hasKey .Values.service "sessionAffinityTimeout")) -}}
{{- fail "replicas, podAntiAffinity or service.sessionAffinityTimeout is missing, which is what --reuse-values does across a chart that added them\n  fix: upgrade with --reset-then-reuse-values instead" -}}
{{- end -}}
{{- if not (has (toString .Values.podAntiAffinity) (list "hard" "soft" "none")) -}}
{{- fail (printf "podAntiAffinity: %q is not hard, soft or none" (toString .Values.podAntiAffinity)) -}}
{{- end -}}
{{- /* serviceName is immutable. An empty lookup (helm template, a first
      install) passes. */ -}}
{{- $sts := include "remote-docker-workspace.fullname" . -}}
{{- with lookup "apps/v1" "StatefulSet" .Release.Namespace $sts -}}
{{- $have := .spec.serviceName | default "" -}}
{{- $want := include "remote-docker-workspace.headlessName" $ -}}
{{- if and $have (ne $have $want) -}}
{{- fail (printf "StatefulSet %s has serviceName %s, which cannot change to %s in place\n  fix: kubectl delete sts %s -n %s --cascade=orphan, then upgrade again; the pod and its volumes are kept" $sts $have $want $sts $.Release.Namespace) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
podAntiAffinity on the node, matched on the selector labels. Empty for none.
*/}}
{{- define "remote-docker-workspace.antiAffinity" -}}
{{- $term := dict "topologyKey" "kubernetes.io/hostname" "labelSelector" (dict "matchLabels" (include "remote-docker-workspace.selectorLabels" . | fromYaml)) -}}
{{- if eq .Values.podAntiAffinity "hard" -}}
podAntiAffinity:
  requiredDuringSchedulingIgnoredDuringExecution:
    {{- list $term | toYaml | nindent 4 }}
{{- else if eq .Values.podAntiAffinity "soft" -}}
podAntiAffinity:
  preferredDuringSchedulingIgnoredDuringExecution:
    - weight: 100
      podAffinityTerm:
        {{- $term | toYaml | nindent 8 }}
{{- end -}}
{{- end -}}

{{- define "remote-docker-workspace.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "remote-docker-workspace.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "remote-docker-workspace.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/*
The ephemeral accounts' environment (ADR 0050), refused here rather than by an
agent that will not start. Empty while no account is listed; a variable `env`
names is left to it.
*/}}
{{- define "remote-docker-workspace.ephemeralEnv" -}}
{{- $e := .Values.ephemeral -}}
{{- if $e.accounts -}}
{{- $grace := toString $e.grace -}}
{{- if not (and (regexMatch "^([0-9]*[.]?[0-9]+(ns|us|µs|ms|s|m|h))+$" $grace) (regexMatch "[1-9]" $grace)) -}}
{{- fail (printf "ephemeral.grace: %q is not a positive duration, such as 2m" $grace) -}}
{{- end -}}
{{- $max := toString $e.maxClients -}}
{{- if or (not (regexMatch "^[0-9]+$" $max)) (lt (atoi $max) 1) -}}
{{- fail (printf "ephemeral.maxClients: %q is not a count of at least 1" $max) -}}
{{- end -}}
{{- if not .Values.existingSecret -}}
{{- range $e.accounts -}}
{{- if not (get $.Values.authorizedKeys (toString .)) -}}
{{- fail (printf "ephemeral.accounts: %q has no key in authorizedKeys; add one, or set existingSecret" (toString .)) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $vars := dict
      "WORKSPACE_EPHEMERAL_ACCOUNTS" (join "," $e.accounts)
      "WORKSPACE_EPHEMERAL_MAX_CLIENTS" $max
      "WORKSPACE_EPHEMERAL_GRACE" $grace
      "WORKSPACE_EPHEMERAL_CLEANUP_CONTAINERS" (toString $e.cleanupContainers) -}}
{{- range $name, $value := $vars }}
{{- if not (hasKey $.Values.env $name) }}
- name: {{ $name }}
  value: {{ $value | quote }}
{{- end }}
{{- end }}
{{- end -}}
{{- end -}}

{{/*
The Secret holding authorized keys: the one this chart renders, or the one the
operator manages.
*/}}
{{- define "remote-docker-workspace.keysSecretName" -}}
{{- if .Values.existingSecret -}}
{{- .Values.existingSecret -}}
{{- else -}}
{{- printf "%s-keys" (include "remote-docker-workspace.fullname" .) -}}
{{- end -}}
{{- end -}}
