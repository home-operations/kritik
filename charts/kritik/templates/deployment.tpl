{{- $roles := splitList "," (include "kritik.enabledRoles" .) -}}
{{- if not (include "kritik.enabledRoles" .) -}}
{{- fail "no role is enabled: set roles.all.enabled, or roles.ingest.enabled and roles.worker.enabled" -}}
{{- end -}}
{{- if and .Values.roles.all.enabled (or .Values.roles.ingest.enabled .Values.roles.worker.enabled) -}}
{{- fail "roles.all is the single-process topology; do not enable it together with roles.ingest or roles.worker" -}}
{{- end -}}
{{- if not .Values.database.app.existingSecret -}}
{{- fail "database.app.existingSecret is required: the Secret holding the application role's connection URI" -}}
{{- end -}}
{{- if and (include "kritik.hasWorker" .) (not .Values.database.owner.existingSecret) -}}
{{- fail "database.owner.existingSecret is required for roles.all / roles.worker: the leader runs migrations with it" -}}
{{- end -}}
{{- if and (include "kritik.hasWorker" .) (not .Values.database.runner.existingSecret) -}}
{{- fail "database.runner.existingSecret is required for roles.all / roles.worker: runner Jobs connect with it" -}}
{{- end -}}
{{- if and (include "kritik.embeddingEnabled" .) (not (include "kritik.embeddingSecretName" .)) -}}
{{- fail "embedding.model is set but no key is given: set embedding.apiKey or embedding.existingSecret" -}}
{{- end -}}
{{- if and .Values.roles.web.enabled (not .Values.web.url) -}}
{{- fail "web.url is required when roles.web.enabled is true" -}}
{{- end -}}
{{- if and .Values.web.url (not (regexMatch "^https?://" .Values.web.url)) -}}
{{- fail "web.url must be an http(s) URL" -}}
{{- end -}}
{{- range $role := $roles }}
{{- $spec := index $.Values.roles $role }}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "kritik.roleName" (dict "root" $ "role" $role) }}
  namespace: {{ $.Release.Namespace }}
  labels:
    {{- include "kritik.labels" $ | nindent 4 }}
    app.kubernetes.io/component: {{ $role }}
  {{- with $.Values.deploymentAnnotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
spec:
  replicas: {{ $spec.replicas }}
  selector:
    matchLabels:
      {{- include "kritik.selectorLabels" $ | nindent 6 }}
      app.kubernetes.io/component: {{ $role }}
  template:
    metadata:
      labels:
        {{- include "kritik.labels" $ | nindent 8 }}
        app.kubernetes.io/component: {{ $role }}
        {{- if and (ne $role "worker") (ne $role "web") }}
        # Selected by the webhook Service.
        kritik.home-operations.com/hooks: "true"
        {{- end }}
        {{- if and (ne $role "ingest") (ne $role "web") $.Values.gateway.enabled }}
        # Selected by the gateway Service and the runner network policy.
        kritik.home-operations.com/gateway: "true"
        {{- end }}
        {{- if or (eq $role "web") (and (eq $role "all") $.Values.web.url) }}
        # Selected by the dashboard Service and the web-ingress network policy rule.
        kritik.home-operations.com/web: "true"
        {{- end }}
        {{- with $.Values.podLabels }}
        {{- tpl (toYaml .) $ | nindent 8 }}
        {{- end }}
      {{- with $.Values.podAnnotations }}
      annotations:
        {{- tpl (toYaml .) $ | nindent 8 }}
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
            - --role
            - {{ $role }}
          securityContext:
            {{- tpl (toYaml $.Values.securityContext) $ | nindent 12 }}
          env:
            - name: KRITIK_CONFIG_FILE
              value: /etc/kritik/config.yaml
            - name: KRITIK_CONFIG_RELOAD_INTERVAL
              value: {{ tpl (toString $.Values.config.reloadInterval) $ | quote }}
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
            {{- with $.Values.dashboard.keySecret.name }}
            - name: KRITIK_DASHBOARD_KEY
              valueFrom:
                secretKeyRef:
                  name: {{ tpl . $ | quote }}
                  key: {{ $.Values.dashboard.keySecret.key | quote }}
            {{- end }}
            {{- with $.Values.dashboard.oldKeysSecret.name }}
            - name: KRITIK_DASHBOARD_OLD_KEYS
              valueFrom:
                secretKeyRef:
                  name: {{ tpl . $ | quote }}
                  key: {{ $.Values.dashboard.oldKeysSecret.key | quote }}
            {{- end }}
            {{- if and (ne $role "ingest") (ne $role "web") }}
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
            - name: KRITIK_RUNNER_DEADLINE
              value: {{ tpl (toString $.Values.runner.deadline) $ | quote }}
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
            - name: KRITIK_POLL_INTERVAL
              value: {{ tpl (toString $.Values.config.pollInterval) $ | quote }}
            - name: KRITIK_POLL_LOOKBACK
              value: {{ tpl (toString $.Values.config.pollLookback) $ | quote }}
            {{- if include "kritik.embeddingEnabled" $ }}
            - name: KRITIK_EMBED_BASE_URL
              value: {{ tpl $.Values.embedding.baseUrl $ | quote }}
            - name: KRITIK_EMBED_MODEL
              value: {{ tpl $.Values.embedding.model $ | quote }}
            - name: KRITIK_EMBED_DIMS
              value: {{ $.Values.embedding.dims | quote }}
            - name: KRITIK_EMBED_API_KEY
              valueFrom:
                secretKeyRef:
                  name: {{ include "kritik.embeddingSecretName" $ | quote }}
                  key: {{ include "kritik.embeddingSecretKey" $ | quote }}
            - name: KRITIK_EMBED_MAX_BATCH
              value: {{ $.Values.embedding.maxBatch | quote }}
            - name: KRITIK_EMBED_MAX_BATCH_CHARS
              value: {{ $.Values.embedding.maxBatchChars | quote }}
            - name: KRITIK_EMBED_MAX_ITEM_CHARS
              value: {{ $.Values.embedding.maxItemChars | quote }}
            {{- if $.Values.embedding.reindexOnModelChange }}
            - name: KRITIK_REINDEX_ON_MODEL_CHANGE
              value: "true"
            {{- end }}
            {{- end }}
            {{- end }}
            {{- if or (eq $role "web") (and (eq $role "all") $.Values.web.url) }}
            - name: KRITIK_WEB_URL
              value: {{ tpl $.Values.web.url $ | quote }}
            - name: KRITIK_WEB_ADDR
              value: {{ printf ":%d" (int $.Values.web.port) | quote }}
            {{- end }}
            {{- with $.Values.config.extraEnv }}
            {{- tpl (toYaml .) $ | nindent 12 }}
            {{- end }}
          ports:
            {{- if and (ne $role "worker") (ne $role "web") }}
            - name: http
              containerPort: {{ $.Values.service.port }}
              protocol: TCP
            {{- end }}
            - name: metrics
              containerPort: {{ $.Values.service.metricsPort }}
              protocol: TCP
            {{- if and (ne $role "ingest") (ne $role "web") $.Values.gateway.enabled }}
            - name: gateway
              containerPort: {{ $.Values.gateway.port }}
              protocol: TCP
            {{- end }}
            {{- if or (eq $role "web") (and (eq $role "all") $.Values.web.url) }}
            - name: web
              containerPort: {{ $.Values.web.port }}
              protocol: TCP
            {{- end }}
          livenessProbe:
            {{- tpl (toYaml $.Values.livenessProbe) $ | nindent 12 }}
          readinessProbe:
            {{- tpl (toYaml $.Values.readinessProbe) $ | nindent 12 }}
          {{- with (default $.Values.resources $spec.resources) }}
          resources:
            {{- tpl (toYaml .) $ | nindent 12 }}
          {{- end }}
          volumeMounts:
            - name: config
              mountPath: /etc/kritik
              readOnly: true
            {{- range $.Values.secretMounts }}
            - name: {{ .name }}
              mountPath: {{ tpl .mountPath $ }}
              readOnly: true
            {{- end }}
            {{- with $.Values.volumeMounts }}
            {{- tpl (toYaml .) $ | nindent 12 }}
            {{- end }}
      volumes:
        - name: config
          configMap:
            name: {{ include "kritik.configMapName" $ | quote }}
        {{- range $.Values.secretMounts }}
        - name: {{ .name }}
          secret:
            secretName: {{ tpl .secretName $ | quote }}
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
{{- end }}
