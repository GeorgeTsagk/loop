package sweepbatcher

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"fmt"
	"math"
	"math/big"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	defaultFeeRateStep     = chainfee.SatPerKWeight(500)
	defaultBatchConfTarget = 12
)

// Sweep stores any data related to sweeping a specific outpoint.
type Sweep struct {
	SwapHash               lntypes.Hash
	Outpoint               wire.OutPoint
	Value                  btcutil.Amount
	ConfTarget             int32
	Timeout                int32
	InitiationHeight       int32
	Htlc                   swap.Htlc
	Preimage               lntypes.Preimage
	SwapInvoicePaymentAddr [32]byte
	Key                    [33]byte
	HtlcKeys               loopdb.HtlcKeys
	ProtocolVersion        loopdb.ProtocolVersion
}

// batchState is the state of the batch.
type batchState uint8

const (
	// Open is the state in which the batch is able to accept new sweeps.
	Open batchState = iota

	// Closed is the state in which the batch is no longer able to accept
	// new sweeps.
	Closed

	// Confirmed is the state in which the batch transaction has reached the
	// configured conf height.
	Confirmed
)

// BatchConfig is the configuration for a batch.
type BatchConfig struct {
	MaxTimeoutDistance int32
	BatchConfTarget    int32
}

// RBFCache stores data related to our last fee bump.
type RBFCache struct {
	LastHeight int32
	FeeRate    chainfee.SatPerKWeight
}

// Batch is a collection of sweeps that are published together.
type Batch struct {
	// ID is the primary identifier of this batch.
	ID lntypes.Hash

	// State is the current State of the batch.
	State batchState

	// PrimarySweepID is the swap hash of the primary sweep in the batch.
	PrimarySweepID lntypes.Hash

	// Sweeps store the Sweeps that this batch currently contains.
	Sweeps map[lntypes.Hash]Sweep

	// currentHeight is the current block height.
	currentHeight int32

	// RequestChan is the channel over which new sweeps are received from.
	RequestChan chan *Sweep

	// returnChan is the channel over which sweeps are returned back to the
	// batcher in order to enter a new batch. This occurs when some sweeps
	// are left out because an older version of the batch tx got confirmed.
	// This is a behavior introduced by RBF replacements.
	returnChan chan SweepRequest

	// blockEpochChan is the channel over which block epoch notifications
	// are received.
	blockEpochChan chan int32

	// spendChan is the channel over which spend notifications are received.
	spendChan chan *chainntnfs.SpendDetail

	// spendRefreshChan is a communication channel for the routines that
	// monitor spends. This channel is simply used by the new routine to
	// signal to the previous routine that it should exit. This helps the
	// batch have at most one spend monitor active at a time.
	spendRefreshChan chan int32

	// confChan is the channel over which confirmation notifications are
	// received.
	confChan chan *chainntnfs.TxConfirmation

	// confRefreshChan is a communication channel for the routines that
	// monitor confirmations. This channel is simply used by the new
	// routine to signal to the previous routine that it should exit. This
	// helps the batch have at most one confirmation monitor active at a
	// time.
	confRefreshChan chan int32

	// errChan is the channel over which errors are received.
	errChan chan error

	// completeChan is the channel over which the batch sends its ID back
	// to the batcher to signal its exit.
	completeChan chan lntypes.Hash

	// batchTx is the transaction that is currently being monitored for
	// confirmations.
	BatchTxid *chainhash.Hash

	// batchPkScript is the pkScript of the batch transaction's output.
	BatchPkScript []byte

	// RbfCache stores data related to the RBF fee bumping mechanism.
	RbfCache RBFCache

	// wallet is the wallet client used to create and publish the batch
	// transaction.
	wallet lndclient.WalletKitClient

	// chainNotifier is the chain notifier client used to monitor the
	// blockchain for spends and confirmations.
	chainNotifier lndclient.ChainNotifierClient

	// signerClient is the signer client used to sign the batch transaction.
	signerClient lndclient.SignerClient

	// muSig2Kit includes all the required functionality to collect
	// and verify signatures by the swap server in order to cooperatively
	// sweep funds.
	muSig2SignSweep MuSig2SignSweep

	// store includes all the database interactions that are needed by the
	// batch.
	store BatcherStore

	// Cfg is the configuration for this batch.
	Cfg *BatchConfig
}

