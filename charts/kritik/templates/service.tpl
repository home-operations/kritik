# The public listener: the webhooks and the dashboard.
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
---
# The gateway: the forward proxy runner Jobs reach the outside through, and
# their model and similar-code endpoints.
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
---
# Metrics.
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
