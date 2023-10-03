// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package parser

import (
	"errors"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/nomad-pack/internal/pkg/errors/packdiags"
	"github.com/hashicorp/nomad-pack/internal/pkg/variable/parser/config"
	"github.com/hashicorp/nomad-pack/sdk/pack"
	"github.com/hashicorp/nomad-pack/sdk/pack/variables"
	"golang.org/x/exp/maps"
)

// ParsedVariables wraps the parsed variables returned by parser.Parse and
// provides functionality to access them.
type ParsedVariables struct {
	v1Vars   map[string]map[string]*variables.Variable
	v2Vars   map[pack.ID]map[variables.ID]*variables.Variable
	Metadata *pack.Metadata
	version  *config.ParserVersion
}

func (pv *ParsedVariables) IsV2() bool {
	return *pv.version == config.V2
}

func (pv *ParsedVariables) IsV1() bool {
	return *pv.version == config.V1
}

func (pv *ParsedVariables) isLoaded() bool {
	return !(pv.version == nil)
}

// LoadV1Result populates this ParsedVariables with the result from
// parser_v1.Parse(). This function errors if the ParsedVariable has already
// been loaded.
func (pv *ParsedVariables) LoadV1Result(in map[string]map[string]*variables.Variable) error {
	if pv.isLoaded() {
		return errors.New("already loaded")
	}
	var vPtr = config.V1
	pv.v1Vars = maps.Clone(in)
	pv.version = &vPtr
	return nil
}

// LoadV2Result populates this ParsedVariables with the result from
// parser_v2.Parse(). This function errors if the ParsedVariable has already
// been loaded.
func (pv *ParsedVariables) LoadV2Result(in map[pack.ID]map[variables.ID]*variables.Variable) error {
	if pv.isLoaded() {
		return errors.New("already loaded")
	}
	var vPtr = config.V2
	pv.v2Vars = maps.Clone(in)
	pv.version = &vPtr
	return nil
}

// GetVars returns the data in the v2 interim data format
func (pv *ParsedVariables) GetVars() map[pack.ID]map[variables.ID]*variables.Variable {
	if !pv.isLoaded() {
		return nil
	}
	if *pv.version == config.V1 {
		return asV2Vars(pv.v1Vars)
	}
	return pv.v2Vars
}

// asV2Vars traverses the v1-style and converts it into an equivalent single
// level v2 variable map
func asV2Vars(in map[string]map[string]*variables.Variable) map[pack.ID]map[variables.ID]*variables.Variable {
	var out = make(map[pack.ID]map[variables.ID]*variables.Variable, len(in))
	for k, vs := range in {
		out[pack.ID(k)] = make(map[variables.ID]*variables.Variable, len(vs))
		for vk, v := range vs {
			out[pack.ID(k)][variables.ID(vk)] = v
		}
	}
	return out
}

// The following functions generate the appropriate data formats that are sent
// to the text/template renderer for version 1 and version 2 syntax pack
// templates. V1 templates need to have a template context created by the
// `ConvertVariablesToMapInterface` function. V2 templates use a context
// generated by the `ToPackTemplateContext` function

// SECTION: ParserV2 helper functions

// ToPackTemplateContext creates a PackTemplateContext from this
// ParsedVariables.
// Even though parsing the variable went without error, it is highly
// possible that conversion to native go types can incur an error.
// If an error is returned, it should be considered terminal.
func (pv *ParsedVariables) ToPackTemplateContext(p *pack.Pack) (PackTemplateContext, hcl.Diagnostics) {
	out := make(PackTemplateContext)
	diags := pv.toPackTemplateContextR(&out, p)
	return out, diags
}

// toPackTemplateContextR is the recursive implementation of ToPackTemplateContext
func (pv *ParsedVariables) toPackTemplateContextR(tgt *PackTemplateContext, p *pack.Pack) hcl.Diagnostics {
	pVars, diags := asMapOfStringToAny(pv.v2Vars[p.VariablesPath()])
	if diags.HasErrors() {
		return diags
	}

	(*tgt)[CurrentPackKey] = PackData{
		Pack: p,
		vars: pVars,
		meta: p.Metadata.ConvertToMapInterface(),
	}

	for _, d := range p.Dependencies() {
		out := make(PackTemplateContext)
		diags.Extend(pv.toPackTemplateContextR(&out, d))
		(*tgt)[d.AliasOrName()] = out
	}

	return diags
}

// asMapOfStringToAny builds the map used by the `var` template function
func asMapOfStringToAny(m map[variables.ID]*variables.Variable) (map[string]any, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	o := make(map[string]any)
	for k, cVal := range m {
		val, err := variables.ConvertCtyToInterface(cVal.Value)
		if err != nil {
			diags = packdiags.SafeDiagnosticsAppend(diags, packdiags.DiagFailedToConvertCty(err, cVal.DeclRange.Ptr()))
			continue
		}
		o[string(k)] = val
	}
	return o, diags
}

// SECTION: ParserV1 helper functions

// ConvertVariablesToMapInterface creates the data object for V1 syntax
// templates.
func (pv *ParsedVariables) ConvertVariablesToMapInterface() (map[string]any, hcl.Diagnostics) {

	// Create our output; no matter what we return something.
	out := make(map[string]any)

	// Errors can occur when performing the translation. We want to capture all
	// of these and return them to the user. This allows them to fix problems
	// in a single cycle.
	var diags hcl.Diagnostics

	// Iterate each set of pack variable.
	for packName, packVars := range pv.v1Vars {

		// packVar collects all variables associated to a pack.
		packVar := map[string]any{}

		// Convert each variable and add this to the pack map.
		for variableName, variable := range packVars {
			varInterface, err := variables.ConvertCtyToInterface(variable.Value)
			if err != nil {
				diags = packdiags.SafeDiagnosticsAppend(diags, packdiags.DiagFailedToConvertCty(err, variable.DeclRange.Ptr()))
				continue
			}
			packVar[variableName] = varInterface
		}

		// Add the pack variable to the full output.
		out[packName] = packVar
	}

	return out, diags
}

// SECTION: Generator helper functions

// AsOverrideFile formats a ParsedVariables so it can be used as a var-file.
// This is used in the `generate varfile` command.
func (pv *ParsedVariables) AsOverrideFile() string {
	var out strings.Builder
	out.WriteString(pv.varFileHeader())

	packnames := maps.Keys(pv.v2Vars)
	slices.Sort(packnames)
	for _, packname := range packnames {
		vs := pv.v2Vars[packname]

		varnames := maps.Keys(vs)
		slices.Sort(varnames)
		for _, varname := range varnames {
			v := vs[varname]
			out.WriteString(v.AsOverrideString(packname))
		}
	}

	return out.String()
}

// varFileHeader provides additional content to be placed at the top of a
// generated varfile
func (pv *ParsedVariables) varFileHeader() string {
	// Use pack metadata to enhance the header if desired.
	// _ = vf.Metadata
	// This value will be added to the top of the varfile
	return ""
}