// BatchKit is a kit of dependencies that are used to initialize a batch. This
// struct is only used as a wrapper for the arguments that are required to
// create a new batch.
type BatchKit struct {
	ID              lntypes.Hash
	batchTxid       *chainhash.Hash
	batchPkScript   []byte
	state           batchState
	primaryID       lntypes.Hash
	sweeps          map[lntypes.Hash]Sweep
	rbfCache        RBFCache
	returnChan      chan SweepRequest
	completeChan    chan lntypes.Hash
	wallet          lndclient.WalletKitClient
	chainNotifier   lndclient.ChainNotifierClient
	signerClient    lndclient.SignerClient
	musig2SignSweep MuSig2SignSweep
	store           BatcherStore
}

// NewBatch creates a new batch.
func NewBatch(cfg BatchConfig, batchKit BatchKit) (*Batch, error) {

	newID := lntypes.Hash{}
	_, err := crand.Read(newID[:])
	if err != nil {
		return nil, err
	}

	return &Batch{
		ID:               newID,
		State:            Open,
		Sweeps:           make(map[lntypes.Hash]Sweep),
		RequestChan:      make(chan *Sweep),
		returnChan:       batchKit.returnChan,
		blockEpochChan:   make(chan int32),
		spendChan:        make(chan *chainntnfs.SpendDetail),
		spendRefreshChan: make(chan int32, 1),
		confChan:         make(chan *chainntnfs.TxConfirmation),
		confRefreshChan:  make(chan int32, 1),
		errChan:          make(chan error),
		completeChan:     batchKit.completeChan,
		BatchTxid:        batchKit.batchTxid,
		wallet:           batchKit.wallet,
		chainNotifier:    batchKit.chainNotifier,
		signerClient:     batchKit.signerClient,
		muSig2SignSweep:  batchKit.musig2SignSweep,
		store:            batchKit.store,
		Cfg:              &cfg,
	}, nil
}

// NewBatch creates a new batch that already existed in storage.
func NewBatchFromDB(cfg BatchConfig, batchKit BatchKit) (*Batch, error) {

	newID := lntypes.Hash{}
	_, err := crand.Read(newID[:])
	if err != nil {
		return nil, err
	}

	return &Batch{
		ID:               batchKit.ID,
		State:            batchKit.state,
		PrimarySweepID:   batchKit.primaryID,
		Sweeps:           batchKit.sweeps,
		RequestChan:      make(chan *Sweep),
		returnChan:       batchKit.returnChan,
		blockEpochChan:   make(chan int32),
		spendChan:        make(chan *chainntnfs.SpendDetail),
		spendRefreshChan: make(chan int32, 1),
		confChan:         make(chan *chainntnfs.TxConfirmation),
		confRefreshChan:  make(chan int32, 1),
		errChan:          make(chan error),
		completeChan:     batchKit.completeChan,
		BatchTxid:        batchKit.batchTxid,
		BatchPkScript:    batchKit.batchPkScript,
		RbfCache:         batchKit.rbfCache,
		wallet:           batchKit.wallet,
		chainNotifier:    batchKit.chainNotifier,
		signerClient:     batchKit.signerClient,
		muSig2SignSweep:  batchKit.musig2SignSweep,
		store:            batchKit.store,
		Cfg:              &cfg,
	}, nil
}

// addSweep adds a sweep to the batch. If this is the first sweep being added
// to the batch then it also sets the primary sweep ID.
func (b *Batch) addSweep(ctx context.Context, sweep Sweep) error {

	if b.PrimarySweepID == lntypes.ZeroHash {
		b.PrimarySweepID = sweep.SwapHash
		b.Cfg.BatchConfTarget = sweep.ConfTarget
	}

	if b.PrimarySweepID == sweep.SwapHash {
		b.Cfg.BatchConfTarget = sweep.ConfTarget
	}

	if !b.SweepExists(sweep.SwapHash) {
		log.Infof("Batch %x: adding sweep %x", b.ID[:6],
			sweep.SwapHash[:6])
	}

	b.Sweeps[sweep.SwapHash] = sweep

	err := b.persistSweep(ctx, sweep)
	if err != nil {
		log.Errorf("Batch %x error while persisting: %v", b.ID[:6],
			err)
	}

	return err
}

