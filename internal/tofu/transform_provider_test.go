// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package tofu

import (
	"fmt"
	"strings"
	"testing"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/dag"
)

func testProviderInstanceTransformerGraph(t *testing.T, cfg *configs.Config) *Graph {
	t.Helper()

	g := &Graph{Path: addrs.RootModuleInstance}
	ct := &ConfigTransformer{Config: cfg}
	if err := ct.Transform(g); err != nil {
		t.Fatal(err)
	}
	arct := &AttachResourceConfigTransformer{Config: cfg}
	if err := arct.Transform(g); err != nil {
		t.Fatal(err)
	}

	return g
}

// This variant exists purely for testing and can not currently include the ProviderFunctionTransformer
func testTransformProviders(concrete concreteProviderInstanceNodeFunc, config *configs.Config) GraphTransformer {
	return GraphTransformMulti(
		// Add providers from the config
		&providerConfigTransformer{
			config:           config,
			concreteProvider: concrete,
		},
		// Add any remaining missing providers
		&MissingProviderInstanceTransformer{
			Config:   config,
			Concrete: concrete,
		},
		// Connect the providers
		&ProviderInstanceTransformer{
			Config: config,
		},
		// After schema transformer, we can add function references
		//  &ProviderFunctionTransformer{Config: config},
		// Remove unused providers and proxies
		&PruneProviderInstanceTransformer{},
	)
}

func TestProviderInstanceTransformer(t *testing.T) {
	mod := testModule(t, "transform-provider-basic")

	g := testProviderInstanceTransformerGraph(t, mod)
	{
		transform := &MissingProviderInstanceTransformer{}
		if err := transform.Transform(g); err != nil {
			t.Fatalf("err: %s", err)
		}
	}

	transform := &ProviderInstanceTransformer{}
	if err := transform.Transform(g); err != nil {
		t.Fatalf("err: %s", err)
	}

	actual := strings.TrimSpace(g.String())
	expected := strings.TrimSpace(testTransformProviderBasicStr)
	if actual != expected {
		t.Fatalf("bad:\n\n%s", actual)
	}
}

// Test providers with FQNs that do not match the typeName
func TestProviderInstanceTransformer_fqns(t *testing.T) {
	for _, mod := range []string{"fqns", "fqns-module"} {
		mod := testModule(t, fmt.Sprintf("transform-provider-%s", mod))

		g := testProviderInstanceTransformerGraph(t, mod)
		{
			transform := &MissingProviderInstanceTransformer{Config: mod}
			if err := transform.Transform(g); err != nil {
				t.Fatalf("err: %s", err)
			}
		}

		transform := &ProviderInstanceTransformer{Config: mod}
		if err := transform.Transform(g); err != nil {
			t.Fatalf("err: %s", err)
		}

		actual := strings.TrimSpace(g.String())
		expected := strings.TrimSpace(testTransformProviderBasicStr)
		if actual != expected {
			t.Fatalf("bad:\n\n%s", actual)
		}
	}
}

func TestCloseProviderInstanceTransformer(t *testing.T) {
	mod := testModule(t, "transform-provider-basic")
	g := testProviderInstanceTransformerGraph(t, mod)

	{
		transform := &MissingProviderInstanceTransformer{}
		if err := transform.Transform(g); err != nil {
			t.Fatalf("err: %s", err)
		}
	}

	{
		transform := &ProviderInstanceTransformer{}
		if err := transform.Transform(g); err != nil {
			t.Fatalf("err: %s", err)
		}
	}

	{
		transform := &CloseProviderInstanceTransformer{}
		if err := transform.Transform(g); err != nil {
			t.Fatalf("err: %s", err)
		}
	}

	actual := strings.TrimSpace(g.String())
	expected := strings.TrimSpace(testTransformCloseProviderBasicStr)
	if actual != expected {
		t.Fatalf("bad:\n\n%s", actual)
	}
}

func TestCloseProviderInstanceTransformer_withTargets(t *testing.T) {
	mod := testModule(t, "transform-provider-basic")

	g := testProviderInstanceTransformerGraph(t, mod)
	transforms := []GraphTransformer{
		&MissingProviderInstanceTransformer{},
		&ProviderInstanceTransformer{},
		&CloseProviderInstanceTransformer{},
		&TargetsTransformer{
			Targets: []addrs.Targetable{
				addrs.RootModuleInstance.Resource(
					addrs.ManagedResourceMode, "something", "else",
				),
			},
		},
	}

	for _, tr := range transforms {
		if err := tr.Transform(g); err != nil {
			t.Fatalf("err: %s", err)
		}
	}

	actual := strings.TrimSpace(g.String())
	expected := strings.TrimSpace(``)
	if actual != expected {
		t.Fatalf("expected:%s\n\ngot:\n\n%s", expected, actual)
	}
}

