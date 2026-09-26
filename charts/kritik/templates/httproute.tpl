{{- if and .Values.httpRoute.enabled (include "kritik.hasIngest" .) -}}
{{- $route := .Values.httpRoute -}}
apiVersion: {{ $route.apiVersion | default "gateway.networking.k8s.io/v1" }}
kind: HTTPRoute
metadata:
  name: {{ include "kritik.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritik.labels" . | nindent 4 }}
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
  {{- with $route.hostnames }}
  hostnames:
    {{- tpl (toYaml .) $ | nindent 4 }}
  {{- end }}
  rules:
    - backendRefs:
        - name: {{ include "kritik.fullname" . }}
          port: {{ .Values.service.port }}
      {{- with $route.matches }}
      matches:
        {{- tpl (toYaml .) $ | nindent 8 }}
      {{- end }}
{{- end }}
{{- if and .Values.httpRoute.web.enabled (include "kritik.hasWeb" .) }}
---
{{- $route := .Values.httpRoute.web -}}
apiVersion: {{ $route.apiVersion | default "gateway.networking.k8s.io/v1" }}
kind: HTTPRoute
metadata:
  name: {{ include "kritik.fullname" . }}-web
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritik.labels" . | nindent 4 }}
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
  {{- with $route.hostnames }}
  hostnames:
    {{- tpl (toYaml .) $ | nindent 4 }}
  {{- end }}
  rules:
    - backendRefs:
        - name: {{ include "kritik.fullname" . }}-web
          port: {{ .Values.service.webPort }}
      {{- with $route.matches }}
      matches:
        {{- tpl (toYaml .) $ | nindent 8 }}
      {{- end }}
{{- end }}
