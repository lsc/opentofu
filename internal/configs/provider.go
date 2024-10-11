// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package configs

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclsyntax"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/instances"
	"github.com/opentofu/opentofu/internal/lang/evalchecks"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

// ProviderCommon is the common fields between a Provider Block and a Provider
type ProviderCommon struct {
	Name      string
	NameRange hcl.Range

	Version VersionConstraint

	Config hcl.Body

	DeclRange hcl.Range

	// TODO: this may not be set in some cases, so it is not yet suitable for
	// use outside of this package. We currently only use it for internal
	// validation, but once we verify that this can be set in all cases, we can
	// export this so providers don't need to be re-resolved.
	// This same field is also added to the ProviderConfigRef struct.
	providerType addrs.Provider

	// IsMocked indicates if this provider has been mocked. It is used in
	// testing framework to instantiate test provider wrapper.
	IsMocked      bool
	MockResources []*MockResource
}

// ProviderBlock represents a "provider" block in a module or file. A provider
// block is a provider configuration
type ProviderBlock struct {
	ProviderCommon
	AliasExpr  hcl.Expression // nil if no alias set
	AliasRange *hcl.Range     // nil if no alias set
	ForEach    hcl.Expression
}

// Provider represents an instance of a "provider" block. Created after
// the exvaluation of a provider block containing the specific data
// for each instance derived from the evaluation
type Provider struct {
	ProviderCommon
	Alias        string
	InstanceData instances.RepetitionData
}

// ParseProviderConfigCompact parses the given absolute traversal as a relative
// provider address in compact form. The following are examples of traversals
// that can be successfully parsed as compact relative provider configuration
// addresses:
//
//   - aws
//   - aws.foo
//
// This function will panic if given a relative traversal.
//
// If the returned diagnostics contains errors then the result value is invalid
// and must not be used.
func ParseProviderConfigCompact(traversal hcl.Traversal) (addrs.LocalProviderInstance, tfdiags.Diagnostics) {
	// added as a const to keep the linter happy
	const providerAddrMaxTraversal = 2
	var diags tfdiags.Diagnostics
	ret := addrs.LocalProviderInstance{
		LocalName: traversal.RootName(),
	}

	if len(traversal) < providerAddrMaxTraversal {
		// Just a type name, then.
		return ret, diags
	}

	aliasStep := traversal[1]
	switch ts := aliasStep.(type) {
	case hcl.TraverseAttr:
		ret.Alias = ts.Name
		return ret, diags
	default:
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid provider configuration address",
			Detail:   "The provider type name must either stand alone or be followed by an alias name separated with a dot.",
			Subject:  aliasStep.SourceRange().Ptr(),
		})
	}

	if len(traversal) > providerAddrMaxTraversal {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid provider configuration address",
			Detail:   "Extraneous extra operators after provider configuration address.",
			Subject:  traversal[providerAddrMaxTraversal:].SourceRange().Ptr(),
		})
	}

	return ret, diags
}

// ParseProviderConfigCompactStr is a helper wrapper around ParseProviderConfigCompact
// that takes a string and parses it with the HCL native syntax traversal parser
// before interpreting it.
//
// This should be used only in specialized situations since it will cause the
// created references to not have any meaningful source location information.
// If a reference string is coming from a source that should be identified in
// error messages then the caller should instead parse it directly using a
// suitable function from the HCL API and pass the traversal itself to
// ParseProviderConfigCompact.
//
// Error diagnostics are returned if either the parsing fails or the analysis
// of the traversal fails. There is no way for the caller to distinguish the
// two kinds of diagnostics programmatically. If error diagnostics are returned
// then the returned address is invalid.
func ParseProviderConfigCompactStr(str string) (addrs.LocalProviderInstance, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	traversal, parseDiags := hclsyntax.ParseTraversalAbs([]byte(str), "", hcl.Pos{Line: 1, Column: 1})
	diags = diags.Append(parseDiags)
	if parseDiags.HasErrors() {
		return addrs.LocalProviderInstance{}, diags
	}

	addr, addrDiags := ParseProviderConfigCompact(traversal)
	diags = diags.Append(addrDiags)
	return addr, diags
}

