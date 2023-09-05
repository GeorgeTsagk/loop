package loopdb

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lntypes"
)

type Batch struct {
	// ID is the unique identifier of the batch.
	ID lntypes.Hash

	// BatchState is the current state of the batch.
	BatchState string

	// BatchTxid is the txid of the batch transaction.
	BatchTxid chainhash.Hash

	// BatchPkScript is the pkscript of the batch transaction.
	BatchPkScript []byte

	// LastRbfHeight is the height at which the last RBF attempt was made.
	LastRbfHeight int32

	// LastRbfSatPerKw is the sat per kw of the last RBF attempt.
	LastRbfSatPerKw int32

	// MaxTimeoutDistance is the maximum timeout distance of the batch.
	MaxTimeoutDistance int32
}

type Sweep struct {
	// ID is the unique identifier of the sweep.
	ID int32

	// BatchID is the ID of the batch that the sweep belongs to.
	BatchID lntypes.Hash

	// SwapHash is the hash of the swap that the sweep belongs to.
	SwapHash lntypes.Hash

	// Outpoint is the outpoint of the sweep.
	Outpoint wire.OutPoint

	// Amount is the amount of the sweep.
	Amount btcutil.Amount

	// LoopOut is the loop out that the sweep belongs to.
	LoopOut *LoopOut
}
