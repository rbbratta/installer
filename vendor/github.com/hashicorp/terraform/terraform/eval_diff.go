package terraform

import (
	"fmt"
	"log"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/terraform/addrs"
	"github.com/hashicorp/terraform/configs"
	"github.com/hashicorp/terraform/plans"
	"github.com/hashicorp/terraform/plans/objchange"
	"github.com/hashicorp/terraform/providers"
	"github.com/hashicorp/terraform/states"
	"github.com/hashicorp/terraform-plugin-sdk/tfdiags"
)

// EvalCheckPlannedChange is an EvalNode implementation that produces errors
// if the _actual_ expected value is not compatible with what was recorded
// in the plan.
//
// Errors here are most often indicative of a bug in the provider, so our
// error messages will report with that in mind. It's also possible that
// there's a bug in Terraform's Core's own "proposed new value" code in
// EvalDiff.
type EvalCheckPlannedChange struct {
	Addr           addrs.ResourceInstance
	ProviderAddr   addrs.AbsProviderConfig
	ProviderSchema **ProviderSchema

	// We take ResourceInstanceChange objects here just because that's what's
	// convenient to pass in from the evaltree implementation, but we really
	// only look at the "After" value of each change.
	Planned, Actual **plans.ResourceInstanceChange
}

func (n *EvalCheckPlannedChange) Eval(ctx EvalContext) (interface{}, error) {
	providerSchema := *n.ProviderSchema
	plannedChange := *n.Planned
	actualChange := *n.Actual

	schema, _ := providerSchema.SchemaForResourceAddr(n.Addr.ContainingResource())
	if schema == nil {
		// Should be caught during validation, so we don't bother with a pretty error here
		return nil, fmt.Errorf("provider does not support %q", n.Addr.Resource.Type)
	}

	var diags tfdiags.Diagnostics
	absAddr := n.Addr.Absolute(ctx.Path())

	log.Printf("[TRACE] EvalCheckPlannedChange: Verifying that actual change (action %s) matches planned change (action %s)", actualChange.Action, plannedChange.Action)

	if plannedChange.Action != actualChange.Action {
		switch {
		case plannedChange.Action == plans.Update && actualChange.Action == plans.NoOp:
			// It's okay for an update to become a NoOp once we've filled in
			// all of the unknown values, since the final values might actually
			// match what was there before after all.
			log.Printf("[DEBUG] After incorporating new values learned so far during apply, %s change has become NoOp", absAddr)

		case (plannedChange.Action == plans.CreateThenDelete && actualChange.Action == plans.DeleteThenCreate) ||
			(plannedChange.Action == plans.DeleteThenCreate && actualChange.Action == plans.CreateThenDelete):
			// If the order of replacement changed, then that is a bug in terraform
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Terraform produced inconsistent final plan",
				fmt.Sprintf(
					"When expanding the plan for %s to include new values learned so far during apply, the planned action changed from %s to %s.\n\nThis is a bug in Terraform and should be reported.",
					absAddr, plannedChange.Action, actualChange.Action,
				),
			))
		default:
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Provider produced inconsistent final plan",
				fmt.Sprintf(
					"When expanding the plan for %s to include new values learned so far during apply, provider %q changed the planned action from %s to %s.\n\nThis is a bug in the provider, which should be reported in the provider's own issue tracker.",
					absAddr, n.ProviderAddr.Provider.String(),
					plannedChange.Action, actualChange.Action,
				),
			))
		}
	}

	errs := objchange.AssertObjectCompatible(schema, plannedChange.After, actualChange.After)
	for _, err := range errs {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Provider produced inconsistent final plan",
			fmt.Sprintf(
				"When expanding the plan for %s to include new values learned so far during apply, provider %q produced an invalid new value for %s.\n\nThis is a bug in the provider, which should be reported in the provider's own issue tracker.",
				absAddr, n.ProviderAddr.Provider.String(), tfdiags.FormatError(err),
			),
		))
	}
	return nil, diags.Err()
}

