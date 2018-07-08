package bitcoindnotify

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/roasbeef/btcd/btcjson"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/rpcclient"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcwallet/chain"
	"github.com/roasbeef/btcwallet/wtxmgr"
)

const (

	// notifierType uniquely identifies this concrete implementation of the
	// ChainNotifier interface.
	notifierType = "bitcoind"

	// reorgSafetyLimit is assumed maximum depth of a chain reorganization.
	// After this many confirmation, transaction confirmation info will be
	// pruned.
	reorgSafetyLimit = 100
)

var (
	// ErrChainNotifierShuttingDown is used when we are trying to
	// measure a spend notification when notifier is already stopped.
	ErrChainNotifierShuttingDown = errors.New("chainntnfs: system interrupt " +
		"while attempting to register for spend notification.")
)

// chainUpdate encapsulates an update to the current main chain. This struct is
// used as an element within an unbounded queue in order to avoid blocking the
// main rpc dispatch rule.
type chainUpdate struct {
	blockHash   *chainhash.Hash
	blockHeight int32
}

// TODO(roasbeef): generalize struct below:
//  * move chans to config
//  * extract common code
//  * allow outside callers to handle send conditions

// BitcoindNotifier implements the ChainNotifier interface using a bitcoind
// chain client. Multiple concurrent clients are supported. All notifications
// are achieved via non-blocking sends on client channels.
type BitcoindNotifier struct {
	spendClientCounter uint64 // To be used atomically.
	epochClientCounter uint64 // To be used atomically.

	started int32 // To be used atomically.
	stopped int32 // To be used atomically.

	chainConn *chain.BitcoindClient

	notificationCancels  chan interface{}
	notificationRegistry chan interface{}

	spendNotifications map[wire.OutPoint]map[uint64]*spendNotification

	txConfNotifier *chainntnfs.TxConfNotifier

	blockEpochClients map[uint64]*blockEpochRegistration

	bestBlock chainntnfs.BlockEpoch

	wg   sync.WaitGroup
	quit chan struct{}
}

// Ensure BitcoindNotifier implements the ChainNotifier interface at compile
// time.
var _ chainntnfs.ChainNotifier = (*BitcoindNotifier)(nil)

// New returns a new BitcoindNotifier instance. This function assumes the
// bitcoind node  detailed in the passed configuration is already running, and
// willing to accept RPC requests and new zmq clients.
func New(config *rpcclient.ConnConfig, zmqConnect string,
	params chaincfg.Params) (*BitcoindNotifier, error) {
	notifier := &BitcoindNotifier{
		notificationCancels:  make(chan interface{}),
		notificationRegistry: make(chan interface{}),

		blockEpochClients: make(map[uint64]*blockEpochRegistration),

		spendNotifications: make(map[wire.OutPoint]map[uint64]*spendNotification),

		quit: make(chan struct{}),
	}

	// Disable connecting to bitcoind within the rpcclient.New method. We
	// defer establishing the connection to our .Start() method.
	config.DisableConnectOnNew = true
	config.DisableAutoReconnect = false
	chainConn, err := chain.NewBitcoindClient(&params, config.Host,
		config.User, config.Pass, zmqConnect, 100*time.Millisecond)
	if err != nil {
		return nil, err
	}
	notifier.chainConn = chainConn

	return notifier, nil
}

// Start connects to the running bitcoind node over websockets, registers for
// block notifications, and finally launches all related helper goroutines.
func (b *BitcoindNotifier) Start() error {
	// Already started?
	if atomic.AddInt32(&b.started, 1) != 1 {
		return nil
	}

	// Connect to bitcoind, and register for notifications on connected,
	// and disconnected blocks.
	if err := b.chainConn.Start(); err != nil {
		return err
	}
	if err := b.chainConn.NotifyBlocks(); err != nil {
		return err
	}

	currentHash, currentHeight, err := b.chainConn.GetBestBlock()
	if err != nil {
		return err
	}

	b.txConfNotifier = chainntnfs.NewTxConfNotifier(
		uint32(currentHeight), reorgSafetyLimit)

	b.bestBlock = chainntnfs.BlockEpoch{
		Height: currentHeight,
		Hash:   currentHash,
	}

	b.wg.Add(1)
	go b.notificationDispatcher()

	return nil
}

