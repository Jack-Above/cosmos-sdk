package keeper

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/gov/types/v1beta1"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
)

// TODO: Break into several smaller functions for clarity

// Tally iterates over the votes and updates the tally of a proposal based on the voting power of the
// voters
func (keeper Keeper) Tally(ctx sdk.Context, proposal v1beta1.Proposal) (passes bool, burnDeposits bool, tallyResults v1beta1.TallyResult) {
	results := make(map[v1beta1.VoteOption]sdk.Dec)
	results[v1beta1.OptionYes] = sdk.ZeroDec()
	results[v1beta1.OptionAbstain] = sdk.ZeroDec()
	results[v1beta1.OptionNo] = sdk.ZeroDec()
	results[v1beta1.OptionNoWithVeto] = sdk.ZeroDec()

	totalVotingPower := sdk.ZeroDec()
	currValidators := make(map[string]v1beta1.ValidatorGovInfo)

	// fetch all the bonded validators, insert them into currValidators
	keeper.sk.IterateBondedValidatorsByPower(ctx, func(index int64, validator stakingtypes.ValidatorI) (stop bool) {
		currValidators[validator.GetOperator().String()] = v1beta1.NewValidatorGovInfo(
			validator.GetOperator(),
			validator.GetBondedTokens(),
			validator.GetDelegatorShares(),
			sdk.ZeroDec(),
			v1beta1.WeightedVoteOptions{},
		)

		return false
	})

	keeper.IterateVotes(ctx, proposal.ProposalId, func(vote v1beta1.Vote) bool {
		// if validator, just record it in the map
		voter, err := sdk.AccAddressFromBech32(vote.Voter)

		if err != nil {
			panic(err)
		}

		valAddrStr := sdk.ValAddress(voter.Bytes()).String()
		if val, ok := currValidators[valAddrStr]; ok {
			val.Vote = vote.Options
			currValidators[valAddrStr] = val
		}

		// iterate over all delegations from voter, deduct from any delegated-to validators
		keeper.sk.IterateDelegations(ctx, voter, func(index int64, delegation stakingtypes.DelegationI) (stop bool) {
			valAddrStr := delegation.GetValidatorAddr().String()

			if val, ok := currValidators[valAddrStr]; ok {
				// There is no need to handle the special case that validator address equal to voter address.
				// Because voter's voting power will tally again even if there will deduct voter's voting power from validator.
				val.DelegatorDeductions = val.DelegatorDeductions.Add(delegation.GetShares())
				currValidators[valAddrStr] = val

				// delegation shares * bonded / total shares
				votingPower := delegation.GetShares().MulInt(val.BondedTokens).Quo(val.DelegatorShares)

				for _, option := range vote.Options {
					subPower := votingPower.Mul(option.Weight)
					results[option.Option] = results[option.Option].Add(subPower)
				}
				totalVotingPower = totalVotingPower.Add(votingPower)
			}

			return false
		})

		keeper.deleteVote(ctx, vote.ProposalId, voter)
		return false
	})

	// iterate over the validators again to tally their voting power
	for _, val := range currValidators {
		if len(val.Vote) == 0 {
			continue
		}

		sharesAfterDeductions := val.DelegatorShares.Sub(val.DelegatorDeductions)
		votingPower := sharesAfterDeductions.MulInt(val.BondedTokens).Quo(val.DelegatorShares)

		for _, option := range val.Vote {
			subPower := votingPower.Mul(option.Weight)
			results[option.Option] = results[option.Option].Add(subPower)
		}
		totalVotingPower = totalVotingPower.Add(votingPower)
	}

	tallyParams := keeper.GetTallyParams(ctx)
	tallyResults = v1beta1.NewTallyResultFromMap(results)

	// TODO: Upgrade the spec to cover all of these cases & remove pseudocode.
	// If there is no staked coins, the proposal fails
	if keeper.sk.TotalBondedTokens(ctx).IsZero() {
		return false, false, tallyResults
	}

	// If there is not enough quorum of votes, the proposal fails
	percentVoting := totalVotingPower.Quo(keeper.sk.TotalBondedTokens(ctx).ToDec())
	if percentVoting.LT(tallyParams.Quorum) {
		return false, true, tallyResults
	}

	// If no one votes (everyone abstains), proposal fails
	if totalVotingPower.Sub(results[v1beta1.OptionAbstain]).Equal(sdk.ZeroDec()) {
		return false, false, tallyResults
	}

	// If more than 1/3 of voters veto, proposal fails
	if results[v1beta1.OptionNoWithVeto].Quo(totalVotingPower).GT(tallyParams.VetoThreshold) {
		return false, true, tallyResults
	}

	// If more than 1/2 of non-abstaining voters vote Yes, proposal passes
	if results[v1beta1.OptionYes].Quo(totalVotingPower.Sub(results[v1beta1.OptionAbstain])).GT(tallyParams.Threshold) {
		return true, false, tallyResults
	}

	// If more than 1/2 of non-abstaining voters vote No, proposal fails
	return false, false, tallyResults
}