// SweepExists returns true if the batch contains the sweep with the given hash.
func (b *Batch) SweepExists(hash lntypes.Hash) bool {
	_, ok := b.Sweeps[hash]
	return ok
}

// AcceptsSweep returns true if the batch is able to accept the given sweep. To
// accept a sweep a batch needs to be in an Open state, and the incoming sweep's
// timeout must not be too far away from the other timeouts of existing sweeps.
func (b *Batch) AcceptsSweep(sweep *Sweep) bool {
	tooFar := false
	for _, s := range b.Sweeps {
		if int32(math.Abs(float64(sweep.Timeout-s.Timeout))) >
			b.Cfg.MaxTimeoutDistance {

			tooFar = true
		}
	}

	return (b.State == Open) && !tooFar
}

// Run is the batch's main event loop.
func (b *Batch) Run(ctx context.Context) error {
	// Dispatch the blockheight monitor.
	go b.monitorBlockHeight(ctx)

	// Dispatch the spend monitor.
	go b.monitorSpend(ctx)

	// Since this is the first time the batch is running we save the batch
	// on storage.
	err := b.persist(ctx)
	if err != nil {
		return err
	}

	log.Infof("Batch %x: started, primary %x, total sweeps %v", b.ID[0:6],
		b.PrimarySweepID[0:6], len(b.Sweeps))

	for {
		select {
		case sweep := <-b.RequestChan:
			err := b.addSweep(ctx, *sweep)
			if err != nil {
				return err
			}

		case height := <-b.blockEpochChan:
			log.Debugf("Batch %x: received block %v", b.ID[:6],
				height)

			firstHeight := (b.currentHeight == 0)
			b.currentHeight = height

			// We skip the step if this was the first height we
			// received, as this means we just started running. This
			// prevents immediately publishing a transaction on
			// batch creation.
			if !firstHeight {
				err := b.step(ctx)
				if err != nil {
					return err
				}
			} else {
				log.Debugf("Batch %x: skipping step on first "+
					"height", b.ID[:6])
			}

		case spend := <-b.spendChan:
			err := b.handleSpend(ctx, spend.SpendingTx)
			if err != nil {
				return err
			}

		case <-b.confChan:
			return b.handleConf(ctx)

		case err := <-b.errChan:
			return err

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// step executes the next step in the batch's lifecycle.
func (b *Batch) step(ctx context.Context) error {
	switch b.State {
	// For an open state we want to publish the latest version of the batch
	// transaction. This should contain the latest set of sweeps and the
	// most up-to-date fee rate at current blockheight.
	case Open:
		err := b.publish(ctx)
		if err != nil {
			return err
		}

	// For a closed state we want to dispatch a fresh confirmation monitor.
	// The reason this is done in the step function, therefore on each new
	// block, is to ensure that the batch is always monitoring the latest
	// version of the batch transaction. This could prove useful on restarts
	// or on re-org events.
	case Closed:
		go b.monitorConfirmations(ctx)
	}

	return nil
}

// publish creates and publishes the latest batch transaction to the network.
func (b *Batch) publish(ctx context.Context) error {
	go b.monitorSpend(ctx)

	var (
		err         error
		fee         btcutil.Amount
		coopSuccess bool
	)

	// Run the RBF rate update.
	err = b.updateRbfRate(ctx)
	if err != nil {
		return err
	}

	fee, err, coopSuccess = b.publishBatchCoop(ctx)
	if err != nil {
		log.Warnf("Batch %x (co-op): publish error: %v",
			b.ID[:6], err)
	}

	if !coopSuccess {
		fee, err = b.publishBatch(ctx)
	}

	if err != nil {
		log.Warnf("Batch %x: publish error: %v", b.ID[:6], err)
		return nil
	}

	log.Infof(
		"Batch %x: published, total sweeps: %v, fees: %v",
		b.ID[0:6], len(b.Sweeps), fee,
	)

	return b.persist(ctx)
}

// publishBatch creates and publishes the batch transaction. It will consult the
// RBFCache to determine the fee rate to use.
func (b *Batch) publishBatch(ctx context.Context) (btcutil.Amount, error) {
	// Create the batch transaction.
	batchTx := wire.NewMsgTx(2)
	batchTx.LockTime = uint32(b.currentHeight)

	batchAmt := btcutil.Amount(0)
	prevOuts := make([]*wire.TxOut, 0, len(b.Sweeps))
	signDescs := make([]*lndclient.SignDescriptor, 0, len(b.Sweeps))
	sweeps := make([]Sweep, 0, len(b.Sweeps))
	fee := btcutil.Amount(0)
	inputCounter := 0

	var weightEstimate input.TxWeightEstimator

	// Add all the sweeps to the batch transaction.
	for _, sweep := range b.Sweeps {
		batchAmt += sweep.Value
		batchTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: sweep.Outpoint,
			Sequence:         sweep.Htlc.SuccessSequence(),
		})

		updateWeightEstimate(&weightEstimate, sweep.Htlc.PkScript, true)

		// Append this sweep to an array of sweeps. This is needed to
		// keep the order of sweeps stored, as iterating the sweeps map
		// does not guarantee same order.
		sweeps = append(sweeps, sweep)

		// Create and store the previous outpoint for this sweep.
		prevOuts = append(prevOuts, &wire.TxOut{
			Value:    int64(sweep.Value),
			PkScript: sweep.Htlc.PkScript,
		})

		key, err := btcec.ParsePubKey(sweep.Key[:])
		if err != nil {
			return fee, err
		}

		// Create and store the sign descriptor for this sweep.
		signDesc := lndclient.SignDescriptor{
			WitnessScript: sweep.Htlc.SuccessScript(),
			Output:        prevOuts[len(prevOuts)-1],
			HashType:      sweep.Htlc.SigHash(),
			InputIndex:    inputCounter,
			KeyDesc: keychain.KeyDescriptor{
				PubKey: key,
			},
		}

		inputCounter++

		if sweep.Htlc.Version == swap.HtlcV3 {
			signDesc.SignMethod = input.TaprootScriptSpendSignMethod
		}

		signDescs = append(signDescs, &signDesc)
	}

	// Generate a wallet address for the batch transaction's output.
	address, err := b.wallet.NextAddr(
		ctx, "", walletrpc.AddressType_TAPROOT_PUBKEY, false,
	)
	if err != nil {
		return fee, err
	}

	batchPkScript, err := txscript.PayToAddrScript(address)
	if err != nil {
		return fee, err
	}

	weightEstimate.AddP2TROutput()

	totalWeight := int64(weightEstimate.Weight())

	// Run the RBF rate update.
	err = b.updateRbfRate(ctx)
	if err != nil {
		return fee, err
	}

	fee = b.RbfCache.FeeRate.FeeForWeight(totalWeight)

	// Add the batch transaction output, which excludes the fees paid to
	// miners.
	batchTx.AddTxOut(&wire.TxOut{
		PkScript: batchPkScript,
		Value:    int64(batchAmt - fee),
	})

	// Collect the signatures for our inputs.
	rawSigs, err := b.signerClient.SignOutputRaw(
		ctx, batchTx, signDescs, prevOuts,
	)
	if err != nil {
		return fee, err
	}

	for i, sweep := range sweeps {

		// Generate the success witness for the sweep.
		witness, err := sweep.Htlc.GenSuccessWitness(
			rawSigs[i], sweep.Preimage,
		)
		if err != nil {
			return fee, err
		}

		// Add the success witness to our batch transaction's inputs.
		batchTx.TxIn[i].Witness = witness
	}

	log.Debugf("Batch %x: attempting to publish tx with feerate=%v, "+
		"totalfee=%v", b.ID[:6], b.RbfCache.FeeRate, fee)
	err = b.wallet.PublishTransaction(
		ctx, batchTx, labels.LoopOutBatchSweepSuccess(b.ID.String()),
	)
	if err != nil {
		return fee, err
	}

	// Store the batch transaction's txid and pkScript, for monitoring
	// purposes.
	txHash := batchTx.TxHash()
	b.BatchTxid = &txHash
	b.BatchPkScript = batchPkScript

	return fee, nil
}

// publishBatchCoop attempts to construct and publish a batch transaction that
// collects all the required signatures interactively from the server. This
// helps with collecting the funds immediately without revealing any information
// related to the HTLC script.
func (b *Batch) publishBatchCoop(ctx context.Context) (btcutil.Amount,
	error, bool) {
	batchAmt := btcutil.Amount(0)
	sweeps := make([]Sweep, 0, len(b.Sweeps))
	fee := btcutil.Amount(0)
	var weightEstimate input.TxWeightEstimator

	// Create the batch transaction.
	batchTx := &wire.MsgTx{
		Version:  2,
		LockTime: uint32(b.currentHeight),
	}

	for _, sweep := range b.Sweeps {
		// Append this sweep to an array of sweeps. This is needed to
		// keep the order of sweeps stored, as iterating the sweeps map
		// does not guarantee same order.
		sweeps = append(sweeps, sweep)
	}

	// Add all the sweeps to the batch transaction.
	for _, sweep := range sweeps {
		// Keep track of the total amount this batch is sweeping back.
		batchAmt += sweep.Value

		// Add this sweep's input to the transaction.
		batchTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: sweep.Outpoint,
		})

		updateWeightEstimate(&weightEstimate, sweep.Htlc.PkScript, true)
	}

	// Generate a wallet address for the batch transaction's output.
	address, err := b.wallet.NextAddr(
		ctx, "", walletrpc.AddressType_TAPROOT_PUBKEY, false,
	)
	if err != nil {
		return fee, err, false
	}

	batchPkScript, err := txscript.PayToAddrScript(address)
	if err != nil {
		return fee, err, false
	}

	weightEstimate.AddP2TROutput()

	totalWeight := int64(weightEstimate.Weight())

	fee = b.RbfCache.FeeRate.FeeForWeight(totalWeight)

	// Add the batch transaction output, which excludes the fees paid to
	// miners.
	batchTx.AddTxOut(&wire.TxOut{
		PkScript: batchPkScript,
		Value:    int64(batchAmt - fee),
	})

	packet, err := psbt.NewFromUnsignedTx(batchTx)
	if err != nil {
		return fee, err, false
	}

	prevOuts := make(map[wire.OutPoint]*wire.TxOut)

	for i, sweep := range sweeps {
		txOut := &wire.TxOut{
			Value:    int64(sweep.Value),
			PkScript: sweep.Htlc.PkScript,
		}

		prevOuts[sweep.Outpoint] = txOut
		packet.Inputs[i].WitnessUtxo = txOut
	}

	var psbtBuf bytes.Buffer
	err = packet.Serialize(&psbtBuf)
	if err != nil {
		return fee, err, false
	}

	prevOutputFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)

	for i, sweep := range sweeps {

		sigHashes := txscript.NewTxSigHashes(
			packet.UnsignedTx, prevOutputFetcher,
		)

		sigHash, err := txscript.CalcTaprootSignatureHash(
			sigHashes, txscript.SigHashDefault, packet.UnsignedTx,
			i, prevOutputFetcher,
		)
		if err != nil {
			return fee, err, false
		}

		var (
			signers       [][]byte
			muSig2Version input.MuSig2Version
		)

		// Depending on the MuSig2 version we either pass 32 byte
		// Schnorr public keys or normal 33 byte public keys.
		if sweep.ProtocolVersion >= loopdb.ProtocolVersionMuSig2 {
			muSig2Version = input.MuSig2Version100RC2
			signers = [][]byte{
				sweep.HtlcKeys.SenderInternalPubKey[:],
				sweep.HtlcKeys.ReceiverInternalPubKey[:],
			}
		} else {
			muSig2Version = input.MuSig2Version040
			signers = [][]byte{
				sweep.HtlcKeys.SenderInternalPubKey[1:],
				sweep.HtlcKeys.ReceiverInternalPubKey[1:],
			}
		}

		htlcScript, ok := sweep.Htlc.HtlcScript.(*swap.HtlcScriptV3)
		if !ok {
			return fee, fmt.Errorf("invalid htlc script version"),
				false
		}

		// Now we're creating a local MuSig2 session using the receiver
		// key's key locator and the htlc's root hash.
		musig2SessionInfo, err := b.signerClient.MuSig2CreateSession(
			ctx, muSig2Version,
			&sweep.HtlcKeys.ClientScriptKeyLocator, signers,
			lndclient.MuSig2TaprootTweakOpt(
				htlcScript.RootHash[:], false,
			),
		)
		if err != nil {
			return fee, err, false
		}

		// With the session active, we can now send the server our public nonce
		// and the sig hash, so that it can create it's own MuSig2 session and
		// return the server side nonce and partial signature.
		serverNonce, serverSig, err := b.muSig2SignSweep(
			ctx, sweep.ProtocolVersion, sweep.SwapHash,
			sweep.SwapInvoicePaymentAddr,
			musig2SessionInfo.PublicNonce[:], psbtBuf.Bytes(),
			prevOuts,
		)
		if err != nil {
			return fee, err, false
		}

		var serverPublicNonce [musig2.PubNonceSize]byte
		copy(serverPublicNonce[:], serverNonce)

		// Register the server's nonce before attempting to create our partial
		// signature.
		haveAllNonces, err := b.signerClient.MuSig2RegisterNonces(
			ctx, musig2SessionInfo.SessionID,
			[][musig2.PubNonceSize]byte{serverPublicNonce},
		)
		if err != nil {
			return fee, err, false
		}

		// Sanity check that we have all the nonces.
		if !haveAllNonces {
			return fee, fmt.Errorf("invalid MuSig2 session: " +
				"nonces missing"), false
		}

		var digest [32]byte
		copy(digest[:], sigHash)

		// Since our MuSig2 session has all nonces, we can now create the local
		// partial signature by signing the sig hash.
		_, err = b.signerClient.MuSig2Sign(
			ctx, musig2SessionInfo.SessionID, digest, false,
		)
		if err != nil {
			return fee, err, false
		}

		// Now combine the partial signatures to use the final combined
		// signature in the sweep transaction's witness.
		haveAllSigs, finalSig, err := b.signerClient.MuSig2CombineSig(
			ctx, musig2SessionInfo.SessionID, [][]byte{serverSig},
		)
		if err != nil {
			return fee, err, false
		}

		if !haveAllSigs {
			return fee, fmt.Errorf("failed to combine signatures"),
				false
		}

		// To be sure that we're good, parse and validate that the combined
		// signature is indeed valid for the sig hash and the internal pubkey.
		err = verifySchnorrSig(
			htlcScript.TaprootKey, sigHash, finalSig,
		)
		if err != nil {
			return fee, err, false
		}

		packet.UnsignedTx.TxIn[i].Witness = wire.TxWitness{
			finalSig,
		}
	}

	log.Debugf("Batch %x: attempting to publish coop tx with feerate=%v, "+
		"totalfee=%v, sweeps=%v", b.ID[:6], b.RbfCache.FeeRate, fee,
		len(batchTx.TxIn))
	err = b.wallet.PublishTransaction(
		ctx, batchTx, labels.LoopOutBatchSweepSuccess(b.ID.String()),
	)
	if err != nil {
		return fee, err, true
	}

	// Store the batch transaction's txid and pkScript, for monitoring
	// purposes.
	txHash := batchTx.TxHash()
	b.BatchTxid = &txHash
	b.BatchPkScript = batchPkScript

	return fee, nil, true
}

