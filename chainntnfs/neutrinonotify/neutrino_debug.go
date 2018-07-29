// +build debug

package neutrinonotify

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/neutrino"
	"github.com/lightningnetwork/lnd/chainntnfs"
)

// UnsafeStart starts the notifier with a specified best block.
// Its bestBlock, txConfNotifier and neutrino node are initialized with bestBlock.
// Used for testing.
func (n *NeutrinoNotifier) UnsafeStart(block chainntnfs.BlockEpoch) error {
	// First, we'll obtain the latest block height of the p2p node. We'll
	// start the auto-rescan from this point. Once a caller actually wishes
	// to register a chain view, the rescan state will be rewound
	// accordingly.
	startingPoint := &waddrmgr.BlockStamp{
		Height: block.Height,
		Hash:   *block.Hash,
	}
	n.bestHeight = uint32(block.Height)

	// Next, we'll create our set of rescan options. Currently it's
	// required that a user MUST set an addr/outpoint/txid when creating a
	// rescan. To get around this, we'll add a "zero" outpoint, that won't
	// actually be matched.
	var zeroHash chainhash.Hash
	rescanOptions := []neutrino.RescanOption{
		neutrino.StartBlock(startingPoint),
		neutrino.QuitChan(n.quit),
		neutrino.NotificationHandlers(
			rpcclient.NotificationHandlers{
				OnFilteredBlockConnected:    n.onFilteredBlockConnected,
				OnFilteredBlockDisconnected: n.onFilteredBlockDisconnected,
			},
		),
		neutrino.WatchTxIDs(zeroHash),
	}

	n.txConfNotifier = chainntnfs.NewTxConfNotifier(
		uint32(block.Height), reorgSafetyLimit)

	n.chainConn = &NeutrinoChainConn{n.p2pNode}

	// Finally, we'll create our rescan struct, start it, and launch all
	// the goroutines we need to operate this ChainNotifier instance.
	n.chainView = n.p2pNode.NewRescan(rescanOptions...)
	n.rescanErr = n.chainView.Start()

	n.chainUpdates.Start()

	n.wg.Add(1)
	go n.notificationDispatcher()

	return nil
}

// GetBestHeight returns the best height for the neutrino notifier.
func (n *NeutrinoNotifier) GetBestHeight() uint32 {
	n.heightMtx.RLock()
	defer n.heightMtx.RUnlock()

	return n.bestHeight
}