// Stop shutsdown the BitcoindNotifier.
func (b *BitcoindNotifier) Stop() error {
	// Already shutting down?
	if atomic.AddInt32(&b.stopped, 1) != 1 {
		return nil
	}

	// Shutdown the rpc client, this gracefully disconnects from bitcoind,
	// and cleans up all related resources.
	b.chainConn.Stop()

	close(b.quit)
	b.wg.Wait()

	// Notify all pending clients of our shutdown by closing the related
	// notification channels.
	for _, spendClients := range b.spendNotifications {
		for _, spendClient := range spendClients {
			close(spendClient.spendChan)
		}
	}
	for _, epochClient := range b.blockEpochClients {
		close(epochClient.cancelChan)
		epochClient.wg.Wait()

		close(epochClient.epochChan)
	}
	b.txConfNotifier.TearDown()

	return nil
}

// blockNtfn packages a notification of a connected/disconnected block along
// with its height at the time.
type blockNtfn struct {
	sha    *chainhash.Hash
	height int32
}

// notificationDispatcher is the primary goroutine which handles client
// notification registrations, as well as notification dispatches.
func (b *BitcoindNotifier) notificationDispatcher() {
out:
	for {
		select {
		case cancelMsg := <-b.notificationCancels:
			switch msg := cancelMsg.(type) {
			case *spendCancel:
				chainntnfs.Log.Infof("Cancelling spend "+
					"notification for out_point=%v, "+
					"spend_id=%v", msg.op, msg.spendID)

				// Before we attempt to close the spendChan,
				// ensure that the notification hasn't already
				// yet been dispatched.
				if outPointClients, ok := b.spendNotifications[msg.op]; ok {
					close(outPointClients[msg.spendID].spendChan)
					delete(b.spendNotifications[msg.op], msg.spendID)
				}

			case *epochCancel:
				chainntnfs.Log.Infof("Cancelling epoch "+
					"notification, epoch_id=%v", msg.epochID)

				// First, we'll lookup the original
				// registration in order to stop the active
				// queue goroutine.
				reg := b.blockEpochClients[msg.epochID]
				reg.epochQueue.Stop()

				// Next, close the cancel channel for this
				// specific client, and wait for the client to
				// exit.
				close(b.blockEpochClients[msg.epochID].cancelChan)
				b.blockEpochClients[msg.epochID].wg.Wait()

				// Once the client has exited, we can then
				// safely close the channel used to send epoch
				// notifications, in order to notify any
				// listeners that the intent has been
				// cancelled.
				close(b.blockEpochClients[msg.epochID].epochChan)
				delete(b.blockEpochClients, msg.epochID)

			}
		case registerMsg := <-b.notificationRegistry:
			switch msg := registerMsg.(type) {
			case *spendNotification:
				chainntnfs.Log.Infof("New spend subscription: "+
					"utxo=%v", msg.targetOutpoint)
				op := *msg.targetOutpoint

				if _, ok := b.spendNotifications[op]; !ok {
					b.spendNotifications[op] = make(map[uint64]*spendNotification)
				}
				b.spendNotifications[op][msg.spendID] = msg
				b.chainConn.NotifySpent([]*wire.OutPoint{&op})
			case *confirmationNotification:
				chainntnfs.Log.Infof("New confirmation "+
					"subscription: txid=%v, numconfs=%v",
					msg.TxID, msg.NumConfirmations)

				_, currentHeight, err := b.chainConn.GetBestBlock()
				if err != nil {
					chainntnfs.Log.Error(err)
				}

				// Lookup whether the transaction is already included in the
				// active chain.
				txConf, err := b.historicalConfDetails(
					msg.TxID, msg.heightHint, uint32(currentHeight),
				)
				if err != nil {
					chainntnfs.Log.Error(err)
				}

				err = b.txConfNotifier.Register(&msg.ConfNtfn, txConf)
				if err != nil {
					chainntnfs.Log.Error(err)
				}
			case *blockEpochRegistration:
				chainntnfs.Log.Infof("New block epoch subscription")
				b.blockEpochClients[msg.epochID] = msg
				err := b.catchUpClientOnBlocks(msg.bestBlock)
				if err != nil {
					chainntnfs.Log.Error(err)
				}
			case chain.RelevantTx:
				b.handleRelevantTx(msg, b.bestBlock.Height)
			}

		case ntfn := <-b.chainConn.Notifications():
			switch item := ntfn.(type) {
			case chain.BlockConnected:
				if item.Height != b.bestBlock.Height+1 {
					// Send historical block notifications to clients
					// starting with our currently best known block
					chainntnfs.Log.Infof("Missed blocks, " +
						"attempting to catch up")
					err := b.catchUpOnMissedBlocks(item.Height)
					if err != nil {
						chainntnfs.Log.Error(err)
						continue
					}
				}

				b.bestBlock.Height = item.Height
				b.bestBlock.Hash = &item.Hash

				err := b.handleBlockConnected(item.Height, item.Hash)
				if err != nil {
					chainntnfs.Log.Error(err)
				}
				continue

			case chain.BlockDisconnected:
				if item.Height != b.bestBlock.Height {
					chainntnfs.Log.Warnf("Received blocks "+
						"out of order: current height="+
						"%d, disconnected height=%d",
						b.bestBlock.Height, item.Height)
					continue
				}

				blockHash, err := b.chainConn.GetBlockHash(int64(item.Height - 1))
				if err != nil {
					chainntnfs.Log.Warnf("unable to get hash from block "+
						"with height=%d: %v", item.Height-1, err)
				}
				b.bestBlock.Height = item.Height - 1
				b.bestBlock.Hash = blockHash

				chainntnfs.Log.Infof("Block disconnected from "+
					"main chain: height=%v, sha=%v",
					item.Height, item.Hash)

				err = b.txConfNotifier.DisconnectTip(
					uint32(item.Height))
				if err != nil {
					chainntnfs.Log.Error(err)
				}

			case chain.RelevantTx:
				b.handleRelevantTx(item, b.bestBlock.Height)
			}

		case <-b.quit:
			break out
		}
	}
	b.wg.Done()
}

