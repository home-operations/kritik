{{- if .Values.httpRoute.enabled -}}
{{- $route := .Values.httpRoute -}}
{{- $base := include "kritika.webPath" . -}}
# One HTTPRoute for web.url, to the public listener: the webhooks under
# /hooks and the dashboard everywhere else.
apiVersion: {{ $route.apiVersion | default "gateway.networking.k8s.io/v1" }}
kind: HTTPRoute
metadata:
  name: {{ include "kritika.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritika.labels" . | nindent 4 }}
    {{- with $route.labels }}
    {{- tpl (toYaml .) $ | nindent 4 }}
    {{- end }}
  {{- with $route.annotations }}
  annotations:
    {{- tpl (toYaml .) $ | nindent 4 }}
  {{- end }}
spec:
  {{- with $route.parentRefs }}
  parentRefs:
    {{- tpl (toYaml .) $ | nindent 4 }}
  {{- end }}
  hostnames:
    - {{ include "kritika.webHost" . | quote }}
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: {{ $base | default "/" | quote }}
      backendRefs:
        - name: {{ include "kritika.fullname" . }}
          port: {{ .Values.service.port }}
{{- end }}
