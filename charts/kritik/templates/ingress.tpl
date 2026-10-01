{{- if .Values.ingress.enabled -}}
{{- $base := include "kritik.webPath" . -}}
# One Ingress for web.url, to the public listener: the webhooks under /hooks
# and the dashboard everywhere else.
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
          - path: {{ $base | default "/" | quote }}
            pathType: Prefix
            backend:
              service:
                name: {{ include "kritik.fullname" . }}
                port:
                  name: http
{{- end }}
