package policyregistry_test

import (
	"bytes"
	"context"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
	"weave-os/router/internal/policyregistry"
	"weave-os/router/internal/router/hmm/rosterdata"
)

const alternateRosterArm = "openai/gpt-6-sol"

func publishRosterArm(t *testing.T, store *servingMemoryStore, base policyregistry.PolicyObject, arm string) policyregistry.PolicyObject {
	t.Helper()
	roster := store.policies[policyregistry.ObjectRef{URI: base.URI, SHA256: base.SHA256, Generation: base.Generation}]
	payload, err := rosterdata.CanonicalBytes(roster)
	require.NoError(t, err)
	payload = bytes.ReplaceAll(payload, []byte(roster.AllArms()[0]), []byte(arm))
	changed, err := rosterdata.ParseValidated(payload)
	require.NoError(t, err)
	digest := policyregistry.Digest(payload)
	ref := policyregistry.ObjectRef{URI: testRegistryRoot + "/router_policy/v1/policies/sha256/" + digest + ".json", SHA256: digest, Generation: 1}
	store.policies[ref] = changed
	return policyregistry.PolicyObject{URI: ref.URI, SHA256: ref.SHA256, Generation: ref.Generation, SchemaVersion: changed.SchemaVersion}
}

func cohortFixture(t *testing.T) (*servingMemoryStore, policyregistry.SelectionSetV3) {
	t.Helper()
	store, _, legacy := controllerFixture(t)
	lane := foldLane(t, store, legacy.Default)
	candidate := store.object(t, policyregistry.ServingCandidate, lane.Candidate).(*policyregistry.CandidateV2)
	set := policyregistry.SelectionSetV3{SchemaVersion: policyregistry.ServingSelectionSetV3, Target: legacy.Target,
		Code: policyregistry.CodeBinding{Candidate: lane.Candidate, LaneBinding: lane.LaneBinding}, DefaultPolicy: candidate.Policy,
		Profiles: map[string]policyregistry.PolicyObject{profileKeyOne: candidate.Policy, profileKeyTwo: publishRosterArm(t, store, candidate.Policy, alternateRosterArm)}}
	return store, set
}

func cohortProposal(t *testing.T, store *servingMemoryStore, set policyregistry.SelectionSetV3, previous *policyregistry.ObjectRef, scope policyregistry.ChangeScope, profile string) policyregistry.DeploymentProposalV2 {
	t.Helper()
	return policyregistry.DeploymentProposalV2{SchemaVersion: policyregistry.ServingProposalV2, Target: set.Target, PreviousSelectionSet: previous,
		SelectionSet: store.publishArtifact(t, policyregistry.ServingSelectionSet, set), SourceCandidate: set.Code.Candidate, Scope: scope, ProfileKey: profile,
		Actor: "test-operator", Reason: "cohort configuration", RequestID: "synthetic-cohort-release", CreatedAt: servingEpoch,
		Evidence: []policyregistry.ObjectRef{artifactRef("evidence")}, WithdrawActivations: []string{}}
}

func TestSelectionSetV3StrictContractAndHistoricalDecoders(t *testing.T) {
	store, set := cohortFixture(t)
	payload := servingPayload(t, set)
	decoded, err := policyregistry.DecodePublishableServingManifest(payload, testRegistryRoot, policyregistry.ServingSelectionSet)
	require.NoError(t, err)
	require.Equal(t, &set, decoded)
	for name, mutate := range map[string]func(*policyregistry.SelectionSetV3){
		"missing profiles": func(s *policyregistry.SelectionSetV3) { s.Profiles = nil },
		"mutable policy":   func(s *policyregistry.SelectionSetV3) { s.DefaultPolicy.Generation = 0 },
		"foreign policy": func(s *policyregistry.SelectionSetV3) {
			s.Profiles[profileKeyOne] = policyregistry.PolicyObject{URI: "gs://foreign/policy"}
		},
		"profile key":     func(s *policyregistry.SelectionSetV3) { s.Profiles["untrusted-key"] = s.DefaultPolicy },
		"code generation": func(s *policyregistry.SelectionSetV3) { s.Code.Candidate.Generation = 0 },
		"target":          func(s *policyregistry.SelectionSetV3) { s.Target = "prod/customer" },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := set
			invalid.Profiles = maps.Clone(set.Profiles)
			mutate(&invalid)
			require.Error(t, invalid.Validate(testRegistryRoot))
		})
	}
	for _, invalid := range [][]byte{
		bytes.Replace(payload, []byte(`"code":{`), []byte(`"code":{"profile_key":"forbidden",`), 1),
		bytes.Replace(payload, []byte(`"default_policy":{`), []byte(`"default_policy":{"candidate":{},`), 1),
		append(bytes.Clone(payload), []byte(`{}`)...),
		bytes.Replace(payload, []byte(policyregistry.ServingSelectionSetV3), []byte(policyregistry.ServingSelectionSetV2), 1),
	} {
		_, err := policyregistry.DecodePublishableServingManifest(invalid, testRegistryRoot, policyregistry.ServingSelectionSet)
		require.Error(t, err)
	}
	ref := store.publishArtifact(t, policyregistry.ServingSelectionSet, set)
	for key, selection := range set.View(ref).Profiles {
		prepared, err := policyregistry.ReadPreparedSelection(context.Background(), store, set.Target, key, selection)
		require.NoError(t, err)
		require.Equal(t, set.Profiles[key], prepared.PolicyReference)
		require.Equal(t, set.DefaultPolicy, prepared.Candidate.Policy, "candidate remains its immutable build composition")
		require.Equal(t, set.Code.LaneBinding, prepared.Binding)
	}
	_, err = policyregistry.DecodeServingObject(payload, testRegistryRoot, policyregistry.ServingSelectionSet, namespaceRef(policyregistry.ServingSelectionSets, "v3-in-v1"))
	require.ErrorContains(t, err, "storage layout")
}