// updateRbfRate updates the fee rate we should use for the new batch
// transaction. This fee rate does not guarantee RBF success, but the continuous
// increase leads to an eventual successful RBF replacement.
func (b *Batch) updateRbfRate(ctx context.Context) error {
	// If the feeRate is unset then we never published before, so we
	// retrieve the fee estimate from our wallet.
	if b.RbfCache.FeeRate == 0 {
		log.Infof("initializing rbf fee rate for conf target=%v", b.Cfg.BatchConfTarget)
		rate, err := b.wallet.EstimateFeeRate(
			ctx, b.Cfg.BatchConfTarget,
		)
		if err != nil {
			return err
		}

		// Set the initial value for our fee rate.
		b.RbfCache.FeeRate = rate
	} else {
		// Bump the fee rate by the configured step.
		b.RbfCache.FeeRate += defaultFeeRateStep
		b.RbfCache.LastHeight = b.currentHeight
		return b.persist(ctx)
	}

	return nil
}

// monitorBlockHeight monitors the current block height.
func (b *Batch) monitorBlockHeight(ctx context.Context) {
	blockChan, errChan, err := b.chainNotifier.RegisterBlockEpochNtfn(
		ctx,
	)
	if err != nil {
		b.errChan <- err
		return
	}

	for {
		select {
		case height := <-blockChan:
			b.blockEpochChan <- height

		case err := <-errChan:
			b.errChan <- err

		case <-ctx.Done():
			return
		}
	}
}

