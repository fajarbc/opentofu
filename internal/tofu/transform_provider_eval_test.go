// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package tofu

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/dag"
)

// TestEvalProviderFunctionTransformer verifies that unreferenced providers in required_providers
// are tracked in ProviderFunctionTracker and retained during graph pruning for console eval.
func TestEvalProviderFunctionTransformer(t *testing.T) {
	mod := testModuleInline(t, map[string]string{
		"main.tf": `
terraform {
  required_providers {
    test = {
      source = "hashicorp/test"
    }
  }
}
`,
	})

	concrete := func(a *NodeAbstractProvider) dag.Vertex {
		return &NodeEvalableProvider{NodeAbstractProvider: a}
	}

	g := testProviderTransformerGraph(t, mod)

	tracker := make(ProviderFunctionMapping)
	tf := GraphTransformMulti(
		&ProviderConfigTransformer{Config: mod, Concrete: concrete, Operation: walkEval},
		&MissingProviderTransformer{Config: mod, Concrete: concrete},
		&ProviderTransformer{Config: mod},
		&ProviderUnconfiguredTransformer{},
		&ProviderFunctionTransformer{Config: mod, ProviderFunctionTracker: tracker},
		&EvalProviderFunctionTransformer{Config: mod, ProviderFunctionTracker: tracker},
		&PruneEvalProviderTransformer{ProviderFunctionTracker: tracker},
	)

	if err := tf.Transform(t.Context(), g); err != nil {
		t.Fatalf("unexpected transform error: %s", err)
	}

	key := ProviderFunctionReference{
		ModulePath:   addrs.RootModule.String(),
		ProviderName: "test",
	}
	if _, ok := tracker[key]; !ok {
		t.Fatalf("expected provider 'test' to be tracked in ProviderFunctionTracker")
	}

	expected := `provider["registry.opentofu.org/hashicorp/test"]`
	actual := strings.TrimSpace(g.String())
	if diff := cmp.Diff(actual, expected); diff != "" {
		t.Fatalf("expected provider node to be retained:\n%s", diff)
	}
}
