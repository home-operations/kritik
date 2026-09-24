{{- if and .Values.embedding.apiKey (not .Values.embedding.existingSecret) }}
apiVersion: v1
kind: Secret
metadata:
  name: {{ include "kritik.fullname" . }}-embedding
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritik.labels" . | nindent 4 }}
type: Opaque
stringData:
  api-key: {{ tpl .Values.embedding.apiKey $ | quote }}
{{- end }}
