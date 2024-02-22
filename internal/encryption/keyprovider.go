// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package encryption

import (
	"errors"
	"fmt"

	"github.com/opentofu/opentofu/internal/encryption/config"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/opentofu/opentofu/internal/encryption/keyprovider"
	"github.com/opentofu/opentofu/internal/encryption/registry"
	"github.com/opentofu/opentofu/internal/varhcl"
	"github.com/zclconf/go-cty/cty"
)

// setupKeyProviders sets up the key providers for encryption. It returns a list of diagnostics if any of the key providers
// are invalid.
func (e *targetBuilder) setupKeyProviders() hcl.Diagnostics {
	var diags hcl.Diagnostics

	e.keyValues = make(map[string]map[string]cty.Value)

	for _, keyProviderConfig := range e.cfg.KeyProviderConfigs {
		diags = append(diags, e.setupKeyProvider(keyProviderConfig, nil)...)
	}

	// Regenerate the context now that the key provider is loaded
	kpMap := make(map[string]cty.Value)
	for name, kps := range e.keyValues {
		kpMap[name] = cty.ObjectVal(kps)
	}
	e.ctx.Variables["key_provider"] = cty.ObjectVal(kpMap)

	return diags
}

// TODO: Break this method up into smaller methods
func (e *targetBuilder) setupKeyProvider(cfg config.KeyProviderConfig, stack []config.KeyProviderConfig) hcl.Diagnostics {
	// Ensure cfg.Type is in keyValues, if it isn't then add it in preparation for the next step
	if _, ok := e.keyValues[cfg.Type]; !ok {
		e.keyValues[cfg.Type] = make(map[string]cty.Value)
	}

	// Check if we have already setup this Descriptor (due to dependency loading)
	// if we've already setup this key provider, then we don't need to do it again
	// and we can return early
	if _, ok := e.keyValues[cfg.Type][cfg.Name]; ok {
		return nil
	}

	// Mark this key provider as partially handled.  This value will be replaced below once it is actually known.
	// The goal is to allow an early return via the above if statement to prevent duplicate errors if errors are encoutered in the key loading stack.
	e.keyValues[cfg.Type][cfg.Name] = cty.UnknownVal(cty.DynamicPseudoType)

	// Check for circular references, this is done by inspecting the stack of key providers
	// that are currently being setup. If we find a key provider in the stack that matches
	// the current key provider, then we have a circular reference and we should return an error
	// to the user.
	for _, s := range stack {
		if s == cfg {
			addr, diags := keyprovider.NewAddr(cfg.Type, cfg.Name)
			diags = diags.Append(
				&hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Circular reference detected",
					// TODO add the stack trace to the detail message
					Detail: fmt.Sprintf("Can not load %q due to circular reference", addr),
				},
			)
			return diags
		}
	}
	stack = append(stack, cfg)

	// Pull the meta key out for error messages and meta storage
	metaKey, diags := cfg.Addr()
	if diags.HasErrors() {
		return diags
	}

	// Lookup the KeyProviderDescriptor from the registry
	id := keyprovider.ID(cfg.Type)
	keyProviderDescriptor, err := e.reg.GetKeyProviderDescriptor(id)
	if err != nil {
		if errors.Is(err, &registry.KeyProviderNotFoundError{}) {
			return hcl.Diagnostics{&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Unknown key_provider type",
				Detail:   fmt.Sprintf("Can not find %q", cfg.Type),
			}}
		}
		return hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("Error fetching key_provider %q", cfg.Type),
			Detail:   err.Error(),
		}}
	}

	// Now that we know we have the correct Descriptor, we can decode the configuration
	// and build the KeyProvider
	keyProviderConfig := keyProviderDescriptor.ConfigStruct()

	// Locate all the dependencies
	deps, diags := varhcl.VariablesInBody(cfg.Body, keyProviderConfig)
	if diags.HasErrors() {
		return diags
	}

	depDiags := e.validateAndSetupKeyProviders(deps, stack)
	if diags.HasErrors() {
		return append(diags, depDiags...)
	}

	// Initialize the Key Provider
	decodeDiags := gohcl.DecodeBody(cfg.Body, e.ctx, keyProviderConfig)
	diags = append(diags, decodeDiags...)
	if diags.HasErrors() {
		return diags
	}

	// Build the Key Provider from the configuration
	keyProvider, err := keyProviderConfig.Build()
	if err != nil {
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Unable to build encryption key data",
			Detail:   fmt.Sprintf("%s failed with error: %s", metaKey, err.Error()),
		})
	}

	meta := e.keyProviderMetadata[metaKey]

	data, newMeta, err := keyProvider.Provide(meta)
	if err != nil {
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Unable to fetch encryption key data",
			Detail:   fmt.Sprintf("%s failed with error: %s", metaKey, err.Error()),
		})
	}

	e.keyProviderMetadata[metaKey] = newMeta

	// Convert the data into it's cty equivalent
	ctyData := make([]cty.Value, len(data))
	for i, d := range data {
		ctyData[i] = cty.NumberIntVal(int64(d))
	}
	e.keyValues[cfg.Type][cfg.Name] = cty.ListVal(ctyData)

	return nil
}

// TODO: Maybe think of a better name?
func (e *targetBuilder) validateAndSetupKeyProviders(deps []hcl.Traversal, stack []config.KeyProviderConfig) hcl.Diagnostics {
	diags := hcl.Diagnostics{}

	newError := func(sourceRange *hcl.Range) *hcl.Diagnostic {
		return &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid key_provider reference",
			Detail:   "Expected a reference in the form of key_provider.type.name",
			Subject:  sourceRange.Ptr(),
		}
	}

	for _, dep := range deps {
		// Key Provider references should be in the form key_provider.type.name
		if len(dep) != 3 {
			diags = append(diags, newError(dep.SourceRange().Ptr()))
			continue
		}

		depRoot, ok := dep[0].(hcl.TraverseRoot)
		if !ok {
			diags = append(diags, newError(dep.SourceRange().Ptr()))
			continue
		}

		if depRoot.Name != "key_provider" {
			diags = append(diags, newError(dep.SourceRange().Ptr()))
			continue
		}

		depTypeAttr, ok := dep[1].(hcl.TraverseAttr)
		if !ok {
			diags = append(diags, newError(dep.SourceRange().Ptr()))
			continue
		}
		depType := depTypeAttr.Name

		depNameAttr, ok := dep[2].(hcl.TraverseAttr)
		if !ok {
			diags = append(diags, newError(dep.SourceRange().Ptr()))
			continue
		}
		depName := depNameAttr.Name

		for _, kpc := range e.cfg.KeyProviderConfigs {
			// Find the key provider in the config
			if kpc.Type == depType && kpc.Name == depName {
				depDiags := e.setupKeyProvider(kpc, stack)
				diags = append(diags, depDiags...)
				break
			}
		}
	}

	return diags
}