// monitorSpend monitors the primary sweep's outpoint for spends. The reason we
// monitor the primary sweep's outpoint is because the primary sweep was the
// first sweep that entered this batch, therefore it is present in all the
// versions of the batch transaction. This means that even if an older version
// of the batch transaction gets confirmed, due to the uncertainty of RBF
// replacements and network propagation, we can always detect the transaction.
func (b *Batch) monitorSpend(ctx context.Context) {
	// If no primary sweep exists the batch is empty and there is no spend
	// to monitor.
	if b.PrimarySweepID == lntypes.ZeroHash {
		log.Warnf("can't monitor batch spend, no primary sweep")
		return
	}

	// Generate this routine's nonce. This is used to signal to the previous
	// routine that a new routine is now monitoring the spend.
	randValue, err := crand.Int(crand.Reader, big.NewInt(math.MaxInt32))
	if err != nil {
		b.errChan <- err
		return
	}

	nonce := int32(randValue.Int64())
	// Send the nonce to the refresh channel.
	b.spendRefreshChan <- nonce

	// Get the primary sweep.
	sweep := b.Sweeps[b.PrimarySweepID]

	rCtx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
	}()

	spendChan, spendErr, err := b.chainNotifier.RegisterSpendNtfn(
		rCtx, &sweep.Outpoint, sweep.Htlc.PkScript,
		sweep.InitiationHeight,
	)
	if err != nil {
		b.errChan <- err
		return
	}

	log.Infof("Batch %x: monitoring spend for outpoint %s", b.ID[:6],
		sweep.Outpoint.String())

	for {
		select {
		case spend := <-spendChan:
			b.spendChan <- spend
			return

		case err := <-spendErr:
			b.errChan <- err
			return

		case value := <-b.spendRefreshChan:
			// If the nonce we received from the channel is equal
			// to this routine's nonce, this means that we are the
			// first routine to arrive here. So we only exit if the
			// nonce we read is different.
			if value != nonce {
				return
			}

		case <-ctx.Done():
			return
		}
	}
}