func TestMissingProviderInstanceTransformer(t *testing.T) {
	mod := testModule(t, "transform-provider-missing")

	g := testProviderInstanceTransformerGraph(t, mod)
	{
		transform := &MissingProviderInstanceTransformer{}
		if err := transform.Transform(g); err != nil {
			t.Fatalf("err: %s", err)
		}
	}

	{
		transform := &ProviderInstanceTransformer{}
		if err := transform.Transform(g); err != nil {
			t.Fatalf("err: %s", err)
		}
	}

	{
		transform := &CloseProviderInstanceTransformer{}
		if err := transform.Transform(g); err != nil {
			t.Fatalf("err: %s", err)
		}
	}

	actual := strings.TrimSpace(g.String())
	expected := strings.TrimSpace(testTransformMissingProviderBasicStr)
	if actual != expected {
		t.Fatalf("expected:\n%s\n\ngot:\n%s", expected, actual)
	}
}

func TestMissingProviderInstanceTransformer_grandchildMissing(t *testing.T) {
	mod := testModule(t, "transform-provider-missing-grandchild")

	concrete := func(a *nodeAbstractProviderInstance) dag.Vertex { return a }

	g := testProviderInstanceTransformerGraph(t, mod)
	{
		transform := testTransformProviders(concrete, mod)
		if err := transform.Transform(g); err != nil {
			t.Fatalf("err: %s", err)
		}
	}
	{
		transform := &TransitiveReductionTransformer{}
		if err := transform.Transform(g); err != nil {
			t.Fatalf("err: %s", err)
		}
	}

	actual := strings.TrimSpace(g.String())
	expected := strings.TrimSpace(testTransformMissingGrandchildProviderStr)
	if actual != expected {
		t.Fatalf("expected:\n%s\n\ngot:\n%s", expected, actual)
	}
}

func TestPruneProviderInstanceTransformer(t *testing.T) {
	mod := testModule(t, "transform-provider-prune")

	g := testProviderInstanceTransformerGraph(t, mod)
	{
		transform := &MissingProviderInstanceTransformer{}
		if err := transform.Transform(g); err != nil {
			t.Fatalf("err: %s", err)
		}
	}

	{
		transform := &ProviderInstanceTransformer{}
		if err := transform.Transform(g); err != nil {
			t.Fatalf("err: %s", err)
		}
	}

	{
		transform := &CloseProviderInstanceTransformer{}
		if err := transform.Transform(g); err != nil {
			t.Fatalf("err: %s", err)
		}
	}

	{
		transform := &PruneProviderInstanceTransformer{}
		if err := transform.Transform(g); err != nil {
			t.Fatalf("err: %s", err)
		}
	}

	actual := strings.TrimSpace(g.String())
	expected := strings.TrimSpace(testTransformPruneProviderBasicStr)
	if actual != expected {
		t.Fatalf("bad:\n\n%s", actual)
	}
}

const testTransformProviderBasicStr = `
aws_instance.web
  provider["registry.opentofu.org/hashicorp/aws"]
provider["registry.opentofu.org/hashicorp/aws"]
`

const testTransformCloseProviderBasicStr = `
aws_instance.web
  provider["registry.opentofu.org/hashicorp/aws"]
provider["registry.opentofu.org/hashicorp/aws"]
provider["registry.opentofu.org/hashicorp/aws"] (close)
  aws_instance.web
  provider["registry.opentofu.org/hashicorp/aws"]
`

const testTransformMissingProviderBasicStr = `
aws_instance.web
  provider["registry.opentofu.org/hashicorp/aws"]
foo_instance.web
  provider["registry.opentofu.org/hashicorp/foo"]
provider["registry.opentofu.org/hashicorp/aws"]
provider["registry.opentofu.org/hashicorp/aws"] (close)
  aws_instance.web
  provider["registry.opentofu.org/hashicorp/aws"]
provider["registry.opentofu.org/hashicorp/foo"]
provider["registry.opentofu.org/hashicorp/foo"] (close)
  foo_instance.web
  provider["registry.opentofu.org/hashicorp/foo"]
`

const testTransformMissingGrandchildProviderStr = `
module.sub.module.subsub.bar_instance.two
  provider["registry.opentofu.org/hashicorp/bar"]
module.sub.module.subsub.foo_instance.one
  module.sub.provider["registry.opentofu.org/hashicorp/foo"]
module.sub.provider["registry.opentofu.org/hashicorp/foo"]
provider["registry.opentofu.org/hashicorp/bar"]
`

const testTransformPruneProviderBasicStr = `
foo_instance.web
  provider["registry.opentofu.org/hashicorp/foo"]
provider["registry.opentofu.org/hashicorp/foo"]
provider["registry.opentofu.org/hashicorp/foo"] (close)
  foo_instance.web
  provider["registry.opentofu.org/hashicorp/foo"]
`

const testTransformModuleProviderConfigStr = `
module.child.aws_instance.thing
  provider["registry.opentofu.org/hashicorp/aws"].foo
provider["registry.opentofu.org/hashicorp/aws"].foo
`

const testTransformModuleProviderGrandparentStr = `
module.child.module.grandchild.aws_instance.baz
  provider["registry.opentofu.org/hashicorp/aws"].foo
provider["registry.opentofu.org/hashicorp/aws"].foo
`
