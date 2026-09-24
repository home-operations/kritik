{{- if not .Values.config.existingConfigMap }}
{{- if not .Values.config.file }}
{{- fail "config.file is required unless config.existingConfigMap names a ConfigMap with a config.yaml key" }}
{{- end }}
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "kritik.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritik.labels" . | nindent 4 }}
data:
  # NOT tpl'd: the file is kritik's own schema, validated at load with unknown
  # keys rejected; Helm passes it through as given.
  config.yaml: |
    {{- toYaml .Values.config.file | nindent 4 }}
{{- end }}
