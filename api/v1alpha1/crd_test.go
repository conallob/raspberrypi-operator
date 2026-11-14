package v1alpha1_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	celschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/listtype"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"

	rpiv1 "github.com/conallob/raspberrypi-operator/api/v1alpha1"
)

// These tests check the generated CRDs the way the API server does: the CRD
// itself must be acceptable (including CEL compilation and cost limits), and
// manifests must pass schema validation, CEL rules, list-map uniqueness and
// unknown-field detection. No cluster is needed.

type crd struct {
	t          *testing.T
	structural *structuralschema.Structural
	validator  func(obj map[string]any) field.ErrorList
}

func repoFile(t *testing.T, rel ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, rel...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func loadCRD(t *testing.T, file string) *crd {
	t.Helper()
	var v1crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict([]byte(repoFile(t, "config", "crd", file)), &v1crd); err != nil {
		t.Fatal(err)
	}
	var internal apiextensions.CustomResourceDefinition
	if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&v1crd, &internal, nil); err != nil {
		t.Fatal(err)
	}
	// The API server fills this in when it creates the CRD.
	internal.Status.StoredVersions = []string{internal.Spec.Versions[0].Name}
	// The CRD must be accepted: this compiles every CEL rule and checks cost.
	if errs := crdvalidation.ValidateCustomResourceDefinition(context.Background(), &internal); len(errs) > 0 {
		t.Fatalf("CRD %s is not acceptable to the API server: %v", file, errs)
	}
	// Conversion hoists a schema shared by all versions to Spec.Validation.
	props := internal.Spec.Validation.OpenAPIV3Schema
	if props == nil {
		props = internal.Spec.Versions[0].Schema.OpenAPIV3Schema
	}
	s, err := structuralschema.NewStructural(props)
	if err != nil {
		t.Fatal(err)
	}
	sv, _, err := apiservervalidation.NewSchemaValidator(props)
	if err != nil {
		t.Fatal(err)
	}
	c := &crd{t: t, structural: s}
	c.validator = func(obj map[string]any) field.ErrorList {
		return apiservervalidation.ValidateCustomResource(nil, obj, sv)
	}
	return c
}

// check returns every problem the API server would report for obj, given the
// previous object (nil for a create).
func (c *crd) check(obj, old map[string]any) []string {
	c.t.Helper()
	var problems []string
	add := func(errs field.ErrorList) {
		for _, e := range errs {
			problems = append(problems, e.Error())
		}
	}
	// Unknown fields: the API server silently prunes these, which is exactly
	// how a typo'd field goes unnoticed. kubectl's strict validation rejects
	// them; here we detect them directly.
	cp := deepCopy(obj)
	unknown := pruning.PruneWithOptions(cp, c.structural, true, structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true})
	for _, u := range unknown {
		problems = append(problems, "unknown field: "+u)
	}
	add(c.validator(obj))
	add(listtype.ValidateListSetsAndMaps(nil, c.structural, obj))
	celErrs, _ := celschema.NewValidator(c.structural, true, celconfig.PerCallLimit).
		Validate(context.Background(), nil, c.structural, obj, asOld(old), celconfig.RuntimeCELCostBudget)
	add(celErrs)
	return problems
}

func asOld(old map[string]any) any {
	if old == nil {
		return nil
	}
	return old
}