func decodeProviderBlock(block *hcl.Block) (*ProviderBlock, hcl.Diagnostics) {
	var diags hcl.Diagnostics

	content, config, moreDiags := block.Body.PartialContent(providerBlockSchema)
	diags = append(diags, moreDiags...)

	// Provider names must be localized. Produce an error with a message
	// indicating the action the user can take to fix this message if the local
	// name is not localized.
	name := block.Labels[0]
	nameDiags := checkProviderNameNormalized(name, block.DefRange)
	diags = append(diags, nameDiags...)
	if nameDiags.HasErrors() {
		// If the name is invalid then we mustn't produce a result because
		// downstreams could try to use it as a provider type and then crash.
		return nil, diags
	}

	provider := &ProviderBlock{
		ProviderCommon: ProviderCommon{
			Name:      name,
			NameRange: block.LabelRanges[0],
			Config:    config,
			DeclRange: block.DefRange,
		},
	}

	if attr, exists := content.Attributes["alias"]; exists {
		provider.AliasExpr = attr.Expr
		provider.AliasRange = attr.Expr.Range().Ptr()
	}

	if attr, exists := content.Attributes["for_each"]; exists {
		provider.ForEach = attr.Expr
	}

	if provider.AliasExpr != nil && provider.ForEach != nil {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  `Invalid combination of "alias" and "for_each"`,
			Detail:   `The "alias" and "for_each" arguments are mutually-exclusive, only one may be used.`,
			Subject:  provider.AliasExpr.Range().Ptr(),
		})
	}

	if attr, exists := content.Attributes["version"]; exists {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagWarning,
			Summary:  "Version constraints inside provider configuration blocks are deprecated",
			Detail:   "OpenTofu 0.13 and earlier allowed provider version constraints inside the provider configuration block, but that is now deprecated and will be removed in a future version of OpenTofu. To silence this warning, move the provider version constraint into the required_providers block.",
			Subject:  attr.Expr.Range().Ptr(),
		})
		var versionDiags hcl.Diagnostics
		provider.Version, versionDiags = decodeVersionConstraint(attr)
		diags = append(diags, versionDiags...)
	}

	reserveredDiags := checkReservedNames(content)
	diags = append(diags, reserveredDiags...)

	var seenEscapeBlock *hcl.Block
	for _, block := range content.Blocks {
		switch block.Type {
		case "_":
			if seenEscapeBlock != nil {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Duplicate escaping block",
					Detail: fmt.Sprintf(
						"The special block type \"_\" can be used to force particular arguments to be interpreted as provider-specific rather than as meta-arguments, but each provider block can have only one such block. The first escaping block was at %s.",
						seenEscapeBlock.DefRange,
					),
					Subject: &block.DefRange,
				})
				continue
			}
			seenEscapeBlock = block

			// When there's an escaping block its content merges with the
			// existing config we extracted earlier, so later decoding
			// will see a blend of both.
			provider.Config = hcl.MergeBodies([]hcl.Body{provider.Config, block.Body})

		default:
			// All of the other block types in our schema are reserved for
			// future expansion.
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Reserved block type name in provider block",
				Detail:   fmt.Sprintf("The block type name %q is reserved for use by OpenTofu in a future version.", block.Type),
				Subject:  &block.TypeRange,
			})
		}
	}

	return provider, diags
}

func checkReservedNames(content *hcl.BodyContent) hcl.Diagnostics {
	var diags hcl.Diagnostics
	// Reserved attribute names
	for _, name := range []string{"depends_on", "source", "count"} {
		if attr, exists := content.Attributes[name]; exists {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Reserved argument name in provider block",
				Detail:   fmt.Sprintf("The provider argument name %q is reserved for use by OpenTofu in a future version.", name),
				Subject:  &attr.NameRange,
			})
		}
	}
	return diags
}

