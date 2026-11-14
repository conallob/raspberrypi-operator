// Package charttest checks the rendered Helm chart against the kustomize
// install in config/, which is the source of truth, so the two cannot drift.
//
// It needs the chart rendered first (`make chart-test` does that); without
// CHART_RENDER_DIR the tests are skipped.
package charttest

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	rpiv1 "github.com/conallob/raspberrypi-operator/api/v1alpha1"
)

type objs []*unstructured.Unstructured

func decode(t *testing.T, data []byte) objs {
	t.Helper()
	var out objs
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var m map[string]any
		if err := dec.Decode(&m); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decoding: %v", err)
		}
		if len(m) == 0 {
			continue
		}
		out = append(out, &unstructured.Unstructured{Object: m})
	}
	return out
}

func rendered(t *testing.T, name string) objs {
	t.Helper()
	dir := os.Getenv("CHART_RENDER_DIR")
	if dir == "" {
		t.Skip("CHART_RENDER_DIR not set; run `make chart-test`")
	}
	b, err := os.ReadFile(filepath.Join(dir, name+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return decode(t, b)
}

func repo(t *testing.T, rel ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, rel...)...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (o objs) of(kind string) objs {
	var out objs
	for _, x := range o {
		if x.GetKind() == kind {
			out = append(out, x)
		}
	}
	return out
}

func (o objs) named(kind, name string) *unstructured.Unstructured {
	for _, x := range o.of(kind) {
		if x.GetName() == name {
			return x
		}
	}
	return nil
}

func kinds(o objs) map[string]int {
	m := map[string]int{}
	for _, x := range o {
		m[x.GetKind()]++
	}
	return m
}

// normalised round-trips through JSON so numbers and types compare equal.
func normalised(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func field(t *testing.T, o *unstructured.Unstructured, path ...string) any {
	t.Helper()
	v, found, err := unstructured.NestedFieldNoCopy(o.Object, path...)
	if err != nil || !found {
		t.Fatalf("%s %s: no %v (err=%v)", o.GetKind(), o.GetName(), path, err)
	}
	return v
}

func TestDefaultInstallHasEverything(t *testing.T) {
	got := kinds(rendered(t, "default"))
	want := map[string]int{
		"CustomResourceDefinition": 2, "ClusterRole": 2, "ClusterRoleBinding": 2, "ServiceAccount": 2,
		"Role": 1, "RoleBinding": 1, "Deployment": 1, "DaemonSet": 1,
		"ValidatingAdmissionPolicy": 1, "ValidatingAdmissionPolicyBinding": 1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rendered kinds = %v, want %v", got, want)
	}
	// The kustomize install has the same objects plus the Namespace, which Helm
	// leaves to --create-namespace.
	k := kinds(decode(t, repo(t, "config", "manager", "manager.yaml")))
	if k["Namespace"] != 1 || k["Deployment"] != 1 {
		t.Fatalf("kustomize manager.yaml unexpected: %v", k)
	}
}

// The chart's CRDs are generated from config/crd; a hand edit or a forgotten
// `make chart-sync` shows up here.
func TestCRDsMatchTheKustomizeInstall(t *testing.T) {
	got := rendered(t, "default").of("CustomResourceDefinition")
	for _, name := range []string{"rpi.conall.dev_bootloaderpolicies.yaml", "rpi.conall.dev_nodebootloaders.yaml"} {
		src := decode(t, repo(t, "config", "crd", name))
		if len(src) != 1 {
			t.Fatalf("%s: %d objects", name, len(src))
		}
		match := got.named("CustomResourceDefinition", src[0].GetName())
		if match == nil {
			t.Fatalf("chart has no CRD %s", src[0].GetName())
		}
		if !reflect.DeepEqual(normalised(t, match.Object["spec"]), normalised(t, src[0].Object["spec"])) {
			t.Errorf("CRD %s differs from config/crd; run `make chart-sync`", src[0].GetName())
		}
		if a := match.GetAnnotations()["helm.sh/resource-policy"]; a != "keep" {
			t.Errorf("CRD %s would be deleted on uninstall (resource-policy=%q)", src[0].GetName(), a)
		}
	}
}

func TestRBACMatchesTheKustomizeInstall(t *testing.T) {
	o := rendered(t, "default")
	mgr := o.named("ClusterRole", "rel-raspberrypi-operator-manager")
	agent := o.named("ClusterRole", "rel-raspberrypi-operator-agent")
	if mgr == nil || agent == nil {
		t.Fatal("missing ClusterRoles")
	}
	wantMgr := decode(t, repo(t, "config", "rbac", "role.yaml"))[0]
	if !reflect.DeepEqual(normalised(t, mgr.Object["rules"]), normalised(t, wantMgr.Object["rules"])) {
		t.Error("manager rules differ from config/rbac/role.yaml; run `make chart-sync`")
	}
	var wantAgent *unstructured.Unstructured
	for _, x := range decode(t, repo(t, "config", "rbac", "bindings.yaml")) {
		if x.GetKind() == "ClusterRole" && x.GetName() == "raspberrypi-operator-agent" {
			wantAgent = x
		}
	}
	if wantAgent == nil {
		t.Fatal("no agent ClusterRole in config/rbac/bindings.yaml")
	}
	if !reflect.DeepEqual(normalised(t, agent.Object["rules"]), normalised(t, wantAgent.Object["rules"])) {
		t.Error("agent rules differ from config/rbac/bindings.yaml")
	}
}

// Every reference between objects must resolve, in the default release and when
// renamed or installed in another namespace.
func TestReferencesResolve(t *testing.T) {
	for _, c := range []struct{ file, ns, prefix string }{
		{"default", "pis-system", "rel-raspberrypi-operator"},
		{"renamed", "other-ns", "pi-op"},
		{"custom-image", "pis-system", "rel-raspberrypi-operator"},
	} {
		t.Run(c.file, func(t *testing.T) {
			o := rendered(t, c.file)
			for _, sa := range []string{c.prefix + "-manager", c.prefix + "-agent"} {
				if x := o.named("ServiceAccount", sa); x == nil || x.GetNamespace() != c.ns {
					t.Errorf("ServiceAccount %s missing from namespace %s", sa, c.ns)
				}
			}
			for _, kind := range []string{"Deployment", "DaemonSet"} {
				x := o.of(kind)[0]
				sa := field(t, x, "spec", "template", "spec", "serviceAccountName").(string)
				if o.named("ServiceAccount", sa) == nil {
					t.Errorf("%s uses ServiceAccount %q that does not exist", kind, sa)
				}
				if x.GetNamespace() != c.ns {
					t.Errorf("%s is in namespace %q, want %q", kind, x.GetNamespace(), c.ns)
				}
			}
			for _, kind := range []string{"ClusterRoleBinding", "RoleBinding"} {
				for _, b := range o.of(kind) {
					ref := field(t, b, "roleRef", "name").(string)
					rk := field(t, b, "roleRef", "kind").(string)
					if o.named(rk, ref) == nil {
						t.Errorf("%s %s binds missing %s %s", kind, b.GetName(), rk, ref)
					}
					for _, s := range field(t, b, "subjects").([]any) {
						sm := s.(map[string]any)
						if o.named("ServiceAccount", sm["name"].(string)) == nil || sm["namespace"] != c.ns {
							t.Errorf("%s %s has a subject that does not exist: %v", kind, b.GetName(), sm)
						}
					}
				}
			}
			// The admission policy names the agent by its service account user;
			// if that drifts from the real account the policy silently stops
			// restricting (or blocks) the agent.
			want := "system:serviceaccount:" + c.ns + ":" + c.prefix + "-agent"
			vap := o.of("ValidatingAdmissionPolicy")[0]
			var isAgent string
			for _, v := range field(t, vap, "spec", "variables").([]any) {
				if v.(map[string]any)["name"] == "isAgent" {
					isAgent = v.(map[string]any)["expression"].(string)
				}
			}
			if !strings.Contains(isAgent, "'"+want+"'") {
				t.Errorf("admission policy expects another user than %q:\n%s", want, isAgent)
			}
			if pn := field(t, o.of("ValidatingAdmissionPolicyBinding")[0], "spec", "policyName"); pn != vap.GetName() {
				t.Errorf("binding names policy %v, not %s", pn, vap.GetName())
			}
		})
	}
}

// The chart's admission policy is the one in config/policy with only the agent's
// user parameterised.
func TestAdmissionPolicyMatchesTheKustomizeOne(t *testing.T) {
	flat := func(v any) string { return strings.Join(strings.Fields(v.(string)), " ") }
	got := rendered(t, "default").of("ValidatingAdmissionPolicy")[0]
	var want *unstructured.Unstructured
	for _, x := range decode(t, repo(t, "config", "policy", "agent-own-node.yaml")) {
		if x.GetKind() == "ValidatingAdmissionPolicy" {
			want = x
		}
	}
	gv, wv := field(t, got, "spec", "validations").([]any), field(t, want, "spec", "validations").([]any)
	if !reflect.DeepEqual(normalised(t, gv), normalised(t, wv)) {
		t.Error("validations differ from config/policy/agent-own-node.yaml")
	}
	for _, name := range []string{"nodeName"} {
		var a, b string
		for _, v := range field(t, got, "spec", "variables").([]any) {
			if v.(map[string]any)["name"] == name {
				a = flat(v.(map[string]any)["expression"])
			}
		}
		for _, v := range field(t, want, "spec", "variables").([]any) {
			if v.(map[string]any)["name"] == name {
				b = flat(v.(map[string]any)["expression"])
			}
		}
		if a == "" || a != b {
			t.Errorf("variable %s differs:\n chart: %s\n kustomize: %s", name, a, b)
		}
	}
	if !reflect.DeepEqual(normalised(t, field(t, got, "spec", "matchConstraints")), normalised(t, field(t, want, "spec", "matchConstraints"))) {
		t.Error("matchConstraints differ from config/policy/agent-own-node.yaml")
	}
}

func TestImageDefaultsAndOverrides(t *testing.T) {
	check := func(file, want string) {
		t.Helper()
		o := rendered(t, file)
		for _, kind := range []string{"Deployment", "DaemonSet"} {
			cs := field(t, o.of(kind)[0], "spec", "template", "spec", "containers").([]any)
			if got := cs[0].(map[string]any)["image"]; got != want {
				t.Errorf("%s/%s image = %v, want %s", file, kind, got, want)
			}
		}
	}
	// A source checkout's appVersion is "latest"; releases set it from the tag.
	check("default", "ghcr.io/conallob/raspberrypi-operator:latest")
	check("custom-image", "registry.example/pi:1.2.3")
}

// Both pods must be able to run only on the architecture the image supports.
func TestWorkloadsAreArm64Only(t *testing.T) {
	o := rendered(t, "default")
	for _, kind := range []string{"Deployment", "DaemonSet"} {
		ns := field(t, o.of(kind)[0], "spec", "template", "spec", "nodeSelector").(map[string]any)
		if ns["kubernetes.io/arch"] != "arm64" {
			t.Errorf("%s nodeSelector = %v", kind, ns)
		}
	}
}

func TestOptionalPartsCanBeTurnedOff(t *testing.T) {
	got := kinds(rendered(t, "minimal"))
	for _, k := range []string{"CustomResourceDefinition", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"} {
		if got[k] != 0 {
			t.Errorf("%s rendered although disabled", k)
		}
	}
	if got["Deployment"] != 1 || got["DaemonSet"] != 1 {
		t.Errorf("workloads missing when optional parts are off: %v", got)
	}
}

// A policy declared in values must be the sample policy, and a valid
// BootloaderPolicy as far as the Go API is concerned.
func TestPolicyFromValues(t *testing.T) {
	pol := rendered(t, "policy").of("BootloaderPolicy")
	if len(pol) != 1 {
		t.Fatalf("%d BootloaderPolicies rendered", len(pol))
	}
	b, err := yaml.Marshal(pol[0].Object)
	if err != nil {
		t.Fatal(err)
	}
	var typed rpiv1.BootloaderPolicy
	if err := yaml.UnmarshalStrict(b, &typed); err != nil {
		t.Fatalf("not a valid BootloaderPolicy: %v", err)
	}
	sample := decode(t, repo(t, "config", "samples", "bootloaderpolicy.yaml"))[0]
	if !reflect.DeepEqual(normalised(t, pol[0].Object["spec"]), normalised(t, sample.Object["spec"])) {
		t.Error("ci/policy-values.yaml has drifted from config/samples/bootloaderpolicy.yaml")
	}
	if got := kinds(rendered(t, "default")); got["BootloaderPolicy"] != 0 {
		t.Error("a BootloaderPolicy is rendered by default")
	}
}

// values.schema.json turns a mistaken value into an install-time error.
func TestSchemaRejectsMistakes(t *testing.T) {
	dir := os.Getenv("CHART_RENDER_DIR")
	if dir == "" {
		t.Skip("CHART_RENDER_DIR not set")
	}
	b, err := os.ReadFile(filepath.Join(dir, "schema-rejections.txt"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(strings.ReplaceAll(string(b), "\n", " "))
	if len(lines) < 20 {
		t.Fatalf("expected at least 10 cases, got %q", string(b))
	}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !strings.HasPrefix(l, "rejected ") {
			t.Errorf("schema accepted a bad value: %s", l)
		}
	}
}
