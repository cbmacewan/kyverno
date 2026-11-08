package webhook

import (
	"encoding/json"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	policiesv1beta1 "github.com/kyverno/api/api/policies.kyverno.io/v1beta1"
	"github.com/kyverno/kyverno/pkg/config"
	engineapi "github.com/kyverno/kyverno/pkg/engine/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

const nodeLabelUpdateFilter = `request.operation != 'UPDATE' || request.resource.group != '' || request.resource.resource != 'nodes' || object.metadata.?labels.orValue({}) != oldObject.metadata.?labels.orValue({})`

func globalMatchConditionPolicy(kind string, conditions []admissionregistrationv1.MatchCondition) engineapi.GenericPolicy {
	constraints := &admissionregistrationv1.MatchResources{
		ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{RuleWithOperations: admissionregistrationv1.RuleWithOperations{
			Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
			Rule:       admissionregistrationv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"nodes"}},
		}}},
	}
	metadata := metav1.ObjectMeta{Name: "node-label-generator"}
	switch kind {
	case "GeneratingPolicy", "NamespacedGeneratingPolicy":
		spec := policiesv1beta1.GeneratingPolicySpec{
			MatchConstraints: constraints, MatchConditions: conditions,
			EvaluationConfiguration: &policiesv1beta1.GeneratingPolicyEvaluationConfiguration{
				SynchronizationConfiguration: &policiesv1beta1.SynchronizationConfiguration{Enabled: ptr.To(true)},
			},
		}
		if kind == "GeneratingPolicy" {
			return engineapi.NewGeneratingPolicy(&policiesv1beta1.GeneratingPolicy{ObjectMeta: metadata, Spec: spec})
		}
		metadata.Namespace = "default"
		return engineapi.NewNamespacedGeneratingPolicy(&policiesv1beta1.NamespacedGeneratingPolicy{ObjectMeta: metadata, Spec: spec})
	case "ValidatingPolicy":
		return engineapi.NewValidatingPolicy(&policiesv1beta1.ValidatingPolicy{ObjectMeta: metadata, Spec: policiesv1beta1.ValidatingPolicySpec{MatchConstraints: constraints, MatchConditions: conditions}})
	default:
		panic("unsupported test policy kind")
	}
}

func globalMatchConditionWebhooks(t *testing.T, cfg config.Configuration, policy engineapi.GenericPolicy, conditions []admissionregistrationv1.MatchCondition) []admissionregistrationv1.ValidatingWebhook {
	t.Helper()
	cache := NewExpressionCache()
	cache.AddPolicyExpressions(conditions)
	return buildWebhookRules(cfg, "", config.GeneratingPolicyWebhookName, "/gpol", 443, nil, []engineapi.GenericPolicy{policy}, cache)
}

func TestBuildWebhookRules_ConfigMapMatchConditions(t *testing.T) {
	t.Parallel()
	global := []admissionregistrationv1.MatchCondition{{Name: "node-label-updates", Expression: nodeLabelUpdateFilter}}
	encoded, err := json.Marshal(global)
	require.NoError(t, err)
	for _, kind := range []string{"GeneratingPolicy", "NamespacedGeneratingPolicy", "ValidatingPolicy"} {
		for _, fineGrained := range []bool{false, true} {
			t.Run(kind+"/"+map[bool]string{false: "basic", true: "fine-grained"}[fineGrained], func(t *testing.T) {
				t.Parallel()
				var policyConditions []admissionregistrationv1.MatchCondition
				if fineGrained {
					policyConditions = []admissionregistrationv1.MatchCondition{{Name: "enabled", Expression: `object.metadata.?labels["enabled"].orValue("") == "true"`}}
				}
				cfg := config.NewDefaultConfiguration(false)
				cfg.Load(&corev1.ConfigMap{Data: map[string]string{"matchConditions": string(encoded)}})
				policy := globalMatchConditionPolicy(kind, policyConditions)
				webhooks := globalMatchConditionWebhooks(t, cfg, policy, policyConditions)
				require.Len(t, webhooks, 1)
				require.Len(t, webhooks[0].MatchConditions, 1+len(policyConditions))
				assert.Equal(t, global[0], webhooks[0].MatchConditions[0], "global filters must not be rewritten to CREATE-only filters")
				if fineGrained {
					expected := policyConditions[0].Expression
					if kind != "ValidatingPolicy" {
						expected = "request.operation != 'CREATE' || (" + expected + ")"
					}
					assert.Equal(t, expected, webhooks[0].MatchConditions[1].Expression)
				}
				assert.Equal(t, global, cfg.GetMatchConditions(), "building webhooks must not modify shared configuration")
				cfg.Load(nil)
				rebuilt := globalMatchConditionWebhooks(t, cfg, policy, policyConditions)
				require.Len(t, rebuilt, 1)
				assert.Len(t, rebuilt[0].MatchConditions, len(policyConditions), "removing ConfigMap filters must remove them from rebuilt webhooks")
			})
		}
	}
}

