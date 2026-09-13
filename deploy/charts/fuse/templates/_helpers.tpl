{{/*
================================================================================
Named templates for the fuse chart.

THE GUARDS LIVE HERE, and every guard is invoked from `fuse.guards` which is
called from EVERY top-level template — not just from deployment.yaml. A guard
reachable from only one template is not a guard: `helm template -s` on any other
file, or a future values combination that stops rendering the Deployment, would
walk straight past it.
================================================================================
*/}}

{{- define "fuse.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "fuse.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "fuse.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "fuse.labels" -}}
helm.sh/chart: {{ include "fuse.chart" . }}
{{ include "fuse.selectorLabels" . }}
app.kubernetes.io/version: {{ include "fuse.imageTag" . | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: fuse
{{- end -}}

{{- define "fuse.selectorLabels" -}}
app.kubernetes.io/name: {{ include "fuse.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "fuse.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "fuse.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* The tag used for the app.kubernetes.io/version label. */}}
{{- define "fuse.imageTag" -}}
{{- default .Chart.AppVersion .Values.image.tag -}}
{{- end -}}

{{/*
The full image reference. A digest WINS over a tag: a tag is mutable and a
digest is not, so a pinned digest must not be silently overridden by a tag that
happens to also be set.
*/}}
{{- define "fuse.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (include "fuse.imageTag" .) -}}
{{- end -}}
{{- end -}}

{{/* Secret names. */}}
{{- define "fuse.configSecretName" -}}
{{- if .Values.config.existingSecret -}}
{{- .Values.config.existingSecret -}}
{{- else -}}
{{- printf "%s-config" (include "fuse.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "fuse.dsnSecretName" -}}
{{- if .Values.postgres.existingSecret -}}
{{- .Values.postgres.existingSecret -}}
{{- else if .Values.postgres.dsn -}}
{{- printf "%s-dsn" (include "fuse.fullname" .) -}}
{{- else if .Values.postgres.dev.enabled -}}
{{- printf "%s-postgres-dev" (include "fuse.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "fuse.dsnSecretKey" -}}
{{- if .Values.postgres.existingSecret -}}
{{- .Values.postgres.existingSecretKey -}}
{{- else -}}
{{- print "dsn" -}}
{{- end -}}
{{- end -}}

{{- define "fuse.postgresDevFullname" -}}
{{- printf "%s-postgres-dev" (include "fuse.fullname" .) -}}
{{- end -}}

{{/*
================================================================================
fuse.drainSeconds — server.drainTimeout as an integer number of seconds.

server.drainTimeout is handed VERBATIM to `--drain-timeout`, so it is a Go
duration string. This template converts it for
terminationGracePeriodSeconds = drainSeconds + 10, and there is no duration
parser behind that arithmetic: `trimSuffix "s" | int` would turn "1m" into 1 and
"500ms" into 500, producing a grace period wrong by orders of magnitude — and
wrong in the DANGEROUS direction, a SIGKILL landing in the middle of the drain
the value was meant to protect.

So it FAILS CLOSED on anything that is not ^[0-9]+s$ rather than guessing.
================================================================================
*/}}
{{- define "fuse.drainSeconds" -}}
{{- $d := .Values.server.drainTimeout | toString -}}
{{- if not (regexMatch "^[0-9]+s$" $d) -}}
{{- fail (printf "fuse: server.drainTimeout must be a whole number of seconds written as a Go duration, e.g. \"20s\" or \"90s\" — got %q. The chart derives terminationGracePeriodSeconds = drainTimeout + 10 by stripping the trailing \"s\", so a unit other than seconds (\"1m\", \"500ms\") would produce a grace period wrong by orders of magnitude and SIGKILL the pod mid-drain. Write \"90s\", not \"1m30s\"." $d) -}}
{{- end -}}
{{- $secs := $d | trimSuffix "s" | int -}}
{{- if lt $secs 1 -}}
{{- fail (printf "fuse: server.drainTimeout must be at least \"1s\" — got %q. A zero drain makes the graceful shutdown meaningless: in-flight unary calls are cut the instant the pod is told to stop." $d) -}}
{{- end -}}
{{- $secs -}}
{{- end -}}

{{- define "fuse.terminationGracePeriodSeconds" -}}
{{- include "fuse.drainSeconds" . | int | add 10 -}}
{{- end -}}

{{/*
================================================================================
fuse.configYaml — the trusted home file (ADR-0006) rendered from values.

`.Values.config` minus the chart's own plumbing keys (existingSecret, key),
with loop_server.auth layered in from `.Values.auth.tokens`. auth.tokens is
rendered into loop_server.auth and NOT into .Values.config, so an operator
cannot half-set both and get a surprising merge: the tokens key has exactly one
source.

When auth.allowDevToken is true, loop_server.auth is OMITTED entirely — that is
what makes the server synthesize and loudly log its dev token (ADR-0034). An
empty list would be a different thing to no key at all in some loaders, so the
key is absent, not empty.
================================================================================
*/}}
{{- define "fuse.configYaml" -}}
{{- $cfg := omit .Values.config "existingSecret" "key" | deepCopy -}}
{{- if .Values.auth.tokens -}}
{{- $_ := set $cfg "loop_server" (dict "auth" .Values.auth.tokens) -}}
{{- end -}}
{{- toYaml $cfg -}}
{{- end -}}

{{/*
The checksum that rolls the pods when the config changes. It is computed from
the RENDERED config, not from the Secret object, so it is identical whether the
Secret is chart-owned or pre-existing... except that a pre-existing Secret's
content is invisible to Helm. When config.existingSecret is set the checksum
covers the values that still affect the pod, and the annotation carries a marker
saying the Secret's own content is not tracked — so nobody reads a stable
checksum as proof the config did not change.
*/}}
{{- define "fuse.configChecksum" -}}
{{- if .Values.config.existingSecret -}}
{{- printf "external:%s" (include "fuse.configSecretName" . | sha256sum | trunc 16) -}}
{{- else -}}
{{- include "fuse.configYaml" . | sha256sum -}}
{{- end -}}
{{- end -}}

{{/*
================================================================================
GUARDS
================================================================================
*/}}

{{/*
The auth guard. With no loop_server.auth the server synthesizes a
loudly-logged dev token rather than refusing to start (ADR-0034). That is the
right behavior for a binary and the wrong default for a rendered chart: a
production cluster would come up authenticated by a token published in this
repository, and nothing in the manifest would say so. So: say which.
*/}}
{{- define "fuse.guard.auth" -}}
{{- if not (or .Values.auth.tokens .Values.config.existingSecret .Values.auth.allowDevToken) -}}
{{- fail "fuse: no authentication configured. The server would fall back to its built-in dev token — a value published in this repository — and would authenticate the whole cluster with it. Choose one, explicitly:\n  --set-file/--values auth.tokens[0].{token,tenant}   real bearer tokens (ADR-0034)\n  --set config.existingSecret=NAME                     a Secret you manage, holding config.yml\n  --set auth.allowDevToken=true                        the dev token, on purpose, on a throwaway cluster\nSee values.yaml's auth section." -}}
{{- end -}}
{{- end -}}

{{/*
The DSN guard. Without a DSN the server falls back to the filesystem event
store, which lands in the home emptyDir: replicas share nothing, a redeploy
loses every loop, and the pod still passes /readyz because an fsstore in a
writable directory IS healthy. Nothing surfaces the problem at runtime, so the
chart refuses at render time.
*/}}
{{- define "fuse.guard.dsn" -}}
{{- if not (or .Values.postgres.dsn .Values.postgres.existingSecret .Values.postgres.dev.enabled) -}}
{{- fail "fuse: no Postgres DSN configured. The server would fall back to the filesystem event store inside the pod's emptyDir: replicas would share no loops, a redeploy would lose all of them, and /readyz would stay green throughout because a filesystem store in a writable directory is genuinely healthy — so nothing would tell you. Choose one:\n  --set postgres.dsn=postgres://...                 an inline DSN (ends up in a chart-owned Secret)\n  --set postgres.existingSecret=NAME                a Secret you manage\n  --set postgres.dev.enabled=true                   the in-chart DEV-ONLY StatefulSet (not for production)\nSee values.yaml's postgres section." -}}
{{- end -}}
{{- end -}}

{{/*
The sandbox tri-state guard (ADR-0044).

ORDER IS LOAD-BEARING. `kubernetes` is checked and refused BEFORE anything
consults sandbox.kubernetes.minVersion or any #75 manifest, so the operator
reads the real reason — the feature is not in this image — rather than a version
comparison that looks like a misconfiguration they could fix by bumping a value.
*/}}
{{- define "fuse.guard.sandbox" -}}
{{- $mode := .Values.sandbox.mode | toString -}}
{{- if eq $mode "kubernetes" -}}
{{- fail "fuse: sandbox.mode=kubernetes is not yet available in this chart — it requires change #75, which ships the Kubernetes sandbox handler and its RBAC. The released image contains no pod-per-sandbox handler, so rendering RBAC for it would grant pod-create rights to a server that cannot use them. #75 supplies both the manifest and the pinned sandbox.kubernetes.minVersion; until then this mode refuses unconditionally.\nUse sandbox.mode=none (the bash tool reports itself unavailable), or sandbox.mode=docker-socket if you have read ADR-0044 and accept that socket access is approximately host root." -}}
{{- else if eq $mode "docker-socket" -}}
{{- if not .Values.sandbox.dockerSocket.acknowledgeHostRoot -}}
{{- fail "fuse: sandbox.mode=docker-socket requires sandbox.dockerSocket.acknowledgeHostRoot=true.\nADR-0044 is explicit that mounting the node's docker socket into the pod is APPROXIMATELY HOST ROOT: a model-authored bash command can start a privileged container and own the node, and everything else scheduled on it. This flag is not a safety mechanism — it changes nothing about the risk. It exists so the tradeoff is accepted by a human, in a file someone can review, rather than inherited from a default.\nSet it only on a cluster you are willing to lose." -}}
{{- end -}}
{{- else if not (eq $mode "none") -}}
{{- fail (printf "fuse: sandbox.mode must be one of \"none\", \"docker-socket\", or \"kubernetes\" — got %q." $mode) -}}
{{- end -}}
{{- end -}}

{{/*
fuse.guards — every guard, invoked from every top-level template. See the
header: a guard reachable from one template only is not a guard.
*/}}
{{- define "fuse.guards" -}}
{{- include "fuse.guard.auth" . -}}
{{- include "fuse.guard.dsn" . -}}
{{- include "fuse.guard.sandbox" . -}}
{{/* Evaluated for its side effect — fuse.drainSeconds fails on a bad unit.
     Assigned to a discarded variable so the number is not emitted here. */}}
{{- $_ := include "fuse.drainSeconds" . -}}
{{- end -}}
