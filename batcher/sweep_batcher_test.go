package sweepbatcher

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestSweepBatcherBatchCreation tests that sweep requests enter the expected
// batch based on their timeout distance.
func TestSweepBatcherBatchCreation(t *testing.T) {
	lnd := test.NewMockLnd()
	ctx := context.Background()

	batcher := NewBatcher(ctx, lnd.WalletKit, lnd.ChainNotifier, lnd.Signer)
	go func() {
		err := batcher.Run(ctx)
		require.NoError(t, err)
	}()

	// Create a sweep request.
	sweep1 := &Sweep{
		SwapHash: lntypes.Hash{1, 1, 1},
		Value:    111,
		Timeout:  111111,
	}

	// Deliver sweep request to batcher.
	batcher.SweepReqs <- sweep1

	// Insert the same swap twice, this should not be inserted.
	batcher.SweepReqs <- sweep1

	// Once batcher receives sweep request it will eventually spin up a
	// batch.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 1
	}, test.Timeout, 500*time.Millisecond)

	// Create a second sweep request that has a timeout distance less than
	// our configured threshold.
	sweep2 := &Sweep{
		SwapHash: lntypes.Hash{2, 2, 2},
		Value:    222,
		Timeout:  111111 + DefaultMaxTimeoutDistance - 1,
	}

	batcher.SweepReqs <- sweep2

	// Batcher should not create a second batch as timeout distance is small
	// enough.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 1
	}, test.Timeout, 500*time.Millisecond)

	// Create a third sweep request that has more timeout distance than
	// the default.
	sweep3 := &Sweep{
		SwapHash: lntypes.Hash{3, 3, 3},
		Value:    333,
		Timeout:  111111 + DefaultMaxTimeoutDistance + 1,
	}

	batcher.SweepReqs <- sweep3

	// Batcher should create a second batch as timeout distance is greater
	// than the threshold
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 2
	}, test.Timeout, 500*time.Millisecond)

	// Verify that each batch has the correct number of sweeps in it.
	for _, batch := range batcher.batches {
		switch batch.PrimarySweepID {
		case sweep1.SwapHash:
			require.Equal(t, len(batch.Sweeps), 2)

		case sweep3.SwapHash:
			require.Equal(t, len(batch.Sweeps), 1)
		}
	}
}

// TestSweepBatcherSimpleLifecycle tests the simple lifecycle of the batches
// that are created and run by the batcher.
func TestSweepBatcherSimpleLifecycle(t *testing.T) {
	lnd := test.NewMockLnd()
	tctx := test.NewContext(t, lnd)
	ctx := context.Background()

	batcher := NewBatcher(ctx, lnd.WalletKit, lnd.ChainNotifier, lnd.Signer)
	go func() {
		err := batcher.Run(ctx)
		require.NoError(t, err)
	}()

	// Create a sweep request.
	sweep1 := &Sweep{
		SwapHash: lntypes.Hash{1, 1, 1},
		Value:    111,
		Timeout:  111111,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{1, 1},
			Index: 1,
		},
	}

	// Deliver sweep request to batcher.
	batcher.SweepReqs <- sweep1

	// Eventually request will be consumed and a new batch will spin up.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 1
	}, test.Timeout, 500*time.Millisecond)

	// When batch is successfully created it will execute it's first step,
	// which leads to a spend monitor of the primary sweep.
	<-tctx.Lnd.RegisterSpendChannel

	// Find the batch and assign it to a local variable for easier access.
	batch := &Batch{}
	for _, btch := range batcher.batches {
		if btch.PrimarySweepID == sweep1.SwapHash {
			batch = btch
		}
	}

	// Batch should have the sweep stored.
	require.Len(t, batch.Sweeps, 1)
	// The primary sweep id should be that of the first inserted sweep.
	require.Equal(t, batch.PrimarySweepID, sweep1.SwapHash)

	err := lnd.NotifyHeight(601)
	require.NoError(t, err)

	// After receiving a height notification the batch will step again,
	// leading to a new spend monitoring.
	require.Eventually(t, func() bool {
		return batch.currentHeight == 601
	}, test.Timeout, 500*time.Millisecond)

	<-tctx.Lnd.RegisterSpendChannel

	// Create the spending tx that will trigger the spend monitor of the
	// batch.
	spendingTx := &wire.MsgTx{
		Version: 1,
		// Since the spend monitor is registered on the primary sweep's
		// outpoint we insert that outpoint here.
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: sweep1.Outpoint,
			},
		},
		TxOut: []*wire.TxOut{
			{
				PkScript: []byte{3, 2, 1},
			},
		},
	}

	spendingTxHash := spendingTx.TxHash()

	// Send the spending tx to the mock spend channel.
	lnd.SpendChannel <- &chainntnfs.SpendDetail{
		SpentOutPoint:     &sweep1.Outpoint,
		SpendingTx:        spendingTx,
		SpenderTxHash:     &spendingTxHash,
		SpenderInputIndex: 0,
		SpendingHeight:    601,
	}

	// The batch should eventually read the spend notification and progress
	// its state to closed.
	require.Eventually(t, func() bool {
		return batch.State == Closed
	}, test.Timeout, 500*time.Millisecond)

	err = lnd.NotifyHeight(602)
	require.NoError(t, err)

	// The batch stepped again after receiving a height notification, but
	// this time instead of monitoring the spend it start monitoring the
	// transaction confirmations.
	<-tctx.Lnd.RegisterConfChannel

	// We mock the tx confirmation notification.
	lnd.ConfChannel <- &chainntnfs.TxConfirmation{
		BlockHeight: 605,
		TxIndex:     1,
		Tx:          spendingTx,
	}

	// Eventually the batch receives the confirmation notification and
	// gracefully exists by providing the exit signal to the batcher. After
	// receiving that signal the batcher should delete the batch.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 0
	}, test.Timeout, 500*time.Millisecond)
}