// handleRelevantTx notifies any clients of a relevant transaction.
func (b *BitcoindNotifier) handleRelevantTx(tx chain.RelevantTx, bestHeight int32) {
	msgTx := tx.TxRecord.MsgTx
	// First, check if this transaction spends an output
	// that has an existing spend notification for it.
	for i, txIn := range msgTx.TxIn {
		prevOut := txIn.PreviousOutPoint

		// If this transaction indeed does spend an
		// output which we have a registered
		// notification for, then create a spend
		// summary, finally sending off the details to
		// the notification subscriber.
		if clients, ok := b.spendNotifications[prevOut]; ok {
			spenderSha := msgTx.TxHash()
			spendDetails := &chainntnfs.SpendDetail{
				SpentOutPoint:     &prevOut,
				SpenderTxHash:     &spenderSha,
				SpendingTx:        &msgTx,
				SpenderInputIndex: uint32(i),
			}
			// TODO(roasbeef): after change to
			// loadfilter, only notify on block
			// inclusion?
			if tx.Block != nil {
				spendDetails.SpendingHeight = tx.Block.Height
			} else {
				spendDetails.SpendingHeight = bestHeight + 1
			}

			for _, ntfn := range clients {
				chainntnfs.Log.Infof("Dispatching "+
					"spend notification for "+
					"outpoint=%v", ntfn.targetOutpoint)
				ntfn.spendChan <- spendDetails

				// Close spendChan to ensure that any calls to Cancel will not
				// block. This is safe to do since the channel is buffered, and the
				// message can still be read by the receiver.
				close(ntfn.spendChan)
			}
			delete(b.spendNotifications, prevOut)
		}
	}
}

