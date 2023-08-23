package sweepbatcher

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/zpay32"
)

const (
	// DefaultMaxTimeoutDistance is the default maximum timeout distance
	// of sweeps that can appear in the same batch.
	DefaultMaxTimeoutDistance = 288
)

type BatcherStore interface {
	// FetchUnconfirmedSweepBatches fetches all the batches from the database that are
	// not in a confirmed state.
	FetchUnconfirmedSweepBatches(ctx context.Context) ([]*loopdb.Batch, error)

	// UpsertSweepBatch inserts a batch into the database, or updates an existing batch
	// if it already exists.
	UpsertSweepBatch(ctx context.Context, batch *loopdb.Batch) error

	// ConfirmBatch confirms a batch by setting its state to confirmed.
	ConfirmBatch(ctx context.Context, id []byte) error

	// FetchBatchSweeps fetches all the sweeps that belong to a batch.
	FetchBatchSweeps(ctx context.Context,
		id []byte) ([]*loopdb.Sweep, error)

	// UspertSweep inserts a sweep into the database, or updates an existing
	// sweep if it already exists.
	UpsertSweep(ctx context.Context, sweep *loopdb.Sweep) error

	// FetchLoopOutSwap fetches a loop out swap from the database.
	FetchLoopOutSwap(ctx context.Context,
		hash lntypes.Hash) (*loopdb.LoopOut, error)
}

type MuSig2SignSweep func(ctx context.Context,
	protocolVersion loopdb.ProtocolVersion, swapHash lntypes.Hash,
	paymentAddr [32]byte, nonce []byte, sweepTxPsbt []byte,
	prevoutMap map[wire.OutPoint]*wire.TxOut) (
	[]byte, []byte, error)

type SweepRequest struct {
	SwapHash lntypes.Hash
	Outpoint wire.OutPoint
	Value    btcutil.Amount
}

// Batcher is a system that is responsible for accepting sweep requests and
// placing them in appropriate batches. It will spin up new batches as needed.
type Batcher struct {
	// batches is a map of batch IDs to the currently active batches.
	batches map[lntypes.Hash]*Batch

	// SweepReqs is a channel where sweep requests are received.
	SweepReqs chan SweepRequest

	// errChan is a channel where errors are received.
	errChan chan error

	// completeChan is a channel where batches signal their completion by
	// providing their ID.
	completeChan chan lntypes.Hash

	// wallet is the wallet kit client that is used by batches.
	wallet lndclient.WalletKitClient

	// chainNotifier is the chain notifier client that is used by batches.
	chainNotifier lndclient.ChainNotifierClient

	// signerClient is the signer client that is used by batches.
	signerClient lndclient.SignerClient

	// musig2ServerKit includes all the required functionality to collect
	// and verify signatures by the swap server in order to cooperatively
	// sweep funds.
	musig2ServerSign MuSig2SignSweep

	// chainParams are the chain parameters of the chain that is used by
	// batches.
	chainParams *chaincfg.Params

	// store includes all the database interactions that are needed by the
	// batcher and the batches.
	store BatcherStore
}

// NewBatcher creates a new Batcher instance.
func NewBatcher(wallet lndclient.WalletKitClient,
	chainNotifier lndclient.ChainNotifierClient,
	signerClient lndclient.SignerClient, musig2ServerSigner MuSig2SignSweep,
	chainparams *chaincfg.Params, store BatcherStore) *Batcher {

	return &Batcher{
		batches:          make(map[lntypes.Hash]*Batch),
		SweepReqs:        make(chan SweepRequest),
		errChan:          make(chan error),
		completeChan:     make(chan lntypes.Hash),
		wallet:           wallet,
		chainNotifier:    chainNotifier,
		signerClient:     signerClient,
		musig2ServerSign: musig2ServerSigner,
		chainParams:      chainparams,
		store:            store,
	}
}

