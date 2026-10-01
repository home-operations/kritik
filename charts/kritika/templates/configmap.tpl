{{- if and .Values.config (not .Values.existingConfigMap) }}
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "kritika.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritika.labels" . | nindent 4 }}
data:
  # NOT tpl'd: the file is kritika's own schema, validated at load with unknown
  # keys rejected; Helm passes it through as given.
  config.yaml: |
    {{- toYaml .Values.config | nindent 4 }}
{{- end }}
