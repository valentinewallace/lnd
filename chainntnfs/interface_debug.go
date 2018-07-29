// +build debug

package chainntnfs

// TestChainNotifier enables the use of methods that are only
// present during testing for ChainNotifiers.
type TestChainNotifier interface {
	ChainNotifier

	// UnsafeStart enables notifiers to start up with a specific
	// best block. Used for testing.
	UnsafeStart(BlockEpoch) error

	// GetBestHeight allows tests to retrieve the best height
	// for a notifier.
	GetBestHeight() uint32
}
