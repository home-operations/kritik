{{- if and .Values.ingress.enabled (include "kritik.hasIngest" .) -}}
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
    {{- range .Values.ingress.hosts }}
    - host: {{ tpl .host $ | quote }}
      http:
        paths:
          {{- range .paths }}
          - path: {{ .path }}
            pathType: {{ .pathType }}
            backend:
              service:
                name: {{ include "kritik.fullname" $ }}
                port:
                  name: http
          {{- end }}
    {{- end }}
{{- end }}
{{- if and .Values.ingress.web.enabled (include "kritik.hasWeb" .) }}
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: {{ include "kritik.fullname" . }}-web
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritik.labels" . | nindent 4 }}
  {{- with .Values.ingress.web.annotations }}
  annotations:
    {{- tpl (toYaml .) $ | nindent 4 }}
  {{- end }}
spec:
  {{- with .Values.ingress.web.className }}
  ingressClassName: {{ tpl . $ | quote }}
  {{- end }}
  {{- with .Values.ingress.web.tls }}
  tls:
    {{- tpl (toYaml .) $ | nindent 4 }}
  {{- end }}
  rules:
    {{- range .Values.ingress.web.hosts }}
    - host: {{ tpl .host $ | quote }}
      http:
        paths:
          {{- range .paths }}
          - path: {{ .path }}
            pathType: {{ .pathType }}
            backend:
              service:
                name: {{ include "kritik.fullname" $ }}-web
                port:
                  name: web
          {{- end }}
    {{- end }}
{{- end }}
