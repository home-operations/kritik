{{- if and .Values.rbac.create (include "kritik.hasWorker" .) }}
# The worker creates runner Jobs in its own namespace, each with a
# job-scoped Secret it creates and hands to the Job, and reads their pods
# and logs; nothing cluster-wide, nothing else. Listing Secrets lets the
# leader find run Secrets a dead worker left without an owning Job; a Role
# cannot narrow list by label, so it reaches every Secret in the namespace.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: {{ include "kritik.fullname" . }}-worker
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritik.labels" . | nindent 4 }}
rules:
  - apiGroups: ["batch"]
    resources: ["jobs"]
    verbs: ["create", "get", "list", "watch", "delete"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["pods/log"]
    verbs: ["get"]
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["create", "list", "patch", "delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {{ include "kritik.fullname" . }}-worker
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "kritik.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: {{ include "kritik.fullname" . }}-worker
subjects:
  - kind: ServiceAccount
    name: {{ include "kritik.serviceAccountName" . | quote }}
    namespace: {{ .Release.Namespace }}
{{- end }}
