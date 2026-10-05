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
