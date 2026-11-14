package v1alpha1_test

import (
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	celplugin "k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/cel/environment"
	"sigs.k8s.io/yaml"
)

// accessor adapts a policy expression to the API server's compiler.
type accessor struct {
	name, expr string
	ret        *cel.Type
}

func (a accessor) GetExpression() string    { return a.expr }
func (a accessor) ReturnTypes() []*cel.Type { return []*cel.Type{a.ret} }
func (a accessor) GetName() string          { return a.name }

func loadPolicy(t *testing.T) *admissionregistrationv1.ValidatingAdmissionPolicy {
	t.Helper()
	docs := strings.Split(repoFile(t, "config", "policy", "agent-own-node.yaml"), "\n---\n")
	var p admissionregistrationv1.ValidatingAdmissionPolicy
	if err := yaml.UnmarshalStrict([]byte(docs[0]), &p); err != nil {
		t.Fatal(err)
	}
	var b admissionregistrationv1.ValidatingAdmissionPolicyBinding
	if err := yaml.UnmarshalStrict([]byte(docs[1]), &b); err != nil {
		t.Fatal(err)
	}
	if b.Spec.PolicyName != p.Name {
		t.Fatalf("binding names policy %q, not %q", b.Spec.PolicyName, p.Name)
	}
	return &p
}

// The expressions must compile under the API server's own CEL environment, so
// a typo or an unavailable function is caught here and not at admission time.
func TestAgentPolicyCompiles(t *testing.T) {
	p := loadPolicy(t)
	cc, err := celplugin.NewCompositedCompiler(environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion(), true))
	if err != nil {
		t.Fatal(err)
	}
	var vars []celplugin.NamedExpressionAccessor
	for _, v := range p.Spec.Variables {
		vars = append(vars, accessor{name: v.Name, expr: v.Expression, ret: cel.DynType})
	}
	opts := celplugin.OptionalVariableDeclarations{}
	cc.CompileAndStoreVariables(vars, opts, environment.StoredExpressions)
	var conds []celplugin.ExpressionAccessor
	for _, v := range p.Spec.Validations {
		conds = append(conds, accessor{expr: v.Expression, ret: cel.BoolType})
	}
	ev := cc.CompileCondition(conds, opts, environment.StoredExpressions)
	if errs := ev.CompilationErrors(); len(errs) > 0 {
		t.Fatalf("policy does not compile: %v", errs)
	}
}

// And the logic does what the comment says. Variables are evaluated first, then
// each validation, as the API server does.
func TestAgentPolicyBehaviour(t *testing.T) {
	p := loadPolicy(t)
	env, err := cel.NewEnv(
		cel.Variable("request", cel.DynType), cel.Variable("object", cel.DynType),
		cel.Variable("oldObject", cel.DynType), cel.Variable("variables", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	eval := func(expr string, act map[string]any) any {
		ast, iss := env.Compile(expr)
		if iss.Err() != nil {
			t.Fatalf("%s: %v", expr, iss.Err())
		}
		prg, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		out, _, err := prg.Eval(act)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		return out.Value()
	}
	allowed := func(user string, extra map[string]any, objName string, newSpec, oldSpec string) bool {
		act := map[string]any{
			"request":   map[string]any{"userInfo": map[string]any{"username": user, "extra": extra}},
			"object":    map[string]any{"metadata": map[string]any{"name": objName}, "spec": map[string]any{"v": newSpec}},
			"oldObject": map[string]any{"spec": map[string]any{"v": oldSpec}},
		}
		vars := map[string]any{}
		act["variables"] = vars
		for _, v := range p.Spec.Variables {
			vars[v.Name] = eval(v.Expression, act)
		}
		for _, v := range p.Spec.Validations {
			if ok, _ := eval(v.Expression, act).(bool); !ok {
				return false
			}
		}
		return true
	}
	const agent = "system:serviceaccount:raspberrypi-operator-system:raspberrypi-operator-agent"
	node := func(n string) map[string]any {
		return map[string]any{"authentication.kubernetes.io/node-name": []any{n}}
	}
	for _, c := range []struct {
		name  string
		allow bool
		got   bool
	}{
		{"agent updates its own node's status", true, allowed(agent, node("pi-1"), "pi-1", "a", "a")},
		{"agent updates another node's object", false, allowed(agent, node("pi-1"), "pi-2", "a", "a")},
		{"agent without a node claim is denied", false, allowed(agent, map[string]any{}, "pi-1", "a", "a")},
		{"agent changes spec", false, allowed(agent, node("pi-1"), "pi-1", "b", "a")},
		{"the controller is not restricted", true, allowed("system:serviceaccount:raspberrypi-operator-system:raspberrypi-operator-manager", map[string]any{}, "pi-2", "b", "a")},
		{"a human admin is not restricted", true, allowed("alice", map[string]any{}, "pi-2", "b", "a")},
	} {
		if c.got != c.allow {
			t.Errorf("%s: allowed=%v, want %v", c.name, c.got, c.allow)
		}
	}
}