// Run starts the batcher and processes incoming sweep requests.
func (br *Batcher) Run(ctx context.Context) error {
	// First we fetch all the batches that are not in a confirmed state from
	// the database. We will then resume the execution of these batches.
	batches, err := br.FetchUnconfirmedBatches(ctx)
	if err != nil {
		return err
	}

	for _, batch := range batches {
		_, err := br.spinUpBatchFromDB(ctx, *batch)
		if err != nil {
			return err
		}
	}

	for {
		select {
		case sweepReq := <-br.SweepReqs:
			sweep, err := br.FetchSweep(ctx, sweepReq)
			if err != nil {
				return err
			}

			err = br.handleSweep(ctx, sweep)
			if err != nil {
				return err
			}

		case id := <-br.completeChan:
			delete(br.batches, id)

		case err := <-br.errChan:
			return err

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// handleSweep handles a sweep request by either placing it in an existing
// batch, or by spinning up a new batch for it.
func (br *Batcher) handleSweep(ctx context.Context, sweep *Sweep) error {
	// Check if the sweep is already in a batch. If that is the case, we
	// provide the sweep to that batch and return.
	for _, batch := range br.batches {
		if batch.SweepExists(sweep.SwapHash) {
			return writeToBatchChan(ctx, batch, sweep)
		}
	}

	// If one of the batches accepts the sweep, we provide it to that batch.
	for _, batch := range br.batches {
		if batch.AcceptsSweep(sweep) {
			return writeToBatchChan(ctx, batch, sweep)
		}
	}

	// If no batch is capable of accepting the sweep, we spin up a fresh
	// batch and hand the sweep over to it.
	batch, err := br.spinUpBatch(ctx)
	if err != nil {
		return err
	}

	return writeToBatchChan(ctx, batch, sweep)
}

// spinUpBatch spins up a new batch and returns it.
func (br *Batcher) spinUpBatch(ctx context.Context) (*Batch, error) {
	cfg := BatchConfig{
		MaxTimeoutDistance: DefaultMaxTimeoutDistance,
		BatchConfTarget:    defaultBatchConfTarget,
	}

	batchKit := BatchKit{
		returnChan:      br.SweepReqs,
		completeChan:    br.completeChan,
		wallet:          br.wallet,
		chainNotifier:   br.chainNotifier,
		signerClient:    br.signerClient,
		musig2SignSweep: br.musig2ServerSign,
		store:           br.store,
	}

	batch, err := NewBatch(cfg, batchKit)
	if err != nil {
		return nil, err
	}

	// We add the batch to our map of batches and start it.
	br.batches[batch.ID] = batch
	go func() {
		err := batch.Run(ctx)
		if err != nil {
			br.errChan <- err
		}
	}()

	return batch, nil
}

// spinUpBatchDB spins up a a batch that already existed in storage, then
// returns it.
func (br *Batcher) spinUpBatchFromDB(ctx context.Context, batch Batch) (*Batch, error) {
	cfg := BatchConfig{
		MaxTimeoutDistance: batch.Cfg.MaxTimeoutDistance,
		BatchConfTarget:    defaultBatchConfTarget,
	}

	rbfCache := RBFCache{
		LastHeight: batch.RbfCache.LastHeight,
		FeeRate:    batch.RbfCache.FeeRate,
	}

	dbSweeps, err := br.store.FetchBatchSweeps(ctx, batch.ID[:])
	if err != nil {
		return nil, err
	}

	primarySweep := dbSweeps[0]

	sweeps := make(map[lntypes.Hash]Sweep)

	for _, dbSweep := range dbSweeps {
		sweep, err := br.ConvertSweep(ctx, dbSweep)
		if err != nil {
			return nil, err
		}

		sweeps[sweep.SwapHash] = *sweep
	}

	batchKit := BatchKit{
		ID:              batch.ID,
		batchTxid:       batch.BatchTxid,
		batchPkScript:   batch.BatchPkScript,
		state:           batch.State,
		primaryID:       primarySweep.SwapHash,
		sweeps:          sweeps,
		rbfCache:        rbfCache,
		returnChan:      br.SweepReqs,
		completeChan:    br.completeChan,
		wallet:          br.wallet,
		chainNotifier:   br.chainNotifier,
		signerClient:    br.signerClient,
		musig2SignSweep: br.musig2ServerSign,
		store:           br.store,
	}

	newBatch, err := NewBatchFromDB(cfg, batchKit)
	if err != nil {
		return nil, err
	}

	// We add the batch to our map of batches and start it.
	br.batches[batch.ID] = newBatch
	go func() {
		err := newBatch.Run(ctx)
		if err != nil {
			br.errChan <- err
		}
	}()

	return newBatch, nil
}

// FetchUnconfirmedBatches fetches all the batches from the database that are
// not in a confirmed state.
func (br *Batcher) FetchUnconfirmedBatches(ctx context.Context) ([]*Batch,
	error) {

	dbBatches, err := br.store.FetchUnconfirmedSweepBatches(ctx)
	if err != nil {
		return nil, err
	}

	batches := make([]*Batch, 0, len(dbBatches))
	for _, bch := range dbBatches {
		batch := Batch{}
		batch.ID = bch.ID

		switch bch.BatchState {
		case "open":
			batch.State = Open

		case "closed":
			batch.State = Closed

		case "confirmed":
			batch.State = Confirmed
		}

		batch.BatchTxid = &bch.BatchTxid
		batch.BatchPkScript = bch.BatchPkScript

		rbfCache := RBFCache{
			LastHeight: bch.LastRbfHeight,
			FeeRate:    chainfee.SatPerKWeight(bch.LastRbfSatPerKw),
		}
		batch.RbfCache = rbfCache

		bchCfg := BatchConfig{
			MaxTimeoutDistance: bch.MaxTimeoutDistance,
		}
		batch.Cfg = &bchCfg

		batches = append(batches, &batch)
	}

	return batches, nil
}

// ConvertSweep converts a fetched sweep from the database to a sweep that is
// ready to be processed by the batcher.
func (br *Batcher) ConvertSweep(ctx context.Context,
	dbSweep *loopdb.Sweep) (*Sweep, error) {

	swap := dbSweep.LoopOut

	htlc, err := GetHtlc(
		dbSweep.SwapHash, &swap.Contract.SwapContract, br.chainParams,
	)
	if err != nil {
		return nil, err
	}

	swapPaymentAddr, err := ObtainSwapPaymentAddr(
		swap.Contract.SwapInvoice, br.chainParams,
	)
	if err != nil {
		return nil, err
	}

	sweep := Sweep{
		SwapHash:               swap.Hash,
		Outpoint:               dbSweep.Outpoint,
		Value:                  dbSweep.Amount,
		ConfTarget:             swap.Contract.SweepConfTarget,
		Timeout:                swap.Contract.CltvExpiry,
		InitiationHeight:       swap.Contract.InitiationHeight,
		Htlc:                   *htlc,
		Preimage:               swap.Contract.Preimage,
		SwapInvoicePaymentAddr: *swapPaymentAddr,
		Key:                    swap.Contract.HtlcKeys.ReceiverScriptKey,
		HtlcKeys:               swap.Contract.HtlcKeys,
		ProtocolVersion:        swap.Contract.ProtocolVersion,
	}

	return &sweep, nil
}

// FetchSweep fetches the sweep related information from the database.
func (br *Batcher) FetchSweep(ctx context.Context,
	sweepReq SweepRequest) (*Sweep, error) {

	swapHash, err := lntypes.MakeHash(sweepReq.SwapHash[:])
	if err != nil {
		return nil, err
	}

	swap, err := br.store.FetchLoopOutSwap(ctx, swapHash)
	if err != nil {
		return nil, err
	}

	htlc, err := GetHtlc(
		swapHash, &swap.Contract.SwapContract, br.chainParams,
	)
	if err != nil {
		return nil, err
	}

	swapPaymentAddr, err := ObtainSwapPaymentAddr(
		swap.Contract.SwapInvoice, br.chainParams,
	)
	if err != nil {
		return nil, err
	}

	sweep := Sweep{
		SwapHash:               swap.Hash,
		Outpoint:               sweepReq.Outpoint,
		Value:                  sweepReq.Value,
		ConfTarget:             swap.Contract.SweepConfTarget,
		Timeout:                swap.Contract.CltvExpiry,
		InitiationHeight:       swap.Contract.InitiationHeight,
		Htlc:                   *htlc,
		Preimage:               swap.Contract.Preimage,
		SwapInvoicePaymentAddr: *swapPaymentAddr,
		Key:                    swap.Contract.HtlcKeys.ReceiverScriptKey,
		HtlcKeys:               swap.Contract.HtlcKeys,
		ProtocolVersion:        swap.Contract.ProtocolVersion,
	}

	return &sweep, nil
}

// writeToBatchChan is a helper function that writes to a batch's channel but
// also listens to context errors. The reason we are not just blocking on the
// batch chan is to avoid deadlocking when the context exits.
func writeToBatchChan(ctx context.Context, batch *Batch, sweep *Sweep) error {
	select {
	case batch.RequestChan <- sweep:
		return nil

	case <-ctx.Done():
		return ctx.Err()
	}
}

// GetHtlc composes and returns the on-chain swap script.
func GetHtlc(hash lntypes.Hash, contract *loopdb.SwapContract,
	chainParams *chaincfg.Params) (*swap.Htlc, error) {

	switch GetHtlcScriptVersion(contract.ProtocolVersion) {
	case swap.HtlcV2:
		return swap.NewHtlcV2(
			contract.CltvExpiry, contract.HtlcKeys.SenderScriptKey,
			contract.HtlcKeys.ReceiverScriptKey, hash,
			chainParams,
		)

	case swap.HtlcV3:
		// Swaps that implement the new MuSig2 protocol will be expected
		// to use the 1.0RC2 MuSig2 key derivation scheme.
		muSig2Version := input.MuSig2Version040
		if contract.ProtocolVersion >= loopdb.ProtocolVersionMuSig2 {
			muSig2Version = input.MuSig2Version100RC2
		}

		return swap.NewHtlcV3(
			muSig2Version,
			contract.CltvExpiry,
			contract.HtlcKeys.SenderInternalPubKey,
			contract.HtlcKeys.ReceiverInternalPubKey,
			contract.HtlcKeys.SenderScriptKey,
			contract.HtlcKeys.ReceiverScriptKey,
			hash, chainParams,
		)
	}

	return nil, swap.ErrInvalidScriptVersion
}

// GetHtlcScriptVersion returns the correct HTLC script version for the passed
// protocol version.
func GetHtlcScriptVersion(
	protocolVersion loopdb.ProtocolVersion) swap.ScriptVersion {

	// If the swap was initiated before we had our v3 script, use v2.
	if protocolVersion < loopdb.ProtocolVersionHtlcV3 ||
		protocolVersion == loopdb.ProtocolVersionUnrecorded {

		return swap.HtlcV2
	}

	return swap.HtlcV3
}

// ObtainSwapPaymentAddr will retrieve the payment addr from the passed invoice.
func ObtainSwapPaymentAddr(swapInvoice string, chainParams *chaincfg.Params) (
	*[32]byte, error) {

	swapPayReq, err := zpay32.Decode(
		swapInvoice, chainParams,
	)
	if err != nil {
		return nil, err
	}

	if swapPayReq.PaymentAddr == nil {
		return nil, fmt.Errorf("expected payment address for invoice")
	}

	return swapPayReq.PaymentAddr, nil
}
