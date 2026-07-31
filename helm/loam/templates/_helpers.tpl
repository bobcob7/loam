{{/*
Selector labels for the loam Deployment/Service. Matches
deploy/k8s's app.kubernetes.io/name: loam convention exactly, so an
in-place migration from the kustomize set to this chart does not orphan
the existing Deployment/Service/Pods (same selector => same owned
objects).
*/}}
{{- define "loam.selectorLabels" -}}
app.kubernetes.io/name: loam
{{- end -}}

{{/*
Selector labels for the postgres StatefulSet/Service. Same rationale as
loam.selectorLabels above.
*/}}
{{- define "postgres.selectorLabels" -}}
app.kubernetes.io/name: postgres
{{- end -}}

{{/*
Chart-wide label applied to every resource's metadata.labels (and, per
deploy/k8s/kustomization.yaml's `labels: - pairs: ... includeSelectors:
true`, folded into every selector/matchLabels too -- see
loam.selectorLabels/postgres.selectorLabels callers). Deliberately just
this one pair, not the fuller helm.sh/chart + app.kubernetes.io/managed-by
set a stock Helm starter chart would add: those weren't in the kustomize
output this chart replaces, and adding them would be a needless render
diff against it.
*/}}
{{- define "loam.partOfLabel" -}}
app.kubernetes.io/part-of: loam
{{- end -}}

{{/*
Guard: replicaCount must be exactly 1. Enforced here, not left as a
comment, because a chart can actually fail the render where a raw
manifest can only warn. See values.yaml's replicaCount comment for the
full two-part correctness argument (RWO volume multi-attach AND
per-tick repos.sync_state ownership, loam-fp7) this is protecting.
*/}}
{{- define "loam.validateReplicaCount" -}}
{{- if gt (int .Values.replicaCount) 1 -}}
{{- fail "replicaCount must be 1: loam's data PVC is ReadWriteOnce (a second pod cannot mount it), and the sync scheduler assumes single-writer ownership of repos.sync_state per tick (loam-fp7) -- this is a correctness constraint, not a capacity one. Do not raise replicaCount without first solving both." -}}
{{- end -}}
{{- end -}}
