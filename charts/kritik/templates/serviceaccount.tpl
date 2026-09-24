{{- if .Values.serviceAccount.create }}
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ include "kritik.serviceAccountName" . | quote }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritik.labels" . | nindent 4 }}
  {{- with .Values.serviceAccount.annotations }}
  annotations:
    {{- tpl (toYaml .) $ | nindent 4 }}
  {{- end }}
automountServiceAccountToken: {{ .Values.serviceAccount.automount }}
{{- end }}
{{- if and (include "kritik.hasWorker" .) .Values.runner.serviceAccount.create }}
---
# Runner pods run as this account; it grants nothing and mounts no token.
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ include "kritik.runnerServiceAccountName" . | quote }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritik.labels" . | nindent 4 }}
    app.kubernetes.io/component: runner
  {{- with .Values.runner.serviceAccount.annotations }}
  annotations:
    {{- tpl (toYaml .) $ | nindent 4 }}
  {{- end }}
automountServiceAccountToken: false
{{- end }}
