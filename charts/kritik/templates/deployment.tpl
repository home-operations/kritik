{{- if not .Values.database.app.existingSecret -}}
{{- fail "database.app.existingSecret is required: the Secret holding the application role's connection URI" -}}
{{- end -}}
{{- if not .Values.database.owner.existingSecret -}}
{{- fail "database.owner.existingSecret is required: the leader runs migrations with it" -}}
{{- end -}}
{{- if not .Values.database.runner.existingSecret -}}
{{- fail "database.runner.existingSecret is required: runner Jobs connect with it" -}}
{{- end -}}
{{- if not .Values.web.url -}}
{{- fail "web.url is required: the dashboard's public URL, which the webhook listener shares under /hooks" -}}
{{- end -}}
{{- if not (regexMatch "^https?://" .Values.web.url) -}}
{{- fail "web.url must be an http(s) URL" -}}
{{- end -}}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "kritik.fullname" $ }}
  namespace: {{ $.Release.Namespace }}
  labels:
    {{- include "kritik.labels" $ | nindent 4 }}
    app.kubernetes.io/component: server
  {{- with $.Values.deploymentAnnotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
spec:
  replicas: {{ $.Values.replicas }}
  selector:
    matchLabels:
      {{- include "kritik.selectorLabels" $ | nindent 6 }}
      app.kubernetes.io/component: server
  template:
    metadata:
      labels:
        {{- include "kritik.labels" $ | nindent 8 }}
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
        # kritik reads the file at startup (ADR-0022), so a change rolls the pods.
        checksum/config: {{ . }}
        {{- end }}
        {{- with $.Values.podAnnotations }}
        {{- tpl (toYaml .) $ | nindent 8 }}
        {{- end }}
      {{- end }}
    spec:
      # The Service is named after the release; the kubelet's legacy link
      # variables would otherwise inject KRITIK_PORT and collide with config.
      enableServiceLinks: false
      {{- with $.Values.imagePullSecrets }}
      imagePullSecrets:
        {{- tpl (toYaml .) $ | nindent 8 }}
      {{- end }}
      serviceAccountName: {{ include "kritik.serviceAccountName" $ | quote }}
      automountServiceAccountToken: {{ $.Values.serviceAccount.automount }}
      {{- with $.Values.priorityClassName }}
      priorityClassName: {{ tpl . $ | quote }}
      {{- end }}
      terminationGracePeriodSeconds: {{ $.Values.terminationGracePeriodSeconds }}
      securityContext:
        {{- tpl (toYaml $.Values.podSecurityContext) $ | nindent 8 }}
      containers:
        - name: kritik
          image: {{ include "kritik.image" $ | quote }}
          imagePullPolicy: {{ $.Values.image.pullPolicy }}
          args:
            - serve
          securityContext:
            {{- tpl (toYaml $.Values.securityContext) $ | nindent 12 }}
          env:
            {{- if include "kritik.hasConfigFile" $ }}
            - name: KRITIK_CONFIG_FILE
              value: /etc/kritik/config.yaml
            {{- end }}
            - name: KRITIK_WEB_URL
              value: {{ tpl $.Values.web.url $ | quote }}
            {{- include "kritik.authEnv" $ | nindent 12 }}
            {{- range $.Values.secretEnv }}
            - name: {{ .name }}
              valueFrom:
                secretKeyRef:
                  name: {{ tpl .secretName $ | quote }}
                  key: {{ .key | quote }}
            {{- end }}
            - name: KRITIK_LOG_LEVEL
              value: {{ tpl $.Values.config.logLevel $ | quote }}
            - name: KRITIK_LOG_FORMAT
              value: {{ tpl $.Values.config.logFormat $ | quote }}
            - name: KRITIK_ADDR
              value: {{ printf ":%d" (int $.Values.service.port) | quote }}
            - name: KRITIK_METRICS_ADDR
              value: {{ printf ":%d" (int $.Values.service.metricsPort) | quote }}
            - name: KRITIK_DATABASE_APP_ROLE
              value: {{ $.Values.database.app.role | quote }}
            - name: KRITIK_DATABASE_RUNNER_ROLE
              value: {{ $.Values.database.runner.role | quote }}
            - name: KRITIK_DATABASE_URL
              valueFrom:
                secretKeyRef:
                  name: {{ tpl $.Values.database.app.existingSecret $ | quote }}
                  key: {{ $.Values.database.app.key | quote }}
            - name: KRITIK_DATABASE_OWNER_URL
              valueFrom:
                secretKeyRef:
                  name: {{ tpl $.Values.database.owner.existingSecret $ | quote }}
                  key: {{ $.Values.database.owner.key | quote }}
            - name: KRITIK_EXECUTOR
              value: kubernetes
            - name: KRITIK_RUNNER_IMAGE
              value: {{ include "kritik.runnerImage" $ | quote }}
            - name: KRITIK_RUNNER_SERVICE_ACCOUNT
              value: {{ include "kritik.runnerServiceAccountName" $ | quote }}
            - name: KRITIK_RUNNER_DATABASE_SECRET
              value: {{ tpl $.Values.database.runner.existingSecret $ | quote }}
            - name: KRITIK_RUNNER_DATABASE_SECRET_KEY
              value: {{ $.Values.database.runner.key | quote }}
            - name: KRITIK_RUNNER_TTL
              value: {{ tpl (toString $.Values.runner.ttl) $ | quote }}
            {{- with $.Values.runner.runtimeClassName }}
            - name: KRITIK_RUNNER_RUNTIME_CLASS
              value: {{ tpl . $ | quote }}
            {{- end }}
            - name: KRITIK_GATEWAY_ADDR
              value: {{ printf ":%d" (int $.Values.gateway.port) | quote }}
            {{- with include "kritik.gatewayURL" $ }}
            - name: KRITIK_GATEWAY_URL
              value: {{ . | quote }}
            {{- end }}
            - name: KRITIK_REVIEW_WORKERS
              value: {{ $.Values.config.reviewWorkers | quote }}
            - name: KRITIK_INDEX_WORKERS
              value: {{ $.Values.config.indexWorkers | quote }}
            {{- range $name, $value := dict "KRITIK_POLL_INTERVAL" $.Values.config.pollInterval "KRITIK_POLL_LOOKBACK" $.Values.config.pollLookback "KRITIK_INDEX_GRACE" $.Values.config.indexGrace "KRITIK_TRANSCRIPT_RETENTION" $.Values.config.transcriptRetention "KRITIK_RUNNER_DEADLINE" $.Values.runner.deadline }}
            {{- with $value }}
            - name: {{ $name }}
              value: {{ . | quote }}
            {{- end }}
            {{- end }}
            {{- with $.Values.config.onboardWindow }}
            - name: KRITIK_ONBOARD_WINDOW
              value: {{ . | quote }}
            {{- end }}
            {{- with $.Values.runner.resources }}
            - name: KRITIK_RUNNER_RESOURCES
              value: {{ toJson . | quote }}
            {{- end }}
            {{- with $.Values.runner.tools }}
            - name: KRITIK_RUNNER_TOOLS
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
            {{- if $.Values.gateway.enabled }}
            - name: gateway
              containerPort: {{ $.Values.gateway.port }}
              protocol: TCP
            {{- end }}

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
            {{- if include "kritik.hasConfigFile" $ }}
            - name: config
              mountPath: /etc/kritik
              readOnly: true
            {{- end }}
            {{- with $.Values.volumeMounts }}
            {{- tpl (toYaml .) $ | nindent 12 }}
            {{- end }}
      volumes:
        {{- if include "kritik.hasConfigFile" $ }}
        - name: config
          configMap:
            name: {{ include "kritik.configMapName" $ | quote }}
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
      {{- with $.Values.tolerations }}
      tolerations:
        {{- tpl (toYaml .) $ | nindent 8 }}
      {{- end }}
