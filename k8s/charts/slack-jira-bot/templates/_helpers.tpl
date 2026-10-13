{{/* リソース名。Secret 名やセレクターと揃えるため固定にしている */}}
{{- define "slack-jira-bot.name" -}}
{{- .Chart.Name -}}
{{- end -}}

{{/*
セレクター用のラベル。kustomize 時代と同じ値にしている（Deployment のセレクターは変更できないため）
*/}}
{{- define "slack-jira-bot.selectorLabels" -}}
app: {{ include "slack-jira-bot.name" . }}
{{- end -}}

{{- define "slack-jira-bot.labels" -}}
{{ include "slack-jira-bot.selectorLabels" . }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
app.kubernetes.io/name: {{ include "slack-jira-bot.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "slack-jira-bot.image" -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}
{{- end -}}
