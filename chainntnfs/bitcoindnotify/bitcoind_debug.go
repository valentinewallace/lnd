// +build debug

package bitcoindnotify

import "github.com/lightningnetwork/lnd/chainntnfs"

// UnsafeStart starts the notifier with a specified best block.
// Both its bestBlock and its txConfNotifier are initialized with bestBlock.
// Used for testing.
func (b *BitcoindNotifier) UnsafeStart(bestBlock chainntnfs.BlockEpoch) error {
	// Connect to bitcoind, and register for notifications on connected,
	// and disconnected blocks.
	if err := b.chainConn.Start(); err != nil {
		return err
	}
	if err := b.chainConn.NotifyBlocks(); err != nil {
		return err
	}

	b.txConfNotifier = chainntnfs.NewTxConfNotifier(
		uint32(bestBlock.Height), reorgSafetyLimit)

	b.bestBlock = bestBlock

	b.wg.Add(1)
	go b.notificationDispatcher()

	return nil
}

// GetBestHeight returns the best height for the bitcoind notifier.
func (b *BitcoindNotifier) GetBestHeight() uint32 {
	return 0
}