// historicalConfDetails looks up whether a transaction is already included in a
// block in the active chain and, if so, returns details about the confirmation.
func (b *BitcoindNotifier) historicalConfDetails(txid *chainhash.Hash,
	heightHint, currentHeight uint32) (*chainntnfs.TxConfirmation, error) {

	// First, we'll attempt to retrieve the transaction details using the
	// backend node's transaction index.
	txConf, err := b.confDetailsFromTxIndex(txid)
	if err != nil {
		return nil, err
	}

	if txConf != nil {
		return txConf, nil
	}

	// If the backend node's transaction index is not enabled, then we'll
	// fall back to manually scanning the chain's blocks, looking for the
	// block where the transaction was included in.
	return b.confDetailsManually(txid, heightHint, currentHeight)
}

// confDetailsFromTxIndex looks up whether a transaction is already included
// in a block in the active chain by using the backend node's transaction index.
// If the transaction is found, its confirmation details are returned.
// Otherwise, nil is returned.
func (b *BitcoindNotifier) confDetailsFromTxIndex(txid *chainhash.Hash,
) (*chainntnfs.TxConfirmation, error) {

	// If the transaction has some or all of its confirmations required,
	// then we may be able to dispatch it immediately.
	tx, err := b.chainConn.GetRawTransactionVerbose(txid)
	if err != nil {
		// Avoid returning an error if the transaction index is not
		// enabled to proceed with fallback methods.
		jsonErr, ok := err.(*btcjson.RPCError)
		if !ok || jsonErr.Code != btcjson.ErrRPCNoTxInfo {
			return nil, fmt.Errorf("unable to query for txid "+
				"%v: %v", txid, err)
		}
	}

	// Make sure we actually retrieved a transaction that is included in a
	// block. Without this, we won't be able to retrieve its confirmation
	// details.
	if tx == nil || tx.BlockHash == "" {
		return nil, nil
	}

	// As we need to fully populate the returned TxConfirmation struct,
	// grab the block in which the transaction was confirmed so we can
	// locate its exact index within the block.
	blockHash, err := chainhash.NewHashFromStr(tx.BlockHash)
	if err != nil {
		return nil, fmt.Errorf("unable to get block hash %v for "+
			"historical dispatch: %v", tx.BlockHash, err)
	}

	block, err := b.chainConn.GetBlockVerbose(blockHash)
	if err != nil {
		return nil, fmt.Errorf("unable to get block with hash %v for "+
			"historical dispatch: %v", blockHash, err)
	}

	// If the block was obtained, locate the transaction's index within the
	// block so we can give the subscriber full confirmation details.
	targetTxidStr := txid.String()
	for txIndex, txHash := range block.Tx {
		if txHash == targetTxidStr {
			return &chainntnfs.TxConfirmation{
				BlockHash:   blockHash,
				BlockHeight: uint32(block.Height),
				TxIndex:     uint32(txIndex),
			}, nil
		}
	}

	// We return an error because we should have found the transaction
	// within the block, but didn't.
	return nil, fmt.Errorf("unable to locate tx %v in block %v", txid,
		blockHash)
}

