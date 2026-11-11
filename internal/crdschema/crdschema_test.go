/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package crdschema_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/SpareCores/scarpenter/internal/crdschema"
	"github.com/SpareCores/scarpenter/internal/testutil"
)

func repo(parts ...string) string {
	return filepath.Join(append([]string{testutil.RepoRoot()}, parts...)...)
}

func schema(t *testing.T) (raw []byte, decoded any) {
	t.Helper()
	crd, err := os.ReadFile(repo("config", "crd", "bases", "scarpenter.sparecores.com_scnodeclasses.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err = crdschema.Convert(crd)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return raw, decoded
}

// The published file is what the CRD yields: `make docs-api` regenerates it.
func TestThePublishedSchemaIsTheCRDs(t *testing.T) {
	raw, _ := schema(t)
	got, err := os.ReadFile(repo("docs", "reference", "schema", "scnodeclass.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Error("docs/reference/schema/scnodeclass.schema.json is not the schema of the CRD: run make docs-api")
	}
	if strings.Contains(string(raw), "x-kubernetes") {
		t.Error("the schema keeps an x-kubernetes extension")
	}
}

func example(t *testing.T, path string) any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return doc
}

func TestEveryExampleAndSampleValidatesAgainstTheSchema(t *testing.T) {
	_, s := schema(t)
	var files []string
	for _, glob := range []string{repo("docs", "examples", "*.yaml"), repo("config", "samples", "scarpenter_*.yaml")} {
		m, err := filepath.Glob(glob)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) < 6 {
		t.Fatalf("expected the examples and samples, got %v", files)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			for _, err := range crdschema.Validate(s, example(t, f)) {
				t.Error(err)
			}
		})
	}
}

// The schema catches what a tool that renders a NodeClass gets wrong.
func TestTheSchemaRejectsWhatTheAPIServerRejects(t *testing.T) {
	_, s := schema(t)
	base := func() map[string]any {
		return example(t, repo("docs", "examples", "aws-bring-your-own.yaml")).(map[string]any)
	}
	aws := func(d map[string]any) map[string]any {
		return d["spec"].(map[string]any)["vendors"].(map[string]any)["aws"].(map[string]any)
	}
	for name, tc := range map[string]struct {
		mutate func(d map[string]any)
		want   string
	}{
		"a misspelt field of a selector": {func(d map[string]any) {
			aws(d)["resources"].(map[string]any)["subnets"].(map[string]any)["use"].(map[string]any)["tag"] = map[string]any{"a": "b"}
		}, `unknown field "tag"`},
		"an unknown match attribute": {func(d map[string]any) {
			aws(d)["resources"].(map[string]any)["subnets"].(map[string]any)["use"].(map[string]any)["match"] = map[string]any{"cidrs": "10.0.0.0/8"}
		}, `unknown field "cidrs"`},
		"a select that does not exist": {func(d map[string]any) {
			aws(d)["resources"].(map[string]any)["subnets"].(map[string]any)["use"].(map[string]any)["select"] = "some"
		}, "want one of"},
		"a lifecycle that does not exist": {func(d map[string]any) {
			aws(d)["resources"].(map[string]any)["securityGroups"] = map[string]any{"create": map[string]any{"lifecycle": map[string]any{"policy": "forever"}}}
		}, "want one of"},
		"a missing required field": {func(d map[string]any) { delete(d["spec"].(map[string]any), "kubernetes") }, `required field "kubernetes"`},
		"another kind":             {func(d map[string]any) { d["kind"] = "NodeClass" }, "want SCNodeClass"},
		"an unknown spec field":    {func(d map[string]any) { d["spec"].(map[string]any)["vendor"] = map[string]any{} }, `unknown field "vendor"`},
		"an empty ids list": {func(d map[string]any) {
			aws(d)["resources"].(map[string]any)["securityGroups"].(map[string]any)["use"].(map[string]any)["ids"] = map[string]any{"us-east-1": []any{}}
		}, "fewer than"},
	} {
		t.Run(name, func(t *testing.T) {
			d := base()
			tc.mutate(d)
			errs := crdschema.Validate(s, d)
			if len(errs) == 0 {
				t.Fatal("accepted")
			}
			joined := ""
			for _, e := range errs {
				joined += e.Error() + "\n"
			}
			if !strings.Contains(joined, tc.want) {
				t.Errorf("errors %q do not mention %q", joined, tc.want)
			}
		})
	}
	if errs := crdschema.Validate(s, base()); len(errs) != 0 {
		t.Errorf("the unmodified example: %v", errs)
	}
}
