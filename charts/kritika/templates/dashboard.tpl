{{- if .Values.monitoring.dashboards.enabled }}
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "kritika.fullname" . }}-dashboard
  namespace: {{ .Values.monitoring.dashboards.namespace | default .Release.Namespace }}
  {{- with .Values.monitoring.dashboards.annotations }}
  annotations:
    {{- tpl (toYaml .) $ | nindent 4 }}
  {{- end }}
  labels:
    {{- include "kritika.labels" . | nindent 4 }}
    {{- if not .Values.monitoring.dashboards.grafanaOperator.enabled }}
    grafana_dashboard: "1"
    {{- end }}
    {{- with .Values.monitoring.dashboards.labels }}
    {{- tpl (toYaml .) $ | nindent 4 }}
    {{- end }}
data:
  kritika.json: |
{{ .Files.Get "dashboards/kritika.json" | nindent 4 }}
{{- end }}