// monitorConfirmations monitors the batch transaction for confirmations.
func (b *Batch) monitorConfirmations(ctx context.Context) {
	// Generate this routine's nonce. This is used to signal to the previous
	// routine that a new routine is now monitoring the confirmations.
	randValue, err := crand.Int(crand.Reader, big.NewInt(math.MaxInt32))
	if err != nil {
		b.errChan <- err
		return
	}

	nonce := int32(randValue.Int64())
	// Send the nonce to the refresh channel.
	b.confRefreshChan <- nonce

	rCtx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
	}()

	reorgChan := make(chan struct{})

	confChan, errChan, err := b.chainNotifier.RegisterConfirmationsNtfn(
		rCtx, b.BatchTxid, b.BatchPkScript, 3, b.currentHeight,
		lndclient.WithReOrgChan(reorgChan),
	)
	if err != nil {
		log.Errorf("unable to register for confirmation ntfn: %v", err)
		return
	}

	for {
		select {
		case conf := <-confChan:
			b.confChan <- conf
			return

		case err := <-errChan:
			b.errChan <- err
			return

		case <-reorgChan:
			// A re-org has been detected. We set the batch's state
			// back to open since our batch transaction is no longer
			// present in any block. We can accept more sweeps and
			// try to publish new transactions, at this point we
			// need to monitor again for a new spend.
			b.State = Open
			log.Warnf("Batch %x: reorg detected", b.ID[:6])
			return

		case value := <-b.confRefreshChan:
			// If the nonce we received from the channel is equal
			// to this routine's nonce, this means that we are the
			// first routine to arrive here. So we only exit if the
			// nonce we read is different.
			if value != nonce {
				return
			}

		case <-ctx.Done():
			return
		}
	}
}

