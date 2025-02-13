package baseapp

import (
	"encoding/json"
	"fmt"

	"github.com/cockroachdb/errors"
	abci "github.com/cometbft/cometbft/abci/types"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/mempool"
	"github.com/cosmos/cosmos-sdk/x/auth/signing"
)

type (
	// GasTx defines the contract that a transaction with a gas limit must implement.
	GasTx interface {
		GetGas() uint64
	}

	// ProposalTxVerifier defines the interface that is implemented by BaseApp,
	// that any custom ABCI PrepareProposal and ProcessProposal handler can use
	// to verify a transaction.
	ProposalTxVerifier interface {
		PrepareProposalVerifyTx(tx sdk.Tx) ([]byte, error)
		ProcessProposalVerifyTx(txBz []byte) (sdk.Tx, error)
	}

	// DefaultProposalHandler defines the default ABCI PrepareProposal and
	// ProcessProposal handlers.
	DefaultProposalHandler struct {
		mempool    mempool.Mempool
		txVerifier ProposalTxVerifier
		txSelector TxSelector
	}
)

func NewDefaultProposalHandler(mp mempool.Mempool, txVerifier ProposalTxVerifier) *DefaultProposalHandler {
	return &DefaultProposalHandler{
		mempool:    mp,
		txVerifier: txVerifier,
		txSelector: NewDefaultTxSelector(),
	}
}

// SetTxSelector sets the TxSelector function on the DefaultProposalHandler.
func (h *DefaultProposalHandler) SetTxSelector(ts TxSelector) {
	h.txSelector = ts
}

// PrepareProposalHandler returns the default implementation for processing an
// ABCI proposal. The application's mempool is enumerated and all valid
// transactions are added to the proposal. Transactions are valid if they:
//
// 1) Successfully encode to bytes.
// 2) Are valid (i.e. pass runTx, AnteHandler only).
//
// Enumeration is halted once RequestPrepareProposal.MaxBytes of transactions is
// reached or the mempool is exhausted.
//
// Note:
//
// - Step (2) is identical to the validation step performed in
// DefaultProcessProposal. It is very important that the same validation logic
// is used in both steps, and applications must ensure that this is the case in
// non-default handlers.
//
// - If no mempool is set or if the mempool is a no-op mempool, the transactions
// requested from CometBFT will simply be returned, which, by default, are in
// FIFO order.
func (h *DefaultProposalHandler) PrepareProposalHandler() sdk.PrepareProposalHandler {
	return func(ctx sdk.Context, req abci.RequestPrepareProposal) abci.ResponsePrepareProposal {
		var maxBlockGas uint64
		if b := ctx.ConsensusParams().Block; b != nil {
			maxBlockGas = uint64(b.MaxGas)
		}

		defer h.txSelector.Clear()

		// If the mempool is nil or NoOp we simply return the transactions
		// requested from CometBFT, which, by default, should be in FIFO order.
		//
		// Note, we still need to ensure the transactions returned respect req.MaxTxBytes.
		_, isNoOp := h.mempool.(mempool.NoOpMempool)
		if h.mempool == nil || isNoOp {
			for _, txBz := range req.Txs {
				// XXX: We pass nil as the memTx because we have no way of decoding the
				// txBz. We'd need to break (update) the ProposalTxVerifier interface.
				// As a result, we CANNOT account for block max gas.
				stop := h.txSelector.SelectTxForProposal(uint64(req.MaxTxBytes), maxBlockGas, nil, txBz)
				if stop {
					break
				}
			}

			return abci.ResponsePrepareProposal{Txs: h.txSelector.SelectedTxs()}
		}

		iterator := h.mempool.Select(ctx, req.Txs)
		selectedTxsSignersSeqs := make(map[string]uint64)
		var selectedTxsNums int
		for iterator != nil {
			memTx := iterator.Tx()
			sigs, err := memTx.(signing.SigVerifiableTx).GetSignaturesV2()
			if err != nil {
				panic(fmt.Errorf("failed to get signatures: %w", err))
			}

			// If the signers aren't in selectedTxsSignersSeqs then we haven't seen them before
			// so we add them and continue given that we don't need to check the sequence.
			shouldAdd := true
			txSignersSeqs := make(map[string]uint64)
			for _, sig := range sigs {
				signer := sdk.AccAddress(sig.PubKey.Address()).String()
				seq, ok := selectedTxsSignersSeqs[signer]
				if !ok {
					txSignersSeqs[signer] = sig.Sequence
					continue
				}

				// If we have seen this signer before in this block, we must make
				// sure that the current sequence is seq+1; otherwise is invalid
				// and we skip it.
				if seq+1 != sig.Sequence {
					shouldAdd = false
					break
				}
				txSignersSeqs[signer] = sig.Sequence
			}
			if !shouldAdd {
				iterator = iterator.Next()
				continue
			}

			// NOTE: Since transaction verification was already executed in CheckTx,
			// which calls mempool.Insert, in theory everything in the pool should be
			// valid. But some mempool implementations may insert invalid txs, so we
			// check again.
			txBz, err := h.txVerifier.PrepareProposalVerifyTx(memTx)
			if err != nil {
				err := h.mempool.Remove(memTx)
				if err != nil && !errors.Is(err, mempool.ErrTxNotFound) {
					panic(err)
				}
			} else {
				stop := h.txSelector.SelectTxForProposal(uint64(req.MaxTxBytes), maxBlockGas, memTx, txBz)
				if stop {
					break
				}

				txsLen := len(h.txSelector.SelectedTxs())
				for sender, seq := range txSignersSeqs {
					// If txsLen != selectedTxsNums is true, it means that we've
					// added a new tx to the selected txs, so we need to update
					// the sequence of the sender.
					if txsLen != selectedTxsNums {
						selectedTxsSignersSeqs[sender] = seq
					} else if _, ok := selectedTxsSignersSeqs[sender]; !ok {
						// The transaction hasn't been added but it passed the
						// verification, so we know that the sequence is correct.
						// So we set this sender's sequence to seq-1, in order
						// to avoid unnecessary calls to PrepareProposalVerifyTx.
						selectedTxsSignersSeqs[sender] = seq - 1
					}
				}
				selectedTxsNums = txsLen
			}

			iterator = iterator.Next()
		}

		return abci.ResponsePrepareProposal{Txs: h.txSelector.SelectedTxs()}
	}
}