// EvalDiff is an EvalNode implementation that detects changes for a given
// resource instance.
type EvalDiff struct {
	Addr           addrs.ResourceInstance
	Config         *configs.Resource
	Provider       *providers.Interface
	ProviderAddr   addrs.AbsProviderConfig
	ProviderMetas  map[addrs.Provider]*configs.ProviderMeta
	ProviderSchema **ProviderSchema
	State          **states.ResourceInstanceObject
	PreviousDiff   **plans.ResourceInstanceChange

	// CreateBeforeDestroy is set if either the resource's own config sets
	// create_before_destroy explicitly or if dependencies have forced the
	// resource to be handled as create_before_destroy in order to avoid
	// a dependency cycle.
	CreateBeforeDestroy bool

	OutputChange **plans.ResourceInstanceChange
	OutputState  **states.ResourceInstanceObject

	Stub bool
}

// TODO: test
func (n *EvalDiff) Eval(ctx EvalContext) (interface{}, error) {
	state := *n.State
	config := *n.Config
	provider := *n.Provider
	providerSchema := *n.ProviderSchema

	createBeforeDestroy := n.CreateBeforeDestroy
	if n.PreviousDiff != nil {
		// If we already planned the action, we stick to that plan
		createBeforeDestroy = (*n.PreviousDiff).Action == plans.CreateThenDelete
	}

	if providerSchema == nil {
		return nil, fmt.Errorf("provider schema is unavailable for %s", n.Addr)
	}
	if n.ProviderAddr.Provider.Type == "" {
		panic(fmt.Sprintf("EvalDiff for %s does not have ProviderAddr set", n.Addr.Absolute(ctx.Path())))
	}

	var diags tfdiags.Diagnostics

	// Evaluate the configuration
	schema, _ := providerSchema.SchemaForResourceAddr(n.Addr.ContainingResource())
	if schema == nil {
		// Should be caught during validation, so we don't bother with a pretty error here
		return nil, fmt.Errorf("provider does not support resource type %q", n.Addr.Resource.Type)
	}
	forEach, _ := evaluateForEachExpression(n.Config.ForEach, ctx)
	keyData := EvalDataForInstanceKey(n.Addr.Key, forEach)
	origConfigVal, _, configDiags := ctx.EvaluateBlock(config.Config, schema, nil, keyData)
	diags = diags.Append(configDiags)
	if configDiags.HasErrors() {
		return nil, diags.Err()
	}

	metaConfigVal := cty.NullVal(cty.DynamicPseudoType)
	if n.ProviderMetas != nil {
		if m, ok := n.ProviderMetas[n.ProviderAddr.Provider]; ok && m != nil {
			// if the provider doesn't support this feature, throw an error
			if (*n.ProviderSchema).ProviderMeta == nil {
				diags = diags.Append(&hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("Provider %s doesn't support provider_meta", n.ProviderAddr.Provider.String()),
					Detail:   fmt.Sprintf("The resource %s belongs to a provider that doesn't support provider_meta blocks", n.Addr),
					Subject:  &m.ProviderRange,
				})
			} else {
				var configDiags tfdiags.Diagnostics
				metaConfigVal, _, configDiags = ctx.EvaluateBlock(m.Config, (*n.ProviderSchema).ProviderMeta, nil, EvalDataForNoInstanceKey)
				diags = diags.Append(configDiags)
				if configDiags.HasErrors() {
					return nil, diags.Err()
				}
			}
		}
	}

	absAddr := n.Addr.Absolute(ctx.Path())
	var priorVal cty.Value
	var priorValTainted cty.Value
	var priorPrivate []byte
	if state != nil {
		if state.Status != states.ObjectTainted {
			priorVal = state.Value
			priorPrivate = state.Private
		} else {
			// If the prior state is tainted then we'll proceed below like
			// we're creating an entirely new object, but then turn it into
			// a synthetic "Replace" change at the end, creating the same
			// result as if the provider had marked at least one argument
			// change as "requires replacement".
			priorValTainted = state.Value
			priorVal = cty.NullVal(schema.ImpliedType())
		}
	} else {
		priorVal = cty.NullVal(schema.ImpliedType())
	}

	// Create an unmarked version of our config val and our prior val.
	// Store the paths for the config val to re-markafter
	// we've sent things over the wire.
	unmarkedConfigVal, unmarkedPaths := origConfigVal.UnmarkDeepWithPaths()
	unmarkedPriorVal, priorPaths := priorVal.UnmarkDeepWithPaths()

	log.Printf("[TRACE] Re-validating config for %q", n.Addr.Absolute(ctx.Path()))
	// Allow the provider to validate the final set of values.
	// The config was statically validated early on, but there may have been
	// unknown values which the provider could not validate at the time.
	// TODO: It would be more correct to validate the config after
	// ignore_changes has been applied, but the current implementation cannot
	// exclude computed-only attributes when given the `all` option.
	validateResp := provider.ValidateResourceTypeConfig(
		providers.ValidateResourceTypeConfigRequest{
			TypeName: n.Addr.Resource.Type,
			Config:   unmarkedConfigVal,
		},
	)
	if validateResp.Diagnostics.HasErrors() {
		return nil, validateResp.Diagnostics.InConfigBody(config.Config).Err()
	}

	// ignore_changes is meant to only apply to the configuration, so it must
	// be applied before we generate a plan. This ensures the config used for
	// the proposed value, the proposed value itself, and the config presented
	// to the provider in the PlanResourceChange request all agree on the
	// starting values.
	configValIgnored, ignoreChangeDiags := n.processIgnoreChanges(unmarkedPriorVal, unmarkedConfigVal)
	diags = diags.Append(ignoreChangeDiags)
	if ignoreChangeDiags.HasErrors() {
		return nil, diags.Err()
	}

	proposedNewVal := objchange.ProposedNewObject(schema, unmarkedPriorVal, configValIgnored)

	// Call pre-diff hook
	if !n.Stub {
		err := ctx.Hook(func(h Hook) (HookAction, error) {
			return h.PreDiff(absAddr, states.CurrentGen, priorVal, proposedNewVal)
		})
		if err != nil {
			return nil, err
		}
	}

	resp := provider.PlanResourceChange(providers.PlanResourceChangeRequest{
		TypeName:         n.Addr.Resource.Type,
		Config:           configValIgnored,
		PriorState:       unmarkedPriorVal,
		ProposedNewState: proposedNewVal,
		PriorPrivate:     priorPrivate,
		ProviderMeta:     metaConfigVal,
	})
	diags = diags.Append(resp.Diagnostics.InConfigBody(config.Config))
	if diags.HasErrors() {
		return nil, diags.Err()
	}

	plannedNewVal := resp.PlannedState
	plannedPrivate := resp.PlannedPrivate

	if plannedNewVal == cty.NilVal {
		// Should never happen. Since real-world providers return via RPC a nil
		// is always a bug in the client-side stub. This is more likely caused
		// by an incompletely-configured mock provider in tests, though.
		panic(fmt.Sprintf("PlanResourceChange of %s produced nil value", absAddr.String()))
	}

	// We allow the planned new value to disagree with configuration _values_
	// here, since that allows the provider to do special logic like a
	// DiffSuppressFunc, but we still require that the provider produces
	// a value whose type conforms to the schema.
	for _, err := range plannedNewVal.Type().TestConformance(schema.ImpliedType()) {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Provider produced invalid plan",
			fmt.Sprintf(
				"Provider %q planned an invalid value for %s.\n\nThis is a bug in the provider, which should be reported in the provider's own issue tracker.",
				n.ProviderAddr.Provider.String(), tfdiags.FormatErrorPrefixed(err, absAddr.String()),
			),
		))
	}
	if diags.HasErrors() {
		return nil, diags.Err()
	}

	if errs := objchange.AssertPlanValid(schema, unmarkedPriorVal, configValIgnored, plannedNewVal); len(errs) > 0 {
		if resp.LegacyTypeSystem {
			// The shimming of the old type system in the legacy SDK is not precise
			// enough to pass this consistency check, so we'll give it a pass here,
			// but we will generate a warning about it so that we are more likely
			// to notice in the logs if an inconsistency beyond the type system
			// leads to a downstream provider failure.
			var buf strings.Builder
			fmt.Fprintf(&buf,
				"[WARN] Provider %q produced an invalid plan for %s, but we are tolerating it because it is using the legacy plugin SDK.\n    The following problems may be the cause of any confusing errors from downstream operations:",
				n.ProviderAddr.Provider.String(), absAddr,
			)
			for _, err := range errs {
				fmt.Fprintf(&buf, "\n      - %s", tfdiags.FormatError(err))
			}
			log.Print(buf.String())
		} else {
			for _, err := range errs {
				diags = diags.Append(tfdiags.Sourceless(
					tfdiags.Error,
					"Provider produced invalid plan",
					fmt.Sprintf(
						"Provider %q planned an invalid value for %s.\n\nThis is a bug in the provider, which should be reported in the provider's own issue tracker.",
						n.ProviderAddr.Provider.String(), tfdiags.FormatErrorPrefixed(err, absAddr.String()),
					),
				))
			}
			return nil, diags.Err()
		}
	}

	if resp.LegacyTypeSystem {
		// Because we allow legacy providers to depart from the contract and
		// return changes to non-computed values, the plan response may have
		// altered values that were already suppressed with ignore_changes.
		// A prime example of this is where providers attempt to obfuscate
		// config data by turning the config value into a hash and storing the
		// hash value in the state. There are enough cases of this in existing
		// providers that we must accommodate the behavior for now, so for
		// ignore_changes to work at all on these values, we will revert the
		// ignored values once more.
		plannedNewVal, ignoreChangeDiags = n.processIgnoreChanges(unmarkedPriorVal, plannedNewVal)
		diags = diags.Append(ignoreChangeDiags)
		if ignoreChangeDiags.HasErrors() {
			return nil, diags.ErrWithWarnings()
		}
	}

	// Add the marks back to the planned new value -- this must happen after ignore changes
	// have been processed
	unmarkedPlannedNewVal := plannedNewVal
	if len(unmarkedPaths) > 0 {
		plannedNewVal = plannedNewVal.MarkWithPaths(unmarkedPaths)
	}

	// The provider produces a list of paths to attributes whose changes mean
	// that we must replace rather than update an existing remote object.
	// However, we only need to do that if the identified attributes _have_
	// actually changed -- particularly after we may have undone some of the
	// changes in processIgnoreChanges -- so now we'll filter that list to
	// include only where changes are detected.
	reqRep := cty.NewPathSet()
	if len(resp.RequiresReplace) > 0 {
		for _, path := range resp.RequiresReplace {
			if priorVal.IsNull() {
				// If prior is null then we don't expect any RequiresReplace at all,
				// because this is a Create action.
				continue
			}

			priorChangedVal, priorPathDiags := hcl.ApplyPath(unmarkedPriorVal, path, nil)
			plannedChangedVal, plannedPathDiags := hcl.ApplyPath(plannedNewVal, path, nil)
			if plannedPathDiags.HasErrors() && priorPathDiags.HasErrors() {
				// This means the path was invalid in both the prior and new
				// values, which is an error with the provider itself.
				diags = diags.Append(tfdiags.Sourceless(
					tfdiags.Error,
					"Provider produced invalid plan",
					fmt.Sprintf(
						"Provider %q has indicated \"requires replacement\" on %s for a non-existent attribute path %#v.\n\nThis is a bug in the provider, which should be reported in the provider's own issue tracker.",
						n.ProviderAddr.Provider.String(), absAddr, path,
					),
				))
				continue
			}

			// Make sure we have valid Values for both values.
			// Note: if the opposing value was of the type
			// cty.DynamicPseudoType, the type assigned here may not exactly
			// match the schema. This is fine here, since we're only going to
			// check for equality, but if the NullVal is to be used, we need to
			// check the schema for th true type.
			switch {
			case priorChangedVal == cty.NilVal && plannedChangedVal == cty.NilVal:
				// this should never happen without ApplyPath errors above
				panic("requires replace path returned 2 nil values")
			case priorChangedVal == cty.NilVal:
				priorChangedVal = cty.NullVal(plannedChangedVal.Type())
			case plannedChangedVal == cty.NilVal:
				plannedChangedVal = cty.NullVal(priorChangedVal.Type())
			}

			// Unmark for this value for the equality test. If only sensitivity has changed,
			// this does not require an Update or Replace
			unmarkedPlannedChangedVal, _ := plannedChangedVal.UnmarkDeep()
			eqV := unmarkedPlannedChangedVal.Equals(priorChangedVal)
			if !eqV.IsKnown() || eqV.False() {
				reqRep.Add(path)
			}
		}
		if diags.HasErrors() {
			return nil, diags.Err()
		}
	}

	// Unmark for this test for value equality.
	eqV := unmarkedPlannedNewVal.Equals(unmarkedPriorVal)
	eq := eqV.IsKnown() && eqV.True()

	var action plans.Action
	switch {
	case priorVal.IsNull():
		action = plans.Create
	case eq:
		action = plans.NoOp
	case !reqRep.Empty():
		// If there are any "requires replace" paths left _after our filtering
		// above_ then this is a replace action.
		if createBeforeDestroy {
			action = plans.CreateThenDelete
		} else {
			action = plans.DeleteThenCreate
		}
	default:
		action = plans.Update
		// "Delete" is never chosen here, because deletion plans are always
		// created more directly elsewhere, such as in "orphan" handling.
	}

	if action.IsReplace() {
		// In this strange situation we want to produce a change object that
		// shows our real prior object but has a _new_ object that is built
		// from a null prior object, since we're going to delete the one
		// that has all the computed values on it.
		//
		// Therefore we'll ask the provider to plan again here, giving it
		// a null object for the prior, and then we'll meld that with the
		// _actual_ prior state to produce a correctly-shaped replace change.
		// The resulting change should show any computed attributes changing
		// from known prior values to unknown values, unless the provider is
		// able to predict new values for any of these computed attributes.
		nullPriorVal := cty.NullVal(schema.ImpliedType())

		// Since there is no prior state to compare after replacement, we need
		// a new unmarked config from our original with no ignored values.
		unmarkedConfigVal := origConfigVal
		if origConfigVal.ContainsMarked() {
			unmarkedConfigVal, _ = origConfigVal.UnmarkDeep()
		}

		// create a new proposed value from the null state and the config
		proposedNewVal = objchange.ProposedNewObject(schema, nullPriorVal, unmarkedConfigVal)

		resp = provider.PlanResourceChange(providers.PlanResourceChangeRequest{
			TypeName:         n.Addr.Resource.Type,
			Config:           unmarkedConfigVal,
			PriorState:       nullPriorVal,
			ProposedNewState: proposedNewVal,
			PriorPrivate:     plannedPrivate,
			ProviderMeta:     metaConfigVal,
		})
		// We need to tread carefully here, since if there are any warnings
		// in here they probably also came out of our previous call to
		// PlanResourceChange above, and so we don't want to repeat them.
		// Consequently, we break from the usual pattern here and only
		// append these new diagnostics if there's at least one error inside.
		if resp.Diagnostics.HasErrors() {
			diags = diags.Append(resp.Diagnostics.InConfigBody(config.Config))
			return nil, diags.Err()
		}
		plannedNewVal = resp.PlannedState
		plannedPrivate = resp.PlannedPrivate

		if len(unmarkedPaths) > 0 {
			plannedNewVal = plannedNewVal.MarkWithPaths(unmarkedPaths)
		}

		for _, err := range plannedNewVal.Type().TestConformance(schema.ImpliedType()) {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Provider produced invalid plan",
				fmt.Sprintf(
					"Provider %q planned an invalid value for %s%s.\n\nThis is a bug in the provider, which should be reported in the provider's own issue tracker.",
					n.ProviderAddr.Provider.String(), absAddr, tfdiags.FormatError(err),
				),
			))
		}
		if diags.HasErrors() {
			return nil, diags.Err()
		}
	}

	// If our prior value was tainted then we actually want this to appear
	// as a replace change, even though so far we've been treating it as a
	// create.
	if action == plans.Create && !priorValTainted.IsNull() {
		if createBeforeDestroy {
			action = plans.CreateThenDelete
		} else {
			action = plans.DeleteThenCreate
		}
		priorVal = priorValTainted
	}

	// If we plan to write or delete sensitive paths from state,
	// this is an Update action
	if action == plans.NoOp && !marksEqual(priorPaths, unmarkedPaths) {
		action = plans.Update
	}

	// As a special case, if we have a previous diff (presumably from the plan
	// phases, whereas we're now in the apply phase) and it was for a replace,
	// we've already deleted the original object from state by the time we
	// get here and so we would've ended up with a _create_ action this time,
	// which we now need to paper over to get a result consistent with what
	// we originally intended.
	if n.PreviousDiff != nil {
		prevChange := *n.PreviousDiff
		if prevChange.Action.IsReplace() && action == plans.Create {
			log.Printf("[TRACE] EvalDiff: %s treating Create change as %s change to match with earlier plan", absAddr, prevChange.Action)
			action = prevChange.Action
			priorVal = prevChange.Before
		}
	}

	// Call post-refresh hook
	if !n.Stub {
		err := ctx.Hook(func(h Hook) (HookAction, error) {
			return h.PostDiff(absAddr, states.CurrentGen, action, priorVal, plannedNewVal)
		})
		if err != nil {
			return nil, err
		}
	}

	// Update our output if we care
	if n.OutputChange != nil {
		*n.OutputChange = &plans.ResourceInstanceChange{
			Addr:         absAddr,
			Private:      plannedPrivate,
			ProviderAddr: n.ProviderAddr,
			Change: plans.Change{
				Action: action,
				Before: priorVal,
				// Pass the marked planned value through in our change
				// to propogate through evaluation.
				// Marks will be removed when encoding.
				After: plannedNewVal,
			},
			RequiredReplace: reqRep,
		}
	}

	// Update the state if we care
	if n.OutputState != nil {
		*n.OutputState = &states.ResourceInstanceObject{
			// We use the special "planned" status here to note that this
			// object's value is not yet complete. Objects with this status
			// cannot be used during expression evaluation, so the caller
			// must _also_ record the returned change in the active plan,
			// which the expression evaluator will use in preference to this
			// incomplete value recorded in the state.
			Status:  states.ObjectPlanned,
			Value:   plannedNewVal,
			Private: plannedPrivate,
		}
	}

	return nil, nil
}

