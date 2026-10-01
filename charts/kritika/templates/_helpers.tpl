{{/*
Expand the name of the chart.
*/}}
{{- define "kritika.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name (truncated to the 63-char DNS limit).
*/}}
{{- define "kritika.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Chart name and version as used by the chart label.
*/}}
{{- define "kritika.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "kritika.labels" -}}
helm.sh/chart: {{ include "kritika.chart" . }}
{{ include "kritika.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels of the server pods. Runner pods lack the instance label, so
these select the server alone.
*/}}
{{- define "kritika.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kritika.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Service account name to use.
*/}}
{{- define "kritika.serviceAccountName" -}}
{{- $name := tpl (.Values.serviceAccount.name | default "") $ -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kritika.fullname" .) $name }}
{{- else }}
{{- default "default" $name }}
{{- end }}
{{- end }}

{{/*
Runner service account name.
*/}}
{{- define "kritika.runnerServiceAccountName" -}}
{{- $name := tpl (.Values.runner.serviceAccount.name | default "") $ -}}
{{- if .Values.runner.serviceAccount.create }}
{{- default (printf "%s-runner" (include "kritika.fullname" .)) $name }}
{{- else }}
{{- default "default" $name }}
{{- end }}
{{- end }}

{{/*
Container image reference. A digest pins immutably and wins when set;
otherwise it's repository:tag, with tag defaulting to the chart appVersion.
*/}}
{{- define "kritika.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- end -}}
{{- end }}

{{/*
The helm test pod's curl image.
*/}}
{{- define "kritika.testImage" -}}
{{- $img := .Values.tests.image -}}
{{- printf "%s:%s" $img.repository $img.tag -}}
{{- end }}

{{/*
Runner image: the runner block's override, else the chart image.
*/}}
{{- define "kritika.runnerImage" -}}
{{- .Values.runner.image | default (include "kritika.image" .) -}}
{{- end }}

{{/*
ConfigMap the file is read from.
*/}}
{{- define "kritika.configMapName" -}}
{{- if .Values.existingConfigMap -}}
{{- tpl .Values.existingConfigMap $ -}}
{{- else -}}
{{- include "kritika.fullname" . -}}
{{- end -}}
{{- end }}

{{/*
In-cluster URL runner Jobs are handed as HTTPS_PROXY and their model endpoint.
*/}}
{{- define "kritika.gatewayURL" -}}
{{- printf "http://%s-gateway.%s.svc.cluster.local:%d" (include "kritika.fullname" .) .Release.Namespace (int .Values.service.gatewayPort) -}}
{{- end }}

{{/*
Whether a configuration file is mounted: the chart's, or an existing
ConfigMap. Without one, kritika runs on its environment alone.
*/}}
{{- define "kritika.hasConfigFile" -}}
{{- if or .Values.existingConfigMap .Values.config -}}true{{- end -}}
{{- end }}

{{/*
web.url's host, without a port, and its path without a trailing slash: the
one URL the Ingress or HTTPRoute serves, the dashboard at the path and the
webhook listener under it at /hooks.
*/}}
{{- define "kritika.webHost" -}}
{{- (urlParse (tpl .Values.web.url .)).host | splitList ":" | first -}}
{{- end }}

{{- define "kritika.webPath" -}}
{{- (urlParse (tpl .Values.web.url .)).path | trimSuffix "/" -}}
{{- end }}

{{/*
The variables the chart derives from its values: the kritika serve container's
environment before `env`. Rendered as a list so the deployment can also refuse
an `env` key that would duplicate one of them.
*/}}
{{- define "kritika.serveEnv" -}}
{{- if include "kritika.hasConfigFile" . }}
- name: KRITIKA_CONFIG_FILE
  value: /etc/kritika/config.yaml
{{- end }}
- name: KRITIKA_WEB_URL
  value: {{ tpl .Values.web.url . | quote }}
- name: KRITIKA_LOG_LEVEL
  value: {{ tpl .Values.logging.level . | quote }}
- name: KRITIKA_LOG_FORMAT
  value: {{ tpl .Values.logging.format . | quote }}
- name: KRITIKA_ADDR
  value: {{ printf ":%d" (int .Values.service.port) | quote }}
- name: KRITIKA_METRICS_ADDR
  value: {{ printf ":%d" (int .Values.service.metricsPort) | quote }}
- name: KRITIKA_GATEWAY_ADDR
  value: {{ printf ":%d" (int .Values.service.gatewayPort) | quote }}
- name: KRITIKA_GATEWAY_URL
  value: {{ include "kritika.gatewayURL" . | quote }}
- name: KRITIKA_DATABASE_APP_ROLE
  value: {{ .Values.database.app.role | quote }}
- name: KRITIKA_DATABASE_RUNNER_ROLE
  value: {{ .Values.database.runner.role | quote }}
- name: KRITIKA_DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ tpl .Values.database.app.existingSecret . | quote }}
      key: {{ .Values.database.app.key | quote }}
- name: KRITIKA_DATABASE_OWNER_URL
  valueFrom:
    secretKeyRef:
      name: {{ tpl .Values.database.owner.existingSecret . | quote }}
      key: {{ .Values.database.owner.key | quote }}
- name: KRITIKA_EXECUTOR
  value: kubernetes
- name: KRITIKA_RUNNER_IMAGE
  value: {{ include "kritika.runnerImage" . | quote }}
- name: KRITIKA_RUNNER_SERVICE_ACCOUNT
  value: {{ include "kritika.runnerServiceAccountName" . | quote }}
- name: KRITIKA_RUNNER_DATABASE_SECRET
  value: {{ tpl .Values.database.runner.existingSecret . | quote }}
- name: KRITIKA_RUNNER_DATABASE_SECRET_KEY
  value: {{ .Values.database.runner.key | quote }}
- name: KRITIKA_RUNNER_TTL
  value: {{ tpl (toString .Values.runner.ttl) . | quote }}
{{- with .Values.runner.resources }}
- name: KRITIKA_RUNNER_RESOURCES
  value: {{ toJson . | quote }}
{{- end }}
{{- with .Values.runner.tools }}
- name: KRITIKA_RUNNER_TOOLS
  value: {{ toJson . | quote }}
{{- end }}
{{- end }}