// ProcessProposalHandler returns the default implementation for processing an
// ABCI proposal. Every transaction in the proposal must pass 2 conditions:
//
// 1. The transaction bytes must decode to a valid transaction.
// 2. The transaction must be valid (i.e. pass runTx, AnteHandler only)
//
// If any transaction fails to pass either condition, the proposal is rejected.
// Note that step (2) is identical to the validation step performed in
// DefaultPrepareProposal. It is very important that the same validation logic
// is used in both steps, and applications must ensure that this is the case in
// non-default handlers.
func (h *DefaultProposalHandler) ProcessProposalHandler() sdk.ProcessProposalHandler {
	// If the mempool is nil or NoOp we simply return ACCEPT,
	// because PrepareProposal may have included txs that could fail verification.
	_, isNoOp := h.mempool.(mempool.NoOpMempool)
	if h.mempool == nil || isNoOp {
		return NoOpProcessProposal()
	}

	return func(ctx sdk.Context, req abci.RequestProcessProposal) abci.ResponseProcessProposal {
		var totalTxGas uint64

		var maxBlockGas int64
		if b := ctx.ConsensusParams().Block; b != nil {
			maxBlockGas = b.MaxGas
		}

		for i, txBytes := range req.Txs {
			tx, err := h.txVerifier.ProcessProposalVerifyTx(txBytes)
			if err != nil {
				proposal, err := json.Marshal(req)
				if err == nil {
					ctx.Logger().Error("proposal failed on ProcessProposalVerifyTx", "error", err, "proposal", string(proposal), "tx_index", i)
				}
				return abci.ResponseProcessProposal{Status: abci.ResponseProcessProposal_REJECT}
			}

			if maxBlockGas > 0 {
				gasTx, ok := tx.(GasTx)
				if ok {
					totalTxGas += gasTx.GetGas()
				}

				if totalTxGas > uint64(maxBlockGas) {
					proposal, err := json.Marshal(req)
					if err == nil {
						ctx.Logger().Error("proposal failed on ProcessProposalVerifyTx", "error", err, "proposal", string(proposal), "tx_index", i)
					}
					ctx.Logger().Error("proposal failed on totalTxGas > maxBlockGas", "totalTxGas", totalTxGas, "maxBlockGas", maxBlockGas)
					return abci.ResponseProcessProposal{Status: abci.ResponseProcessProposal_REJECT}
				}
			}
		}

		return abci.ResponseProcessProposal{Status: abci.ResponseProcessProposal_ACCEPT}
	}
}