// handleSpend handles a spend notification.
func (b *Batch) handleSpend(ctx context.Context, spendTx *wire.MsgTx) error {
	// As soon as a spend arrives we should no longer accept sweeps. Also,
	// the closed state will cause the batch to start monitoring the
	// confirmation of the batch transaction.
	b.State = Closed

	txHash := spendTx.TxHash()
	b.BatchTxid = &txHash
	purgedCounter := 0

	// As a previous version of the batch transaction may get confirmed,
	// which does not contain the latest sweeps, we need to detect the
	// sweeps that did not make it to the confirmed transaction and feed
	// them back to the batcher. This will ensure that the sweeps will enter
	// a new batch instead of remaining dangling.
	for _, sweep := range b.Sweeps {
		found := false

		for _, txIn := range spendTx.TxIn {
			if txIn.PreviousOutPoint == sweep.Outpoint {
				found = true
			}
		}

		// If the sweep's outpoint was not found in the transaction's
		// inputs this means it was left out. So we delete it from this
		// batch and feed it back to the batcher.
		if !found {
			newSweep := sweep
			delete(b.Sweeps, sweep.SwapHash)
			b.returnChan <- SweepRequest{
				SwapHash: newSweep.SwapHash,
				Outpoint: newSweep.Outpoint,
				Value:    newSweep.Value,
			}
			purgedCounter++
		}
	}

	log.Infof("Batch %x: spent, total sweeps: %v, purged sweeps: %v",
		b.ID[:6], len(b.Sweeps), purgedCounter)

	return b.persist(ctx)
}