// confDetailsManually looks up whether a transaction is already included in a
// block in the active chain by scanning the chain's blocks, starting from the
// earliest height the transaction could have been included in, to the current
// height in the chain. If the transaction is found, its confirmation details
// are returned. Otherwise, nil is returned.
func (b *BitcoindNotifier) confDetailsManually(txid *chainhash.Hash,
	heightHint, currentHeight uint32) (*chainntnfs.TxConfirmation, error) {

	targetTxidStr := txid.String()

	// Begin scanning blocks at every height to determine where the
	// transaction was included in.
	for height := heightHint; height <= currentHeight; height++ {
		blockHash, err := b.chainConn.GetBlockHash(int64(height))
		if err != nil {
			return nil, fmt.Errorf("unable to get hash from block "+
				"with height %d", height)
		}

		block, err := b.chainConn.GetBlockVerbose(blockHash)
		if err != nil {
			return nil, fmt.Errorf("unable to get block with hash "+
				"%v: %v", blockHash, err)
		}

		for txIndex, txHash := range block.Tx {
			// If we're able to find the transaction in this block,
			// return its confirmation details.
			if txHash == targetTxidStr {
				return &chainntnfs.TxConfirmation{
					BlockHash:   blockHash,
					BlockHeight: height,
					TxIndex:     uint32(txIndex),
				}, nil
			}
		}
	}

	// If we reach here, then we were not able to find the transaction
	// within a block, so we avoid returning an error.
	return nil, nil
}

// handleBlockConnected applies a chain update for a new block. Any watched
// transactions included this block will processed to either send notifications
// now or after numConfirmations confs.
func (b *BitcoindNotifier) handleBlockConnected(height int32, hash chainhash.Hash) error {
	rawBlock, err := b.chainConn.GetBlock(&hash)
	if err != nil {
		return fmt.Errorf("unable to get block: %v", err)
	}

	chainntnfs.Log.Infof("New block: height=%v, sha=%v",
		height, hash)

	txns := btcutil.NewBlock(rawBlock).Transactions()
	err = b.txConfNotifier.ConnectTip(&hash,
		uint32(height), txns)
	if err != nil {
		return fmt.Errorf("unable to connect tip: %v", err)
	}

	b.notifyBlockEpochs(height, &hash)
	return nil
}

// notifyBlockEpochs notifies all registered block epoch clients of the newly
// connected block to the main chain.
func (b *BitcoindNotifier) notifyBlockEpochs(newHeight int32, newSha *chainhash.Hash) {
	epoch := &chainntnfs.BlockEpoch{
		Height: newHeight,
		Hash:   newSha,
	}

	for _, epochClient := range b.blockEpochClients {
		select {

		case epochClient.epochQueue.ChanIn() <- epoch:

		case <-epochClient.cancelChan:

		case <-b.quit:
		}
	}
}

// spendNotification couples a target outpoint along with the channel used for
// notifications once a spend of the outpoint has been detected.
type spendNotification struct {
	targetOutpoint *wire.OutPoint

	spendChan chan *chainntnfs.SpendDetail

	spendID uint64

	heightHint uint32
}

// spendCancel is a message sent to the BitcoindNotifier when a client wishes
// to cancel an outstanding spend notification that has yet to be dispatched.
type spendCancel struct {
	// op is the target outpoint of the notification to be cancelled.
	op wire.OutPoint

	// spendID the ID of the notification to cancel.
	spendID uint64
}

