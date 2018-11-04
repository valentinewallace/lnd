package channelnotifier

import (
	"sync/atomic"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/subscribe"
)

// ChannelNotifier is a subsystem which all active, inactive, and closed channel
// events pipe through. It takes subscriptions for its events, and whenever
// it receives a new event it notifies its subscribers over the proper channel.
type ChannelNotifier struct {
	started uint32
	stopped uint32

	ntfnServer *subscribe.Server
}

// ActiveChannelEvent represents a new event where a channel becomes active.
type ActiveChannelEvent struct {
	// Channel is the channel that has become active.
	Channel *channeldb.OpenChannel
}

// InactiveChannelEvent represents a new event where a channel becomes inactive.
type InactiveChannelEvent struct {
	// Channel is the channel that has become inactive.
	Channel *channeldb.OpenChannel
}

// ClosedChannelEvent represents a new event where a channel becomes closed.
type ClosedChannelEvent struct {
	// CloseSummary is the summary of the channel close that has occurred.
	CloseSummary *channeldb.ChannelCloseSummary
}

// New creates a new channel notifier. The ChannelNotifier gets channel
// events from peers and from the chain arbitrator, and dispatches them to
// its clients.
func New() *ChannelNotifier {
	return &ChannelNotifier{
		ntfnServer: subscribe.NewServer(),
	}
}

// Start starts the ChannelNotifier and all goroutines it needs to carry out its task.
func (c *ChannelNotifier) Start() error {
	if !atomic.CompareAndSwapUint32(&c.started, 0, 1) {
		return nil
	}

	log.Tracef("ChannelNotifier %v starting", c)

	if err := c.ntfnServer.Start(); err != nil {
		return err
	}

	return nil
}

// Stop signals the notifier for a graceful shutdown.
func (c *ChannelNotifier) Stop() {
	if !atomic.CompareAndSwapUint32(&c.stopped, 0, 1) {
		return
	}

	c.ntfnServer.Stop()
}

// SubscribeChannelEvents returns a subscribe.Client that will receive updates
// any time the Server is made aware of a new event.
func (c *ChannelNotifier) SubscribeChannelEvents() (*subscribe.Client, error) {
	return c.ntfnServer.Subscribe()
}

// NotifyActiveChannelEvent notifies the channelEventNotifier goroutine that a
// channel has become active.
func (c *ChannelNotifier) NotifyActiveChannelEvent(channel *channeldb.OpenChannel) {
	// Send the active event to all channel event subscribers.
	event := ActiveChannelEvent{Channel: channel}
	if err := c.ntfnServer.SendUpdate(event); err != nil {
		log.Warnf("error sending active channel update: %v", err)
	}
}

// NotifyInactiveChannelEvent notifies the channelEventNotifier goroutine that a
// channel has become inactive.
func (c *ChannelNotifier) NotifyInactiveChannelEvent(channel *channeldb.OpenChannel) {
	// Send the inactive event to all channel event subscribers.
	event := InactiveChannelEvent{Channel: channel}
	if err := c.ntfnServer.SendUpdate(event); err != nil {
		log.Warnf("error sending inactive channel update: %v", err)
	}
}

// NotifyClosedChannelEvent notifies the channelEventNotifier goroutine that a
// channel has closed.
func (c *ChannelNotifier) NotifyClosedChannelEvent(summary *channeldb.ChannelCloseSummary) {
	// Send the closed event to all channel event subscribers.
	event := ClosedChannelEvent{CloseSummary: summary}
	if err := c.ntfnServer.SendUpdate(event); err != nil {
		log.Warnf("error sending closed channel update: %v", err)
	}
}