// handleConf handles a confirmation notification. This is the final step of the
// batch. Here we signal to the batcher that this batch was completed.
func (b *Batch) handleConf(ctx context.Context) error {
	log.Infof("Batch %x: confirmed", b.ID[:6])

	b.State = Confirmed

	b.completeChan <- b.ID

	return b.markConfirmed(ctx)
}

// markConfirmed marks the batch as confirmed in the database.
func (b *Batch) markConfirmed(ctx context.Context) error {
	return b.store.ConfirmBatch(ctx, b.ID[:])
}

// persist stores the batch in the database.
func (b *Batch) persist(ctx context.Context) error {
	bch := &loopdb.Batch{}

	bch.ID = b.ID
	switch b.State {
	case Open:
		bch.BatchState = "open"

	case Closed:
		bch.BatchState = "closed"

	case Confirmed:
		bch.BatchState = "confirmed"
	}

	if b.BatchTxid != nil {
		bch.BatchTxid = *b.BatchTxid
	}

	bch.BatchPkScript = b.BatchPkScript
	bch.LastRbfHeight = b.RbfCache.LastHeight
	bch.LastRbfSatPerKw = int32(b.RbfCache.FeeRate)
	bch.MaxTimeoutDistance = b.Cfg.MaxTimeoutDistance

	return b.store.UpsertSweepBatch(ctx, bch)

}

func (b *Batch) persistSweep(ctx context.Context, sweep Sweep) error {
	return b.store.UpsertSweep(ctx, &loopdb.Sweep{
		BatchID:  b.ID,
		SwapHash: sweep.SwapHash,
		Outpoint: sweep.Outpoint,
		Amount:   sweep.Value,
	})
}

// updateWeightEstimate updates the weight estimate based on the passed pkScript
// and whether it is an input or output.
func updateWeightEstimate(weightEstimate *input.TxWeightEstimator,
	pkScript []byte, input bool) error {

	if input {
		switch {
		case txscript.IsPayToWitnessPubKeyHash(pkScript):
			weightEstimate.AddP2WKHInput()

		case txscript.IsPayToScriptHash(pkScript):
			weightEstimate.AddNestedP2WKHInput()

		case txscript.IsPayToTaproot(pkScript):
			weightEstimate.AddTaprootKeySpendInput(
				txscript.SigHashDefault,
			)

		default:
			return fmt.Errorf("unsupported input script: %x",
				pkScript)
		}
	} else {
		// Currently only P2WSH and P2TR outputs are supported.
		switch {
		case txscript.IsPayToWitnessScriptHash(pkScript):
			weightEstimate.AddP2WSHOutput()

		case txscript.IsPayToTaproot(pkScript):
			weightEstimate.AddP2TROutput()

		default:
			return fmt.Errorf("unsupported output script: %x",
				pkScript)
		}
	}

	return nil
}

// verifySchnorrSig verifies that the passed signature is valid for the passed
// public key and message hash.
func verifySchnorrSig(pubKey *btcec.PublicKey, hash, sig []byte) error {
	schnorrSig, err := schnorr.ParseSignature(sig)
	if err != nil {
		return err
	}

	if !schnorrSig.Verify(hash, pubKey) {
		return fmt.Errorf("invalid signature")
	}

	return nil
}
