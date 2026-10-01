{{- if .Values.networkPolicy.enabled }}
{{- $np := .Values.networkPolicy }}
{{- $public := int .Values.service.port }}
{{- $metrics := int .Values.service.metricsPort }}
{{- $gateway := int .Values.gateway.port }}
{{- $postgres := int $np.postgresPort }}
{{- /* Runner pods reach the outside through the gateway alone. */}}
{{- $runnerPorts := list $postgres }}
{{- $serverPorts := concat $np.egressPorts (list $postgres) }}
{{- $serverSelector := printf "app.kubernetes.io/name == '%s' && app.kubernetes.io/instance == '%s'" (include "kritika.name" .) .Release.Name }}
{{- if eq $np.type "default" }}
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ include "kritika.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritika.labels" . | nindent 4 }}
spec:
  podSelector:
    matchLabels:
      {{- include "kritika.selectorLabels" . | nindent 6 }}
  policyTypes:
    - Ingress
    - Egress
  ingress:
    # No `from`: the public and metrics ports are reachable by any peer;
    # lock down per cluster with your own policy if needed.
    - ports:
        - port: {{ $public }}
          protocol: TCP
        - port: {{ $metrics }}
          protocol: TCP
    # The gateway is for runner pods alone.
    - from:
        - podSelector:
            matchLabels:
              kritika.home-operations.com/role: runner
      ports:
        - port: {{ $gateway }}
          protocol: TCP
  egress:
    {{- if $np.allowDNS }}
    - ports:
        - port: 53
          protocol: UDP
        - port: 53
          protocol: TCP
    {{- end }}
    - ports:
        {{- range $serverPorts }}
        - port: {{ . }}
          protocol: TCP
        {{- end }}
    # The API server, to create and watch runner Jobs.
    - ports:
        - port: 443
          protocol: TCP
        - port: 6443
          protocol: TCP
---
# Runner pods: no ingress at all; egress to DNS, Postgres and the gateway
# port on the server pods, through which the git remote, the model and
# similar-code endpoints and every allowed host are reached.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ include "kritika.fullname" . }}-runner
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritika.labels" . | nindent 4 }}
    app.kubernetes.io/component: runner
spec:
  podSelector:
    matchLabels:
      kritika.home-operations.com/role: runner
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
        {{- range $runnerPorts }}
        - port: {{ . }}
          protocol: TCP
        {{- end }}
    - to:
        - podSelector:
            matchLabels:
              {{- include "kritika.selectorLabels" . | nindent 14 }}
      ports:
        - port: {{ $gateway }}
          protocol: TCP
{{- else if eq $np.type "cilium" }}
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: {{ include "kritika.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritika.labels" . | nindent 4 }}
spec:
  endpointSelector:
    matchLabels:
      {{- include "kritika.selectorLabels" . | nindent 6 }}
  ingress:
    - fromEntities:
        - all
      toPorts:
        - ports:
            - port: {{ $public | quote }}
              protocol: TCP
            - port: {{ $metrics | quote }}
              protocol: TCP
    - fromEndpoints:
        - matchLabels:
            kritika.home-operations.com/role: runner
      toPorts:
        - ports:
            - port: {{ $gateway | quote }}
              protocol: TCP
  egress:
    {{- if $np.allowDNS }}
    - toEndpoints:
        - matchLabels:
            k8s:io.kubernetes.pod.namespace: kube-system
            k8s-app: kube-dns
      toPorts:
        - ports:
            - port: "53"
              protocol: UDP
            - port: "53"
              protocol: TCP
    {{- end }}
    - toEntities:
        - cluster
        - world
      toPorts:
        - ports:
            {{- range $serverPorts }}
            - port: {{ . | quote }}
              protocol: TCP
            {{- end }}
    - toEntities:
        - kube-apiserver
      toPorts:
        - ports:
            - port: "443"
              protocol: TCP
            - port: "6443"
              protocol: TCP
---
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: {{ include "kritika.fullname" . }}-runner
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritika.labels" . | nindent 4 }}
    app.kubernetes.io/component: runner
spec:
  endpointSelector:
    matchLabels:
      kritika.home-operations.com/role: runner
  # An ingress section with one empty rule puts the pods into default deny
  # for ingress without allowing any peer.
  ingress:
    - {}
  egress:
    {{- if $np.allowDNS }}
    - toEndpoints:
        - matchLabels:
            k8s:io.kubernetes.pod.namespace: kube-system
            k8s-app: kube-dns
      toPorts:
        - ports:
            - port: "53"
              protocol: UDP
            - port: "53"
              protocol: TCP
    {{- end }}
    - toEntities:
        - cluster
        - world
      toPorts:
        - ports:
            {{- range $runnerPorts }}
            - port: {{ . | quote }}
              protocol: TCP
            {{- end }}
    - toEndpoints:
        - matchLabels:
            {{- include "kritika.selectorLabels" . | nindent 12 }}
      toPorts:
        - ports:
            - port: {{ $gateway | quote }}
              protocol: TCP
{{- else if eq $np.type "calico" }}
apiVersion: projectcalico.org/v3
kind: NetworkPolicy
metadata:
  name: {{ include "kritika.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritika.labels" . | nindent 4 }}
spec:
  selector: {{ $serverSelector }}
  types:
    - Ingress
    - Egress
  ingress:
    - action: Allow
      protocol: TCP
      destination:
        ports:
          - {{ $public }}
          - {{ $metrics }}
    - action: Allow
      protocol: TCP
      source:
        selector: kritika.home-operations.com/role == 'runner'
      destination:
        ports:
          - {{ $gateway }}
  egress:
    {{- if $np.allowDNS }}
    - action: Allow
      protocol: UDP
      destination:
        ports:
          - 53
    - action: Allow
      protocol: TCP
      destination:
        ports:
          - 53
    {{- end }}
    - action: Allow
      protocol: TCP
      destination:
        ports:
          {{- range $serverPorts }}
          - {{ . }}
          {{- end }}
    - action: Allow
      protocol: TCP
      destination:
        ports:
          - 443
          - 6443
---
apiVersion: projectcalico.org/v3
kind: NetworkPolicy
metadata:
  name: {{ include "kritika.fullname" . }}-runner
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritika.labels" . | nindent 4 }}
    app.kubernetes.io/component: runner
spec:
  selector: kritika.home-operations.com/role == 'runner'
  # Ingress in types with no ingress rules: nothing may reach a runner pod.
  types:
    - Ingress
    - Egress
  egress:
    {{- if $np.allowDNS }}
    - action: Allow
      protocol: UDP
      destination:
        ports:
          - 53
    - action: Allow
      protocol: TCP
      destination:
        ports:
          - 53
    {{- end }}
    - action: Allow
      protocol: TCP
      destination:
        ports:
          {{- range $runnerPorts }}
          - {{ . }}
          {{- end }}
    - action: Allow
      protocol: TCP
      destination:
        selector: {{ $serverSelector | quote }}
        ports:
          - {{ $gateway }}
{{- else }}
{{- fail (printf "networkPolicy.type must be one of: default, cilium, calico (got %q)" $np.type) }}
{{- end }}
{{- end }}
