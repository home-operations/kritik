{{- if .Values.podDisruptionBudget.enabled }}
{{- range $role := splitList "," (include "kritik.enabledRoles" .) }}
{{- $spec := index $.Values.roles $role }}
{{- if gt (int $spec.replicas) 1 }}
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: {{ include "kritik.roleName" (dict "root" $ "role" $role) }}
  namespace: {{ $.Release.Namespace }}
  labels:
    {{- include "kritik.labels" $ | nindent 4 }}
    app.kubernetes.io/component: {{ $role }}
spec:
  maxUnavailable: {{ $.Values.podDisruptionBudget.maxUnavailable }}
  selector:
    matchLabels:
      {{- include "kritik.selectorLabels" $ | nindent 6 }}
      app.kubernetes.io/component: {{ $role }}
{{- end }}
{{- end }}
{{- end }}
