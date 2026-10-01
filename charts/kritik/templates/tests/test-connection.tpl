apiVersion: v1
kind: Pod
metadata:
  name: {{ include "kritik.fullname" . }}-test-connection
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritik.labels" . | nindent 4 }}
  annotations:
    helm.sh/hook: test
    # before-hook-creation only (no hook-succeeded): `helm test` then never
    # deletes the pod itself, so it cannot block on Helm 4's wait-for-delete
    # after a green run. The pod is recreated on the next run, and a failed
    # run's pod stays for `helm test --logs` and kubectl.
    helm.sh/hook-delete-policy: before-hook-creation
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: connection
      image: {{ include "kritik.testImage" . | quote }}
      imagePullPolicy: {{ .Values.tests.image.pullPolicy }}
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop:
            - ALL
      # /readyz answers once a replica has loaded its configuration and its
      # listeners are up, so this checks that the metrics Service routes to
      # a replica that answers. curl -f fails on a non-2xx or a refused
      # connection, failing `helm test`; -sS stays quiet but surfaces errors,
      # and the body goes to stdout, so the rootfs stays read-only.
      command:
        - curl
      args:
        - -fsS
        - http://{{ include "kritik.fullname" . }}-metrics:{{ .Values.service.metricsPort }}/readyz