// RegisterSpendNtfn registers an intent to be notified once the target
// outpoint has been spent by a transaction on-chain. Once a spend of the target
// outpoint has been detected, the details of the spending event will be sent
// across the 'Spend' channel. The heightHint should represent the earliest
// height in the chain where the transaction could have been spent in.
func (b *BitcoindNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	heightHint uint32, _ bool) (*chainntnfs.SpendEvent, error) {

	ntfn := &spendNotification{
		targetOutpoint: outpoint,
		spendChan:      make(chan *chainntnfs.SpendDetail, 1),
		spendID:        atomic.AddUint64(&b.spendClientCounter, 1),
	}

	select {
	case <-b.quit:
		return nil, ErrChainNotifierShuttingDown
	case b.notificationRegistry <- ntfn:
	}

	if err := b.chainConn.NotifySpent([]*wire.OutPoint{outpoint}); err != nil {
		return nil, err
	}

	// The following conditional checks to ensure that when a spend
	// notification is registered, the output hasn't already been spent. If
	// the output is no longer in the UTXO set, the chain will be rescanned
	// from the point where the output was added. The rescan will dispatch
	// the notification.
	txOut, err := b.chainConn.GetTxOut(&outpoint.Hash, outpoint.Index, true)
	if err != nil {
		return nil, err
	}

	if txOut == nil {
		// First, we'll attempt to retrieve the transaction's block hash
		// using the backend's transaction index.
		tx, err := b.chainConn.GetRawTransactionVerbose(&outpoint.Hash)
		if err != nil {
			// Avoid returning an error if the transaction was not
			// found to proceed with fallback methods.
			jsonErr, ok := err.(*btcjson.RPCError)
			if !ok || jsonErr.Code != btcjson.ErrRPCNoTxInfo {
				return nil, fmt.Errorf("unable to query for "+
					"txid %v: %v", outpoint.Hash, err)
			}
		}

		var blockHash *chainhash.Hash
		if tx != nil && tx.BlockHash != "" {
			// If we're able to retrieve a valid block hash from the
			// transaction, then we'll use it as our rescan starting
			// point.
			blockHash, err = chainhash.NewHashFromStr(tx.BlockHash)
			if err != nil {
				return nil, err
			}
		} else {
			// Otherwise, we'll attempt to retrieve the hash for the
			// block at the heightHint.
			blockHash, err = b.chainConn.GetBlockHash(
				int64(heightHint),
			)
			if err != nil {
				return nil, err
			}
		}

		// We'll only scan old blocks if the transaction has actually
		// been included within a block. Otherwise, we'll encounter an
		// error when scanning for blocks. This can happens in the case
		// of a race condition, wherein the output itself is unspent,
		// and only arrives in the mempool after the getxout call.
		if blockHash != nil {
			// Rescan all the blocks until the current one.
			startHeight, err := b.chainConn.GetBlockHeight(
				blockHash,
			)
			if err != nil {
				return nil, err
			}

			_, endHeight, err := b.chainConn.GetBestBlock()
			if err != nil {
				return nil, err
			}

		out:
			for i := startHeight; i <= endHeight; i++ {
				blockHash, err := b.chainConn.GetBlockHash(int64(i))
				if err != nil {
					return nil, err
				}
				block, err := b.chainConn.GetBlock(blockHash)
				if err != nil {
					return nil, err
				}
				for _, tx := range block.Transactions {
					for _, in := range tx.TxIn {
						if in.PreviousOutPoint == *outpoint {
							relTx := chain.RelevantTx{
								TxRecord: &wtxmgr.TxRecord{
									MsgTx:    *tx,
									Hash:     tx.TxHash(),
									Received: block.Header.Timestamp,
								},
								Block: &wtxmgr.BlockMeta{
									Block: wtxmgr.Block{
										Hash:   block.BlockHash(),
										Height: i,
									},
									Time: block.Header.Timestamp,
								},
							}
							select {
							case <-b.quit:
								return nil, ErrChainNotifierShuttingDown
							case b.notificationRegistry <- relTx:
							}
							break out
						}
					}
				}
			}
		}
	}

	return &chainntnfs.SpendEvent{
		Spend: ntfn.spendChan,
		Cancel: func() {
			cancel := &spendCancel{
				op:      *outpoint,
				spendID: ntfn.spendID,
			}

			// Submit spend cancellation to notification dispatcher.
			select {
			case b.notificationCancels <- cancel:
				// Cancellation is being handled, drain the spend chan until it is
				// closed before yielding to the caller.
				for {
					select {
					case _, ok := <-ntfn.spendChan:
						if !ok {
							return
						}
					case <-b.quit:
						return
					}
				}
			case <-b.quit:
			}
		},
	}, nil
}

// confirmationNotification represents a client's intent to receive a
// notification once the target txid reaches numConfirmations confirmations.
type confirmationNotification struct {
	chainntnfs.ConfNtfn
	heightHint uint32
}

