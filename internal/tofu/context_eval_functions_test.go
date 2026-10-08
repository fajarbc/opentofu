// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package tofu

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/plugins"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/zclconf/go-cty/cty"
)

// TestContextEval_providerFunctionsUnreferenced verifies that provider-defined functions
// can be evaluated in tofu console (ctx.Eval) when a provider is declared in required_providers
// even if the configuration does not already call the function statically (Issue #4631).
func TestContextEval_providerFunctionsUnreferenced(t *testing.T) {
	p := testProvider("test")
	p.GetProviderSchemaResponse.Functions = map[string]providers.FunctionSpec{
		"echo": {
			Parameters: []providers.FunctionParameterSpec{{
				Name: "input",
				Type: cty.String,
			}},
			Return: cty.String,
		},
	}
	p.CallFunctionResponse = &providers.CallFunctionResponse{
		Result: cty.StringVal("hello from test provider"),
	}

	m := testModuleInline(t, map[string]string{
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

	ctx := testContext2(t, &ContextOpts{
		Plugins: plugins.NewLibrary(map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		}, nil),
	})

	scope, diags := ctx.Eval(t.Context(), m, states.NewState(), addrs.RootModuleInstance, &EvalOpts{})
	assertNoErrors(t, diags)

	expr, hclDiags := hclsyntax.ParseExpression([]byte(`provider::test::echo("input")`), "<test-input>", hcl.Pos{Line: 1, Column: 1})
	if hclDiags.HasErrors() {
		t.Fatalf("unexpected parse errors: %s", hclDiags.Error())
	}

	got, evalDiags := scope.EvalExpr(t.Context(), expr, cty.DynamicPseudoType)
	assertNoErrors(t, evalDiags)
	if got.AsString() != "hello from test provider" {
		t.Fatalf("expected 'hello from test provider', got %q", got.AsString())
	}
}

// TestContextEval_providerFunctionsWithAlias verifies that provider-defined functions
// on an aliased provider configuration can be called in console eval when unreferenced in .tf.
func TestContextEval_providerFunctionsWithAlias(t *testing.T) {
	p := testProvider("test")
	p.GetProviderSchemaResponse.Functions = map[string]providers.FunctionSpec{
		"greet": {
			Parameters: []providers.FunctionParameterSpec{{
				Name: "name",
				Type: cty.String,
			}},
			Return: cty.String,
		},
	}
	p.CallFunctionResponse = &providers.CallFunctionResponse{
		Result: cty.StringVal("hello alice"),
	}

	m := testModuleInline(t, map[string]string{
		"main.tf": `
terraform {
  required_providers {
    test = {
      source = "hashicorp/test"
    }
  }
}

provider "test" {
  alias = "custom"
}
`,
	})

	ctx := testContext2(t, &ContextOpts{
		Plugins: plugins.NewLibrary(map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"): testProviderFuncFixed(p),
		}, nil),
	})

	scope, diags := ctx.Eval(t.Context(), m, states.NewState(), addrs.RootModuleInstance, &EvalOpts{})
	assertNoErrors(t, diags)

	expr, hclDiags := hclsyntax.ParseExpression([]byte(`provider::test::custom::greet("alice")`), "<test-input>", hcl.Pos{Line: 1, Column: 1})
	if hclDiags.HasErrors() {
		t.Fatalf("unexpected parse errors: %s", hclDiags.Error())
	}

	got, evalDiags := scope.EvalExpr(t.Context(), expr, cty.DynamicPseudoType)
	assertNoErrors(t, evalDiags)
	if got.AsString() != "hello alice" {
		t.Fatalf("expected 'hello alice', got %q", got.AsString())
	}
}

// TestContextEval_providerFunctionsUnknown verifies that calling a provider function
// for a provider not declared in the module produces a clear error.
func TestContextEval_providerFunctionsUnknown(t *testing.T) {
	m := testModuleInline(t, map[string]string{
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

	ctx := testContext2(t, &ContextOpts{})

	scope, diags := ctx.Eval(t.Context(), m, states.NewState(), addrs.RootModuleInstance, &EvalOpts{})
	assertNoErrors(t, diags)

	expr, _ := hclsyntax.ParseExpression([]byte(`provider::nonexistent::echo("foo")`), "<test-input>", hcl.Pos{Line: 1, Column: 1})
	_, evalDiags := scope.EvalExpr(t.Context(), expr, cty.DynamicPseudoType)
	if !evalDiags.HasErrors() {
		t.Fatal("expected error evaluating function on nonexistent provider")
	}
	errStr := evalDiags.Err().Error()
	if !strings.Contains(errStr, "nonexistent") {
		t.Fatalf("expected error mentioning nonexistent provider, got: %s", errStr)
	}
}
