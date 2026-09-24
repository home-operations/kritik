{{- if and .Values.rbac.create (include "kritik.hasWorker" .) }}
# The worker creates runner Jobs in its own namespace and reads their pods
# and logs; nothing cluster-wide, nothing else.
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