func TestSelectionSetV3RejectsUnavailableOrIncompatiblePolicy(t *testing.T) {
	for _, failure := range []string{"missing", "taxonomy", "schema", "catalog"} {
		t.Run(failure, func(t *testing.T) {
			store, set := cohortFixture(t)
			policy := set.Profiles[profileKeyTwo]
			ref := policyregistry.ObjectRef{URI: policy.URI, SHA256: policy.SHA256, Generation: policy.Generation}
			switch failure {
			case "missing":
				delete(store.policies, ref)
			case "taxonomy":
				store.policies[ref].ClassOrder = []string{"other"}
			case "schema":
				store.policies[ref].SchemaVersion = rosterdata.SchemaVersion("unsupported")
			case "catalog":
				store.policies[ref].Clusters["low"].Arms[0] = "unknown/unavailable"
			}
			setRef := store.publishArtifact(t, policyregistry.ServingSelectionSet, set)
			cache := cohortCache(t, store)
			_, err := cache.Snapshot(context.Background(), policyregistry.SessionReleaseBinding{Target: set.Target, ProfileKey: profileKeyTwo, Selection: set.View(setRef).Profiles[profileKeyTwo]})
			require.Error(t, err, "invalid assigned policy must not fall back to default or a cached customer")
		})
	}
}

func TestSelectionSetV3TransitionPreservesV2ProfilePolicies(t *testing.T) {
	store, controller, legacy, initial := heterogeneousLaneFixture(t)
	v2 := store.object(t, policyregistry.ServingSelectionSet, initial.Activation.SelectionSet).(*policyregistry.SelectionSetV2)
	candidate := store.object(t, policyregistry.ServingCandidate, v2.Default.Candidate).(*policyregistry.CandidateV2)
	set := policyregistry.SelectionSetV3{SchemaVersion: policyregistry.ServingSelectionSetV3, Target: legacy.Target,
		Code: policyregistry.CodeBinding{Candidate: v2.Default.Candidate, LaneBinding: v2.Default.LaneBinding}, DefaultPolicy: candidate.Policy, Profiles: map[string]policyregistry.PolicyObject{}}
	for key, lane := range v2.Profiles {
		set.Profiles[key] = *lane.ProfilePolicy
	}
	proposal := cohortProposal(t, store, set, &initial.Activation.SelectionSet, policyregistry.ChangeFull, "")
	require.NoError(t, controller.ValidateProposal(context.Background(), proposal))
	for key, old := range v2.View(initial.Activation.SelectionSet).Profiles {
		oldPrepared, err := policyregistry.ReadPreparedSelection(context.Background(), store, set.Target, key, old)
		require.NoError(t, err)
		nextPrepared, err := policyregistry.ReadPreparedSelection(context.Background(), store, set.Target, key, set.View(proposal.SelectionSet).Profiles[key])
		require.NoError(t, err)
		require.Equal(t, oldPrepared.PolicyReference, nextPrepared.PolicyReference)
		require.Equal(t, oldPrepared.Policy, nextPrepared.Policy)
	}
	set.Profiles[profileKeyTwo] = set.DefaultPolicy
	proposal.SelectionSet = store.publishArtifact(t, policyregistry.ServingSelectionSet, set)
	require.ErrorContains(t, controller.ValidateProposal(context.Background(), proposal), "retain destination profile revisions")
}