// TestSweepBatcherSweepReentry tests that when an old version of the batch tx
// gets confirmed the sweep leftovers are sent back to the batcher.
func TestSweepBatcherSweepReentry(t *testing.T) {
	lnd := test.NewMockLnd()
	tctx := test.NewContext(t, lnd)
	ctx := context.Background()

	batcher := NewBatcher(ctx, lnd.WalletKit, lnd.ChainNotifier, lnd.Signer)
	go func() {
		err := batcher.Run(ctx)
		require.NoError(t, err)
	}()

	// Create some sweep requests with timeouts not too far away, in order
	// to enter the same batch.
	sweep1 := &Sweep{
		SwapHash: lntypes.Hash{1, 1, 1},
		Value:    111,
		Timeout:  111111,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{1, 1},
			Index: 1,
		},
	}

	sweep2 := &Sweep{
		SwapHash: lntypes.Hash{2, 2, 2},
		Value:    222,
		Timeout:  111112,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{2, 2},
			Index: 2,
		},
	}

	sweep3 := &Sweep{
		SwapHash: lntypes.Hash{3, 3, 3},
		Value:    333,
		Timeout:  111113,
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{3, 3},
			Index: 3,
		},
	}

	// Feed the sweeps to the batcher.
	batcher.SweepReqs <- sweep1
	batcher.SweepReqs <- sweep2
	batcher.SweepReqs <- sweep3

	// Batcher should create a batch for the sweeps.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 1
	}, test.Timeout, 500*time.Millisecond)

	// After its creation, the batch registers its first spend monitor.
	<-tctx.Lnd.RegisterSpendChannel

	// Find the batch and store it in a local variable for easier access.
	batch := &Batch{}
	for _, btch := range batcher.batches {
		if btch.PrimarySweepID == sweep1.SwapHash {
			batch = btch
		}
	}

	// Verify that the batch contains 3 sweeps.
	require.Len(t, batch.Sweeps, 3)
	// Verify that the batch has a primary sweep id that matches the first
	// inserted sweep, sweep1.
	require.Equal(t, batch.PrimarySweepID, sweep1.SwapHash)

	// Create the spending tx. In order to simulate an older version of the
	// batch transaction being confirmed, we only insert the primary sweep's
	// outpoint as a TxIn. This means that the other two sweeps did not
	// appear in the spending transaction. (This simulates a possible
	// scenario caused by RBF replacements.)
	spendingTx := &wire.MsgTx{
		Version: 1,
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: sweep1.Outpoint,
			},
		},
		TxOut: []*wire.TxOut{
			{
				Value:    int64(sweep1.Value.ToUnit(btcutil.AmountSatoshi)),
				PkScript: []byte{3, 2, 1},
			},
		},
	}

	spendingTxHash := spendingTx.TxHash()

	// Send the spending notification to the mock channel.
	lnd.SpendChannel <- &chainntnfs.SpendDetail{
		SpentOutPoint:     &sweep1.Outpoint,
		SpendingTx:        spendingTx,
		SpenderTxHash:     &spendingTxHash,
		SpenderInputIndex: 0,
		SpendingHeight:    601,
	}

	// Eventually the batch reads the notification and proceeds to a closed
	// state.
	require.Eventually(t, func() bool {
		return batch.State == Closed
	}, test.Timeout, 500*time.Millisecond)

	// While handling the spend notification the batch should detect that
	// some sweeps did not appear in the spending tx, therefore it redirects
	// them back to the batcher and the batcher inserts them in a new batch.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 2
	}, test.Timeout, 500*time.Millisecond)

	err := lnd.NotifyHeight(602)
	require.NoError(t, err)

	// Upon stepping after the height notification, the batch should
	// register its confirmation monitor.
	<-tctx.Lnd.RegisterConfChannel

	// We mock the confirmation notification.
	lnd.ConfChannel <- &chainntnfs.TxConfirmation{
		BlockHeight: 605,
		TxIndex:     1,
		Tx:          spendingTx,
	}

	// Eventually the batch receives the confirmation notification,
	// gracefully exits and the batcher deletes it.
	require.Eventually(t, func() bool {
		return len(batcher.batches) == 1
	}, test.Timeout, 500*time.Millisecond)

	// Find the other batch, which includes the sweeps that did not appear
	// in the spending tx.
	batch = &Batch{}
	for _, btch := range batcher.batches {
		batch = btch
	}

	// It should contain 2 sweeps.
	require.Len(t, batch.Sweeps, 2)
	// The batch should be in an open state.
	require.Equal(t, batch.State, Open)
}