func (n *EvalDiff) processIgnoreChanges(prior, config cty.Value) (cty.Value, tfdiags.Diagnostics) {
	// ignore_changes only applies when an object already exists, since we
	// can't ignore changes to a thing we've not created yet.
	if prior.IsNull() {
		return config, nil
	}

	ignoreChanges := n.Config.Managed.IgnoreChanges
	ignoreAll := n.Config.Managed.IgnoreAllChanges

	if len(ignoreChanges) == 0 && !ignoreAll {
		return config, nil
	}
	if ignoreAll {
		return prior, nil
	}
	if prior.IsNull() || config.IsNull() {
		// Ignore changes doesn't apply when we're creating for the first time.
		// Proposed should never be null here, but if it is then we'll just let it be.
		return config, nil
	}

	return processIgnoreChangesIndividual(prior, config, ignoreChanges)
}

func processIgnoreChangesIndividual(prior, config cty.Value, ignoreChanges []hcl.Traversal) (cty.Value, tfdiags.Diagnostics) {
	// When we walk below we will be using cty.Path values for comparison, so
	// we'll convert our traversals here so we can compare more easily.
	ignoreChangesPath := make([]cty.Path, len(ignoreChanges))
	for i, traversal := range ignoreChanges {
		path := make(cty.Path, len(traversal))
		for si, step := range traversal {
			switch ts := step.(type) {
			case hcl.TraverseRoot:
				path[si] = cty.GetAttrStep{
					Name: ts.Name,
				}
			case hcl.TraverseAttr:
				path[si] = cty.GetAttrStep{
					Name: ts.Name,
				}
			case hcl.TraverseIndex:
				path[si] = cty.IndexStep{
					Key: ts.Key,
				}
			default:
				panic(fmt.Sprintf("unsupported traversal step %#v", step))
			}
		}
		ignoreChangesPath[i] = path
	}

	type ignoreChange struct {
		// Path is the full path, minus any trailing map index
		path cty.Path
		// Value is the value we are to retain at the above path. If there is a
		// key value, this must be a map and the desired value will be at the
		// key index.
		value cty.Value
		// Key is the index key if the ignored path ends in a map index.
		key cty.Value
	}
	var ignoredValues []ignoreChange

	// Find the actual changes first and store them in the ignoreChange struct.
	// If the change was to a map value, and the key doesn't exist in the
	// config, it would never be visited in the transform walk.
	for _, icPath := range ignoreChangesPath {
		key := cty.NullVal(cty.String)
		// check for a map index, since maps are the only structure where we
		// could have invalid path steps.
		last, ok := icPath[len(icPath)-1].(cty.IndexStep)
		if ok {
			if last.Key.Type() == cty.String {
				icPath = icPath[:len(icPath)-1]
				key = last.Key
			}
		}

		// The structure should have been validated already, and we already
		// trimmed the trailing map index. Any other intermediate index error
		// means we wouldn't be able to apply the value below, so no need to
		// record this.
		p, err := icPath.Apply(prior)
		if err != nil {
			continue
		}
		c, err := icPath.Apply(config)
		if err != nil {
			continue
		}

		// If this is a map, it is checking the entire map value for equality
		// rather than the individual key. This means that the change is stored
		// here even if our ignored key doesn't change. That is OK since it
		// won't cause any changes in the transformation, but allows us to skip
		// breaking up the maps and checking for key existence here too.
		eq := p.Equals(c)
		if !eq.IsKnown() || eq.False() {
			// there a change to ignore at this path, store the prior value
			ignoredValues = append(ignoredValues, ignoreChange{icPath, p, key})
		}
	}

	if len(ignoredValues) == 0 {
		return config, nil
	}

	ret, _ := cty.Transform(config, func(path cty.Path, v cty.Value) (cty.Value, error) {
		// Easy path for when we are only matching the entire value. The only
		// values we break up for inspection are maps.
		if !v.Type().IsMapType() {
			for _, ignored := range ignoredValues {
				if path.Equals(ignored.path) {
					return ignored.value, nil
				}
			}
			return v, nil
		}
		// We now know this must be a map, so we need to accumulate the values
		// key-by-key.

		if !v.IsNull() && !v.IsKnown() {
			// since v is not known, we cannot ignore individual keys
			return v, nil
		}

		// The configMap is the current configuration value, which we will
		// mutate based on the ignored paths and the prior map value.
		var configMap map[string]cty.Value
		switch {
		case v.IsNull() || v.LengthInt() == 0:
			configMap = map[string]cty.Value{}
		default:
			configMap = v.AsValueMap()
		}

		for _, ignored := range ignoredValues {
			if !path.Equals(ignored.path) {
				continue
			}

			if ignored.key.IsNull() {
				// The map address is confirmed to match at this point,
				// so if there is no key, we want the entire map and can
				// stop accumulating values.
				return ignored.value, nil
			}
			// Now we know we are ignoring a specific index of this map, so get
			// the config map and modify, add, or remove the desired key.

			// We also need to create a prior map, so we can check for
			// existence while getting the value, because Value.Index will
			// return null for a key with a null value and for a non-existent
			// key.
			var priorMap map[string]cty.Value
			switch {
			case ignored.value.IsNull() || ignored.value.LengthInt() == 0:
				priorMap = map[string]cty.Value{}
			default:
				priorMap = ignored.value.AsValueMap()
			}

			key := ignored.key.AsString()
			priorElem, keep := priorMap[key]

			switch {
			case !keep:
				// this didn't exist in the old map value, so we're keeping the
				// "absence" of the key by removing it from the config
				delete(configMap, key)
			default:
				configMap[key] = priorElem
			}
		}

		if len(configMap) == 0 {
			return cty.MapValEmpty(v.Type().ElementType()), nil
		}

		return cty.MapVal(configMap), nil
	})
	return ret, nil
}

