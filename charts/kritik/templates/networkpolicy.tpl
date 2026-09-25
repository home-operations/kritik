{{- if .Values.networkPolicy.enabled }}
{{- $np := .Values.networkPolicy }}
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ include "kritik.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritik.labels" . | nindent 4 }}
spec:
  podSelector:
    matchLabels:
      {{- include "kritik.selectorLabels" . | nindent 6 }}
  policyTypes:
    - Ingress
    - Egress
  ingress:
    # No `from`: the webhook and metrics ports are reachable by any peer;
    # lock down per cluster with your own policy if needed.
    - ports:
        - port: {{ .Values.service.port }}
          protocol: TCP
        - port: {{ .Values.service.metricsPort }}
          protocol: TCP
        {{- if include "kritik.hasWeb" . }}
        - port: {{ .Values.service.webPort }}
          protocol: TCP
        {{- end }}
    {{- if .Values.gateway.enabled }}
    # The gateway is for runner pods alone.
    - from:
        - podSelector:
            matchLabels:
              kritik.home-operations.com/role: runner
      ports:
        - port: {{ .Values.gateway.port }}
          protocol: TCP
    {{- end }}
  egress:
    {{- if $np.allowDNS }}
    - ports:
        - port: 53
          protocol: UDP
        - port: 53
          protocol: TCP
    {{- end }}
    - ports:
        {{- range $np.egressPorts }}
        - port: {{ . }}
          protocol: TCP
        {{- end }}
        - port: {{ $np.postgresPort }}
          protocol: TCP
    {{- if include "kritik.hasWorker" . }}
    # The worker talks to the API server to create and watch runner Jobs.
    - ports:
        - port: 443
          protocol: TCP
        - port: 6443
          protocol: TCP
    {{- end }}
{{- if include "kritik.hasWorker" . }}
---
# Runner pods: no ingress at all; egress to DNS, Postgres and, with the
# gateway, the gateway port on worker-capable pods, through which the git
# remote, the model endpoint and every allowed host are reached. Without
# the gateway, the egressPorts to anywhere, as before.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ include "kritik.fullname" . }}-runner
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritik.labels" . | nindent 4 }}
    app.kubernetes.io/component: runner
spec:
  podSelector:
    matchLabels:
      kritik.home-operations.com/role: runner
  policyTypes:
    - Ingress
    - Egress
  egress:
    {{- if $np.allowDNS }}
    - ports:
        - port: 53
          protocol: UDP
        - port: 53
          protocol: TCP
    {{- end }}
    - ports:
        {{- if not .Values.gateway.enabled }}
        {{- range $np.egressPorts }}
        - port: {{ . }}
          protocol: TCP
        {{- end }}
        {{- end }}
        - port: {{ $np.postgresPort }}
          protocol: TCP
    {{- if .Values.gateway.enabled }}
    - to:
        - podSelector:
            matchLabels:
              kritik.home-operations.com/gateway: "true"
      ports:
        - port: {{ .Values.gateway.port }}
          protocol: TCP
    {{- end }}
{{- end }}
{{- end }}
