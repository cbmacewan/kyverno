#!/usr/bin/env bash
set -euo pipefail

namespace=gpol-global-match-conditions
condition_name=conformance-global-label-filter
expression="request.operation != 'UPDATE' || request.resource.group != '' || request.resource.resource != 'namespaces' || object.metadata.name != '$namespace' || object.metadata.?labels.orValue({}) != oldObject.metadata.?labels.orValue({})"
original=$(kubectl -n kyverno get configmap kyverno -o json | jq '.data.matchConditions // null')

cleanup() {
  kubectl -n kyverno patch configmap kyverno --type=merge \
    -p "$(jq -nc --argjson original "$original" '{data:{matchConditions:$original}}')"
  kubectl delete namespace "$namespace" --ignore-not-found --wait=false
}
trap cleanup EXIT

conditions=$(jq -nc --argjson original "$original" --arg name "$condition_name" --arg expression "$expression" \
  '($original // "[]" | fromjson) + [{name:$name,expression:$expression}]')
kubectl -n kyverno patch configmap kyverno --type=merge \
  -p "$(jq -nc --arg conditions "$conditions" '{data:{matchConditions:$conditions}}')"

retry() {
  for ((i=0; i<60; i++)); do
    if "$@"; then return 0; fi
    sleep 1
  done
  printf 'Timed out: %s\n' "$*" >&2
  return 1
}

webhook_has_filter() {
  kubectl get validatingwebhookconfiguration kyverno-resource-validating-webhook-cfg -o json | \
    jq -e --arg name "$condition_name" --arg expression "$expression" \
      '[.webhooks[] | select(.clientConfig.service.path == "/gpol/gpol-global-match-conditions")
        | .matchConditions[] | select(.name == $name and .expression == $expression)] | length > 0' >/dev/null
}
generated_color() {
  [[ "$(kubectl -n "$namespace" get configmap generated -o jsonpath='{.data.color}' 2>/dev/null)" == "$1" ]]
}
downstream_deleted() {
  [[ -z "$(kubectl -n "$namespace" get configmap generated --ignore-not-found -o name)" ]]
}

# Verify the exact UPDATE filter, rather than a CREATE-only adaptation of it.
retry webhook_has_filter
kubectl create -f trigger.yaml
retry generated_color red

# An unchanged-label heartbeat must be skipped by the emitted webhook filter.
kubectl annotate namespace "$namespace" example.com/heartbeat=updated
retry generated_color red

# Changes and removals of relevant labels must still synchronize downstreams.
kubectl label namespace "$namespace" example.com/color=blue --overwrite
retry generated_color blue
kubectl label namespace "$namespace" example.com/color-
retry downstream_deleted
