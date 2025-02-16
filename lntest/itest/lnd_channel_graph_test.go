package itest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/peersrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntemp"
	"github.com/lightningnetwork/lnd/lntemp/node"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// testUpdateChanStatus checks that calls to the UpdateChanStatus RPC update
// the channel graph as expected, and that channel state is properly updated
// in the presence of interleaved node disconnects / reconnects.
//
// NOTE: this test can be flaky as we are testing the chan-enable-timeout and
// chan-disable-timeout flags here.
//
// For chan-enable-timeout, setting this value too small will cause an enabled
// channel never be considered active by our channel status manager. Upon
// reconnection, our Brontide will send a request to enable this channel after
// the "chan-enable-timeout" has passed. The request is handled by the channel
// status manager, which will check the channel's eligibility to forward links
// by asking htlcswitch/link. Meanwhile, the htlcswitch/link won't mark the
// link as eligible unless it has finished its initialization, which takes some
// time. Thus, if the Brontide sends a request too early it will get a false
// report saying the channel link is not eligible because that link hasn't
// finished its initialization.
//
// For chan-disable-timeout, setting this value too small will cause an already
// enabled channel being marked as disabled. For instance, if some operations
// take more than 5 seconds to finish, the channel will be marked as disabled,
// thus a following operation will fail if it relies on the channel being
// enabled.
func testUpdateChanStatus(ht *lntemp.HarnessTest) {
	// Create two fresh nodes and open a channel between them.
	alice, bob := ht.Alice, ht.Bob
	args := []string{
		"--minbackoff=60s",
		"--chan-enable-timeout=3s",
		"--chan-disable-timeout=6s",
		"--chan-status-sample-interval=0.5s",
	}
	ht.RestartNodeWithExtraArgs(alice, args)
	ht.RestartNodeWithExtraArgs(bob, args)
	ht.EnsureConnected(alice, bob)

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	chanAmt := btcutil.Amount(100000)
	chanPoint := ht.OpenChannel(
		alice, bob, lntemp.OpenChannelParams{Amt: chanAmt},
	)
	defer ht.CloseChannel(alice, chanPoint)

	// assertEdgeDisabled ensures that Alice has the correct Disabled state
	// for given channel from her DescribeGraph.
	assertEdgeDisabled := func(disabled bool) {
		outPoint := ht.OutPointFromChannelPoint(chanPoint)

		err := wait.NoError(func() error {
			edge := ht.AssertNumEdges(alice, 1, true)[0]
			if edge.ChanPoint != outPoint.String() {
				return fmt.Errorf("expected chan_point %v, "+
					"got %v", outPoint, edge.ChanPoint)
			}
			var policy *lnrpc.RoutingPolicy
			if alice.PubKeyStr == edge.Node1Pub {
				policy = edge.Node1Policy
			} else {
				policy = edge.Node2Policy
			}
			if disabled != policy.Disabled {
				return fmt.Errorf("expected policy.Disabled "+
					"to be %v, but policy was %v", disabled,
					policy.Disabled)
			}

			return nil
		}, defaultTimeout)
		require.NoError(ht, err, "assert edge disabled timeout")
	}

	// aliceSendReq sends an UpdateChanStatus request to Alice.
	aliceSendReq := func(action routerrpc.ChanStatusAction) {
		req := &routerrpc.UpdateChanStatusRequest{
			ChanPoint: chanPoint,
			Action:    action,
		}
		alice.RPC.UpdateChanStatus(req)
	}

	// Initially, the channel between Alice and Bob should not be disabled.
	//
	// NOTE: This check should happen right after the channel openning as
	// we've used a short timeout value for `--chan-disable-timeout`. If we
	// wait longer than that we might get a flake saying the channel is
	// disabled.
	assertEdgeDisabled(false)

	// Launch a node for Carol which will connect to Alice and Bob in order
	// to receive graph updates. This will ensure that the channel updates
	// are propagated throughout the network.
	carol := ht.NewNode("Carol", nil)

	// assertChannelUpdate checks that the required policy update has been
	// heard in Carol's network.
	assertChannelUpdate := func(node *node.HarnessNode,
		policy *lnrpc.RoutingPolicy) {

		ht.AssertChannelPolicyUpdate(
			carol, node, policy, chanPoint, false,
		)
	}

	// Connect both Alice and Bob to the new node Carol, so she can sync
	// her graph.
	ht.ConnectNodes(alice, carol)
	ht.ConnectNodes(bob, carol)
	ht.WaitForGraphSync(carol)

	// If the above waitForGraphSync takes more than 4 seconds, the channel
	// Alice=>Bob will be marked as disabled now. Thus we connect Alice and
	// Bob again to make sure the channel is alive.
	ht.EnsureConnected(alice, bob)

	// When updating the state of the channel between Alice and Bob, we
	// should expect to see channel updates with the default routing
	// policy. The value of "Disabled" will depend on the specific scenario
	// being tested.
	expectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMsat:      int64(chainreg.DefaultBitcoinBaseFeeMSat),
		FeeRateMilliMsat: int64(chainreg.DefaultBitcoinFeeRate),
		TimeLockDelta:    chainreg.DefaultBitcoinTimeLockDelta,
		MinHtlc:          1000, // default value
		MaxHtlcMsat:      calculateMaxHtlc(chanAmt),
	}

	// Manually disable the channel and ensure that a "Disabled = true"
	// update is propagated.
	aliceSendReq(routerrpc.ChanStatusAction_DISABLE)
	expectedPolicy.Disabled = true
	assertChannelUpdate(alice, expectedPolicy)

	// Re-enable the channel and ensure that a "Disabled = false" update is
	// propagated.
	aliceSendReq(routerrpc.ChanStatusAction_ENABLE)
	expectedPolicy.Disabled = false
	assertChannelUpdate(alice, expectedPolicy)

	// Manually enabling a channel should NOT prevent subsequent
	// disconnections from automatically disabling the channel again (we
	// don't want to clutter the network with channels that are falsely
	// advertised as enabled when they don't work).
	ht.DisconnectNodes(alice, bob)
	expectedPolicy.Disabled = true
	assertChannelUpdate(alice, expectedPolicy)
	assertChannelUpdate(bob, expectedPolicy)

	// Reconnecting the nodes should propagate a "Disabled = false" update.
	ht.EnsureConnected(alice, bob)
	expectedPolicy.Disabled = false
	assertChannelUpdate(alice, expectedPolicy)
	assertChannelUpdate(bob, expectedPolicy)

	// Manually disabling the channel should prevent a subsequent
	// disconnect/reconnect from re-enabling the channel on Alice's end.
	// Note the asymmetry between manual enable and manual disable!
	aliceSendReq(routerrpc.ChanStatusAction_DISABLE)

	// Alice sends out the "Disabled = true" update in response to the
	// ChanStatusAction_DISABLE request.
	expectedPolicy.Disabled = true
	assertChannelUpdate(alice, expectedPolicy)

	ht.DisconnectNodes(alice, bob)

	// Bob sends a "Disabled = true" update upon detecting the disconnect.
	expectedPolicy.Disabled = true
	assertChannelUpdate(bob, expectedPolicy)

	// Bob sends a "Disabled = false" update upon detecting the reconnect.
	ht.EnsureConnected(alice, bob)
	expectedPolicy.Disabled = false
	assertChannelUpdate(bob, expectedPolicy)

	// However, since we manually disabled the channel on Alice's end, the
	// policy on Alice's end should still be "Disabled = true". Again, note
	// the asymmetry between manual enable and manual disable!
	assertEdgeDisabled(true)

	ht.DisconnectNodes(alice, bob)

	// Bob sends a "Disabled = true" update upon detecting the disconnect.
	expectedPolicy.Disabled = true
	assertChannelUpdate(bob, expectedPolicy)

	// After restoring automatic channel state management on Alice's end,
	// BOTH Alice and Bob should set the channel state back to "enabled" on
	// reconnect.
	aliceSendReq(routerrpc.ChanStatusAction_AUTO)
	ht.EnsureConnected(alice, bob)

	expectedPolicy.Disabled = false
	assertChannelUpdate(alice, expectedPolicy)
	assertChannelUpdate(bob, expectedPolicy)
	assertEdgeDisabled(false)
}

