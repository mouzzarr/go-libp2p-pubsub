package pubsub

import (
	"fmt"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p-core/connmgr"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/protocol"
)

var (
	// GossipSubConnTagValueDirectPeer is the connection manager tag value to
	// apply to direct peers. This should be high, as we want to prioritize these
	// connections above all others.
	GossipSubConnTagValueDirectPeer = 1000

	// GossipSubConnTagValueMeshPeer is the connection manager tag value to apply to
	// peers in a topic mesh. If a peer is in the mesh for multiple topics, their
	// connection will be tagged separately for each.
	GossipSubConnTagValueMeshPeer = 20

	// GossipSubConnTagBumpMessageDelivery is the amount to add to the connection manager
	// tag that tracks message deliveries. Each time a peer is the first to deliver a
	// message within a topic, we "bump" a tag by this amount, up to a maximum
	// of GossipSubConnTagMessageDeliveryCap.
	// Note that the delivery tags decay over time, decreasing by GossipSubConnTagDecayAmount
	// at every GossipSubConnTagDecayInterval.
	GossipSubConnTagBumpMessageDelivery = 1

	// GossipSubConnTagDecayInterval is the decay interval for decaying connection manager tags.
	GossipSubConnTagDecayInterval = 10 * time.Minute

	// GossipSubConnTagDecayAmount is subtracted from decaying tag values at each decay interval.
	GossipSubConnTagDecayAmount = 1

	// GossipSubConnTagMessageDeliveryCap is the maximum value for the connection manager tags that
	// track message deliveries.
	GossipSubConnTagMessageDeliveryCap = 15
)

// tagTracer is an internal tracer that applies connection manager tags to peer
// connections based on their behavior.
//
// We tag a peer's connections for the following reasons:
// - Directly connected peers are tagged with GossipSubConnTagValueDirectPeer (default 1000).
// - Mesh peers are tagged with a value of GossipSubConnTagValueMeshPeer (default 20).
//   If a peer is in multiple topic meshes, they'll be tagged for each.
// - For each message that we receive, we bump a delivery tag for peer that delivered the message
//   first.
//   The delivery tags have a maximum value, GossipSubConnTagMessageDeliveryCap, and they decay at
//   a rate of GossipSubConnTagDecayAmount / GossipSubConnTagDecayInterval.
type tagTracer struct {
	sync.RWMutex

	cmgr     connmgr.ConnManager
	decayer  connmgr.Decayer
	decaying map[string]connmgr.DecayingTag
	direct   map[peer.ID]struct{}
}

func newTagTracer(cmgr connmgr.ConnManager) *tagTracer {
	decayer, ok := connmgr.SupportsDecay(cmgr)
	if !ok {
		log.Warnf("connection manager does not support decaying tags, delivery tags will not be applied")
	}
	return &tagTracer{
		cmgr:     cmgr,
		decayer:  decayer,
		decaying: make(map[string]connmgr.DecayingTag),
	}
}

func (t *tagTracer) tagPeerIfDirect(p peer.ID) {
	if t.direct == nil {
		return
	}

	// tag peer if it is a direct peer
	_, direct := t.direct[p]
	if direct {
		t.cmgr.TagPeer(p, "pubsub:direct", GossipSubConnTagValueDirectPeer)
	}
}

func (t *tagTracer) tagMeshPeer(p peer.ID, topic string) {
	tag := topicTag(topic)
	t.cmgr.TagPeer(p, tag, GossipSubConnTagValueMeshPeer)
}

func (t *tagTracer) untagMeshPeer(p peer.ID, topic string) {
	tag := topicTag(topic)
	t.cmgr.UntagPeer(p, tag)
}

func topicTag(topic string) string {
	return fmt.Sprintf("pubsub:%s", topic)
}

func (t *tagTracer) addDeliveryTag(topic string) {
	if t.decayer == nil {
		return
	}

	t.Lock()
	defer t.Unlock()
	tag, err := t.decayingDeliveryTag(topic)
	if err != nil {
		log.Warnf("unable to create decaying delivery tag: %s", err)
		return
	}
	t.decaying[topic] = tag
}

func (t *tagTracer) removeDeliveryTag(topic string) {
	t.Lock()
	defer t.Unlock()
	delete(t.decaying, topic)
}

func (t *tagTracer) decayingDeliveryTag(topic string) (connmgr.DecayingTag, error) {
	if t.decayer == nil {
		return nil, fmt.Errorf("connection manager does not support decaying tags")
	}
	name := fmt.Sprintf("pubsub-deliveries:%s", topic)

	// decrement tag value by GossipSubConnTagDecayAmount at each decay interval
	decayFn := func(value connmgr.DecayingValue) (after int, rm bool) {
		v := value.Value - GossipSubConnTagDecayAmount
		return v, v <= 0
	}

	// bump up to max of GossipSubConnTagMessageDeliveryCap
	bumpFn := func(value connmgr.DecayingValue, delta int) (after int) {
		val := value.Value + delta
		if val > GossipSubConnTagMessageDeliveryCap {
			return GossipSubConnTagMessageDeliveryCap
		}
		return val
	}

	return t.decayer.RegisterDecayingTag(name, GossipSubConnTagDecayInterval, decayFn, bumpFn)
}

func (t *tagTracer) bumpDeliveryTag(p peer.ID, topic string) error {
	t.RLock()
	defer t.RUnlock()

	tag, ok := t.decaying[topic]
	if !ok {
		return fmt.Errorf("no decaying tag registered for topic %s", topic)
	}
	return tag.Bump(p, GossipSubConnTagBumpMessageDelivery)
}

func (t *tagTracer) bumpTagsForMessage(p peer.ID, msg *Message) {
	for _, topic := range msg.TopicIDs {
		err := t.bumpDeliveryTag(p, topic)
		if err != nil {
			log.Warnf("error bumping delivery tag: %s", err)
		}
	}
}

// -- internalTracer interface methods
var _ internalTracer = (*tagTracer)(nil)

func (t *tagTracer) AddPeer(p peer.ID, proto protocol.ID) {
	t.tagPeerIfDirect(p)
}

func (t *tagTracer) Join(topic string) {
	t.addDeliveryTag(topic)
}

func (t *tagTracer) DeliverMessage(msg *Message) {
	// TODO: also give a bump to "near-first" message deliveries
	t.bumpTagsForMessage(msg.ReceivedFrom, msg)
}

func (t *tagTracer) Leave(topic string) {
	t.removeDeliveryTag(topic)
}

func (t *tagTracer) Graft(p peer.ID, topic string) {
	t.tagMeshPeer(p, topic)
}

func (t *tagTracer) Prune(p peer.ID, topic string) {
	t.untagMeshPeer(p, topic)
}

func (t *tagTracer) RemovePeer(peer.ID)             {}
func (t *tagTracer) ValidateMessage(*Message)       {}
func (t *tagTracer) RejectMessage(*Message, string) {}
func (t *tagTracer) DuplicateMessage(*Message)      {}