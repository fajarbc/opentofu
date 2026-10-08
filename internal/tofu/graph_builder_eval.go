// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package tofu

import (
	"context"
	"log"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/dag"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

// EvalGraphBuilder implements GraphBuilder and constructs a graph suitable
// for evaluating in-memory values (input variables, local values, output
// values).
//
// In-memory values can be evaluated before or during a plan or apply walk,
// but the eval graph can also be used on its own to evaluate expressions
// in an interactive console, without changing the state.
//
// In order to successfully evaluate in-memory values, the configuration
// must already be loaded, and the state must have already been initialized
// or read from persistent storage. Additionally, schemas must be available
// in order to obtain schema information used for type checking, etc.
type EvalGraphBuilder struct {
	// Config is the configuration tree.
	Config *configs.Config

	// State is the current state.
	State *states.State

	// RootVariableValues provides values for root module input variables.
	RootVariableValues InputValues

	// Plugins is a library of plug-in components (providers and
	// provisioners) available for use.
	Plugins *contextPlugins

	ProviderFunctionTracker ProviderFunctionMapping
}

// See GraphBuilder
func (b *EvalGraphBuilder) Build(ctx context.Context, path addrs.ModuleInstance) (*Graph, tfdiags.Diagnostics) {
	return (&BasicGraphBuilder{
		Steps: b.Steps(),
		Name:  "EvalGraphBuilder",
	}).Build(ctx, path)
}

// See GraphBuilder
func (b *EvalGraphBuilder) Steps() []GraphTransformer {
	concreteProvider := func(a *NodeAbstractProvider) dag.Vertex {
		return &NodeEvalableProvider{
			NodeAbstractProvider: a,
		}
	}

	steps := []GraphTransformer{
		// Creates all the data resources that aren't in the state. This will also
		// add any orphans from scaling in as destroy nodes.
		&ConfigTransformer{
			Config: b.Config,
		},

		// Add dynamic values
		&RootVariableTransformer{Config: b.Config, RawValues: b.RootVariableValues},
		&ModuleVariableTransformer{Config: b.Config},
		&LocalTransformer{Config: b.Config},
		&OutputTransformer{
			Config:   b.Config,
			Planning: true,
		},

		// Attach the configuration to any resources
		&AttachResourceConfigTransformer{Config: b.Config},

		// Attach the state
		&AttachStateTransformer{State: b.State},

		transformProviders(concreteProvider, b.Config, walkEval),

		// Must attach schemas before ReferenceTransformer so that we can
		// analyze the configuration to find references.
		&AttachSchemaTransformer{Plugins: b.Plugins, Config: b.Config},

		// Replace providers that have no config or dependencies to
		// NodeEvalableProvider. This allows using provider-defined functions
		// even when the provider isn't configured.
		&ProviderUnconfiguredTransformer{},

		// After schema transformer, we can add function references
		&ProviderFunctionTransformer{Config: b.Config, ProviderFunctionTracker: b.ProviderFunctionTracker},

		// For console evaluation, also track all available providers in the configuration so
		// provider-defined functions can be called dynamically even if not statically referenced in .tf
		&EvalProviderFunctionTransformer{Config: b.Config, ProviderFunctionTracker: b.ProviderFunctionTracker},

		// Remove unused providers and proxies, but retain providers tracked for functions in console
		&PruneEvalProviderTransformer{ProviderFunctionTracker: b.ProviderFunctionTracker},

		// Create expansion nodes for all of the module calls. This must
		// come after all other transformers that create nodes representing
		// objects that can belong to modules.
		&ModuleExpansionTransformer{Config: b.Config},

		// Connect so that the references are ready for targeting. We'll
		// have to connect again later for providers and so on.
		&ReferenceTransformer{},

		// Although we don't configure providers, we do still start them up
		// to get their schemas, and so we must shut them down again here.
		&CloseProviderTransformer{},

		// Close root module
		&CloseRootModuleTransformer{
			RootConfig: b.Config,
		},

		// Remove redundant edges to simplify the graph.
		&TransitiveReductionTransformer{},
	}

	return steps
}

// EvalProviderFunctionTransformer tracks all providers in Config in the ProviderFunctionTracker
// so that provider-defined functions can be called dynamically in console evaluation,
// even when the configuration does not already call them statically.
type EvalProviderFunctionTransformer struct {
	Config                  *configs.Config
	ProviderFunctionTracker ProviderFunctionMapping
}

func (t *EvalProviderFunctionTransformer) Transform(_ context.Context, g *Graph) error {
	if t.Config == nil || t.ProviderFunctionTracker == nil {
		return nil
	}

	providerVerts := providerVertexMap(g)

	var trackModule func(c *configs.Config)
	trackModule = func(c *configs.Config) {
		if c == nil || c.Module == nil {
			return
		}

		if c.Module.ProviderRequirements != nil {
			for name, rp := range c.Module.ProviderRequirements.RequiredProviders {
				t.trackProvider(g, providerVerts, c.Path, name, rp.Type, "")
				for _, alias := range rp.Aliases {
					t.trackProvider(g, providerVerts, c.Path, name, rp.Type, alias.Alias)
				}
			}
		}

		for _, pConfig := range c.Module.ProviderConfigs {
			var providerType addrs.Provider
			if c.Module.ProviderRequirements != nil {
				if rp, ok := c.Module.ProviderRequirements.RequiredProviders[pConfig.Name]; ok {
					providerType = rp.Type
				}
			}
			if providerType.Type == "" {
				providerType = addrs.ImpliedProviderForUnqualifiedType(pConfig.Name)
			}
			t.trackProvider(g, providerVerts, c.Path, pConfig.Name, providerType, pConfig.Alias)
		}

		for _, child := range c.Children {
			trackModule(child)
		}
	}

	trackModule(t.Config)
	return nil
}

func (t *EvalProviderFunctionTransformer) trackProvider(
	g *Graph,
	providerVerts map[string]GraphNodeProvider,
	modPath addrs.Module,
	name string,
	providerType addrs.Provider,
	alias string,
) {
	key := ProviderFunctionReference{
		ModulePath:    modPath.String(),
		ProviderName:  name,
		ProviderAlias: alias,
	}
	if _, ok := t.ProviderFunctionTracker[key]; ok {
		return
	}

	absPc := addrs.AbsProviderConfig{
		Provider: providerType,
		Module:   modPath,
		Alias:    alias,
	}

	var provider GraphNodeProvider = providerVerts[absPc.String()]
	if provider == nil {
		stubAddr := addrs.AbsProviderConfig{
			Module:   addrs.RootModule,
			Provider: providerType,
			Alias:    alias,
		}
		provider = providerVerts[stubAddr.String()]
		if provider == nil && alias != "" {
			defaultStubAddr := addrs.AbsProviderConfig{
				Module:   addrs.RootModule,
				Provider: providerType,
			}
			provider = providerVerts[defaultStubAddr.String()]
		}
		if provider == nil {
			log.Printf("[TRACE] EvalProviderFunctionTransformer: creating init-only node for %s", stubAddr)
			stub := &NodeEvalableProvider{
				NodeAbstractProvider: &NodeAbstractProvider{
					Addr: stubAddr,
				},
			}
			provider = stub
			providerVerts[stubAddr.String()] = stub
			g.Add(stub)
		}
	}

	if p, ok := provider.(*graphNodeProxyProvider); ok {
		provider = p.Target()
	}

	t.ProviderFunctionTracker[key] = FunctionProvidedBy{
		Provider:  provider.ProviderAddr(),
		Instance:  provider.Instance,
		KeyModule: modPath,
	}
}

// PruneEvalProviderTransformer removes unused providers and proxies, but retains
// providers tracked in ProviderFunctionTracker so their instances are initialized for console eval.
type PruneEvalProviderTransformer struct {
	ProviderFunctionTracker ProviderFunctionMapping
}

func (t *PruneEvalProviderTransformer) Transform(_ context.Context, g *Graph) error {
	for _, v := range g.Vertices() {
		pv, ok := v.(GraphNodeProvider)
		if !ok {
			continue
		}

		if _, ok := v.(*graphNodeProxyProvider); ok {
			log.Printf("[DEBUG] pruning proxy %s", dag.VertexName(v))
			g.Remove(v)
			continue
		}

		if t.ProviderFunctionTracker != nil && t.hasProvider(pv.ProviderAddr()) {
			log.Printf("[DEBUG] retaining %s for console eval functions", dag.VertexName(v))
			continue
		}

		// Remove providers with no dependencies.
		if g.UpEdges(v).Len() == 0 {
			log.Printf("[DEBUG] pruning unused %s", dag.VertexName(v))
			g.Remove(v)
		}
	}
	return nil
}

func (t *PruneEvalProviderTransformer) hasProvider(addr addrs.AbsProviderConfig) bool {
	addrStr := addr.String()
	for _, target := range t.ProviderFunctionTracker {
		if target.Provider.String() == addrStr {
			return true
		}
	}
	return false
}