// EvalDiffDestroy is an EvalNode implementation that returns a plain
// destroy diff.
type EvalDiffDestroy struct {
	Addr         addrs.ResourceInstance
	DeposedKey   states.DeposedKey
	State        **states.ResourceInstanceObject
	ProviderAddr addrs.AbsProviderConfig

	Output      **plans.ResourceInstanceChange
	OutputState **states.ResourceInstanceObject
}

// TODO: test
func (n *EvalDiffDestroy) Eval(ctx EvalContext) (interface{}, error) {
	absAddr := n.Addr.Absolute(ctx.Path())
	state := *n.State

	if n.ProviderAddr.Provider.Type == "" {
		if n.DeposedKey == "" {
			panic(fmt.Sprintf("EvalDiffDestroy for %s does not have ProviderAddr set", absAddr))
		} else {
			panic(fmt.Sprintf("EvalDiffDestroy for %s (deposed %s) does not have ProviderAddr set", absAddr, n.DeposedKey))
		}
	}

	// If there is no state or our attributes object is null then we're already
	// destroyed.
	if state == nil || state.Value.IsNull() {
		return nil, nil
	}

	// Call pre-diff hook
	err := ctx.Hook(func(h Hook) (HookAction, error) {
		return h.PreDiff(
			absAddr, n.DeposedKey.Generation(),
			state.Value,
			cty.NullVal(cty.DynamicPseudoType),
		)
	})
	if err != nil {
		return nil, err
	}

	// Change is always the same for a destroy. We don't need the provider's
	// help for this one.
	// TODO: Should we give the provider an opportunity to veto this?
	change := &plans.ResourceInstanceChange{
		Addr:       absAddr,
		DeposedKey: n.DeposedKey,
		Change: plans.Change{
			Action: plans.Delete,
			Before: state.Value,
			After:  cty.NullVal(cty.DynamicPseudoType),
		},
		Private:      state.Private,
		ProviderAddr: n.ProviderAddr,
	}

	// Call post-diff hook
	err = ctx.Hook(func(h Hook) (HookAction, error) {
		return h.PostDiff(
			absAddr,
			n.DeposedKey.Generation(),
			change.Action,
			change.Before,
			change.After,
		)
	})
	if err != nil {
		return nil, err
	}

	// Update our output
	*n.Output = change

	if n.OutputState != nil {
		// Record our proposed new state, which is nil because we're destroying.
		*n.OutputState = nil
	}

	return nil, nil
}