// testUnannouncedChannels checks unannounced channels are not returned by
// describeGraph RPC request unless explicitly asked for.
func testUnannouncedChannels(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	amount := funding.MaxBtcFundingAmount

	// Open a channel between Alice and Bob, ensuring the
	// channel has been opened properly.
	chanOpenUpdate := openChannelStream(
		t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: amount,
		},
	)

	// Mine 2 blocks, and check that the channel is opened but not yet
	// announced to the network.
	mineBlocks(t, net, 2, 1)

	// One block is enough to make the channel ready for use, since the
	// nodes have defaultNumConfs=1 set.
	fundingChanPoint, err := net.WaitForChannelOpen(chanOpenUpdate)
	if err != nil {
		t.Fatalf("error while waiting for channel open: %v", err)
	}

	// Alice should have 1 edge in her graph.
	req := &lnrpc.ChannelGraphRequest{
		IncludeUnannounced: true,
	}
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	chanGraph, err := net.Alice.DescribeGraph(ctxt, req)
	if err != nil {
		t.Fatalf("unable to query alice's graph: %v", err)
	}

	numEdges := len(chanGraph.Edges)
	if numEdges != 1 {
		t.Fatalf("expected to find 1 edge in the graph, found %d", numEdges)
	}

	// Channels should not be announced yet, hence Alice should have no
	// announced edges in her graph.
	req.IncludeUnannounced = false
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	chanGraph, err = net.Alice.DescribeGraph(ctxt, req)
	if err != nil {
		t.Fatalf("unable to query alice's graph: %v", err)
	}

	numEdges = len(chanGraph.Edges)
	if numEdges != 0 {
		t.Fatalf("expected to find 0 announced edges in the graph, found %d",
			numEdges)
	}

	// Mine 4 more blocks, and check that the channel is now announced.
	mineBlocks(t, net, 4, 0)

	// Give the network a chance to learn that auth proof is confirmed.
	var predErr error
	err = wait.Predicate(func() bool {
		// The channel should now be announced. Check that Alice has 1
		// announced edge.
		req.IncludeUnannounced = false
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		chanGraph, err = net.Alice.DescribeGraph(ctxt, req)
		if err != nil {
			predErr = fmt.Errorf("unable to query alice's graph: %v", err)
			return false
		}

		numEdges = len(chanGraph.Edges)
		if numEdges != 1 {
			predErr = fmt.Errorf("expected to find 1 announced edge in "+
				"the graph, found %d", numEdges)
			return false
		}
		return true
	}, defaultTimeout)
	if err != nil {
		t.Fatalf("%v", predErr)
	}

	// The channel should now be announced. Check that Alice has 1 announced
	// edge.
	req.IncludeUnannounced = false
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	chanGraph, err = net.Alice.DescribeGraph(ctxt, req)
	if err != nil {
		t.Fatalf("unable to query alice's graph: %v", err)
	}

	numEdges = len(chanGraph.Edges)
	if numEdges != 1 {
		t.Fatalf("expected to find 1 announced edge in the graph, found %d",
			numEdges)
	}

	// Close the channel used during the test.
	closeChannelAndAssert(t, net, net.Alice, fundingChanPoint, false)
}

