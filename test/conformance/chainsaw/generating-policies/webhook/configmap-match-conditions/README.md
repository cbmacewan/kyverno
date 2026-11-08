This test temporarily appends a global ConfigMap match condition, checks that
the generating webhook contains the exact expression, exercises label changes
and synchronized downstream cleanup, and restores the original ConfigMap value.
It must run serially because the ConfigMap is shared. It requires `kubectl`,
`jq`, and an installed Kyverno built with this fix.

Run the fixture with Chainsaw against a disposable test cluster:

```sh
chainsaw test --test-dir test/conformance/chainsaw/generating-policies/webhook/configmap-match-conditions
```

For a Node generator whose inputs depend only on labels, the corresponding Helm
proof configuration is:

```yaml
config:
  matchConditions:
  - name: node-label-updates
    expression: >-
      request.operation != 'UPDATE' ||
      request.resource.group != '' || request.resource.resource != 'nodes' ||
      object.metadata.?labels.orValue({}) != oldObject.metadata.?labels.orValue({})
```

This conservatively admits every label change, including removal of the last
eligible label, and allows CREATE and DELETE. Annotation-only and status-only
Node updates do not invoke CEL resource webhooks. This is a global filter: use
it only if skipping those Node updates is appropriate for every affected policy.
Generators depending on annotations or status need a different predicate.

The webhook unit tests evaluate the emitted conditions for Node heartbeat,
label-add/change/removal, CREATE, DELETE, and unrelated resource requests. The
CLI does not register admission webhooks, so it cannot reproduce this bug.

The policy and trigger can also be checked with the CLI from this directory:

```sh
kyverno apply policy.yaml --resource trigger.yaml
```

The expected generated ConfigMap is named `generated` with `data.color: red`.