// checkProviderNameNormalized verifies that the given string is already
// normalized and returns an error if not.
func checkProviderNameNormalized(name string, declrange hcl.Range) hcl.Diagnostics {
	var diags hcl.Diagnostics
	// verify that the provider local name is normalized
	normalized, err := addrs.IsProviderPartNormalized(name)
	if err != nil {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid provider local name",
			Detail:   fmt.Sprintf("%s is an invalid provider local name: %s", name, err),
			Subject:  &declrange,
		})
		return diags
	}
	if !normalized {
		// we would have returned this error already
		normalizedProvider, _ := addrs.ParseProviderPart(name)
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid provider local name",
			Detail:   fmt.Sprintf("Provider names must be normalized. Replace %q with %q to fix this error.", name, normalizedProvider),
			Subject:  &declrange,
		})
	}
	return diags
}

// Addr returns the address of the receiving provider configuration, relative
// to its containing module.
func (p *Provider) Addr() addrs.LocalProviderInstance {
	return addrs.LocalProviderInstance{
		LocalName: p.Name,
		Alias:     p.Alias,
	}
}

func (p *ProviderBlock) decodeStaticFields(eval *StaticEvaluator) ([]*Provider, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	if p.ForEach != nil {
		return p.generateForEachProviders(eval)
	}

	result := Provider{ProviderCommon: p.ProviderCommon}
	if p.AliasExpr != nil {
		if eval != nil {
			valDiags := eval.DecodeExpression(p.AliasExpr, StaticIdentifier{
				Module:    eval.call.addr,
				Subject:   fmt.Sprintf("provider.%s.alias", p.Name),
				DeclRange: p.AliasExpr.Range(),
			}, &result.Alias)
			diags = append(diags, valDiags...)
		} else {
			// Test files don't have a static context
			valDiags := gohcl.DecodeExpression(p.AliasExpr, nil, &result.Alias)
			diags = append(diags, valDiags...)
		}

		if diags.HasErrors() {
			return nil, diags
		}

		if !hclsyntax.ValidIdentifier(result.Alias) {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid provider configuration alias",
				Detail:   fmt.Sprintf("Alias %q must be a valid name. %s", result.Alias, badIdentifierDetail),
				Subject:  p.AliasExpr.Range().Ptr(),
			})
		}
	}
	return []*Provider{&result}, diags
}

func (p *ProviderBlock) generateForEachProviders(eval *StaticEvaluator) ([]*Provider, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	if eval == nil {
		return nil, diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Iteration not allowed in test files",
			Detail:   "for_each was declared as a provider attribute in a test file",
			Subject:  p.ForEach.Range().Ptr(),
		})
	}

	forEachRefsFunc := func(refs []*addrs.Reference) (*hcl.EvalContext, tfdiags.Diagnostics) {
		var diags tfdiags.Diagnostics
		evalContext, evalDiags := eval.EvalContext(StaticIdentifier{
			Module:    eval.call.addr,
			Subject:   fmt.Sprintf("provider.%s.for_each", p.Name),
			DeclRange: p.ForEach.Range(),
		}, refs)
		return evalContext, diags.Append(evalDiags)
	}

	forVal, evalDiags := evalchecks.EvaluateForEachExpression(p.ForEach, forEachRefsFunc)
	diags = append(diags, evalDiags.ToHCL()...)
	if evalDiags.HasErrors() {
		return nil, diags
	}

	var out []*Provider
	for k, v := range forVal {
		if !hclsyntax.ValidIdentifier(k) {
			return nil, diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid for_each key alias",
				Detail:   fmt.Sprintf("Alias %q must be a valid name. %s", k, badIdentifierDetail),
				Subject:  p.ForEach.Range().Ptr(),
			})
		}

		out = append(out, &Provider{
			ProviderCommon: p.ProviderCommon,
			Alias:          k,
			InstanceData: instances.RepetitionData{
				EachValue: v,
			},
		})
	}
	return out, diags
}

var providerBlockSchema = &hcl.BodySchema{ //nolint: gochecknoglobals // pre-existing code
	Attributes: []hcl.AttributeSchema{
		{
			Name: "alias",
		},
		{
			Name: "version",
		},
		{
			Name: "for_each",
		},

		// Attribute names reserved for future expansion.
		{Name: "count"},
		{Name: "depends_on"},
		{Name: "source"},
	},
	Blocks: []hcl.BlockHeaderSchema{
		{Type: "_"}, // meta-argument escaping block

		// The rest of these are reserved for future expansion.
		{Type: "lifecycle"},
		{Type: "locals"},
	},
}
