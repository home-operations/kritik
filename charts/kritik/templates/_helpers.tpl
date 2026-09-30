{{/*
Expand the name of the chart.
*/}}
{{- define "kritik.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name (truncated to the 63-char DNS limit).
*/}}
{{- define "kritik.fullname" -}}
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
{{- define "kritik.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "kritik.labels" -}}
helm.sh/chart: {{ include "kritik.chart" . }}
{{ include "kritik.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels of the server pods. Runner pods lack the instance label, so
these select the server alone.
*/}}
{{- define "kritik.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kritik.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Service account name to use.
*/}}
{{- define "kritik.serviceAccountName" -}}
{{- $name := tpl (.Values.serviceAccount.name | default "") $ -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kritik.fullname" .) $name }}
{{- else }}
{{- default "default" $name }}
{{- end }}
{{- end }}

{{/*
Runner service account name.
*/}}
{{- define "kritik.runnerServiceAccountName" -}}
{{- $name := tpl (.Values.runner.serviceAccount.name | default "") $ -}}
{{- if .Values.runner.serviceAccount.create }}
{{- default (printf "%s-runner" (include "kritik.fullname" .)) $name }}
{{- else }}
{{- default "default" $name }}
{{- end }}
{{- end }}

{{/*
Container image reference. A digest pins immutably and wins when set;
otherwise it's repository:tag, with tag defaulting to the chart appVersion.
*/}}
{{- define "kritik.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- end -}}
{{- end }}

{{/*
The helm test pod's curl image.
*/}}
{{- define "kritik.testImage" -}}
{{- $img := .Values.tests.image -}}
{{- printf "%s:%s" $img.repository $img.tag -}}
{{- end }}

{{/*
Runner image: the runner block's override, else the chart image.
*/}}
{{- define "kritik.runnerImage" -}}
{{- .Values.runner.image | default (include "kritik.image" .) -}}
{{- end }}

{{/*
ConfigMap the file is read from.
*/}}
{{- define "kritik.configMapName" -}}
{{- if .Values.config.existingConfigMap -}}
{{- tpl .Values.config.existingConfigMap $ -}}
{{- else -}}
{{- include "kritik.fullname" . -}}
{{- end -}}
{{- end }}

{{/*
In-cluster URL runner Jobs are handed as HTTPS_PROXY, empty when the gateway
is off.
*/}}
{{- define "kritik.gatewayURL" -}}
{{- if .Values.gateway.enabled -}}
{{- printf "http://%s-gateway.%s.svc.cluster.local:%d" (include "kritik.fullname" .) .Release.Namespace (int .Values.gateway.port) -}}
{{- end -}}
{{- end }}

{{/*
Whether a configuration file is mounted: the chart's, or an existing
ConfigMap. Without one, kritik runs on its environment alone.
*/}}
{{- define "kritik.hasConfigFile" -}}
{{- if or .Values.config.existingConfigMap .Values.config.file -}}true{{- end -}}
{{- end }}

{{/*
web.url's host, without a port, and its path without a trailing slash: the
one URL the Ingress or HTTPRoute serves, the dashboard at the path and the
webhook listener under it at /hooks.
*/}}
{{- define "kritik.webHost" -}}
{{- (urlParse (tpl .Values.web.url .)).host | splitList ":" | first -}}
{{- end }}

{{- define "kritik.webPath" -}}
{{- (urlParse (tpl .Values.web.url .)).path | trimSuffix "/" -}}
{{- end }}

{{/*
The auth values as KRITIK_AUTH_* variables, each only when set so a value
the configuration file gives is not overridden with an empty one.
*/}}
{{- define "kritik.authEnv" -}}
{{- $a := .Values.auth -}}
{{- $plain := list
  (list "KRITIK_AUTH_SESSION_TTL" $a.sessionTTL)
  (list "KRITIK_AUTH_ADMIN_USER" $a.admin.user)
  (list "KRITIK_AUTH_OIDC_NAME" $a.oidc.name)
  (list "KRITIK_AUTH_OIDC_ISSUER" $a.oidc.issuer)
  (list "KRITIK_AUTH_OIDC_CLIENT_ID" $a.oidc.clientId)
  (list "KRITIK_AUTH_OIDC_SCOPES" (join "," $a.oidc.scopes))
  (list "KRITIK_AUTH_OIDC_ROLES_CLAIM" $a.oidc.rolesClaim)
  (list "KRITIK_AUTH_OIDC_ROLE_MAPPING_EXPR" $a.oidc.roleMappingExpr)
  (list "KRITIK_AUTH_OIDC_DEFAULT_ROLE" $a.oidc.defaultRole)
  (list "KRITIK_AUTH_GITHUB_CLIENT_ID" $a.github.clientId)
  (list "KRITIK_AUTH_GITHUB_ROLE_MAPPING_EXPR" $a.github.roleMappingExpr)
-}}
{{- range $plain }}
{{- if index . 1 }}
- name: {{ index . 0 }}
  value: {{ tpl (toString (index . 1)) $ | quote }}
{{- end }}
{{- end }}
{{- $secrets := list
  (list "KRITIK_AUTH_ADMIN_PASSWORD" $a.admin.passwordSecret)
  (list "KRITIK_AUTH_OIDC_CLIENT_SECRET" $a.oidc.clientSecretSecret)
  (list "KRITIK_AUTH_GITHUB_CLIENT_SECRET" $a.github.clientSecretSecret)
-}}
{{- range $secrets }}
{{- $ref := index . 1 }}
{{- if $ref.name }}
- name: {{ index . 0 }}
  valueFrom:
    secretKeyRef:
      name: {{ tpl $ref.name $ | quote }}
      key: {{ $ref.key | quote }}
{{- end }}
{{- end }}
{{- end }}
