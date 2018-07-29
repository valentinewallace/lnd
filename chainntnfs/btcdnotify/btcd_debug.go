// +build debug

package btcdnotify

import "github.com/lightningnetwork/lnd/chainntnfs"

// UnsafeStart starts the notifier with a specified best block.
// Both its bestBlock and its txConfNotifier are initialized with bestBlock.
// Used for testing.
func (b *BtcdNotifier) UnsafeStart(bestBlock chainntnfs.BlockEpoch) error {
	// Connect to btcd, and register for notifications on connected, and
	// disconnected blocks.
	if err := b.chainConn.Connect(20); err != nil {
		return err
	}
	if err := b.chainConn.NotifyBlocks(); err != nil {
		return err
	}

	b.txConfNotifier = chainntnfs.NewTxConfNotifier(
		uint32(bestBlock.Height), reorgSafetyLimit)

	b.chainUpdates.Start()
	b.txUpdates.Start()

	b.bestBlock = bestBlock

	b.wg.Add(1)
	go b.notificationDispatcher()

	return nil
}

// GetBestHeight returns the best height for the btcd notifier.
func (b *BtcdNotifier) GetBestHeight() uint32 {
	return 0
}
