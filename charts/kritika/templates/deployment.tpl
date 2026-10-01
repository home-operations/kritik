{{- if not .Values.database.app.existingSecret -}}
{{- fail "database.app.existingSecret is required: the Secret holding the application role's credentials" -}}
{{- end -}}
{{- if not .Values.database.owner.existingSecret -}}
{{- fail "database.owner.existingSecret is required: the leader runs migrations with it" -}}
{{- end -}}
{{- if not .Values.database.runner.existingSecret -}}
{{- fail "database.runner.existingSecret is required: runner Jobs connect with it" -}}
{{- end -}}
{{- if and (not .Values.database.host) (or (not .Values.database.app.uriKey) (not .Values.database.owner.uriKey) (not .Values.database.runner.uriKey)) -}}
{{- fail "database.host is required: a role whose Secret holds a username and password connects to it; or set every role's uriKey" -}}
{{- end -}}
{{- if not .Values.web.url -}}
{{- fail "web.url is required: the dashboard's public URL, which the webhook listener shares under /hooks" -}}
{{- end -}}
{{- if not (regexMatch "^https?://" .Values.web.url) -}}
{{- fail "web.url must be an http(s) URL" -}}
{{- end -}}
{{- if and .Values.runner.tools (semverCompare "<1.33.0-0" .Capabilities.KubeVersion.Version) -}}
{{- fail (printf "runner.tools needs Kubernetes 1.33 or newer, which mounts an image volume with a subPath; this cluster is %s" .Capabilities.KubeVersion.Version) -}}
{{- end -}}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "kritika.fullname" $ }}
  namespace: {{ $.Release.Namespace }}
  labels:
    {{- include "kritika.labels" $ | nindent 4 }}
    app.kubernetes.io/component: server
  {{- with $.Values.deploymentAnnotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
spec:
  replicas: {{ $.Values.replicas }}
  {{- with $.Values.strategy }}
  strategy:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  selector:
    matchLabels:
      {{- include "kritika.selectorLabels" $ | nindent 6 }}
      app.kubernetes.io/component: server
  template:
    metadata:
      labels:
        {{- include "kritika.labels" $ | nindent 8 }}
        app.kubernetes.io/component: server
        {{- with $.Values.podLabels }}
        {{- tpl (toYaml .) $ | nindent 8 }}
        {{- end }}
      {{- $checksum := "" }}
      {{- if and $.Values.config (not $.Values.existingConfigMap) }}
      {{- $checksum = toYaml $.Values.config | sha256sum }}
      {{- end }}
      {{- if or $checksum $.Values.podAnnotations }}
      annotations:
        {{- with $checksum }}
        # kritika reads the file at startup, so a change rolls the pods.
        checksum/config: {{ . }}
        {{- end }}
        {{- with $.Values.podAnnotations }}
        {{- tpl (toYaml .) $ | nindent 8 }}
        {{- end }}
      {{- end }}
    spec:
      # The Service is named after the release; the kubelet's legacy link
      # variables would otherwise inject KRITIKA_PORT and collide with config.
      enableServiceLinks: false
      {{- with $.Values.imagePullSecrets }}
      imagePullSecrets:
        {{- tpl (toYaml .) $ | nindent 8 }}
      {{- end }}
      serviceAccountName: {{ include "kritika.serviceAccountName" $ | quote }}
      automountServiceAccountToken: {{ $.Values.serviceAccount.automount }}
      {{- with $.Values.priorityClassName }}
      priorityClassName: {{ tpl . $ | quote }}
      {{- end }}
      terminationGracePeriodSeconds: {{ $.Values.terminationGracePeriodSeconds }}
      securityContext:
        {{- tpl (toYaml $.Values.podSecurityContext) $ | nindent 8 }}
      containers:
        - name: kritika
          image: {{ include "kritika.image" $ | quote }}
          imagePullPolicy: {{ $.Values.image.pullPolicy }}
          args:
            - serve
          securityContext:
            {{- tpl (toYaml $.Values.securityContext) $ | nindent 12 }}
          {{- $serveEnv := include "kritika.serveEnv" $ | fromYamlArray }}
          env:
            {{- toYaml $serveEnv | nindent 12 }}
            {{- range $name, $value := $.Values.env }}
            {{- range $serveEnv }}
            {{- if eq .name $name }}
            {{- fail (printf "env.%s: the chart sets this variable from its other values" $name) }}
            {{- end }}
            {{- end }}
            - name: {{ $name }}
              {{- if kindIs "map" $value }}
              {{- tpl (toYaml $value) $ | nindent 14 }}
              {{- else }}
              value: {{ tpl (toString $value) $ | quote }}
              {{- end }}
            {{- end }}
          {{- with $.Values.envFrom }}
          envFrom:
            {{- tpl (toYaml .) $ | nindent 12 }}
          {{- end }}
          ports:
            - name: http
              containerPort: {{ $.Values.service.port }}
              protocol: TCP
            - name: metrics
              containerPort: {{ $.Values.service.metricsPort }}
              protocol: TCP
            - name: gateway
              containerPort: {{ $.Values.service.gatewayPort }}
              protocol: TCP

          livenessProbe:
            {{- tpl (toYaml $.Values.livenessProbe) $ | nindent 12 }}
          readinessProbe:
            {{- tpl (toYaml $.Values.readinessProbe) $ | nindent 12 }}
          {{- with $.Values.startupProbe }}
          startupProbe:
            {{- tpl (toYaml .) $ | nindent 12 }}
          {{- end }}
          {{- with $.Values.resources }}
          resources:
            {{- tpl (toYaml .) $ | nindent 12 }}
          {{- end }}
          volumeMounts:
            {{- if include "kritika.hasConfigFile" $ }}
            - name: config
              mountPath: /etc/kritika
              readOnly: true
            {{- end }}
            {{- with $.Values.volumeMounts }}
            {{- tpl (toYaml .) $ | nindent 12 }}
            {{- end }}
      volumes:
        {{- if include "kritika.hasConfigFile" $ }}
        - name: config
          configMap:
            name: {{ include "kritika.configMapName" $ | quote }}
        {{- end }}
        {{- with $.Values.volumes }}
        {{- tpl (toYaml .) $ | nindent 8 }}
        {{- end }}
      {{- with $.Values.nodeSelector }}
      nodeSelector:
        {{- tpl (toYaml .) $ | nindent 8 }}
      {{- end }}
      {{- with $.Values.affinity }}
      affinity:
        {{- tpl (toYaml .) $ | nindent 8 }}
      {{- end }}
      {{- with $.Values.topologySpreadConstraints }}
      topologySpreadConstraints:
        {{- tpl (toYaml .) $ | nindent 8 }}
      {{- end }}
      {{- with $.Values.tolerations }}
      tolerations:
        {{- tpl (toYaml .) $ | nindent 8 }}
      {{- end }}