func deepCopy(in map[string]any) map[string]any {
	b, _ := json.Marshal(in)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func sample(t *testing.T) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := yaml.Unmarshal([]byte(repoFile(t, "config", "samples", "bootloaderpolicy.yaml")), &obj); err != nil {
		t.Fatal(err)
	}
	// yaml.Unmarshal yields float64 numbers; the API server sees int64.
	b, _ := json.Marshal(obj)
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func mutated(t *testing.T, f func(spec map[string]any)) map[string]any {
	t.Helper()
	obj := sample(t)
	f(obj["spec"].(map[string]any))
	return obj
}

func TestCRDsAreAcceptable(t *testing.T) {
	loadCRD(t, "rpi.conallob.dev_bootloaderpolicies.yaml")
	loadCRD(t, "rpi.conallob.dev_nodebootloaders.yaml")
}

func TestSampleIsValid(t *testing.T) {
	c := loadCRD(t, "rpi.conallob.dev_bootloaderpolicies.yaml")
	if p := c.check(sample(t), nil); len(p) > 0 {
		t.Fatalf("sample manifest rejected: %v", p)
	}
	// And it decodes strictly into the Go type: no field the Go API doesn't know.
	var pol rpiv1.BootloaderPolicy
	if err := yaml.UnmarshalStrict([]byte(repoFile(t, "config", "samples", "bootloaderpolicy.yaml")), &pol); err != nil {
		t.Fatalf("strict decode: %v", err)
	}
	if len(pol.Spec.Config.BootOrder) == 0 || len(pol.Spec.Firmware.Images) != 2 {
		t.Errorf("decoded sample looks wrong: %+v", pol.Spec)
	}
}

func cfg(spec map[string]any) map[string]any { return spec["config"].(map[string]any) }
func images(spec map[string]any) []any {
	return spec["firmware"].(map[string]any)["images"].([]any)
}

func TestInvalidManifestsAreRejected(t *testing.T) {
	c := loadCRD(t, "rpi.conallob.dev_bootloaderpolicies.yaml")
	cases := []struct {
		name   string
		mutate func(spec map[string]any)
		want   string // substring of an expected problem
	}{
		{"typo in a field name", func(s map[string]any) { cfg(s)["bootOrdr"] = []any{"sd"} }, "unknown field"},
		{"typo in a nested field", func(s map[string]any) { images(s)[0].(map[string]any)["sha265"] = "x" }, "unknown field"},
		{"unknown boot source", func(s map[string]any) { cfg(s)["bootOrder"] = []any{"floppy", "restart"} }, "bootOrder"},
		{"hex boot order no longer accepted", func(s map[string]any) { cfg(s)["bootOrder"] = "0xf146" }, "bootOrder"},
		{"repeated boot source", func(s map[string]any) { cfg(s)["bootOrder"] = []any{"sd", "sd", "restart"} }, "listed only once"},
		{"nothing bootable", func(s map[string]any) { cfg(s)["bootOrder"] = []any{"rpiboot", "restart"} }, "at least one bootable"},
		{"too many entries", func(s map[string]any) {
			cfg(s)["bootOrder"] = []any{"sd", "usb", "nvme", "http", "network", "rpiboot", "bcm-usb", "stop", "restart"}
		}, "bootOrder"},
		{"BOOT_ORDER in settings", func(s map[string]any) { cfg(s)["settings"].(map[string]any)["BOOT_ORDER"] = "0x1" }, "bootOrder, not in settings"},
		{"self-update in settings", func(s map[string]any) { cfg(s)["settings"].(map[string]any)["ENABLE_SELF_UPDATE"] = "0" }, "ENABLE_SELF_UPDATE"},
		{"lower-case setting name", func(s map[string]any) { cfg(s)["settings"].(map[string]any)["sd_boot_max_retries"] = "3" }, "upper-case"},
		{"setting value with a newline", func(s map[string]any) {
			cfg(s)["settings"].(map[string]any)["SD_BOOT_MAX_RETRIES"] = "3\nBOOT_ORDER=0x1"
		}, "settings"},
		{"typed and untyped duplicate", func(s map[string]any) {
			cfg(s)["powerOffOnHalt"] = true
			cfg(s)["settings"].(map[string]any)["POWER_OFF_ON_HALT"] = "0"
		}, "both as powerOffOnHalt"},
		{"typed field with a string", func(s map[string]any) { cfg(s)["bootUART"] = "yes" }, "bootUART"},
		{"duplicate chip", func(s map[string]any) {
			images(s)[1].(map[string]any)["chip"] = "bcm2711"
		}, "Duplicate value"},
		{"unknown chip", func(s map[string]any) { images(s)[0].(map[string]any)["chip"] = "bcm2837" }, "chip"},
		{"more images than chips", func(s map[string]any) {
			s["firmware"].(map[string]any)["images"] = append(images(s), images(s)[0], images(s)[1])
		}, "images"},
		{"http url", func(s map[string]any) { images(s)[0].(map[string]any)["url"] = "http://example.com/x.bin" }, "url"},
		{"short sha256", func(s map[string]any) { images(s)[0].(map[string]any)["sha256"] = "abc" }, "sha256"},
		{"upper-case sha256", func(s map[string]any) {
			images(s)[0].(map[string]any)["sha256"] = strings.Repeat("A", 64)
		}, "sha256"},
		{"missing build timestamp", func(s map[string]any) { delete(images(s)[0].(map[string]any), "buildTimestamp") }, "buildTimestamp"},
		{"zero max unavailable", func(s map[string]any) { s["rollout"].(map[string]any)["maxUnavailable"] = int64(0) }, "maxUnavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problems := c.check(mutated(t, tc.mutate), nil)
			if len(problems) == 0 {
				t.Fatal("accepted, want rejected")
			}
			if !strings.Contains(strings.Join(problems, "\n"), tc.want) {
				t.Fatalf("rejected, but not for %q:\n%s", tc.want, strings.Join(problems, "\n"))
			}
			t.Logf("rejected: %s", problems[0])
		})
	}
}

func TestDowngradeIsRefusedOnUpdate(t *testing.T) {
	c := loadCRD(t, "rpi.conallob.dev_bootloaderpolicies.yaml")
	old := sample(t)
	older := func(allow bool, chip string) map[string]any {
		return mutated(t, func(s map[string]any) {
			for _, i := range images(s) {
				im := i.(map[string]any)
				if im["chip"] == chip {
					im["buildTimestamp"] = im["buildTimestamp"].(int64) - 1000
				}
			}
			if allow {
				s["safety"] = map[string]any{"allowDowngrade": true}
			}
		})
	}
	if p := c.check(older(false, "bcm2712"), old); !strings.Contains(strings.Join(p, "\n"), "downgrade") {
		t.Errorf("downgrade accepted: %v", p)
	}
	if p := c.check(older(true, "bcm2712"), old); len(p) > 0 {
		t.Errorf("explicit downgrade rejected: %v", p)
	}
	// Creating a policy, or moving forward, is never a downgrade.
	if p := c.check(older(false, "bcm2712"), nil); len(p) > 0 {
		t.Errorf("create rejected: %v", p)
	}
	newer := mutated(t, func(s map[string]any) {
		for _, i := range images(s) {
			im := i.(map[string]any)
			im["buildTimestamp"] = im["buildTimestamp"].(int64) + 1000
		}
	})
	if p := c.check(newer, old); len(p) > 0 {
		t.Errorf("upgrade rejected: %v", p)
	}
}
