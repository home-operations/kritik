{{- if .Values.ingress.enabled -}}
{{- $base := include "kritik.webPath" . -}}
# One Ingress for web.url: the webhook listener under /hooks, the dashboard
# everywhere else (ADR-0014 §2.1).
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: {{ include "kritik.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritik.labels" . | nindent 4 }}
  {{- with .Values.ingress.annotations }}
  annotations:
    {{- tpl (toYaml .) $ | nindent 4 }}
  {{- end }}
spec:
  {{- with .Values.ingress.className }}
  ingressClassName: {{ tpl . $ | quote }}
  {{- end }}
  {{- with .Values.ingress.tls }}
  tls:
    {{- tpl (toYaml .) $ | nindent 4 }}
  {{- end }}
  rules:
    - host: {{ include "kritik.webHost" . | quote }}
      http:
        paths:
          {{- if include "kritik.hasIngest" . }}
          - path: {{ printf "%s/hooks" $base | quote }}
            pathType: Prefix
            backend:
              service:
                name: {{ include "kritik.fullname" . }}
                port:
                  name: http
          {{- end }}
          - path: {{ $base | default "/" | quote }}
            pathType: Prefix
            backend:
              service:
                name: {{ include "kritik.fullname" . }}-web
                port:
                  name: web
{{- end }}