// NoOpPrepareProposal defines a no-op PrepareProposal handler. It will always
// return the transactions sent by the client's request.
func NoOpPrepareProposal() sdk.PrepareProposalHandler {
	return func(_ sdk.Context, req abci.RequestPrepareProposal) abci.ResponsePrepareProposal {
		return abci.ResponsePrepareProposal{Txs: req.Txs}
	}
}

// NoOpProcessProposal defines a no-op ProcessProposal Handler. It will always
// return ACCEPT.
func NoOpProcessProposal() sdk.ProcessProposalHandler {
	return func(_ sdk.Context, _ abci.RequestProcessProposal) abci.ResponseProcessProposal {
		return abci.ResponseProcessProposal{Status: abci.ResponseProcessProposal_ACCEPT}
	}
}

// TxSelector defines a helper type that assists in selecting transactions during
// mempool transaction selection in PrepareProposal. It keeps track of the total
// number of bytes and total gas of the selected transactions. It also keeps
// track of the selected transactions themselves.
type TxSelector interface {
	// SelectedTxs should return a copy of the selected transactions.
	SelectedTxs() [][]byte

	// Clear should clear the TxSelector, nulling out all relevant fields.
	Clear()

	// SelectTxForProposal should attempt to select a transaction for inclusion in
	// a proposal based on inclusion criteria defined by the TxSelector. It must
	// return <true> if the caller should halt the transaction selection loop
	// (typically over a mempool) or <false> otherwise.
	SelectTxForProposal(maxTxBytes, maxBlockGas uint64, memTx sdk.Tx, txBz []byte) bool
}

type defaultTxSelector struct {
	totalTxBytes uint64
	totalTxGas   uint64
	selectedTxs  [][]byte
}

func NewDefaultTxSelector() TxSelector {
	return &defaultTxSelector{}
}

func (ts *defaultTxSelector) SelectedTxs() [][]byte {
	txs := make([][]byte, len(ts.selectedTxs))
	copy(txs, ts.selectedTxs)
	return txs
}

func (ts *defaultTxSelector) Clear() {
	ts.totalTxBytes = 0
	ts.totalTxGas = 0
	ts.selectedTxs = nil
}

func (ts *defaultTxSelector) SelectTxForProposal(maxTxBytes, maxBlockGas uint64, memTx sdk.Tx, txBz []byte) bool {
	txSize := uint64(len(txBz))

	var txGasLimit uint64
	if memTx != nil {
		if gasTx, ok := memTx.(GasTx); ok {
			txGasLimit = gasTx.GetGas()
		}
	}

	// only add the transaction to the proposal if we have enough capacity
	if (txSize + ts.totalTxBytes) <= maxTxBytes {
		// If there is a max block gas limit, add the tx only if the limit has
		// not been met.
		if maxBlockGas > 0 {
			if (txGasLimit + ts.totalTxGas) <= maxBlockGas {
				ts.totalTxGas += txGasLimit
				ts.totalTxBytes += txSize
				ts.selectedTxs = append(ts.selectedTxs, txBz)
			}
		} else {
			ts.totalTxBytes += txSize
			ts.selectedTxs = append(ts.selectedTxs, txBz)
		}
	}

	// check if we've reached capacity; if so, we cannot select any more transactions
	return ts.totalTxBytes >= maxTxBytes || (maxBlockGas > 0 && (ts.totalTxGas >= maxBlockGas))
}
