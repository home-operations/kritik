{{- if .Values.serviceAccount.create }}
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ include "kritika.serviceAccountName" . | quote }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritika.labels" . | nindent 4 }}
  {{- with .Values.serviceAccount.annotations }}
  annotations:
    {{- tpl (toYaml .) $ | nindent 4 }}
  {{- end }}
automountServiceAccountToken: {{ .Values.serviceAccount.automount }}
{{- end }}
{{- if .Values.runner.serviceAccount.create }}
---
# Runner pods run as this account; it grants nothing and mounts no token.
# The executor builds runner Jobs without pull secrets, so the account
# carries the chart's: the kubelet adds a ServiceAccount's to every pod
# that runs as it, whether or not a token is mounted.
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ include "kritika.runnerServiceAccountName" . | quote }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritika.labels" . | nindent 4 }}
    app.kubernetes.io/component: runner
  {{- with .Values.runner.serviceAccount.annotations }}
  annotations:
    {{- tpl (toYaml .) $ | nindent 4 }}
  {{- end }}
automountServiceAccountToken: false
{{- with .Values.imagePullSecrets }}
imagePullSecrets:
  {{- tpl (toYaml .) $ | nindent 2 }}
{{- end }}
{{- end }}
