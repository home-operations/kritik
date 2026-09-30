{{- if .Values.rbac.create }}
# kritik serve creates runner Jobs in its own namespace, reads each back by
# name, lists their pods and reads their logs; nothing cluster-wide, nothing
# else, and no watches: it polls.
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
    verbs: ["create", "get", "delete"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list"]
  - apiGroups: [""]
    resources: ["pods/log"]
    verbs: ["get"]
  # One Secret per runner Job carries that run's credentials, owned by the
  # Job. No get or list: the worker writes them and never reads any; the
  # leader deletes one a dead worker left unowned by name, from the run's
  # database row.
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["create", "patch", "delete"]
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
