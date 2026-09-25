{{- if include "kritik.hasIngest" . }}
# Webhook listener: only pods that serve hooks (`all` and `ingest`).
apiVersion: v1
kind: Service
metadata:
  name: {{ include "kritik.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritik.labels" . | nindent 4 }}
spec:
  type: {{ .Values.service.type }}
  ports:
    - name: http
      port: {{ .Values.service.port }}
      targetPort: http
      protocol: TCP
  selector:
    {{- include "kritik.selectorLabels" . | nindent 4 }}
    kritik.home-operations.com/hooks: "true"
{{- end }}
{{- if and (include "kritik.hasWorker" .) .Values.gateway.enabled }}
---
# Egress gateway: the forward proxy runner Jobs reach the outside through
# (ADR-0008), served by worker-capable pods.
apiVersion: v1
kind: Service
metadata:
  name: {{ include "kritik.fullname" . }}-gateway
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritik.labels" . | nindent 4 }}
    app.kubernetes.io/component: gateway
spec:
  type: ClusterIP
  ports:
    - name: gateway
      port: {{ .Values.gateway.port }}
      targetPort: gateway
      protocol: TCP
  selector:
    {{- include "kritik.selectorLabels" . | nindent 4 }}
    kritik.home-operations.com/gateway: "true"
{{- end }}
---
# Metrics: every role's pods.
apiVersion: v1
kind: Service
metadata:
  name: {{ include "kritik.fullname" . }}-metrics
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritik.labels" . | nindent 4 }}
    app.kubernetes.io/component: metrics
spec:
  type: ClusterIP
  ports:
    - name: metrics
      port: {{ .Values.service.metricsPort }}
      targetPort: metrics
      protocol: TCP
  selector:
    {{- include "kritik.selectorLabels" . | nindent 4 }}