func TestGlobalNodeFilterPreservesGenerationLifecycle(t *testing.T) {
	t.Parallel()
	global := []admissionregistrationv1.MatchCondition{{Name: "node-label-updates", Expression: nodeLabelUpdateFilter}}
	encoded, err := json.Marshal(global)
	require.NoError(t, err)
	cfg := config.NewDefaultConfiguration(false)
	cfg.Load(&corev1.ConfigMap{Data: map[string]string{"matchConditions": string(encoded)}})
	eligibility := []admissionregistrationv1.MatchCondition{{Name: "enabled", Expression: `object.metadata.?labels["enabled"].orValue("") == "true"`}}
	webhooks := globalMatchConditionWebhooks(t, cfg, globalMatchConditionPolicy("GeneratingPolicy", eligibility), eligibility)
	require.Len(t, webhooks, 1)
	env, err := cel.NewEnv(cel.OptionalTypes(), cel.Variable("request", cel.DynType), cel.Variable("object", cel.DynType), cel.Variable("oldObject", cel.DynType))
	require.NoError(t, err)
	programs := make([]cel.Program, 0, len(webhooks[0].MatchConditions))
	for _, condition := range webhooks[0].MatchConditions {
		compiled := NewExpressionCache().GetOrCompile(condition)
		require.True(t, compiled.isValid, "must compile in the Kubernetes webhook environment: %v", compiled.errors)
		ast, issues := env.Compile(condition.Expression)
		require.NoError(t, issues.Err())
		program, err := env.Program(ast)
		require.NoError(t, err)
		programs = append(programs, program)
	}
	node := func(labels map[string]any, heartbeat string) map[string]any {
		metadata := map[string]any{"annotations": map[string]any{"example.com/heartbeat": heartbeat}}
		if labels != nil {
			metadata["labels"] = labels
		}
		return map[string]any{"metadata": metadata}
	}
	enabled := map[string]any{"enabled": "true", "egress-ip": "192.0.2.10"}
	tests := []struct {
		name, operation, resource string
		old, current              any
		want                      bool
	}{
		{"heartbeat only", "UPDATE", "nodes", node(enabled, "old"), node(enabled, "new"), false},
		{"identical update", "UPDATE", "nodes", node(enabled, "old"), node(enabled, "old"), false},
		{"network input changed", "UPDATE", "nodes", node(enabled, "old"), node(map[string]any{"enabled": "true", "egress-ip": "192.0.2.11"}, "new"), true},
		{"label removed reaches cleanup", "UPDATE", "nodes", node(enabled, "old"), node(map[string]any{"egress-ip": "192.0.2.10"}, "new"), true},
		{"all labels removed reaches cleanup", "UPDATE", "nodes", node(enabled, "old"), node(nil, "new"), true},
		{"label added", "UPDATE", "nodes", node(nil, "old"), node(enabled, "new"), true},
		{"unlabelled heartbeat", "UPDATE", "nodes", node(nil, "old"), node(nil, "new"), false},
		{"eligible create", "CREATE", "nodes", nil, node(enabled, "new"), true},
		{"ineligible create", "CREATE", "nodes", nil, node(nil, "new"), false},
		{"trigger deletion", "DELETE", "nodes", node(enabled, "old"), nil, true},
		{"other resource unchanged", "UPDATE", "configmaps", node(nil, "old"), node(nil, "new"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			allowed := true
			for _, program := range programs {
				result, _, err := program.Eval(map[string]any{"request": map[string]any{"operation": tt.operation, "resource": map[string]any{"group": "", "resource": tt.resource}}, "object": tt.current, "oldObject": tt.old})
				require.NoError(t, err)
				if result == types.False {
					allowed = false
					break
				}
				require.Equal(t, types.True, result)
			}
			assert.Equal(t, tt.want, allowed)
		})
	}
}

func TestMergeWebhookMatchConditions(t *testing.T) {
	t.Parallel()
	global := []admissionregistrationv1.MatchCondition{{Name: "shared", Expression: "request.operation != 'UPDATE'"}}
	policy := []admissionregistrationv1.MatchCondition{{Name: "shared", Expression: "object.metadata.name != 'blocked'"}, {Name: "policy-only", Expression: "true"}}
	merged := mergeWebhookMatchConditions(global, policy)
	require.Len(t, merged, 2)
	assert.Equal(t, "(request.operation != 'UPDATE') && (object.metadata.name != 'blocked')", merged[0].Expression)
	assert.Equal(t, policy[1], merged[1])
	assert.Equal(t, "request.operation != 'UPDATE'", global[0].Expression)
	assert.Equal(t, "object.metadata.name != 'blocked'", policy[0].Expression)
	assert.Equal(t, global, mergeWebhookMatchConditions(global, global))
	assert.Nil(t, mergeWebhookMatchConditions(nil, nil))
}
