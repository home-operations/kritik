{{- if .Values.monitoring.serviceMonitor.enabled -}}
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: {{ include "kritik.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritik.labels" . | nindent 4 }}
    {{- with .Values.monitoring.serviceMonitor.labels }}
    {{- tpl (toYaml .) $ | nindent 4 }}
    {{- end }}
  {{- with .Values.monitoring.serviceMonitor.annotations }}
  annotations:
    {{- tpl (toYaml .) $ | nindent 4 }}
  {{- end }}
spec:
  selector:
    matchLabels:
      {{- include "kritik.selectorLabels" . | nindent 6 }}
      app.kubernetes.io/component: metrics
  endpoints:
    - port: metrics
      interval: {{ .Values.monitoring.serviceMonitor.interval | default "30s" }}
      scrapeTimeout: {{ .Values.monitoring.serviceMonitor.scrapeTimeout | default "10s" }}
      path: /metrics
      {{- with .Values.monitoring.serviceMonitor.metricRelabelings }}
      metricRelabelings:
        {{- tpl (toYaml .) $ | nindent 8 }}
      {{- end }}
      {{- with .Values.monitoring.serviceMonitor.relabelings }}
      relabelings:
        {{- tpl (toYaml .) $ | nindent 8 }}
      {{- end }}
{{- end }}