// RegisterConfirmationsNtfn registers a notification with BitcoindNotifier
// which will be triggered once the txid reaches numConfs number of
// confirmations.
func (b *BitcoindNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	numConfs, heightHint uint32) (*chainntnfs.ConfirmationEvent, error) {

	ntfn := &confirmationNotification{
		ConfNtfn: chainntnfs.ConfNtfn{
			TxID:             txid,
			NumConfirmations: numConfs,
			Event:            chainntnfs.NewConfirmationEvent(numConfs),
		},
		heightHint: heightHint,
	}

	select {
	case <-b.quit:
		return nil, ErrChainNotifierShuttingDown
	case b.notificationRegistry <- ntfn:
		return ntfn.Event, nil
	}
}

// blockEpochRegistration represents a client's intent to receive a
// notification with each newly connected block.
type blockEpochRegistration struct {
	epochID uint64

	epochChan chan *chainntnfs.BlockEpoch

	epochQueue *chainntnfs.ConcurrentQueue

	bestBlock *chainntnfs.BlockEpoch

	cancelChan chan struct{}

	wg sync.WaitGroup
}

// epochCancel is a message sent to the BitcoindNotifier when a client wishes
// to cancel an outstanding epoch notification that has yet to be dispatched.
type epochCancel struct {
	epochID uint64
}

