{{/* Name of the chart. */}}
{{- define "scarpenter.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Fully qualified name: the release name, unless it already contains the chart name. */}}
{{- define "scarpenter.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "scarpenter.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "scarpenter.selectorLabels" -}}
app.kubernetes.io/name: {{ include "scarpenter.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "scarpenter.labels" -}}
helm.sh/chart: {{ include "scarpenter.chart" . }}
{{ include "scarpenter.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "scarpenter.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "scarpenter.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* The image reference: a digest wins over a tag; the tag defaults to the appVersion. */}}
{{- define "scarpenter.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}
{{- end }}

{{/*
Seconds of a Go duration made of h, m and s parts ("1h", "90s", "1h30m").
*/}}
{{- define "scarpenter.seconds" -}}
{{- $total := 0 }}
{{- range $part := regexFindAll "[0-9]+[hms]" (toString .) -1 }}
{{- $n := int (regexFind "^[0-9]+" $part) }}
{{- $unit := regexFind "[hms]$" $part }}
{{- if eq $unit "h" }}{{ $total = add $total (mul $n 3600) }}
{{- else if eq $unit "m" }}{{ $total = add $total (mul $n 60) }}
{{- else }}{{ $total = add $total $n }}
{{- end }}
{{- end }}
{{- $total }}
{{- end }}

{{/* The vendors flag: aws, hcloud, ovh and vultr if enabled, then the extra ones (development builds: fake, qemu). */}}
{{- define "scarpenter.vendors" -}}
{{- $v := list }}
{{- if .Values.vendors.aws.enabled }}{{ $v = append $v "aws" }}{{ end }}
{{- if .Values.vendors.hcloud.enabled }}{{ $v = append $v "hcloud" }}{{ end }}
{{- if .Values.vendors.ovh.enabled }}{{ $v = append $v "ovh" }}{{ end }}
{{- if .Values.vendors.vultr.enabled }}{{ $v = append $v "vultr" }}{{ end }}
{{- range .Values.vendors.extra }}{{ $v = append $v . }}{{ end }}
{{- join "," $v }}
{{- end }}

{{/* Name of the ConfigMap or Secret that holds the kubelet client CA, and its key. */}}
{{- define "scarpenter.kubeletClientCAName" -}}
{{- $s := .Values.kubeletClientCA.source }}
{{- if or (eq $s "inline") (eq $s "extensionApiserverAuthentication") }}{{ include "scarpenter.fullname" . }}-kubelet-client-ca
{{- else if eq $s "configMap" }}{{ .Values.kubeletClientCA.configMap.name }}
{{- else if eq $s "secret" }}{{ .Values.kubeletClientCA.secret.name }}
{{- end }}
{{- end }}
{{- define "scarpenter.kubeletClientCAKey" -}}
{{- $s := .Values.kubeletClientCA.source }}
{{- if eq $s "configMap" }}{{ .Values.kubeletClientCA.configMap.key }}
{{- else if eq $s "secret" }}{{ .Values.kubeletClientCA.secret.key }}
{{- else }}client-ca.crt
{{- end }}
{{- end }}
