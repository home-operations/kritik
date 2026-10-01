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
      {{- if and $.Values.config.file (not $.Values.config.existingConfigMap) }}
      {{- $checksum = toYaml $.Values.config.file | sha256sum }}
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
          env:
            {{- if include "kritika.hasConfigFile" $ }}
            - name: KRITIKA_CONFIG_FILE
              value: /etc/kritika/config.yaml
            {{- end }}
            - name: KRITIKA_WEB_URL
              value: {{ tpl $.Values.web.url $ | quote }}
            {{- include "kritika.authEnv" $ | nindent 12 }}
            {{- range $.Values.secretEnv }}
            - name: {{ .name }}
              valueFrom:
                secretKeyRef:
                  name: {{ tpl .secretName $ | quote }}
                  key: {{ .key | quote }}
            {{- end }}
            - name: KRITIKA_LOG_LEVEL
              value: {{ tpl $.Values.config.logLevel $ | quote }}
            - name: KRITIKA_LOG_FORMAT
              value: {{ tpl $.Values.config.logFormat $ | quote }}
            - name: KRITIKA_ADDR
              value: {{ printf ":%d" (int $.Values.service.port) | quote }}
            - name: KRITIKA_METRICS_ADDR
              value: {{ printf ":%d" (int $.Values.service.metricsPort) | quote }}
            - name: KRITIKA_DATABASE_APP_ROLE
              value: {{ $.Values.database.app.role | quote }}
            - name: KRITIKA_DATABASE_RUNNER_ROLE
              value: {{ $.Values.database.runner.role | quote }}
            {{- with $.Values.database.host }}
            - name: KRITIKA_DATABASE_HOST
              value: {{ tpl . $ | quote }}
            - name: KRITIKA_DATABASE_PORT
              value: {{ $.Values.database.port | quote }}
            - name: KRITIKA_DATABASE_NAME
              value: {{ tpl $.Values.database.name $ | quote }}
            - name: KRITIKA_DATABASE_SSLMODE
              value: {{ tpl $.Values.database.sslmode $ | quote }}
            - name: KRITIKA_DATABASE_CONNECT_TIMEOUT
              value: {{ tpl (toString $.Values.database.connectTimeout) $ | quote }}
            {{- end }}
            {{- range $role := list (dict "prefix" "KRITIKA_DATABASE" "spec" $.Values.database.app) (dict "prefix" "KRITIKA_DATABASE_OWNER" "spec" $.Values.database.owner) }}
            {{- if $role.spec.uriKey }}
            - name: {{ $role.prefix }}_URL
              valueFrom:
                secretKeyRef:
                  name: {{ tpl $role.spec.existingSecret $ | quote }}
                  key: {{ $role.spec.uriKey | quote }}
            {{- else }}
            - name: {{ $role.prefix }}_USER
              valueFrom:
                secretKeyRef:
                  name: {{ tpl $role.spec.existingSecret $ | quote }}
                  key: {{ $role.spec.usernameKey | quote }}
            - name: {{ $role.prefix }}_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: {{ tpl $role.spec.existingSecret $ | quote }}
                  key: {{ $role.spec.passwordKey | quote }}
            {{- end }}
            {{- end }}
            - name: KRITIKA_EXECUTOR
              value: kubernetes
            - name: KRITIKA_RUNNER_IMAGE
              value: {{ include "kritika.runnerImage" $ | quote }}
            - name: KRITIKA_RUNNER_SERVICE_ACCOUNT
              value: {{ include "kritika.runnerServiceAccountName" $ | quote }}
            - name: KRITIKA_RUNNER_DATABASE_SECRET
              value: {{ tpl $.Values.database.runner.existingSecret $ | quote }}
            - name: KRITIKA_RUNNER_DATABASE_SECRET_KEY
              value: {{ $.Values.database.runner.uriKey | quote }}
            - name: KRITIKA_RUNNER_DATABASE_SECRET_USER_KEY
              value: {{ $.Values.database.runner.usernameKey | quote }}
            - name: KRITIKA_RUNNER_DATABASE_SECRET_PASSWORD_KEY
              value: {{ $.Values.database.runner.passwordKey | quote }}
            - name: KRITIKA_RUNNER_TTL
              value: {{ tpl (toString $.Values.runner.ttl) $ | quote }}
            {{- with $.Values.runner.runtimeClassName }}
            - name: KRITIKA_RUNNER_RUNTIME_CLASS
              value: {{ tpl . $ | quote }}
            {{- end }}
            - name: KRITIKA_GATEWAY_ADDR
              value: {{ printf ":%d" (int $.Values.gateway.port) | quote }}
            - name: KRITIKA_GATEWAY_URL
              value: {{ include "kritika.gatewayURL" $ | quote }}
            - name: KRITIKA_REVIEW_WORKERS
              value: {{ $.Values.config.reviewWorkers | quote }}
            - name: KRITIKA_INDEX_WORKERS
              value: {{ $.Values.config.indexWorkers | quote }}
            {{- range $name, $value := dict "KRITIKA_POLL_INTERVAL" $.Values.config.pollInterval "KRITIKA_POLL_LOOKBACK" $.Values.config.pollLookback "KRITIKA_INDEX_GRACE" $.Values.config.indexGrace "KRITIKA_TRANSCRIPT_RETENTION" $.Values.config.transcriptRetention "KRITIKA_DIFF_RETENTION" $.Values.config.diffRetention "KRITIKA_RUNNER_DEADLINE" $.Values.runner.deadline }}
            {{- with $value }}
            - name: {{ $name }}
              value: {{ . | quote }}
            {{- end }}
            {{- end }}
            {{- with $.Values.config.onboardWindow }}
            - name: KRITIKA_ONBOARD_WINDOW
              value: {{ . | quote }}
            {{- end }}
            {{- with $.Values.runner.resources }}
            - name: KRITIKA_RUNNER_RESOURCES
              value: {{ toJson . | quote }}
            {{- end }}
            {{- with $.Values.runner.tools }}
            - name: KRITIKA_RUNNER_TOOLS
              value: {{ toJson . | quote }}
            {{- end }}
            {{- with $.Values.config.extraEnv }}
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
              containerPort: {{ $.Values.gateway.port }}
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