// RegisterBlockEpochNtfn returns a BlockEpochEvent which subscribes the
// caller to receive notifications, of each new block connected to the main
// chain.
func (b *BitcoindNotifier) RegisterBlockEpochNtfn(
	bestBlock *chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {
	reg := &blockEpochRegistration{
		epochQueue: chainntnfs.NewConcurrentQueue(20),
		epochChan:  make(chan *chainntnfs.BlockEpoch, 20),
		cancelChan: make(chan struct{}),
		epochID:    atomic.AddUint64(&b.epochClientCounter, 1),
		bestBlock:  bestBlock,
	}
	reg.epochQueue.Start()

	// Before we send the request to the main goroutine, we'll launch a new
	// goroutine to proxy items added to our queue to the client itself.
	// This ensures that all notifications are received *in order*.
	reg.wg.Add(1)
	go func() {
		defer reg.wg.Done()

		for {
			select {
			case ntfn := <-reg.epochQueue.ChanOut():
				blockNtfn := ntfn.(*chainntnfs.BlockEpoch)
				select {
				case reg.epochChan <- blockNtfn:

				case <-reg.cancelChan:
					return

				case <-b.quit:
					return
				}

			case <-reg.cancelChan:
				return

			case <-b.quit:
				return
			}
		}
	}()

	select {
	case <-b.quit:
		// As we're exiting before the registration could be sent,
		// we'll stop the queue now ourselves.
		reg.epochQueue.Stop()

		return nil, errors.New("chainntnfs: system interrupt while " +
			"attempting to register for block epoch notification.")
	case b.notificationRegistry <- reg:
		return &chainntnfs.BlockEpochEvent{
			Epochs: reg.epochChan,
			Cancel: func() {
				cancel := &epochCancel{
					epochID: reg.epochID,
				}

				// Submit epoch cancellation to notification dispatcher.
				select {
				case b.notificationCancels <- cancel:
					// Cancellation is being handled, drain the epoch channel until it is
					// closed before yielding to caller.
					for {
						select {
						case _, ok := <-reg.epochChan:
							if !ok {
								return
							}
						case <-b.quit:
							return
						}
					}
				case <-b.quit:
				}
			},
		}, nil
	}
}

// catchUpOnMissedBlocks is called when the chain backend misses a series of
// blocks. It dispatches all relevant notifications for the missed blocks.
func (b *BitcoindNotifier) catchUpOnMissedBlocks(newHeight int32) error {
	// We want to start catching up at the block after the client's best
	// known block, to avoid a redundant notification
	startingHeight := b.bestBlock.Height + 1
	hashAtBestHeight, err := b.chainConn.GetBlockHash(int64(b.bestBlock.Height))
	if err != nil {
		return fmt.Errorf("unable to find blockhash for height=%d: %v",
			b.bestBlock.Height, err)
	}

	// If a reorg causes the hash to be incorrect, determine the height of the
	// first common parent block between the notifier's view of the chain and the
	// main chain and dispatch notifications from there
	if b.bestBlock.Hash != nil && *hashAtBestHeight != *b.bestBlock.Hash {
		commonAncestorHeight, err := b.getCommonBlockAncestorHeight(
			*b.bestBlock.Hash,
			*hashAtBestHeight,
		)
		if err != nil {
			return fmt.Errorf("unable to find common ancestor: %v", err)
		}
		startingHeight = commonAncestorHeight + 1
	}
	for height := startingHeight; height < newHeight; height++ {
		hash, err := b.chainConn.GetBlockHash(int64(height))
		if err != nil {
			return fmt.Errorf("unable to get block hash: %v", err)
		}
		if err := b.handleBlockConnected(height, *hash); err != nil {
			return fmt.Errorf("error on handling connected block: %v",
				err)
		}
	}
	return nil
}

// catchUpClientOnBlocks is called when a notification client's best block
// is behind our current best block.
// It dispatches block notifications for all the blocks missed by the client
// or does nothing if the client doesn't send us its best block.
func (b *BitcoindNotifier) catchUpClientOnBlocks(bestBlock *chainntnfs.BlockEpoch) error {
	if bestBlock == nil {
		return nil
	}

	// We want to start catching up at the block after the client's best
	// known block, to avoid a redundant notification
	startingHeight := bestBlock.Height + 1
	hashAtBestHeight, err := b.chainConn.GetBlockHash(int64(bestBlock.Height))
	if err != nil {
		return fmt.Errorf("unable to find blockhash for height=%d: %v",
			bestBlock.Height, err)
	}

	// If a reorg causes the hash to be incorrect, determine the height of the
	// first common parent block between the client's view of the chain and the
	// main chain and dispatch notifications from there
	if bestBlock.Hash != nil && *hashAtBestHeight != *bestBlock.Hash {
		commonAncestorHeight, err := b.getCommonBlockAncestorHeight(
			*bestBlock.Hash,
			*hashAtBestHeight,
		)
		if err != nil {
			return fmt.Errorf("unable to find common ancestor: %v", err)
		}
		startingHeight = commonAncestorHeight + 1
	}
	for height := startingHeight; height <= b.bestBlock.Height; height++ {
		hash, err := b.chainConn.GetBlockHash(int64(height))
		if err != nil {
			return fmt.Errorf("unable to find blockhash for height=%d: %v",
				bestBlock.Height, err)
		}
		b.notifyBlockEpochs(height, hash)
	}
	return nil
}

// getCommonBlockAncestorHeight takes in:
// (1) the hash of a block that has been reorged out of the main chain
// (2) the hash of the block of the same height from the main chain
// It returns the height of the nearest common ancestor between the two hashes,
// or an error
func (b *BitcoindNotifier) getCommonBlockAncestorHeight(
	reorgHash, chainHash chainhash.Hash) (int32, error) {
	for reorgHash != chainHash {
		reorgHeader, err := b.chainConn.GetBlockHeader(&reorgHash)
		if err != nil {
			return 0, fmt.Errorf("unable to get header for hash=%v: %v", reorgHash, err)
		}
		chainHeader, err := b.chainConn.GetBlockHeader(&chainHash)
		if err != nil {
			return 0, fmt.Errorf("unable to get header for hash=%v: %v", chainHash, err)
		}
		reorgHash = reorgHeader.PrevBlock
		chainHash = chainHeader.PrevBlock
	}
	verboseHeader, err := b.chainConn.GetBlockHeaderVerbose(&chainHash)
	if err != nil {
		return 0, fmt.Errorf("unable to get verbose header for hash=%v: %v", chainHash, err)
	}
	return verboseHeader.Height, nil
}
