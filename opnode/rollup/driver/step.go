package driver

import (
	"context"
	"fmt"
	"github.com/ethereum-optimism/optimistic-specs/opnode/rollup"
	"time"

	"github.com/ethereum/go-ethereum/core/types"

	"github.com/ethereum-optimism/optimistic-specs/opnode/eth"
	"github.com/ethereum-optimism/optimistic-specs/opnode/l2"
	"github.com/ethereum-optimism/optimistic-specs/opnode/rollup/derive"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

type DriverAPI interface {
	l2.EngineAPI
	l2.EthBackend
}

func Execute(ctx context.Context, rpc DriverAPI, payload *l2.ExecutionPayload) error {
	execCtx, cancel := context.WithTimeout(ctx, time.Second*5)
	defer cancel()
	execRes, err := rpc.ExecutePayload(execCtx, payload)
	if err != nil {
		return fmt.Errorf("failed to execute payload: %v", err)
	}
	switch execRes.Status {
	case l2.ExecutionValid:
		return nil
	case l2.ExecutionSyncing:
		return fmt.Errorf("failed to execute payload %s, node is syncing, latest valid hash is %s", payload.ID(), execRes.LatestValidHash)
	case l2.ExecutionInvalid:
		return fmt.Errorf("execution payload %s was INVALID! Latest valid hash is %s, ignoring bad block: %q", payload.ID(), execRes.LatestValidHash, execRes.ValidationError)
	default:
		return fmt.Errorf("unknown execution status on %s: %q, ", payload.ID(), string(execRes.Status))
	}
}

func ForkchoiceUpdate(ctx context.Context, rpc DriverAPI, l2BlockHash common.Hash, l2Finalized common.Hash) error {
	postState := &l2.ForkchoiceState{
		HeadBlockHash:      l2BlockHash, // no difference yet between Head and Safe, no data ahead of L1 yet.
		SafeBlockHash:      l2BlockHash,
		FinalizedBlockHash: l2Finalized,
	}

	fcCtx, cancel := context.WithTimeout(ctx, time.Second*5)
	defer cancel()
	fcRes, err := rpc.ForkchoiceUpdated(fcCtx, postState, nil)
	if err != nil {
		return fmt.Errorf("failed to update forkchoice: %v", err)
	}
	switch fcRes.Status {
	case l2.UpdateSyncing:
		return fmt.Errorf("updated forkchoice, but node is syncing: %v", err)
	case l2.UpdateSuccess:
		return nil
	default:
		return fmt.Errorf("unknown forkchoice status on %s: %q, ", l2BlockHash, string(fcRes.Status))
	}
}

type Downloader interface {
	Fetch(ctx context.Context, id eth.BlockID) (*types.Block, []*types.Receipt, error)
}

// DriverStep derives and processes one or more L2 blocks from the given sequencing window of L1 blocks.
// An incomplete sequencing window will result in an incomplete L2 chain if so.
//
// After the step completes it returns the block ID of the last processed L2 block, even if an error occurs.
func DriverStep(ctx context.Context, log log.Logger, config *rollup.Config, rpc DriverAPI,
	dl Downloader, l1Input []eth.BlockID, l2Parent eth.BlockID, l2Time uint64, l2Finalized common.Hash) (out eth.BlockID, err error) {

	logger := log.New("input_l1_first", l1Input[0], "input_l1_last", l1Input[len(l1Input)-1],
		"input_l2_parent", l2Parent, "finalized_l2", l2Finalized, "l2_time", l2Time)

	epoch := rollup.Epoch(l1Input[0].Number)

	// TODO: download full seq window
	//fetchCtx, cancel := context.WithTimeout(ctx, time.Second*20)
	//defer cancel()
	//
	//
	//bl, receipts, err := dl.Fetch(fetchCtx, l1Input)
	//if err != nil {
	//	return l2Parent, fmt.Errorf("failed to fetch block with receipts: %v", err)
	//}
	//logger.Debug("fetched L1 data for driver")
	seqWindow := []derive.BlockInput{}

	// TODO: update args
	attrsList, err := derive.PayloadAttributes(config, l2Time, seqWindow)
	if err != nil {
		return l2Parent, fmt.Errorf("failed to derive execution payload inputs: %v", err)
	}
	logger.Debug("derived L2 block inputs")

	last := l2Parent
	for i, attrs := range attrsList {
		last, err := AddBlock(ctx, logger, rpc, last, l2Finalized, attrs)
		if err != nil {
			return last, fmt.Errorf("failed to extend L2 chain at block %d/%d of epoch %d: %v", i, len(attrsList), epoch, err)
		}
	}

	return last, nil
}

// AddBlock extends the L2 chain by deriving the full execution payload from inputs,
// and then executing and persisting it.
//
// After the step completes it returns the block ID of the last processed L2 block, even if an error occurs.
func AddBlock(ctx context.Context, logger log.Logger, rpc DriverAPI,
	l2Parent eth.BlockID, l2Finalized common.Hash, attrs *l2.PayloadAttributes) (eth.BlockID, error) {

	payload, err := derive.ExecutionPayload(ctx, rpc, l2Parent.Hash, l2Finalized, attrs)
	if err != nil {
		return l2Parent, fmt.Errorf("failed to derive execution payload: %v", err)
	}

	logger = logger.New("derived_l2", payload.ID())
	logger.Info("derived full block")

	err = Execute(ctx, rpc, payload)
	if err != nil {
		return l2Parent, fmt.Errorf("failed to apply execution payload: %v", err)
	}
	logger.Info("executed block")

	err = ForkchoiceUpdate(ctx, rpc, payload.BlockHash, l2Finalized)
	if err != nil {
		return payload.ID(), fmt.Errorf("failed to persist execution payload: %v", err)
	}
	logger.Info("updated fork-choice with block")
	return payload.ID(), nil
}