func testGraphTopologyNotifications(net *lntest.NetworkHarness, t *harnessTest) {
	t.t.Run("pinned", func(t *testing.T) {
		ht := newHarnessTest(t, net)
		testGraphTopologyNtfns(net, ht, true)
	})
	t.t.Run("unpinned", func(t *testing.T) {
		ht := newHarnessTest(t, net)
		testGraphTopologyNtfns(net, ht, false)
	})
}

func testGraphTopologyNtfns(net *lntest.NetworkHarness, t *harnessTest, pinned bool) {
	ctxb := context.Background()

	const chanAmt = funding.MaxBtcFundingAmount

	// Spin up Bob first, since we will need to grab his pubkey when
	// starting Alice to test pinned syncing.
	bob := net.NewNode(t.t, "bob", nil)
	defer shutdownAndAssert(net, t, bob)

	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	bobInfo, err := bob.GetInfo(ctxt, &lnrpc.GetInfoRequest{})
	require.NoError(t.t, err)
	bobPubkey := bobInfo.IdentityPubkey

	// For unpinned syncing, start Alice as usual. Otherwise grab Bob's
	// pubkey to include in his pinned syncer set.
	var aliceArgs []string
	if pinned {
		aliceArgs = []string{
			"--numgraphsyncpeers=0",
			fmt.Sprintf("--gossip.pinned-syncers=%s", bobPubkey),
		}
	}

	alice := net.NewNode(t.t, "alice", aliceArgs)
	defer shutdownAndAssert(net, t, alice)

	// Connect Alice and Bob.
	net.EnsureConnected(t.t, alice, bob)

	// Alice stimmy.
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, alice)

	// Bob stimmy.
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, bob)

	// Assert that Bob has the correct sync type before proceeding.
	if pinned {
		assertSyncType(t, alice, bobPubkey, lnrpc.Peer_PINNED_SYNC)
	} else {
		assertSyncType(t, alice, bobPubkey, lnrpc.Peer_ACTIVE_SYNC)
	}

	// Regardless of syncer type, ensure that both peers report having
	// completed their initial sync before continuing to make a channel.
	waitForGraphSync(t, alice)

	// Let Alice subscribe to graph notifications.
	graphSub := subscribeGraphNotifications(ctxb, t, alice)
	defer close(graphSub.quit)

	// Open a new channel between Alice and Bob.
	chanPoint := openChannelAndAssert(
		t, net, alice, bob,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// The channel opening above should have triggered a few notifications
	// sent to the notification client. We'll expect two channel updates,
	// and two node announcements.
	var numChannelUpds int
	var numNodeAnns int
	for numChannelUpds < 2 && numNodeAnns < 2 {
		select {
		// Ensure that a new update for both created edges is properly
		// dispatched to our registered client.
		case graphUpdate := <-graphSub.updateChan:
			// Process all channel updates presented in this update
			// message.
			for _, chanUpdate := range graphUpdate.ChannelUpdates {
				switch chanUpdate.AdvertisingNode {
				case alice.PubKeyStr:
				case bob.PubKeyStr:
				default:
					t.Fatalf("unknown advertising node: %v",
						chanUpdate.AdvertisingNode)
				}
				switch chanUpdate.ConnectingNode {
				case alice.PubKeyStr:
				case bob.PubKeyStr:
				default:
					t.Fatalf("unknown connecting node: %v",
						chanUpdate.ConnectingNode)
				}

				if chanUpdate.Capacity != int64(chanAmt) {
					t.Fatalf("channel capacities mismatch:"+
						" expected %v, got %v", chanAmt,
						btcutil.Amount(chanUpdate.Capacity))
				}
				numChannelUpds++
			}

			for _, nodeUpdate := range graphUpdate.NodeUpdates {
				switch nodeUpdate.IdentityKey {
				case alice.PubKeyStr:
				case bob.PubKeyStr:
				default:
					t.Fatalf("unknown node: %v",
						nodeUpdate.IdentityKey)
				}
				numNodeAnns++
			}
		case err := <-graphSub.errChan:
			t.Fatalf("unable to recv graph update: %v", err)
		case <-time.After(time.Second * 10):
			t.Fatalf("timeout waiting for graph notifications, "+
				"only received %d/2 chanupds and %d/2 nodeanns",
				numChannelUpds, numNodeAnns)
		}
	}

	_, blockHeight, err := net.Miner.Client.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	// Now we'll test that updates are properly sent after channels are closed
	// within the network.
	closeChannelAndAssert(t, net, alice, chanPoint, false)

	// Now that the channel has been closed, we should receive a
	// notification indicating so.
out:
	for {
		select {
		case graphUpdate := <-graphSub.updateChan:
			if len(graphUpdate.ClosedChans) != 1 {
				continue
			}

			closedChan := graphUpdate.ClosedChans[0]
			if closedChan.ClosedHeight != uint32(blockHeight+1) {
				t.Fatalf("close heights of channel mismatch: "+
					"expected %v, got %v", blockHeight+1,
					closedChan.ClosedHeight)
			}
			chanPointTxid, err := lnrpc.GetChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			closedChanTxid, err := lnrpc.GetChanPointFundingTxid(
				closedChan.ChanPoint,
			)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			if !bytes.Equal(closedChanTxid[:], chanPointTxid[:]) {
				t.Fatalf("channel point hash mismatch: "+
					"expected %v, got %v", chanPointTxid,
					closedChanTxid)
			}
			if closedChan.ChanPoint.OutputIndex != chanPoint.OutputIndex {
				t.Fatalf("output index mismatch: expected %v, "+
					"got %v", chanPoint.OutputIndex,
					closedChan.ChanPoint)
			}

			break out

		case err := <-graphSub.errChan:
			t.Fatalf("unable to recv graph update: %v", err)
		case <-time.After(time.Second * 10):
			t.Fatalf("notification for channel closure not " +
				"sent")
		}
	}

	// For the final portion of the test, we'll ensure that once a new node
	// appears in the network, the proper notification is dispatched. Note
	// that a node that does not have any channels open is ignored, so first
	// we disconnect Alice and Bob, open a channel between Bob and Carol,
	// and finally connect Alice to Bob again.
	if err := net.DisconnectNodes(alice, bob); err != nil {
		t.Fatalf("unable to disconnect alice and bob: %v", err)
	}
	carol := net.NewNode(t.t, "Carol", nil)
	defer shutdownAndAssert(net, t, carol)

	net.ConnectNodes(t.t, bob, carol)
	chanPoint = openChannelAndAssert(
		t, net, bob, carol,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Reconnect Alice and Bob. This should result in the nodes syncing up
	// their respective graph state, with the new addition being the
	// existence of Carol in the graph, and also the channel between Bob
	// and Carol. Note that we will also receive a node announcement from
	// Bob, since a node will update its node announcement after a new
	// channel is opened.
	net.EnsureConnected(t.t, alice, bob)

	// We should receive an update advertising the newly connected node,
	// Bob's new node announcement, and the channel between Bob and Carol.
	numNodeAnns = 0
	numChannelUpds = 0
	for numChannelUpds < 2 && numNodeAnns < 1 {
		select {
		case graphUpdate := <-graphSub.updateChan:
			for _, nodeUpdate := range graphUpdate.NodeUpdates {
				switch nodeUpdate.IdentityKey {
				case carol.PubKeyStr:
				case bob.PubKeyStr:
				default:
					t.Fatalf("unknown node update pubey: %v",
						nodeUpdate.IdentityKey)
				}
				numNodeAnns++
			}

			for _, chanUpdate := range graphUpdate.ChannelUpdates {
				switch chanUpdate.AdvertisingNode {
				case carol.PubKeyStr:
				case bob.PubKeyStr:
				default:
					t.Fatalf("unknown advertising node: %v",
						chanUpdate.AdvertisingNode)
				}
				switch chanUpdate.ConnectingNode {
				case carol.PubKeyStr:
				case bob.PubKeyStr:
				default:
					t.Fatalf("unknown connecting node: %v",
						chanUpdate.ConnectingNode)
				}

				if chanUpdate.Capacity != int64(chanAmt) {
					t.Fatalf("channel capacities mismatch:"+
						" expected %v, got %v", chanAmt,
						btcutil.Amount(chanUpdate.Capacity))
				}
				numChannelUpds++
			}
		case err := <-graphSub.errChan:
			t.Fatalf("unable to recv graph update: %v", err)
		case <-time.After(time.Second * 10):
			t.Fatalf("timeout waiting for graph notifications, "+
				"only received %d/2 chanupds and %d/2 nodeanns",
				numChannelUpds, numNodeAnns)
		}
	}

	// Close the channel between Bob and Carol.
	closeChannelAndAssert(t, net, bob, chanPoint, false)
}

// testNodeAnnouncement ensures that when a node is started with one or more
// external IP addresses specified on the command line, that those addresses
// announced to the network and reported in the network graph.
func testNodeAnnouncement(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	aliceSub := subscribeGraphNotifications(ctxb, t, net.Alice)
	defer close(aliceSub.quit)

	advertisedAddrs := []string{
		"192.168.1.1:8333",
		"[2001:db8:85a3:8d3:1319:8a2e:370:7348]:8337",
		"bkb6azqggsaiskzi.onion:9735",
		"fomvuglh6h6vcag73xo5t5gv56ombih3zr2xvplkpbfd7wrog4swjwid.onion:1234",
	}

	var lndArgs []string
	for _, addr := range advertisedAddrs {
		lndArgs = append(lndArgs, "--externalip="+addr)
	}

	dave := net.NewNode(t.t, "Dave", lndArgs)
	defer shutdownAndAssert(net, t, dave)

	// We must let Dave have an open channel before he can send a node
	// announcement, so we open a channel with Bob,
	net.ConnectNodes(t.t, net.Bob, dave)

	// Alice shouldn't receive any new updates yet since the channel has yet
	// to be opened.
	select {
	case <-aliceSub.updateChan:
		t.Fatalf("received unexpected update from dave")
	case <-time.After(time.Second):
	}

	// We'll then go ahead and open a channel between Bob and Dave. This
	// ensures that Alice receives the node announcement from Bob as part of
	// the announcement broadcast.
	chanPoint := openChannelAndAssert(
		t, net, net.Bob, dave,
		lntest.OpenChannelParams{
			Amt: 1000000,
		},
	)

	assertAddrs := func(addrsFound []string, targetAddrs ...string) {
		addrs := make(map[string]struct{}, len(addrsFound))
		for _, addr := range addrsFound {
			addrs[addr] = struct{}{}
		}

		for _, addr := range targetAddrs {
			if _, ok := addrs[addr]; !ok {
				t.Fatalf("address %v not found in node "+
					"announcement", addr)
			}
		}
	}

	waitForAddrsInUpdate := func(graphSub graphSubscription,
		nodePubKey string, targetAddrs ...string) {

		for {
			select {
			case graphUpdate := <-graphSub.updateChan:
				for _, update := range graphUpdate.NodeUpdates {
					if update.IdentityKey == nodePubKey {
						assertAddrs(
							update.Addresses, // nolint:staticcheck
							targetAddrs...,
						)
						return
					}
				}
			case err := <-graphSub.errChan:
				t.Fatalf("unable to recv graph update: %v", err)
			case <-time.After(defaultTimeout):
				t.Fatalf("did not receive node ann update")
			}
		}
	}

	// We'll then wait for Alice to receive Dave's node announcement
	// including the expected advertised addresses from Bob since they
	// should already be connected.
	waitForAddrsInUpdate(
		aliceSub, dave.PubKeyStr, advertisedAddrs...,
	)

	// Close the channel between Bob and Dave.
	closeChannelAndAssert(t, net, net.Bob, chanPoint, false)
}

// graphSubscription houses the proxied update and error chans for a node's
// graph subscriptions.
type graphSubscription struct {
	updateChan chan *lnrpc.GraphTopologyUpdate
	errChan    chan error
	quit       chan struct{}
}

// subscribeGraphNotifications subscribes to channel graph updates and launches
// a goroutine that forwards these to the returned channel.
func subscribeGraphNotifications(ctxb context.Context, t *harnessTest,
	node *lntest.HarnessNode) graphSubscription {

	// We'll first start by establishing a notification client which will
	// send us notifications upon detected changes in the channel graph.
	req := &lnrpc.GraphTopologySubscription{}
	ctx, cancelFunc := context.WithCancel(ctxb)
	topologyClient, err := node.SubscribeChannelGraph(ctx, req)
	require.NoError(t.t, err, "unable to create topology client")

	// We'll launch a goroutine that will be responsible for proxying all
	// notifications recv'd from the client into the channel below.
	errChan := make(chan error, 1)
	quit := make(chan struct{})
	graphUpdates := make(chan *lnrpc.GraphTopologyUpdate, 20)
	go func() {
		for {
			defer cancelFunc()

			select {
			case <-quit:
				return
			default:
				graphUpdate, err := topologyClient.Recv()
				select {
				case <-quit:
					return
				default:
				}

				if err == io.EOF {
					return
				} else if err != nil {
					select {
					case errChan <- err:
					case <-quit:
					}
					return
				}

				select {
				case graphUpdates <- graphUpdate:
				case <-quit:
					return
				}
			}
		}
	}()

	return graphSubscription{
		updateChan: graphUpdates,
		errChan:    errChan,
		quit:       quit,
	}
}

// waitForNodeAnnUpdates monitors the nodeAnnUpdates until we get one for
// the expected node and asserts that has the expected information.
func waitForNodeAnnUpdates(graphSub graphSubscription, nodePubKey string,
	expectedUpdate *lnrpc.NodeUpdate, t *harnessTest) {

	for {
		select {
		case graphUpdate := <-graphSub.updateChan:
			for _, update := range graphUpdate.NodeUpdates {
				if update.IdentityKey == nodePubKey {
					assertNodeAnnouncement(
						t, update, expectedUpdate,
					)
					return
				}
			}
		case err := <-graphSub.errChan:
			t.Fatalf("unable to recv graph update: %v", err)
		case <-time.After(defaultTimeout):
			t.Fatalf("did not receive node ann update")
		}
	}
}

// testUpdateNodeAnnouncement ensures that the RPC endpoint validates
// the requests correctly and that the new node announcement is brodcasted
// with the right information after updating our node.
func testUpdateNodeAnnouncement(net *lntest.NetworkHarness, t *harnessTest) {
	// context timeout for the whole test.
	ctxt, cancel := context.WithTimeout(
		context.Background(), defaultTimeout,
	)
	defer cancel()

	// Launch notification clients for alice, such that we can
	// get notified when there are updates in the graph.
	aliceSub := subscribeGraphNotifications(ctxt, t, net.Alice)
	defer close(aliceSub.quit)

	var lndArgs []string

	// Add some exta addresses to the default ones.
	extraAddrs := []string{
		"192.168.1.1:8333",
		"[2001:db8:85a3:8d3:1319:8a2e:370:7348]:8337",
		"bkb6azqggsaiskzi.onion:9735",
		"fomvuglh6h6vcag73xo5t5gv56ombih3zr2xvplkpbfd7wrog4swjwid.onion:1234",
	}
	for _, addr := range extraAddrs {
		lndArgs = append(lndArgs, "--externalip="+addr)
	}
	dave := net.NewNode(t.t, "Dave", lndArgs)
	defer shutdownAndAssert(net, t, dave)

	// Get dave default information so we can compare
	// it lately with the brodcasted updates.
	nodeInfoReq := &lnrpc.GetInfoRequest{}
	resp, err := dave.GetInfo(ctxt, nodeInfoReq)
	require.NoError(t.t, err, "unable to get dave's information")

	defaultAddrs := make([]*lnrpc.NodeAddress, 0, len(resp.Uris))
	for _, uri := range resp.GetUris() {
		values := strings.Split(uri, "@")
		defaultAddrs = append(
			defaultAddrs, &lnrpc.NodeAddress{
				Addr:    values[1],
				Network: "tcp",
			},
		)
	}

	// This feature bit is used to test that our endpoint sets/unsets
	// feature bits properly. If the current FeatureBit is set by default
	// update this one for another one unset by default at random.
	featureBit := lnrpc.FeatureBit_WUMBO_CHANNELS_REQ
	featureIdx := uint32(featureBit)
	if _, ok := resp.Features[featureIdx]; ok {
		t.Fatalf("unexpected feature bit enabled by default")
	}

	defaultDaveNodeAnn := &lnrpc.NodeUpdate{
		Alias:         resp.Alias,
		Color:         resp.Color,
		NodeAddresses: defaultAddrs,
	}

	// Dave must have an open channel before he can send a node
	// announcement, so we open a channel with Bob.
	net.ConnectNodes(t.t, net.Bob, dave)

	// Go ahead and open a channel between Bob and Dave. This
	// ensures that Alice receives the node announcement from Bob as part of
	// the announcement broadcast.
	chanPoint := openChannelAndAssert(
		t, net, net.Bob, dave,
		lntest.OpenChannelParams{
			Amt: 1000000,
		},
	)
	require.NoError(t.t, err, "unexpected error opening a channel")

	// Wait for Alice to receive dave's node announcement with the default
	// values.
	waitForNodeAnnUpdates(
		aliceSub, dave.PubKeyStr, defaultDaveNodeAnn, t,
	)

	// We cannot differentiate between requests with Alias = "" and requests
	// that do not provide that field. If a user sets Alias = "" in the request
	// the field will simply be ignored. The request must fail because no
	// modifiers are applied.
	invalidNodeAnnReq := &peersrpc.NodeAnnouncementUpdateRequest{
		Alias: "",
	}

	_, err = dave.UpdateNodeAnnouncement(ctxt, invalidNodeAnnReq)
	require.Error(t.t, err, "requests without modifiers should field")

	// Alias too long.
	invalidNodeAnnReq = &peersrpc.NodeAnnouncementUpdateRequest{
		Alias: strings.Repeat("a", 50),
	}

	_, err = dave.UpdateNodeAnnouncement(ctxt, invalidNodeAnnReq)
	require.Error(t.t, err, "failed to validate an invalid alias for an "+
		"update node announcement request")

	// Update Node.
	newAlias := "new-alias"
	newColor := "#2288ee"

	newAddresses := []string{
		"192.168.1.10:8333",
		"192.168.1.11:8333",
	}

	updateAddressActions := []*peersrpc.UpdateAddressAction{
		{
			Action:  peersrpc.UpdateAction_ADD,
			Address: newAddresses[0],
		},
		{
			Action:  peersrpc.UpdateAction_ADD,
			Address: newAddresses[1],
		},
		{
			Action:  peersrpc.UpdateAction_REMOVE,
			Address: defaultAddrs[0].Addr,
		},
	}

	updateFeatureActions := []*peersrpc.UpdateFeatureAction{
		{
			Action:     peersrpc.UpdateAction_ADD,
			FeatureBit: featureBit,
		},
	}

	nodeAnnReq := &peersrpc.NodeAnnouncementUpdateRequest{
		Alias:          newAlias,
		Color:          newColor,
		AddressUpdates: updateAddressActions,
		FeatureUpdates: updateFeatureActions,
	}

	response, err := dave.UpdateNodeAnnouncement(ctxt, nodeAnnReq)
	require.NoError(t.t, err, "unable to update dave's node announcement")

	expectedOps := map[string]int{
		"features":  1,
		"color":     1,
		"alias":     1,
		"addresses": 3,
	}
	assertUpdateNodeAnnouncementResponse(t, response, expectedOps)

	newNodeAddresses := []*lnrpc.NodeAddress{}
	// We removed the first address.
	newNodeAddresses = append(newNodeAddresses, defaultAddrs[1:]...)
	newNodeAddresses = append(
		newNodeAddresses,
		&lnrpc.NodeAddress{Addr: newAddresses[0], Network: "tcp"},
		&lnrpc.NodeAddress{Addr: newAddresses[1], Network: "tcp"},
	)

	// After updating the node we expect the update to contain
	// the requested color, requested alias and the new added addresses.
	newDaveNodeAnn := &lnrpc.NodeUpdate{
		Alias:         newAlias,
		Color:         newColor,
		NodeAddresses: newNodeAddresses,
	}

	// We'll then wait for Alice to receive dave's node announcement
	// with the new values.
	waitForNodeAnnUpdates(
		aliceSub, dave.PubKeyStr, newDaveNodeAnn, t,
	)

	// Check that the feature bit was set correctly.
	resp, err = dave.GetInfo(ctxt, nodeInfoReq)
	require.NoError(t.t, err, "unable to get dave's information")

	if _, ok := resp.Features[featureIdx]; !ok {
		t.Fatalf("failed to set feature bit")
	}

	// Check that we cannot set a feature bit that is already set.
	nodeAnnReq = &peersrpc.NodeAnnouncementUpdateRequest{
		FeatureUpdates: updateFeatureActions,
	}

	_, err = dave.UpdateNodeAnnouncement(ctxt, nodeAnnReq)
	require.Error(
		t.t, err, "missing expected error: cannot set a feature bit "+
			"that is already set",
	)

	// Check that we can unset feature bits.
	updateFeatureActions = []*peersrpc.UpdateFeatureAction{
		{
			Action:     peersrpc.UpdateAction_REMOVE,
			FeatureBit: featureBit,
		},
	}

	nodeAnnReq = &peersrpc.NodeAnnouncementUpdateRequest{
		FeatureUpdates: updateFeatureActions,
	}

	response, err = dave.UpdateNodeAnnouncement(ctxt, nodeAnnReq)
	require.NoError(t.t, err, "unable to update dave's node announcement")

	expectedOps = map[string]int{
		"features": 1,
	}
	assertUpdateNodeAnnouncementResponse(t, response, expectedOps)

	resp, err = dave.GetInfo(ctxt, nodeInfoReq)
	require.NoError(t.t, err, "unable to get dave's information")

	if _, ok := resp.Features[featureIdx]; ok {
		t.Fatalf("failed to unset feature bit")
	}

	// Check that we cannot unset a feature bit that is already unset.
	nodeAnnReq = &peersrpc.NodeAnnouncementUpdateRequest{
		FeatureUpdates: updateFeatureActions,
	}

	_, err = dave.UpdateNodeAnnouncement(ctxt, nodeAnnReq)
	require.Error(
		t.t, err, "missing expected error: cannot unset a feature bit "+
			"that is already unset",
	)

	// Close the channel between Bob and Dave.
	closeChannelAndAssert(t, net, net.Bob, chanPoint, false)
}