// EvalReduceDiff is an EvalNode implementation that takes a planned resource
// instance change as might be produced by EvalDiff or EvalDiffDestroy and
// "simplifies" it to a single atomic action to be performed by a specific
// graph node.
//
// Callers must specify whether they are a destroy node or a regular apply
// node.  If the result is NoOp then the given change requires no action for
// the specific graph node calling this and so evaluation of the that graph
// node should exit early and take no action.
//
// The object written to OutChange may either be identical to InChange or
// a new change object derived from InChange. Because of the former case, the
// caller must not mutate the object returned in OutChange.
type EvalReduceDiff struct {
	Addr      addrs.ResourceInstance
	InChange  **plans.ResourceInstanceChange
	Destroy   bool
	OutChange **plans.ResourceInstanceChange
}

// TODO: test
func (n *EvalReduceDiff) Eval(ctx EvalContext) (interface{}, error) {
	in := *n.InChange
	out := in.Simplify(n.Destroy)
	if n.OutChange != nil {
		*n.OutChange = out
	}
	if out.Action != in.Action {
		if n.Destroy {
			log.Printf("[TRACE] EvalReduceDiff: %s change simplified from %s to %s for destroy node", n.Addr, in.Action, out.Action)
		} else {
			log.Printf("[TRACE] EvalReduceDiff: %s change simplified from %s to %s for apply node", n.Addr, in.Action, out.Action)
		}
	}
	return nil, nil
}

