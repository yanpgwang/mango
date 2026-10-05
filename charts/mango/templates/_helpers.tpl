{{- define "mango.resourceName" -}}
{{- $name := printf "%s-%s" (default .root.Release.Name .root.Values.fullnameOverride) .suffix -}}
{{- if gt (len $name) 63 -}}
{{- printf "%s-%s" ($name | trunc 54 | trimSuffix "-") ($name | sha256sum | trunc 8) -}}
{{- else -}}{{ $name }}{{- end -}}
{{- end -}}

{{- define "mango.labels" -}}
app.kubernetes.io/name: mango
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | quote }}
{{- end -}}

{{- define "mango.selector" -}}
app.kubernetes.io/name: mango
app.kubernetes.io/instance: {{ .root.Release.Name | quote }}
app.kubernetes.io/component: {{ .role | quote }}
{{- end -}}

{{- define "mango.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end -}}

{{- define "mango.podSecurity" -}}
runAsNonRoot: true
runAsUser: 65532
runAsGroup: 65532
fsGroup: 65532
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{- define "mango.containerSecurity" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop: [ALL]
{{- end -}}

{{- define "mango.runtimeEnv" -}}
- name: MANGO_DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.existingSecret.name | quote }}
      key: {{ .Values.database.existingSecret.key | quote }}
- name: MANGO_NATS_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.nats.existingSecret.name | quote }}
      key: {{ .Values.nats.existingSecret.key | quote }}
- name: MANGO_FILE_S3_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.files.existingSecret.name | quote }}
      key: {{ .Values.files.existingSecret.accessKey | quote }}
- name: MANGO_FILE_S3_SECRET_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.files.existingSecret.name | quote }}
      key: {{ .Values.files.existingSecret.secretKey | quote }}
{{- $config := include "mango.resourceName" (dict "root" . "suffix" "config") -}}
{{- range $key := list "MANGO_TEMPORAL_HOSTPORT" "MANGO_TEMPORAL_NAMESPACE" "MANGO_FILE_S3_ENDPOINT" "MANGO_FILE_S3_REGION" "MANGO_FILE_S3_BUCKET" "MANGO_FILE_S3_PATH_STYLE" "MANGO_FILE_S3_CREATE_BUCKET" "MANGO_FILE_UPLOAD_TEMP_DIR" }}
- name: {{ $key }}
  valueFrom:
    configMapKeyRef: {name: {{ $config | quote }}, key: {{ $key | quote }}}
{{- end }}
{{- if .Values.vault.enabled }}
- name: MANGO_VAULT_KEYRING_FILE
  value: /run/mango/keyring/keyring.json
{{- end }}
{{- end -}}

{{- define "mango.keyringVolume" -}}
{{- if .Values.vault.enabled -}}
- name: keyring
  secret:
    secretName: {{ .Values.vault.existingSecret.name | quote }}
    defaultMode: 0440
    items:
      - key: {{ .Values.vault.existingSecret.key | quote }}
        path: keyring.json
{{- end -}}
{{- end -}}

{{- define "mango.keyringMount" -}}
{{- if .Values.vault.enabled -}}
- name: keyring
  mountPath: /run/mango/keyring
  readOnly: true
{{- end -}}
{{- end -}}
