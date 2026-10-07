// This is its own module so the Kubernetes API types it needs never enter the
// main module's dependencies. It checks the manifests offline, without any
// cluster: every document decodes strictly into its API type, and the
// container's arguments and probe paths are ones the binary actually has.
package validate

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func documents(t *testing.T) [][]byte {
	b, err := os.ReadFile("../benchgrid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for _, d := range bytes.Split(b, []byte("\n---\n")) {
		if len(bytes.TrimSpace(d)) > 0 {
			out = append(out, d)
		}
	}
	return out
}

func TestManifestsDecodeStrictly(t *testing.T) {
	kinds := map[string]bool{}
	for _, d := range documents(t) {
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal(d, &meta); err != nil {
			t.Fatal(err)
		}
		var target any
		switch meta.Kind {
		case "Deployment":
			target = &appsv1.Deployment{}
		case "Service":
			target = &corev1.Service{}
		case "PersistentVolumeClaim":
			target = &corev1.PersistentVolumeClaim{}
		default:
			t.Fatalf("unexpected kind %q", meta.Kind)
		}
		if err := yaml.UnmarshalStrict(d, target); err != nil {
			t.Errorf("%s: %v", meta.Kind, err)
		}
		kinds[meta.Kind] = true
	}
	if len(kinds) != 3 {
		t.Errorf("kinds %v", kinds)
	}
}

// The arguments in the manifest must be flags the binary defines, and the
// readiness path one the server serves. Both are read from the source rather
// than listed here, so renaming a flag fails this test instead of a rollout.
func TestManifestMatchesTheBinary(t *testing.T) {
	src, err := os.ReadFile("../../../cmd/benchgrid/main.go")
	if err != nil {
		t.Fatal(err)
	}
	flags := map[string]bool{}
	for _, m := range regexp.MustCompile(`flag\.\w+\("([a-z-]+)"`).FindAllSubmatch(src, -1) {
		flags[string(m[1])] = true
	}
	server, err := os.ReadFile("../../../internal/server/server.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range documents(t) {
		var dep appsv1.Deployment
		if yaml.Unmarshal(d, &dep) != nil || dep.Kind != "Deployment" {
			continue
		}
		c := dep.Spec.Template.Spec.Containers[0]
		for _, a := range c.Args {
			if strings.HasPrefix(a, "-") && !flags[strings.TrimLeft(a, "-")] {
				t.Errorf("argument %s is not a flag of cmd/benchgrid", a)
			}
		}
		path := c.ReadinessProbe.HTTPGet.Path
		if !bytes.Contains(server, []byte(`"GET `+path+`"`)) {
			t.Errorf("readiness path %s is not served", path)
		}
		if dep.Spec.Template.Spec.Containers[0].Env[0].Name != "BENCHGRID_DATABASE_URL" || !bytes.Contains(src, []byte(`"BENCHGRID_DATABASE_URL"`)) {
			t.Error("database URL is not passed the way the binary reads it")
		}
	}
}