// EvalWriteDiff is an EvalNode implementation that saves a planned change
// for an instance object into the set of global planned changes.
type EvalWriteDiff struct {
	Addr           addrs.ResourceInstance
	DeposedKey     states.DeposedKey
	ProviderSchema **ProviderSchema
	Change         **plans.ResourceInstanceChange
}

// TODO: test
func (n *EvalWriteDiff) Eval(ctx EvalContext) (interface{}, error) {
	changes := ctx.Changes()
	addr := n.Addr.Absolute(ctx.Path())
	if n.Change == nil || *n.Change == nil {
		// Caller sets nil to indicate that we need to remove a change from
		// the set of changes.
		gen := states.CurrentGen
		if n.DeposedKey != states.NotDeposed {
			gen = n.DeposedKey
		}
		changes.RemoveResourceInstanceChange(addr, gen)
		return nil, nil
	}

	providerSchema := *n.ProviderSchema
	change := *n.Change

	if change.Addr.String() != addr.String() || change.DeposedKey != n.DeposedKey {
		// Should never happen, and indicates a bug in the caller.
		panic("inconsistent address and/or deposed key in EvalWriteDiff")
	}

	schema, _ := providerSchema.SchemaForResourceAddr(n.Addr.ContainingResource())
	if schema == nil {
		// Should be caught during validation, so we don't bother with a pretty error here
		return nil, fmt.Errorf("provider does not support resource type %q", n.Addr.Resource.Type)
	}

	csrc, err := change.Encode(schema.ImpliedType())
	if err != nil {
		return nil, fmt.Errorf("failed to encode planned changes for %s: %s", addr, err)
	}

	changes.AppendResourceInstanceChange(csrc)
	if n.DeposedKey == states.NotDeposed {
		log.Printf("[TRACE] EvalWriteDiff: recorded %s change for %s", change.Action, addr)
	} else {
		log.Printf("[TRACE] EvalWriteDiff: recorded %s change for %s deposed object %s", change.Action, addr, n.DeposedKey)
	}

	return nil, nil
}
