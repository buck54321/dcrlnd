// +build rpctest

package itest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrutil/v4"
	dcrutilv3 "github.com/decred/dcrd/dcrutil/v4"
	jsonrpctypes "github.com/decred/dcrd/rpc/jsonrpc/types/v3"
	"github.com/decred/dcrd/rpcclient/v7"
	"github.com/decred/dcrd/rpctest"
	"github.com/decred/dcrd/wire"
	"github.com/decred/dcrlnd"
	"github.com/decred/dcrlnd/channeldb"
	"github.com/decred/dcrlnd/input"
	"github.com/decred/dcrlnd/internal/testutils"
	"github.com/decred/dcrlnd/lncfg"
	"github.com/decred/dcrlnd/lnrpc"
	"github.com/decred/dcrlnd/lnrpc/invoicesrpc"
	"github.com/decred/dcrlnd/lnrpc/routerrpc"
	"github.com/decred/dcrlnd/lnrpc/signrpc"
	"github.com/decred/dcrlnd/lnrpc/walletrpc"
	"github.com/decred/dcrlnd/lnrpc/watchtowerrpc"
	"github.com/decred/dcrlnd/lnrpc/wtclientrpc"
	"github.com/decred/dcrlnd/lntest"
	"github.com/decred/dcrlnd/lntest/wait"
	"github.com/decred/dcrlnd/lntypes"
	"github.com/decred/dcrlnd/lnwallet"
	"github.com/decred/dcrlnd/lnwallet/chainfee"
	"github.com/decred/dcrlnd/lnwire"
	"github.com/decred/dcrlnd/routing"
	"github.com/go-errors/errors"
	"github.com/stretchr/testify/require"
)

var (
	harnessNetParams = chaincfg.SimNetParams()
)

const (
	testFeeBase         = 1e+6
	defaultCSV          = lntest.DefaultCSV
	defaultTimeout      = lntest.DefaultTimeout
	minerMempoolTimeout = lntest.MinerMempoolTimeout
	channelOpenTimeout  = lntest.ChannelOpenTimeout * 4
	channelCloseTimeout = lntest.ChannelCloseTimeout

	// defaultChanAmt is the default channel capacity for channels opened
	// for testing. This is an amount that should allow a large number of
	// channels to be opened among the default test nodes (Alice and Bob)
	// of the lntest harness, assuming the harness initializes those nodes
	// with 10 outputs of 1 DCR each.
	defaultChanAmt = dcrutil.Amount(1<<24) - 1

	itestLndBinary = "../../dcrlnd-itest"

	// anchorSize is the value of an anchor output. This MUST match what
	// lnwallet uses.
	anchorSize       = 12060
	noFeeLimitMAtoms = math.MaxInt64
)

// harnessTest wraps a regular testing.T providing enhanced error detection
// and propagation. All error will be augmented with a full stack-trace in
// order to aid in debugging. Additionally, any panics caused by active
// test cases will also be handled and represented as fatals.
type harnessTest struct {
	t *testing.T

	// testCase is populated during test execution and represents the
	// current test case.
	testCase *testCase

	// lndHarness is a reference to the current network harness. Will be
	// nil if not yet set up.
	lndHarness *lntest.NetworkHarness
}

// newHarnessTest creates a new instance of a harnessTest from a regular
// testing.T instance.
func newHarnessTest(t *testing.T, net *lntest.NetworkHarness) *harnessTest {
	return &harnessTest{t, nil, net}
}

// Skipf calls the underlying testing.T's Skip method, causing the current test
// to be skipped.
func (h *harnessTest) Skipf(format string, args ...interface{}) {
	h.t.Skipf(format, args...)
}

// Fatalf causes the current active test case to fail with a fatal error. All
// integration tests should mark test failures solely with this method due to
// the error stack traces it produces.
func (h *harnessTest) Fatalf(format string, a ...interface{}) {
	if h.lndHarness != nil {
		h.lndHarness.SaveProfilesPages()
	}

	stacktrace := errors.Wrap(fmt.Sprintf(format, a...), 1).ErrorStack()

	if h.testCase != nil {
		h.t.Fatalf("Failed: (%v): exited with error: \n"+
			"%v", h.testCase.name, stacktrace)
	} else {
		h.t.Fatalf("Error outside of test: %v", stacktrace)
	}
}

// RunTestCase executes a harness test case. Any errors or panics will be
// represented as fatal.
func (h *harnessTest) RunTestCase(testCase *testCase) {
	h.testCase = testCase
	defer func() {
		h.testCase = nil
	}()

	defer func() {
		if err := recover(); err != nil {
			description := errors.Wrap(err, 2).ErrorStack()
			h.t.Fatalf("Failed: (%v) panicked with: \n%v",
				h.testCase.name, description)
		}
	}()

	testCase.test(h.lndHarness, h)
	assertCleanState(h, h.lndHarness)
}

func (h *harnessTest) Logf(format string, args ...interface{}) {
	h.t.Logf(format, args...)
}

func (h *harnessTest) Log(args ...interface{}) {
	h.t.Log(args...)
}

// assertCleanState ensures the state of the main test nodes and the mempool
// are in a clean state (no open channels, no txs in the mempool, etc).
func assertCleanState(h *harnessTest, net *lntest.NetworkHarness) {
	_, minerHeight, err := net.Miner.Node.GetBestBlock(context.TODO())
	if err != nil {
		h.Fatalf("unable to get best height: %v", err)
	}
	assertNodeBlockHeight(h, net.Alice, int32(minerHeight))
	assertNodeBlockHeight(h, net.Bob, int32(minerHeight))
	assertNodeNumChannels(h, net.Alice, 0)
	assertNumPendingChannels(h, net.Alice, 0, 0, 0, 0)
	assertNodeNumChannels(h, net.Bob, 0)
	assertNumPendingChannels(h, net.Bob, 0, 0, 0, 0)
	assertNumUnminedUnspent(h, net.Alice, 0)
	assertNumUnminedUnspent(h, net.Bob, 0)
	waitForNTxsInMempool(net.Miner.Node, 0, minerMempoolTimeout)
}

func assertNodeBlockHeight(t *harnessTest, node *lntest.HarnessNode, height int32) {
	t.t.Helper()

	err := wait.NoError(func() error {
		ctxt, cancel := context.WithTimeout(context.Background(), defaultTimeout)
		defer cancel()
		getInfoReq := &lnrpc.GetInfoRequest{}
		getInfoResp, err := node.GetInfo(ctxt, getInfoReq)
		if err != nil {
			return err
		}
		if int32(getInfoResp.BlockHeight) != height {
			return fmt.Errorf("unexpected block height for node %s: "+
				"want=%d got=%d", node.Name(),
				height, getInfoResp.BlockHeight)
		}

		return nil
	}, defaultTimeout)
	if err != nil {
		t.Fatalf("failed to assert node block height: %v", err)
	}
}

func assertTxInBlock(t *harnessTest, block *wire.MsgBlock, txid *chainhash.Hash) {
	for _, tx := range block.Transactions {
		sha := tx.TxHash()
		if bytes.Equal(txid[:], sha[:]) {
			return
		}
	}

	t.Fatalf("tx %s was not included in block", txid)
}

func assertWalletUnspent(t *harnessTest, node *lntest.HarnessNode, out *lnrpc.OutPoint) {
	t.t.Helper()

	err := wait.NoError(func() error {
		ctxt, cancel := context.WithTimeout(context.Background(), defaultTimeout)
		defer cancel()
		unspent, err := node.ListUnspent(ctxt, &lnrpc.ListUnspentRequest{})
		if err != nil {
			return err
		}

		err = errors.New("tx with wanted txhash never found")
		for _, utxo := range unspent.Utxos {
			if !bytes.Equal(utxo.Outpoint.TxidBytes, out.TxidBytes) {
				continue
			}

			err = errors.New("wanted output is not a wallet utxo")
			if utxo.Outpoint.OutputIndex != out.OutputIndex {
				continue
			}

			return nil
		}

		t.Logf("nak %v", err)

		return err
	}, defaultTimeout)
	if err != nil {
		t.Fatalf("outpoint %s not unspent by %s's wallet", out, node.Name())
	}
}

func rpcPointToWirePoint(t *harnessTest, chanPoint *lnrpc.ChannelPoint) wire.OutPoint {
	txid, err := dcrlnd.GetChanPointFundingTxid(chanPoint)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}

	return wire.OutPoint{
		Hash:  *txid,
		Index: chanPoint.OutputIndex,
	}
}

// mineBlocks mine 'num' of blocks and check that blocks are present in
// node blockchain. numTxs should be set to the number of transactions
// (excluding the coinbase) we expect to be included in the first mined block.
func mineBlocks(t *harnessTest, net *lntest.NetworkHarness,
	num uint32, numTxs int) []*wire.MsgBlock {

	// If we expect transactions to be included in the blocks we'll mine,
	// we wait here until they are seen in the miner's mempool.
	var txids []*chainhash.Hash
	var err error
	if numTxs > 0 {
		txids, err = waitForNTxsInMempool(
			net.Miner.Node, numTxs, minerMempoolTimeout,
		)
		if err != nil {
			t.Fatalf("unable to find txns in mempool: %v", err)
		}
	}

	blocks := make([]*wire.MsgBlock, num)

	blockHashes, err := net.Generate(num)
	if err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	for i, blockHash := range blockHashes {
		block, err := net.Miner.Node.GetBlock(context.Background(), blockHash)
		if err != nil {
			t.Fatalf("unable to get block: %v", err)
		}

		blocks[i] = block
	}

	// Finally, assert that all the transactions were included in the first
	// block.
	for _, txid := range txids {
		assertTxInBlock(t, blocks[0], txid)
	}

	return blocks
}

// openChannelStream blocks until an OpenChannel request for a channel funding
// by alice succeeds. If it does, a stream client is returned to receive events
// about the opening channel.
func openChannelStream(ctx context.Context, t *harnessTest,
	net *lntest.NetworkHarness, alice, bob *lntest.HarnessNode,
	p lntest.OpenChannelParams) lnrpc.Lightning_OpenChannelClient {

	t.t.Helper()

	// Wait until we are able to fund a channel successfully. This wait
	// prevents us from erroring out when trying to create a channel while
	// the node is starting up.
	var chanOpenUpdate lnrpc.Lightning_OpenChannelClient
	err := wait.NoError(func() error {
		var err error
		chanOpenUpdate, err = net.OpenChannel(ctx, alice, bob, p)
		return err
	}, defaultTimeout)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	return chanOpenUpdate
}

// openChannelAndAssert attempts to open a channel with the specified
// parameters extended from Alice to Bob. Additionally, two items are asserted
// after the channel is considered open: the funding transaction should be
// found within a block, and that Alice can report the status of the new
// channel.
func openChannelAndAssert(ctx context.Context, t *harnessTest,
	net *lntest.NetworkHarness, alice, bob *lntest.HarnessNode,
	p lntest.OpenChannelParams) *lnrpc.ChannelPoint {

	t.t.Helper()

	chanOpenUpdate := openChannelStream(ctx, t, net, alice, bob, p)

	// Mine 6 blocks, then wait for Alice's node to notify us that the
	// channel has been opened. The funding transaction should be found
	// within the first newly mined block. We mine 6 blocks so that in the
	// case that the channel is public, it is announced to the network.
	block := mineBlocks(t, net, 6, 1)[0]

	fundingChanPoint, err := net.WaitForChannelOpen(ctx, chanOpenUpdate)
	if err != nil {
		t.Fatalf("error while waiting for channel open: %v", err)
	}
	fundingTxID, err := dcrlnd.GetChanPointFundingTxid(fundingChanPoint)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	assertTxInBlock(t, block, fundingTxID)

	// The channel should be listed in the peer information returned by
	// both peers.
	chanPoint := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: fundingChanPoint.OutputIndex,
	}
	if err := net.AssertChannelExists(ctx, alice, &chanPoint); err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}
	if err := net.AssertChannelExists(ctx, bob, &chanPoint); err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}

	return fundingChanPoint
}

// closeChannelAndAssert attempts to close a channel identified by the passed
// channel point owned by the passed Lightning node. A fully blocking channel
// closure is attempted, therefore the passed context should be a child derived
// via timeout from a base parent. Additionally, once the channel has been
// detected as closed, an assertion checks that the transaction is found within
// a block. Finally, this assertion verifies that the node always sends out a
// disable update when closing the channel if the channel was previously enabled.
//
// NOTE: This method assumes that the provided funding point is confirmed
// on-chain AND that the edge exists in the node's channel graph. If the funding
// transactions was reorged out at some point, use closeReorgedChannelAndAssert.
func closeChannelAndAssert(ctx context.Context, t *harnessTest,
	net *lntest.NetworkHarness, node *lntest.HarnessNode,
	fundingChanPoint *lnrpc.ChannelPoint, force bool) *chainhash.Hash {

	return closeChannelAndAssertType(ctx, t, net, node, fundingChanPoint, false, force)
}

func closeChannelAndAssertType(ctx context.Context, t *harnessTest,
	net *lntest.NetworkHarness, node *lntest.HarnessNode,
	fundingChanPoint *lnrpc.ChannelPoint, anchors, force bool) *chainhash.Hash {

	// If this is not a force close, we'll wait a few seconds for the
	// channel to finish settling any outstanding HTLCs that might still be
	// in flight.
	if !force {
		err := waitForPendingHtlcs(node, fundingChanPoint, 0)
		if err != nil {
			t.Fatalf("co-op channel close attempt with active htlcs: %v", err)
		}
	}

	// Fetch the current channel policy. If the channel is currently
	// enabled, we will register for graph notifications before closing to
	// assert that the node sends out a disabling update as a result of the
	// channel being closed.
	curPolicy := getChannelPolicies(t, node, node.PubKeyStr, fundingChanPoint)[0]
	expectDisable := !curPolicy.Disabled

	// If the current channel policy is enabled, begin subscribing the graph
	// updates before initiating the channel closure.
	var graphSub *graphSubscription
	if expectDisable {
		sub := subscribeGraphNotifications(t, ctx, node)
		graphSub = &sub
		defer close(graphSub.quit)
	}

	closeUpdates, _, err := net.CloseChannel(ctx, node, fundingChanPoint, force)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}

	// If the channel policy was enabled prior to the closure, wait until we
	// received the disabled update.
	if expectDisable {
		curPolicy.Disabled = true
		waitForChannelUpdate(
			t, *graphSub,
			[]expectedChanUpdate{
				{node.PubKeyStr, curPolicy, fundingChanPoint},
			},
		)
	}

	return assertChannelClosed(
		ctx, t, net, node, fundingChanPoint, anchors, closeUpdates,
	)
}

// closeReorgedChannelAndAssert attempts to close a channel identified by the
// passed channel point owned by the passed Lightning node. A fully blocking
// channel closure is attempted, therefore the passed context should be a child
// derived via timeout from a base parent. Additionally, once the channel has
// been detected as closed, an assertion checks that the transaction is found
// within a block.
//
// NOTE: This method does not verify that the node sends a disable update for
// the closed channel.
func closeReorgedChannelAndAssert(ctx context.Context, t *harnessTest,
	net *lntest.NetworkHarness, node *lntest.HarnessNode,
	fundingChanPoint *lnrpc.ChannelPoint, force bool) *chainhash.Hash {

	closeUpdates, _, err := net.CloseChannel(ctx, node, fundingChanPoint, force)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}

	return assertChannelClosed(
		ctx, t, net, node, fundingChanPoint, false, closeUpdates,
	)
}

// assertChannelClosed asserts that the channel is properly cleaned up after
// initiating a cooperative or local close.
func assertChannelClosed(ctx context.Context, t *harnessTest,
	net *lntest.NetworkHarness, node *lntest.HarnessNode,
	fundingChanPoint *lnrpc.ChannelPoint, anchors bool,
	closeUpdates lnrpc.Lightning_CloseChannelClient) *chainhash.Hash {

	txid, err := dcrlnd.GetChanPointFundingTxid(fundingChanPoint)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	chanPointStr := fmt.Sprintf("%v:%v", txid, fundingChanPoint.OutputIndex)

	// If the channel appears in list channels, ensure that its state
	// contains ChanStatusCoopBroadcasted.
	ctxt, _ := context.WithTimeout(ctx, defaultTimeout)
	listChansRequest := &lnrpc.ListChannelsRequest{}
	listChansResp, err := node.ListChannels(ctxt, listChansRequest)
	if err != nil {
		t.Fatalf("unable to query for list channels: %v", err)
	}
	for _, channel := range listChansResp.Channels {
		// Skip other channels.
		if channel.ChannelPoint != chanPointStr {
			continue
		}

		// Assert that the channel is in coop broadcasted.
		if !strings.Contains(channel.ChanStatusFlags,
			channeldb.ChanStatusCoopBroadcasted.String()) {
			t.Fatalf("channel not coop broadcasted, "+
				"got: %v", channel.ChanStatusFlags)
		}
	}

	// At this point, the channel should now be marked as being in the
	// state of "waiting close".
	ctxt, _ = context.WithTimeout(ctx, defaultTimeout)
	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	pendingChanResp, err := node.PendingChannels(ctxt, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}
	var found bool
	for _, pendingClose := range pendingChanResp.WaitingCloseChannels {
		if pendingClose.Channel.ChannelPoint == chanPointStr {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("channel not marked as waiting close")
	}

	// We'll now, generate a single block, wait for the final close status
	// update, then ensure that the closing transaction was included in the
	// block. If there are anchors, we also expect an anchor sweep.
	expectedTxes := 1
	if anchors {
		expectedTxes = 2
	}

	block := mineBlocks(t, net, 1, expectedTxes)[0]

	closingTxid, err := net.WaitForChannelClose(ctx, closeUpdates)
	if err != nil {
		t.Fatalf("error while waiting for channel close: %v", err)
	}

	assertTxInBlock(t, block, closingTxid)

	// Finally, the transaction should no longer be in the waiting close
	// state as we've just mined a block that should include the closing
	// transaction.
	err = wait.Predicate(func() bool {
		pendingChansRequest := &lnrpc.PendingChannelsRequest{}
		pendingChanResp, err := node.PendingChannels(
			ctx, pendingChansRequest,
		)
		if err != nil {
			return false
		}

		for _, pendingClose := range pendingChanResp.WaitingCloseChannels {
			if pendingClose.Channel.ChannelPoint == chanPointStr {
				return false
			}
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("closing transaction not marked as fully closed")
	}

	return closingTxid
}

// waitForChannelPendingForceClose waits for the node to report that the
// channel is pending force close, and that the UTXO nursery is aware of it.
func waitForChannelPendingForceClose(ctx context.Context,
	node *lntest.HarnessNode, fundingChanPoint *lnrpc.ChannelPoint) error {

	txid, err := dcrlnd.GetChanPointFundingTxid(fundingChanPoint)
	if err != nil {
		return err
	}

	op := wire.OutPoint{
		Hash:  *txid,
		Index: fundingChanPoint.OutputIndex,
	}

	return wait.NoError(func() error {
		pendingChansRequest := &lnrpc.PendingChannelsRequest{}
		pendingChanResp, err := node.PendingChannels(
			ctx, pendingChansRequest,
		)
		if err != nil {
			return fmt.Errorf("unable to get pending "+
				"channels: %v", err)
		}

		forceClose, err := findForceClosedChannel(pendingChanResp, &op)
		if err != nil {
			return fmt.Errorf("unable to find force-closed "+
				"channel: %v", err)
		}

		// We must wait until the UTXO nursery has received the channel
		// and is aware of its maturity height.
		if forceClose.MaturityHeight == 0 {
			return fmt.Errorf("channel had maturity height of 0")
		}

		return nil
	}, 15*time.Second)
}

// cleanupForceClose mines a force close commitment found in the mempool and
// the following sweep transaction from the force closing node.
func cleanupForceClose(t *harnessTest, net *lntest.NetworkHarness,
	node *lntest.HarnessNode, chanPoint *lnrpc.ChannelPoint) {
	ctxb := context.Background()

	// Wait for the channel to be marked pending force close.
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	err := waitForChannelPendingForceClose(ctxt, node, chanPoint)
	if err != nil {
		t.Fatalf("channel not pending force close: %v", err)
	}

	// Mine enough blocks for the node to sweep its funds from the force
	// closed channel.
	//
	// The commit sweeper resolver is able to broadcast the sweep tx up to
	// one block before the CSV elapses, so wait until defaultCSV-1.
	_, err = net.Generate(defaultCSV - 1)
	if err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// We might find either 1 or 2 sweep txs in the mempool, depending on
	// which nodes were online at the time of the cleanup. If we detect 2
	// txs within three seconds, we can proceed directly. Otherwise we'll
	// wait for one tx and then proceed.
	_, err = waitForNTxsInMempool(
		net.Miner.Node, 2, time.Second*3,
	)
	if err != nil {
		_, err = waitForNTxsInMempool(
			net.Miner.Node, 1, minerMempoolTimeout,
		)
		if err != nil {
			t.Fatalf("unable to find a single sweep tx: %v", err)
		}
	}

	// Mine the sweep tx(s)
	_, err = net.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// Wait until the channel is no longer marked pendingForceClose
	txid, err := dcrlnd.GetChanPointFundingTxid(chanPoint)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	chanPointStr := fmt.Sprintf("%v:%v", txid, chanPoint.OutputIndex)
	err = wait.Predicate(func() bool {
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		pendingChansRequest := &lnrpc.PendingChannelsRequest{}
		pendingChanResp, err := node.PendingChannels(
			ctxt, pendingChansRequest,
		)
		if err != nil {
			return false
		}

		for _, pendingClose := range pendingChanResp.PendingForceClosingChannels {
			if pendingClose.Channel.ChannelPoint == chanPointStr {
				return false
			}
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("force-closed channel still not cleaned up after timeout: %v", err)
	}
}

// waitForPendingHtlcs waits for up to 15 seconds for the given channel in the
// given node to show the specified number of pending HTLCs.
func waitForPendingHtlcs(node *lntest.HarnessNode,
	chanPoint *lnrpc.ChannelPoint, pendingHtlcs int) error {

	fundingTxID, err := chainhash.NewHash(chanPoint.GetFundingTxidBytes())
	if err != nil {
		return fmt.Errorf("unable to convert funding txid into "+
			"chainhash.Hash: %v", err)
	}
	outPoint := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: chanPoint.OutputIndex,
	}
	targetChan := outPoint.String()

	req := &lnrpc.ListChannelsRequest{}
	ctxb := context.Background()

	var predErr error
	wait.Predicate(func() bool {
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		channelInfo, err := node.ListChannels(ctxt, req)
		if err != nil {
			predErr = err
			return false
		}

		for _, channel := range channelInfo.Channels {
			if channel.ChannelPoint != targetChan {
				continue
			}

			foundHtlcs := len(channel.PendingHtlcs)
			if foundHtlcs == pendingHtlcs {
				predErr = nil
				return true
			}

			predErr = fmt.Errorf("found only %d htlcs (wanted %d)",
				foundHtlcs, pendingHtlcs)
			return false
		}

		predErr = fmt.Errorf("could not find channel %s", targetChan)
		return false
	}, time.Second*15)
	return predErr
}

// numOpenChannelsPending sends an RPC request to a node to get a count of the
// node's channels that are currently in a pending state (with a broadcast, but
// not confirmed funding transaction).
func numOpenChannelsPending(ctxt context.Context, node *lntest.HarnessNode) (int, error) {
	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	resp, err := node.PendingChannels(ctxt, pendingChansRequest)
	if err != nil {
		return 0, err
	}
	return len(resp.PendingOpenChannels), nil
}

// assertNumOpenChannelsPending asserts that a pair of nodes have the expected
// number of pending channels between them.
func assertNumOpenChannelsPending(ctxt context.Context, t *harnessTest,
	alice, bob *lntest.HarnessNode, expected int) {

	err := wait.NoError(func() error {
		aliceNumChans, err := numOpenChannelsPending(ctxt, alice)
		if err != nil {
			return fmt.Errorf("error fetching alice's node (%v) "+
				"pending channels %v", alice.NodeID, err)
		}
		bobNumChans, err := numOpenChannelsPending(ctxt, bob)
		if err != nil {
			return fmt.Errorf("error fetching bob's node (%v) "+
				"pending channels %v", bob.NodeID, err)
		}

		aliceStateCorrect := aliceNumChans == expected
		if !aliceStateCorrect {
			return fmt.Errorf("number of pending channels for "+
				"alice incorrect. expected %v, got %v",
				expected, aliceNumChans)
		}

		bobStateCorrect := bobNumChans == expected
		if !bobStateCorrect {
			return fmt.Errorf("number of pending channels for bob "+
				"incorrect. expected %v, got %v", expected,
				bobNumChans)
		}

		return nil
	}, 15*time.Second)
	if err != nil {
		t.Fatalf(err.Error())
	}
}

// assertNumConnections asserts number current connections between two peers.
func assertNumConnections(t *harnessTest, alice, bob *lntest.HarnessNode,
	expected int) {
	ctxb := context.Background()

	const nPolls = 10

	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()

	pollsLeft := nPolls
	for range tick.C {
		pollsLeft--
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		aNumPeers, err := alice.ListPeers(ctxt, &lnrpc.ListPeersRequest{})
		if err != nil {
			t.Fatalf("unable to fetch alice's node (%v) list peers %v",
				alice.NodeID, err)
		}

		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		bNumPeers, err := bob.ListPeers(ctxt, &lnrpc.ListPeersRequest{})
		if err != nil {
			t.Fatalf("unable to fetch bob's node (%v) list peers %v",
				bob.NodeID, err)
		}
		if len(aNumPeers.Peers) != expected {
			// Continue polling if this is not the final
			// loop.
			if pollsLeft > 0 {
				continue
			}
			t.Fatalf("number of peers connected to alice is incorrect: "+
				"expected %v, got %v", expected, len(aNumPeers.Peers))
		}
		if len(bNumPeers.Peers) != expected {
			// Continue polling if this is not the final
			// loop.
			if pollsLeft > 0 {
				continue
			}
			t.Fatalf("number of peers connected to bob is incorrect: "+
				"expected %v, got %v", expected, len(bNumPeers.Peers))
		}

		// Alice and Bob both have the required number of
		// peers, stop polling and return to caller.
		return
	}
}

func assertNumUnminedUnspent(t *harnessTest, node *lntest.HarnessNode, expected int) {
	err := wait.NoError(func() error {
		ctxb := context.Background()
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		utxoReq := &lnrpc.ListUnspentRequest{}
		utxoResp, err := node.ListUnspent(ctxt, utxoReq)
		if err != nil {
			return fmt.Errorf("unable to query utxos: %v", err)
		}

		actual := len(utxoResp.Utxos)
		if actual != expected {
			return fmt.Errorf("node %s has wrong number of unmined utxos ("+
				"expected %d actual %d)", node.Name(), expected, actual)
		}

		return nil

	}, defaultTimeout)
	if err != nil {
		t.Fatalf("failed asserting nb of unmined unspent: %v", err)
	}
}

// shutdownAndAssert shuts down the given node and asserts that no errors
// occur.
func shutdownAndAssert(net *lntest.NetworkHarness, t *harnessTest,
	node *lntest.HarnessNode) {
	if err := net.ShutdownNode(node); err != nil {
		t.Fatalf("unable to shutdown %v: %v", node.Name(), err)
	}
}

// completePaymentRequests sends payments from a lightning node to complete all
// payment requests. If the awaitResponse parameter is true, this function does
// not return until all payments successfully complete without errors.
func completePaymentRequests(ctx context.Context, client lnrpc.LightningClient,
	routerClient routerrpc.RouterClient, paymentRequests []string,
	awaitResponse bool) error {

	// We start by getting the current state of the client's channels. This
	// is needed to ensure the payments actually have been committed before
	// we return.
	ctxt, _ := context.WithTimeout(ctx, defaultTimeout)
	req := &lnrpc.ListChannelsRequest{}
	listResp, err := client.ListChannels(ctxt, req)
	if err != nil {
		return err
	}

	// send sends a payment and returns an error if it doesn't succeeded.
	send := func(payReq string) error {
		ctxc, cancel := context.WithCancel(ctx)
		defer cancel()

		payStream, err := routerClient.SendPaymentV2(
			ctxc,
			&routerrpc.SendPaymentRequest{
				PaymentRequest: payReq,
				TimeoutSeconds: 60,
				FeeLimitMAtoms: noFeeLimitMAtoms,
			},
		)
		if err != nil {
			return err
		}

		resp, err := getPaymentResult(payStream)
		if err != nil {
			return err
		}
		if resp.Status != lnrpc.Payment_SUCCEEDED {
			return errors.New(resp.FailureReason)
		}

		return nil
	}

	// Launch all payments simultaneously.
	results := make(chan error)
	for _, payReq := range paymentRequests {
		payReqCopy := payReq
		go func() {
			err := send(payReqCopy)
			if awaitResponse {
				results <- err
			}
		}()
	}

	// If awaiting a response, verify that all payments succeeded.
	if awaitResponse {
		for range paymentRequests {
			err := <-results
			if err != nil {
				return err
			}
		}
		return nil
	}

	// We are not waiting for feedback in the form of a response, but we
	// should still wait long enough for the server to receive and handle
	// the send before cancelling the request. We wait for the number of
	// updates to one of our channels has increased before we return.
	err = wait.Predicate(func() bool {
		ctxt, _ = context.WithTimeout(ctx, defaultTimeout)
		newListResp, err := client.ListChannels(ctxt, req)
		if err != nil {
			return false
		}

		// If the number of open channels is now lower than before
		// attempting the payments, it means one of the payments
		// triggered a force closure (for example, due to an incorrect
		// preimage). Return early since it's clear the payment was
		// attempted.
		if len(newListResp.Channels) < len(listResp.Channels) {
			return true
		}

		for _, c1 := range listResp.Channels {
			for _, c2 := range newListResp.Channels {
				if c1.ChannelPoint != c2.ChannelPoint {
					continue
				}

				// If this channel has an increased numbr of
				// updates, we assume the payments are
				// committed, and we can return.
				if c2.NumUpdates > c1.NumUpdates {
					return true
				}
			}
		}

		return false
	}, time.Second*15)
	if err != nil {
		return err
	}

	return nil
}

// makeFakePayHash creates random pre image hash
func makeFakePayHash(t *harnessTest) []byte {
	randBuf := make([]byte, 32)

	if _, err := rand.Read(randBuf); err != nil {
		t.Fatalf("internal error, cannot generate random string: %v", err)
	}

	return randBuf
}

// createPayReqs is a helper method that will create a slice of payment
// requests for the given node.
func createPayReqs(node *lntest.HarnessNode, paymentAmt dcrutil.Amount,
	numInvoices int) ([]string, [][]byte, []*lnrpc.Invoice, error) {

	payReqs := make([]string, numInvoices)
	rHashes := make([][]byte, numInvoices)
	invoices := make([]*lnrpc.Invoice, numInvoices)
	for i := 0; i < numInvoices; i++ {
		preimage := make([]byte, 32)
		_, err := rand.Read(preimage)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("unable to generate "+
				"preimage: %v", err)
		}
		invoice := &lnrpc.Invoice{
			Memo:      "testing",
			RPreimage: preimage,
			Value:     int64(paymentAmt),

			// Historically, integration tests assumed this check never happened,
			// so disable by default. There are specific tests for asserting the
			// behavior when this flag is false.
			IgnoreMaxInboundAmt: true,
		}
		ctxt, _ := context.WithTimeout(
			context.Background(), defaultTimeout,
		)
		resp, err := node.AddInvoice(ctxt, invoice)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("unable to add "+
				"invoice: %v", err)
		}

		payReqs[i] = resp.PaymentRequest
		rHashes[i] = resp.RHash
		invoices[i] = invoice
	}
	return payReqs, rHashes, invoices, nil
}

// getChanInfo is a helper method for getting channel info for a node's sole
// channel.
func getChanInfo(ctx context.Context, node *lntest.HarnessNode) (
	*lnrpc.Channel, error) {

	req := &lnrpc.ListChannelsRequest{}
	channelInfo, err := node.ListChannels(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(channelInfo.Channels) != 1 {
		return nil, fmt.Errorf("node should only have a single "+
			"channel, instead it has %v", len(channelInfo.Channels))
	}

	return channelInfo.Channels[0], nil
}

// recordedTxFee returns the tx fee recorded in the transaction itself (that
// is, sum(TxOut[].Value) - sum(TxIn[].ValueIn)). While this is not currently
// enforced by consensus rules and cannot be relied upon for validation
// purposes, it's sufficient for testing purposes, assuming the procedure that
// generated the transaction correctly fills the ValueIn (which should be true
// for transactions produced by dcrlnd).
func recordedTxFee(tx *wire.MsgTx) int64 {
	var amountIn, amountOut int64
	for _, in := range tx.TxIn {
		amountIn += in.ValueIn
	}
	for _, out := range tx.TxOut {
		amountOut += out.Value
	}
	return amountIn - amountOut
}

const (
	AddrTypePubkeyHash = lnrpc.AddressType_PUBKEY_HASH
)

// testGetRecoveryInfo checks whether lnd gives the right information about
// the wallet recovery process.
func testGetRecoveryInfo(net *lntest.NetworkHarness, t *harnessTest) {
	// TODO: reenable after the wallet impls support this.
	t.Skipf("Re-enable after wallet impls support this")

	ctxb := context.Background()

	// First, create a new node with strong passphrase and grab the mnemonic
	// used for key derivation. This will bring up Carol with an empty
	// wallet, and such that she is synced up.
	password := []byte("The Magic Words are Squeamish Ossifrage")
	carol, mnemonic, err := net.NewNodeWithSeed("Carol", nil, password)
	if err != nil {
		t.Fatalf("unable to create node with seed; %v", err)
	}

	shutdownAndAssert(net, t, carol)

	checkInfo := func(expectedRecoveryMode, expectedRecoveryFinished bool,
		expectedProgress float64, recoveryWindow int32) {

		// Restore Carol, passing in the password, mnemonic, and
		// desired recovery window.
		node, err := net.RestoreNodeWithSeed(
			"Carol", nil, password, mnemonic, recoveryWindow, nil,
		)
		if err != nil {
			t.Fatalf("unable to restore node: %v", err)
		}

		// Wait for Carol to sync to the chain.
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		_, minerHeight, err := net.Miner.Node.GetBestBlock(ctxt)
		if err != nil {
			t.Fatalf("unable to get current blockheight %v", err)
		}
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		err = waitForNodeBlockHeight(ctxt, node, minerHeight)
		if err != nil {
			t.Fatalf("unable to sync to chain: %v", err)
		}

		// Query carol for her current wallet recovery progress.
		var (
			recoveryMode     bool
			recoveryFinished bool
			progress         float64
		)

		err = wait.Predicate(func() bool {
			// Verify that recovery info gives the right response.
			req := &lnrpc.GetRecoveryInfoRequest{}
			ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
			resp, err := node.GetRecoveryInfo(ctxt, req)
			if err != nil {
				t.Fatalf("unable to query recovery info: %v", err)
			}

			recoveryMode = resp.RecoveryMode
			recoveryFinished = resp.RecoveryFinished
			progress = resp.Progress

			if recoveryMode != expectedRecoveryMode ||
				recoveryFinished != expectedRecoveryFinished ||
				progress != expectedProgress {
				return false
			}

			return true
		}, 15*time.Second)
		if err != nil {
			t.Fatalf("expected recovery mode to be %v, got %v, "+
				"expected recovery finished to be %v, got %v, "+
				"expected progress %v, got %v",
				expectedRecoveryMode, recoveryMode,
				expectedRecoveryFinished, recoveryFinished,
				expectedProgress, progress,
			)
		}

		// Lastly, shutdown this Carol so we can move on to the next
		// restoration.
		shutdownAndAssert(net, t, node)
	}

	// Restore Carol with a recovery window of 0. Since it's not in recovery
	// mode, the recovery info will give a response with recoveryMode=false,
	// recoveryFinished=false, and progress=0
	checkInfo(false, false, 0, 0)

	// Change the recovery windown to be 1 to turn on recovery mode. Since the
	// current chain height is the same as the birthday height, it should
	// indicate the recovery process is finished.
	checkInfo(true, true, 1, 1)

	// We now go ahead 5 blocks. Because the wallet's syncing process is
	// controlled by a goroutine in the background, it will catch up quickly.
	// This makes the recovery progress back to 1.
	mineBlocks(t, net, 5, 0)
	checkInfo(true, true, 1, 1)
}

// testOnchainFundRecovery checks lnd's ability to rescan for onchain outputs
// when providing a valid aezeed that owns outputs on the chain. This test
// performs multiple restorations using the same seed and various recovery
// windows to ensure we detect funds properly.
func testOnchainFundRecovery(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// First, create a new node with strong passphrase and grab the mnemonic
	// used for key derivation. This will bring up Carol with an empty
	// wallet, and such that she is synced up.
	password := []byte("The Magic Words are Squeamish Ossifrage")
	carol, mnemonic, err := net.NewNodeWithSeed("Carol", nil, password)
	if err != nil {
		t.Fatalf("unable to create node with seed; %v", err)
	}
	shutdownAndAssert(net, t, carol)

	// Create a closure for testing the recovery of Carol's wallet. This
	// method takes the expected value of Carol's balance when using the
	// given recovery window. Additionally, the caller can specify an action
	// to perform on the restored node before the node is shutdown.
	restoreCheckBalance := func(expAmount int64, expectedNumUTXOs uint32,
		recoveryWindow int32, fn func(*lntest.HarnessNode)) {

		// Restore Carol, passing in the password, mnemonic, and
		// desired recovery window.
		node, err := net.RestoreNodeWithSeed(
			"Carol", nil, password, mnemonic, recoveryWindow, nil,
		)
		if err != nil {
			t.Fatalf("unable to restore node: %v", err)
		}

		// Query carol for her current wallet balance, and also that we
		// gain the expected number of UTXOs.
		var (
			currBalance  int64
			currNumUTXOs uint32
		)
		err = wait.Predicate(func() bool {
			req := &lnrpc.WalletBalanceRequest{}
			ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
			resp, err := node.WalletBalance(ctxt, req)
			if err != nil {
				t.Fatalf("unable to query wallet balance: %v",
					err)
			}
			currBalance = resp.ConfirmedBalance

			utxoReq := &lnrpc.ListUnspentRequest{
				MaxConfs: math.MaxInt32,
			}
			ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
			utxoResp, err := node.ListUnspent(ctxt, utxoReq)
			if err != nil {
				t.Fatalf("unable to query utxos: %v", err)
			}
			currNumUTXOs = uint32(len(utxoResp.Utxos))

			// Verify that Carol's balance and number of UTXOs
			// matches what's expected.
			if expAmount != currBalance {
				return false
			}
			if currNumUTXOs != expectedNumUTXOs {
				return false
			}

			return true
		}, 15*time.Second)
		if err != nil {
			t.Fatalf("expected restored node to have %d atoms, "+
				"instead has %d atoms, expected %d utxos "+
				"instead has %d", expAmount, currBalance,
				expectedNumUTXOs, currNumUTXOs)
		}

		// If the user provided a callback, execute the commands against
		// the restored Carol.
		if fn != nil {
			fn(node)
		}

		// Lastly, shutdown this Carol so we can move on to the next
		// restoration.
		shutdownAndAssert(net, t, node)
	}

	// Create a closure-factory for building closures that can generate and
	// skip a configurable number of addresses, before finally sending coins
	// to a next generated address. The returned closure will apply the same
	// behavior to both default P2WKH and NP2WKH scopes.
	skipAndSend := func(nskip int) func(*lntest.HarnessNode) {
		return func(node *lntest.HarnessNode) {
			newP2PKHAddrReq := &lnrpc.NewAddressRequest{
				Type: AddrTypePubkeyHash,
			}

			// Generate and skip the number of addresses requested.
			for i := 0; i < nskip; i++ {
				ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
				_, err = node.NewAddress(ctxt, newP2PKHAddrReq)
				if err != nil {
					t.Fatalf("unable to generate new "+
						"p2pkh address: %v", err)
				}
			}

			// Send one DCR to the next P2PKH address.
			ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
			err = net.SendCoins(
				ctxt, dcrutil.AtomsPerCoin, node,
			)
			if err != nil {
				t.Fatalf("unable to send coins to node: %v",
					err)
			}
		}
	}

	// Restore Carol with a recovery window of 0. Since no coins have been
	// sent, her balance should be zero.
	//
	// After, one DCR is sent to both her first external P2WKH and NP2WKH
	// addresses.
	restoreCheckBalance(0, 0, 0, skipAndSend(0))

	// Check that restoring without a look-ahead results in having no funds
	// in the wallet, even though they exist on-chain.
	restoreCheckBalance(0, 0, 0, nil)

	// Now, check that using a look-ahead of 1 recovers the balance from
	// the two transactions above. We should also now have 2 UTXOs in the
	// wallet at the end of the recovery attempt.
	//
	// After, we will generate and skip 9 P2WKH and NP2WKH addresses, and
	// send another DCR to the subsequent 10th address in each derivation
	// path.
	restoreCheckBalance(2*dcrutil.AtomsPerCoin, 2, 1, skipAndSend(9))

	// Check that using a recovery window of 9 does not find the two most
	// recent txns.
	restoreCheckBalance(2*dcrutil.AtomsPerCoin, 2, 9, nil)

	// Extending our recovery window to 10 should find the most recent
	// transactions, leaving the wallet with 4 BTC total. We should also
	// learn of the two additional UTXOs created above.
	//
	// After, we will skip 19 more addrs, sending to the 20th address past
	// our last found address, and repeat the same checks.
	restoreCheckBalance(4*dcrutil.AtomsPerCoin, 4, 10, skipAndSend(19))

	// Check that recovering with a recovery window of 19 fails to find the
	// most recent transactions.
	restoreCheckBalance(4*dcrutil.AtomsPerCoin, 4, 19, nil)

	// Ensure that using a recovery window of 20 succeeds with all UTXOs
	// found and the final balance reflected.

	// After these checks are done, we'll want to make sure we can also
	// recover change address outputs.  This is mainly motivated by a now
	// fixed bug in the wallet in which change addresses could at times be
	// created outside of the default key scopes. Recovery only used to be
	// performed on the default key scopes, so ideally this test case
	// would've caught the bug earlier. Carol has received 6 BTC so far from
	// the miner, we'll send 5 back to ensure all of her UTXOs get spent to
	// avoid fee discrepancies and a change output is formed.
	const minerAmt = 5 * dcrutil.AtomsPerCoin
	const finalBalance = 6 * dcrutil.AtomsPerCoin
	promptChangeAddr := func(node *lntest.HarnessNode) {
		minerAddr, err := net.Miner.NewAddress()
		if err != nil {
			t.Fatalf("unable to create new miner address: %v", err)
		}
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		resp, err := node.SendCoins(ctxt, &lnrpc.SendCoinsRequest{
			Addr:   minerAddr.String(),
			Amount: minerAmt,
		})
		if err != nil {
			t.Fatalf("unable to send coins to miner: %v", err)
		}
		txid, err := waitForTxInMempool(
			net.Miner.Node, minerMempoolTimeout,
		)
		if err != nil {
			t.Fatalf("transaction not found in mempool: %v", err)
		}
		if resp.Txid != txid.String() {
			t.Fatalf("txid mismatch: %v vs %v", resp.Txid,
				txid.String())
		}
		block := mineBlocks(t, net, 1, 1)[0]
		assertTxInBlock(t, block, txid)
	}
	restoreCheckBalance(finalBalance, 6, 20, promptChangeAddr)

	// We should expect a static fee of 27750 satoshis for spending 6 inputs
	// (3 P2WPKH, 3 NP2WPKH) to two P2WPKH outputs. Carol should therefore
	// only have one UTXO present (the change output) of 6 - 5 - fee BTC.
	const fee = 27750
	restoreCheckBalance(finalBalance-minerAmt-fee, 1, 21, nil)
}

// commitType is a simple enum used to run though the basic funding flow with
// different commitment formats.
type commitType byte

const (
	// commitTypeLegacy is the old school commitment type.
	commitTypeLegacy commitType = iota

	// commiTypeTweakless is the commitment type where the remote key is
	// static (non-tweaked).
	commitTypeTweakless

	// commitTypeAnchors is the kind of commitment that has extra outputs
	// used for anchoring down to commitment using CPFP.
	commitTypeAnchors
)

// String returns that name of the commitment type.
func (c commitType) String() string {
	switch c {
	case commitTypeLegacy:
		return "legacy"
	case commitTypeTweakless:
		return "tweakless"
	case commitTypeAnchors:
		return "anchors"
	default:
		return "invalid"
	}
}

// Args returns the command line flag to supply to enable this commitment type.
func (c commitType) Args() []string {
	switch c {
	case commitTypeLegacy:
		return []string{"--protocol.legacy.committweak"}
	case commitTypeTweakless:
		return []string{}
	case commitTypeAnchors:
		return []string{"--protocol.anchors"}
	}

	return nil
}

// calcStaticFee calculates appropriate fees for commitment transactions.  This
// function provides a simple way to allow test balance assertions to take fee
// calculations into account.
func (c commitType) calcStaticFee(numHTLCs int) dcrutil.Amount {
	const htlcSize = input.HTLCOutputSize
	var (
		feePerKB   = chainfee.AtomPerKByte(1e4)
		commitSize = input.CommitmentTxSize
		anchors    = dcrutil.Amount(0)
	)

	// The anchor commitment type is slightly heavier, and we must also add
	// the value of the two anchors to the resulting fee the initiator
	// pays.
	if c == commitTypeAnchors {
		commitSize = input.CommitmentWithAnchorsTxSize
		anchors = 2 * anchorSize
	}

	return feePerKB.FeeForSize(int64(commitSize+htlcSize*int64(numHTLCs))) +
		anchors
}

// channelCommitType retrieves the active channel commitment type for the given
// chan point.
func channelCommitType(node *lntest.HarnessNode,
	chanPoint *lnrpc.ChannelPoint) (commitType, error) {

	ctxb := context.Background()
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)

	req := &lnrpc.ListChannelsRequest{}
	channels, err := node.ListChannels(ctxt, req)
	if err != nil {
		return 0, fmt.Errorf("listchannels failed: %v", err)
	}

	for _, c := range channels.Channels {
		if c.ChannelPoint == txStr(chanPoint) {
			switch c.CommitmentType {

			// If the anchor output size is non-zero, we are
			// dealing with the anchor type.
			case lnrpc.CommitmentType_ANCHORS:
				return commitTypeAnchors, nil

			// StaticRemoteKey means it is tweakless,
			case lnrpc.CommitmentType_STATIC_REMOTE_KEY:
				return commitTypeTweakless, nil

			// Otherwise legacy.
			default:
				return commitTypeLegacy, nil
			}
		}
	}

	return 0, fmt.Errorf("channel point %v not found", chanPoint)
}

// basicChannelFundingTest is a sub-test of the main testBasicChannelFunding
// test. Given two nodes: Alice and Bob, it'll assert proper channel creation,
// then return a function closure that should be called to assert proper
// channel closure.
func basicChannelFundingTest(t *harnessTest, net *lntest.NetworkHarness,
	alice *lntest.HarnessNode, bob *lntest.HarnessNode,
	fundingShim *lnrpc.FundingShim) (*lnrpc.Channel, *lnrpc.Channel, func(), error) {

	chanAmt := defaultChanAmt
	pushAmt := dcrutil.Amount(100000)

	// First establish a channel with a capacity of 0.5 DCR between Alice
	// and Bob with Alice pushing 100k atoms to Bob's side during
	// funding. This function will block until the channel itself is fully
	// open or an error occurs in the funding process. A series of
	// assertions will be executed to ensure the funding process completed
	// successfully.
	ctxb := context.Background()
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, alice, bob,
		lntest.OpenChannelParams{
			Amt:         chanAmt,
			PushAmt:     pushAmt,
			FundingShim: fundingShim,
		},
	)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err := alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("alice didn't report "+
			"channel: %v", err)
	}
	err = bob.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("bob didn't report "+
			"channel: %v", err)
	}

	cType, err := channelCommitType(alice, chanPoint)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("unable to get channel "+
			"type: %v", err)
	}

	// With the channel open, ensure that the amount specified above has
	// properly been pushed to Bob.
	balReq := &lnrpc.ChannelBalanceRequest{}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	aliceBal, err := alice.ChannelBalance(ctxt, balReq)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("unable to get alice's "+
			"balance: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	bobBal, err := bob.ChannelBalance(ctxt, balReq)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("unable to get bobs's "+
			"balance: %v", err)
	}

	expBalanceAlice := chanAmt - pushAmt - cType.calcStaticFee(0)
	aliceBalance := dcrutil.Amount(aliceBal.Balance)
	if aliceBalance != expBalanceAlice {
		return nil, nil, nil, fmt.Errorf("alice's balance is "+
			"incorrect: expected %v got %v",
			expBalanceAlice, aliceBalance)
	}

	bobBalance := dcrutil.Amount(bobBal.Balance)
	if bobBalance != pushAmt {
		return nil, nil, nil, fmt.Errorf("bob's balance is incorrect: "+
			"expected %v got %v", pushAmt, bobBalance)
	}

	req := &lnrpc.ListChannelsRequest{}
	aliceChannel, err := alice.ListChannels(context.Background(), req)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("unable to obtain chan: %v", err)
	}

	bobChannel, err := bob.ListChannels(context.Background(), req)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("unable to obtain chan: %v", err)
	}

	closeChan := func() {
		// Finally, immediately close the channel. This function will
		// also block until the channel is closed and will additionally
		// assert the relevant channel closing post conditions.
		ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
		closeChannelAndAssert(ctxt, t, net, alice, chanPoint, false)
	}

	return aliceChannel.Channels[0], bobChannel.Channels[0], closeChan, nil
}

// testBasicChannelFunding performs a test exercising expected behavior from a
// basic funding workflow. The test creates a new channel between Alice and
// Bob, then immediately closes the channel after asserting some expected post
// conditions. Finally, the chain itself is checked to ensure the closing
// transaction was mined.
func testBasicChannelFunding(net *lntest.NetworkHarness, t *harnessTest) {

	ctxb := context.Background()

	// Run through the test with combinations of all the different
	// commitment types.
	allTypes := []commitType{
		commitTypeLegacy,
		commitTypeTweakless,
		commitTypeAnchors,
	}

test:
	// We'll test all possible combinations of the feature bit presence
	// that both nodes can signal for this new channel type. We'll make a
	// new Carol+Dave for each test instance as well.
	for _, carolCommitType := range allTypes {
		for _, daveCommitType := range allTypes {
			// Based on the current tweak variable for Carol, we'll
			// preferentially signal the legacy commitment format.
			// We do the same for Dave shortly below.
			carolArgs := carolCommitType.Args()
			carol, err := net.NewNode("Carol", carolArgs)
			if err != nil {
				t.Fatalf("unable to create new node: %v", err)
			}

			// Each time, we'll send Carol a new set of coins in
			// order to fund the channel.
			ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
			err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, carol)
			if err != nil {
				t.Fatalf("unable to send coins to carol: %v", err)
			}

			daveArgs := daveCommitType.Args()
			dave, err := net.NewNode("Dave", daveArgs)
			if err != nil {
				t.Fatalf("unable to create new node: %v", err)
			}

			// Before we start the test, we'll ensure both sides
			// are connected to the funding flow can properly be
			// executed.
			ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
			err = net.EnsureConnected(ctxt, carol, dave)
			if err != nil {
				t.Fatalf("unable to connect peers: %v", err)
			}

			testName := fmt.Sprintf("carol_commit=%v,dave_commit=%v",
				carolCommitType, daveCommitType)

			ht := t
			success := t.t.Run(testName, func(t *testing.T) {
				carolChannel, daveChannel, closeChan, err := basicChannelFundingTest(
					ht, net, carol, dave, nil,
				)
				if err != nil {
					t.Fatalf("failed funding flow: %v", err)
				}

				// Both nodes should report the same commitment
				// type.
				chansCommitType := carolChannel.CommitmentType
				if daveChannel.CommitmentType != chansCommitType {
					t.Fatalf("commit types don't match, "+
						"carol got %v, dave got %v",
						carolChannel.CommitmentType,
						daveChannel.CommitmentType,
					)
				}

				// Now check that the commitment type reported
				// by both nodes is what we expect. It will be
				// the minimum of the two nodes' preference, in
				// the order Legacy, Tweakless, Anchors.
				expType := carolCommitType

				switch daveCommitType {

				// Dave supports anchors, type will be what
				// Carol supports.
				case commitTypeAnchors:

				// Dave only supports tweakless, channel will
				// be downgraded to this type if Carol supports
				// anchors.
				case commitTypeTweakless:
					if expType == commitTypeAnchors {
						expType = commitTypeTweakless
					}

				// Dave only supoprts legacy type, channel will
				// be downgraded to this type.
				case commitTypeLegacy:
					expType = commitTypeLegacy

				default:
					t.Fatalf("invalid commit type %v",
						daveCommitType)
				}

				// Check that the signalled type matches what we
				// expect.
				switch {
				case expType == commitTypeAnchors &&
					chansCommitType == lnrpc.CommitmentType_ANCHORS:

				case expType == commitTypeTweakless &&
					chansCommitType == lnrpc.CommitmentType_STATIC_REMOTE_KEY:

				case expType == commitTypeLegacy &&
					chansCommitType == lnrpc.CommitmentType_LEGACY:

				default:
					t.Fatalf("expected nodes to signal "+
						"commit type %v, instead got "+
						"%v", expType, chansCommitType)
				}

				// As we've concluded this sub-test case we'll
				// now close out the channel for both sides.
				closeChan()
			})
			if !success {
				break test
			}

			shutdownAndAssert(net, t, carol)
			shutdownAndAssert(net, t, dave)
		}
	}
}

// testUnconfirmedChannelFunding tests that our unconfirmed change outputs can
// be used to fund channels.
func testUnconfirmedChannelFunding(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const (
		chanAmt = defaultChanAmt
		pushAmt = dcrutil.Amount(100000)
	)

	// We'll start off by creating a node for Carol.
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create carol's node: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	// We'll send her some confirmed funds.
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, 2*chanAmt, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}

	// Now let Carol send some funds to herself, making a unconfirmed
	// change output.
	addrReq := &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_PUBKEY_HASH,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	resp, err := carol.NewAddress(ctxt, addrReq)
	if err != nil {
		t.Fatalf("unable to get new address: %v", err)
	}

	sendReq := &lnrpc.SendCoinsRequest{
		Addr:   resp.Address,
		Amount: int64(chanAmt) / 5,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = carol.SendCoins(ctxt, sendReq)
	if err != nil {
		t.Fatalf("unable to send coins: %v", err)
	}

	// Make sure the unconfirmed tx is seen in the mempool.
	_, err = waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("failed to find tx in miner mempool: %v", err)
	}

	// Now, we'll connect her to Alice so that they can open a channel
	// together. The funding flow should select Carol's unconfirmed output
	// as she doesn't have any other funds since it's a new node.

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, carol, net.Alice); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}

	chanOpenUpdate := openChannelStream(
		ctxt, t, net, carol, net.Alice,
		lntest.OpenChannelParams{
			Amt:              chanAmt,
			PushAmt:          pushAmt,
			SpendUnconfirmed: true,
		},
	)

	// Confirm the channel and wait for it to be recognized by both
	// parties. Two transactions should be mined, the unconfirmed spend and
	// the funding tx.
	mineBlocks(t, net, 6, 2)
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	chanPoint, err := net.WaitForChannelOpen(ctxt, chanOpenUpdate)
	if err != nil {
		t.Fatalf("error while waiting for channel open: %v", err)
	}

	cType, err := channelCommitType(net.Alice, chanPoint)
	if err != nil {
		t.Fatalf("unable to get channel type: %v", err)
	}

	// With the channel open, we'll check the balances on each side of the
	// channel as a sanity check to ensure things worked out as intended.
	balReq := &lnrpc.ChannelBalanceRequest{}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	carolBal, err := carol.ChannelBalance(ctxt, balReq)
	if err != nil {
		t.Fatalf("unable to get carol's balance: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	aliceBal, err := net.Alice.ChannelBalance(ctxt, balReq)
	if err != nil {
		t.Fatalf("unable to get alice's balance: %v", err)
	}
	if carolBal.Balance != int64(chanAmt-pushAmt-cType.calcStaticFee(0)) {
		t.Fatalf("carol's balance is incorrect: expected %v got %v",
			chanAmt-pushAmt-cType.calcStaticFee(0), carolBal)
	}
	if aliceBal.Balance != int64(pushAmt) {
		t.Fatalf("alice's balance is incorrect: expected %v got %v",
			pushAmt, aliceBal.Balance)
	}

	// Now that we're done with the test, the channel can be closed.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPoint, false)
}

// testConcurrentNodeConnection tests whether we can repeatedly connect and
// disconnect two nodes and that trying to connect "at the same time" will not
// cause problems.
//
// "At the same time" is used in scare quotes, since at the level of abstraction
// used in these tests it's not possible to really enforce that the connection
// attempts are sent in any specific order or parallelism. So we resort to doing
// a number of repeated number of trial runs, with as much concurrency as
// possible.
func testConcurrentNodeConnection(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	aliceToBobReq := &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{
			Pubkey: net.Bob.PubKeyStr,
			Host:   net.Bob.P2PAddr(),
		},
		Perm: false,
	}

	bobToAliceReq := &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{
			Pubkey: net.Alice.PubKeyStr,
			Host:   net.Alice.P2PAddr(),
		},
		Perm: false,
	}

	connect := func(node *lntest.HarnessNode, req *lnrpc.ConnectPeerRequest, wg *sync.WaitGroup) error {
		_, err := node.ConnectPeer(ctxb, req)
		wg.Done()
		return err
	}

	// Initially disconnect Alice and Bob. Several connection attempts will
	// be performed later on. Ignore errors if they are not connected and
	// give some time for the disconnection to clear all resources.
	net.DisconnectNodes(ctxb, net.Alice, net.Bob)
	time.Sleep(50 * time.Millisecond)

	// Perform a number of trial runs in sequence, so we have some reasonable
	// chance actually performing connections "at the same time".
	nbAttempts := 10
	for i := 0; i < nbAttempts; i++ {
		// Sanity check that neither node has a connection.
		assertNumConnections(t, net.Alice, net.Bob, 0)

		logLine := fmt.Sprintf("=== %s: Starting connection iteration %d\n",
			time.Now(), i)
		net.Alice.AddToLog(logLine)
		net.Bob.AddToLog(logLine)

		var aliceReply, bobReply error
		wg := new(sync.WaitGroup)

		// Start two go routines which will try to connect "at the same
		// time".
		wg.Add(2)
		go func() { aliceReply = connect(net.Alice, aliceToBobReq, wg) }()
		go func() { bobReply = connect(net.Bob, bobToAliceReq, wg) }()

		wgWaitChan := make(chan struct{})
		go func() {
			wg.Wait()
			close(wgWaitChan)
		}()

		select {
		case <-wgWaitChan:
			if aliceReply != nil && bobReply != nil {
				// Depending on exact timings, one of the replies might fail
				// due to the nodes already being connected, but not both.
				t.Fatalf("Both replies should not error out")
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("Timeout while waiting for connection reply")
		}

		// Give the nodes time to settle their connections and background
		// processes.
		time.Sleep(50 * time.Millisecond)

		logLine = fmt.Sprintf("=== %s: Connections requests sent. Will check on status\n",
			time.Now())
		net.Alice.AddToLog(logLine)
		net.Bob.AddToLog(logLine)

		// Sanity check connection number.
		assertNumConnections(t, net.Alice, net.Bob, 1)

		// Check whether the connection was made alice -> bob or bob ->
		// alice.  The assert above ensures we can safely access
		// alicePeers[0].
		alicePeers, err := net.Alice.ListPeers(ctxb, &lnrpc.ListPeersRequest{})
		if err != nil {
			t.Fatalf("unable to fetch carol's peers %v", err)
		}
		if !alicePeers.Peers[0].Inbound {
			// Connection was made in the alice -> bob direction.
			net.DisconnectNodes(ctxb, net.Alice, net.Bob)
		} else {
			// Connection was made in the alice <- bob direction.
			net.DisconnectNodes(ctxb, net.Alice, net.Bob)
		}
	}
	logLine := fmt.Sprintf("=== %s: Reconnection tests successful\n",
		time.Now())
	net.Alice.AddToLog(logLine)
	net.Bob.AddToLog(logLine)

	// Wait for the final disconnection to release all resources, then
	// ensure both nodes are connected again.
	time.Sleep(time.Millisecond * 50)
	assertNumConnections(t, net.Alice, net.Bob, 0)
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	err := net.EnsureConnected(ctxt, net.Alice, net.Bob)
	if err != nil {
		t.Fatalf("unable to connect alice to bob: %v", err)
	}
	time.Sleep(time.Millisecond * 50)
}

// testPaymentFollowingChannelOpen tests that the channel transition from
// 'pending' to 'open' state does not cause any inconsistencies within other
// subsystems trying to udpate the channel state in the db. We follow this
// transition with a payment that updates the commitment state and verify that
// the pending state is up to date.
func testPaymentFollowingChannelOpen(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// We first establish a channel between Alice and Bob.
	const paymentAmt = dcrutil.Amount(100)
	channelCapacity := dcrutil.Amount(paymentAmt * 1000)

	ctxt, cancel := context.WithTimeout(ctxb, channelOpenTimeout)
	defer cancel()
	pendingUpdate, err := net.OpenPendingChannel(ctxt, net.Alice, net.Bob,
		channelCapacity, 0)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	// At this point, the channel's funding transaction will have been
	// broadcast, but not confirmed. Alice and Bob's nodes
	// should reflect this when queried via RPC.
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	assertNumOpenChannelsPending(ctxt, t, net.Alice, net.Bob, 1)

	// We are restarting Bob's node to let the link be created for the
	// pending channel.
	if err := net.RestartNode(net.Bob, nil); err != nil {
		t.Fatalf("Bob restart failed: %v", err)
	}

	// We ensure that Bob reconnets to Alice.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.EnsureConnected(ctxt, net.Bob, net.Alice); err != nil {
		t.Fatalf("peers unable to reconnect after restart: %v", err)
	}

	// We mine one block for the channel to be confirmed.
	_ = mineBlocks(t, net, 6, 1)[0]

	// We verify that the chanel is open from both nodes point of view.
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	assertNumOpenChannelsPending(ctxt, t, net.Alice, net.Bob, 0)

	// With the channel open, we'll create invoices for Bob that Alice will
	// pay to in order to advance the state of the channel.
	bobPayReqs, _, _, err := createPayReqs(
		net.Bob, paymentAmt, 1,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// Send payment to Bob so that a channel update to disk will be
	// executed.
	sendAndAssertSuccess(
		t, net.Alice,
		&routerrpc.SendPaymentRequest{
			PaymentRequest: bobPayReqs[0],
			TimeoutSeconds: 60,
			FeeLimitAtoms:  1000000,
		},
	)

	// At this point we want to make sure the channel is opened and not
	// pending.
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	res, err := net.Bob.ListChannels(ctxt, &lnrpc.ListChannelsRequest{})
	if err != nil {
		t.Fatalf("unable to list bob channels: %v", err)
	}
	if len(res.Channels) == 0 {
		t.Fatalf("bob list of channels is empty")
	}

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: pendingUpdate.Txid,
		},
		OutputIndex: pendingUpdate.OutputIndex,
	}
	ctxt, cancel = context.WithTimeout(ctxb, channelCloseTimeout)
	defer cancel()
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}

// txStr returns the string representation of the channel's funding transaction.
func txStr(chanPoint *lnrpc.ChannelPoint) string {
	fundingTxID, err := dcrlnd.GetChanPointFundingTxid(chanPoint)
	if err != nil {
		return ""
	}
	cp := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: chanPoint.OutputIndex,
	}
	return cp.String()
}

// expectedChanUpdate houses params we expect a ChannelUpdate to advertise.
type expectedChanUpdate struct {
	advertisingNode string
	expectedPolicy  *lnrpc.RoutingPolicy
	chanPoint       *lnrpc.ChannelPoint
}

// calculateMaxHtlc re-implements the RequiredRemoteChannelReserve of the
// funding manager's config, which corresponds to the maximum MaxHTLC value we
// allow users to set when updating a channel policy.
func calculateMaxHtlc(chanCap dcrutil.Amount) uint64 {
	reserve := lnwire.NewMAtomsFromAtoms(chanCap / 100)
	max := lnwire.NewMAtomsFromAtoms(chanCap) - reserve
	return uint64(max)
}

// waitForChannelUpdate waits for a node to receive the expected channel
// updates.
func waitForChannelUpdate(t *harnessTest, subscription graphSubscription,
	expUpdates []expectedChanUpdate) {

	// Create an array indicating which expected channel updates we have
	// received.
	found := make([]bool, len(expUpdates))
out:
	for {
		select {
		case graphUpdate := <-subscription.updateChan:
			for _, update := range graphUpdate.ChannelUpdates {
				// For each expected update, check if it matches
				// the update we just received.
				for i, exp := range expUpdates {
					fundingTxStr := txStr(update.ChanPoint)
					if fundingTxStr != txStr(exp.chanPoint) {
						continue
					}

					if update.AdvertisingNode !=
						exp.advertisingNode {
						continue
					}

					err := checkChannelPolicy(
						update.RoutingPolicy,
						exp.expectedPolicy,
					)
					if err != nil {
						continue
					}

					// We got a policy update that matched
					// the values and channel point of what
					// we expected, mark it as found.
					found[i] = true

					// If we have no more channel updates
					// we are waiting for, break out of the
					// loop.
					rem := 0
					for _, f := range found {
						if !f {
							rem++
						}
					}

					if rem == 0 {
						break out
					}

					// Since we found a match among the
					// expected updates, break out of the
					// inner loop.
					break
				}
			}
		case err := <-subscription.errChan:
			t.Fatalf("unable to recv graph update: %v", err)
		case <-time.After(20 * time.Second):
			t.Fatalf("did not receive channel update")
		}
	}
}

// assertNoChannelUpdates ensures that no ChannelUpdates are sent via the
// graphSubscription. This method will block for the provided duration before
// returning to the caller if successful.
func assertNoChannelUpdates(t *harnessTest, subscription graphSubscription,
	duration time.Duration) {

	timeout := time.After(duration)
	for {
		select {
		case graphUpdate := <-subscription.updateChan:
			if len(graphUpdate.ChannelUpdates) > 0 {
				t.Fatalf("received %d channel updates when "+
					"none were expected",
					len(graphUpdate.ChannelUpdates))
			}

		case err := <-subscription.errChan:
			t.Fatalf("graph subscription failure: %v", err)

		case <-timeout:
			// No updates received, success.
			return
		}
	}
}

// getChannelPolicies queries the channel graph and retrieves the current edge
// policies for the provided channel points.
func getChannelPolicies(t *harnessTest, node *lntest.HarnessNode,
	advertisingNode string,
	chanPoints ...*lnrpc.ChannelPoint) []*lnrpc.RoutingPolicy {

	ctxb := context.Background()

	descReq := &lnrpc.ChannelGraphRequest{
		IncludeUnannounced: true,
	}
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	chanGraph, err := node.DescribeGraph(ctxt, descReq)
	if err != nil {
		t.Fatalf("unable to query for alice's graph: %v", err)
	}

	var policies []*lnrpc.RoutingPolicy
out:
	for _, chanPoint := range chanPoints {
		for _, e := range chanGraph.Edges {
			if e.ChanPoint != txStr(chanPoint) {
				continue
			}

			if e.Node1Pub == advertisingNode {
				policies = append(policies, e.Node1Policy)
			} else {
				policies = append(policies, e.Node2Policy)
			}

			continue out
		}

		// If we've iterated over all the known edges and we weren't
		// able to find this specific one, then we'll fail.
		t.Fatalf("did not find edge %v", txStr(chanPoint))
	}

	return policies
}

// assertChannelPolicy asserts that the passed node's known channel policy for
// the passed chanPoint is consistent with the expected policy values.
func assertChannelPolicy(t *harnessTest, node *lntest.HarnessNode,
	advertisingNode string, expectedPolicy *lnrpc.RoutingPolicy,
	chanPoints ...*lnrpc.ChannelPoint) {

	policies := getChannelPolicies(t, node, advertisingNode, chanPoints...)
	for _, policy := range policies {
		err := checkChannelPolicy(policy, expectedPolicy)
		if err != nil {
			t.Fatalf(err.Error())
		}
	}
}

// checkChannelPolicy checks that the policy matches the expected one.
func checkChannelPolicy(policy, expectedPolicy *lnrpc.RoutingPolicy) error {
	if policy.FeeBaseMAtoms != expectedPolicy.FeeBaseMAtoms {
		return fmt.Errorf("expected base fee %v, got %v",
			expectedPolicy.FeeBaseMAtoms, policy.FeeBaseMAtoms)
	}
	if policy.FeeRateMilliMAtoms != expectedPolicy.FeeRateMilliMAtoms {
		return fmt.Errorf("expected fee rate %v, got %v",
			expectedPolicy.FeeRateMilliMAtoms,
			policy.FeeRateMilliMAtoms)
	}
	if policy.TimeLockDelta != expectedPolicy.TimeLockDelta {
		return fmt.Errorf("expected time lock delta %v, got %v",
			expectedPolicy.TimeLockDelta,
			policy.TimeLockDelta)
	}
	if policy.MinHtlc != expectedPolicy.MinHtlc {
		return fmt.Errorf("expected min htlc %v, got %v",
			expectedPolicy.MinHtlc, policy.MinHtlc)
	}
	if policy.MaxHtlcMAtoms != expectedPolicy.MaxHtlcMAtoms {
		return fmt.Errorf("expected max htlc %v, got %v",
			expectedPolicy.MaxHtlcMAtoms, policy.MaxHtlcMAtoms)
	}
	if policy.Disabled != expectedPolicy.Disabled {
		return errors.New("edge should be disabled but isn't")
	}

	return nil
}

// testUpdateChannelPolicy tests that policy updates made to a channel
// gets propagated to other nodes in the network.
func testUpdateChannelPolicy(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const (
		defaultFeeBase       = 1000
		defaultFeeRate       = 1
		defaultTimeLockDelta = dcrlnd.DefaultDecredTimeLockDelta
		defaultMinHtlc       = 1000
	)

	// Launch notification clients for all nodes, such that we can
	// get notified when they discover new channels and updates in the
	// graph.
	aliceSub := subscribeGraphNotifications(t, ctxb, net.Alice)
	defer close(aliceSub.quit)
	bobSub := subscribeGraphNotifications(t, ctxb, net.Bob)
	defer close(bobSub.quit)

	chanAmt := defaultChanAmt
	pushAmt := chanAmt / 2
	defaultMaxHtlc := calculateMaxHtlc(chanAmt)

	// Create a channel Alice->Bob.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)

	// We add all the nodes' update channels to a slice, such that we can
	// make sure they all receive the expected updates.
	graphSubs := []graphSubscription{aliceSub, bobSub}
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob}

	// Alice and Bob should see each other's ChannelUpdates, advertising the
	// default routing policies.
	expectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMAtoms:      defaultFeeBase,
		FeeRateMilliMAtoms: defaultFeeRate,
		TimeLockDelta:      defaultTimeLockDelta,
		MinHtlc:            defaultMinHtlc,
		MaxHtlcMAtoms:      defaultMaxHtlc,
	}

	for _, graphSub := range graphSubs {
		waitForChannelUpdate(
			t, graphSub,
			[]expectedChanUpdate{
				{net.Alice.PubKeyStr, expectedPolicy, chanPoint},
				{net.Bob.PubKeyStr, expectedPolicy, chanPoint},
			},
		)
	}

	// They should now know about the default policies.
	for _, node := range nodes {
		assertChannelPolicy(
			t, node, net.Alice.PubKeyStr, expectedPolicy, chanPoint,
		)
		assertChannelPolicy(
			t, node, net.Bob.PubKeyStr, expectedPolicy, chanPoint,
		)
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err := net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't report channel: %v", err)
	}
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
	}

	// Create Carol and a new channel Bob->Carol.
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}

	// Clean up carol's node when the test finishes.
	defer shutdownAndAssert(net, t, carol)

	carolSub := subscribeGraphNotifications(t, ctxb, carol)
	defer close(carolSub.quit)

	graphSubs = append(graphSubs, carolSub)
	nodes = append(nodes, carol)

	// Send some coins to Carol that can be used for channel funding.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}

	if err := net.ConnectNodes(ctxb, carol, net.Bob); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}

	// Open the channel Carol->Bob with a custom min_htlc value set. Since
	// Carol is opening the channel, she will require Bob to not forward
	// HTLCs smaller than this value, and hence he should advertise it as
	// part of his ChannelUpdate.
	const customMinHtlc = 5000
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPoint2 := openChannelAndAssert(
		ctxt, t, net, carol, net.Bob,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
			MinHtlc: customMinHtlc,
		},
	)

	expectedPolicyBob := &lnrpc.RoutingPolicy{
		FeeBaseMAtoms:      defaultFeeBase,
		FeeRateMilliMAtoms: defaultFeeRate,
		TimeLockDelta:      defaultTimeLockDelta,
		MinHtlc:            customMinHtlc,
		MaxHtlcMAtoms:      defaultMaxHtlc,
	}

	expectedPolicyCarol := &lnrpc.RoutingPolicy{
		FeeBaseMAtoms:      defaultFeeBase,
		FeeRateMilliMAtoms: defaultFeeRate,
		TimeLockDelta:      defaultTimeLockDelta,
		MinHtlc:            defaultMinHtlc,
		MaxHtlcMAtoms:      defaultMaxHtlc,
	}

	for _, graphSub := range graphSubs {
		waitForChannelUpdate(
			t, graphSub,
			[]expectedChanUpdate{
				{net.Bob.PubKeyStr, expectedPolicyBob, chanPoint2},
				{carol.PubKeyStr, expectedPolicyCarol, chanPoint2},
			},
		)
	}

	// Check that all nodes now know about the updated policies.
	for _, node := range nodes {
		assertChannelPolicy(
			t, node, net.Bob.PubKeyStr, expectedPolicyBob,
			chanPoint2,
		)
		assertChannelPolicy(
			t, node, carol.PubKeyStr, expectedPolicyCarol,
			chanPoint2,
		)
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint2)
	if err != nil {
		t.Fatalf("alice didn't report channel: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint2)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPoint2)
	if err != nil {
		t.Fatalf("carol didn't report channel: %v", err)
	}

	// First we'll try to send a payment from Alice to Carol with an amount
	// less than the min_htlc value required by Carol. This payment should
	// fail, as the channel Bob->Carol cannot carry HTLCs this small.
	payAmt := dcrutil.Amount(4)
	invoice := &lnrpc.Invoice{
		Memo:  "testing",
		Value: int64(payAmt),
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	resp, err := carol.AddInvoice(ctxt, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, net.Alice, net.Alice.RouterClient,
		[]string{resp.PaymentRequest}, true,
	)

	// Alice knows about the channel policy of Carol and should therefore
	// not be able to find a path during routing.
	expErr := lnrpc.PaymentFailureReason_FAILURE_REASON_NO_ROUTE
	if err.Error() != expErr.String() {
		t.Fatalf("expected %v, instead got %v", expErr, err)
	}

	// Now we try to send a payment over the channel with a value too low
	// to be accepted. First we query for a route to route a payment of
	// 5000 milli-atoms, as this is accepted.
	payAmt = dcrutil.Amount(5)
	routesReq := &lnrpc.QueryRoutesRequest{
		PubKey:         carol.PubKeyStr,
		Amt:            int64(payAmt),
		FinalCltvDelta: defaultTimeLockDelta,
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	routes, err := net.Alice.QueryRoutes(ctxt, routesReq)
	if err != nil {
		t.Fatalf("unable to get route: %v", err)
	}

	if len(routes.Routes) != 1 {
		t.Fatalf("expected to find 1 route, got %v", len(routes.Routes))
	}

	// We change the route to carry a payment of 4000 milli-atoms instead of 5000
	// milli-atoms.
	payAmt = dcrutil.Amount(4)
	amtAtoms := int64(payAmt)
	amtMAtoms := int64(lnwire.NewMAtomsFromAtoms(payAmt))
	routes.Routes[0].Hops[0].AmtToForward = amtAtoms
	routes.Routes[0].Hops[0].AmtToForwardMAtoms = amtMAtoms
	routes.Routes[0].Hops[1].AmtToForward = amtAtoms
	routes.Routes[0].Hops[1].AmtToForwardMAtoms = amtMAtoms

	// Send the payment with the modified value.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	alicePayStream, err := net.Alice.SendToRoute(ctxt)
	if err != nil {
		t.Fatalf("unable to create payment stream for alice: %v", err)
	}
	sendReq := &lnrpc.SendToRouteRequest{
		PaymentHash: resp.RHash,
		Route:       routes.Routes[0],
	}

	err = alicePayStream.Send(sendReq)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// We expect this payment to fail, and that the min_htlc value is
	// communicated back to us, since the attempted HTLC value was too low.
	sendResp, err := alicePayStream.Recv()
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// Expected as part of the error message.
	substrs := []string{
		"AmountBelowMinimum",
		"HtlcMinimumMAtoms: (lnwire.MilliAtom) 5000 milli-atoms",
	}
	for _, s := range substrs {
		if !strings.Contains(sendResp.PaymentError, s) {
			t.Fatalf("expected error to contain \"%v\", instead "+
				"got %v", s, sendResp.PaymentError)
		}
	}

	// Make sure sending using the original value succeeds.
	payAmt = dcrutil.Amount(5)
	amtAtoms = int64(payAmt)
	amtMAtoms = int64(lnwire.NewMAtomsFromAtoms(payAmt))
	routes.Routes[0].Hops[0].AmtToForward = amtAtoms
	routes.Routes[0].Hops[0].AmtToForwardMAtoms = amtMAtoms
	routes.Routes[0].Hops[1].AmtToForward = amtAtoms
	routes.Routes[0].Hops[1].AmtToForwardMAtoms = amtMAtoms

	sendReq = &lnrpc.SendToRouteRequest{
		PaymentHash: resp.RHash,
		Route:       routes.Routes[0],
	}

	err = alicePayStream.Send(sendReq)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	sendResp, err = alicePayStream.Recv()
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	if sendResp.PaymentError != "" {
		t.Fatalf("expected payment to succeed, instead got %v",
			sendResp.PaymentError)
	}

	// With our little cluster set up, we'll update the fees and the max htlc
	// size for the Bob side of the Alice->Bob channel, and make sure
	// all nodes learn about it.
	baseFee := int64(1500)
	feeRate := int64(12)
	timeLockDelta := uint32(66)
	maxHtlc := uint64(500000)

	expectedPolicy = &lnrpc.RoutingPolicy{
		FeeBaseMAtoms:      baseFee,
		FeeRateMilliMAtoms: testFeeBase * feeRate,
		TimeLockDelta:      timeLockDelta,
		MinHtlc:            defaultMinHtlc,
		MaxHtlcMAtoms:      maxHtlc,
	}

	req := &lnrpc.PolicyUpdateRequest{
		BaseFeeMAtoms: baseFee,
		FeeRate:       float64(feeRate),
		TimeLockDelta: timeLockDelta,
		MaxHtlcMAtoms: maxHtlc,
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: chanPoint,
		},
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if _, err := net.Bob.UpdateChannelPolicy(ctxt, req); err != nil {
		t.Fatalf("unable to get alice's balance: %v", err)
	}

	// Wait for all nodes to have seen the policy update done by Bob.
	for _, graphSub := range graphSubs {
		waitForChannelUpdate(
			t, graphSub,
			[]expectedChanUpdate{
				{net.Bob.PubKeyStr, expectedPolicy, chanPoint},
			},
		)
	}

	// Check that all nodes now know about Bob's updated policy.
	for _, node := range nodes {
		assertChannelPolicy(
			t, node, net.Bob.PubKeyStr, expectedPolicy, chanPoint,
		)
	}

	// Now that all nodes have received the new channel update, we'll try
	// to send a payment from Alice to Carol to ensure that Alice has
	// internalized this fee update. This shouldn't affect the route that
	// Alice takes though: we updated the Alice -> Bob channel and she
	// doesn't pay for transit over that channel as it's direct.
	// Note that the payment amount is >= the min_htlc value for the
	// channel Bob->Carol, so it should successfully be forwarded.
	payAmt = dcrutil.Amount(5)
	invoice = &lnrpc.Invoice{
		Memo:  "testing",
		Value: int64(payAmt),
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	resp, err = carol.AddInvoice(ctxt, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, net.Alice, net.Alice.RouterClient,
		[]string{resp.PaymentRequest}, true,
	)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// We'll now open a channel from Alice directly to Carol.
	if err := net.ConnectNodes(ctxb, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPoint3 := openChannelAndAssert(
		ctxt, t, net, net.Alice, carol,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint3)
	if err != nil {
		t.Fatalf("alice didn't report channel: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPoint3)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
	}

	// Make a global update, and check that both channels' new policies get
	// propagated.
	baseFee = int64(800)
	feeRate = int64(123)
	timeLockDelta = uint32(22)
	maxHtlc = maxHtlc * 2

	expectedPolicy.FeeBaseMAtoms = baseFee
	expectedPolicy.FeeRateMilliMAtoms = testFeeBase * feeRate
	expectedPolicy.TimeLockDelta = timeLockDelta
	expectedPolicy.MaxHtlcMAtoms = maxHtlc

	req = &lnrpc.PolicyUpdateRequest{
		BaseFeeMAtoms: baseFee,
		FeeRate:       float64(feeRate),
		TimeLockDelta: timeLockDelta,
		MaxHtlcMAtoms: maxHtlc,
	}
	req.Scope = &lnrpc.PolicyUpdateRequest_Global{}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = net.Alice.UpdateChannelPolicy(ctxt, req)
	if err != nil {
		t.Fatalf("unable to update alice's channel policy: %v", err)
	}

	// Wait for all nodes to have seen the policy updates for both of
	// Alice's channels.
	for _, graphSub := range graphSubs {
		waitForChannelUpdate(
			t, graphSub,
			[]expectedChanUpdate{
				{net.Alice.PubKeyStr, expectedPolicy, chanPoint},
				{net.Alice.PubKeyStr, expectedPolicy, chanPoint3},
			},
		)
	}

	// And finally check that all nodes remembers the policy update they
	// received.
	for _, node := range nodes {
		assertChannelPolicy(
			t, node, net.Alice.PubKeyStr, expectedPolicy,
			chanPoint, chanPoint3,
		)
	}

	// Close the channels.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Bob, chanPoint2, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint3, false)
}

// waitForNodeBlockHeight queries the node for its current block height until
// it reaches the passed height.
func waitForNodeBlockHeight(ctx context.Context, node *lntest.HarnessNode,
	height int64) error {
	var predErr error
	err := wait.Predicate(func() bool {
		ctxt, _ := context.WithTimeout(ctx, 10*time.Second)
		info, err := node.GetInfo(ctxt, &lnrpc.GetInfoRequest{})
		if err != nil {
			predErr = err
			return false
		}

		if int64(info.BlockHeight) != height {
			predErr = fmt.Errorf("expected block height to "+
				"be %v, was %v", height, info.BlockHeight)
			return false
		}
		return true
	}, 15*time.Second)
	if err != nil {
		return predErr
	}
	return nil
}

// assertMinerBlockHeightDelta ensures that tempMiner is 'delta' blocks ahead
// of miner.
func assertMinerBlockHeightDelta(t *harnessTest,
	miner, tempMiner *rpctest.Harness, delta int64) {

	// Ensure the chain lengths are what we expect.
	var predErr error
	err := wait.Predicate(func() bool {
		_, tempMinerHeight, err := tempMiner.Node.GetBestBlock(context.Background())
		if err != nil {
			predErr = fmt.Errorf("unable to get current "+
				"blockheight %v", err)
			return false
		}

		_, minerHeight, err := miner.Node.GetBestBlock(context.Background())
		if err != nil {
			predErr = fmt.Errorf("unable to get current "+
				"blockheight %v", err)
			return false
		}

		if tempMinerHeight != minerHeight+delta {
			predErr = fmt.Errorf("expected new miner(%d) to be %d "+
				"blocks ahead of original miner(%d)",
				tempMinerHeight, delta, minerHeight)
			return false
		}
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf(predErr.Error())
	}
}

// testOpenChannelAfterReorg tests that in the case where we have an open
// channel where the funding tx gets reorged out, the channel will no
// longer be present in the node's routing table.
func testOpenChannelAfterReorg(net *lntest.NetworkHarness, t *harnessTest) {
	var ctxb = context.Background()

	// Currently disabled due to
	// https://github.com/decred/dcrwallet/issues/1710. Re-assess after
	// that is fixed.
	if net.BackendCfg.Name() == "spv" {
		t.Skipf("Skipping for SPV for the moment")
	}

	// Set up a new miner that we can use to cause a reorg.
	args := []string{"--rejectnonstd", "--txindex"}
	tempMiner, err := testutils.NewSetupRPCTest(
		t.t, 5, harnessNetParams, &rpcclient.NotificationHandlers{}, args,
		false, 0,
	)
	if err != nil {
		t.Fatalf("unable to create mining node: %v", err)
	}
	defer tempMiner.TearDown()

	// We start by connecting the new miner to our original miner, such
	// that it will sync to our original chain.
	if err := rpctest.ConnectNode(net.Miner, tempMiner); err != nil {
		t.Fatalf("unable to connect harnesses: %v", err)
	}
	nodeSlice := []*rpctest.Harness{net.Miner, tempMiner}
	if err := rpctest.JoinNodes(nodeSlice, rpctest.Blocks); err != nil {
		t.Fatalf("unable to join node on blocks: %v", err)
	}

	// The two miners should be on the same blockheight.
	assertMinerBlockHeightDelta(t, net.Miner, tempMiner, 0)

	// We disconnect the two nodes, such that we can start mining on them
	// individually without the other one learning about the new blocks.
	err = rpctest.RemoveNode(context.Background(), net.Miner, tempMiner)
	if err != nil {
		t.Fatalf("unable to remove node: %v", err)
	}

	// Create a new channel that requires 1 confs before it's considered
	// open, then broadcast the funding transaction
	chanAmt := defaultChanAmt
	pushAmt := dcrutil.Amount(0)
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	pendingUpdate, err := net.OpenPendingChannel(ctxt, net.Alice, net.Bob,
		chanAmt, pushAmt)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	// Wait for miner to have seen the funding tx. The temporary miner is
	// disconnected, and won't see the transaction.
	_, err = waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("failed to find funding tx in mempool: %v", err)
	}

	// At this point, the channel's funding transaction will have been
	// broadcast, but not confirmed, and the channel should be pending.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	assertNumOpenChannelsPending(ctxt, t, net.Alice, net.Bob, 1)

	fundingTxID, err := chainhash.NewHash(pendingUpdate.Txid)
	if err != nil {
		t.Fatalf("unable to convert funding txid into chainhash.Hash:"+
			" %v", err)
	}

	// We now cause a fork, by letting our original miner mine 10 blocks,
	// and our new miner mine 15. This will also confirm our pending
	// channel on the original miner's chain, which should be considered
	// open.
	block := mineBlocks(t, net, 10, 1)[0]
	assertTxInBlock(t, block, fundingTxID)
	testutils.AdjustedSimnetMiner(tempMiner.Node, 15)

	// Ensure the chain lengths are what we expect, with the temp miner
	// being 5 blocks ahead.
	assertMinerBlockHeightDelta(t, net.Miner, tempMiner, 5)

	// Wait for Alice to sync to the original miner's chain.
	_, minerHeight, err := net.Miner.Node.GetBestBlock(context.Background())
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = waitForNodeBlockHeight(ctxt, net.Alice, minerHeight)
	if err != nil {
		t.Fatalf("unable to sync to chain: %v", err)
	}

	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: pendingUpdate.Txid,
		},
		OutputIndex: pendingUpdate.OutputIndex,
	}

	// Ensure channel is no longer pending.
	assertNumOpenChannelsPending(ctxt, t, net.Alice, net.Bob, 0)

	// Wait for Alice and Bob to recognize and advertise the new channel
	// generated above.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't advertise channel before "+
			"timeout: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("bob didn't advertise channel before "+
			"timeout: %v", err)
	}

	// Alice should now have 1 edge in her graph.
	req := &lnrpc.ChannelGraphRequest{
		IncludeUnannounced: true,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	chanGraph, err := net.Alice.DescribeGraph(ctxt, req)
	if err != nil {
		t.Fatalf("unable to query for alice's routing table: %v", err)
	}

	numEdges := len(chanGraph.Edges)
	if numEdges != 1 {
		t.Fatalf("expected to find one edge in the graph, found %d",
			numEdges)
	}

	// Let enough time pass for all wallet sync to be complete.
	time.Sleep(3 * time.Second)

	// Now we disconnect Alice's chain backend from the original miner, and
	// connect the two miners together. Since the temporary miner knows
	// about a longer chain, both miners should sync to that chain.
	err = net.BackendCfg.DisconnectMiner()
	if err != nil {
		t.Fatalf("unable to remove node: %v", err)
	}

	// Connecting to the temporary miner should now cause our original
	// chain to be re-orged out.
	err = rpctest.ConnectNode(net.Miner, tempMiner)
	if err != nil {
		t.Fatalf("unable to remove node: %v", err)
	}

	nodes := []*rpctest.Harness{tempMiner, net.Miner}
	if err := rpctest.JoinNodes(nodes, rpctest.Blocks); err != nil {
		t.Fatalf("unable to join node on blocks: %v", err)
	}

	// Once again they should be on the same chain.
	assertMinerBlockHeightDelta(t, net.Miner, tempMiner, 0)

	// Now we disconnect the two miners, and connect our original miner to
	// our chain backend once again.
	err = rpctest.RemoveNode(context.Background(), net.Miner, tempMiner)
	if err != nil {
		t.Fatalf("unable to remove node: %v", err)
	}

	err = net.BackendCfg.ConnectMiner()
	if err != nil {
		t.Fatalf("unable to remove node: %v", err)
	}

	// This should have caused a reorg, and Alice should sync to the longer
	// chain, where the funding transaction is not confirmed.
	_, tempMinerHeight, err := tempMiner.Node.GetBestBlock(context.Background())
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = waitForNodeBlockHeight(ctxt, net.Alice, tempMinerHeight)
	if err != nil {
		t.Fatalf("unable to sync to chain: %v", err)
	}

	// Since the fundingtx was reorged out, Alice should now have no edges
	// in her graph.
	req = &lnrpc.ChannelGraphRequest{
		IncludeUnannounced: true,
	}

	var predErr error
	err = wait.Predicate(func() bool {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		chanGraph, err = net.Alice.DescribeGraph(ctxt, req)
		if err != nil {
			predErr = fmt.Errorf("unable to query for alice's routing table: %v", err)
			return false
		}

		numEdges = len(chanGraph.Edges)
		if numEdges != 0 {
			predErr = fmt.Errorf("expected to find no edge in the graph, found %d",
				numEdges)
			return false
		}
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf(predErr.Error())
	}

	// Wait again for any outstanding ops in the subsystems to catch up.
	time.Sleep(3 * time.Second)

	// Cleanup by mining the funding tx again, then closing the channel.
	block = mineBlocks(t, net, 1, 1)[0]
	assertTxInBlock(t, block, fundingTxID)

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeReorgedChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}

// testDisconnectingTargetPeer performs a test which disconnects Alice-peer from
// Bob-peer and then re-connects them again. We expect Alice to be able to
// disconnect at any point.
func testDisconnectingTargetPeer(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// We'll start both nodes with a high backoff so that they don't
	// reconnect automatically during our test.
	args := []string{
		"--minbackoff=1m",
		"--maxbackoff=1m",
	}

	alice, err := net.NewNode("Alice", args)
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}
	defer shutdownAndAssert(net, t, alice)

	bob, err := net.NewNode("Bob", args)
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}
	defer shutdownAndAssert(net, t, bob)

	// Start by connecting Alice and Bob with no channels.
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, alice, bob); err != nil {
		t.Fatalf("unable to connect Alice's peer to Bob's: err %v", err)
	}

	// Check existing connection.
	assertNumConnections(t, alice, bob, 1)

	// Give Alice some coins so she can fund a channel.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, alice)
	if err != nil {
		t.Fatalf("unable to send coins to alice: %v", err)
	}

	chanAmt := defaultChanAmt
	pushAmt := dcrutil.Amount(0)

	// Create a new channel that requires 1 confs before it's considered
	// open, then broadcast the funding transaction
	const numConfs = 1
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	pendingUpdate, err := net.OpenPendingChannel(
		ctxt, alice, bob, chanAmt, pushAmt,
	)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	// At this point, the channel's funding transaction will have been
	// broadcast, but not confirmed. Alice and Bob's nodes should reflect
	// this when queried via RPC.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	assertNumOpenChannelsPending(ctxt, t, alice, bob, 1)

	// Disconnect Alice-peer from Bob-peer and get error causes by one
	// pending channel with detach node is existing.
	if err := net.DisconnectNodes(ctxt, alice, bob); err != nil {
		t.Fatalf("Bob's peer was disconnected from Alice's"+
			" while one pending channel is existing: err %v", err)
	}

	time.Sleep(time.Millisecond * 300)

	// Assert that the connection was torn down.
	assertNumConnections(t, alice, bob, 0)

	fundingTxID, err := chainhash.NewHash(pendingUpdate.Txid)
	if err != nil {
		t.Fatalf("unable to convert funding txid into chainhash.Hash:"+
			" %v", err)
	}

	// Mine a block, then wait for Alice's node to notify us that the
	// channel has been opened. The funding transaction should be found
	// within the newly mined block.
	block := mineBlocks(t, net, numConfs, 1)[0]
	assertTxInBlock(t, block, fundingTxID)

	// At this point, the channel should be fully opened and there should be
	// no pending channels remaining for either node.
	time.Sleep(time.Millisecond * 300)
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)

	assertNumOpenChannelsPending(ctxt, t, alice, bob, 0)

	// Reconnect the nodes so that the channel can become active.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, alice, bob); err != nil {
		t.Fatalf("unable to connect Alice's peer to Bob's: err %v", err)
	}

	// The channel should be listed in the peer information returned by both
	// peers.
	outPoint := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: pendingUpdate.OutputIndex,
	}

	// Check both nodes to ensure that the channel is ready for operation.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.AssertChannelExists(ctxt, alice, &outPoint); err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.AssertChannelExists(ctxt, bob, &outPoint); err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}

	// Disconnect Alice-peer from Bob-peer and get error causes by one
	// active channel with detach node is existing.
	if err := net.DisconnectNodes(ctxt, alice, bob); err != nil {
		t.Fatalf("Bob's peer was disconnected from Alice's"+
			" while one active channel is existing: err %v", err)
	}

	// Check existing connection.
	assertNumConnections(t, alice, bob, 0)

	// Reconnect both nodes before force closing the channel.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, alice, bob); err != nil {
		t.Fatalf("unable to connect Alice's peer to Bob's: err %v", err)
	}

	// Finally, immediately close the channel. This function will also block
	// until the channel is closed and will additionally assert the relevant
	// channel closing post conditions.
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: pendingUpdate.Txid,
		},
		OutputIndex: pendingUpdate.OutputIndex,
	}

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, alice, chanPoint, true)

	// Disconnect Alice-peer from Bob-peer without getting error about
	// existing channels.
	if err := net.DisconnectNodes(ctxt, alice, bob); err != nil {
		t.Fatalf("unable to disconnect Bob's peer from Alice's: err %v",
			err)
	}

	// Check zero peer connections.
	assertNumConnections(t, alice, bob, 0)

	// Finally, re-connect both nodes.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, alice, bob); err != nil {
		t.Fatalf("unable to connect Alice's peer to Bob's: err %v", err)
	}

	// Check existing connection.
	assertNumConnections(t, alice, net.Bob, 1)

	// Cleanup by mining the force close and sweep transaction.
	cleanupForceClose(t, net, alice, chanPoint)
}

// testFundingPersistence is intended to ensure that the Funding Manager
// persists the state of new channels prior to broadcasting the channel's
// funding transaction. This ensures that the daemon maintains an up-to-date
// representation of channels if the system is restarted or disconnected.
// testFundingPersistence mirrors testBasicChannelFunding, but adds restarts
// and checks for the state of channels with unconfirmed funding transactions.
func testChannelFundingPersistence(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	chanAmt := defaultChanAmt
	pushAmt := dcrutil.Amount(0)

	// As we need to create a channel that requires more than 1
	// confirmation before it's open, with the current set of defaults,
	// we'll need to create a new node instance.
	const numConfs = 5
	carolArgs := []string{fmt.Sprintf("--defaultchanconfs=%v", numConfs)}
	carol, err := net.NewNode("Carol", carolArgs)
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}

	// Clean up carol's node when the test finishes.
	defer shutdownAndAssert(net, t, carol)

	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect alice to carol: %v", err)
	}

	// Create a new channel that requires 5 confs before it's considered
	// open, then broadcast the funding transaction
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	pendingUpdate, err := net.OpenPendingChannel(ctxt, net.Alice, carol,
		chanAmt, pushAmt)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	// At this point, the channel's funding transaction will have been
	// broadcast, but not confirmed. Alice and Bob's nodes should reflect
	// this when queried via RPC.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	assertNumOpenChannelsPending(ctxt, t, net.Alice, carol, 1)

	// Restart both nodes to test that the appropriate state has been
	// persisted and that both nodes recover gracefully.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}
	if err := net.RestartNode(carol, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	fundingTxID, err := chainhash.NewHash(pendingUpdate.Txid)
	if err != nil {
		t.Fatalf("unable to convert funding txid into chainhash.Hash:"+
			" %v", err)
	}

	// Mine a block, then wait for Alice's node to notify us that the
	// channel has been opened. The funding transaction should be found
	// within the newly mined block.
	block := mineBlocks(t, net, 1, 1)[0]
	assertTxInBlock(t, block, fundingTxID)

	// Restart both nodes to test that the appropriate state has been
	// persisted and that both nodes recover gracefully.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}
	if err := net.RestartNode(carol, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// The following block ensures that after both nodes have restarted,
	// they have reconnected before the execution of the next test.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.EnsureConnected(ctxt, net.Alice, carol); err != nil {
		t.Fatalf("peers unable to reconnect after restart: %v", err)
	}

	// Next, mine enough blocks s.t the channel will open with a single
	// additional block mined.
	if _, err := net.Generate(3); err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}

	// Both nodes should still show a single channel as pending.
	time.Sleep(time.Second * 1)
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	assertNumOpenChannelsPending(ctxt, t, net.Alice, carol, 1)

	// Finally, mine the last block which should mark the channel as open.
	if _, err := net.Generate(1); err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}

	// At this point, the channel should be fully opened and there should
	// be no pending channels remaining for either node.
	time.Sleep(time.Second * 1)
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	assertNumOpenChannelsPending(ctxt, t, net.Alice, carol, 0)

	// The channel should be listed in the peer information returned by
	// both peers.
	outPoint := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: pendingUpdate.OutputIndex,
	}

	// Check both nodes to ensure that the channel is ready for operation.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.AssertChannelExists(ctxt, net.Alice, &outPoint); err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.AssertChannelExists(ctxt, carol, &outPoint); err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: pendingUpdate.Txid,
		},
		OutputIndex: pendingUpdate.OutputIndex,
	}
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}

// testChannelBalance creates a new channel between Alice and  Bob, then
// checks channel balance to be equal amount specified while creation of channel.
func testChannelBalance(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// Open a channel with 0.16 DCR between Alice and Bob, ensuring the
	// channel has been opened properly.
	amount := defaultChanAmt

	// Creates a helper closure to be used below which asserts the proper
	// response to a channel balance RPC.
	checkChannelBalance := func(node lnrpc.LightningClient,
		amount dcrutil.Amount) {

		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		response, err := node.ChannelBalance(ctxt, &lnrpc.ChannelBalanceRequest{})
		if err != nil {
			t.Fatalf("unable to get channel balance: %v", err)
		}

		balance := dcrutil.Amount(response.Balance)
		if balance != amount {
			t.Fatalf("channel balance wrong: %v != %v", balance,
				amount)
		}
	}

	// Before beginning, make sure alice and bob are connected.
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	if err := net.EnsureConnected(ctxt, net.Alice, net.Bob); err != nil {
		t.Fatalf("unable to connect alice and bob: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: amount,
		},
	)

	// Wait for both Alice and Bob to recognize this new channel.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err := net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't advertise channel before "+
			"timeout: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("bob didn't advertise channel before "+
			"timeout: %v", err)
	}

	cType, err := channelCommitType(net.Alice, chanPoint)
	if err != nil {
		t.Fatalf("unable to get channel type: %v", err)
	}

	// As this is a single funder channel, Alice's balance should be
	// exactly 0.5 DCR since now state transitions have taken place yet.
	checkChannelBalance(net.Alice, amount-cType.calcStaticFee(0))

	// Ensure Bob currently has no available balance within the channel.
	checkChannelBalance(net.Bob, 0)

	// Finally close the channel between Alice and Bob, asserting that the
	// channel has been properly closed on-chain.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}

// testChannelUnsettledBalance will test that the UnsettledBalance field
// is updated according to the number of Pending Htlcs.
// Alice will send Htlcs to Carol while she is in hodl mode. This will result
// in a build of pending Htlcs. We expect the channels unsettled balance to
// equal the sum of all the Pending Htlcs.
func testChannelUnsettledBalance(net *lntest.NetworkHarness, t *harnessTest) {
	const chanAmt = dcrutil.Amount(1000000)
	ctxb := context.Background()

	// Create carol in hodl mode.
	carol, err := net.NewNode("Carol", []string{"--hodl.exit-settle"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	// Connect Alice to Carol.
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxb, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect alice to carol: %v", err)
	}

	// Open a channel between Alice and Carol.
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointAlice := openChannelAndAssert(
		ctxt, t, net, net.Alice, carol,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Wait for Alice and Carol to receive the channel edge from the
	// funding manager.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPointAlice)
	if err != nil {
		t.Fatalf("alice didn't see the alice->carol channel before "+
			"timeout: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointAlice)
	if err != nil {
		t.Fatalf("alice didn't see the alice->carol channel before "+
			"timeout: %v", err)
	}

	// Channel should be ready for payments.
	const (
		payAmt      = 100
		numInvoices = 6
	)

	// Simulateneously send numInvoices payments from Alice to Carol.
	carolPubKey := carol.PubKey[:]
	errChan := make(chan error)
	for i := 0; i < numInvoices; i++ {
		go func() {
			ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
			_, err := net.Alice.RouterClient.SendPaymentV2(ctxt,
				&routerrpc.SendPaymentRequest{
					Dest:           carolPubKey,
					Amt:            int64(payAmt),
					PaymentHash:    makeFakePayHash(t),
					FinalCltvDelta: dcrlnd.DefaultDecredTimeLockDelta,
					TimeoutSeconds: 60,
					FeeLimitMAtoms: noFeeLimitMAtoms,
				})

			if err != nil {
				errChan <- err
			}
		}()
	}

	// Test that the UnsettledBalance for both Alice and Carol
	// is equal to the amount of invoices * payAmt.
	var unsettledErr error
	nodes := []*lntest.HarnessNode{net.Alice, carol}
	err = wait.Predicate(func() bool {
		// There should be a number of PendingHtlcs equal
		// to the amount of Invoices sent.
		unsettledErr = assertNumActiveHtlcs(nodes, numInvoices)
		if unsettledErr != nil {
			return false
		}

		// Set the amount expected for the Unsettled Balance for
		// this channel.
		expectedBalance := numInvoices * payAmt

		// Check each nodes UnsettledBalance field.
		for _, node := range nodes {
			// Get channel info for the node.
			ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
			chanInfo, err := getChanInfo(ctxt, node)
			if err != nil {
				unsettledErr = err
				return false
			}

			// Check that UnsettledBalance is what we expect.
			if int(chanInfo.UnsettledBalance) != expectedBalance {
				unsettledErr = fmt.Errorf("unsettled balance failed "+
					"expected: %v, received: %v", expectedBalance,
					chanInfo.UnsettledBalance)
				return false
			}
		}

		return true
	}, defaultTimeout)
	if err != nil {
		t.Fatalf("unsettled balace error: %v", unsettledErr)
	}

	// Check for payment errors.
	select {
	case err := <-errChan:
		t.Fatalf("payment error: %v", err)
	default:
	}

	// Force and assert the channel closure.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, true)

	// Cleanup by mining the force close and sweep transaction.
	cleanupForceClose(t, net, net.Alice, chanPointAlice)
}

// findForceClosedChannel searches a pending channel response for a particular
// channel, returning the force closed channel upon success.
func findForceClosedChannel(pendingChanResp *lnrpc.PendingChannelsResponse,
	op *wire.OutPoint) (*lnrpc.PendingChannelsResponse_ForceClosedChannel, error) {

	for _, forceClose := range pendingChanResp.PendingForceClosingChannels {
		if forceClose.Channel.ChannelPoint == op.String() {
			return forceClose, nil
		}
	}

	return nil, errors.New("channel not marked as force closed")
}

// findWaitingCloseChannel searches a pending channel response for a particular
// channel, returning the waiting close channel upon success.
func findWaitingCloseChannel(pendingChanResp *lnrpc.PendingChannelsResponse,
	op *wire.OutPoint) (*lnrpc.PendingChannelsResponse_WaitingCloseChannel, error) {

	for _, waitingClose := range pendingChanResp.WaitingCloseChannels {
		if waitingClose.Channel.ChannelPoint == op.String() {
			return waitingClose, nil
		}
	}

	return nil, errors.New("channel not marked as waiting close")
}

func checkCommitmentMaturity(
	forceClose *lnrpc.PendingChannelsResponse_ForceClosedChannel,
	maturityHeight uint32, blocksTilMaturity int32) error {

	if forceClose.MaturityHeight != maturityHeight {
		return fmt.Errorf("expected commitment maturity height to be "+
			"%d, found %d instead", maturityHeight,
			forceClose.MaturityHeight)
	}
	if forceClose.BlocksTilMaturity != blocksTilMaturity {
		return fmt.Errorf("expected commitment blocks til maturity to "+
			"be %d, found %d instead", blocksTilMaturity,
			forceClose.BlocksTilMaturity)
	}

	return nil
}

// checkForceClosedChannelNumHtlcs verifies that a force closed channel has the
// proper number of htlcs.
func checkPendingChannelNumHtlcs(
	forceClose *lnrpc.PendingChannelsResponse_ForceClosedChannel,
	expectedNumHtlcs int) error {

	if len(forceClose.PendingHtlcs) != expectedNumHtlcs {
		return fmt.Errorf("expected force closed channel to have %d "+
			"pending htlcs, found %d instead", expectedNumHtlcs,
			len(forceClose.PendingHtlcs))
	}

	return nil
}

// checkNumForceClosedChannels checks that a pending channel response has the
// expected number of force closed channels.
func checkNumForceClosedChannels(pendingChanResp *lnrpc.PendingChannelsResponse,
	expectedNumChans int) error {

	if len(pendingChanResp.PendingForceClosingChannels) != expectedNumChans {
		return fmt.Errorf("expected to find %d force closed channels, "+
			"got %d", expectedNumChans,
			len(pendingChanResp.PendingForceClosingChannels))
	}

	return nil
}

// checkNumWaitingCloseChannels checks that a pending channel response has the
// expected number of channels waiting for closing tx to confirm.
func checkNumWaitingCloseChannels(pendingChanResp *lnrpc.PendingChannelsResponse,
	expectedNumChans int) error {

	if len(pendingChanResp.WaitingCloseChannels) != expectedNumChans {
		return fmt.Errorf("expected to find %d channels waiting "+
			"closure, got %d", expectedNumChans,
			len(pendingChanResp.WaitingCloseChannels))
	}

	return nil
}

// checkPendingHtlcStageAndMaturity uniformly tests all pending htlc's belonging
// to a force closed channel, testing for the expected stage number, blocks till
// maturity, and the maturity height.
func checkPendingHtlcStageAndMaturity(
	forceClose *lnrpc.PendingChannelsResponse_ForceClosedChannel,
	stage, maturityHeight uint32, blocksTillMaturity int32) error {

	for _, pendingHtlc := range forceClose.PendingHtlcs {
		if pendingHtlc.Stage != stage {
			return fmt.Errorf("expected pending htlc to be stage "+
				"%d, found %d", stage, pendingHtlc.Stage)
		}
		if pendingHtlc.MaturityHeight != maturityHeight {
			return fmt.Errorf("expected pending htlc maturity "+
				"height to be %d, instead has %d",
				maturityHeight, pendingHtlc.MaturityHeight)
		}
		if pendingHtlc.BlocksTilMaturity != blocksTillMaturity {
			return fmt.Errorf("expected pending htlc blocks til "+
				"maturity to be %d, instead has %d",
				blocksTillMaturity,
				pendingHtlc.BlocksTilMaturity)
		}
	}

	return nil
}

// padCLTV is a small helper function that pads a cltv value with a block
// padding.
func padCLTV(cltv uint32) uint32 {
	return cltv + uint32(routing.BlockPadding)
}

// testChannelForceClosure performs a test to exercise the behavior of "force"
// closing a channel or unilaterally broadcasting the latest local commitment
// state on-chain. The test creates a new channel between Alice and Carol, then
// force closes the channel after some cursory assertions. Within the test, a
// total of 3 + n transactions will be broadcast, representing the commitment
// transaction, a transaction sweeping the local CSV delayed output, a
// transaction sweeping the CSV delayed 2nd-layer htlcs outputs, and n
// htlc timeout transactions, where n is the number of payments Alice attempted
// to send to Carol.  This test includes several restarts to ensure that the
// transaction output states are persisted throughout the forced closure
// process.
//
// TODO(roasbeef): also add an unsettled HTLC before force closing.
func testChannelForceClosure(net *lntest.NetworkHarness, t *harnessTest) {
	// We'll test the scenario for some of the commitment types, to ensure
	// outputs can be swept.
	commitTypes := []commitType{
		commitTypeLegacy,
		commitTypeAnchors,
	}

	for _, channelType := range commitTypes {
		testName := fmt.Sprintf("committype=%v", channelType)

		success := t.t.Run(testName, func(t *testing.T) {
			ht := newHarnessTest(t, net)

			args := channelType.Args()
			alice, err := net.NewNode("Alice", args)
			if err != nil {
				t.Fatalf("unable to create new node: %v", err)
			}
			defer shutdownAndAssert(net, ht, alice)

			// Since we'd like to test failure scenarios with
			// outstanding htlcs, we'll introduce another node into
			// our test network: Carol.
			carolArgs := []string{"--hodl.exit-settle"}
			carolArgs = append(carolArgs, args...)
			carol, err := net.NewNode("Carol", carolArgs)
			if err != nil {
				t.Fatalf("unable to create new nodes: %v", err)
			}
			defer shutdownAndAssert(net, ht, carol)

			// Each time, we'll send Alice  new set of coins in
			// order to fund the channel.
			ctxt, _ := context.WithTimeout(
				context.Background(), defaultTimeout,
			)
			err = net.SendCoins(
				ctxt, dcrutil.AtomsPerCoin, alice,
			)
			if err != nil {
				t.Fatalf("unable to send coins to Alice: %v",
					err)
			}

			// Also give Carol some coins to allow her to sweep her
			// anchor.
			err = net.SendCoins(
				ctxt, dcrutil.AtomsPerCoin, carol,
			)
			if err != nil {
				t.Fatalf("unable to send coins to Alice: %v",
					err)
			}

			channelForceClosureTest(
				net, ht, alice, carol, channelType,
			)
		})
		if !success {
			return
		}
	}
}

func channelForceClosureTest(net *lntest.NetworkHarness, t *harnessTest,
	alice, carol *lntest.HarnessNode, channelType commitType) {

	ctxb := context.Background()

	const (
		chanAmt     = dcrutil.Amount(10e6)
		pushAmt     = dcrutil.Amount(5e6)
		paymentAmt  = 100000
		numInvoices = 6
	)

	// TODO(roasbeef): should check default value in config here
	// instead, or make delay a param
	defaultCLTV := uint32(dcrlnd.DefaultDecredTimeLockDelta)

	// We must let Alice have an open channel before she can send a node
	// announcement, so we open a channel with Carol,
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, alice, carol); err != nil {
		t.Fatalf("unable to connect alice to carol: %v", err)
	}

	// Before we start, obtain Carol's current wallet balance, we'll check
	// to ensure that at the end of the force closure by Alice, Carol
	// recognizes his new on-chain output.
	carolBalReq := &lnrpc.WalletBalanceRequest{}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	carolBalResp, err := carol.WalletBalance(ctxt, carolBalReq)
	if err != nil {
		t.Fatalf("unable to get carol's balance: %v", err)
	}

	carolStartingBalance := carolBalResp.ConfirmedBalance

	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, alice, carol,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)

	// Wait for Alice and Carol to receive the channel edge from the
	// funding manager.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't see the alice->carol channel before "+
			"timeout: %v", err)
	}
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't see the alice->carol channel before "+
			"timeout: %v", err)
	}

	// Send payments from Alice to Carol, since Carol is htlchodl mode, the
	// htlc outputs should be left unsettled, and should be swept by the
	// utxo nursery.
	carolPubKey := carol.PubKey[:]
	for i := 0; i < numInvoices; i++ {
		ctx, cancel := context.WithCancel(ctxb)
		defer cancel()

		_, err := alice.RouterClient.SendPaymentV2(
			ctx,
			&routerrpc.SendPaymentRequest{
				Dest:           carolPubKey,
				Amt:            int64(paymentAmt),
				PaymentHash:    makeFakePayHash(t),
				FinalCltvDelta: dcrlnd.DefaultDecredTimeLockDelta,
				TimeoutSeconds: 60,
				FeeLimitMAtoms: noFeeLimitMAtoms,
			},
		)
		if err != nil {
			t.Fatalf("unable to send alice htlc: %v", err)
		}
	}

	// Once the HTLC has cleared, all the nodes n our mini network should
	// show that the HTLC has been locked in.
	nodes := []*lntest.HarnessNode{alice, carol}
	var predErr error
	err = wait.Predicate(func() bool {
		predErr = assertNumActiveHtlcs(nodes, numInvoices)
		return predErr == nil
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// Fetch starting height of this test so we can compute the block
	// heights we expect certain events to take place.
	_, curHeight, err := net.Miner.Node.GetBestBlock(context.Background())
	if err != nil {
		t.Fatalf("unable to get best block height")
	}

	// Using the current height of the chain, derive the relevant heights
	// for incubating two-stage htlcs.
	var (
		startHeight           = uint32(curHeight)
		commCsvMaturityHeight = startHeight + 1 + defaultCSV
		htlcExpiryHeight      = padCLTV(startHeight + defaultCLTV)
		htlcCsvMaturityHeight = padCLTV(startHeight + defaultCLTV + 1 + defaultCSV)
	)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	aliceChan, err := getChanInfo(ctxt, alice)
	if err != nil {
		t.Fatalf("unable to get alice's channel info: %v", err)
	}
	if aliceChan.NumUpdates == 0 {
		t.Fatalf("alice should see at least one update to her channel")
	}

	// Now that the channel is open and we have unsettled htlcs, immediately
	// execute a force closure of the channel. This will also assert that
	// the commitment transaction was immediately broadcast in order to
	// fulfill the force closure request.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	_, closingTxID, err := net.CloseChannel(ctxt, alice, chanPoint, true)
	if err != nil {
		t.Fatalf("unable to execute force channel closure: %v", err)
	}

	// Now that the channel has been force closed, it should show up in the
	// PendingChannels RPC under the waiting close section.
	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	pendingChanResp, err := alice.PendingChannels(ctxt, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}
	err = checkNumWaitingCloseChannels(pendingChanResp, 1)
	if err != nil {
		t.Fatalf(err.Error())
	}

	// Compute the outpoint of the channel, which we will use repeatedly to
	// locate the pending channel information in the rpc responses.
	txid, err := dcrlnd.GetChanPointFundingTxid(chanPoint)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	op := wire.OutPoint{
		Hash:  *txid,
		Index: chanPoint.OutputIndex,
	}

	waitingClose, err := findWaitingCloseChannel(pendingChanResp, &op)
	if err != nil {
		t.Fatalf(err.Error())
	}

	// Immediately after force closing, all of the funds should be in limbo.
	if waitingClose.LimboBalance == 0 {
		t.Fatalf("all funds should still be in limbo")
	}

	// Create a map of outpoints to expected resolutions for alice and carol
	// which we will add reports to as we sweep outputs.
	var (
		aliceReports = make(map[string]*lnrpc.Resolution)
		carolReports = make(map[string]*lnrpc.Resolution)
	)

	// The several restarts in this test are intended to ensure that when a
	// channel is force-closed, the UTXO nursery has persisted the state of
	// the channel in the closure process and will recover the correct state
	// when the system comes back on line. This restart tests state
	// persistence at the beginning of the process, when the commitment
	// transaction has been broadcast but not yet confirmed in a block.
	if err := net.RestartNode(alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Mine a block which should confirm the commitment transaction
	// broadcast as a result of the force closure. If there are anchors, we
	// also expect the anchor sweep tx to be in the mempool.
	expectedTxes := 1
	if channelType == commitTypeAnchors {
		expectedTxes = 2
	}

	sweepTxns, err := waitForNTxsInMempool(
		net.Miner.Node, expectedTxes, minerMempoolTimeout,
	)
	if err != nil {
		t.Fatalf("failed to find commitment in miner mempool: %v", err)
	}

	// Find alice's commit sweep and anchor sweep (if present) in the
	// mempool.
	aliceCloseTx := waitingClose.Commitments.LocalTxid
	_, aliceAnchor := findCommitAndAnchor(
		t, net, sweepTxns, aliceCloseTx,
	)

	// If we expect anchors, add alice's anchor to our expected set of
	// reports.
	if channelType == commitTypeAnchors {
		aliceReports[aliceAnchor.OutPoint.String()] = &lnrpc.Resolution{
			ResolutionType: lnrpc.ResolutionType_ANCHOR,
			Outcome:        lnrpc.ResolutionOutcome_CLAIMED,
			SweepTxid:      aliceAnchor.SweepTx,
			Outpoint: &lnrpc.OutPoint{
				TxidBytes:   aliceAnchor.OutPoint.Hash[:],
				TxidStr:     aliceAnchor.OutPoint.Hash.String(),
				OutputIndex: aliceAnchor.OutPoint.Index,
			},
			AmountAtoms: uint64(anchorSize),
		}
	}

	if _, err := net.Generate(1); err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	// Now that the commitment has been confirmed, the channel should be
	// marked as force closed.
	err = wait.NoError(func() error {
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		pendingChanResp, err := alice.PendingChannels(
			ctxt, pendingChansRequest,
		)
		if err != nil {
			return fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
		}

		err = checkNumForceClosedChannels(pendingChanResp, 1)
		if err != nil {
			return err
		}

		forceClose, err := findForceClosedChannel(pendingChanResp, &op)
		if err != nil {
			return err
		}

		// Now that the channel has been force closed, it should now
		// have the height and number of blocks to confirm populated.
		err = checkCommitmentMaturity(
			forceClose, commCsvMaturityHeight, int32(defaultCSV),
		)
		if err != nil {
			return err
		}

		// None of our outputs have been swept, so they should all be in
		// limbo. For anchors, we expect the anchor amount to be
		// recovered.
		if forceClose.LimboBalance == 0 {
			return errors.New("all funds should still be in " +
				"limbo")
		}
		expectedRecoveredBalance := int64(0)
		if channelType == commitTypeAnchors {
			expectedRecoveredBalance = anchorSize
		}
		if forceClose.RecoveredBalance != expectedRecoveredBalance {
			return errors.New("no funds should yet be shown " +
				"as recovered")
		}

		return nil
	}, 15*time.Second)
	if err != nil {
		t.Fatalf(predErr.Error())
	}

	// The following restart is intended to ensure that outputs from the
	// force close commitment transaction have been persisted once the
	// transaction has been confirmed, but before the outputs are spendable
	// (the "kindergarten" bucket.)
	if err := net.RestartNode(alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	if channelType == commitTypeAnchors {
		// Note(decred): in lnd due to a large difference in fees, two
		// transactions are expected at this point: one sweeping the
		// commitment output and one the anchor output. In decred,
		// since fees are all using the default relay fee only a single
		// transaction sweeping both outputs is expected.
		expectedTxes = 1
	}

	// Carol's sweep tx should be in the mempool already, as her output is
	// not timelocked. If there are anchors, we also expect Carol's anchor
	// sweep now.
	sweepTxns, err = waitForNTxsInMempool(
		net.Miner.Node, expectedTxes, minerMempoolTimeout,
	)
	if err != nil {
		t.Fatalf("failed to find Carol's sweep in miner mempool: %v",
			err)
	}

	// We look up the sweep txns we have found in mempool and create
	// expected resolutions for carol.
	carolCommit, carolAnchor := findCommitAndAnchor(
		t, net, sweepTxns, aliceCloseTx,
	)

	// If we have anchors, add an anchor resolution for carol.
	if channelType == commitTypeAnchors {
		carolReports[carolAnchor.OutPoint.String()] = &lnrpc.Resolution{
			ResolutionType: lnrpc.ResolutionType_ANCHOR,
			Outcome:        lnrpc.ResolutionOutcome_CLAIMED,
			SweepTxid:      carolAnchor.SweepTx,
			AmountAtoms:    anchorSize,
			Outpoint: &lnrpc.OutPoint{
				TxidBytes:   carolAnchor.OutPoint.Hash[:],
				TxidStr:     carolAnchor.OutPoint.Hash.String(),
				OutputIndex: carolAnchor.OutPoint.Index,
			},
		}
	}

	// Currently within the codebase, the default CSV is 4 relative blocks.
	// For the persistence test, we generate two blocks, then trigger
	// a restart and then generate the final block that should trigger
	// the creation of the sweep transaction.
	if _, err := net.Generate(defaultCSV - 2); err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}

	// The following restart checks to ensure that outputs in the
	// kindergarten bucket are persisted while waiting for the required
	// number of confirmations to be reported.
	if err := net.RestartNode(alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Alice should see the channel in her set of pending force closed
	// channels with her funds still in limbo.
	var aliceBalance int64
	err = wait.NoError(func() error {
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		pendingChanResp, err := alice.PendingChannels(
			ctxt, pendingChansRequest,
		)
		if err != nil {
			return fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
		}

		err = checkNumForceClosedChannels(pendingChanResp, 1)
		if err != nil {
			return err
		}

		forceClose, err := findForceClosedChannel(
			pendingChanResp, &op,
		)
		if err != nil {
			return err
		}

		// Make a record of the balances we expect for alice and carol.
		aliceBalance = forceClose.Channel.LocalBalance

		// At this point, the nursery should show that the commitment
		// output has 2 block left before its CSV delay expires. In
		// total, we have mined exactly defaultCSV blocks, so the htlc
		// outputs should also reflect that this many blocks have
		// passed.
		err = checkCommitmentMaturity(
			forceClose, commCsvMaturityHeight, 2,
		)
		if err != nil {
			return err
		}

		// All funds should still be shown in limbo.
		if forceClose.LimboBalance == 0 {
			return errors.New("all funds should still be in " +
				"limbo")
		}
		expectedRecoveredBalance := int64(0)
		if channelType == commitTypeAnchors {
			expectedRecoveredBalance = anchorSize
		}
		if forceClose.RecoveredBalance != expectedRecoveredBalance {
			return errors.New("no funds should yet be shown " +
				"as recovered")
		}

		return nil
	}, 15*time.Second)
	if err != nil {
		t.Fatalf(err.Error())
	}

	// Generate an additional block, which should cause the CSV delayed
	// output from the commitment txn to expire.
	if _, err := net.Generate(1); err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}

	// At this point, the CSV will expire in the next block, meaning that
	// the sweeping transaction should now be broadcast. So we fetch the
	// node's mempool to ensure it has been properly broadcast.
	sweepingTXID, err := waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("failed to get sweep tx from mempool: %v", err)
	}

	// Fetch the sweep transaction, all input it's spending should be from
	// the commitment transaction which was broadcast on-chain.
	sweepTx, err := net.Miner.Node.GetRawTransaction(context.Background(), sweepingTXID)
	if err != nil {
		t.Fatalf("unable to fetch sweep tx: %v", err)
	}
	for _, txIn := range sweepTx.MsgTx().TxIn {
		if !closingTxID.IsEqual(&txIn.PreviousOutPoint.Hash) {
			t.Fatalf("sweep transaction not spending from commit "+
				"tx %v, instead spending %v",
				closingTxID, txIn.PreviousOutPoint)
		}
	}

	// We expect a resolution which spends our commit output.
	output := sweepTx.MsgTx().TxIn[0].PreviousOutPoint
	aliceReports[output.String()] = &lnrpc.Resolution{
		ResolutionType: lnrpc.ResolutionType_COMMIT,
		Outcome:        lnrpc.ResolutionOutcome_CLAIMED,
		SweepTxid:      sweepingTXID.String(),
		Outpoint: &lnrpc.OutPoint{
			TxidBytes:   output.Hash[:],
			TxidStr:     output.Hash.String(),
			OutputIndex: output.Index,
		},
		AmountAtoms: uint64(aliceBalance),
	}

	carolReports[carolCommit.OutPoint.String()] = &lnrpc.Resolution{
		ResolutionType: lnrpc.ResolutionType_COMMIT,
		Outcome:        lnrpc.ResolutionOutcome_CLAIMED,
		Outpoint: &lnrpc.OutPoint{
			TxidBytes:   carolCommit.OutPoint.Hash[:],
			TxidStr:     carolCommit.OutPoint.Hash.String(),
			OutputIndex: carolCommit.OutPoint.Index,
		},
		AmountAtoms: uint64(pushAmt),
		SweepTxid:   carolCommit.SweepTx,
	}

	// Check that we can find the commitment sweep in our set of known
	// sweeps.
	err = findSweep(ctxb, alice, sweepingTXID)
	if err != nil {
		t.Fatalf("csv sweep not found: %v", err)
	}

	// Restart Alice to ensure that she resumes watching the finalized
	// commitment sweep txid.
	if err := net.RestartNode(alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Next, we mine an additional block which should include the sweep
	// transaction as the input scripts and the sequence locks on the
	// inputs should be properly met.
	blockHash, err := net.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}
	block, err := net.Miner.Node.GetBlock(context.Background(), blockHash[0])
	if err != nil {
		t.Fatalf("unable to get block: %v", err)
	}

	assertTxInBlock(t, block, sweepTx.Hash())

	// Update current height
	_, curHeight, err = net.Miner.Node.GetBestBlock(context.Background())
	if err != nil {
		t.Fatalf("unable to get best block height")
	}

	err = wait.Predicate(func() bool {
		// Now that the commit output has been fully swept, check to see
		// that the channel remains open for the pending htlc outputs.
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		pendingChanResp, err := alice.PendingChannels(
			ctxt, pendingChansRequest,
		)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}

		err = checkNumForceClosedChannels(pendingChanResp, 1)
		if err != nil {
			predErr = err
			return false
		}

		// The commitment funds will have been recovered after the
		// commit txn was included in the last block. The htlc funds
		// will be shown in limbo.
		forceClose, err := findForceClosedChannel(pendingChanResp, &op)
		if err != nil {
			predErr = err
			return false
		}
		predErr = checkPendingChannelNumHtlcs(forceClose, numInvoices)
		if predErr != nil {
			return false
		}
		predErr = checkPendingHtlcStageAndMaturity(
			forceClose, 1, htlcExpiryHeight,
			int32(htlcExpiryHeight-uint32(curHeight)),
		)
		if predErr != nil {
			return false
		}
		if forceClose.LimboBalance == 0 {
			predErr = fmt.Errorf("expected funds in limbo, found 0")
			return false
		}

		return true
	}, 15*time.Second)
	if err != nil {
		t.Fatalf(predErr.Error())
	}

	// Compute the height preceding that which will cause the htlc CLTV
	// timeouts will expire. The outputs entered at the same height as the
	// output spending from the commitment txn, so we must deduct the number
	// of blocks we have generated since adding it to the nursery, and take
	// an additional block off so that we end up one block shy of the expiry
	// height, and add the block padding.
	cltvHeightDelta := padCLTV(defaultCLTV - defaultCSV - 1 - 1)

	// Advance the blockchain until just before the CLTV expires, nothing
	// exciting should have happened during this time.
	_, err = net.Generate(cltvHeightDelta)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	// We now restart Alice, to ensure that she will broadcast the presigned
	// htlc timeout txns after the delay expires after experiencing a while
	// waiting for the htlc outputs to incubate.
	if err := net.RestartNode(alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Alice should now see the channel in her set of pending force closed
	// channels with one pending HTLC.
	err = wait.NoError(func() error {
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		pendingChanResp, err := alice.PendingChannels(
			ctxt, pendingChansRequest,
		)
		if err != nil {
			return fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
		}

		err = checkNumForceClosedChannels(pendingChanResp, 1)
		if err != nil {
			return err
		}

		forceClose, err := findForceClosedChannel(
			pendingChanResp, &op,
		)
		if err != nil {
			return err
		}

		// We should now be at the block just before the utxo nursery
		// will attempt to broadcast the htlc timeout transactions.
		err = checkPendingChannelNumHtlcs(forceClose, numInvoices)
		if err != nil {
			return err
		}
		err = checkPendingHtlcStageAndMaturity(
			forceClose, 1, htlcExpiryHeight, 1,
		)
		if err != nil {
			return err
		}

		// Now that our commitment confirmation depth has been
		// surpassed, we should now see a non-zero recovered balance.
		// All htlc outputs are still left in limbo, so it should be
		// non-zero as well.
		if forceClose.LimboBalance == 0 {
			return errors.New("htlc funds should still be in " +
				"limbo")
		}

		return nil
	}, 15*time.Second)
	if err != nil {
		t.Fatalf(err.Error())
	}

	// Now, generate the block which will cause Alice to broadcast the
	// presigned htlc timeout txns.
	_, err = net.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	// Since Alice had numInvoices (6) htlcs extended to Carol before force
	// closing, we expect Alice to broadcast an htlc timeout txn for each
	// one. Wait for them all to show up in the mempool.
	htlcTxIDs, err := waitForNTxsInMempool(net.Miner.Node, numInvoices,
		minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find htlc timeout txns in mempool: %v", err)
	}

	// Retrieve each htlc timeout txn from the mempool, and ensure it is
	// well-formed. This entails verifying that each only spends from
	// output, and that that output is from the commitment txn. We do not
	// the sweeper check for these timeout transactions because they are
	// not swept by the sweeper; the nursery broadcasts the pre-signed
	// transaction.
	var htlcLessFees uint64
	for _, htlcTxID := range htlcTxIDs {
		// Fetch the sweep transaction, all input it's spending should
		// be from the commitment transaction which was broadcast
		// on-chain.
		htlcTx, err := net.Miner.Node.GetRawTransaction(context.Background(), htlcTxID)
		if err != nil {
			t.Fatalf("unable to fetch sweep tx: %v", err)
		}
		// Ensure the htlc transaction only has one input.
		inputs := htlcTx.MsgTx().TxIn
		if len(inputs) != 1 {
			t.Fatalf("htlc transaction should only have one txin, "+
				"has %d", len(htlcTx.MsgTx().TxIn))
		}
		// Ensure the htlc transaction is spending from the commitment
		// transaction.
		txIn := inputs[0]
		if !closingTxID.IsEqual(&txIn.PreviousOutPoint.Hash) {
			t.Fatalf("htlc transaction not spending from commit "+
				"tx %v, instead spending %v",
				closingTxID, txIn.PreviousOutPoint)
		}

		outputs := htlcTx.MsgTx().TxOut
		if len(outputs) != 1 {
			t.Fatalf("htlc transaction should only have one "+
				"txout, has: %v", len(outputs))
		}

		// For each htlc timeout transaction, we expect a resolver
		// report recording this on chain resolution for both alice and
		// carol.
		outpoint := txIn.PreviousOutPoint
		resolutionOutpoint := &lnrpc.OutPoint{
			TxidBytes:   outpoint.Hash[:],
			TxidStr:     outpoint.Hash.String(),
			OutputIndex: outpoint.Index,
		}

		// We expect alice to have a timeout tx resolution with an
		// amount equal to the payment amount.
		aliceReports[outpoint.String()] = &lnrpc.Resolution{
			ResolutionType: lnrpc.ResolutionType_OUTGOING_HTLC,
			Outcome:        lnrpc.ResolutionOutcome_FIRST_STAGE,
			SweepTxid:      htlcTx.Hash().String(),
			Outpoint:       resolutionOutpoint,
			AmountAtoms:    uint64(paymentAmt),
		}

		// We expect carol to have a resolution with an incoming htlc
		// timeout which reflects the full amount of the htlc. It has
		// no spend tx, because carol stops monitoring the htlc once
		// it has timed out.
		carolReports[outpoint.String()] = &lnrpc.Resolution{
			ResolutionType: lnrpc.ResolutionType_INCOMING_HTLC,
			Outcome:        lnrpc.ResolutionOutcome_TIMEOUT,
			SweepTxid:      "",
			Outpoint:       resolutionOutpoint,
			AmountAtoms:    uint64(paymentAmt),
		}

		// We record the htlc amount less fees here, so that we know
		// what value to expect for the second stage of our htlc
		// htlc resolution.
		htlcLessFees = uint64(outputs[0].Value)
	}

	// With the htlc timeout txns still in the mempool, we restart Alice to
	// verify that she can resume watching the htlc txns she broadcasted
	// before crashing.
	if err := net.RestartNode(alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Generate a block that mines the htlc timeout txns. Doing so now
	// activates the 2nd-stage CSV delayed outputs.
	_, err = net.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	// Alice is restarted here to ensure that she promptly moved the crib
	// outputs to the kindergarten bucket after the htlc timeout txns were
	// confirmed.
	if err := net.RestartNode(alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Advance the chain until just before the 2nd-layer CSV delays expire.
	_, err = net.Generate(defaultCSV - 1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	// Restart Alice to ensure that she can recover from a failure before
	// having graduated the htlc outputs in the kindergarten bucket.
	if err := net.RestartNode(alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Now that the channel has been fully swept, it should no longer show
	// incubated, check to see that Alice's node still reports the channel
	// as pending force closed.
	err = wait.Predicate(func() bool {
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		pendingChanResp, err = alice.PendingChannels(
			ctxt, pendingChansRequest,
		)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}
		err = checkNumForceClosedChannels(pendingChanResp, 1)
		if err != nil {
			predErr = err
			return false
		}

		forceClose, err := findForceClosedChannel(pendingChanResp, &op)
		if err != nil {
			predErr = err
			return false
		}

		if forceClose.LimboBalance == 0 {
			predErr = fmt.Errorf("htlc funds should still be in limbo")
			return false
		}

		predErr = checkPendingChannelNumHtlcs(forceClose, numInvoices)
		return predErr == nil
	}, 15*time.Second)
	if err != nil {
		t.Fatalf(predErr.Error())
	}

	// Generate a block that causes Alice to sweep the htlc outputs in the
	// kindergarten bucket.
	_, err = net.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	// Wait for the single sweep txn to appear in the mempool.
	htlcSweepTxID, err := waitForTxInMempool(
		net.Miner.Node, minerMempoolTimeout,
	)
	if err != nil {
		t.Fatalf("failed to get sweep tx from mempool: %v", err)
	}

	// Construct a map of the already confirmed htlc timeout txids, that
	// will count the number of times each is spent by the sweep txn. We
	// prepopulate it in this way so that we can later detect if we are
	// spending from an output that was not a confirmed htlc timeout txn.
	var htlcTxIDSet = make(map[chainhash.Hash]int)
	for _, htlcTxID := range htlcTxIDs {
		htlcTxIDSet[*htlcTxID] = 0
	}

	// Fetch the htlc sweep transaction from the mempool.
	htlcSweepTx, err := net.Miner.Node.GetRawTransaction(context.Background(), htlcSweepTxID)
	if err != nil {
		t.Fatalf("unable to fetch sweep tx: %v", err)
	}
	// Ensure the htlc sweep transaction only has one input for each htlc
	// Alice extended before force closing.
	if len(htlcSweepTx.MsgTx().TxIn) != numInvoices {
		t.Fatalf("htlc transaction should have %d txin, "+
			"has %d", numInvoices, len(htlcSweepTx.MsgTx().TxIn))
	}
	outputCount := len(htlcSweepTx.MsgTx().TxOut)
	if outputCount != 1 {
		t.Fatalf("htlc sweep transaction should have one output, has: "+
			"%v", outputCount)
	}

	// Ensure that each output spends from exactly one htlc timeout txn.
	for _, txIn := range htlcSweepTx.MsgTx().TxIn {
		outpoint := txIn.PreviousOutPoint.Hash
		// Check that the input is a confirmed htlc timeout txn.
		if _, ok := htlcTxIDSet[outpoint]; !ok {
			t.Fatalf("htlc sweep output not spending from htlc "+
				"tx, instead spending output %v", outpoint)
		}
		// Increment our count for how many times this output was spent.
		htlcTxIDSet[outpoint]++

		// Check that each is only spent once.
		if htlcTxIDSet[outpoint] > 1 {
			t.Fatalf("htlc sweep tx has multiple spends from "+
				"outpoint %v", outpoint)
		}

		// Since we have now swept our htlc timeout tx, we expect to
		// have timeout resolutions for each of our htlcs.
		output := txIn.PreviousOutPoint
		aliceReports[output.String()] = &lnrpc.Resolution{
			ResolutionType: lnrpc.ResolutionType_OUTGOING_HTLC,
			Outcome:        lnrpc.ResolutionOutcome_TIMEOUT,
			SweepTxid:      htlcSweepTx.Hash().String(),
			Outpoint: &lnrpc.OutPoint{
				TxidBytes:   output.Hash[:],
				TxidStr:     output.Hash.String(),
				OutputIndex: output.Index,
			},
			AmountAtoms: uint64(htlcLessFees),
		}
	}

	// Check that we can find the htlc sweep in our set of sweeps.
	err = findSweep(ctxb, alice, htlcSweepTx.Hash())
	if err != nil {
		t.Fatalf("htlc sweep not found: %v", err)
	}

	// The following restart checks to ensure that the nursery store is
	// storing the txid of the previously broadcast htlc sweep txn, and that
	// it begins watching that txid after restarting.
	if err := net.RestartNode(alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Now that the channel has been fully swept, it should no longer show
	// incubated, check to see that Alice's node still reports the channel
	// as pending force closed.
	err = wait.Predicate(func() bool {
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		pendingChanResp, err := alice.PendingChannels(
			ctxt, pendingChansRequest,
		)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}
		err = checkNumForceClosedChannels(pendingChanResp, 1)
		if err != nil {
			predErr = err
			return false
		}

		// All htlcs should show zero blocks until maturity, as
		// evidenced by having checked the sweep transaction in the
		// mempool.
		forceClose, err := findForceClosedChannel(pendingChanResp, &op)
		if err != nil {
			predErr = err
			return false
		}
		predErr = checkPendingChannelNumHtlcs(forceClose, numInvoices)
		if predErr != nil {
			return false
		}
		err = checkPendingHtlcStageAndMaturity(
			forceClose, 2, htlcCsvMaturityHeight, 0,
		)
		if err != nil {
			predErr = err
			return false
		}

		return true
	}, 15*time.Second)
	if err != nil {
		t.Fatalf(predErr.Error())
	}

	// Generate the final block that sweeps all htlc funds into the user's
	// wallet, and make sure the sweep is in this block.
	block = mineBlocks(t, net, 1, 1)[0]
	assertTxInBlock(t, block, htlcSweepTxID)

	// Now that the channel has been fully swept, it should no longer show
	// up within the pending channels RPC.
	err = wait.Predicate(func() bool {
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		pendingChanResp, err := alice.PendingChannels(
			ctxt, pendingChansRequest,
		)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}

		predErr = checkNumForceClosedChannels(pendingChanResp, 0)
		if predErr != nil {
			return false
		}

		// In addition to there being no pending channels, we verify
		// that pending channels does not report any money still in
		// limbo.
		if pendingChanResp.TotalLimboBalance != 0 {
			predErr = errors.New("no user funds should be left " +
				"in limbo after incubation")
			return false
		}

		return true
	}, 15*time.Second)
	if err != nil {
		t.Fatalf(predErr.Error())
	}

	// At this point, Bob should now be aware of his new immediately
	// spendable on-chain balance, as it was Alice who broadcast the
	// commitment transaction.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	carolBalResp, err = net.Bob.WalletBalance(ctxt, carolBalReq)
	if err != nil {
		t.Fatalf("unable to get carol's balance: %v", err)
	}
	carolExpectedBalance := dcrutil.Amount(carolStartingBalance) + pushAmt
	if dcrutil.Amount(carolBalResp.ConfirmedBalance) < carolExpectedBalance {
		t.Fatalf("carol's balance is incorrect: expected %v got %v",
			carolExpectedBalance,
			carolBalResp.ConfirmedBalance)
	}

	// Finally, we check that alice and carol have the set of resolutions
	// we expect.
	assertReports(ctxb, t, alice, op, aliceReports)
	assertReports(ctxb, t, carol, op, carolReports)
}

type sweptOutput struct {
	OutPoint wire.OutPoint
	SweepTx  string
}

// findCommitAndAnchor looks for a commitment sweep and anchor sweep in the
// mempool. Our anchor output is identified by having multiple inputs, because
// we have to bring another input to add fees to the anchor. Note that the
// anchor swept output may be nil if the channel did not have anchors.
func findCommitAndAnchor(t *harnessTest, net *lntest.NetworkHarness,
	sweepTxns []*chainhash.Hash, closeTx string) (*sweptOutput, *sweptOutput) {

	var commitSweep, anchorSweep *sweptOutput
	ctxb := context.Background()

	for _, tx := range sweepTxns {
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		sweepTx, err := net.Miner.Node.GetRawTransaction(ctxt, tx)
		require.NoError(t.t, err)

		// For anchor detection in dcrlnd we do a slightly different
		// procedure because CPFP isn't fully integrated and due to fee
		// differences, anchor outputs behave slightly different in
		// some sweep situations (they end up swept along with the
		// commitment output).
		inputs := sweepTx.MsgTx().TxIn
		for _, txin := range inputs {
			// If the output sweeps from the close tx and its
			// ValueIn has the size of an anchor output, then it's
			// sweeping the anchor output.
			//
			// Otherwise, if the input spends from the close tx,
			// then this is sweeping the commitment output.
			//
			// Note: this logic is slighly broken in that it
			// _could_ also be sweeping an htlc, but this function
			// is only called in situations where the sweepTxs has
			// commitment or anchor sweeping txs.
			if txin.ValueIn == anchorSize && txin.PreviousOutPoint.Hash.String() == closeTx {
				anchorSweep = &sweptOutput{
					OutPoint: txin.PreviousOutPoint,
					SweepTx:  tx.String(),
				}
			} else if txin.PreviousOutPoint.Hash.String() == closeTx {
				commitSweep = &sweptOutput{
					OutPoint: txin.PreviousOutPoint,
					SweepTx:  tx.String(),
				}
			}
		}
	}

	return commitSweep, anchorSweep
}

// assertReports checks that the count of resolutions we have present per
// type matches a set of expected resolutions.
func assertReports(ctxb context.Context, t *harnessTest,
	node *lntest.HarnessNode, channelPoint wire.OutPoint,
	expected map[string]*lnrpc.Resolution) {

	// Get our node's closed channels.
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	closed, err := node.ClosedChannels(
		ctxt, &lnrpc.ClosedChannelsRequest{},
	)
	require.NoError(t.t, err)

	var resolutions []*lnrpc.Resolution
	for _, close := range closed.Channels {
		if close.ChannelPoint == channelPoint.String() {
			resolutions = close.Resolutions
			break
		}
	}

	require.NotNil(t.t, resolutions)
	require.Equal(t.t, len(expected), len(resolutions))

	for _, res := range resolutions {
		outPointStr := fmt.Sprintf("%v:%v", res.Outpoint.TxidStr,
			res.Outpoint.OutputIndex)

		expected, ok := expected[outPointStr]
		require.True(t.t, ok)
		require.Equal(t.t, expected, res)
	}
}

// findSweep looks up a sweep in a nodes list of broadcast sweeps.
func findSweep(ctx context.Context, node *lntest.HarnessNode,
	sweep *chainhash.Hash) error {

	// List all sweeps that alice's node had broadcast.
	ctx, _ = context.WithTimeout(ctx, defaultTimeout)
	sweepResp, err := node.WalletKitClient.ListSweeps(
		ctx, &walletrpc.ListSweepsRequest{
			Verbose: false,
		})
	if err != nil {
		return fmt.Errorf("list sweeps error: %v", err)
	}

	sweepTxIDs, ok := sweepResp.Sweeps.(*walletrpc.ListSweepsResponse_TransactionIds)
	if !ok {
		return errors.New("expected sweep txids in response")
	}

	// Check that the sweep tx we have just produced is present.
	for _, tx := range sweepTxIDs.TransactionIds.TransactionIds {
		if tx == sweep.String() {
			return nil
		}
	}

	return fmt.Errorf("sweep: %v not found", sweep.String())
}

// assertAmountSent generates a closure which queries listchannels for sndr and
// rcvr, and asserts that sndr sent amt satoshis, and that rcvr received amt
// satoshis.
//
// NOTE: This method assumes that each node only has one channel, and it is the
// channel used to send the payment.
func assertAmountSent(amt dcrutil.Amount, sndr, rcvr *lntest.HarnessNode) func() error {
	return func() error {
		// Both channels should also have properly accounted from the
		// amount that has been sent/received over the channel.
		listReq := &lnrpc.ListChannelsRequest{}
		ctxb := context.Background()
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		sndrListChannels, err := sndr.ListChannels(ctxt, listReq)
		if err != nil {
			return fmt.Errorf("unable to query for %s's channel "+
				"list: %v", sndr.Name(), err)
		}
		sndrSatoshisSent := sndrListChannels.Channels[0].TotalAtomsSent
		if sndrSatoshisSent != int64(amt) {
			return fmt.Errorf("%s's atoms sent is incorrect "+
				"got %v, expected %v", sndr.Name(),
				sndrSatoshisSent, amt)
		}

		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		rcvrListChannels, err := rcvr.ListChannels(ctxt, listReq)
		if err != nil {
			return fmt.Errorf("unable to query for %s's channel "+
				"list: %v", rcvr.Name(), err)
		}
		rcvrSatoshisReceived := rcvrListChannels.Channels[0].TotalAtomsReceived
		if rcvrSatoshisReceived != int64(amt) {
			return fmt.Errorf("%s's atoms received is "+
				"incorrect got %v, expected %v", rcvr.Name(),
				rcvrSatoshisReceived, amt)
		}

		return nil
	}
}

// assertLastHTLCError checks that the last sent HTLC of the last payment sent
// by the given node failed with the expected failure code.
func assertLastHTLCError(t *harnessTest, node *lntest.HarnessNode,
	code lnrpc.Failure_FailureCode) {

	req := &lnrpc.ListPaymentsRequest{
		IncludeIncomplete: true,
	}
	ctxt, _ := context.WithTimeout(context.Background(), defaultTimeout)
	paymentsResp, err := node.ListPayments(ctxt, req)
	if err != nil {
		t.Fatalf("error when obtaining payments: %v", err)
	}

	payments := paymentsResp.Payments
	if len(payments) == 0 {
		t.Fatalf("no payments found")
	}

	payment := payments[len(payments)-1]
	htlcs := payment.Htlcs
	if len(htlcs) == 0 {
		t.Fatalf("no htlcs")
	}

	htlc := htlcs[len(htlcs)-1]
	if htlc.Failure == nil {
		t.Fatalf("expected failure")
	}

	if htlc.Failure.Code != code {
		t.Fatalf("expected failure %v, got %v", code, htlc.Failure.Code)
	}
}

// testSphinxReplayPersistence verifies that replayed onion packets are rejected
// by a remote peer after a restart. We use a combination of unsafe
// configuration arguments to force Carol to replay the same sphinx packet after
// reconnecting to Dave, and compare the returned failure message with what we
// expect for replayed onion packets.
func testSphinxReplayPersistence(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// Open a channel with 100k atoms between Carol and Dave with Carol being
	// the sole funder of the channel.
	chanAmt := dcrutil.Amount(100000)

	// First, we'll create Dave, the receiver, and start him in hodl mode.
	dave, err := net.NewNode("Dave", []string{"--hodl.exit-settle"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}

	// We must remember to shutdown the nodes we created for the duration
	// of the tests, only leaving the two seed nodes (Alice and Bob) within
	// our test network.
	defer shutdownAndAssert(net, t, dave)

	// Next, we'll create Carol and establish a channel to from her to
	// Dave. Carol is started in both unsafe-replay which will cause her to
	// replay any pending Adds held in memory upon reconnection.
	carol, err := net.NewNode("Carol", []string{"--unsafe-replay"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, carol, dave); err != nil {
		t.Fatalf("unable to connect carol to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, carol, dave,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Next, we'll create Fred who is going to initiate the payment and
	// establish a channel to from him to Carol. We can't perform this test
	// by paying from Carol directly to Dave, because the '--unsafe-replay'
	// setup doesn't apply to locally added htlcs. In that case, the
	// mailbox, that is responsible for generating the replay, is bypassed.
	fred, err := net.NewNode("Fred", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, fred)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, fred, carol); err != nil {
		t.Fatalf("unable to connect fred to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, fred)
	if err != nil {
		t.Fatalf("unable to send coins to fred: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointFC := openChannelAndAssert(
		ctxt, t, net, fred, carol,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Now that the channel is open, create an invoice for Dave which
	// expects a payment of 1000 atoms from Carol paid via a particular
	// preimage.
	const paymentAmt = 1000
	preimage := bytes.Repeat([]byte("A"), 32)
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: preimage,
		Value:     paymentAmt,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	invoiceResp, err := dave.AddInvoice(ctxt, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Wait for all channels to be recognized and advertized.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't advertise channel before "+
			"timeout: %v", err)
	}
	err = dave.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("bob didn't advertise channel before "+
			"timeout: %v", err)
	}
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointFC)
	if err != nil {
		t.Fatalf("alice didn't advertise channel before "+
			"timeout: %v", err)
	}
	err = fred.WaitForNetworkChannelOpen(ctxt, chanPointFC)
	if err != nil {
		t.Fatalf("bob didn't advertise channel before "+
			"timeout: %v", err)
	}

	// With the invoice for Dave added, send a payment from Fred paying
	// to the above generated invoice.
	ctx, cancel := context.WithCancel(ctxb)
	defer cancel()

	payStream, err := fred.RouterClient.SendPaymentV2(
		ctx,
		&routerrpc.SendPaymentRequest{
			PaymentRequest: invoiceResp.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMAtoms: noFeeLimitMAtoms,
		},
	)
	if err != nil {
		t.Fatalf("unable to open payment stream: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	// Dave's invoice should not be marked as settled.
	payHash := &lnrpc.PaymentHash{
		RHash: invoiceResp.RHash,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	dbInvoice, err := dave.LookupInvoice(ctxt, payHash)
	if err != nil {
		t.Fatalf("unable to lookup invoice: %v", err)
	}
	if dbInvoice.Settled {
		t.Fatalf("dave's invoice should not be marked as settled: %v",
			spew.Sdump(dbInvoice))
	}

	// With the payment sent but hedl, all balance related stats should not
	// have changed.
	err = wait.InvariantNoError(
		assertAmountSent(0, carol, dave), 3*time.Second,
	)
	if err != nil {
		t.Fatalf(err.Error())
	}

	// With the first payment sent, restart dave to make sure he is
	// persisting the information required to detect replayed sphinx
	// packets.
	if err := net.RestartNode(dave, nil); err != nil {
		t.Fatalf("unable to restart dave: %v", err)
	}

	// Carol should retransmit the Add hedl in her mailbox on startup. Dave
	// should not accept the replayed Add, and actually fail back the
	// pending payment. Even though he still holds the original settle, if
	// he does fail, it is almost certainly caused by the sphinx replay
	// protection, as it is the only validation we do in hodl mode.
	result, err := getPaymentResult(payStream)
	if err != nil {
		t.Fatalf("unable to receive payment response: %v", err)
	}

	// Assert that Fred receives the expected failure after Carol sent a
	// duplicate packet that fails due to sphinx replay detection.
	if result.Status == lnrpc.Payment_SUCCEEDED {
		t.Fatalf("expected payment error")
	}
	assertLastHTLCError(t, fred, lnrpc.Failure_INVALID_ONION_KEY)

	// Since the payment failed, the balance should still be left
	// unaltered.
	err = wait.InvariantNoError(
		assertAmountSent(0, carol, dave), 3*time.Second,
	)
	if err != nil {
		t.Fatalf(err.Error())
	}

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPoint, true)

	// Cleanup by mining the force close and sweep transaction.
	cleanupForceClose(t, net, carol, chanPoint)
}

func assertChannelConstraintsEqual(
	t *harnessTest, want, got *lnrpc.ChannelConstraints) {

	t.t.Helper()

	if want.CsvDelay != got.CsvDelay {
		t.Fatalf("CsvDelay mismatched, want: %v, got: %v",
			want.CsvDelay, got.CsvDelay,
		)
	}

	if want.ChanReserveAtoms != got.ChanReserveAtoms {
		t.Fatalf("ChanReserveAtoms mismatched, want: %v, got: %v",
			want.ChanReserveAtoms, got.ChanReserveAtoms,
		)
	}

	if want.DustLimitAtoms != got.DustLimitAtoms {
		t.Fatalf("DustLimitAtoms mismatched, want: %v, got: %v",
			want.DustLimitAtoms, got.DustLimitAtoms,
		)
	}

	if want.MaxPendingAmtMAtoms != got.MaxPendingAmtMAtoms {
		t.Fatalf("MaxPendingAmtMAtoms mismatched, want: %v, got: %v",
			want.MaxPendingAmtMAtoms, got.MaxPendingAmtMAtoms,
		)
	}

	if want.MinHtlcMAtoms != got.MinHtlcMAtoms {
		t.Fatalf("MinHtlcMAtoms mismatched, want: %v, got: %v",
			want.MinHtlcMAtoms, got.MinHtlcMAtoms,
		)
	}

	if want.MaxAcceptedHtlcs != got.MaxAcceptedHtlcs {
		t.Fatalf("MaxAcceptedHtlcs mismatched, want: %v, got: %v",
			want.MaxAcceptedHtlcs, got.MaxAcceptedHtlcs,
		)
	}
}

// testListChannels checks that the response from ListChannels is correct. It
// tests the values in all ChannelConstraints are returned as expected. Once
// ListChannels becomes mature, a test against all fields in ListChannels should
// be performed.
func testListChannels(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const aliceRemoteMaxHtlcs = 50
	const bobRemoteMaxHtlcs = 100

	// Create two fresh nodes and open a channel between them.
	alice, err := net.NewNode("Alice", nil)
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}
	defer shutdownAndAssert(net, t, alice)

	bob, err := net.NewNode("Bob", []string{
		fmt.Sprintf("--default-remote-max-htlcs=%v", bobRemoteMaxHtlcs),
	})
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}
	defer shutdownAndAssert(net, t, bob)

	// Connect Alice to Bob.
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxb, alice, bob); err != nil {
		t.Fatalf("unable to connect alice to bob: %v", err)
	}

	// Give Alice some coins so she can fund a channel.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, alice)
	if err != nil {
		t.Fatalf("unable to send coins to alice: %v", err)
	}

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel. The minial HTLC amount is set to
	// 4200 msats.
	const customizedMinHtlc = 4200

	chanAmt := dcrutil.Amount(100000)
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, alice, bob,
		lntest.OpenChannelParams{
			Amt:            chanAmt,
			MinHtlc:        customizedMinHtlc,
			RemoteMaxHtlcs: aliceRemoteMaxHtlcs,
		},
	)

	// Wait for Alice and Bob to receive the channel edge from the
	// funding manager.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't see the alice->bob channel before "+
			"timeout: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = bob.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("bob didn't see the bob->alice channel before "+
			"timeout: %v", err)
	}

	// Alice should have one channel opened with Bob.
	assertNodeNumChannels(t, alice, 1)
	// Bob should have one channel opened with Alice.
	assertNodeNumChannels(t, bob, 1)

	// Get the ListChannel response from Alice.
	listReq := &lnrpc.ListChannelsRequest{}
	ctxb = context.Background()
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	resp, err := alice.ListChannels(ctxt, listReq)
	if err != nil {
		t.Fatalf("unable to query for %s's channel list: %v",
			alice.Name(), err)
	}

	// Check the returned response is correct.
	aliceChannel := resp.Channels[0]

	// defaultConstraints is a ChannelConstraints with default values. It is
	// used to test against Alice's local channel constraints.
	defaultConstraints := &lnrpc.ChannelConstraints{
		CsvDelay:            4,
		ChanReserveAtoms:    6030,
		DustLimitAtoms:      uint64(lnwallet.DefaultDustLimit()),
		MaxPendingAmtMAtoms: 99000000,
		MinHtlcMAtoms:       1000,
		MaxAcceptedHtlcs:    bobRemoteMaxHtlcs,
	}
	assertChannelConstraintsEqual(
		t, defaultConstraints, aliceChannel.LocalConstraints,
	)

	// customizedConstraints is a ChannelConstraints with customized values.
	// Ideally, all these values can be passed in when creating the channel.
	// Currently, only the MinHtlcMAtoms is customized. It is used to check
	// against Alice's remote channel constratins.
	customizedConstraints := &lnrpc.ChannelConstraints{
		CsvDelay:            4,
		ChanReserveAtoms:    6030,
		DustLimitAtoms:      uint64(lnwallet.DefaultDustLimit()),
		MaxPendingAmtMAtoms: 99000000,
		MinHtlcMAtoms:       customizedMinHtlc,
		MaxAcceptedHtlcs:    aliceRemoteMaxHtlcs,
	}
	assertChannelConstraintsEqual(
		t, customizedConstraints, aliceChannel.RemoteConstraints,
	)

	// Get the ListChannel response for Bob.
	listReq = &lnrpc.ListChannelsRequest{}
	ctxb = context.Background()
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	resp, err = bob.ListChannels(ctxt, listReq)
	if err != nil {
		t.Fatalf("unable to query for %s's channel "+
			"list: %v", bob.Name(), err)
	}

	bobChannel := resp.Channels[0]
	if bobChannel.ChannelPoint != aliceChannel.ChannelPoint {
		t.Fatalf("Bob's channel point mismatched, want: %s, got: %s",
			chanPoint.String(), bobChannel.ChannelPoint,
		)
	}

	// Check channel constraints match. Alice's local channel constraint should
	// be equal to Bob's remote channel constraint, and her remote one should
	// be equal to Bob's local one.
	assertChannelConstraintsEqual(
		t, aliceChannel.LocalConstraints, bobChannel.RemoteConstraints,
	)
	assertChannelConstraintsEqual(
		t, aliceChannel.RemoteConstraints, bobChannel.LocalConstraints,
	)

}

func testListPayments(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// First start by deleting all payments that Alice knows of. This will
	// allow us to execute the test with a clean state for Alice.
	delPaymentsReq := &lnrpc.DeleteAllPaymentsRequest{}
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	if _, err := net.Alice.DeleteAllPayments(ctxt, delPaymentsReq); err != nil {
		t.Fatalf("unable to delete payments: %v", err)
	}

	// Check that there are no payments before test.
	reqInit := &lnrpc.ListPaymentsRequest{}
	ctxt, _ = context.WithTimeout(ctxt, defaultTimeout)
	paymentsRespInit, err := net.Alice.ListPayments(ctxt, reqInit)
	if err != nil {
		t.Fatalf("error when obtaining Alice payments: %v", err)
	}
	if len(paymentsRespInit.Payments) != 0 {
		t.Fatalf("incorrect number of payments, got %v, want %v",
			len(paymentsRespInit.Payments), 0)
	}

	// Open a channel with 100k atoms between Alice and Bob with Alice
	// being the sole funder of the channel.
	chanAmt := dcrutil.Amount(100000)
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Now that the channel is open, create an invoice for Bob which
	// expects a payment of 20000 atoms from Alice paid via a particular
	// preimage.
	const paymentAmt = 20000
	preimage := bytes.Repeat([]byte("B"), 32)
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: preimage,
		Value:     paymentAmt,
	}
	addInvoiceCtxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	invoiceResp, err := net.Bob.AddInvoice(addInvoiceCtxt, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Wait for Alice to recognize and advertise the new channel generated
	// above.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint); err != nil {
		t.Fatalf("alice didn't advertise channel before "+
			"timeout: %v", err)
	}
	if err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint); err != nil {
		t.Fatalf("bob didn't advertise channel before "+
			"timeout: %v", err)
	}

	// With the invoice for Bob added, send a payment towards Alice paying
	// to the above generated invoice.
	sendAndAssertSuccess(
		t, net.Alice,
		&routerrpc.SendPaymentRequest{
			PaymentRequest: invoiceResp.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitAtoms:  1000000,
		},
	)

	// Grab Alice's list of payments, she should show the existence of
	// exactly one payment.
	req := &lnrpc.ListPaymentsRequest{}
	ctxt, _ = context.WithTimeout(ctxt, defaultTimeout)
	paymentsResp, err := net.Alice.ListPayments(ctxt, req)
	if err != nil {
		t.Fatalf("error when obtaining Alice payments: %v", err)
	}
	if len(paymentsResp.Payments) != 1 {
		t.Fatalf("incorrect number of payments, got %v, want %v",
			len(paymentsResp.Payments), 1)
	}
	p := paymentsResp.Payments[0]
	path := p.Htlcs[len(p.Htlcs)-1].Route.Hops

	// Ensure that the stored path shows a direct payment to Bob with no
	// other nodes in-between.
	if len(path) != 1 || path[0].PubKey != net.Bob.PubKeyStr {
		t.Fatalf("incorrect path")
	}

	// The payment amount should also match our previous payment directly.
	if p.Value != paymentAmt {
		t.Fatalf("incorrect amount, got %v, want %v",
			p.Value, paymentAmt)
	}

	// The payment hash (or r-hash) should have been stored correctly.
	correctRHash := hex.EncodeToString(invoiceResp.RHash)
	if !reflect.DeepEqual(p.PaymentHash, correctRHash) {
		t.Fatalf("incorrect RHash, got %v, want %v",
			p.PaymentHash, correctRHash)
	}

	// As we made a single-hop direct payment, there should have been no fee
	// applied.
	if p.Fee != 0 {
		t.Fatalf("incorrect Fee, got %v, want %v", p.Fee, 0)
	}

	// Finally, verify that the payment request returned by the rpc matches
	// the invoice that we paid.
	if p.PaymentRequest != invoiceResp.PaymentRequest {
		t.Fatalf("incorrect payreq, got: %v, want: %v",
			p.PaymentRequest, invoiceResp.PaymentRequest)
	}

	// Delete all payments from Alice. DB should have no payments.
	delReq := &lnrpc.DeleteAllPaymentsRequest{}
	ctxt, _ = context.WithTimeout(ctxt, defaultTimeout)
	_, err = net.Alice.DeleteAllPayments(ctxt, delReq)
	if err != nil {
		t.Fatalf("Can't delete payments at the end: %v", err)
	}

	// Check that there are no payments after test.
	listReq := &lnrpc.ListPaymentsRequest{}
	ctxt, _ = context.WithTimeout(ctxt, defaultTimeout)
	paymentsResp, err = net.Alice.ListPayments(ctxt, listReq)
	if err != nil {
		t.Fatalf("error when obtaining Alice payments: %v", err)
	}
	if len(paymentsResp.Payments) != 0 {
		t.Fatalf("incorrect number of payments, got %v, want %v",
			len(paymentsRespInit.Payments), 0)
	}

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}

// assertAmountPaid checks that the ListChannels command of the provided
// node list the total amount sent and received as expected for the
// provided channel.
func assertAmountPaid(t *harnessTest, channelName string,
	node *lntest.HarnessNode, chanPoint wire.OutPoint, amountSent,
	amountReceived int64) {
	ctxb := context.Background()

	checkAmountPaid := func() error {
		listReq := &lnrpc.ListChannelsRequest{}
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		resp, err := node.ListChannels(ctxt, listReq)
		if err != nil {
			return fmt.Errorf("unable to for node's "+
				"channels: %v", err)
		}
		for _, channel := range resp.Channels {
			if channel.ChannelPoint != chanPoint.String() {
				continue
			}

			if channel.TotalAtomsSent != amountSent {
				return fmt.Errorf("%v: incorrect amount"+
					" sent: %v != %v", channelName,
					channel.TotalAtomsSent,
					amountSent)
			}
			if channel.TotalAtomsReceived !=
				amountReceived {
				return fmt.Errorf("%v: incorrect amount"+
					" received: %v != %v",
					channelName,
					channel.TotalAtomsReceived,
					amountReceived)
			}

			return nil
		}
		return fmt.Errorf("channel not found")
	}

	// As far as HTLC inclusion in commitment transaction might be
	// postponed we will try to check the balance couple of times,
	// and then if after some period of time we receive wrong
	// balance return the error.
	// TODO(roasbeef): remove sleep after invoice notification hooks
	// are in place
	var timeover uint32
	go func() {
		<-time.After(time.Second * 20)
		atomic.StoreUint32(&timeover, 1)
	}()

	for {
		isTimeover := atomic.LoadUint32(&timeover) == 1
		if err := checkAmountPaid(); err != nil {
			if isTimeover {
				t.Fatalf("Check amount Paid failed: %v", err)
			}
		} else {
			break
		}
	}
}

// updateChannelPolicy updates the channel policy of node to the
// given fees and timelock delta. This function blocks until
// listenerNode has received the policy update.
func updateChannelPolicy(t *harnessTest, node *lntest.HarnessNode,
	chanPoint *lnrpc.ChannelPoint, baseFee int64, feeRate int64,
	timeLockDelta uint32, maxHtlc uint64, listenerNode *lntest.HarnessNode) {

	ctxb := context.Background()

	expectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMAtoms:      baseFee,
		FeeRateMilliMAtoms: feeRate,
		TimeLockDelta:      timeLockDelta,
		MinHtlc:            1000, // default value
		MaxHtlcMAtoms:      maxHtlc,
	}

	updateFeeReq := &lnrpc.PolicyUpdateRequest{
		BaseFeeMAtoms: baseFee,
		FeeRate:       float64(feeRate) / testFeeBase,
		TimeLockDelta: timeLockDelta,
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: chanPoint,
		},
		MaxHtlcMAtoms: maxHtlc,
	}

	// Create the subscription before sending the update so we're certain
	// to get it.
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	graphSub := subscribeGraphNotifications(t, ctxt, listenerNode)
	defer close(graphSub.quit)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if _, err := node.UpdateChannelPolicy(ctxt, updateFeeReq); err != nil {
		t.Fatalf("unable to update chan policy: %v", err)
	}

	// Wait for listener node to receive the channel update from node.
	waitForChannelUpdate(
		t, graphSub,
		[]expectedChanUpdate{
			{node.PubKeyStr, expectedPolicy, chanPoint},
		},
	)
}

type singleHopSendToRouteCase struct {
	name string

	// streaming tests streaming SendToRoute if true, otherwise tests
	// synchronous SenToRoute.
	streaming bool

	// routerrpc submits the request to the routerrpc subserver if true,
	// otherwise submits to the main rpc server.
	routerrpc bool

	// mpp sets the MPP fields on the request if true, otherwise submits a
	// regular payment.
	mpp bool
}

var singleHopSendToRouteCases = []singleHopSendToRouteCase{
	{
		name: "regular main sync",
	},
	{
		name:      "regular main stream",
		streaming: true,
	},
	{
		name:      "regular routerrpc sync",
		routerrpc: true,
	},
	{
		name: "mpp main sync",
		mpp:  true,
	},
	{
		name:      "mpp main stream",
		streaming: true,
		mpp:       true,
	},
	{
		name:      "mpp routerrpc sync",
		routerrpc: true,
		mpp:       true,
	},
}

// testSingleHopSendToRoute tests that payments are properly processed through a
// provided route with a single hop. We'll create the following network
// topology:
//      Carol --100k--> Dave
// We'll query the daemon for routes from Carol to Dave and then send payments
// by feeding the route back into the various SendToRoute RPC methods. Here we
// test all three SendToRoute endpoints, forcing each to perform both a regular
// payment and an MPP payment.
func testSingleHopSendToRoute(net *lntest.NetworkHarness, t *harnessTest) {
	for _, test := range singleHopSendToRouteCases {
		test := test

		ok := t.t.Run(test.name, func(t1 *testing.T) {
			ht := newHarnessTest(t1, t.lndHarness)
			ht.RunTestCase(&testCase{
				name: test.name,
				test: func(_ *lntest.NetworkHarness, tt *harnessTest) {
					testSingleHopSendToRouteCase(net, tt, test)
				},
			})
		})
		if !ok {
			break
		}
	}
}

func testSingleHopSendToRouteCase(net *lntest.NetworkHarness, t *harnessTest,
	test singleHopSendToRouteCase) {

	const chanAmt = dcrutil.Amount(100000)
	const paymentAmtAtoms = 1000
	const numPayments = 5
	const amountPaid = int64(numPayments * paymentAmtAtoms)

	ctxb := context.Background()
	var networkChans []*lnrpc.ChannelPoint

	// Create Carol and Dave, then establish a channel between them. Carol
	// is the sole funder of the channel with 100k atoms. The network
	// topology should look like: Carol -> 100k -> Dave
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	dave, err := net.NewNode("Dave", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, carol, dave); err != nil {
		t.Fatalf("unable to connect carol to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}

	// Open a channel with 100k atoms between Carol and Dave with Carol
	// being the sole funder of the channel.
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointCarol := openChannelAndAssert(
		ctxt, t, net, carol, dave,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	networkChans = append(networkChans, chanPointCarol)

	carolChanTXID, err := dcrlnd.GetChanPointFundingTxid(chanPointCarol)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	carolFundPoint := wire.OutPoint{
		Hash:  *carolChanTXID,
		Index: chanPointCarol.OutputIndex,
	}

	// Wait for all nodes to have seen all channels.
	nodes := []*lntest.HarnessNode{carol, dave}
	for _, chanPoint := range networkChans {
		for _, node := range nodes {
			txid, err := dcrlnd.GetChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
			err = node.WaitForNetworkChannelOpen(ctxt, chanPoint)
			if err != nil {
				t.Fatalf("%s(%d): timeout waiting for "+
					"channel(%s) open: %v", node.Name(),
					node.NodeID, point, err)
			}
		}
	}

	// Create invoices for Dave, which expect a payment from Carol.
	payReqs, rHashes, _, err := createPayReqs(
		dave, paymentAmtAtoms, numPayments,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// Reconstruct payment addresses.
	var payAddrs [][]byte
	for _, payReq := range payReqs {
		ctx, _ := context.WithTimeout(
			context.Background(), defaultTimeout,
		)
		resp, err := dave.DecodePayReq(
			ctx,
			&lnrpc.PayReqString{PayReq: payReq},
		)
		if err != nil {
			t.Fatalf("decode pay req: %v", err)
		}
		payAddrs = append(payAddrs, resp.PaymentAddr)
	}

	// Assert Carol and Dave are synced to the chain before proceeding, to
	// ensure the queried route will have a valid final CLTV once the HTLC
	// reaches Dave.
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	_, minerHeight, err := net.Miner.Node.GetBestBlock(ctxt)
	if err != nil {
		t.Fatalf("unable to get best height: %v", err)
	}
	assertNodeBlockHeight(t, carol, int32(minerHeight))
	assertNodeBlockHeight(t, dave, int32(minerHeight))

	// Query for routes to pay from Carol to Dave using the default CLTV
	// config.
	routesReq := &lnrpc.QueryRoutesRequest{
		PubKey: dave.PubKeyStr,
		Amt:    paymentAmtAtoms,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	routes, err := carol.QueryRoutes(ctxt, routesReq)
	if err != nil {
		t.Fatalf("unable to get route from %s: %v",
			carol.Name(), err)
	}

	// There should only be one route to try, so take the first item.
	r := routes.Routes[0]

	// Construct a closure that will set MPP fields on the route, which
	// allows us to test MPP payments.
	setMPPFields := func(i int) {
		hop := r.Hops[len(r.Hops)-1]
		hop.TlvPayload = true
		hop.MppRecord = &lnrpc.MPPRecord{
			PaymentAddr:    payAddrs[i],
			TotalAmtMAtoms: paymentAmtAtoms * 1000,
		}
	}

	// Construct closures for each of the payment types covered:
	//  - main rpc server sync
	//  - main rpc server streaming
	//  - routerrpc server sync
	sendToRouteSync := func() {
		for i, rHash := range rHashes {
			// Populate the MPP fields for the final hop if we are
			// testing MPP payments.
			if test.mpp {
				setMPPFields(i)
			}

			sendReq := &lnrpc.SendToRouteRequest{
				PaymentHash: rHash,
				Route:       r,
			}
			ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
			resp, err := carol.SendToRouteSync(
				ctxt, sendReq,
			)
			if err != nil {
				t.Fatalf("unable to send to route for "+
					"%s: %v", carol.Name(), err)
			}
			if resp.PaymentError != "" {
				t.Fatalf("received payment error from %s: %v",
					carol.Name(), resp.PaymentError)
			}
		}
	}
	sendToRouteStream := func() {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		alicePayStream, err := carol.SendToRoute(ctxt)
		if err != nil {
			t.Fatalf("unable to create payment stream for "+
				"carol: %v", err)
		}

		for i, rHash := range rHashes {
			// Populate the MPP fields for the final hop if we are
			// testing MPP payments.
			if test.mpp {
				setMPPFields(i)
			}

			sendReq := &lnrpc.SendToRouteRequest{
				PaymentHash: rHash,
				Route:       routes.Routes[0],
			}
			err := alicePayStream.Send(sendReq)

			if err != nil {
				t.Fatalf("unable to send payment: %v", err)
			}

			resp, err := alicePayStream.Recv()
			if err != nil {
				t.Fatalf("unable to send payment: %v", err)
			}
			if resp.PaymentError != "" {
				t.Fatalf("received payment error: %v",
					resp.PaymentError)
			}
		}
	}
	sendToRouteRouterRPC := func() {
		for i, rHash := range rHashes {
			// Populate the MPP fields for the final hop if we are
			// testing MPP payments.
			if test.mpp {
				setMPPFields(i)
			}

			sendReq := &routerrpc.SendToRouteRequest{
				PaymentHash: rHash,
				Route:       r,
			}
			ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
			resp, err := carol.RouterClient.SendToRouteV2(
				ctxt, sendReq,
			)
			if err != nil {
				t.Fatalf("unable to send to route for "+
					"%s: %v", carol.Name(), err)
			}
			if resp.Failure != nil {
				t.Fatalf("received payment error from %s: %v",
					carol.Name(), resp.Failure)
			}
		}
	}

	// Using Carol as the node as the source, send the payments
	// synchronously via the the routerrpc's SendToRoute, or via the main RPC
	// server's SendToRoute streaming or sync calls.
	switch {
	case !test.routerrpc && test.streaming:
		sendToRouteStream()
	case !test.routerrpc && !test.streaming:
		sendToRouteSync()
	case test.routerrpc && !test.streaming:
		sendToRouteRouterRPC()
	default:
		t.Fatalf("routerrpc does not support streaming send_to_route")
	}

	// Verify that the payment's from Carol's PoV have the correct payment
	// hash and amount.
	ctxt, _ = context.WithTimeout(ctxt, defaultTimeout)
	paymentsResp, err := carol.ListPayments(
		ctxt, &lnrpc.ListPaymentsRequest{},
	)
	if err != nil {
		t.Fatalf("error when obtaining %s payments: %v",
			carol.Name(), err)
	}
	if len(paymentsResp.Payments) != numPayments {
		t.Fatalf("incorrect number of payments, got %v, want %v",
			len(paymentsResp.Payments), numPayments)
	}

	for i, p := range paymentsResp.Payments {
		// Assert that the payment hashes for each payment match up.
		rHashHex := hex.EncodeToString(rHashes[i])
		if p.PaymentHash != rHashHex {
			t.Fatalf("incorrect payment hash for payment %d, "+
				"want: %s got: %s",
				i, rHashHex, p.PaymentHash)
		}

		// Assert that each payment has no invoice since the payment was
		// completed using SendToRoute.
		if p.PaymentRequest != "" {
			t.Fatalf("incorrect payment request for payment: %d, "+
				"want: \"\", got: %s",
				i, p.PaymentRequest)
		}

		// Assert the payment ammount is correct.
		if p.ValueAtoms != paymentAmtAtoms {
			t.Fatalf("incorrect payment amt for payment %d, "+
				"want: %d, got: %d",
				i, paymentAmtAtoms, p.ValueAtoms)
		}

		// Assert exactly one htlc was made.
		if len(p.Htlcs) != 1 {
			t.Fatalf("expected 1 htlc for payment %d, got: %d",
				i, len(p.Htlcs))
		}

		// Assert the htlc's route is populated.
		htlc := p.Htlcs[0]
		if htlc.Route == nil {
			t.Fatalf("expected route for payment %d", i)
		}

		// Assert the hop has exactly one hop.
		if len(htlc.Route.Hops) != 1 {
			t.Fatalf("expected 1 hop for payment %d, got: %d",
				i, len(htlc.Route.Hops))
		}

		// If this is an MPP test, assert the MPP record's fields are
		// properly populated. Otherwise the hop should not have an MPP
		// record.
		hop := htlc.Route.Hops[0]
		if test.mpp {
			if hop.MppRecord == nil {
				t.Fatalf("expected mpp record for mpp payment")
			}

			if hop.MppRecord.TotalAmtMAtoms != paymentAmtAtoms*1000 {
				t.Fatalf("incorrect mpp total matoms for payment %d "+
					"want: %d, got: %d",
					i, paymentAmtAtoms*1000,
					hop.MppRecord.TotalAmtMAtoms)
			}

			expAddr := payAddrs[i]
			if !bytes.Equal(hop.MppRecord.PaymentAddr, expAddr) {
				t.Fatalf("incorrect mpp payment addr for payment %d "+
					"want: %x, got: %x",
					i, expAddr, hop.MppRecord.PaymentAddr)
			}
		} else if hop.MppRecord != nil {
			t.Fatalf("unexpected mpp record for non-mpp payment")
		}
	}

	// Verify that the invoices's from Dave's PoV have the correct payment
	// hash and amount.
	ctxt, _ = context.WithTimeout(ctxt, defaultTimeout)
	invoicesResp, err := dave.ListInvoices(
		ctxt, &lnrpc.ListInvoiceRequest{},
	)
	if err != nil {
		t.Fatalf("error when obtaining %s payments: %v",
			dave.Name(), err)
	}
	if len(invoicesResp.Invoices) != numPayments {
		t.Fatalf("incorrect number of invoices, got %v, want %v",
			len(invoicesResp.Invoices), numPayments)
	}

	for i, inv := range invoicesResp.Invoices {
		// Assert that the payment hashes match up.
		if !bytes.Equal(inv.RHash, rHashes[i]) {
			t.Fatalf("incorrect payment hash for invoice %d, "+
				"want: %x got: %x",
				i, rHashes[i], inv.RHash)
		}

		// Assert that the amount paid to the invoice is correct.
		if inv.AmtPaidAtoms != paymentAmtAtoms {
			t.Fatalf("incorrect payment amt for invoice %d, "+
				"want: %d, got %d",
				i, paymentAmtAtoms, inv.AmtPaidAtoms)
		}
	}

	// At this point all the channels within our proto network should be
	// shifted by 5k atoms in the direction of Dave, the sink within the
	// payment flow generated above. The order of asserts corresponds to
	// increasing of time is needed to embed the HTLC in commitment
	// transaction, in channel Carol->Dave, order is Dave and then Carol.
	assertAmountPaid(t, "Carol(local) => Dave(remote)", dave,
		carolFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, "Carol(local) => Dave(remote)", carol,
		carolFundPoint, amountPaid, int64(0))

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarol, false)
}

// testMultiHopSendToRoute tests that payments are properly processed
// through a provided route. We'll create the following network topology:
//      Alice --100k--> Bob --100k--> Carol
// We'll query the daemon for routes from Alice to Carol and then
// send payments through the routes.
func testMultiHopSendToRoute(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const chanAmt = dcrutil.Amount(100000)
	var networkChans []*lnrpc.ChannelPoint

	// Open a channel with 100k atoms between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointAlice := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	networkChans = append(networkChans, chanPointAlice)

	aliceChanTXID, err := dcrlnd.GetChanPointFundingTxid(chanPointAlice)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// Create Carol and establish a channel from Bob. Bob is the sole funder
	// of the channel with 100k atoms. The network topology should look like:
	// Alice -> Bob -> Carol
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, carol, net.Bob); err != nil {
		t.Fatalf("unable to connect carol to alice: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, net.Bob)
	if err != nil {
		t.Fatalf("unable to send coins to bob: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointBob := openChannelAndAssert(
		ctxt, t, net, net.Bob, carol,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	networkChans = append(networkChans, chanPointBob)
	bobChanTXID, err := dcrlnd.GetChanPointFundingTxid(chanPointBob)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	bobFundPoint := wire.OutPoint{
		Hash:  *bobChanTXID,
		Index: chanPointBob.OutputIndex,
	}

	// Wait for all nodes to have seen all channels.
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol}
	nodeNames := []string{"Alice", "Bob", "Carol"}
	for _, chanPoint := range networkChans {
		for i, node := range nodes {
			txid, err := dcrlnd.GetChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
			err = node.WaitForNetworkChannelOpen(ctxt, chanPoint)
			if err != nil {
				t.Fatalf("%s(%d): timeout waiting for "+
					"channel(%s) open: %v", nodeNames[i],
					node.NodeID, point, err)
			}
		}
	}

	// Query for routes to pay from Alice to Carol.
	// We set FinalCltvDelta to 40 since by default QueryRoutes returns
	// the last hop with a final cltv delta of 9 where as the default in
	// htlcswitch is 80.
	const paymentAmt = 1000
	routesReq := &lnrpc.QueryRoutesRequest{
		PubKey:         carol.PubKeyStr,
		Amt:            paymentAmt,
		FinalCltvDelta: dcrlnd.DefaultDecredTimeLockDelta,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	routes, err := net.Alice.QueryRoutes(ctxt, routesReq)
	if err != nil {
		t.Fatalf("unable to get route: %v", err)
	}

	// Create 5 invoices for Carol, which expect a payment from Alice for 1k
	// atoms with a different preimage each time.
	const numPayments = 5
	_, rHashes, _, err := createPayReqs(
		carol, paymentAmt, numPayments,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// We'll wait for all parties to recognize the new channels within the
	// network.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointBob)
	if err != nil {
		t.Fatalf("bob didn't advertise his channel in time: %v", err)
	}

	time.Sleep(time.Millisecond * 50)

	// Using Alice as the source, pay to the 5 invoices from Carol created
	// above.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	alicePayStream, err := net.Alice.SendToRoute(ctxt)
	if err != nil {
		t.Fatalf("unable to create payment stream for alice: %v", err)
	}

	for _, rHash := range rHashes {
		sendReq := &lnrpc.SendToRouteRequest{
			PaymentHash: rHash,
			Route:       routes.Routes[0],
		}
		err := alicePayStream.Send(sendReq)

		if err != nil {
			t.Fatalf("unable to send payment: %v", err)
		}
	}

	for range rHashes {
		resp, err := alicePayStream.Recv()
		if err != nil {
			t.Fatalf("unable to send payment: %v", err)
		}
		if resp.PaymentError != "" {
			t.Fatalf("received payment error: %v", resp.PaymentError)
		}
	}

	// When asserting the amount of atoms moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when
	// creating the seed nodes in the network.
	const baseFee = 1

	// At this point all the channels within our proto network should be
	// shifted by 5k atoms in the direction of Carol, the sink within the
	// payment flow generated above. The order of asserts corresponds to
	// increasing of time is needed to embed the HTLC in commitment
	// transaction, in channel Alice->Bob->Carol, order is Carol, Bob,
	// Alice.
	const amountPaid = int64(5000)
	assertAmountPaid(t, "Bob(local) => Carol(remote)", carol,
		bobFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, "Bob(local) => Carol(remote)", net.Bob,
		bobFundPoint, amountPaid, int64(0))
	assertAmountPaid(t, "Alice(local) => Bob(remote)", net.Bob,
		aliceFundPoint, int64(0), amountPaid+(baseFee*numPayments))
	assertAmountPaid(t, "Alice(local) => Bob(remote)", net.Alice,
		aliceFundPoint, amountPaid+(baseFee*numPayments), int64(0))

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointBob, false)
}

// testSendToRouteErrorPropagation tests propagation of errors that occur
// while processing a multi-hop payment through an unknown route.
func testSendToRouteErrorPropagation(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const chanAmt = dcrutil.Amount(100000)

	// Open a channel with 100k atoms between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointAlice := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err := net.Alice.WaitForNetworkChannelOpen(ctxt, chanPointAlice)
	if err != nil {
		t.Fatalf("alice didn't advertise her channel: %v", err)
	}

	// Create a new nodes (Carol and Dave), load her with some funds, then
	// establish a connection between Carol and Dave with a channel that
	// has identical capacity to the one created above.Then we will get
	// route via queryroutes call which will be fake route for Alice -> Bob
	// graph.
	//
	// The network topology should now look like: Alice -> Bob; Carol ->
	// Dave.
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}

	dave, err := net.NewNode("Dave", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, dave)
	if err != nil {
		t.Fatalf("unable to send coins to dave: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, carol, dave); err != nil {
		t.Fatalf("unable to connect carol to alice: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointCarol := openChannelAndAssert(
		ctxt, t, net, carol, dave,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointCarol)
	if err != nil {
		t.Fatalf("carol didn't advertise her channel: %v", err)
	}

	// Query routes from Carol to Dave which will be an invalid route for
	// Alice -> Bob.
	fakeReq := &lnrpc.QueryRoutesRequest{
		PubKey: dave.PubKeyStr,
		Amt:    int64(1),
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	fakeRoute, err := carol.QueryRoutes(ctxt, fakeReq)
	if err != nil {
		t.Fatalf("unable get fake route: %v", err)
	}

	// Create 1 invoices for Bob, which expect a payment from Alice for 1k
	// atoms
	const paymentAmt = 1000

	invoice := &lnrpc.Invoice{
		Memo:  "testing",
		Value: paymentAmt,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	resp, err := net.Bob.AddInvoice(ctxt, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	rHash := resp.RHash

	// Using Alice as the source, pay to the 5 invoices from Bob created above.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	alicePayStream, err := net.Alice.SendToRoute(ctxt)
	if err != nil {
		t.Fatalf("unable to create payment stream for alice: %v", err)
	}

	sendReq := &lnrpc.SendToRouteRequest{
		PaymentHash: rHash,
		Route:       fakeRoute.Routes[0],
	}

	if err := alicePayStream.Send(sendReq); err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// At this place we should get an rpc error with notification
	// that edge is not found on hop(0)
	if _, err := alicePayStream.Recv(); err != nil && strings.Contains(err.Error(),
		"edge not found") {

	} else if err != nil {
		t.Fatalf("payment stream has been closed but fake route has consumed: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarol, false)
}

// testUnannouncedChannels checks unannounced channels are not returned by
// describeGraph RPC request unless explicitly asked for.
func testUnannouncedChannels(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	amount := defaultChanAmt

	// Open a channel between Alice and Bob, ensuring the
	// channel has been opened properly.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	chanOpenUpdate := openChannelStream(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: amount,
		},
	)

	// Mine 2 blocks, and check that the channel is opened but not yet
	// announced to the network.
	mineBlocks(t, net, 2, 1)

	// One block is enough to make the channel ready for use, since the
	// nodes have defaultNumConfs=1 set.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	fundingChanPoint, err := net.WaitForChannelOpen(ctxt, chanOpenUpdate)
	if err != nil {
		t.Fatalf("error while waiting for channel open: %v", err)
	}

	// Alice should have 1 edge in her graph.
	req := &lnrpc.ChannelGraphRequest{
		IncludeUnannounced: true,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
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
	}, time.Second*15)
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
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, fundingChanPoint, false)
}

// testPrivateChannels tests that a private channel can be used for
// routing by the two endpoints of the channel, but is not known by
// the rest of the nodes in the graph.
func testPrivateChannels(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const chanAmt = dcrutil.Amount(100000)
	var networkChans []*lnrpc.ChannelPoint

	// We create the following topology:
	//
	// Dave --100k--> Alice --200k--> Bob
	//  ^		    ^
	//  |		    |
	// 100k		   100k
	//  |		    |
	//  +---- Carol ----+
	//
	// where the 100k channel between Carol and Alice is private.

	// Open a channel with 200k atoms between Alice and Bob.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointAlice := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: chanAmt * 2,
		},
	)
	networkChans = append(networkChans, chanPointAlice)

	aliceChanTXID, err := dcrlnd.GetChanPointFundingTxid(chanPointAlice)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// Create Dave, and a channel to Alice of 100k.
	dave, err := net.NewNode("Dave", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, dave, net.Alice); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, dave)
	if err != nil {
		t.Fatalf("unable to send coins to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointDave := openChannelAndAssert(
		ctxt, t, net, dave, net.Alice,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	networkChans = append(networkChans, chanPointDave)
	daveChanTXID, err := dcrlnd.GetChanPointFundingTxid(chanPointDave)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	daveFundPoint := wire.OutPoint{
		Hash:  *daveChanTXID,
		Index: chanPointDave.OutputIndex,
	}

	// Next, we'll create Carol and establish a channel from her to
	// Dave of 100k.
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, carol, dave); err != nil {
		t.Fatalf("unable to connect carol to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointCarol := openChannelAndAssert(
		ctxt, t, net, carol, dave,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	networkChans = append(networkChans, chanPointCarol)

	carolChanTXID, err := dcrlnd.GetChanPointFundingTxid(chanPointCarol)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	carolFundPoint := wire.OutPoint{
		Hash:  *carolChanTXID,
		Index: chanPointCarol.OutputIndex,
	}

	// Wait for all nodes to have seen all these channels, as they
	// are all public.
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol, dave}
	nodeNames := []string{"Alice", "Bob", "Carol", "Dave"}
	for _, chanPoint := range networkChans {
		for i, node := range nodes {
			txid, err := dcrlnd.GetChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
			err = node.WaitForNetworkChannelOpen(ctxt, chanPoint)
			if err != nil {
				t.Fatalf("%s(%d): timeout waiting for "+
					"channel(%s) open: %v", nodeNames[i],
					node.NodeID, point, err)
			}
		}
	}
	// Now create a _private_ channel directly between Carol and
	// Alice of 100k.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, carol, net.Alice); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanOpenUpdate := openChannelStream(
		ctxt, t, net, carol, net.Alice,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			Private: true,
		},
	)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	// One block is enough to make the channel ready for use, since the
	// nodes have defaultNumConfs=1 set.
	block := mineBlocks(t, net, 1, 1)[0]

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	chanPointPrivate, err := net.WaitForChannelOpen(ctxt, chanOpenUpdate)
	if err != nil {
		t.Fatalf("error while waiting for channel open: %v", err)
	}
	fundingTxID, err := dcrlnd.GetChanPointFundingTxid(chanPointPrivate)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	assertTxInBlock(t, block, fundingTxID)

	// The channel should be listed in the peer information returned by
	// both peers.
	privateFundPoint := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: chanPointPrivate.OutputIndex,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.AssertChannelExists(ctxt, carol, &privateFundPoint)
	if err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.AssertChannelExists(ctxt, net.Alice, &privateFundPoint)
	if err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}

	// The channel should be available for payments between Carol and Alice.
	// We check this by sending payments from Carol to Bob, that
	// collectively would deplete at least one of Carol's channels.

	// Create 2 invoices for Bob, each of 70k atoms. Since each of
	// Carol's channels is of size 100k, these payments cannot succeed
	// by only using one of the channels.
	const numPayments = 2
	const paymentAmt = 70000
	payReqs, _, _, err := createPayReqs(
		net.Bob, paymentAmt, numPayments,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	time.Sleep(time.Millisecond * 50)

	// Let Carol pay the invoices.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, carol, carol.RouterClient, payReqs, true,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// When asserting the amount of atoms moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when
	// creating the seed nodes in the network.
	const baseFee = 1

	// Bob should have received 140k atoms from Alice.
	assertAmountPaid(t, "Alice(local) => Bob(remote)", net.Bob,
		aliceFundPoint, int64(0), 2*paymentAmt)

	// Alice sent 140k to Bob.
	assertAmountPaid(t, "Alice(local) => Bob(remote)", net.Alice,
		aliceFundPoint, 2*paymentAmt, int64(0))

	// Alice received 70k + fee from Dave.
	assertAmountPaid(t, "Dave(local) => Alice(remote)", net.Alice,
		daveFundPoint, int64(0), paymentAmt+baseFee)

	// Dave sent 70k+fee to Alice.
	assertAmountPaid(t, "Dave(local) => Alice(remote)", dave,
		daveFundPoint, paymentAmt+baseFee, int64(0))

	// Dave received 70k+fee of two hops from Carol.
	assertAmountPaid(t, "Carol(local) => Dave(remote)", dave,
		carolFundPoint, int64(0), paymentAmt+baseFee*2)

	// Carol sent 70k+fee of two hops to Dave.
	assertAmountPaid(t, "Carol(local) => Dave(remote)", carol,
		carolFundPoint, paymentAmt+baseFee*2, int64(0))

	// Alice received 70k+fee from Carol.
	assertAmountPaid(t, "Carol(local) [private=>] Alice(remote)",
		net.Alice, privateFundPoint, int64(0), paymentAmt+baseFee)

	// Carol sent 70k+fee to Alice.
	assertAmountPaid(t, "Carol(local) [private=>] Alice(remote)",
		carol, privateFundPoint, paymentAmt+baseFee, int64(0))

	// Alice should also be able to route payments using this channel,
	// so send two payments of 60k back to Carol.
	const paymentAmt60k = 60000
	payReqs, _, _, err = createPayReqs(
		carol, paymentAmt60k, numPayments,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	time.Sleep(time.Millisecond * 50)

	// Let Bob pay the invoices.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, net.Alice, net.Alice.RouterClient, payReqs, true,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Finally, we make sure Dave and Bob does not know about the
	// private channel between Carol and Alice. We first mine
	// plenty of blocks, such that the channel would have been
	// announced in case it was public.
	mineBlocks(t, net, 10, 0)

	// Wait until all nodes have caught up to this height.
	_, minerHeight, err := net.Miner.Node.GetBestBlock(context.Background())
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}
	allNodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol, dave}
	for _, node := range allNodes {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		err = waitForNodeBlockHeight(ctxt, node, minerHeight)
		if err != nil {
			t.Fatalf("unable to sync to chain: %v", err)
		}
	}

	// We create a helper method to check how many edges each of the nodes
	// know about. Carol and Alice should know about 4, while Bob and Dave
	// should only know about 3, since one channel is private.
	numChannels := func(node *lntest.HarnessNode, includeUnannounced bool) int {
		req := &lnrpc.ChannelGraphRequest{
			IncludeUnannounced: includeUnannounced,
		}
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		chanGraph, err := node.DescribeGraph(ctxt, req)
		if err != nil {
			t.Fatalf("unable go describegraph: %v", err)
		}
		return len(chanGraph.Edges)
	}

	var predErr error
	err = wait.Predicate(func() bool {
		aliceChans := numChannels(net.Alice, true)
		if aliceChans != 4 {
			predErr = fmt.Errorf("expected Alice to know 4 edges, "+
				"had %v", aliceChans)
			return false
		}
		alicePubChans := numChannels(net.Alice, false)
		if alicePubChans != 3 {
			predErr = fmt.Errorf("expected Alice to know 3 public edges, "+
				"had %v", alicePubChans)
			return false
		}
		bobChans := numChannels(net.Bob, true)
		if bobChans != 3 {
			predErr = fmt.Errorf("expected Bob to know 3 edges, "+
				"had %v", bobChans)
			return false
		}
		carolChans := numChannels(carol, true)
		if carolChans != 4 {
			predErr = fmt.Errorf("expected Carol to know 4 edges, "+
				"had %v", carolChans)
			return false
		}
		carolPubChans := numChannels(carol, false)
		if carolPubChans != 3 {
			predErr = fmt.Errorf("expected Carol to know 3 public edges, "+
				"had %v", carolPubChans)
			return false
		}
		daveChans := numChannels(dave, true)
		if daveChans != 3 {
			predErr = fmt.Errorf("expected Dave to know 3 edges, "+
				"had %v", daveChans)
			return false
		}
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("%v", predErr)
	}

	// Close all channels.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, dave, chanPointDave, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarol, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointPrivate, false)
}

// testInvoiceRoutingHints tests that the routing hints for an invoice are
// created properly.
func testInvoiceRoutingHints(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const chanAmt = dcrutil.Amount(100000)

	// Throughout this test, we'll be opening a channel between Alice and
	// several other parties.
	//
	// First, we'll create a private channel between Alice and Bob. This
	// will be the only channel that will be considered as a routing hint
	// throughout this test. We'll include a push amount since we currently
	// require channels to have enough remote balance to cover the invoice's
	// payment.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointBob := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: chanAmt / 2,
			Private: true,
		},
	)

	// Then, we'll create Carol's node and open a public channel between her
	// and Alice. This channel will not be considered as a routing hint due
	// to it being public.
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create carol's node: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect alice to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointCarol := openChannelAndAssert(
		ctxt, t, net, net.Alice, carol,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: chanAmt / 2,
		},
	)

	// We'll also create a public channel between Bob and Carol to ensure
	// that Bob gets selected as the only routing hint. We do this as
	// we should only include routing hints for nodes that are publicly
	// advertised, otherwise we'd end up leaking information about nodes
	// that wish to stay unadvertised.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, net.Bob, carol); err != nil {
		t.Fatalf("unable to connect alice to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointBobCarol := openChannelAndAssert(
		ctxt, t, net, net.Bob, carol,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: chanAmt / 2,
		},
	)

	// Then, we'll create Dave's node and open a private channel between him
	// and Alice. We will not include a push amount in order to not consider
	// this channel as a routing hint as it will not have enough remote
	// balance for the invoice's amount.
	dave, err := net.NewNode("Dave", nil)
	if err != nil {
		t.Fatalf("unable to create dave's node: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, net.Alice, dave); err != nil {
		t.Fatalf("unable to connect alice to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointDave := openChannelAndAssert(
		ctxt, t, net, net.Alice, dave,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			Private: true,
		},
	)

	// Finally, we'll create Eve's node and open a private channel between
	// her and Alice. This time though, we'll take Eve's node down after the
	// channel has been created to avoid populating routing hints for
	// inactive channels.
	eve, err := net.NewNode("Eve", nil)
	if err != nil {
		t.Fatalf("unable to create eve's node: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, net.Alice, eve); err != nil {
		t.Fatalf("unable to connect alice to eve: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointEve := openChannelAndAssert(
		ctxt, t, net, net.Alice, eve,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: chanAmt / 2,
			Private: true,
		},
	)

	// Make sure all the channels have been opened.
	nodeNames := []string{"bob", "carol", "bob->carol", "dave", "eve"}
	aliceChans := []*lnrpc.ChannelPoint{
		chanPointBob, chanPointCarol, chanPointBobCarol, chanPointDave,
		chanPointEve,
	}
	for i, chanPoint := range aliceChans {
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
		if err != nil {
			t.Fatalf("timed out waiting for channel open between "+
				"alice and %s: %v", nodeNames[i], err)
		}
	}

	// Now that the channels are open, we'll take down Eve's node.
	shutdownAndAssert(net, t, eve)

	// Create an invoice for Alice that will populate the routing hints.
	invoice := &lnrpc.Invoice{
		Memo:    "routing hints",
		Value:   int64(chanAmt / 4),
		Private: true,
	}

	// Due to the way the channels were set up above, the channel between
	// Alice and Bob should be the only channel used as a routing hint.
	var predErr error
	var decoded *lnrpc.PayReq
	err = wait.Predicate(func() bool {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		resp, err := net.Alice.AddInvoice(ctxt, invoice)
		if err != nil {
			predErr = fmt.Errorf("unable to add invoice: %v", err)
			return false
		}

		// We'll decode the invoice's payment request to determine which
		// channels were used as routing hints.
		payReq := &lnrpc.PayReqString{
			PayReq: resp.PaymentRequest,
		}
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		decoded, err = net.Alice.DecodePayReq(ctxt, payReq)
		if err != nil {
			predErr = fmt.Errorf("unable to decode payment "+
				"request: %v", err)
			return false
		}

		if len(decoded.RouteHints) != 1 {
			predErr = fmt.Errorf("expected one route hint, got %d",
				len(decoded.RouteHints))
			return false
		}
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf(predErr.Error())
	}

	hops := decoded.RouteHints[0].HopHints
	if len(hops) != 1 {
		t.Fatalf("expected one hop in route hint, got %d", len(hops))
	}
	chanID := hops[0].ChanId

	// We'll need the short channel ID of the channel between Alice and Bob
	// to make sure the routing hint is for this channel.
	listReq := &lnrpc.ListChannelsRequest{}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	listResp, err := net.Alice.ListChannels(ctxt, listReq)
	if err != nil {
		t.Fatalf("unable to retrieve alice's channels: %v", err)
	}

	var aliceBobChanID uint64
	for _, channel := range listResp.Channels {
		if channel.RemotePubkey == net.Bob.PubKeyStr {
			aliceBobChanID = channel.ChanId
		}
	}

	if aliceBobChanID == 0 {
		t.Fatalf("channel between alice and bob not found")
	}

	if chanID != aliceBobChanID {
		t.Fatalf("expected channel ID %d, got %d", aliceBobChanID,
			chanID)
	}

	// Now that we've confirmed the routing hints were added correctly, we
	// can close all the channels and shut down all the nodes created.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointBob, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointCarol, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Bob, chanPointBobCarol, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointDave, false)

	// The channel between Alice and Eve should be force closed since Eve
	// is offline.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointEve, true)

	// Cleanup by mining the force close and sweep transaction.
	cleanupForceClose(t, net, net.Alice, chanPointEve)
}

// testMultiHopOverPrivateChannels tests that private channels can be used as
// intermediate hops in a route for payments.
func testMultiHopOverPrivateChannels(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// We'll test that multi-hop payments over private channels work as
	// intended. To do so, we'll create the following topology:
	//         private        public           private
	// Alice <--100k--> Bob <--100k--> Carol <--100k--> Dave
	const chanAmt = dcrutil.Amount(100000)

	// First, we'll open a private channel between Alice and Bob with Alice
	// being the funder.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointAlice := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			Private: true,
		},
	)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err := net.Alice.WaitForNetworkChannelOpen(ctxt, chanPointAlice)
	if err != nil {
		t.Fatalf("alice didn't see the channel alice <-> bob before "+
			"timeout: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPointAlice)
	if err != nil {
		t.Fatalf("bob didn't see the channel alice <-> bob before "+
			"timeout: %v", err)
	}

	// Retrieve Alice's funding outpoint.
	aliceChanTXID, err := dcrlnd.GetChanPointFundingTxid(chanPointAlice)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// Next, we'll create Carol's node and open a public channel between
	// her and Bob with Bob being the funder.
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create carol's node: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, net.Bob, carol); err != nil {
		t.Fatalf("unable to connect bob to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointBob := openChannelAndAssert(
		ctxt, t, net, net.Bob, carol,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPointBob)
	if err != nil {
		t.Fatalf("bob didn't see the channel bob <-> carol before "+
			"timeout: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointBob)
	if err != nil {
		t.Fatalf("carol didn't see the channel bob <-> carol before "+
			"timeout: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPointBob)
	if err != nil {
		t.Fatalf("alice didn't see the channel bob <-> carol before "+
			"timeout: %v", err)
	}

	// Retrieve Bob's funding outpoint.
	bobChanTXID, err := dcrlnd.GetChanPointFundingTxid(chanPointBob)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	bobFundPoint := wire.OutPoint{
		Hash:  *bobChanTXID,
		Index: chanPointBob.OutputIndex,
	}

	// Next, we'll create Dave's node and open a private channel between him
	// and Carol with Carol being the funder.
	dave, err := net.NewNode("Dave", nil)
	if err != nil {
		t.Fatalf("unable to create dave's node: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, carol, dave); err != nil {
		t.Fatalf("unable to connect carol to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointCarol := openChannelAndAssert(
		ctxt, t, net, carol, dave,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			Private: true,
		},
	)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointCarol)
	if err != nil {
		t.Fatalf("carol didn't see the channel carol <-> dave before "+
			"timeout: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = dave.WaitForNetworkChannelOpen(ctxt, chanPointCarol)
	if err != nil {
		t.Fatalf("dave didn't see the channel carol <-> dave before "+
			"timeout: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = dave.WaitForNetworkChannelOpen(ctxt, chanPointBob)
	if err != nil {
		t.Fatalf("dave didn't see the channel bob <-> carol before "+
			"timeout: %v", err)
	}

	// Retrieve Carol's funding point.
	carolChanTXID, err := dcrlnd.GetChanPointFundingTxid(chanPointCarol)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	carolFundPoint := wire.OutPoint{
		Hash:  *carolChanTXID,
		Index: chanPointCarol.OutputIndex,
	}

	// Now that all the channels are set up according to the topology from
	// above, we can proceed to test payments. We'll create an invoice for
	// Dave of 20k atoms and pay it with Alice. Since there is no public
	// route from Alice to Dave, we'll need to use the private channel
	// between Carol and Dave as a routing hint encoded in the invoice.
	const paymentAmt = 20000

	// Create the invoice for Dave.
	invoice := &lnrpc.Invoice{
		Memo:    "two hopz!",
		Value:   paymentAmt,
		Private: true,
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	resp, err := dave.AddInvoice(ctxt, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice for dave: %v", err)
	}

	// Let Alice pay the invoice.
	payReqs := []string{resp.PaymentRequest}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, net.Alice, net.Alice.RouterClient, payReqs, true,
	)
	if err != nil {
		t.Fatalf("unable to send payments from alice to dave: %v", err)
	}

	// When asserting the amount of atoms moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when opening
	// the channels.
	const baseFee = 1

	// Dave should have received 20k atoms from Carol.
	assertAmountPaid(t, "Carol(local) [private=>] Dave(remote)",
		dave, carolFundPoint, 0, paymentAmt)

	// Carol should have sent 20k atoms to Dave.
	assertAmountPaid(t, "Carol(local) [private=>] Dave(remote)",
		carol, carolFundPoint, paymentAmt, 0)

	// Carol should have received 20k atoms + fee for one hop from Bob.
	assertAmountPaid(t, "Bob(local) => Carol(remote)",
		carol, bobFundPoint, 0, paymentAmt+baseFee)

	// Bob should have sent 20k atoms + fee for one hop to Carol.
	assertAmountPaid(t, "Bob(local) => Carol(remote)",
		net.Bob, bobFundPoint, paymentAmt+baseFee, 0)

	// Bob should have received 20k atoms + fee for two hops from Alice.
	assertAmountPaid(t, "Alice(local) [private=>] Bob(remote)", net.Bob,
		aliceFundPoint, 0, paymentAmt+baseFee*2)

	// Alice should have sent 20k atoms + fee for two hops to Bob.
	assertAmountPaid(t, "Alice(local) [private=>] Bob(remote)", net.Alice,
		aliceFundPoint, paymentAmt+baseFee*2, 0)

	// At this point, the payment was successful. We can now close all the
	// channels and shutdown the nodes created throughout this test.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Bob, chanPointBob, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarol, false)
}

func testInvoiceSubscriptions(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const chanAmt = dcrutil.Amount(500000)

	// Open a channel with 500k atoms between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Next create a new invoice for Bob requesting 1k atoms.
	// TODO(roasbeef): make global list of invoices for each node to re-use
	// and avoid collisions
	const paymentAmt = 1000
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: makeFakePayHash(t),
		Value:     paymentAmt,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	invoiceResp, err := net.Bob.AddInvoice(ctxt, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}
	lastAddIndex := invoiceResp.AddIndex

	// Create a new invoice subscription client for Bob, the notification
	// should be dispatched shortly below.
	req := &lnrpc.InvoiceSubscription{}
	ctx, cancelInvoiceSubscription := context.WithCancel(ctxb)
	bobInvoiceSubscription, err := net.Bob.SubscribeInvoices(ctx, req)
	if err != nil {
		t.Fatalf("unable to subscribe to bob's invoice updates: %v", err)
	}

	var settleIndex uint64
	quit := make(chan struct{})
	updateSent := make(chan struct{})
	go func() {
		invoiceUpdate, err := bobInvoiceSubscription.Recv()
		select {
		case <-quit:
			// Received cancellation
			return
		default:
		}

		if err != nil {
			t.Fatalf("unable to recv invoice update: %v", err)
		}

		// The invoice update should exactly match the invoice created
		// above, but should now be settled and have SettleDate
		if !invoiceUpdate.Settled {
			t.Fatalf("invoice not settled but should be")
		}
		if invoiceUpdate.SettleDate == 0 {
			t.Fatalf("invoice should have non zero settle date, but doesn't")
		}

		if !bytes.Equal(invoiceUpdate.RPreimage, invoice.RPreimage) {
			t.Fatalf("payment preimages don't match: expected %v, got %v",
				invoice.RPreimage, invoiceUpdate.RPreimage)
		}

		if invoiceUpdate.SettleIndex == 0 {
			t.Fatalf("invoice should have settle index")
		}

		settleIndex = invoiceUpdate.SettleIndex

		close(updateSent)
	}()

	// Wait for the channel to be recognized by both Alice and Bob before
	// continuing the rest of the test.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		// TODO(roasbeef): will need to make num blocks to advertise a
		// node param
		close(quit)
		t.Fatalf("channel not seen by alice before timeout: %v", err)
	}

	// With the assertion above set up, send a payment from Alice to Bob
	// which should finalize and settle the invoice.
	sendReq := &routerrpc.SendPaymentRequest{
		PaymentRequest: invoiceResp.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMAtoms: noFeeLimitMAtoms,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	stream, err := net.Alice.RouterClient.SendPaymentV2(ctxt, sendReq)
	if err != nil {
		close(quit)
		t.Fatalf("unable to send payment: %v", err)
	}
	result, err := getPaymentResult(stream)
	if err != nil {
		close(quit)
		t.Fatalf("cannot get payment result: %v", err)
	}
	if result.Status != lnrpc.Payment_SUCCEEDED {
		close(quit)
		t.Fatalf("error when attempting recv: %v", result.Status)
	}

	select {
	case <-time.After(time.Second * 10):
		close(quit)
		t.Fatalf("update not sent after 10 seconds")
	case <-updateSent: // Fall through on success
	}

	// With the base case working, we'll now cancel Bob's current
	// subscription in order to exercise the backlog fill behavior.
	cancelInvoiceSubscription()

	// We'll now add 3 more invoices to Bob's invoice registry.
	const numInvoices = 3
	payReqs, _, newInvoices, err := createPayReqs(
		net.Bob, paymentAmt, numInvoices,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// Now that the set of invoices has been added, we'll re-register for
	// streaming invoice notifications for Bob, this time specifying the
	// add invoice of the last prior invoice.
	req = &lnrpc.InvoiceSubscription{
		AddIndex: lastAddIndex,
	}
	ctx, cancelInvoiceSubscription = context.WithCancel(ctxb)
	bobInvoiceSubscription, err = net.Bob.SubscribeInvoices(ctx, req)
	if err != nil {
		t.Fatalf("unable to subscribe to bob's invoice updates: %v", err)
	}

	// Since we specified a value of the prior add index above, we should
	// now immediately get the invoices we just added as we should get the
	// backlog of notifications.
	for i := 0; i < numInvoices; i++ {
		invoiceUpdate, err := bobInvoiceSubscription.Recv()
		if err != nil {
			t.Fatalf("unable to receive subscription")
		}

		// We should now get the ith invoice we added, as they should
		// be returned in order.
		if invoiceUpdate.Settled {
			t.Fatalf("should have only received add events")
		}
		originalInvoice := newInvoices[i]
		rHash := sha256.Sum256(originalInvoice.RPreimage)
		if !bytes.Equal(invoiceUpdate.RHash, rHash[:]) {
			t.Fatalf("invoices have mismatched payment hashes: "+
				"expected %x, got %x", rHash,
				invoiceUpdate.RHash)
		}
	}

	cancelInvoiceSubscription()

	// We'll now have Bob settle out the remainder of these invoices so we
	// can test that all settled invoices are properly notified.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, net.Alice, net.Alice.RouterClient, payReqs, true,
	)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// With the set of invoices paid, we'll now cancel the old
	// subscription, and create a new one for Bob, this time using the
	// settle index to obtain the backlog of settled invoices.
	req = &lnrpc.InvoiceSubscription{
		SettleIndex: settleIndex,
	}
	ctx, cancelInvoiceSubscription = context.WithCancel(ctxb)
	bobInvoiceSubscription, err = net.Bob.SubscribeInvoices(ctx, req)
	if err != nil {
		t.Fatalf("unable to subscribe to bob's invoice updates: %v", err)
	}

	defer cancelInvoiceSubscription()

	// As we specified the index of the past settle index, we should now
	// receive notifications for the three HTLCs that we just settled. As
	// the order that the HTLCs will be settled in is partially randomized,
	// we'll use a map to assert that the proper set has been settled.
	settledInvoices := make(map[[32]byte]struct{})
	for _, invoice := range newInvoices {
		rHash := sha256.Sum256(invoice.RPreimage)
		settledInvoices[rHash] = struct{}{}
	}
	for i := 0; i < numInvoices; i++ {
		invoiceUpdate, err := bobInvoiceSubscription.Recv()
		if err != nil {
			t.Fatalf("unable to receive subscription")
		}

		// We should now get the ith invoice we added, as they should
		// be returned in order.
		if !invoiceUpdate.Settled {
			t.Fatalf("should have only received settle events")
		}

		var rHash [32]byte
		copy(rHash[:], invoiceUpdate.RHash)
		if _, ok := settledInvoices[rHash]; !ok {
			t.Fatalf("unknown invoice settled: %x", rHash)
		}

		delete(settledInvoices, rHash)
	}

	// At this point, all the invoices should be fully settled.
	if len(settledInvoices) != 0 {
		t.Fatalf("not all invoices settled")
	}

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}

// channelSubscription houses the proxied update and error chans for a node's
// channel subscriptions.
type channelSubscription struct {
	updateChan chan *lnrpc.ChannelEventUpdate
	errChan    chan error
	quit       chan struct{}
}

// subscribeChannelNotifications subscribes to channel updates and launches a
// goroutine that forwards these to the returned channel.
func subscribeChannelNotifications(ctxb context.Context, t *harnessTest,
	node *lntest.HarnessNode) channelSubscription {

	// We'll first start by establishing a notification client which will
	// send us notifications upon channels becoming active, inactive or
	// closed.
	req := &lnrpc.ChannelEventSubscription{}
	ctx, cancelFunc := context.WithCancel(ctxb)

	chanUpdateClient, err := node.SubscribeChannelEvents(ctx, req)
	if err != nil {
		t.Fatalf("unable to create channel update client: %v", err)
	}

	// We'll launch a goroutine that will be responsible for proxying all
	// notifications recv'd from the client into the channel below.
	errChan := make(chan error, 1)
	quit := make(chan struct{})
	chanUpdates := make(chan *lnrpc.ChannelEventUpdate, 20)
	go func() {
		defer cancelFunc()
		for {
			select {
			case <-quit:
				return
			default:
				chanUpdate, err := chanUpdateClient.Recv()
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
				case chanUpdates <- chanUpdate:
				case <-quit:
					return
				}
			}
		}
	}()

	return channelSubscription{
		updateChan: chanUpdates,
		errChan:    errChan,
		quit:       quit,
	}
}

// verifyCloseUpdate is used to verify that a closed channel update is of the
// expected type.
func verifyCloseUpdate(chanUpdate *lnrpc.ChannelEventUpdate,
	closeType lnrpc.ChannelCloseSummary_ClosureType,
	closeInitiator lnrpc.Initiator) error {

	// We should receive one inactive and one closed notification
	// for each channel.
	switch update := chanUpdate.Channel.(type) {
	case *lnrpc.ChannelEventUpdate_InactiveChannel:
		if chanUpdate.Type != lnrpc.ChannelEventUpdate_INACTIVE_CHANNEL {
			return fmt.Errorf("update type mismatch: expected %v, got %v",
				lnrpc.ChannelEventUpdate_INACTIVE_CHANNEL,
				chanUpdate.Type)
		}
	case *lnrpc.ChannelEventUpdate_ClosedChannel:
		if chanUpdate.Type !=
			lnrpc.ChannelEventUpdate_CLOSED_CHANNEL {
			return fmt.Errorf("update type mismatch: expected %v, got %v",
				lnrpc.ChannelEventUpdate_CLOSED_CHANNEL,
				chanUpdate.Type)
		}

		if update.ClosedChannel.CloseType != closeType {
			return fmt.Errorf("channel closure type "+
				"mismatch: expected %v, got %v",
				closeType,
				update.ClosedChannel.CloseType)
		}

		if update.ClosedChannel.CloseInitiator != closeInitiator {
			return fmt.Errorf("expected close intiator: %v, got: %v",
				closeInitiator,
				update.ClosedChannel.CloseInitiator)
		}

	default:
		return fmt.Errorf("channel update channel of wrong type, "+
			"expected closed channel, got %T",
			update)
	}

	return nil
}

// testBasicChannelCreationAndUpdates tests multiple channel opening and closing,
// and ensures that if a node is subscribed to channel updates they will be
// received correctly for both cooperative and force closed channels.
func testBasicChannelCreationAndUpdates(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()
	const (
		numChannels = 2
		amount      = defaultChanAmt
	)

	// Subscribe Bob and Alice to channel event notifications.
	bobChanSub := subscribeChannelNotifications(ctxb, t, net.Bob)
	defer close(bobChanSub.quit)

	aliceChanSub := subscribeChannelNotifications(ctxb, t, net.Alice)
	defer close(aliceChanSub.quit)

	// Open the channel between Alice and Bob, asserting that the
	// channel has been properly open on-chain.
	chanPoints := make([]*lnrpc.ChannelPoint, numChannels)
	for i := 0; i < numChannels; i++ {
		ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
		chanPoints[i] = openChannelAndAssert(
			ctxt, t, net, net.Alice, net.Bob,
			lntest.OpenChannelParams{
				Amt: amount,
			},
		)
	}

	// Since each of the channels just became open, Bob and Alice should
	// each receive an open and an active notification for each channel.
	var numChannelUpds int
	const totalNtfns = 3 * numChannels
	verifyOpenUpdatesReceived := func(sub channelSubscription) error {
		numChannelUpds = 0
		for numChannelUpds < totalNtfns {
			select {
			case update := <-sub.updateChan:
				switch update.Type {
				case lnrpc.ChannelEventUpdate_PENDING_OPEN_CHANNEL:
					if numChannelUpds%3 != 0 {
						return fmt.Errorf("expected " +
							"open or active" +
							"channel ntfn, got pending open " +
							"channel ntfn instead")
					}
				case lnrpc.ChannelEventUpdate_OPEN_CHANNEL:
					if numChannelUpds%3 != 1 {
						return fmt.Errorf("expected " +
							"pending open or active" +
							"channel ntfn, got open" +
							"channel ntfn instead")
					}
				case lnrpc.ChannelEventUpdate_ACTIVE_CHANNEL:
					if numChannelUpds%3 != 2 {
						return fmt.Errorf("expected " +
							"pending open or open" +
							"channel ntfn, got active " +
							"channel ntfn instead")
					}
				default:
					return fmt.Errorf("update type mismatch: "+
						"expected open or active channel "+
						"notification, got: %v",
						update.Type)
				}
				numChannelUpds++
			case <-time.After(time.Second * 10):
				return fmt.Errorf("timeout waiting for channel "+
					"notifications, only received %d/%d "+
					"chanupds", numChannelUpds,
					totalNtfns)
			}
		}

		return nil
	}

	if err := verifyOpenUpdatesReceived(bobChanSub); err != nil {
		t.Fatalf("error verifying open updates: %v", err)
	}
	if err := verifyOpenUpdatesReceived(aliceChanSub); err != nil {
		t.Fatalf("error verifying open updates: %v", err)
	}

	// Close the channel between Alice and Bob, asserting that the channel
	// has been properly closed on-chain.
	for i, chanPoint := range chanPoints {
		ctx, _ := context.WithTimeout(context.Background(), defaultTimeout)

		// Force close half of the channels.
		force := i%2 == 0
		closeChannelAndAssert(ctx, t, net, net.Alice, chanPoint, force)
		if force {
			cleanupForceClose(t, net, net.Alice, chanPoint)
		}
	}

	// verifyCloseUpdatesReceived is used to verify that Alice and Bob
	// receive the correct channel updates in order.
	verifyCloseUpdatesReceived := func(sub channelSubscription,
		forceType lnrpc.ChannelCloseSummary_ClosureType,
		closeInitiator lnrpc.Initiator) error {

		// Ensure one inactive and one closed notification is received for each
		// closed channel.
		numChannelUpds := 0
		for numChannelUpds < 2*numChannels {
			expectedCloseType := lnrpc.ChannelCloseSummary_COOPERATIVE_CLOSE

			// Every other channel should be force closed. If this
			// channel was force closed, set the expected close type
			// the the type passed in.
			force := (numChannelUpds/2)%2 == 0
			if force {
				expectedCloseType = forceType
			}

			select {
			case chanUpdate := <-sub.updateChan:
				err := verifyCloseUpdate(
					chanUpdate, expectedCloseType,
					closeInitiator,
				)
				if err != nil {
					return err
				}

				numChannelUpds++
			case err := <-sub.errChan:
				return err
			case <-time.After(time.Second * 10):
				return fmt.Errorf("timeout waiting "+
					"for channel notifications, only "+
					"received %d/%d chanupds",
					numChannelUpds, 2*numChannels)
			}
		}

		return nil
	}

	// Verify Bob receives all closed channel notifications. He should
	// receive a remote force close notification for force closed channels.
	// All channels (cooperatively and force closed) should have a remote
	// close initiator because Alice closed the channels.
	if err := verifyCloseUpdatesReceived(bobChanSub,
		lnrpc.ChannelCloseSummary_REMOTE_FORCE_CLOSE,
		lnrpc.Initiator_INITIATOR_REMOTE); err != nil {
		t.Fatalf("errored verifying close updates: %v", err)
	}

	// Verify Alice receives all closed channel notifications. She should
	// receive a remote force close notification for force closed channels.
	// All channels (cooperatively and force closed) should have a local
	// close initiator because Alice closed the channels.
	if err := verifyCloseUpdatesReceived(aliceChanSub,
		lnrpc.ChannelCloseSummary_LOCAL_FORCE_CLOSE,
		lnrpc.Initiator_INITIATOR_LOCAL); err != nil {
		t.Fatalf("errored verifying close updates: %v", err)
	}
}

// testMaxPendingChannels checks that error is returned from remote peer if
// max pending channel number was exceeded and that '--maxpendingchannels' flag
// exists and works properly.
func testMaxPendingChannels(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	maxPendingChannels := lncfg.DefaultMaxPendingChannels + 1
	amount := defaultChanAmt

	// Create a new node (Carol) with greater number of max pending
	// channels.
	args := []string{
		fmt.Sprintf("--maxpendingchannels=%v", maxPendingChannels),
	}
	carol, err := net.NewNode("Carol", args)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect carol to alice: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	carolBalance := dcrutil.Amount(maxPendingChannels) * amount
	if err := net.SendCoins(ctxt, carolBalance, carol); err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}

	// Send open channel requests without generating new blocks thereby
	// increasing pool of pending channels. Then check that we can't open
	// the channel if the number of pending channels exceed max value.
	openStreams := make([]lnrpc.Lightning_OpenChannelClient, maxPendingChannels)
	for i := 0; i < maxPendingChannels; i++ {
		ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
		stream := openChannelStream(
			ctxt, t, net, net.Alice, carol,
			lntest.OpenChannelParams{
				Amt: amount,
			},
		)
		openStreams[i] = stream
	}

	// Carol exhausted available amount of pending channels, next open
	// channel request should cause ErrorGeneric to be sent back to Alice.
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	_, err = net.OpenChannel(
		ctxt, net.Alice, carol,
		lntest.OpenChannelParams{
			Amt: amount,
		},
	)

	if err == nil {
		t.Fatalf("error wasn't received")
	} else if !strings.Contains(
		err.Error(), lnwire.ErrMaxPendingChannels.Error(),
	) {
		t.Fatalf("not expected error was received: %v", err)
	}

	// For now our channels are in pending state, in order to not interfere
	// with other tests we should clean up - complete opening of the
	// channel and then close it.

	// Mine 6 blocks, then wait for node's to notify us that the channel has
	// been opened. The funding transactions should be found within the
	// first newly mined block. 6 blocks make sure the funding transaction
	// has enough confirmations to be announced publicly.
	block := mineBlocks(t, net, 6, maxPendingChannels)[0]

	chanPoints := make([]*lnrpc.ChannelPoint, maxPendingChannels)
	for i, stream := range openStreams {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		fundingChanPoint, err := net.WaitForChannelOpen(ctxt, stream)
		if err != nil {
			t.Fatalf("error while waiting for channel open: %v", err)
		}

		fundingTxID, err := dcrlnd.GetChanPointFundingTxid(fundingChanPoint)
		if err != nil {
			t.Fatalf("unable to get txid: %v", err)
		}

		// Ensure that the funding transaction enters a block, and is
		// properly advertised by Alice.
		assertTxInBlock(t, block, fundingTxID)
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		err = net.Alice.WaitForNetworkChannelOpen(ctxt, fundingChanPoint)
		if err != nil {
			t.Fatalf("channel not seen on network before "+
				"timeout: %v", err)
		}

		// The channel should be listed in the peer information
		// returned by both peers.
		chanPoint := wire.OutPoint{
			Hash:  *fundingTxID,
			Index: fundingChanPoint.OutputIndex,
		}
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		if err := net.AssertChannelExists(ctxt, net.Alice, &chanPoint); err != nil {
			t.Fatalf("unable to assert channel existence: %v", err)
		}

		chanPoints[i] = fundingChanPoint
	}

	// Next, close the channel between Alice and Carol, asserting that the
	// channel has been properly closed on-chain.
	for _, chanPoint := range chanPoints {
		ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
		closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
	}
}

// waitForTxInMempool polls until finding one transaction in the provided
// miner's mempool. An error is returned if *one* transaction isn't found within
// the given timeout.
func waitForTxInMempool(miner *rpcclient.Client,
	timeout time.Duration) (*chainhash.Hash, error) {

	txs, err := waitForNTxsInMempool(miner, 1, timeout)
	if err != nil {
		return nil, err
	}

	return txs[0], err
}

// waitForNTxsInMempool polls until finding the desired number of transactions
// in the provided miner's mempool. An error is returned if this number is not
// met after the given timeout.
func waitForNTxsInMempool(miner *rpcclient.Client, n int,
	timeout time.Duration) ([]*chainhash.Hash, error) {

	breakTimeout := time.After(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	var err error
	var mempool []*chainhash.Hash
	for {
		select {
		case <-breakTimeout:
			return nil, fmt.Errorf("wanted %v, found %v txs "+
				"in mempool: %v", n, len(mempool), mempool)
		case <-ticker.C:
			mempool, err = miner.GetRawMempool(context.Background(), jsonrpctypes.GRMRegular)
			if err != nil {
				return nil, err
			}

			if len(mempool) == n {
				return mempool, nil
			}
		}
	}
}

// getNTxsFromMempool polls until finding the desired number of transactions in
// the provided miner's mempool and returns the full transactions to the caller.
func getNTxsFromMempool(miner *rpcclient.Client, n int,
	timeout time.Duration) ([]*wire.MsgTx, error) {

	txids, err := waitForNTxsInMempool(miner, n, timeout)
	if err != nil {
		return nil, err
	}

	var txes []*wire.MsgTx
	for _, txid := range txids {
		ctxt, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		tx, err := miner.GetRawTransaction(ctxt, txid)
		if err != nil {
			return nil, err
		}
		txes = append(txes, tx.MsgTx())
	}
	return txes, nil
}

// testFailingChannel tests that we will fail the channel by force closing ii
// in the case where a counterparty tries to settle an HTLC with the wrong
// preimage.
func testFailingChannel(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const (
		paymentAmt = 10000
	)

	chanAmt := defaultChanAmt

	// We'll introduce Carol, which will settle any incoming invoice with a
	// totally unrelated preimage.
	carol, err := net.NewNode("Carol", []string{"--hodl.bogus-settle"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	// Let Alice connect and open a channel to Carol,
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect alice to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, carol,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// With the channel open, we'll create a invoice for Carol that Alice
	// will attempt to pay.
	preimage := bytes.Repeat([]byte{byte(192)}, 32)
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: preimage,
		Value:     paymentAmt,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	resp, err := carol.AddInvoice(ctxt, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}
	carolPayReqs := []string{resp.PaymentRequest}

	// Wait for Alice to receive the channel edge from the funding manager.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't see the alice->carol channel before "+
			"timeout: %v", err)
	}

	// Send the payment from Alice to Carol. We expect Carol to attempt to
	// settle this payment with the wrong preimage.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, net.Alice, net.Alice.RouterClient, carolPayReqs, false,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Since Alice detects that Carol is trying to trick her by providing a
	// fake preimage, she should fail and force close the channel.
	var predErr error
	err = wait.Predicate(func() bool {
		pendingChansRequest := &lnrpc.PendingChannelsRequest{}
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		pendingChanResp, err := net.Alice.PendingChannels(ctxt,
			pendingChansRequest)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}
		n := len(pendingChanResp.WaitingCloseChannels)
		if n != 1 {
			predErr = fmt.Errorf("Expected to find %d channels "+
				"waiting close, found %d", 1, n)
			return false
		}
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("%v", predErr)
	}

	// Mine a block to confirm the broadcasted commitment.
	block := mineBlocks(t, net, 1, 1)[0]
	if len(block.Transactions) != 2 {
		t.Fatalf("transaction wasn't mined")
	}

	// The channel should now show up as force closed both for Alice and
	// Carol.
	err = wait.Predicate(func() bool {
		pendingChansRequest := &lnrpc.PendingChannelsRequest{}
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		pendingChanResp, err := net.Alice.PendingChannels(ctxt,
			pendingChansRequest)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}
		n := len(pendingChanResp.WaitingCloseChannels)
		if n != 0 {
			predErr = fmt.Errorf("Expected to find %d channels "+
				"waiting close, found %d", 0, n)
			return false
		}
		n = len(pendingChanResp.PendingForceClosingChannels)
		if n != 1 {
			predErr = fmt.Errorf("expected to find %d channel "+
				"pending force close, found %d", 1, n)
			return false
		}
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("%v", predErr)
	}

	err = wait.Predicate(func() bool {
		pendingChansRequest := &lnrpc.PendingChannelsRequest{}
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		pendingChanResp, err := carol.PendingChannels(ctxt,
			pendingChansRequest)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}
		n := len(pendingChanResp.PendingForceClosingChannels)
		if n != 1 {
			predErr = fmt.Errorf("expected to find %d channel "+
				"pending force close, found %d", 1, n)
			return false
		}
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("%v", predErr)
	}

	// Carol will use the correct preimage to resolve the HTLC on-chain.
	_, err = waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find Carol's resolve tx in mempool: %v", err)
	}

	// Mine enough blocks for Alice to sweep her funds from the force
	// closed channel.
	_, err = net.Generate(defaultCSV - 1)
	if err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// Wait for the sweeping tx to be broadcast.
	_, err = waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find Alice's sweep tx in mempool: %v", err)
	}

	// Mine the sweep.
	_, err = net.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// No pending channels should be left.
	err = wait.Predicate(func() bool {
		pendingChansRequest := &lnrpc.PendingChannelsRequest{}
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		pendingChanResp, err := net.Alice.PendingChannels(ctxt,
			pendingChansRequest)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}
		n := len(pendingChanResp.PendingForceClosingChannels)
		if n != 0 {
			predErr = fmt.Errorf("expected to find %d channel "+
				"pending force close, found %d", 0, n)
			return false
		}
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("%v", predErr)
	}
}

// testGarbageCollectLinkNodes tests that we properly garbase collect link nodes
// from the database and the set of persistent connections within the server.
func testGarbageCollectLinkNodes(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const (
		chanAmt = 1000000
	)

	// Open a channel between Alice and Bob which will later be
	// cooperatively closed.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	coopChanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Create Carol's node and connect Alice to her.
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create carol's node: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect alice and carol: %v", err)
	}

	// Open a channel between Alice and Carol which will later be force
	// closed.
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	forceCloseChanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, carol,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Now, create Dave's a node and also open a channel between Alice and
	// him. This link will serve as the only persistent link throughout
	// restarts in this test.
	dave, err := net.NewNode("Dave", nil)
	if err != nil {
		t.Fatalf("unable to create dave's node: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, net.Alice, dave); err != nil {
		t.Fatalf("unable to connect alice to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	persistentChanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, dave,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// isConnected is a helper closure that checks if a peer is connected to
	// Alice.
	isConnected := func(pubKey string) bool {
		req := &lnrpc.ListPeersRequest{}
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		resp, err := net.Alice.ListPeers(ctxt, req)
		if err != nil {
			t.Fatalf("unable to retrieve alice's peers: %v", err)
		}

		for _, peer := range resp.Peers {
			if peer.PubKey == pubKey {
				return true
			}
		}

		return false
	}

	// Ensure there's enough time after opening the channel for all nodes
	// to process it before shutting them down, otherwise we might deadlock
	// waiting for the chain to shut down.
	//
	// See https://github.com/decred/dcrlnd/issues/97
	time.Sleep(time.Second)

	// Restart both Bob and Carol to ensure Alice is able to reconnect to
	// them.
	if err := net.RestartNode(net.Bob, nil); err != nil {
		t.Fatalf("unable to restart bob's node: %v", err)
	}
	if err := net.RestartNode(carol, nil); err != nil {
		t.Fatalf("unable to restart carol's node: %v", err)
	}

	err = wait.Predicate(func() bool {
		return isConnected(net.Bob.PubKeyStr)
	}, 15*time.Second)
	if err != nil {
		t.Fatalf("alice did not reconnect to bob")
	}
	err = wait.Predicate(func() bool {
		return isConnected(carol.PubKeyStr)
	}, 15*time.Second)
	if err != nil {
		t.Fatalf("alice did not reconnect to carol")
	}

	// We'll also restart Alice to ensure she can reconnect to her peers
	// with open channels.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("unable to restart alice's node: %v", err)
	}

	err = wait.Predicate(func() bool {
		return isConnected(net.Bob.PubKeyStr)
	}, 15*time.Second)
	if err != nil {
		t.Fatalf("alice did not reconnect to bob")
	}
	err = wait.Predicate(func() bool {
		return isConnected(carol.PubKeyStr)
	}, 15*time.Second)
	if err != nil {
		t.Fatalf("alice did not reconnect to carol")
	}
	err = wait.Predicate(func() bool {
		return isConnected(dave.PubKeyStr)
	}, 15*time.Second)
	if err != nil {
		t.Fatalf("alice did not reconnect to dave")
	}

	// testReconnection is a helper closure that restarts the nodes at both
	// ends of a channel to ensure they do not reconnect after restarting.
	// When restarting Alice, we'll first need to ensure she has
	// reestablished her connection with Dave, as they still have an open
	// channel together.
	testReconnection := func(node *lntest.HarnessNode) {
		// Restart both nodes, to trigger the pruning logic.
		if err := net.RestartNode(node, nil); err != nil {
			t.Fatalf("unable to restart %v's node: %v",
				node.Name(), err)
		}

		if err := net.RestartNode(net.Alice, nil); err != nil {
			t.Fatalf("unable to restart alice's node: %v", err)
		}

		// Now restart both nodes and make sure they don't reconnect.
		if err := net.RestartNode(node, nil); err != nil {
			t.Fatalf("unable to restart %v's node: %v", node.Name(),
				err)
		}
		err = wait.Invariant(func() bool {
			return !isConnected(node.PubKeyStr)
		}, 5*time.Second)
		if err != nil {
			t.Fatalf("alice reconnected to %v", node.Name())
		}

		if err := net.RestartNode(net.Alice, nil); err != nil {
			t.Fatalf("unable to restart alice's node: %v", err)
		}
		err = wait.Predicate(func() bool {
			return isConnected(dave.PubKeyStr)
		}, 20*time.Second)
		if err != nil {
			t.Fatalf("alice didn't reconnect to Dave")
		}

		err = wait.Invariant(func() bool {
			return !isConnected(node.PubKeyStr)
		}, 5*time.Second)
		if err != nil {
			t.Fatalf("alice reconnected to %v", node.Name())
		}
	}

	// Now, we'll close the channel between Alice and Bob and ensure there
	// is no reconnection logic between the both once the channel is fully
	// closed.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, coopChanPoint, false)

	testReconnection(net.Bob)

	// We'll do the same with Alice and Carol, but this time we'll force
	// close the channel instead.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, forceCloseChanPoint, true)

	// Cleanup by mining the force close and sweep transaction.
	cleanupForceClose(t, net, net.Alice, forceCloseChanPoint)

	// We'll need to mine some blocks in order to mark the channel fully
	// closed.
	_, err = net.Generate(dcrlnd.DefaultDecredTimeLockDelta - defaultCSV)
	if err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// Before we test reconnection, we'll ensure that the channel has been
	// fully cleaned up for both Carol and Alice.
	var predErr error
	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	err = wait.Predicate(func() bool {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		pendingChanResp, err := net.Alice.PendingChannels(
			ctxt, pendingChansRequest,
		)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}

		predErr = checkNumForceClosedChannels(pendingChanResp, 0)
		if predErr != nil {
			return false
		}

		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		pendingChanResp, err = carol.PendingChannels(
			ctxt, pendingChansRequest,
		)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}

		predErr = checkNumForceClosedChannels(pendingChanResp, 0)
		return predErr == nil
	}, time.Second*15)
	if err != nil {
		t.Fatalf("channels not marked as fully resolved: %v", predErr)
	}

	testReconnection(carol)

	// Finally, we'll ensure that Bob and Carol no longer show in Alice's
	// channel graph.
	describeGraphReq := &lnrpc.ChannelGraphRequest{
		IncludeUnannounced: true,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	channelGraph, err := net.Alice.DescribeGraph(ctxt, describeGraphReq)
	if err != nil {
		t.Fatalf("unable to query for alice's channel graph: %v", err)
	}
	for _, node := range channelGraph.Nodes {
		if node.PubKey == net.Bob.PubKeyStr {
			t.Logf("Full channel graph: %s", spew.Sdump(channelGraph))
			t.Fatalf("did not expect to find bob in the channel " +
				"graph, but did")
		}
		if node.PubKey == carol.PubKeyStr {
			t.Logf("Full channel graph: %s", spew.Sdump(channelGraph))
			t.Fatalf("did not expect to find carol in the channel " +
				"graph, but did")
		}
	}

	// Now that the test is done, we can also close the persistent link.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, persistentChanPoint, false)
}

// testRevokedCloseRetribution tests that Carol is able carry out
// retribution in the event that she fails immediately after detecting Bob's
// breach txn in the mempool.
func testRevokedCloseRetribution(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const (
		chanAmt     = defaultChanAmt
		paymentAmt  = 10000
		numInvoices = 6
	)

	// Carol will be the breached party. We set --nolisten to ensure Bob
	// won't be able to connect to her and trigger the channel data
	// protection logic automatically. We also can't have Carol
	// automatically re-connect too early, otherwise DLP would be initiated
	// instead of the breach we want to provoke.
	carol, err := net.NewNode(
		"Carol",
		[]string{"--hodl.exit-settle", "--nolisten", "--minbackoff=1h"},
	)
	if err != nil {
		t.Fatalf("unable to create new carol node: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	// We must let Bob communicate with Carol before they are able to open
	// channel, so we connect Bob and Carol,
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	if err := net.EnsureConnected(ctxt, carol, net.Bob); err != nil {
		t.Fatalf("unable to connect dave to carol: %v", err)
	}

	// Before we make a channel, we'll load up Carol with some coins sent
	// directly from the miner.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}

	// In order to test Carol's response to an uncooperative channel
	// closure by Bob, we'll first open up a channel between them with a
	// 0.5 DCR value.
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, carol, net.Bob,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// With the channel open, we'll create a few invoices for Bob that
	// Carol will pay to in order to advance the state of the channel.
	bobPayReqs, _, _, err := createPayReqs(
		net.Bob, paymentAmt, numInvoices,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// Wait for Carol to receive the channel edge from the funding manager.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("carol didn't see the carol->bob channel before "+
			"timeout: %v", err)
	}

	// Send payments from Carol to Bob using 3 of Bob's payment hashes
	// generated above.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, carol, carol.RouterClient, bobPayReqs[:numInvoices/2],
		true,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Next query for Bob's channel state, as we sent 3 payments of 10k
	// atoms each, Bob should now see his balance as being 30k atoms.
	var bobChan *lnrpc.Channel
	var predErr error
	err = wait.Predicate(func() bool {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		bChan, err := getChanInfo(ctxt, net.Bob)
		if err != nil {
			t.Fatalf("unable to get bob's channel info: %v", err)
		}
		if bChan.LocalBalance != 30000 {
			predErr = fmt.Errorf("bob's balance is incorrect, "+
				"got %v, expected %v", bChan.LocalBalance,
				30000)
			return false
		}

		bobChan = bChan
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("%v", predErr)
	}

	// Grab Bob's current commitment height (update number), we'll later
	// revert him to this state after additional updates to force him to
	// broadcast this soon to be revoked state.
	bobStateNumPreCopy := bobChan.NumUpdates

	// Create a temporary file to house Bob's database state at this
	// particular point in history.
	bobTempDbPath, err := ioutil.TempDir("", "bob-past-state")
	if err != nil {
		t.Fatalf("unable to create temp db folder: %v", err)
	}
	bobTempDbFile := filepath.Join(bobTempDbPath, "channel.db")
	defer os.Remove(bobTempDbPath)

	// With the temporary file created, copy Bob's current state into the
	// temporary file we created above. Later after more updates, we'll
	// restore this state.
	if err := lntest.CopyFile(bobTempDbFile, net.Bob.DBPath()); err != nil {
		t.Fatalf("unable to copy database files: %v", err)
	}

	// Finally, send payments from Carol to Bob, consuming Bob's remaining
	// payment hashes.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, carol, carol.RouterClient, bobPayReqs[numInvoices/2:],
		true,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	bobChan, err = getChanInfo(ctxt, net.Bob)
	if err != nil {
		t.Fatalf("unable to get bob chan info: %v", err)
	}

	// Disconnect the nodes to prevent Carol trying to reconnect to Bob after
	// Bob is restarted and fixing the wrong state.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.DisconnectNodes(ctxt, carol, net.Bob)
	if err != nil {
		t.Fatalf("unable to disconnect carol and bob: %v", err)
	}

	// Now we shutdown Bob, copying over the his temporary database state
	// which has the *prior* channel state over his current most up to date
	// state. With this, we essentially force Bob to travel back in time
	// within the channel's history.
	if err = net.RestartNode(net.Bob, func() error {
		return os.Rename(bobTempDbFile, net.Bob.DBPath())
	}); err != nil {
		t.Fatalf("unable to restart node: %v", err)
	}

	// Now query for Bob's channel state, it should show that he's at a
	// state number in the past, not the *latest* state.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	bobChan, err = getChanInfo(ctxt, net.Bob)
	if err != nil {
		t.Fatalf("unable to get bob chan info: %v", err)
	}
	if bobChan.NumUpdates != bobStateNumPreCopy {
		t.Fatalf("db copy failed: %v", bobChan.NumUpdates)
	}

	// Now force Bob to execute a *force* channel closure by unilaterally
	// broadcasting his current channel state. This is actually the
	// commitment transaction of a prior *revoked* state, so he'll soon
	// feel the wrath of Carol's retribution.
	var closeUpdates lnrpc.Lightning_CloseChannelClient
	force := true
	err = wait.Predicate(func() bool {
		ctxt, _ := context.WithTimeout(ctxb, channelCloseTimeout)
		closeUpdates, _, err = net.CloseChannel(ctxt, net.Bob, chanPoint, force)
		if err != nil {
			predErr = err
			return false
		}

		return true
	}, time.Second*10)
	if err != nil {
		t.Fatalf("unable to close channel: %v", predErr)
	}

	// Wait for Bob's breach transaction to show up in the mempool to ensure
	// that Carol's node has started waiting for confirmations.
	_, err = waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find Bob's breach tx in mempool: %v", err)
	}

	// Here, Carol sees Bob's breach transaction in the mempool, but is waiting
	// for it to confirm before continuing her retribution. We restart Carol to
	// ensure that she is persisting her retribution state and continues
	// watching for the breach transaction to confirm even after her node
	// restarts.
	if err := net.RestartNode(carol, nil); err != nil {
		t.Fatalf("unable to restart Carol's node: %v", err)
	}

	// Finally, generate a single block, wait for the final close status
	// update, then ensure that the closing transaction was included in the
	// block.
	block := mineBlocks(t, net, 1, 1)[0]

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	breachTXID, err := net.WaitForChannelClose(ctxt, closeUpdates)
	if err != nil {
		t.Fatalf("error while waiting for channel close: %v", err)
	}
	assertTxInBlock(t, block, breachTXID)

	// Query the mempool for Carol's justice transaction, this should be
	// broadcast as Bob's contract breaching transaction gets confirmed
	// above.
	justiceTXID, err := waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find Carol's justice tx in mempool: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// Query for the mempool transaction found above. Then assert that all
	// the inputs of this transaction are spending outputs generated by
	// Bob's breach transaction above.
	justiceTx, err := net.Miner.Node.GetRawTransaction(context.Background(), justiceTXID)
	if err != nil {
		t.Fatalf("unable to query for justice tx: %v", err)
	}
	for _, txIn := range justiceTx.MsgTx().TxIn {
		if !bytes.Equal(txIn.PreviousOutPoint.Hash[:], breachTXID[:]) {
			t.Fatalf("justice tx not spending commitment utxo "+
				"instead is: %v", txIn.PreviousOutPoint)
		}
	}

	// We restart Carol here to ensure that she persists her retribution state
	// and successfully continues exacting retribution after restarting. At
	// this point, Carol has broadcast the justice transaction, but it hasn't
	// been confirmed yet; when Carol restarts, she should start waiting for
	// the justice transaction to confirm again.
	if err := net.RestartNode(carol, nil); err != nil {
		t.Fatalf("unable to restart Carol's node: %v", err)
	}

	// Now mine a block, this transaction should include Carol's justice
	// transaction which was just accepted into the mempool.
	block = mineBlocks(t, net, 1, 1)[0]

	// The block should have exactly *two* transactions, one of which is
	// the justice transaction.
	if len(block.Transactions) != 2 {
		t.Fatalf("transaction wasn't mined")
	}
	justiceSha := block.Transactions[1].TxHash()
	if !bytes.Equal(justiceTx.Hash()[:], justiceSha[:]) {
		t.Fatalf("justice tx wasn't mined")
	}

	assertNodeNumChannels(t, carol, 0)

	// Mine enough blocks for Bob's channel arbitrator to wrap up the
	// references to the breached channel. The chanarb waits for commitment
	// tx's confHeight+CSV-1 blocks and since we've already mined one that
	// included the justice tx we only need to mine extra DefaultCSV-2
	// blocks to unlock it.
	mineBlocks(t, net, lntest.DefaultCSV-2, 0)

	assertNumPendingChannels(t, net.Bob, 0, 0, 0, 0)
}

// testRevokedCloseRetributionZeroValueRemoteOutput tests that Dave is able
// carry out retribution in the event that she fails in state where the remote
// commitment output has zero-value.
func testRevokedCloseRetributionZeroValueRemoteOutput(net *lntest.NetworkHarness,
	t *harnessTest) {
	ctxb := context.Background()

	const (
		chanAmt     = defaultChanAmt
		paymentAmt  = 10000
		numInvoices = 6
	)

	// Since we'd like to test some multi-hop failure scenarios, we'll
	// introduce another node into our test network: Carol.
	carol, err := net.NewNode("Carol", []string{"--hodl.exit-settle"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	// Dave will be the breached party. We set --nolisten to ensure Carol
	// won't be able to connect to him and trigger the channel data
	// protection logic automatically. We also can't have Dave automatically
	// re-connect too early, otherwise DLP would be initiated instead of the
	// breach we want to provoke.
	dave, err := net.NewNode(
		"Dave",
		[]string{"--hodl.exit-settle", "--nolisten", "--minbackoff=1h"},
	)
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	// We must let Dave have an open channel before she can send a node
	// announcement, so we open a channel with Carol,
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, dave, carol); err != nil {
		t.Fatalf("unable to connect dave to carol: %v", err)
	}

	// Before we make a channel, we'll load up Dave with some coins sent
	// directly from the miner.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, dave)
	if err != nil {
		t.Fatalf("unable to send coins to dave: %v", err)
	}

	// In order to test Dave's response to an uncooperative channel
	// closure by Carol, we'll first open up a channel between them with a
	// 0.5 DCR value.
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, dave, carol,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// With the channel open, we'll create a few invoices for Carol that
	// Dave will pay to in order to advance the state of the channel.
	carolPayReqs, _, _, err := createPayReqs(
		carol, paymentAmt, numInvoices,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// Wait for Dave to receive the channel edge from the funding manager.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = dave.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("dave didn't see the dave->carol channel before "+
			"timeout: %v", err)
	}

	// Next query for Carol's channel state, as we sent 0 payments, Carol
	// should now see her balance as being 0 atoms.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	carolChan, err := getChanInfo(ctxt, carol)
	if err != nil {
		t.Fatalf("unable to get carol's channel info: %v", err)
	}
	if carolChan.LocalBalance != 0 {
		t.Fatalf("carol's balance is incorrect, got %v, expected %v",
			carolChan.LocalBalance, 0)
	}

	// Grab Carol's current commitment height (update number), we'll later
	// revert her to this state after additional updates to force him to
	// broadcast this soon to be revoked state.
	carolStateNumPreCopy := carolChan.NumUpdates

	// Create a temporary file to house Carol's database state at this
	// particular point in history.
	carolTempDbPath, err := ioutil.TempDir("", "carol-past-state")
	if err != nil {
		t.Fatalf("unable to create temp db folder: %v", err)
	}
	carolTempDbFile := filepath.Join(carolTempDbPath, "channel.db")
	defer os.Remove(carolTempDbPath)

	// With the temporary file created, copy Carol's current state into the
	// temporary file we created above. Later after more updates, we'll
	// restore this state.
	if err := lntest.CopyFile(carolTempDbFile, carol.DBPath()); err != nil {
		t.Fatalf("unable to copy database files: %v", err)
	}

	// Finally, send payments from Dave to Carol, consuming Carol's remaining
	// payment hashes.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, dave, dave.RouterClient, carolPayReqs, false,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	carolChan, err = getChanInfo(ctxt, carol)
	if err != nil {
		t.Fatalf("unable to get carol chan info: %v", err)
	}

	// Disconnect Dave from Carol, so that upon Carol's restart he doesn't
	// try to automatically reconnect and alert her of the changed state.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.DisconnectNodes(ctxt, dave, carol)
	if err != nil {
		t.Fatalf("unable to disconnect dave and carol: %v", err)
	}

	// Now we shutdown Carol, copying over the his temporary database state
	// which has the *prior* channel state over his current most up to date
	// state. With this, we essentially force Carol to travel back in time
	// within the channel's history.
	if err = net.RestartNode(carol, func() error {
		return os.Rename(carolTempDbFile, carol.DBPath())
	}); err != nil {
		t.Fatalf("unable to restart node: %v", err)
	}

	// Now query for Carol's channel state, it should show that he's at a
	// state number in the past, not the *latest* state.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	carolChan, err = getChanInfo(ctxt, carol)
	if err != nil {
		t.Fatalf("unable to get carol chan info: %v", err)
	}
	if carolChan.NumUpdates != carolStateNumPreCopy {
		t.Fatalf("db copy failed: %v", carolChan.NumUpdates)
	}

	// Now force Carol to execute a *force* channel closure by unilaterally
	// broadcasting his current channel state. This is actually the
	// commitment transaction of a prior *revoked* state, so he'll soon
	// feel the wrath of Dave's retribution.
	var (
		closeUpdates lnrpc.Lightning_CloseChannelClient
		closeTxId    *chainhash.Hash
		closeErr     error
		force        bool = true
	)
	err = wait.Predicate(func() bool {
		ctxt, _ := context.WithTimeout(ctxb, channelCloseTimeout)
		closeUpdates, closeTxId, closeErr = net.CloseChannel(
			ctxt, carol, chanPoint, force,
		)
		return closeErr == nil
	}, time.Second*15)
	if err != nil {
		t.Fatalf("unable to close channel: %v", closeErr)
	}

	// Query the mempool for the breaching closing transaction, this should
	// be broadcast by Carol when she force closes the channel above.
	txid, err := waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find Carol's force close tx in mempool: %v",
			err)
	}
	if *txid != *closeTxId {
		t.Fatalf("expected closeTx(%v) in mempool, instead found %v",
			closeTxId, txid)
	}

	// Finally, generate a single block, wait for the final close status
	// update, then ensure that the closing transaction was included in the
	// block.
	block := mineBlocks(t, net, 1, 1)[0]

	// Give Dave some time to process and broadcast the brach transaction
	time.Sleep(time.Second * 3)

	// Here, Dave receives a confirmation of Carol's breach transaction.
	// We restart Dave to ensure that she is persisting her retribution
	// state and continues exacting justice after her node restarts.
	if err := net.RestartNode(dave, nil); err != nil {
		t.Fatalf("unable to stop Dave's node: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	breachTXID, err := net.WaitForChannelClose(ctxt, closeUpdates)
	if err != nil {
		t.Fatalf("error while waiting for channel close: %v", err)
	}
	assertTxInBlock(t, block, breachTXID)

	// Query the mempool for Dave's justice transaction, this should be
	// broadcast as Carol's contract breaching transaction gets confirmed
	// above.
	justiceTXID, err := waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find Dave's justice tx in mempool: %v",
			err)
	}
	time.Sleep(100 * time.Millisecond)

	// Query for the mempool transaction found above. Then assert that all
	// the inputs of this transaction are spending outputs generated by
	// Carol's breach transaction above.
	justiceTx, err := net.Miner.Node.GetRawTransaction(context.Background(), justiceTXID)
	if err != nil {
		t.Fatalf("unable to query for justice tx: %v", err)
	}
	for _, txIn := range justiceTx.MsgTx().TxIn {
		if !bytes.Equal(txIn.PreviousOutPoint.Hash[:], breachTXID[:]) {
			t.Fatalf("justice tx not spending commitment utxo "+
				"instead is: %v", txIn.PreviousOutPoint)
		}
	}

	// We restart Dave here to ensure that he persists her retribution state
	// and successfully continues exacting retribution after restarting. At
	// this point, Dave has broadcast the justice transaction, but it hasn't
	// been confirmed yet; when Dave restarts, she should start waiting for
	// the justice transaction to confirm again.
	if err := net.RestartNode(dave, nil); err != nil {
		t.Fatalf("unable to restart Dave's node: %v", err)
	}

	// Now mine a block, this transaction should include Dave's justice
	// transaction which was just accepted into the mempool.
	block = mineBlocks(t, net, 1, 1)[0]

	// The block should have exactly *two* transactions, one of which is
	// the justice transaction.
	if len(block.Transactions) != 2 {
		t.Fatalf("transaction wasn't mined")
	}
	justiceSha := block.Transactions[1].TxHash()
	if !bytes.Equal(justiceTx.Hash()[:], justiceSha[:]) {
		t.Fatalf("justice tx wasn't mined")
	}

	assertNodeNumChannels(t, dave, 0)
}

// testRevokedCloseRetributionRemoteHodl tests that Dave properly responds to a
// channel breach made by the remote party, specifically in the case that the
// remote party breaches before settling extended HTLCs.
//
// This test specifically suspends Carol after she sends her breach commitment
// so that we test the scenario where she does _not_ go to the second level
// before Dave is able to exact justice.
func testRevokedCloseRetributionRemoteHodl(net *lntest.NetworkHarness,
	t *harnessTest) {
	ctxb := context.Background()

	const (
		initialBalance = int64(dcrutil.AtomsPerCoin)
		chanAmt        = defaultChanAmt
		pushAmt        = 400000
		paymentAmt     = 20000
		numInvoices    = 6
	)

	// Since this test will result in the counterparty being left in a
	// weird state, we will introduce another node into our test network:
	// Carol.
	carol, err := net.NewNode("Carol", []string{"--hodl.exit-settle"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	// We'll also create a new node Dave, who will have a channel with
	// Carol, and also use similar settings so we can broadcast a commit
	// with active HTLCs. Dave will be the breached party. We set
	// --nolisten to ensure Carol won't be able to connect to him and
	// trigger the channel data protection logic automatically.
	dave, err := net.NewNode(
		"Dave",
		[]string{"--hodl.exit-settle", "--nolisten"},
	)
	if err != nil {
		t.Fatalf("unable to create new dave node: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	// We must let Dave communicate with Carol before they are able to open
	// channel, so we connect Dave and Carol,
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, dave, carol); err != nil {
		t.Fatalf("unable to connect dave to carol: %v", err)
	}

	// Before we make a channel, we'll load up Dave with some coins sent
	// directly from the miner.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.Amount(initialBalance), dave)
	if err != nil {
		t.Fatalf("unable to send coins to dave: %v", err)
	}

	// In order to test Dave's response to an uncooperative channel closure
	// by Carol, we'll first open up a channel between them with a
	// defaultChanAmt (2^24) atoms value.
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, dave, carol,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)

	// Store the channel type for later.
	cType, err := channelCommitType(carol, chanPoint)
	if err != nil {
		t.Fatalf("unable to get channel type: %v", err)
	}

	// With the channel open, we'll create a few invoices for Carol that
	// Dave will pay to in order to advance the state of the channel.
	carolPayReqs, _, _, err := createPayReqs(
		carol, paymentAmt, numInvoices,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// We'll introduce a closure to validate that Carol's current balance
	// matches the given expected amount.
	checkCarolBalance := func(expectedAmt int64) {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		carolChan, err := getChanInfo(ctxt, carol)
		if err != nil {
			t.Fatalf("unable to get carol's channel info: %v", err)
		}
		if carolChan.LocalBalance != expectedAmt {
			t.Fatalf("carol's balance is incorrect, "+
				"got %v, expected %v", carolChan.LocalBalance,
				expectedAmt)
		}
	}

	// We'll introduce another closure to validate that Carol's current
	// number of updates is at least as large as the provided minimum
	// number.
	checkCarolNumUpdatesAtLeast := func(minimum uint64) {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		carolChan, err := getChanInfo(ctxt, carol)
		if err != nil {
			t.Fatalf("unable to get carol's channel info: %v", err)
		}
		if carolChan.NumUpdates < minimum {
			t.Fatalf("carol's numupdates is incorrect, want %v "+
				"to be at least %v", carolChan.NumUpdates,
				minimum)
		}
	}

	// Wait for Dave to receive the channel edge from the funding manager.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = dave.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("dave didn't see the dave->carol channel before "+
			"timeout: %v", err)
	}

	// Ensure that carol's balance starts with the amount we pushed to her.
	checkCarolBalance(pushAmt)

	// Send payments from Dave to Carol using 3 of Carol's payment hashes
	// generated above.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, dave, dave.RouterClient, carolPayReqs[:numInvoices/2],
		false,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// At this point, we'll also send over a set of HTLC's from Carol to
	// Dave. This ensures that the final revoked transaction has HTLC's in
	// both directions.
	davePayReqs, _, _, err := createPayReqs(
		dave, paymentAmt, numInvoices,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// Send payments from Carol to Dave using 3 of Dave's payment hashes
	// generated above.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, carol, carol.RouterClient, davePayReqs[:numInvoices/2],
		false,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Wait until the channel has acknowledged the pending htlc count.
	err = waitForPendingHtlcs(carol, chanPoint, numInvoices)
	if err != nil {
		t.Fatalf("unable to wait for pending htlcs: %v", err)
	}

	// Next query for Carol's channel state, as we sent 3 payments of 10k
	// atoms each, however Carol should now see her balance as being
	// equal to the push amount in atoms since she has not settled.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	carolChan, err := getChanInfo(ctxt, carol)
	if err != nil {
		t.Fatalf("unable to get carol's channel info: %v", err)
	}

	// Grab Carol's current commitment height (update number), we'll later
	// revert her to this state after additional updates to force her to
	// broadcast this soon to be revoked state.
	carolStateNumPreCopy := carolChan.NumUpdates

	// Ensure that carol's balance still reflects the original amount we
	// pushed to her, minus the HTLCs she just sent to Dave.
	checkCarolBalance(pushAmt - 3*paymentAmt)

	// Since Carol has not settled, she should only see at least one update
	// to her channel.
	checkCarolNumUpdatesAtLeast(1)

	// Create a temporary file to house Carol's database state at this
	// particular point in history.
	carolTempDbPath, err := ioutil.TempDir("", "carol-past-state")
	if err != nil {
		t.Fatalf("unable to create temp db folder: %v", err)
	}
	carolTempDbFile := filepath.Join(carolTempDbPath, "channel.db")
	defer os.Remove(carolTempDbPath)

	// With the temporary file created, copy Carol's current state into the
	// temporary file we created above. Later after more updates, we'll
	// restore this state.
	if err := lntest.CopyFile(carolTempDbFile, carol.DBPath()); err != nil {
		t.Fatalf("unable to copy database files: %v", err)
	}

	// Finally, send payments from Dave to Carol, consuming Carol's
	// remaining payment hashes.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, dave, dave.RouterClient, carolPayReqs[numInvoices/2:],
		false,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Ensure that carol's balance still shows the amount we originally
	// pushed to her (minus the HTLCs she sent to Bob), and that at least
	// one more update has occurred.
	time.Sleep(500 * time.Millisecond)
	checkCarolBalance(pushAmt - 3*paymentAmt)
	checkCarolNumUpdatesAtLeast(carolStateNumPreCopy + 1)

	// Disconnect Carol and Dave, so that the channel isn't corrected once Carol
	// is restarted.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.DisconnectNodes(ctxt, dave, carol)
	if err != nil {
		t.Fatalf("unable to disconnect dave and carol: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	// Now we shutdown Carol, copying over the her temporary database state
	// which has the *prior* channel state over her current most up to date
	// state. With this, we essentially force Carol to travel back in time
	// within the channel's history.
	if err = net.RestartNode(carol, func() error {
		return os.Rename(carolTempDbFile, carol.DBPath())
	}); err != nil {
		t.Fatalf("unable to restart node: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	// Ensure that Carol's view of the channel is consistent with the state
	// of the channel just before it was snapshotted.
	checkCarolBalance(pushAmt - 3*paymentAmt)
	checkCarolNumUpdatesAtLeast(1)

	// Now query for Carol's channel state, it should show that she's at a
	// state number in the past, *not* the latest state.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	carolChan, err = getChanInfo(ctxt, carol)
	if err != nil {
		t.Fatalf("unable to get carol chan info: %v", err)
	}
	if carolChan.NumUpdates != carolStateNumPreCopy {
		t.Fatalf("db copy failed: %v", carolChan.NumUpdates)
	}

	// Now force Carol to execute a *force* channel closure by unilaterally
	// broadcasting her current channel state. This is actually the
	// commitment transaction of a prior *revoked* state, so she'll soon
	// feel the wrath of Dave's retribution.
	force := true
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	_, closeTxId, err := net.CloseChannel(ctxt, carol,
		chanPoint, force)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}

	// Query the mempool for the breaching closing transaction, this should
	// be broadcast by Carol when she force closes the channel above.
	txid, err := waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find Carol's force close tx in mempool: %v",
			err)
	}
	if *txid != *closeTxId {
		t.Fatalf("expected closeTx(%v) in mempool, instead found %v",
			closeTxId, txid)
	}
	time.Sleep(200 * time.Millisecond)

	// Suspend Carol, so that she won't have a chance to go to the second
	// level to redeem the coins before Dave sees the breach tx.
	resumeCarol, err := net.SuspendNode(carol)
	if err != nil {
		t.Fatalf("unable to suspend carol: %v", err)
	}

	// Generate a single block to mine the breach transaction and ensure
	// that it was mined.
	block := mineBlocks(t, net, 1, 1)[0]
	assertTxInBlock(t, block, closeTxId)

	// Wait so Dave receives a confirmation of Carol's breach transaction.
	time.Sleep(2 * time.Second)

	// We restart Dave to ensure that he is persisting his retribution
	// state and continues exacting justice after her node restarts.
	if err := net.RestartNode(dave, nil); err != nil {
		t.Fatalf("unable to stop Dave's node: %v", err)
	}

	// Query the mempool for Dave's justice transaction. This should be
	// broadcast as Carol's contract breaching transaction gets confirmed
	// above. To find it, we'll search through the mempool for a tx that
	// matches the number of expected inputs in the justice tx.
	var predErr error
	var justiceTxid *chainhash.Hash
	exNumInputs := 2 + numInvoices
	errNotFound := fmt.Errorf("justice tx with %d inputs not found", exNumInputs)
	findJusticeTx := func() (*chainhash.Hash, error) {
		mempool, err := net.Miner.Node.GetRawMempool(context.Background(), jsonrpctypes.GRMRegular)
		if err != nil {
			return nil, fmt.Errorf("unable to get mempool from "+
				"miner: %v", err)
		}

		for _, txid := range mempool {
			// Check that the justice tx has the appropriate number
			// of inputs.
			tx, err := net.Miner.Node.GetRawTransaction(context.Background(), txid)
			if err != nil {
				return nil, fmt.Errorf("unable to query for "+
					"txs: %v", err)
			}

			if len(tx.MsgTx().TxIn) == exNumInputs {
				return txid, nil
			}
		}
		return nil, errNotFound
	}

	err = wait.Predicate(func() bool {
		txid, err := findJusticeTx()
		if err != nil {
			predErr = err
			return false
		}

		justiceTxid = txid
		return true
	}, time.Second*10)
	if err != nil {
		t.Fatalf("unable to find dave's justice tx: %v", predErr)
	}

	// Grab some transactions which will be needed for future checks.
	justiceTx, err := net.Miner.Node.GetRawTransaction(context.Background(), justiceTxid)
	if err != nil {
		t.Fatalf("unable to query for justice tx: %v", err)
	}

	breachTx, err := net.Miner.Node.GetRawTransaction(context.Background(), closeTxId)
	if err != nil {
		t.Fatalf("unable to query for breach tx: %v", err)
	}

	fundingTxId, err := chainhash.NewHash(chanPoint.GetFundingTxidBytes())
	if err != nil {
		t.Fatalf("chainpoint id bytes not a chainhash: %v", err)
	}
	fundingTx, err := net.Miner.Node.GetRawTransaction(context.Background(), fundingTxId)
	if err != nil {
		t.Fatalf("unable to query for funding tx: %v", err)
	}

	// Check that all the inputs of the justice transaction are spending
	// outputs generated by Carol's breach transaction above. We'll track
	// the total amount sent into the justice tx so we can check for Dave's
	// balance later on.
	var totalJusticeIn int64
	for _, txIn := range justiceTx.MsgTx().TxIn {
		if bytes.Equal(txIn.PreviousOutPoint.Hash[:], closeTxId[:]) {
			txo := breachTx.MsgTx().TxOut[txIn.PreviousOutPoint.Index]
			totalJusticeIn += txo.Value
			continue
		}

		t.Fatalf("justice tx not spending commitment utxo "+
			"instead is: %v", txIn.PreviousOutPoint)
	}

	// The amount returned by the justice tx must be the total channel
	// amount minus the (breached) commitment tx fee and the justice tx fee
	// itself.
	jtx := justiceTx.MsgTx()
	commitFee := int64(cType.calcStaticFee(6))
	justiceFee := totalJusticeIn - jtx.TxOut[0].Value
	expectedJusticeOut := int64(chanAmt) - commitFee - justiceFee
	if jtx.TxOut[0].Value != expectedJusticeOut {
		t.Fatalf("wrong value returned by justice tx; expected=%d "+
			"actual=%d chanAmt=%d commitFee=%d justiceFee=%d",
			expectedJusticeOut, jtx.TxOut[0].Value, chanAmt,
			commitFee, justiceFee)
	}

	// Restart Carol. She should not be able to do anything else regarding
	// the breached channel.
	err = resumeCarol()
	if err != nil {
		t.Fatalf("unable to resume Carol: %v", err)
	}

	// We restart Dave here to ensure that he persists he retribution state
	// and successfully continues exacting retribution after restarting. At
	// this point, Dave has broadcast the justice transaction, but it
	// hasn't been confirmed yet; when Dave restarts, he should start
	// waiting for the justice transaction to confirm again.
	if err := net.RestartNode(dave, nil); err != nil {
		t.Fatalf("unable to restart Dave's node: %v", err)
	}

	// Now mine a block. This should include Dave's justice transaction
	// which was just accepted into the mempool.
	block = mineBlocks(t, net, 1, 1)[0]
	assertTxInBlock(t, block, justiceTxid)

	// Dave and Carol should have no open channels.
	assertNodeNumChannels(t, dave, 0)
	assertNodeNumChannels(t, carol, 0)

	// We'll now check that the total balance of each party is the one we
	// expect. Introduce a new test closure.
	checkTotalBalance := func(node *lntest.HarnessNode, expectedAmt int64) {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		balances, err := node.WalletBalance(ctxt, &lnrpc.WalletBalanceRequest{})
		if err != nil {
			t.Fatalf("unable to query node balance: %v", err)
		}
		if balances.TotalBalance != expectedAmt {
			t.Fatalf("balance is incorrect, "+
				"got %v, expected %v", balances.TotalBalance,
				expectedAmt)
		}
	}

	// Carol should not have any balance, because even though she was sent
	// coins via an HTLC, she chose to broadcast an older commitment tx and
	// forfeit her coins.
	checkTotalBalance(carol, 0)

	// Dave should the initial amount sent to him for funding channels
	// minus the fees required to broadcast the breached commitment with 6
	// HTLCs and the justice tx.
	fundingFee := recordedTxFee(fundingTx.MsgTx())
	checkTotalBalance(dave, initialBalance-commitFee-justiceFee-fundingFee)
}

// testRevokedCloseRetributionRemoteHodlSecondLevel tests that Dave properly
// responds to a channel breach made by the remote party, specifically in the
// case that the remote party breaches before settling extended HTLCs.
//
// In this test we specifically wait for Carol to redeem her HTLCs via a second
// level transaction before initiating Dave's retribution detection and justice
// service.
func testRevokedCloseRetributionRemoteHodlSecondLevel(net *lntest.NetworkHarness,
	t *harnessTest) {
	ctxb := context.Background()

	// Currently disabled in SPV due to
	// https://github.com/decred/dcrlnd/issues/96. Re-assess after that is
	// fixed.
	if net.BackendCfg.Name() == "spv" {
		t.Skipf("Skipping for SPV for the moment")
	}

	const (
		initialBalance = int64(dcrutil.AtomsPerCoin)
		chanAmt        = defaultChanAmt
		pushAmt        = 400000
		paymentAmt     = 20000
		numInvoices    = 6
	)

	// In order to make the test non-flaky and easier to reason about,
	// we'll redefine the CSV and CLTV limits for Carol and Dave. We'll
	// define these limits such that, after force-closing the channel (with
	// carol's older, revoked state) we can mine a few blocks before she'll
	// attempt to sweep the HTLCs.

	var (
		// The new CLTV delta will be the minimum the node will accept
		// + 4 so that it doesn't immediately close the channel after
		// restarting.
		cltvDelta = routing.MinCLTVDelta + 4

		// The new CSV delay will be the previous delta + 2 so that
		// Carol doesn't attempt to redeem the CSV-encumbered
		// commitment output before Dave can send the justice tx.
		csvDelay = cltvDelta + 2 + int(routing.BlockPadding)
	)

	// Since this test will result in the counterparty being left in a
	// weird state, we will introduce another node into our test network:
	// Carol.
	carol, err := net.NewNode("Carol", []string{
		"--hodl.exit-settle",
		fmt.Sprintf("--timelockdelta=%d", cltvDelta),
		fmt.Sprintf("--defaultremotedelay=%d", csvDelay),
	})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	// We'll also create a new node Dave, who will have a channel with
	// Carol, and also use similar settings so we can broadcast a commit
	// with active HTLCs. Dave will be the breached party. We set
	// --nolisten to ensure Carol won't be able to connect to him and
	// trigger the channel data protection logic automatically.
	dave, err := net.NewNode("Dave", []string{
		"--hodl.exit-settle",
		"--nolisten",
		fmt.Sprintf("--timelockdelta=%d", cltvDelta),
		fmt.Sprintf("--defaultremotedelay=%d", csvDelay),
	})
	if err != nil {
		t.Fatalf("unable to create new dave node: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	// We must let Dave communicate with Carol before they are able to open
	// channel, so we connect Dave and Carol,
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, dave, carol); err != nil {
		t.Fatalf("unable to connect dave to carol: %v", err)
	}

	// Before we make a channel, we'll load up Dave with some coins sent
	// directly from the miner.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.Amount(initialBalance), dave)
	if err != nil {
		t.Fatalf("unable to send coins to dave: %v", err)
	}

	// In order to test Dave's response to an uncooperative channel closure
	// by Carol, we'll first open up a channel between them with a
	// defaultChanAmt (2^24) atoms value.
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, dave, carol,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)

	// Store the channel type for later.
	cType, err := channelCommitType(carol, chanPoint)
	if err != nil {
		t.Fatalf("unable to get channel type: %v", err)
	}

	// With the channel open, we'll create a few invoices for Carol that
	// Dave will pay to in order to advance the state of the channel.
	carolPayReqs, _, _, err := createPayReqs(
		carol, paymentAmt, numInvoices,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// We'll introduce a closure to validate that Carol's current balance
	// matches the given expected amount.
	checkCarolBalance := func(expectedAmt int64) {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		carolChan, err := getChanInfo(ctxt, carol)
		if err != nil {
			t.Fatalf("unable to get carol's channel info: %v", err)
		}
		if carolChan.LocalBalance != expectedAmt {
			t.Fatalf("carol's balance is incorrect, "+
				"got %v, expected %v", carolChan.LocalBalance,
				expectedAmt)
		}
	}

	// We'll introduce another closure to validate that Carol's current
	// number of updates is at least as large as the provided minimum
	// number.
	checkCarolNumUpdatesAtLeast := func(minimum uint64) {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		carolChan, err := getChanInfo(ctxt, carol)
		if err != nil {
			t.Fatalf("unable to get carol's channel info: %v", err)
		}
		if carolChan.NumUpdates < minimum {
			t.Fatalf("carol's numupdates is incorrect, want %v "+
				"to be at least %v", carolChan.NumUpdates,
				minimum)
		}
	}

	// Wait for Dave to receive the channel edge from the funding manager.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = dave.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("dave didn't see the dave->carol channel before "+
			"timeout: %v", err)
	}

	// Ensure that carol's balance starts with the amount we pushed to her.
	checkCarolBalance(pushAmt)

	// Send payments from Dave to Carol using 3 of Carol's payment hashes
	// generated above.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, dave, dave.RouterClient, carolPayReqs[:numInvoices/2], false,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// At this point, we'll also send over a set of HTLC's from Carol to
	// Dave. This ensures that the final revoked transaction has HTLC's in
	// both directions.
	davePayReqs, _, _, err := createPayReqs(
		dave, paymentAmt, numInvoices,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// Send payments from Carol to Dave using 3 of Dave's payment hashes
	// generated above.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, carol, carol.RouterClient, davePayReqs[:numInvoices/2], false,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Next query for Carol's channel state, as we sent 3 payments of 10k
	// atoms each, however Carol should now see her balance as being
	// equal to the push amount in atoms since she has not settled.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	carolChan, err := getChanInfo(ctxt, carol)
	if err != nil {
		t.Fatalf("unable to get carol's channel info: %v", err)
	}

	// Grab Carol's current commitment height (update number), we'll later
	// revert her to this state after additional updates to force her to
	// broadcast this soon to be revoked state.
	carolStateNumPreCopy := carolChan.NumUpdates

	// Ensure that carol's balance still reflects the original amount we
	// pushed to her, minus the HTLCs she just sent to Dave.
	checkCarolBalance(pushAmt - 3*paymentAmt)

	// Since Carol has not settled, she should only see at least one update
	// to her channel.
	checkCarolNumUpdatesAtLeast(1)

	// Create a temporary file to house Carol's database state at this
	// particular point in history.
	carolTempDbPath, err := ioutil.TempDir("", "carol-past-state")
	if err != nil {
		t.Fatalf("unable to create temp db folder: %v", err)
	}
	carolTempDbFile := filepath.Join(carolTempDbPath, "channel.db")
	defer os.Remove(carolTempDbPath)

	// With the temporary file created, copy Carol's current state into the
	// temporary file we created above. Later after more updates, we'll
	// restore this state.
	if err := lntest.CopyFile(carolTempDbFile, carol.DBPath()); err != nil {
		t.Fatalf("unable to copy database files: %v", err)
	}

	// Finally, send payments from Dave to Carol, consuming Carol's
	// remaining payment hashes.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, dave, dave.RouterClient, carolPayReqs[numInvoices/2:], false,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Ensure that carol's balance still shows the amount we originally
	// pushed to her (minus the HTLCs she sent to Bob), and that at least
	// one more update has occurred.
	time.Sleep(500 * time.Millisecond)
	checkCarolBalance(pushAmt - 3*paymentAmt)
	checkCarolNumUpdatesAtLeast(carolStateNumPreCopy + 1)

	// Disconnect Carol and Dave, so that the channel isn't corrected once Carol
	// is restarted.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.DisconnectNodes(ctxt, dave, carol)
	if err != nil {
		t.Fatalf("unable to disconnect dave and carol: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	// Suspend Dave to prevent him from sending a justice tx redeeming the
	// funds from the breach tx before Carol has a chance to redeem the
	// second level HTLCs.
	resumeDave, err := net.SuspendNode(dave)
	if err != nil {
		t.Fatalf("unable to suspend Dave: %v", err)
	}

	// Now we shutdown Carol, copying over the her temporary database state
	// which has the *prior* channel state over her current most up to date
	// state. With this, we essentially force Carol to travel back in time
	// within the channel's history.
	if err = net.RestartNode(carol, func() error {
		return os.Rename(carolTempDbFile, carol.DBPath())
	}); err != nil {
		t.Fatalf("unable to restart node: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	// Ensure that Carol's view of the channel is consistent with the state
	// of the channel just before it was snapshotted.
	checkCarolBalance(pushAmt - 3*paymentAmt)
	checkCarolNumUpdatesAtLeast(1)

	// Now query for Carol's channel state, it should show that she's at a
	// state number in the past, *not* the latest state.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	carolChan, err = getChanInfo(ctxt, carol)
	if err != nil {
		t.Fatalf("unable to get carol chan info: %v", err)
	}
	if carolChan.NumUpdates != carolStateNumPreCopy {
		t.Fatalf("db copy failed: %v", carolChan.NumUpdates)
	}

	// Now force Carol to execute a *force* channel closure by unilaterally
	// broadcasting her current channel state. This is actually the
	// commitment transaction of a prior *revoked* state, so she'll soon
	// feel the wrath of Dave's retribution.
	force := true
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeUpdates, closeTxId, err := net.CloseChannel(ctxt, carol,
		chanPoint, force)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}

	// Query the mempool for the breaching closing transaction, this should
	// be broadcast by Carol when she force closes the channel above.
	txid, err := waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find Carol's force close tx in mempool: %v",
			err)
	}
	if *txid != *closeTxId {
		t.Fatalf("expected closeTx(%v) in mempool, instead found %v",
			closeTxId, txid)
	}

	// Generate a single block to mine the breach transaction.
	block := mineBlocks(t, net, 1, 1)[0]

	// Wait for the final close status update, then ensure that the closing
	// transaction was included in the block.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	breachTXID, err := net.WaitForChannelClose(ctxt, closeUpdates)
	if err != nil {
		t.Fatalf("error while waiting for channel close: %v", err)
	}
	if *breachTXID != *closeTxId {
		t.Fatalf("expected breach ID(%v) to be equal to close ID (%v)",
			breachTXID, closeTxId)
	}
	assertTxInBlock(t, block, breachTXID)

	// isSecondLevelSpend checks that the passed secondLevelTxid is a
	// potential second level spend spending from the commit tx.
	isSecondLevelSpend := func(commitTxid, secondLevelTxid *chainhash.Hash) (bool, *wire.MsgTx) {
		secondLevel, err := net.Miner.Node.GetRawTransaction(
			context.Background(), secondLevelTxid)
		if err != nil {
			t.Fatalf("unable to query for tx: %v", err)
		}

		// A second level spend should have only one input, and one
		// output.
		if len(secondLevel.MsgTx().TxIn) != 1 {
			return false, nil
		}
		if len(secondLevel.MsgTx().TxOut) != 1 {
			return false, nil
		}

		// The sole input should be spending from the commit tx.
		txIn := secondLevel.MsgTx().TxIn[0]
		fromCommit := bytes.Equal(txIn.PreviousOutPoint.Hash[:], commitTxid[:])
		if !fromCommit {
			return false, nil
		}

		return true, secondLevel.MsgTx()
	}

	// Grab transactions which will be required for future checks.
	breachTx, err := net.Miner.Node.GetRawTransaction(context.Background(), closeTxId)
	if err != nil {
		t.Fatalf("unable to query for breach tx: %v", err)
	}

	fundingTxId, err := chainhash.NewHash(chanPoint.GetFundingTxidBytes())
	if err != nil {
		t.Fatalf("chainpoint id bytes not a chainhash: %v", err)
	}
	fundingTx, err := net.Miner.Node.GetRawTransaction(context.Background(), fundingTxId)
	if err != nil {
		t.Fatalf("unable to query for funding tx: %v", err)
	}

	// After force-closing the channel, Carol should send 3 second-level
	// success htlc transactions to redeem her offered htlcs (for which she
	// has the preimage).
	//
	// We'll ensure she has actually sent them and record the fees to
	// correctly account for them in Dave's final balance.
	txids, err := waitForNTxsInMempool(net.Miner.Node, 3, minerMempoolTimeout)
	var numSecondLvlSuccess int
	var feesSecondLvlSuccess int64
	if err != nil {
		t.Fatalf("unable to wait for htlc success transactions: %v", err)
	}
	for _, txid := range txids {
		isSecondLevel, tx := isSecondLevelSpend(breachTXID, txid)
		if !isSecondLevel {
			continue
		}
		numSecondLvlSuccess++
		prevOut := breachTx.MsgTx().TxOut[tx.TxIn[0].PreviousOutPoint.Index]
		feesSecondLvlSuccess += prevOut.Value - tx.TxOut[0].Value
	}
	if numSecondLvlSuccess != 3 {
		t.Fatalf("Carol did not send the expected second-level htlc "+
			"success txs (found %d)", len(txids))
	}

	// Mine enough blocks for Carol to attempt to redeem the timed-out
	// htlcs via second-level timeout txs.
	numBlocks := padCLTV(uint32(cltvDelta - 1))
	mineBlocks(t, net, numBlocks, 0)

	// Wait for Carol to send the timeout second-level txs, then ensure
	// they can be found on the mempool, that they redeem from the breach
	// tx and that they are in fact second-level HTLC txs.
	mempool, err := waitForNTxsInMempool(net.Miner.Node, 3, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to get mempool from miner: %v", err)
	}
	var numSecondLvlTimeout int
	var totalSecondLvlTimeout int64
	var feesSecondLvlTimeout int64
	for _, txid := range mempool {
		isSecondLevel, tx := isSecondLevelSpend(breachTXID, txid)
		if !isSecondLevel {
			continue
		}
		prevOut := breachTx.MsgTx().TxOut[tx.TxIn[0].PreviousOutPoint.Index]
		numSecondLvlTimeout++
		totalSecondLvlTimeout += tx.TxOut[0].Value
		feesSecondLvlTimeout += prevOut.Value - tx.TxOut[0].Value
	}
	if numSecondLvlTimeout != 3 {
		t.Fatalf("unable to find the 3 htlc timeout second-level txs "+
			"from Carol (found %d)", numSecondLvlTimeout)
	}

	// The total redeemed by the second-level timeout txs should be the
	// amount for 3 time-locked invoices minus the fees.
	expectedSecondLvlTimeoutOut := 3*paymentAmt - feesSecondLvlTimeout
	if totalSecondLvlTimeout != expectedSecondLvlTimeoutOut {
		t.Fatalf("unexpected total redeemed by second-level txs; "+
			"expected=%d actual=%d fees=%d", expectedSecondLvlTimeoutOut,
			totalSecondLvlTimeout, feesSecondLvlTimeout)
	}

	// Mine a block with the timeout second-level HTLCs.
	mineBlocks(t, net, 1, 3)

	// Restart Dave. He should notice the breached commitment transaction
	// and the second-level txs. Once he does, he will send a justice tx to
	// sweep the time-locked funds from Carol's breach tx, his side of the
	// commitment tx, the time-locked 3 HTLCs in the success second-level
	// txs and the 3 time-locked HTLCs in the timeout second-level txs, for
	// a grand total of 8 inputs.
	err = resumeDave()
	if err != nil {
		t.Fatalf("unable to resume Dave: %v", err)
	}

	// Query the mempool for Dave's justice transaction.
	var predErr error
	var justiceTxid *chainhash.Hash
	exNumInputs := 2 + numInvoices
	errNotFound := fmt.Errorf("justice tx with %d inputs not found", exNumInputs)
	findJusticeTx := func() (*chainhash.Hash, error) {
		mempool, err := net.Miner.Node.GetRawMempool(context.Background(), jsonrpctypes.GRMRegular)
		if err != nil {
			return nil, fmt.Errorf("unable to get mempool from "+
				"miner: %v", err)
		}

		for _, txid := range mempool {
			// Check that the justice tx has the appropriate number
			// of inputs.
			tx, err := net.Miner.Node.GetRawTransaction(context.Background(), txid)
			if err != nil {
				return nil, fmt.Errorf("unable to query for "+
					"txs: %v", err)
			}

			if len(tx.MsgTx().TxIn) == exNumInputs {
				return txid, nil
			}
		}
		return nil, errNotFound
	}
	err = wait.Predicate(func() bool {
		txid, err := findJusticeTx()
		if err != nil {
			predErr = err
			return false
		}

		justiceTxid = txid
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf(predErr.Error())
	}

	justiceTx, err := net.Miner.Node.GetRawTransaction(context.Background(), justiceTxid)
	if err != nil {
		t.Fatalf("unable to query for justice tx: %v", err)
	}

	// Check that all the inputs of this transaction are spending outputs
	// generated by Carol's breach transaction above or by one of the
	// second-level txs.
	var totalJusticeIn int64
	var justiceTxNumSecondLvlIn int
	for _, txIn := range justiceTx.MsgTx().TxIn {
		if bytes.Equal(txIn.PreviousOutPoint.Hash[:], breachTXID[:]) {
			txo := breachTx.MsgTx().TxOut[txIn.PreviousOutPoint.Index]
			totalJusticeIn += txo.Value
			continue
		}
		if is, tx := isSecondLevelSpend(breachTXID, &txIn.PreviousOutPoint.Hash); is {
			totalJusticeIn += tx.TxOut[0].Value
			justiceTxNumSecondLvlIn++
			continue
		}
		t.Fatalf("justice tx not spending commitment or second-level "+
			"utxo; instead is: %v", txIn.PreviousOutPoint)
	}
	if justiceTxNumSecondLvlIn != 6 {
		t.Fatalf("justice tx does not have 3 inputs from second-level "+
			"txs (found %d)", justiceTxNumSecondLvlIn)
	}

	// The amount returned by the justice tx must be the total channel
	// amount minus the (breached) commitment tx fee, the second-level fees
	// and the justice tx fee itself.
	jtx := justiceTx.MsgTx()
	commitFee := int64(cType.calcStaticFee(6))
	justiceFee := totalJusticeIn - jtx.TxOut[0].Value
	expectedJusticeOut := int64(chanAmt) - commitFee - justiceFee -
		feesSecondLvlSuccess - feesSecondLvlTimeout
	if jtx.TxOut[0].Value != expectedJusticeOut {
		t.Fatalf("wrong value returned by justice tx; expected=%d "+
			"actual=%d chanAmt=%d commitFee=%d justiceFee=%d "+
			"secondLevelSuccessFees=%d secondLevelTimeoutFees=%d",
			expectedJusticeOut, jtx.TxOut[0].Value, chanAmt,
			commitFee, justiceFee, feesSecondLvlSuccess,
			feesSecondLvlTimeout)
	}

	// Now mine a block. This should include Dave's justice transaction
	// which was just accepted into the mempool.
	block = mineBlocks(t, net, 1, 1)[0]
	assertTxInBlock(t, block, justiceTxid)

	// Dave and Carol should have no open channels.
	assertNodeNumChannels(t, dave, 0)
	assertNodeNumChannels(t, carol, 0)

	// We'll now check that the total balance of each party is the one we
	// expect.  Introduce a new test closure.
	checkTotalBalance := func(node *lntest.HarnessNode, expectedAmt int64) {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		balances, err := node.WalletBalance(ctxt, &lnrpc.WalletBalanceRequest{})
		if err != nil {
			t.Fatalf("unable to query node balance: %v", err)
		}
		if balances.TotalBalance != expectedAmt {
			t.Fatalf("balance is incorrect, "+
				"got %v, expected %v", balances.TotalBalance,
				expectedAmt)
		}
	}

	// Carol should not have any balance, because even though she was sent
	// coins via an HTLC, she chose to broadcast an older commitment tx and
	// forfeit her coins.
	checkTotalBalance(carol, 0)

	// Dave should have the initial amount sent to him for funding channels
	// minus the fees required to broadcast the breached commitment with 6
	// HTLCs and the justice tx.
	fundingFee := recordedTxFee(fundingTx.MsgTx())
	expectedDaveBalance := initialBalance - fundingFee - commitFee -
		feesSecondLvlSuccess - feesSecondLvlTimeout - justiceFee
	checkTotalBalance(dave, expectedDaveBalance)

}

// testRevokedCloseRetributionAltruistWatchtower establishes a channel between
// Carol and Dave, where Carol is using a third node Willy as her watchtower.
// After sending some payments, Dave reverts his state and force closes to
// trigger a breach. Carol is kept offline throughout the process and the test
// asserts that Willy responds by broadcasting the justice transaction on
// Carol's behalf sweeping her funds without a reward.
func testRevokedCloseRetributionAltruistWatchtower(net *lntest.NetworkHarness,
	t *harnessTest) {

	ctxb := context.Background()
	const (
		chanAmt     = defaultChanAmt
		paymentAmt  = 10000
		numInvoices = 6
		externalIP  = "1.2.3.4"
	)

	// Since we'd like to test some multi-hop failure scenarios, we'll
	// introduce another node into our test network: Carol.
	carol, err := net.NewNode("Carol", []string{
		"--hodl.exit-settle",
	})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	// Willy the watchtower will protect Dave from Carol's breach. He will
	// remain online in order to punish Carol on Dave's behalf, since the
	// breach will happen while Dave is offline.
	willy, err := net.NewNode("Willy", []string{
		"--watchtower.active",
		"--watchtower.externalip=" + externalIP,
	})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, willy)

	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	willyInfo, err := willy.Watchtower.GetInfo(
		ctxt, &watchtowerrpc.GetInfoRequest{},
	)
	if err != nil {
		t.Fatalf("unable to getinfo from willy: %v", err)
	}

	// Assert that Willy has one listener and it is 0.0.0.0:9911 or
	// [::]:9911. Since no listener is explicitly specified, one of these
	// should be the default depending on whether the host supports IPv6 or
	// not.
	if len(willyInfo.Listeners) != 1 {
		t.Fatalf("Willy should have 1 listener, has %d",
			len(willyInfo.Listeners))
	}
	listener := willyInfo.Listeners[0]
	if listener != "0.0.0.0:9911" && listener != "[::]:9911" {
		t.Fatalf("expected listener on 0.0.0.0:9911 or [::]:9911, "+
			"got %v", listener)
	}

	// Assert the Willy's URIs properly display the chosen external IP.
	if len(willyInfo.Uris) != 1 {
		t.Fatalf("Willy should have 1 uri, has %d",
			len(willyInfo.Uris))
	}
	if !strings.Contains(willyInfo.Uris[0], externalIP) {
		t.Fatalf("expected uri with %v, got %v",
			externalIP, willyInfo.Uris[0])
	}

	// Dave will be the breached party. We set --nolisten to ensure Carol
	// won't be able to connect to him and trigger the channel data
	// protection logic automatically.
	dave, err := net.NewNode("Dave", []string{
		"--nolisten",
		"--wtclient.active",
	})
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	addTowerReq := &wtclientrpc.AddTowerRequest{
		Pubkey:  willyInfo.Pubkey,
		Address: listener,
	}
	if _, err = dave.WatchtowerClient.AddTower(ctxt, addTowerReq); err != nil {
		t.Fatalf("unable to add willy's watchtower: %v", err)
	}

	// We must let Dave have an open channel before she can send a node
	// announcement, so we open a channel with Carol,
	if err := net.ConnectNodes(ctxb, dave, carol); err != nil {
		t.Fatalf("unable to connect dave to carol: %v", err)
	}

	// Before we make a channel, we'll load up Dave with some coins sent
	// directly from the miner.
	err = net.SendCoins(ctxb, dcrutil.AtomsPerCoin, dave)
	if err != nil {
		t.Fatalf("unable to send coins to dave: %v", err)
	}

	// In order to test Dave's response to an uncooperative channel
	// closure by Carol, we'll first open up a channel between them with a
	// 0.5 DCR value.
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, dave, carol,
		lntest.OpenChannelParams{
			Amt:     3 * (chanAmt / 4),
			PushAmt: chanAmt / 4,
		},
	)

	// With the channel open, we'll create a few invoices for Carol that
	// Dave will pay to in order to advance the state of the channel.
	carolPayReqs, _, _, err := createPayReqs(
		carol, paymentAmt, numInvoices,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// Wait for Dave to receive the channel edge from the funding manager.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = dave.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("dave didn't see the dave->carol channel before "+
			"timeout: %v", err)
	}

	// Next query for Carol's channel state, as we sent 0 payments, Carol
	// should still see her balance as the push amount, which is 1/4 of the
	// capacity.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	carolChan, err := getChanInfo(ctxt, carol)
	if err != nil {
		t.Fatalf("unable to get carol's channel info: %v", err)
	}
	if carolChan.LocalBalance != int64(chanAmt/4) {
		t.Fatalf("carol's balance is incorrect, got %v, expected %v",
			carolChan.LocalBalance, chanAmt/4)
	}

	// Grab Carol's current commitment height (update number), we'll later
	// revert her to this state after additional updates to force him to
	// broadcast this soon to be revoked state.
	carolStateNumPreCopy := carolChan.NumUpdates

	// Create a temporary file to house Carol's database state at this
	// particular point in history.
	carolTempDbPath, err := ioutil.TempDir("", "carol-past-state")
	if err != nil {
		t.Fatalf("unable to create temp db folder: %v", err)
	}
	carolTempDbFile := filepath.Join(carolTempDbPath, "channel.db")
	defer os.Remove(carolTempDbPath)

	// With the temporary file created, copy Carol's current state into the
	// temporary file we created above. Later after more updates, we'll
	// restore this state.
	if err := lntest.CopyFile(carolTempDbFile, carol.DBPath()); err != nil {
		t.Fatalf("unable to copy database files: %v", err)
	}

	// Finally, send payments from Dave to Carol, consuming Carol's remaining
	// payment hashes.
	err = completePaymentRequests(
		ctxb, dave, dave.RouterClient, carolPayReqs, false,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	daveBalReq := &lnrpc.WalletBalanceRequest{}
	daveBalResp, err := dave.WalletBalance(ctxt, daveBalReq)
	if err != nil {
		t.Fatalf("unable to get dave's balance: %v", err)
	}

	davePreSweepBalance := daveBalResp.ConfirmedBalance

	// Wait until the backup has been accepted by the watchtower before
	// shutting down Dave.
	err = wait.NoError(func() error {
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		bkpStats, err := dave.WatchtowerClient.Stats(ctxt, &wtclientrpc.StatsRequest{})
		if err != nil {
			return err
		}
		if bkpStats == nil {
			return errors.New("no active backup sessions")
		}
		if bkpStats.NumBackups == 0 {
			return errors.New("no backups accepted")
		}
		return nil
	}, defaultTimeout)
	if err != nil {
		t.Fatalf("unable to verify backup task completed: %v", err)
	}

	// Shutdown Dave to simulate going offline for an extended period of
	// time. Once he's not watching, Carol will try to breach the channel.
	restart, err := net.SuspendNode(dave)
	if err != nil {
		t.Fatalf("unable to suspend Dave: %v", err)
	}

	// Now we shutdown Carol, copying over the his temporary database state
	// which has the *prior* channel state over his current most up to date
	// state. With this, we essentially force Carol to travel back in time
	// within the channel's history.
	if err = net.RestartNode(carol, func() error {
		return os.Rename(carolTempDbFile, carol.DBPath())
	}); err != nil {
		t.Fatalf("unable to restart node: %v", err)
	}

	// Now query for Carol's channel state, it should show that he's at a
	// state number in the past, not the *latest* state.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	carolChan, err = getChanInfo(ctxt, carol)
	if err != nil {
		t.Fatalf("unable to get carol chan info: %v", err)
	}
	if carolChan.NumUpdates != carolStateNumPreCopy {
		t.Fatalf("db copy failed: %v", carolChan.NumUpdates)
	}

	// Now force Carol to execute a *force* channel closure by unilaterally
	// broadcasting his current channel state. This is actually the
	// commitment transaction of a prior *revoked* state, so he'll soon
	// feel the wrath of Dave's retribution.
	closeUpdates, closeTxId, err := net.CloseChannel(
		ctxb, carol, chanPoint, true,
	)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}

	// Query the mempool for the breaching closing transaction, this should
	// be broadcast by Carol when she force closes the channel above.
	txid, err := waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find Carol's force close tx in mempool: %v",
			err)
	}
	if *txid != *closeTxId {
		t.Fatalf("expected closeTx(%v) in mempool, instead found %v",
			closeTxId, txid)
	}

	// Finally, generate a single block, wait for the final close status
	// update, then ensure that the closing transaction was included in the
	// block.
	block := mineBlocks(t, net, 1, 1)[0]

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	breachTXID, err := net.WaitForChannelClose(ctxt, closeUpdates)
	if err != nil {
		t.Fatalf("error while waiting for channel close: %v", err)
	}
	assertTxInBlock(t, block, breachTXID)

	// Query the mempool for Dave's justice transaction, this should be
	// broadcast as Carol's contract breaching transaction gets confirmed
	// above.
	justiceTXID, err := waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find Dave's justice tx in mempool: %v",
			err)
	}
	time.Sleep(100 * time.Millisecond)

	// Query for the mempool transaction found above. Then assert that all
	// the inputs of this transaction are spending outputs generated by
	// Carol's breach transaction above.
	justiceTx, err := net.Miner.Node.GetRawTransaction(context.Background(), justiceTXID)
	if err != nil {
		t.Fatalf("unable to query for justice tx: %v", err)
	}
	for _, txIn := range justiceTx.MsgTx().TxIn {
		if !bytes.Equal(txIn.PreviousOutPoint.Hash[:], breachTXID[:]) {
			t.Fatalf("justice tx not spending commitment utxo "+
				"instead is: %v", txIn.PreviousOutPoint)
		}
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	willyBalReq := &lnrpc.WalletBalanceRequest{}
	willyBalResp, err := willy.WalletBalance(ctxt, willyBalReq)
	if err != nil {
		t.Fatalf("unable to get willy's balance: %v", err)
	}

	if willyBalResp.ConfirmedBalance != 0 {
		t.Fatalf("willy should have 0 balance before mining "+
			"justice transaction, instead has %d",
			willyBalResp.ConfirmedBalance)
	}

	// Now mine a block, this transaction should include Dave's justice
	// transaction which was just accepted into the mempool.
	block = mineBlocks(t, net, 1, 1)[0]

	// The block should have exactly *two* transactions, one of which is
	// the justice transaction.
	if len(block.Transactions) != 2 {
		t.Fatalf("transaction wasn't mined")
	}
	justiceSha := block.Transactions[1].TxHash()
	if !bytes.Equal(justiceTx.Hash()[:], justiceSha[:]) {
		t.Fatalf("justice tx wasn't mined")
	}

	// Ensure that Willy doesn't get any funds, as he is acting as an
	// altruist watchtower.
	var predErr error
	err = wait.Invariant(func() bool {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		willyBalReq := &lnrpc.WalletBalanceRequest{}
		willyBalResp, err := willy.WalletBalance(ctxt, willyBalReq)
		if err != nil {
			t.Fatalf("unable to get willy's balance: %v", err)
		}

		if willyBalResp.ConfirmedBalance != 0 {
			predErr = fmt.Errorf("Expected Willy to have no funds "+
				"after justice transaction was mined, found %v",
				willyBalResp)
			return false
		}

		return true
	}, time.Second*5)
	if err != nil {
		t.Fatalf("%v", predErr)
	}

	// Restart Dave, who will still think his channel with Carol is open.
	// We should him to detect the breach, but realize that the funds have
	// then been swept to his wallet by Willy.
	err = restart()
	if err != nil {
		t.Fatalf("unable to restart dave: %v", err)
	}

	err = wait.Predicate(func() bool {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		daveBalReq := &lnrpc.ChannelBalanceRequest{}
		daveBalResp, err := dave.ChannelBalance(ctxt, daveBalReq)
		if err != nil {
			t.Fatalf("unable to get dave's balance: %v", err)
		}

		if daveBalResp.Balance != 0 {
			predErr = fmt.Errorf("Dave should end up with zero "+
				"channel balance, instead has %d",
				daveBalResp.Balance)
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("%v", predErr)
	}

	assertNumPendingChannels(t, dave, 0, 0, 0, 0)

	err = wait.Predicate(func() bool {
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		daveBalReq := &lnrpc.WalletBalanceRequest{}
		daveBalResp, err := dave.WalletBalance(ctxt, daveBalReq)
		if err != nil {
			t.Fatalf("unable to get dave's balance: %v", err)
		}

		if daveBalResp.ConfirmedBalance <= davePreSweepBalance {
			predErr = fmt.Errorf("Dave should have more than %d "+
				"after sweep, instead has %d",
				davePreSweepBalance,
				daveBalResp.ConfirmedBalance)
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("%v", predErr)
	}

	// Dave should have no open channels.
	assertNodeNumChannels(t, dave, 0)
}

// assertNumPendingChannels checks that a PendingChannels response from the
// node reports the expected number of pending channels.
func assertNumPendingChannels(t *harnessTest, node *lntest.HarnessNode,
	expWaitingClose, expPendingForceClose, expPendingClosing,
	expPendingOpen int) {

	ctxb := context.Background()

	var predErr error
	err := wait.Predicate(func() bool {
		pendingChansRequest := &lnrpc.PendingChannelsRequest{}
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		pendingChanResp, err := node.PendingChannels(ctxt,
			pendingChansRequest)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}
		n := len(pendingChanResp.WaitingCloseChannels)
		if n != expWaitingClose {
			predErr = fmt.Errorf("Expected to find %d channels "+
				"waiting close, found %d", expWaitingClose, n)
			return false
		}
		n = len(pendingChanResp.PendingForceClosingChannels)
		if n != expPendingForceClose {
			predErr = fmt.Errorf("expected to find %d channel "+
				"pending force close, found %d", expPendingForceClose, n)
			return false
		}

		n = len(pendingChanResp.PendingClosingChannels)
		if n != expPendingClosing {
			predErr = fmt.Errorf("expected to find %d channels "+
				"pending closing, found %d", expPendingClosing,
				n)
		}

		n = len(pendingChanResp.PendingOpenChannels)
		if n != expPendingOpen {
			predErr = fmt.Errorf("expected to find %d channels "+
				"pending open, found %d", expPendingOpen, n)
		}
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("%v", predErr)
	}
}

// assertDLPExecuted asserts that Dave is a node that has recovered their state
// form scratch. Carol should then force close on chain, with Dave sweeping his
// funds immediately, and Carol sweeping her fund after her CSV delay is up. If
// the blankSlate value is true, then this means that Dave won't need to sweep
// on chain as he has no funds in the channel.
func assertDLPExecuted(net *lntest.NetworkHarness, t *harnessTest,
	carol *lntest.HarnessNode, carolStartingBalance int64,
	dave *lntest.HarnessNode, daveStartingBalance int64,
	anchors bool) {

	// We disabled auto-reconnect for some tests to avoid timing issues.
	// To make sure the nodes are initiating DLP now, we have to manually
	// re-connect them.
	ctxb := context.Background()
	err := net.ConnectNodes(ctxb, carol, dave)
	if err != nil && !strings.Contains(err.Error(), "already connected") {
		t.Fatalf("unable to connect Carol to Dave to initiate DLP: %v",
			err)
	}

	// Upon reconnection, the nodes should detect that Dave is out of sync.
	// Carol should force close the channel using her latest commitment.
	expectedTxes := 1
	if anchors {
		// The two expected transactions for Carol are the force close
		// transaction and the anchor sweep transaction.
		expectedTxes = 2
	}
	_, err = waitForNTxsInMempool(
		net.Miner.Node, expectedTxes, minerMempoolTimeout,
	)
	if err != nil {
		t.Fatalf("unable to find Carol's force close tx in mempool: %v",
			err)
	}

	// Channel should be in the state "waiting close" for Carol since she
	// broadcasted the force close tx.
	assertNumPendingChannels(t, carol, 1, 0, 0, 0)

	// Dave should also consider the channel "waiting close", as he noticed
	// the channel was out of sync, and is now waiting for a force close to
	// hit the chain.
	assertNumPendingChannels(t, dave, 1, 0, 0, 0)

	// Restart Dave to make sure he is able to sweep the funds after
	// shutdown.
	if err := net.RestartNode(dave, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Generate a single block, which should confirm the closing tx.
	block := mineBlocks(t, net, 1, expectedTxes)[0]

	// Dave should sweep his funds immediately, as they are not timelocked.
	// We also expect Dave to sweep his anchor, if present.

	if anchors {
		// Note(decred): in lnd due to a large difference in fees, two
		// transactions are expected at this point: one sweeping the
		// commitment output and one the anchor output. In decred,
		// since fees are all using the default relay fee only a single
		// transaction sweeping both outputs is expected.
		expectedTxes = 1
	}

	_, err = waitForNTxsInMempool(
		net.Miner.Node, expectedTxes, minerMempoolTimeout,
	)
	if err != nil {
		t.Fatalf("unable to find Dave's sweep tx in mempool: %v", err)
	}

	// Dave should consider the channel pending force close (since he is
	// waiting for his sweep to confirm).
	assertNumPendingChannels(t, dave, 0, 1, 0, 0)

	// Carol is considering it "pending force close", as we must wait
	// before she can sweep her outputs.
	assertNumPendingChannels(t, carol, 0, 1, 0, 0)

	// Mine the sweep tx.
	block = mineBlocks(t, net, 1, expectedTxes)[0]

	// Now Dave should consider the channel fully closed.
	assertNumPendingChannels(t, dave, 0, 0, 0, 0)

	// We query Dave's balance to make sure it increased after the channel
	// closed. This checks that he was able to sweep the funds he had in
	// the channel.
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	balReq := &lnrpc.WalletBalanceRequest{}
	daveBalResp, err := dave.WalletBalance(ctxt, balReq)
	if err != nil {
		t.Fatalf("unable to get dave's balance: %v", err)
	}

	daveBalance := daveBalResp.ConfirmedBalance
	if daveBalance <= daveStartingBalance {
		t.Fatalf("expected dave to have balance above %d, "+
			"instead had %v", daveStartingBalance, daveBalance)
	}

	// After the Carol's output matures, she should also reclaim her funds.
	//
	// The commit sweep resolver publishes the sweep tx at defaultCSV-1 and
	// we already mined one block after the commitmment was published, so
	// take that into account.
	mineBlocks(t, net, defaultCSV-2, 0)
	carolSweep, err := waitForTxInMempool(
		net.Miner.Node, minerMempoolTimeout,
	)
	if err != nil {
		t.Fatalf("unable to find Carol's sweep tx in mempool: %v", err)
	}
	block = mineBlocks(t, net, 1, 1)[0]
	assertTxInBlock(t, block, carolSweep)

	// Now the channel should be fully closed also from Carol's POV.
	assertNumPendingChannels(t, carol, 0, 0, 0, 0)

	// Make sure Carol got her balance back.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	carolBalResp, err := carol.WalletBalance(ctxt, balReq)
	if err != nil {
		t.Fatalf("unable to get carol's balance: %v", err)
	}
	carolBalance := carolBalResp.ConfirmedBalance
	if carolBalance <= carolStartingBalance {
		t.Fatalf("expected carol to have balance above %d, "+
			"instead had %v", carolStartingBalance,
			carolBalance)
	}

	assertNodeNumChannels(t, dave, 0)
	assertNodeNumChannels(t, carol, 0)
}

// testDataLossProtection tests that if one of the nodes in a channel
// relationship lost state, they will detect this during channel sync, and the
// up-to-date party will force close the channel, giving the outdated party the
// opportunity to sweep its output.
func testDataLossProtection(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()
	const (
		chanAmt     = defaultChanAmt
		paymentAmt  = 10000
		numInvoices = 6
	)

	// Carol will be the up-to-date party. We set --nolisten to ensure Dave
	// won't be able to connect to her and trigger the channel data
	// protection logic automatically. We also can't have Carol
	// automatically re-connect too early, otherwise DLP would be initiated
	// at the wrong moment.
	carol, err := net.NewNode(
		"Carol", []string{"--nolisten", "--minbackoff=1h"},
	)
	if err != nil {
		t.Fatalf("unable to create new carol node: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	// Dave will be the party losing his state.
	dave, err := net.NewNode("Dave", nil)
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	// Before we make a channel, we'll load up Carol with some coins sent
	// directly from the miner.
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}

	// timeTravel is a method that will make Carol open a channel to the
	// passed node, settle a series of payments, then reset the node back
	// to the state before the payments happened. When this method returns
	// the node will be unaware of the new state updates. The returned
	// function can be used to restart the node in this state.
	timeTravel := func(node *lntest.HarnessNode) (func() error,
		*lnrpc.ChannelPoint, int64, error) {

		// We must let the node communicate with Carol before they are
		// able to open channel, so we connect them.
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		if err := net.EnsureConnected(ctxt, carol, node); err != nil {
			t.Fatalf("unable to connect %v to carol: %v",
				node.Name(), err)
		}

		// We'll first open up a channel between them with a 0.5 DCR
		// value.
		ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
		chanPoint := openChannelAndAssert(
			ctxt, t, net, carol, node,
			lntest.OpenChannelParams{
				Amt: chanAmt,
			},
		)

		// With the channel open, we'll create a few invoices for the
		// node that Carol will pay to in order to advance the state of
		// the channel.  TODO(halseth): have dangling HTLCs on the
		// commitment, able to  retrive funds?
		payReqs, _, _, err := createPayReqs(
			node, paymentAmt, numInvoices,
		)
		if err != nil {
			t.Fatalf("unable to create pay reqs: %v", err)
		}

		// Wait for Carol to receive the channel edge from the funding
		// manager.
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		err = carol.WaitForNetworkChannelOpen(ctxt, chanPoint)
		if err != nil {
			t.Fatalf("carol didn't see the carol->%s channel "+
				"before timeout: %v", node.Name(), err)
		}

		// Send payments from Carol using 3 of the payment hashes
		// generated above.
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		err = completePaymentRequests(
			ctxt, carol, carol.RouterClient,
			payReqs[:numInvoices/2], true,
		)
		if err != nil {
			t.Fatalf("unable to send payments: %v", err)
		}

		// Next query for the node's channel state, as we sent 3
		// payments of 10k atoms each, it should now see his balance
		// as being 30k atoms.
		var nodeChan *lnrpc.Channel
		var predErr error
		err = wait.Predicate(func() bool {
			ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
			bChan, err := getChanInfo(ctxt, node)
			if err != nil {
				t.Fatalf("unable to get channel info: %v", err)
			}
			if bChan.LocalBalance != 30000 {
				predErr = fmt.Errorf("balance is incorrect, "+
					"got %v, expected %v",
					bChan.LocalBalance, 30000)
				return false
			}

			nodeChan = bChan
			return true
		}, time.Second*15)
		if err != nil {
			t.Fatalf("%v", predErr)
		}

		// Grab the current commitment height (update number), we'll
		// later revert him to this state after additional updates to
		// revoke this state.
		stateNumPreCopy := nodeChan.NumUpdates

		// Create a temporary file to house the database state at this
		// particular point in history.
		tempDbPath, err := ioutil.TempDir("", node.Name()+"-past-state")
		if err != nil {
			t.Fatalf("unable to create temp db folder: %v", err)
		}
		tempDbFile := filepath.Join(tempDbPath, "channel.db")
		defer os.Remove(tempDbPath)

		// With the temporary file created, copy the current state into
		// the temporary file we created above. Later after more
		// updates, we'll restore this state.
		if err := lntest.CopyFile(tempDbFile, node.DBPath()); err != nil {
			t.Fatalf("unable to copy database files: %v", err)
		}

		// Finally, send more payments from , using the remaining
		// payment hashes.
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		err = completePaymentRequests(
			ctxt, carol, carol.RouterClient,
			payReqs[numInvoices/2:], true,
		)
		if err != nil {
			t.Fatalf("unable to send payments: %v", err)
		}

		// Ensure all HTLCs have been finalized so that the most recent
		// commitment doesn't have any.
		err = wait.NoError(func() error {
			return assertNumActiveHtlcs([]*lntest.HarnessNode{node, carol}, 0)
		}, defaultTimeout)
		if err != nil {
			t.Fatalf("nodes still show active htcls: %v", err)
		}

		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		nodeChan, err = getChanInfo(ctxt, node)
		if err != nil {
			t.Fatalf("unable to get dave chan info: %v", err)
		}

		// Disconnect the nodes, to prevent Carol attempting to reconnect
		// and restore the node state.
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		err = net.DisconnectNodes(ctxt, carol, node)
		if err != nil {
			t.Fatalf("unable to disconnect carol and dave: %v", err)
		}

		// Now we shutdown the node, copying over the its temporary
		// database state which has the *prior* channel state over his
		// current most up to date state. With this, we essentially
		// force the node to travel back in time within the channel's
		// history.
		if err = net.RestartNode(node, func() error {
			return os.Rename(tempDbFile, node.DBPath())
		}); err != nil {
			t.Fatalf("unable to restart node: %v", err)
		}

		// Make sure the channel is still there from the PoV of the
		// node.
		assertNodeNumChannels(t, node, 1)

		// Now query for the channel state, it should show that it's at
		// a state number in the past, not the *latest* state.
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		nodeChan, err = getChanInfo(ctxt, node)
		if err != nil {
			t.Fatalf("unable to get dave chan info: %v", err)
		}
		if nodeChan.NumUpdates != stateNumPreCopy {
			t.Fatalf("db copy failed: %v", nodeChan.NumUpdates)
		}

		balReq := &lnrpc.WalletBalanceRequest{}
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		balResp, err := node.WalletBalance(ctxt, balReq)
		if err != nil {
			t.Fatalf("unable to get dave's balance: %v", err)
		}

		restartNode, err := net.SuspendNode(node)
		if err != nil {
			t.Fatalf("unable to suspend node: %v", err)
		}

		restart := func() error {
			err := restartNode()
			if err != nil {
				return err
			}

			// Reconnect carol to the node.
			err = net.ConnectNodes(ctxt, carol, node)
			if err != nil {
				return err
			}

			return nil
		}

		return restart, chanPoint, balResp.ConfirmedBalance, nil
	}

	// Reset Dave to a state where he has an outdated channel state.
	restartDave, _, daveStartingBalance, err := timeTravel(dave)
	if err != nil {
		t.Fatalf("unable to time travel dave: %v", err)
	}

	// We make a note of the nodes' current on-chain balances, to make sure
	// they are able to retrieve the channel funds eventually,
	balReq := &lnrpc.WalletBalanceRequest{}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	carolBalResp, err := carol.WalletBalance(ctxt, balReq)
	if err != nil {
		t.Fatalf("unable to get carol's balance: %v", err)
	}
	carolStartingBalance := carolBalResp.ConfirmedBalance

	// Restart Dave to trigger a channel resync.
	if err := restartDave(); err != nil {
		t.Fatalf("unable to restart dave: %v", err)
	}

	// Assert that once Dave comes up, they reconnect, Carol force closes
	// on chain, and both of them properly carry out the DLP protocol.
	assertDLPExecuted(
		net, t, carol, carolStartingBalance, dave, daveStartingBalance,
		false,
	)

	// As a second part of this test, we will test the scenario where a
	// channel is closed while Dave is offline, loses his state and comes
	// back online. In this case the node should attempt to resync the
	// channel, and the peer should resend a channel sync message for the
	// closed channel, such that Dave can retrieve his funds.
	//
	// We start by letting Dave time travel back to an outdated state.
	restartDave, chanPoint2, daveStartingBalance, err := timeTravel(dave)
	if err != nil {
		t.Fatalf("unable to time travel eve: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	carolBalResp, err = carol.WalletBalance(ctxt, balReq)
	if err != nil {
		t.Fatalf("unable to get carol's balance: %v", err)
	}
	carolStartingBalance = carolBalResp.ConfirmedBalance

	// Now let Carol force close the channel while Dave is offline.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPoint2, true)

	// Wait for the channel to be marked pending force close.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = waitForChannelPendingForceClose(ctxt, carol, chanPoint2)
	if err != nil {
		t.Fatalf("channel not pending force close: %v", err)
	}

	// Mine enough blocks for Carol to sweep her funds.
	mineBlocks(t, net, defaultCSV-1, 0)

	carolSweep, err := waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find Carol's sweep tx in mempool: %v", err)
	}
	block := mineBlocks(t, net, 1, 1)[0]
	assertTxInBlock(t, block, carolSweep)

	// Now the channel should be fully closed also from Carol's POV.
	assertNumPendingChannels(t, carol, 0, 0, 0, 0)

	// Make sure Carol got her balance back.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	carolBalResp, err = carol.WalletBalance(ctxt, balReq)
	if err != nil {
		t.Fatalf("unable to get carol's balance: %v", err)
	}
	carolBalance := carolBalResp.ConfirmedBalance
	if carolBalance <= carolStartingBalance {
		t.Fatalf("expected carol to have balance above %d, "+
			"instead had %v", carolStartingBalance,
			carolBalance)
	}

	assertNodeNumChannels(t, carol, 0)

	// When Dave comes online, he will reconnect to Carol, try to resync
	// the channel, but it will already be closed. Carol should resend the
	// information Dave needs to sweep his funds.
	if err := restartDave(); err != nil {
		t.Fatalf("unable to restart Eve: %v", err)
	}

	// Dave should sweep his funds.
	_, err = waitForTxInMempool(net.Miner.Node, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("unable to find Dave's sweep tx in mempool: %v", err)
	}

	// Mine a block to confirm the sweep, and make sure Dave got his
	// balance back.
	mineBlocks(t, net, 1, 1)
	assertNodeNumChannels(t, dave, 0)

	err = wait.NoError(func() error {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		daveBalResp, err := dave.WalletBalance(ctxt, balReq)
		if err != nil {
			return fmt.Errorf("unable to get dave's balance: %v",
				err)
		}

		daveBalance := daveBalResp.ConfirmedBalance
		if daveBalance <= daveStartingBalance {
			return fmt.Errorf("expected dave to have balance "+
				"above %d, intead had %v", daveStartingBalance,
				daveBalance)
		}

		return nil
	}, time.Second*15)
	if err != nil {
		t.Fatalf("%v", err)
	}
}

// assertNodeNumChannels polls the provided node's list channels rpc until it
// reaches the desired number of total channels.
func assertNodeNumChannels(t *harnessTest, node *lntest.HarnessNode,
	numChannels int) {
	ctxb := context.Background()

	// Poll node for its list of channels.
	req := &lnrpc.ListChannelsRequest{}

	var predErr error
	pred := func() bool {
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		chanInfo, err := node.ListChannels(ctxt, req)
		if err != nil {
			predErr = fmt.Errorf("unable to query for node's "+
				"channels: %v", err)
			return false
		}

		// Return true if the query returned the expected number of
		// channels.
		num := len(chanInfo.Channels)
		if num != numChannels {
			predErr = fmt.Errorf("expected %v channels, got %v",
				numChannels, num)
			return false
		}
		return true
	}

	if err := wait.Predicate(pred, time.Second*15); err != nil {
		t.Fatalf("node has incorrect number of channels: %v", predErr)
	}
}

// testRejectHTLC tests that a node can be created with the flag --rejecthtlc.
// This means that the node will reject all forwarded HTLCs but can still
// accept direct HTLCs as well as send HTLCs.
func testRejectHTLC(net *lntest.NetworkHarness, t *harnessTest) {
	//             RejectHTLC
	// Alice ------> Carol ------> Bob
	//
	const chanAmt = dcrutil.Amount(1000000)
	ctxb := context.Background()
	timeout := time.Second * 5

	// Create Carol with reject htlc flag.
	carol, err := net.NewNode("Carol", []string{"--rejecthtlc"})
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	// Connect Alice to Carol.
	if err := net.ConnectNodes(ctxb, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect alice to carol: %v", err)
	}

	// Connect Carol to Bob.
	if err := net.ConnectNodes(ctxb, carol, net.Bob); err != nil {
		t.Fatalf("unable to conenct carol to net.Bob: %v", err)
	}

	// Send coins to Carol.
	err = net.SendCoins(ctxb, dcrutil.AtomsPerCoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}

	// Send coins to Alice.
	err = net.SendCoins(ctxb, dcrutil.AtomsPerCoin, net.Alice)
	if err != nil {
		t.Fatalf("unable to send coins to alice: %v", err)
	}

	// Open a channel between Alice and Carol.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointAlice := openChannelAndAssert(
		ctxt, t, net, net.Alice, carol,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Open a channel between Carol and Bob.
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointCarol := openChannelAndAssert(
		ctxt, t, net, carol, net.Bob,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Channel should be ready for payments.
	const payAmt = 100

	// Helper closure to generate a random pre image.
	genPreImage := func() []byte {
		preimage := make([]byte, 32)

		_, err = rand.Read(preimage)
		if err != nil {
			t.Fatalf("unable to generate preimage: %v", err)
		}

		return preimage
	}

	// Create an invoice from Carol of 100 satoshis.
	// We expect Alice to be able to pay this invoice.
	preimage := genPreImage()

	carolInvoice := &lnrpc.Invoice{
		Memo:      "testing - alice should pay carol",
		RPreimage: preimage,
		Value:     payAmt,
	}

	// Carol adds the invoice to her database.
	resp, err := carol.AddInvoice(ctxb, carolInvoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Alice pays Carols invoice.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = completePaymentRequests(
		ctxt, net.Alice, net.Alice.RouterClient,
		[]string{resp.PaymentRequest}, true,
	)
	if err != nil {
		t.Fatalf("unable to send payments from alice to carol: %v", err)
	}

	// Create an invoice from Bob of 100 satoshis.
	// We expect Carol to be able to pay this invoice.
	preimage = genPreImage()

	bobInvoice := &lnrpc.Invoice{
		Memo:      "testing - carol should pay bob",
		RPreimage: preimage,
		Value:     payAmt,
	}

	// Bob adds the invoice to his database.
	resp, err = net.Bob.AddInvoice(ctxb, bobInvoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Carol pays Bobs invoice.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = completePaymentRequests(
		ctxt, carol, carol.RouterClient,
		[]string{resp.PaymentRequest}, true,
	)
	if err != nil {
		t.Fatalf("unable to send payments from carol to bob: %v", err)
	}

	// Create an invoice from Bob of 100 satoshis.
	// Alice attempts to pay Bob but this should fail, since we are
	// using Carol as a hop and her node will reject onward HTLCs.
	preimage = genPreImage()

	bobInvoice = &lnrpc.Invoice{
		Memo:      "testing - alice tries to pay bob",
		RPreimage: preimage,
		Value:     payAmt,
	}

	// Bob adds the invoice to his database.
	resp, err = net.Bob.AddInvoice(ctxb, bobInvoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Alice attempts to pay Bobs invoice. This payment should be rejected since
	// we are using Carol as an intermediary hop, Carol is running lnd with
	// --rejecthtlc.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = completePaymentRequests(
		ctxt, net.Alice, net.Alice.RouterClient,
		[]string{resp.PaymentRequest}, true,
	)
	if err == nil {
		t.Fatalf(
			"should have been rejected, carol will not accept forwarded htlcs",
		)
	}

	assertLastHTLCError(t, net.Alice, lnrpc.Failure_CHANNEL_DISABLED)

	// Close all channels.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarol, false)
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
func subscribeGraphNotifications(t *harnessTest, ctxb context.Context,
	node *lntest.HarnessNode) graphSubscription {

	// We'll first start by establishing a notification client which will
	// send us notifications upon detected changes in the channel graph.
	req := &lnrpc.GraphTopologySubscription{}
	ctx, cancelFunc := context.WithCancel(ctxb)
	topologyClient, err := node.SubscribeChannelGraph(ctx, req)
	if err != nil {
		t.Fatalf("unable to create topology client: %v", err)
	}

	// Wait for the client to get registered in the ChannelRouter. Since
	// there isn't a specific signal that we can wait for, just sleep for a
	// bit to give the ChannelRouter a chance to consume the relevant
	// channel sends. This reduces test flakiness when trying to perform a
	// graph change (such as closing a channel) immediately after
	// registering for notifications.
	time.Sleep(100 * time.Millisecond)

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

func testGraphTopologyNotifications(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const chanAmt = defaultChanAmt

	// Let Alice subscribe to graph notifications.
	graphSub := subscribeGraphNotifications(
		t, ctxb, net.Alice,
	)
	defer close(graphSub.quit)

	// Open a new channel between Alice and Bob.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
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
			// Process all channel updates prsented in this update
			// message.
			for _, chanUpdate := range graphUpdate.ChannelUpdates {
				switch chanUpdate.AdvertisingNode {
				case net.Alice.PubKeyStr:
				case net.Bob.PubKeyStr:
				default:
					t.Fatalf("unknown advertising node: %v",
						chanUpdate.AdvertisingNode)
				}
				switch chanUpdate.ConnectingNode {
				case net.Alice.PubKeyStr:
				case net.Bob.PubKeyStr:
				default:
					t.Fatalf("unknown connecting node: %v",
						chanUpdate.ConnectingNode)
				}

				if chanUpdate.Capacity != int64(chanAmt) {
					t.Fatalf("channel capacities mismatch:"+
						" expected %v, got %v", chanAmt,
						dcrutil.Amount(chanUpdate.Capacity))
				}
				numChannelUpds++
			}

			for _, nodeUpdate := range graphUpdate.NodeUpdates {
				switch nodeUpdate.IdentityKey {
				case net.Alice.PubKeyStr:
				case net.Bob.PubKeyStr:
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

	_, blockHeight, err := net.Miner.Node.GetBestBlock(context.Background())
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	// Now we'll test that updates are properly sent after channels are closed
	// within the network.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)

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
			chanPointTxid, err := dcrlnd.GetChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			closedChanTxid, err := dcrlnd.GetChanPointFundingTxid(
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
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.DisconnectNodes(ctxt, net.Alice, net.Bob); err != nil {
		t.Fatalf("unable to disconnect alice and bob: %v", err)
	}
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, net.Bob, carol); err != nil {
		t.Fatalf("unable to connect bob to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPoint = openChannelAndAssert(
		ctxt, t, net, net.Bob, carol,
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
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.EnsureConnected(ctxt, net.Alice, net.Bob); err != nil {
		t.Fatalf("unable to connect alice to bob: %v", err)
	}

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
				case net.Bob.PubKeyStr:
				default:
					t.Fatalf("unknown node update pubey: %v",
						nodeUpdate.IdentityKey)
				}
				numNodeAnns++
			}

			for _, chanUpdate := range graphUpdate.ChannelUpdates {
				switch chanUpdate.AdvertisingNode {
				case carol.PubKeyStr:
				case net.Bob.PubKeyStr:
				default:
					t.Fatalf("unknown advertising node: %v",
						chanUpdate.AdvertisingNode)
				}
				switch chanUpdate.ConnectingNode {
				case carol.PubKeyStr:
				case net.Bob.PubKeyStr:
				default:
					t.Fatalf("unknown connecting node: %v",
						chanUpdate.ConnectingNode)
				}

				if chanUpdate.Capacity != int64(chanAmt) {
					t.Fatalf("channel capacities mismatch:"+
						" expected %v, got %v", chanAmt,
						dcrutil.Amount(chanUpdate.Capacity))
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
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Bob, chanPoint, false)
}

// testNodeAnnouncement ensures that when a node is started with one or more
// external IP addresses specified on the command line, that those addresses
// announced to the network and reported in the network graph.
func testNodeAnnouncement(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	aliceSub := subscribeGraphNotifications(t, ctxb, net.Alice)
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

	dave, err := net.NewNode("Dave", lndArgs)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	// We must let Dave have an open channel before he can send a node
	// announcement, so we open a channel with Bob,
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, net.Bob, dave); err != nil {
		t.Fatalf("unable to connect bob to carol: %v", err)
	}

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
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Bob, dave,
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
							update.Addresses,
							targetAddrs...,
						)
						return
					}
				}
			case err := <-graphSub.errChan:
				t.Fatalf("unable to recv graph update: %v", err)
			case <-time.After(20 * time.Second):
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
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Bob, chanPoint, false)
}

func testNodeSignVerify(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	chanAmt := defaultChanAmt
	pushAmt := dcrutil.Amount(100000)

	// Create a channel between alice and bob.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	aliceBobCh := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)

	aliceMsg := []byte("alice msg")

	// alice signs "alice msg" and sends her signature to bob.
	sigReq := &lnrpc.SignMessageRequest{Msg: aliceMsg}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	sigResp, err := net.Alice.SignMessage(ctxt, sigReq)
	if err != nil {
		t.Fatalf("SignMessage rpc call failed: %v", err)
	}
	aliceSig := sigResp.Signature

	// bob verifying alice's signature should succeed since alice and bob are
	// connected.
	verifyReq := &lnrpc.VerifyMessageRequest{Msg: aliceMsg, Signature: aliceSig}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	verifyResp, err := net.Bob.VerifyMessage(ctxt, verifyReq)
	if err != nil {
		t.Fatalf("VerifyMessage failed: %v", err)
	}
	if !verifyResp.Valid {
		t.Fatalf("alice's signature didn't validate")
	}
	if verifyResp.Pubkey != net.Alice.PubKeyStr {
		t.Fatalf("alice's signature doesn't contain alice's pubkey.")
	}

	// carol is a new node that is unconnected to alice or bob.
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	carolMsg := []byte("carol msg")

	// carol signs "carol msg" and sends her signature to bob.
	sigReq = &lnrpc.SignMessageRequest{Msg: carolMsg}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	sigResp, err = carol.SignMessage(ctxt, sigReq)
	if err != nil {
		t.Fatalf("SignMessage rpc call failed: %v", err)
	}
	carolSig := sigResp.Signature

	// bob verifying carol's signature should fail since they are not connected.
	verifyReq = &lnrpc.VerifyMessageRequest{Msg: carolMsg, Signature: carolSig}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	verifyResp, err = net.Bob.VerifyMessage(ctxt, verifyReq)
	if err != nil {
		t.Fatalf("VerifyMessage failed: %v", err)
	}
	if verifyResp.Valid {
		t.Fatalf("carol's signature should not be valid")
	}
	if verifyResp.Pubkey != carol.PubKeyStr {
		t.Fatalf("carol's signature doesn't contain her pubkey")
	}

	// Close the channel between alice and bob.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, aliceBobCh, false)
}

// testAsyncPayments tests the performance of the async payments.
func testAsyncPayments(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const (
		paymentAmt = 100
	)

	// First establish a channel with a capacity equals to the overall
	// amount of payments, between Alice and Bob, at the end of the test
	// Alice should send all money from her side to Bob.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	channelCapacity := dcrutil.Amount(paymentAmt * 2000)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: channelCapacity,
		},
	)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	info, err := getChanInfo(ctxt, net.Alice)
	if err != nil {
		t.Fatalf("unable to get alice channel info: %v", err)
	}

	// We'll create a number of invoices equal the max number of HTLCs that
	// can be carried in one direction. The number on the commitment will
	// likely be lower, but we can't guarantee that any more HTLCs will
	// succeed due to the limited path diversity and inability of the
	// router to retry via another path.
	numInvoices := int(input.MaxHTLCNumber / 2)

	bobAmt := int64(numInvoices * paymentAmt)
	aliceAmt := info.LocalBalance - bobAmt

	// With the channel open, we'll create invoices for Bob that Alice
	// will pay to in order to advance the state of the channel.
	bobPayReqs, _, _, err := createPayReqs(
		net.Bob, paymentAmt, numInvoices,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// Wait for Alice to receive the channel edge from the funding manager.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't see the alice->bob channel before "+
			"timeout: %v", err)
	}

	// Simultaneously send payments from Alice to Bob using of Bob's payment
	// hashes generated above.
	now := time.Now()
	errChan := make(chan error)
	statusChan := make(chan *lnrpc.Payment)
	for i := 0; i < numInvoices; i++ {
		payReq := bobPayReqs[i]
		go func() {
			ctxt, _ = context.WithTimeout(ctxb, lntest.AsyncBenchmarkTimeout)
			stream, err := net.Alice.RouterClient.SendPaymentV2(
				ctxt,
				&routerrpc.SendPaymentRequest{
					PaymentRequest: payReq,
					TimeoutSeconds: 60,
					FeeLimitMAtoms: noFeeLimitMAtoms,
				},
			)
			if err != nil {
				errChan <- err
			}
			result, err := getPaymentResult(stream)
			if err != nil {
				errChan <- err
			}

			statusChan <- result
		}()
	}

	// Wait until all the payments have settled.
	for i := 0; i < numInvoices; i++ {
		select {
		case result := <-statusChan:
			if result.Status == lnrpc.Payment_SUCCEEDED {
				continue
			}

		case err := <-errChan:
			t.Fatalf("payment error: %v", err)
		}
	}

	// All payments have been sent, mark the finish time.
	timeTaken := time.Since(now)

	// Next query for Bob's and Alice's channel states, in order to confirm
	// that all payment have been successful transmitted.

	// Wait for the revocation to be received so alice no longer has pending
	// htlcs listed and has correct balances. This is needed due to the fact
	// that we now pipeline the settles.
	err = wait.Predicate(func() bool {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		aliceChan, err := getChanInfo(ctxt, net.Alice)
		if err != nil {
			return false
		}
		if len(aliceChan.PendingHtlcs) != 0 {
			return false
		}
		if aliceChan.RemoteBalance != bobAmt {
			return false
		}
		if aliceChan.LocalBalance != aliceAmt {
			return false
		}

		return true
	}, time.Second*5)
	if err != nil {
		t.Fatalf("failed to assert alice's pending htlcs and/or remote/local balance")
	}

	// Wait for Bob to receive revocation from Alice.
	time.Sleep(2 * time.Second)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	bobChan, err := getChanInfo(ctxt, net.Bob)
	if err != nil {
		t.Fatalf("unable to get bob's channel info: %v", err)
	}
	if len(bobChan.PendingHtlcs) != 0 {
		t.Fatalf("bob's pending htlcs is incorrect, got %v, "+
			"expected %v", len(bobChan.PendingHtlcs), 0)
	}
	if bobChan.LocalBalance != bobAmt {
		t.Fatalf("bob's local balance is incorrect, got %v, expected"+
			" %v", bobChan.LocalBalance, bobAmt)
	}
	if bobChan.RemoteBalance != aliceAmt {
		t.Fatalf("bob's remote balance is incorrect, got %v, "+
			"expected %v", bobChan.RemoteBalance, aliceAmt)
	}

	t.Log("\tBenchmark info: Elapsed time: ", timeTaken)
	t.Log("\tBenchmark info: TPS: ", float64(numInvoices)/timeTaken.Seconds())

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}

// testBidirectionalAsyncPayments tests that nodes are able to send the
// payments to each other in async manner without blocking.
func testBidirectionalAsyncPayments(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const (
		paymentAmt = 1000
	)

	// First establish a channel with a capacity equals to the overall
	// amount of payments, between Alice and Bob, at the end of the test
	// Alice should send all money from her side to Bob.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt:     paymentAmt * 500,
			PushAmt: paymentAmt * 250,
		},
	)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	info, err := getChanInfo(ctxt, net.Alice)
	if err != nil {
		t.Fatalf("unable to get alice channel info: %v", err)
	}

	// We'll create a number of invoices equal the max number of HTLCs that
	// can be carried in one direction. The number on the commitment will
	// likely be lower, but we can't guarantee that any more HTLCs will
	// succeed due to the limited path diversity and inability of the router
	// to retry via another path.
	numInvoices := int(input.MaxHTLCNumber / 2)

	// Nodes should exchange the same amount of money and because of this
	// at the end balances should remain the same.
	aliceAmt := info.LocalBalance
	bobAmt := info.RemoteBalance

	// With the channel open, we'll create invoices for Bob that Alice
	// will pay to in order to advance the state of the channel.
	bobPayReqs, _, _, err := createPayReqs(
		net.Bob, paymentAmt, numInvoices,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// With the channel open, we'll create invoices for Alice that Bob
	// will pay to in order to advance the state of the channel.
	alicePayReqs, _, _, err := createPayReqs(
		net.Alice, paymentAmt, numInvoices,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// Wait for Alice to receive the channel edge from the funding manager.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint); err != nil {
		t.Fatalf("alice didn't see the alice->bob channel before "+
			"timeout: %v", err)
	}
	if err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint); err != nil {
		t.Fatalf("bob didn't see the bob->alice channel before "+
			"timeout: %v", err)
	}

	// Reset mission control to prevent previous payment results from
	// interfering with this test. A new channel has been opened, but
	// mission control operates on node pairs.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = net.Alice.RouterClient.ResetMissionControl(
		ctxt, &routerrpc.ResetMissionControlRequest{},
	)
	if err != nil {
		t.Fatalf("unable to reset mc for alice: %v", err)
	}

	// Send payments from Alice to Bob and from Bob to Alice in async
	// manner.
	errChan := make(chan error)
	statusChan := make(chan *lnrpc.Payment)

	send := func(node *lntest.HarnessNode, payReq string) {
		go func() {
			ctxt, _ = context.WithTimeout(
				ctxb, lntest.AsyncBenchmarkTimeout,
			)
			stream, err := node.RouterClient.SendPaymentV2(
				ctxt,
				&routerrpc.SendPaymentRequest{
					PaymentRequest: payReq,
					TimeoutSeconds: 60,
					FeeLimitMAtoms: noFeeLimitMAtoms,
				},
			)
			if err != nil {
				errChan <- err
			}
			result, err := getPaymentResult(stream)
			if err != nil {
				errChan <- err
			}

			statusChan <- result
		}()
	}

	for i := 0; i < numInvoices; i++ {
		send(net.Bob, alicePayReqs[i])
		send(net.Alice, bobPayReqs[i])
	}

	// Expect all payments to succeed.
	for i := 0; i < 2*numInvoices; i++ {
		select {
		case result := <-statusChan:
			if result.Status != lnrpc.Payment_SUCCEEDED {
				t.Fatalf("payment error: %v", result.Status)
			}

		case err := <-errChan:
			t.Fatalf("payment error: %v", err)
		}
	}

	// Wait for Alice and Bob to receive revocations messages, and update
	// states, i.e. balance info.
	time.Sleep(1 * time.Second)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	aliceInfo, err := getChanInfo(ctxt, net.Alice)
	if err != nil {
		t.Fatalf("unable to get bob's channel info: %v", err)
	}
	if aliceInfo.RemoteBalance != bobAmt {
		t.Fatalf("alice's remote balance is incorrect, got %v, "+
			"expected %v", aliceInfo.RemoteBalance, bobAmt)
	}
	if aliceInfo.LocalBalance != aliceAmt {
		t.Fatalf("alice's local balance is incorrect, got %v, "+
			"expected %v", aliceInfo.LocalBalance, aliceAmt)
	}
	if len(aliceInfo.PendingHtlcs) != 0 {
		t.Fatalf("alice's pending htlcs is incorrect, got %v, "+
			"expected %v", len(aliceInfo.PendingHtlcs), 0)
	}

	// Next query for Bob's and Alice's channel states, in order to confirm
	// that all payment have been successful transmitted.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	bobInfo, err := getChanInfo(ctxt, net.Bob)
	if err != nil {
		t.Fatalf("unable to get bob's channel info: %v", err)
	}

	if bobInfo.LocalBalance != bobAmt {
		t.Fatalf("bob's local balance is incorrect, got %v, expected"+
			" %v", bobInfo.LocalBalance, bobAmt)
	}
	if bobInfo.RemoteBalance != aliceAmt {
		t.Fatalf("bob's remote balance is incorrect, got %v, "+
			"expected %v", bobInfo.RemoteBalance, aliceAmt)
	}
	if len(bobInfo.PendingHtlcs) != 0 {
		t.Fatalf("bob's pending htlcs is incorrect, got %v, "+
			"expected %v", len(bobInfo.PendingHtlcs), 0)
	}

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}

// assertActiveHtlcs makes sure all the passed nodes have the _exact_ HTLCs
// matching payHashes on _all_ their channels.
func assertActiveHtlcs(nodes []*lntest.HarnessNode, payHashes ...[]byte) error {
	ctxb := context.Background()

	req := &lnrpc.ListChannelsRequest{}
	for _, node := range nodes {
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		nodeChans, err := node.ListChannels(ctxt, req)
		if err != nil {
			return fmt.Errorf("unable to get node chans: %v", err)
		}

		for _, channel := range nodeChans.Channels {
			// Record all payment hashes active for this channel.
			htlcHashes := make(map[string]struct{})
			for _, htlc := range channel.PendingHtlcs {
				_, ok := htlcHashes[string(htlc.HashLock)]
				if ok {
					return fmt.Errorf("duplicate HashLock")
				}
				htlcHashes[string(htlc.HashLock)] = struct{}{}
			}

			// Channel should have exactly the payHashes active.
			if len(payHashes) != len(htlcHashes) {
				return fmt.Errorf("node %x had %v htlcs active, "+
					"expected %v", node.PubKey[:],
					len(htlcHashes), len(payHashes))
			}

			// Make sure all the payHashes are active.
			for _, payHash := range payHashes {
				if _, ok := htlcHashes[string(payHash)]; ok {
					continue
				}
				return fmt.Errorf("node %x didn't have the "+
					"payHash %v active", node.PubKey[:],
					payHash)
			}
		}
	}

	return nil
}

func assertNumActiveHtlcsChanPoint(node *lntest.HarnessNode,
	chanPoint wire.OutPoint, numHtlcs int) error {
	ctxb := context.Background()

	req := &lnrpc.ListChannelsRequest{}
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	nodeChans, err := node.ListChannels(ctxt, req)
	if err != nil {
		return err
	}

	for _, channel := range nodeChans.Channels {
		if channel.ChannelPoint != chanPoint.String() {
			continue
		}

		if len(channel.PendingHtlcs) != numHtlcs {
			return fmt.Errorf("expected %v active HTLCs, got %v",
				numHtlcs, len(channel.PendingHtlcs))
		}
		return nil
	}

	return fmt.Errorf("channel point %v not found", chanPoint)
}

func assertNumActiveHtlcs(nodes []*lntest.HarnessNode, numHtlcs int) error {
	ctxb := context.Background()

	req := &lnrpc.ListChannelsRequest{}
	for _, node := range nodes {
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		nodeChans, err := node.ListChannels(ctxt, req)
		if err != nil {
			return err
		}

		for _, channel := range nodeChans.Channels {
			if len(channel.PendingHtlcs) != numHtlcs {
				return fmt.Errorf("expected %v HTLCs, got %v",
					numHtlcs, len(channel.PendingHtlcs))
			}
		}
	}

	return nil
}

func assertSpendingTxInMempool(t *harnessTest, miner *rpcclient.Client,
	timeout time.Duration, chanPoint wire.OutPoint) chainhash.Hash {

	tx := getSpendingTxInMempool(t, miner, timeout, chanPoint)
	return tx.TxHash()
}

// getSpendingTxInMempool waits for a transaction spending the given outpoint to
// appear in the mempool and returns that tx in full.
func getSpendingTxInMempool(t *harnessTest, miner *rpcclient.Client,
	timeout time.Duration, chanPoint wire.OutPoint) *wire.MsgTx {

	breakTimeout := time.After(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-breakTimeout:
			t.Fatalf("didn't find tx in mempool")
		case <-ticker.C:
			mempool, err := miner.GetRawMempool(context.Background(), jsonrpctypes.GRMRegular)
			if err != nil {
				t.Fatalf("unable to get mempool: %v", err)
			}

			if len(mempool) == 0 {
				continue
			}

			for _, txid := range mempool {
				tx, err := miner.GetRawTransaction(context.Background(), txid)
				if err != nil {
					t.Fatalf("unable to fetch tx: %v", err)
				}

				msgTx := tx.MsgTx()
				for _, txIn := range msgTx.TxIn {
					if txIn.PreviousOutPoint == chanPoint {
						return msgTx
					}
				}
			}
		}
	}
}

// testSwitchCircuitPersistence creates a multihop network to ensure the sender
// and intermediaries are persisting their open payment circuits. After
// forwarding a packet via an outgoing link, all are restarted, and expected to
// forward a response back from the receiver once back online.
//
// The general flow of this test:
//   1. Carol --> Dave --> Alice --> Bob  forward payment
//   2.        X        X         X  Bob  restart sender and intermediaries
//   3. Carol <-- Dave <-- Alice <-- Bob  expect settle to propagate
func testSwitchCircuitPersistence(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const chanAmt = dcrutil.Amount(1000000)
	const pushAmt = dcrutil.Amount(900000)
	var networkChans []*lnrpc.ChannelPoint

	// Open a channel with 100k atoms between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointAlice := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	networkChans = append(networkChans, chanPointAlice)

	aliceChanTXID, err := dcrlnd.GetChanPointFundingTxid(chanPointAlice)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// As preliminary setup, we'll create two new nodes: Carol and Dave,
	// such that we now have a 4 ndoe, 3 channel topology. Dave will make
	// a channel with Alice, and Carol with Dave. After this setup, the
	// network topology should now look like:
	//     Carol -> Dave -> Alice -> Bob
	//
	// First, we'll create Dave and establish a channel to Alice.
	dave, err := net.NewNode("Dave", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, dave, net.Alice); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, dave)
	if err != nil {
		t.Fatalf("unable to send coins to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointDave := openChannelAndAssert(
		ctxt, t, net, dave, net.Alice,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	networkChans = append(networkChans, chanPointDave)
	daveChanTXID, err := dcrlnd.GetChanPointFundingTxid(chanPointDave)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	daveFundPoint := wire.OutPoint{
		Hash:  *daveChanTXID,
		Index: chanPointDave.OutputIndex,
	}

	// Next, we'll create Carol and establish a channel to from her to
	// Dave. Carol is started in htlchodl mode so that we can disconnect the
	// intermediary hops before starting the settle.
	carol, err := net.NewNode("Carol", []string{"--hodl.exit-settle"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, carol, dave); err != nil {
		t.Fatalf("unable to connect carol to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointCarol := openChannelAndAssert(
		ctxt, t, net, carol, dave,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	networkChans = append(networkChans, chanPointCarol)

	carolChanTXID, err := dcrlnd.GetChanPointFundingTxid(chanPointCarol)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	carolFundPoint := wire.OutPoint{
		Hash:  *carolChanTXID,
		Index: chanPointCarol.OutputIndex,
	}

	// Wait for all nodes to have seen all channels.
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol, dave}
	nodeNames := []string{"Alice", "Bob", "Carol", "Dave"}
	for _, chanPoint := range networkChans {
		for i, node := range nodes {
			txid, err := dcrlnd.GetChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
			err = node.WaitForNetworkChannelOpen(ctxt, chanPoint)
			if err != nil {
				t.Fatalf("%s(%d): timeout waiting for "+
					"channel(%s) open: %v", nodeNames[i],
					node.NodeID, point, err)
			}
		}
	}

	// Create 5 invoices for Carol, which expect a payment from Bob for 1k
	// atoms with a different preimage each time.
	const numPayments = 5
	const paymentAmt = 1000
	payReqs, _, _, err := createPayReqs(
		carol, paymentAmt, numPayments,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// We'll wait for all parties to recognize the new channels within the
	// network.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = dave.WaitForNetworkChannelOpen(ctxt, chanPointDave)
	if err != nil {
		t.Fatalf("dave didn't advertise his channel: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointCarol)
	if err != nil {
		t.Fatalf("carol didn't advertise her channel in time: %v",
			err)
	}

	time.Sleep(time.Millisecond * 50)

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, net.Bob, net.Bob.RouterClient, payReqs, false,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Wait until all nodes in the network have 5 outstanding htlcs.
	var predErr error
	err = wait.Predicate(func() bool {
		predErr = assertNumActiveHtlcs(nodes, numPayments)
		return predErr == nil
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// Restart the intermediaries and the sender.
	if err := net.RestartNode(dave, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	if err := net.RestartNode(net.Bob, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Ensure all of the intermediate links are reconnected.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.EnsureConnected(ctxt, net.Alice, dave)
	if err != nil {
		t.Fatalf("unable to reconnect alice and dave: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.EnsureConnected(ctxt, net.Bob, net.Alice)
	if err != nil {
		t.Fatalf("unable to reconnect bob and alice: %v", err)
	}

	// Ensure all nodes in the network still have 5 outstanding htlcs.
	err = wait.Predicate(func() bool {
		predErr = assertNumActiveHtlcs(nodes, numPayments)
		return predErr == nil
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// Now restart carol without hodl mode, to settle back the outstanding
	// payments.
	carol.SetExtraArgs(nil)
	if err := net.RestartNode(carol, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.EnsureConnected(ctxt, dave, carol)
	if err != nil {
		t.Fatalf("unable to reconnect dave and carol: %v", err)
	}

	// After the payments settle, there should be no active htlcs on any of
	// the nodes in the network.
	err = wait.Predicate(func() bool {
		predErr = assertNumActiveHtlcs(nodes, 0)
		return predErr == nil
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// When asserting the amount of atoms moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when
	// creating the seed nodes in the network.
	const baseFee = 1

	// At this point all the channels within our proto network should be
	// shifted by 5k atoms in the direction of Carol, the sink within the
	// payment flow generated above. The order of asserts corresponds to
	// increasing of time is needed to embed the HTLC in commitment
	// transaction, in channel Bob->Alice->David->Carol, order is Carol,
	// David, Alice, Bob.
	var amountPaid = int64(5000)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", carol,
		carolFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", dave,
		carolFundPoint, amountPaid, int64(0))
	assertAmountPaid(t, "Alice(local) => Dave(remote)", dave,
		daveFundPoint, int64(0), amountPaid+(baseFee*numPayments))
	assertAmountPaid(t, "Alice(local) => Dave(remote)", net.Alice,
		daveFundPoint, amountPaid+(baseFee*numPayments), int64(0))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Alice,
		aliceFundPoint, int64(0), amountPaid+((baseFee*numPayments)*2))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Bob,
		aliceFundPoint, amountPaid+(baseFee*numPayments)*2, int64(0))

	// Lastly, we will send one more payment to ensure all channels are
	// still functioning properly.
	finalInvoice := &lnrpc.Invoice{
		Memo:  "testing",
		Value: paymentAmt,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	resp, err := carol.AddInvoice(ctxt, finalInvoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	payReqs = []string{resp.PaymentRequest}

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, net.Bob, net.Bob.RouterClient, payReqs, true,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	amountPaid = int64(6000)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", carol,
		carolFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", dave,
		carolFundPoint, amountPaid, int64(0))
	assertAmountPaid(t, "Alice(local) => Dave(remote)", dave,
		daveFundPoint, int64(0), amountPaid+(baseFee*(numPayments+1)))
	assertAmountPaid(t, "Alice(local) => Dave(remote)", net.Alice,
		daveFundPoint, amountPaid+(baseFee*(numPayments+1)), int64(0))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Alice,
		aliceFundPoint, int64(0), amountPaid+((baseFee*(numPayments+1))*2))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Bob,
		aliceFundPoint, amountPaid+(baseFee*(numPayments+1))*2, int64(0))

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, dave, chanPointDave, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarol, false)
}

// testSwitchOfflineDelivery constructs a set of multihop payments, and tests
// that the returning payments are not lost if a peer on the backwards path is
// offline when the settle/fails are received. We expect the payments to be
// buffered in memory, and transmitted as soon as the disconnect link comes back
// online.
//
// The general flow of this test:
//   1. Carol --> Dave --> Alice --> Bob  forward payment
//   2. Carol --- Dave  X  Alice --- Bob  disconnect intermediaries
//   3. Carol --- Dave  X  Alice <-- Bob  settle last hop
//   4. Carol <-- Dave <-- Alice --- Bob  reconnect, expect settle to propagate
func testSwitchOfflineDelivery(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const chanAmt = dcrutil.Amount(1000000)
	const pushAmt = dcrutil.Amount(900000)
	var networkChans []*lnrpc.ChannelPoint

	// Open a channel with 100k atoms between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointAlice := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	networkChans = append(networkChans, chanPointAlice)

	aliceChanTXID, err := dcrlnd.GetChanPointFundingTxid(chanPointAlice)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// As preliminary setup, we'll create two new nodes: Carol and Dave,
	// such that we now have a 4 ndoe, 3 channel topology. Dave will make
	// a channel with Alice, and Carol with Dave. After this setup, the
	// network topology should now look like:
	//     Carol -> Dave -> Alice -> Bob
	//
	// First, we'll create Dave and establish a channel to Alice.
	dave, err := net.NewNode("Dave", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, dave, net.Alice); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, dave)
	if err != nil {
		t.Fatalf("unable to send coins to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointDave := openChannelAndAssert(
		ctxt, t, net, dave, net.Alice,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	networkChans = append(networkChans, chanPointDave)
	daveChanTXID, err := dcrlnd.GetChanPointFundingTxid(chanPointDave)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	daveFundPoint := wire.OutPoint{
		Hash:  *daveChanTXID,
		Index: chanPointDave.OutputIndex,
	}

	// Next, we'll create Carol and establish a channel to from her to
	// Dave. Carol is started in htlchodl mode so that we can disconnect the
	// intermediary hops before starting the settle.
	carol, err := net.NewNode("Carol", []string{"--hodl.exit-settle",
		"--nolisten"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, carol, dave); err != nil {
		t.Fatalf("unable to connect carol to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointCarol := openChannelAndAssert(
		ctxt, t, net, carol, dave,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	networkChans = append(networkChans, chanPointCarol)

	carolChanTXID, err := dcrlnd.GetChanPointFundingTxid(chanPointCarol)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	carolFundPoint := wire.OutPoint{
		Hash:  *carolChanTXID,
		Index: chanPointCarol.OutputIndex,
	}

	// Wait for all nodes to have seen all channels.
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol, dave}
	nodeNames := []string{"Alice", "Bob", "Carol", "Dave"}
	for _, chanPoint := range networkChans {
		for i, node := range nodes {
			txid, err := dcrlnd.GetChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
			err = node.WaitForNetworkChannelOpen(ctxt, chanPoint)
			if err != nil {
				t.Fatalf("%s(%d): timeout waiting for "+
					"channel(%s) open: %v", nodeNames[i],
					node.NodeID, point, err)
			}
		}
	}

	// Create 5 invoices for Carol, which expect a payment from Bob for 1k
	// atoms with a different preimage each time.
	const numPayments = 5
	const paymentAmt = 1000
	payReqs, _, _, err := createPayReqs(
		carol, paymentAmt, numPayments,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// We'll wait for all parties to recognize the new channels within the
	// network.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = dave.WaitForNetworkChannelOpen(ctxt, chanPointDave)
	if err != nil {
		t.Fatalf("dave didn't advertise his channel: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointCarol)
	if err != nil {
		t.Fatalf("carol didn't advertise her channel in time: %v",
			err)
	}

	time.Sleep(time.Millisecond * 50)

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, net.Bob, net.Bob.RouterClient, payReqs, false,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Wait for all of the payments to reach Carol.
	var predErr error
	err = wait.Predicate(func() bool {
		predErr = assertNumActiveHtlcs(nodes, numPayments)
		return predErr == nil
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// First, disconnect Dave and Alice so that their link is broken.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.DisconnectNodes(ctxt, dave, net.Alice); err != nil {
		t.Fatalf("unable to disconnect alice from dave: %v", err)
	}

	// Then, reconnect them to ensure Dave doesn't just fail back the htlc.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, dave, net.Alice); err != nil {
		t.Fatalf("unable to reconnect alice to dave: %v", err)
	}

	// Wait to ensure that the payment remain are not failed back after
	// reconnecting. All node should report the number payments initiated
	// for the duration of the interval.
	err = wait.Invariant(func() bool {
		predErr = assertNumActiveHtlcs(nodes, numPayments)
		return predErr == nil
	}, time.Second*2)
	if err != nil {
		t.Fatalf("htlc change: %v", predErr)
	}

	// Now, disconnect Dave from Alice again before settling back the
	// payment.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.DisconnectNodes(ctxt, dave, net.Alice); err != nil {
		t.Fatalf("unable to disconnect alice from dave: %v", err)
	}

	// Now restart carol without hodl mode, to settle back the outstanding
	// payments.
	carol.SetExtraArgs(nil)
	if err := net.RestartNode(carol, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Wait for Carol to report no outstanding htlcs.
	carolNode := []*lntest.HarnessNode{carol}
	err = wait.Predicate(func() bool {
		predErr = assertNumActiveHtlcs(carolNode, 0)
		return predErr == nil
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// Now that the settles have reached Dave, reconnect him with Alice,
	// allowing the settles to return to the sender.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.EnsureConnected(ctxt, dave, net.Alice); err != nil {
		t.Fatalf("unable to reconnect alice to dave: %v", err)
	}

	// Wait until all outstanding htlcs in the network have been settled.
	err = wait.Predicate(func() bool {
		predErr = assertNumActiveHtlcs(nodes, 0)
		return predErr == nil
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// When asserting the amount of atoms moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when
	// creating the seed nodes in the network.
	const baseFee = 1

	// At this point all the channels within our proto network should be
	// shifted by 5k atoms in the direction of Carol, the sink within the
	// payment flow generated above. The order of asserts corresponds to
	// increasing of time is needed to embed the HTLC in commitment
	// transaction, in channel Bob->Alice->David->Carol, order is Carol,
	// David, Alice, Bob.
	var amountPaid = int64(5000)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", carol,
		carolFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", dave,
		carolFundPoint, amountPaid, int64(0))
	assertAmountPaid(t, "Alice(local) => Dave(remote)", dave,
		daveFundPoint, int64(0), amountPaid+(baseFee*numPayments))
	assertAmountPaid(t, "Alice(local) => Dave(remote)", net.Alice,
		daveFundPoint, amountPaid+(baseFee*numPayments), int64(0))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Alice,
		aliceFundPoint, int64(0), amountPaid+((baseFee*numPayments)*2))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Bob,
		aliceFundPoint, amountPaid+(baseFee*numPayments)*2, int64(0))

	// Ensure Carol and Dave are connected so the final payment will
	// complete.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.EnsureConnected(ctxt, carol, dave); err != nil {
		t.Fatalf("unable to reconnect carol to dave: %v", err)
	}

	// Lastly, we will send one more payment to ensure all channels are
	// still functioning properly.
	finalInvoice := &lnrpc.Invoice{
		Memo:  "testing",
		Value: paymentAmt,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	resp, err := carol.AddInvoice(ctxt, finalInvoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	payReqs = []string{resp.PaymentRequest}

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, net.Bob, net.Bob.RouterClient, payReqs, true,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	amountPaid = int64(6000)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", carol,
		carolFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", dave,
		carolFundPoint, amountPaid, int64(0))
	assertAmountPaid(t, "Alice(local) => Dave(remote)", dave,
		daveFundPoint, int64(0), amountPaid+(baseFee*(numPayments+1)))
	assertAmountPaid(t, "Alice(local) => Dave(remote)", net.Alice,
		daveFundPoint, amountPaid+(baseFee*(numPayments+1)), int64(0))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Alice,
		aliceFundPoint, int64(0), amountPaid+((baseFee*(numPayments+1))*2))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Bob,
		aliceFundPoint, amountPaid+(baseFee*(numPayments+1))*2, int64(0))

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, dave, chanPointDave, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarol, false)
}

// testSwitchOfflineDeliveryPersistence constructs a set of multihop payments,
// and tests that the returning payments are not lost if a peer on the backwards
// path is offline when the settle/fails are received AND the peer buffering the
// responses is completely restarts. We expect the payments to be reloaded from
// disk, and transmitted as soon as the intermediaries are reconnected.
//
// The general flow of this test:
//   1. Carol --> Dave --> Alice --> Bob  forward payment
//   2. Carol --- Dave  X  Alice --- Bob  disconnect intermediaries
//   3. Carol --- Dave  X  Alice <-- Bob  settle last hop
//   4. Carol --- Dave  X         X  Bob  restart Alice
//   5. Carol <-- Dave <-- Alice --- Bob  expect settle to propagate
func testSwitchOfflineDeliveryPersistence(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const chanAmt = dcrutil.Amount(1000000)
	const pushAmt = dcrutil.Amount(900000)
	var networkChans []*lnrpc.ChannelPoint

	// Open a channel with 100k atoms between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointAlice := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	networkChans = append(networkChans, chanPointAlice)

	aliceChanTXID, err := dcrlnd.GetChanPointFundingTxid(chanPointAlice)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// As preliminary setup, we'll create two new nodes: Carol and Dave,
	// such that we now have a 4 ndoe, 3 channel topology. Dave will make
	// a channel with Alice, and Carol with Dave. After this setup, the
	// network topology should now look like:
	//     Carol -> Dave -> Alice -> Bob
	//
	// First, we'll create Dave and establish a channel to Alice.
	dave, err := net.NewNode("Dave", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, dave, net.Alice); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, dave)
	if err != nil {
		t.Fatalf("unable to send coins to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointDave := openChannelAndAssert(
		ctxt, t, net, dave, net.Alice,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)

	networkChans = append(networkChans, chanPointDave)
	daveChanTXID, err := dcrlnd.GetChanPointFundingTxid(chanPointDave)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	daveFundPoint := wire.OutPoint{
		Hash:  *daveChanTXID,
		Index: chanPointDave.OutputIndex,
	}

	// Next, we'll create Carol and establish a channel to from her to
	// Dave. Carol is started in htlchodl mode so that we can disconnect the
	// intermediary hops before starting the settle.
	carol, err := net.NewNode("Carol", []string{"--hodl.exit-settle"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, carol, dave); err != nil {
		t.Fatalf("unable to connect carol to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointCarol := openChannelAndAssert(
		ctxt, t, net, carol, dave,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	networkChans = append(networkChans, chanPointCarol)

	carolChanTXID, err := dcrlnd.GetChanPointFundingTxid(chanPointCarol)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	carolFundPoint := wire.OutPoint{
		Hash:  *carolChanTXID,
		Index: chanPointCarol.OutputIndex,
	}

	// Wait for all nodes to have seen all channels.
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol, dave}
	nodeNames := []string{"Alice", "Bob", "Carol", "Dave"}
	for _, chanPoint := range networkChans {
		for i, node := range nodes {
			txid, err := dcrlnd.GetChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
			err = node.WaitForNetworkChannelOpen(ctxt, chanPoint)
			if err != nil {
				t.Fatalf("%s(%d): timeout waiting for "+
					"channel(%s) open: %v", nodeNames[i],
					node.NodeID, point, err)
			}
		}
	}

	// Create 5 invoices for Carol, which expect a payment from Bob for 1k
	// atoms with a different preimage each time.
	const numPayments = 5
	const paymentAmt = 1000
	payReqs, _, _, err := createPayReqs(
		carol, paymentAmt, numPayments,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// We'll wait for all parties to recognize the new channels within the
	// network.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = dave.WaitForNetworkChannelOpen(ctxt, chanPointDave)
	if err != nil {
		t.Fatalf("dave didn't advertise his channel: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointCarol)
	if err != nil {
		t.Fatalf("carol didn't advertise her channel in time: %v",
			err)
	}

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, net.Bob, net.Bob.RouterClient, payReqs, false,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	var predErr error
	err = wait.Predicate(func() bool {
		predErr = assertNumActiveHtlcs(nodes, numPayments)
		return predErr == nil
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// Disconnect the two intermediaries, Alice and Dave, by shutting down
	// Alice.
	if err := net.StopNode(net.Alice); err != nil {
		t.Fatalf("unable to shutdown alice: %v", err)
	}

	// Now restart carol without hodl mode, to settle back the outstanding
	// payments.
	carol.SetExtraArgs(nil)
	if err := net.RestartNode(carol, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Make Carol and Dave are reconnected before waiting for the htlcs to
	// clear.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.EnsureConnected(ctxt, dave, carol)
	if err != nil {
		t.Fatalf("unable to reconnect dave and carol: %v", err)
	}

	// Wait for Carol to report no outstanding htlcs, and also for Dav to
	// receive all the settles from Carol.
	carolNode := []*lntest.HarnessNode{carol}
	err = wait.Predicate(func() bool {
		predErr = assertNumActiveHtlcs(carolNode, 0)
		if predErr != nil {
			return false
		}

		predErr = assertNumActiveHtlcsChanPoint(dave, carolFundPoint, 0)
		return predErr == nil
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// Finally, restart dave who received the settles, but was unable to
	// deliver them to Alice since they were disconnected.
	if err := net.RestartNode(dave, nil); err != nil {
		t.Fatalf("unable to restart dave: %v", err)
	}
	if err = net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("unable to restart alice: %v", err)
	}

	// Force Dave and Alice to reconnect before waiting for the htlcs to
	// clear.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.EnsureConnected(ctxt, dave, net.Alice)
	if err != nil {
		t.Fatalf("unable to reconnect dave and carol: %v", err)
	}

	// Also ensure Alice and Bob have reconnected.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.EnsureConnected(ctxt, net.Alice, net.Bob)
	if err != nil {
		t.Fatalf("unable to reconnect alice and bob: %v", err)
	}

	// After reconnection succeeds, the settles should be propagated all
	// the way back to the sender. All nodes should report no active htlcs.
	err = wait.Predicate(func() bool {
		predErr = assertNumActiveHtlcs(nodes, 0)
		return predErr == nil
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// When asserting the amount of atoms moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when
	// creating the seed nodes in the network.
	const baseFee = 1

	// At this point all the channels within our proto network should be
	// shifted by 5k atoms in the direction of Carol, the sink within the
	// payment flow generated above. The order of asserts corresponds to
	// increasing of time is needed to embed the HTLC in commitment
	// transaction, in channel Bob->Alice->David->Carol, order is Carol,
	// David, Alice, Bob.
	var amountPaid = int64(5000)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", carol,
		carolFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", dave,
		carolFundPoint, amountPaid, int64(0))
	assertAmountPaid(t, "Alice(local) => Dave(remote)", dave,
		daveFundPoint, int64(0), amountPaid+(baseFee*numPayments))
	assertAmountPaid(t, "Alice(local) => Dave(remote)", net.Alice,
		daveFundPoint, amountPaid+(baseFee*numPayments), int64(0))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Alice,
		aliceFundPoint, int64(0), amountPaid+((baseFee*numPayments)*2))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Bob,
		aliceFundPoint, amountPaid+(baseFee*numPayments)*2, int64(0))

	// Before completing the final payment request, ensure that the
	// connection between Dave and Carol has been healed.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.EnsureConnected(ctxt, dave, carol)
	if err != nil {
		t.Fatalf("unable to reconnect dave and carol: %v", err)
	}

	// Lastly, we will send one more payment to ensure all channels are
	// still functioning properly.
	finalInvoice := &lnrpc.Invoice{
		Memo:  "testing",
		Value: paymentAmt,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	resp, err := carol.AddInvoice(ctxt, finalInvoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	payReqs = []string{resp.PaymentRequest}

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, net.Bob, net.Bob.RouterClient, payReqs, true,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	amountPaid = int64(6000)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", carol,
		carolFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", dave,
		carolFundPoint, amountPaid, int64(0))
	assertAmountPaid(t, "Alice(local) => Dave(remote)", dave,
		daveFundPoint, int64(0), amountPaid+(baseFee*(numPayments+1)))
	assertAmountPaid(t, "Alice(local) => Dave(remote)", net.Alice,
		daveFundPoint, amountPaid+(baseFee*(numPayments+1)), int64(0))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Alice,
		aliceFundPoint, int64(0), amountPaid+((baseFee*(numPayments+1))*2))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Bob,
		aliceFundPoint, amountPaid+(baseFee*(numPayments+1))*2, int64(0))

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, dave, chanPointDave, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarol, false)
}

// testSwitchOfflineDeliveryOutgoingOffline constructs a set of multihop payments,
// and tests that the returning payments are not lost if a peer on the backwards
// path is offline when the settle/fails are received AND the peer buffering the
// responses is completely restarts. We expect the payments to be reloaded from
// disk, and transmitted as soon as the intermediaries are reconnected.
//
// The general flow of this test:
//   1. Carol --> Dave --> Alice --> Bob  forward payment
//   2. Carol --- Dave  X  Alice --- Bob  disconnect intermediaries
//   3. Carol --- Dave  X  Alice <-- Bob  settle last hop
//   4. Carol --- Dave  X         X       shutdown Bob, restart Alice
//   5. Carol <-- Dave <-- Alice  X       expect settle to propagate
func testSwitchOfflineDeliveryOutgoingOffline(
	net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const chanAmt = dcrutil.Amount(1000000)
	const pushAmt = dcrutil.Amount(900000)
	var networkChans []*lnrpc.ChannelPoint

	// Open a channel with 100k atoms between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointAlice := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	networkChans = append(networkChans, chanPointAlice)

	aliceChanTXID, err := dcrlnd.GetChanPointFundingTxid(chanPointAlice)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// As preliminary setup, we'll create two new nodes: Carol and Dave,
	// such that we now have a 4 ndoe, 3 channel topology. Dave will make
	// a channel with Alice, and Carol with Dave. After this setup, the
	// network topology should now look like:
	//     Carol -> Dave -> Alice -> Bob
	//
	// First, we'll create Dave and establish a channel to Alice.
	dave, err := net.NewNode("Dave", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, dave, net.Alice); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, dave)
	if err != nil {
		t.Fatalf("unable to send coins to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointDave := openChannelAndAssert(
		ctxt, t, net, dave, net.Alice,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	networkChans = append(networkChans, chanPointDave)
	daveChanTXID, err := dcrlnd.GetChanPointFundingTxid(chanPointDave)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	daveFundPoint := wire.OutPoint{
		Hash:  *daveChanTXID,
		Index: chanPointDave.OutputIndex,
	}

	// Next, we'll create Carol and establish a channel to from her to
	// Dave. Carol is started in htlchodl mode so that we can disconnect the
	// intermediary hops before starting the settle.
	carol, err := net.NewNode("Carol", []string{"--hodl.exit-settle"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, carol, dave); err != nil {
		t.Fatalf("unable to connect carol to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointCarol := openChannelAndAssert(
		ctxt, t, net, carol, dave,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	networkChans = append(networkChans, chanPointCarol)

	carolChanTXID, err := dcrlnd.GetChanPointFundingTxid(chanPointCarol)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	carolFundPoint := wire.OutPoint{
		Hash:  *carolChanTXID,
		Index: chanPointCarol.OutputIndex,
	}

	// Wait for all nodes to have seen all channels.
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol, dave}
	nodeNames := []string{"Alice", "Bob", "Carol", "Dave"}
	for _, chanPoint := range networkChans {
		for i, node := range nodes {
			txid, err := dcrlnd.GetChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
			err = node.WaitForNetworkChannelOpen(ctxt, chanPoint)
			if err != nil {
				t.Fatalf("%s(%d): timeout waiting for "+
					"channel(%s) open: %v", nodeNames[i],
					node.NodeID, point, err)
			}
		}
	}

	// Create 5 invoices for Carol, which expect a payment from Bob for 1k
	// atoms with a different preimage each time.
	const numPayments = 5
	const paymentAmt = 1000
	payReqs, _, _, err := createPayReqs(
		carol, paymentAmt, numPayments,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// We'll wait for all parties to recognize the new channels within the
	// network.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = dave.WaitForNetworkChannelOpen(ctxt, chanPointDave)
	if err != nil {
		t.Fatalf("dave didn't advertise his channel: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointCarol)
	if err != nil {
		t.Fatalf("carol didn't advertise her channel in time: %v",
			err)
	}

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, net.Bob, net.Bob.RouterClient, payReqs, false,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Wait for all payments to reach Carol.
	var predErr error
	err = wait.Predicate(func() bool {
		predErr = assertNumActiveHtlcs(nodes, numPayments)
		return predErr == nil
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// Disconnect the two intermediaries, Alice and Dave, so that when carol
	// restarts, the response will be held by Dave.
	if err := net.StopNode(net.Alice); err != nil {
		t.Fatalf("unable to shutdown alice: %v", err)
	}

	// Now restart carol without hodl mode, to settle back the outstanding
	// payments.
	carol.SetExtraArgs(nil)
	if err := net.RestartNode(carol, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Wait for Carol to report no outstanding htlcs.
	carolNode := []*lntest.HarnessNode{carol}
	err = wait.Predicate(func() bool {
		predErr = assertNumActiveHtlcs(carolNode, 0)
		if predErr != nil {
			return false
		}

		predErr = assertNumActiveHtlcsChanPoint(dave, carolFundPoint, 0)
		return predErr == nil
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// Now check that the total amount was transferred from Dave to Carol.
	// The amount transferred should be exactly equal to the invoice total
	// payment amount, 5k atomssohis.
	const amountPaid = int64(5000)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", carol,
		carolFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", dave,
		carolFundPoint, amountPaid, int64(0))

	// Shutdown carol and leave her offline for the rest of the test. This
	// is critical, as we wish to see if Dave can propragate settles even if
	// the outgoing link is never revived.
	shutdownAndAssert(net, t, carol)

	// Now restart Dave, ensuring he is both persisting the settles, and is
	// able to reforward them to Alice after recovering from a restart.
	if err := net.RestartNode(dave, nil); err != nil {
		t.Fatalf("unable to restart dave: %v", err)
	}
	if err = net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("unable to restart alice: %v", err)
	}

	// Ensure that Dave is reconnected to Alice before waiting for the
	// htlcs to clear.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.EnsureConnected(ctxt, dave, net.Alice)
	if err != nil {
		t.Fatalf("unable to reconnect alice and dave: %v", err)
	}

	// Since Carol has been shutdown permanently, we will wait until all
	// other nodes in the network report no active htlcs.
	nodesMinusCarol := []*lntest.HarnessNode{net.Bob, net.Alice, dave}
	err = wait.Predicate(func() bool {
		predErr = assertNumActiveHtlcs(nodesMinusCarol, 0)
		return predErr == nil
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// When asserting the amount of atoms moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when
	// creating the seed nodes in the network.
	const baseFee = 1

	// At this point, all channels (minus Carol, who is shutdown) should
	// show a shift of 5k atoms towards Carol.  The order of asserts
	// corresponds to increasing of time is needed to embed the HTLC in
	// commitment transaction, in channel Bob->Alice->David, order is
	// David, Alice, Bob.
	assertAmountPaid(t, "Alice(local) => Dave(remote)", dave,
		daveFundPoint, int64(0), amountPaid+(baseFee*numPayments))
	assertAmountPaid(t, "Alice(local) => Dave(remote)", net.Alice,
		daveFundPoint, amountPaid+(baseFee*numPayments), int64(0))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Alice,
		aliceFundPoint, int64(0), amountPaid+((baseFee*numPayments)*2))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Bob,
		aliceFundPoint, amountPaid+(baseFee*numPayments)*2, int64(0))

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, dave, chanPointDave, false)
}

// computeFee calculates the payment fee as specified in BOLT07
func computeFee(baseFee, feeRate, amt lnwire.MilliAtom) lnwire.MilliAtom {
	return baseFee + amt*feeRate/1000000
}

// testQueryRoutes checks the response of queryroutes.
// We'll create the following network topology:
//      Alice --> Bob --> Carol --> Dave
// and query the daemon for routes from Alice to Dave.
func testQueryRoutes(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const chanAmt = dcrutil.Amount(100000)
	var networkChans []*lnrpc.ChannelPoint

	// Open a channel between Alice and Bob.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointAlice := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	networkChans = append(networkChans, chanPointAlice)

	// Create Carol and establish a channel from Bob.
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, carol, net.Bob); err != nil {
		t.Fatalf("unable to connect carol to bob: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, net.Bob)
	if err != nil {
		t.Fatalf("unable to send coins to bob: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointBob := openChannelAndAssert(
		ctxt, t, net, net.Bob, carol,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	networkChans = append(networkChans, chanPointBob)

	// Create Dave and establish a channel from Carol.
	dave, err := net.NewNode("Dave", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, dave, carol); err != nil {
		t.Fatalf("unable to connect dave to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointCarol := openChannelAndAssert(
		ctxt, t, net, carol, dave,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	networkChans = append(networkChans, chanPointCarol)

	// Wait for all nodes to have seen all channels.
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol, dave}
	nodeNames := []string{"Alice", "Bob", "Carol", "Dave"}
	for _, chanPoint := range networkChans {
		for i, node := range nodes {
			txid, err := dcrlnd.GetChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
			err = node.WaitForNetworkChannelOpen(ctxt, chanPoint)
			if err != nil {
				t.Fatalf("%s(%d): timeout waiting for "+
					"channel(%s) open: %v", nodeNames[i],
					node.NodeID, point, err)
			}
		}
	}

	// Query for routes to pay from Alice to Dave.
	const paymentAmt = 1000
	routesReq := &lnrpc.QueryRoutesRequest{
		PubKey: dave.PubKeyStr,
		Amt:    paymentAmt,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	routesRes, err := net.Alice.QueryRoutes(ctxt, routesReq)
	if err != nil {
		t.Fatalf("unable to get route: %v", err)
	}

	const mAtoms = 1000
	feePerHopMAtoms := computeFee(1000, 1, paymentAmt*mAtoms)

	for i, route := range routesRes.Routes {
		expectedTotalFeesMAtoms :=
			lnwire.MilliAtom(len(route.Hops)-1) * feePerHopMAtoms
		expectedTotalAmtMAtoms := (paymentAmt * mAtoms) + expectedTotalFeesMAtoms

		if route.TotalFees != route.TotalFeesMAtoms/mAtoms {
			t.Fatalf("route %v: total fees %v (mAtoms) does not "+
				"round down to %v (atoms)",
				i, route.TotalFeesMAtoms, route.TotalFees)
		}
		if route.TotalFeesMAtoms != int64(expectedTotalFeesMAtoms) {
			t.Fatalf("route %v: total fees in mAtoms expected %v got %v",
				i, expectedTotalFeesMAtoms, route.TotalFeesMAtoms)
		}

		if route.TotalAmt != route.TotalAmtMAtoms/mAtoms {
			t.Fatalf("route %v: total amt %v (mAtoms) does not "+
				"round down to %v (atoms)",
				i, route.TotalAmtMAtoms, route.TotalAmt)
		}
		if route.TotalAmtMAtoms != int64(expectedTotalAmtMAtoms) {
			t.Fatalf("route %v: total amt in mAtoms expected %v got %v",
				i, expectedTotalAmtMAtoms, route.TotalAmtMAtoms)
		}

		// For all hops except the last, we check that fee equals feePerHop
		// and amount to forward deducts feePerHop on each hop.
		expectedAmtToForwardMAtoms := expectedTotalAmtMAtoms
		for j, hop := range route.Hops[:len(route.Hops)-1] {
			expectedAmtToForwardMAtoms -= feePerHopMAtoms

			if hop.Fee != hop.FeeMAtoms/mAtoms {
				t.Fatalf("route %v hop %v: fee %v (mAtoms) does not "+
					"round down to %v (atoms)",
					i, j, hop.FeeMAtoms, hop.Fee)
			}
			if hop.FeeMAtoms != int64(feePerHopMAtoms) {
				t.Fatalf("route %v hop %v: fee in mAtoms expected %v got %v",
					i, j, feePerHopMAtoms, hop.FeeMAtoms)
			}

			if hop.AmtToForward != hop.AmtToForwardMAtoms/mAtoms {
				t.Fatalf("route %v hop %v: amt to forward %v (mAtoms) does not "+
					"round down to %v (atoms)",
					i, j, hop.AmtToForwardMAtoms, hop.AmtToForward)
			}
			if hop.AmtToForwardMAtoms != int64(expectedAmtToForwardMAtoms) {
				t.Fatalf("route %v hop %v: amt to forward in mAtoms "+
					"expected %v got %v",
					i, j, expectedAmtToForwardMAtoms, hop.AmtToForwardMAtoms)
			}
		}
		// Last hop should have zero fee and amount to forward should equal
		// payment amount.
		hop := route.Hops[len(route.Hops)-1]

		if hop.Fee != 0 || hop.FeeMAtoms != 0 {
			t.Fatalf("route %v hop %v: fee expected 0 got %v (atoms) %v (mAtoms)",
				i, len(route.Hops)-1, hop.Fee, hop.FeeMAtoms)
		}

		if hop.AmtToForward != hop.AmtToForwardMAtoms/mAtoms {
			t.Fatalf("route %v hop %v: amt to forward %v (mAtoms) does not "+
				"round down to %v (atoms)",
				i, len(route.Hops)-1, hop.AmtToForwardMAtoms, hop.AmtToForward)
		}
		if hop.AmtToForwardMAtoms != paymentAmt*mAtoms {
			t.Fatalf("route %v hop %v: amt to forward in mAtoms "+
				"expected %v got %v",
				i, len(route.Hops)-1, paymentAmt*mAtoms, hop.AmtToForwardMAtoms)
		}
	}

	// We clean up the test case by closing channels that were created for
	// the duration of the tests.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Bob, chanPointBob, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarol, false)
}

// testRouteFeeCutoff tests that we are able to prevent querying routes and
// sending payments that incur a fee higher than the fee limit.
func testRouteFeeCutoff(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// For this test, we'll create the following topology:
	//
	//              --- Bob ---
	//            /             \
	// Alice ----                 ---- Dave
	//            \             /
	//              -- Carol --
	//
	// Alice will attempt to send payments to Dave that should not incur a
	// fee greater than the fee limit expressed as a percentage of the
	// amount and as a fixed amount of atoms.
	const chanAmt = dcrutil.Amount(100000)

	// Open a channel between Alice and Bob.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointAliceBob := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Create Carol's node and open a channel between her and Alice with
	// Alice being the funder.
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create carol's node: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, carol, net.Alice); err != nil {
		t.Fatalf("unable to connect carol to alice: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointAliceCarol := openChannelAndAssert(
		ctxt, t, net, net.Alice, carol,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Create Dave's node and open a channel between him and Bob with Bob
	// being the funder.
	dave, err := net.NewNode("Dave", nil)
	if err != nil {
		t.Fatalf("unable to create dave's node: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, dave, net.Bob); err != nil {
		t.Fatalf("unable to connect dave to bob: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointBobDave := openChannelAndAssert(
		ctxt, t, net, net.Bob, dave,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Open a channel between Carol and Dave.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, carol, dave); err != nil {
		t.Fatalf("unable to connect carol to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointCarolDave := openChannelAndAssert(
		ctxt, t, net, carol, dave,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Now that all the channels were set up, we'll wait for all the nodes
	// to have seen all the channels.
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol, dave}
	nodeNames := []string{"alice", "bob", "carol", "dave"}
	networkChans := []*lnrpc.ChannelPoint{
		chanPointAliceBob, chanPointAliceCarol, chanPointBobDave,
		chanPointCarolDave,
	}
	for _, chanPoint := range networkChans {
		for i, node := range nodes {
			txid, err := dcrlnd.GetChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			outpoint := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
			err = node.WaitForNetworkChannelOpen(ctxt, chanPoint)
			if err != nil {
				t.Fatalf("%s(%d) timed out waiting for "+
					"channel(%s) open: %v", nodeNames[i],
					node.NodeID, outpoint, err)
			}
		}
	}

	// The payments should only be successful across the route:
	//	Alice -> Bob -> Dave
	// Therefore, we'll update the fee policy on Carol's side for the
	// channel between her and Dave to invalidate the route:
	//	Alice -> Carol -> Dave
	baseFee := int64(10000)
	feeRate := int64(5)
	timeLockDelta := uint32(dcrlnd.DefaultDecredTimeLockDelta)
	maxHtlc := calculateMaxHtlc(chanAmt)

	expectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMAtoms:      baseFee,
		FeeRateMilliMAtoms: testFeeBase * feeRate,
		TimeLockDelta:      timeLockDelta,
		MinHtlc:            1000, // default value
		MaxHtlcMAtoms:      maxHtlc,
	}

	updateFeeReq := &lnrpc.PolicyUpdateRequest{
		BaseFeeMAtoms: baseFee,
		FeeRate:       float64(feeRate),
		TimeLockDelta: timeLockDelta,
		MaxHtlcMAtoms: maxHtlc,
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: chanPointCarolDave,
		},
	}

	// Create a graph ntfn subscription on Alice to watch for Carol's
	// updates.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	aliceSub := subscribeGraphNotifications(t, ctxt, net.Alice)
	defer close(aliceSub.quit)

	// Update Carol's policy.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if _, err := carol.UpdateChannelPolicy(ctxt, updateFeeReq); err != nil {
		t.Fatalf("unable to update chan policy: %v", err)
	}

	// Wait for Alice to receive the channel update from Carol.
	waitForChannelUpdate(
		t, aliceSub,
		[]expectedChanUpdate{
			{carol.PubKeyStr, expectedPolicy, chanPointCarolDave},
		},
	)

	// We'll also need the channel IDs for Bob's channels in order to
	// confirm the route of the payments.
	listReq := &lnrpc.ListChannelsRequest{}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	listResp, err := net.Bob.ListChannels(ctxt, listReq)
	if err != nil {
		t.Fatalf("unable to retrieve bob's channels: %v", err)
	}

	var aliceBobChanID, bobDaveChanID uint64
	for _, channel := range listResp.Channels {
		switch channel.RemotePubkey {
		case net.Alice.PubKeyStr:
			aliceBobChanID = channel.ChanId
		case dave.PubKeyStr:
			bobDaveChanID = channel.ChanId
		}
	}

	if aliceBobChanID == 0 {
		t.Fatalf("channel between alice and bob not found")
	}
	if bobDaveChanID == 0 {
		t.Fatalf("channel between bob and dave not found")
	}
	hopChanIDs := []uint64{aliceBobChanID, bobDaveChanID}

	// checkRoute is a helper closure to ensure the route contains the
	// correct intermediate hops.
	checkRoute := func(route *lnrpc.Route) {
		if len(route.Hops) != 2 {
			t.Fatalf("expected two hops, got %d", len(route.Hops))
		}

		for i, hop := range route.Hops {
			if hop.ChanId != hopChanIDs[i] {
				t.Fatalf("expected chan id %d, got %d",
					hopChanIDs[i], hop.ChanId)
			}
		}
	}

	// We'll be attempting to send two payments from Alice to Dave. One will
	// have a fee cutoff expressed as a percentage of the amount and the
	// other will have it expressed as a fixed amount of atoms.
	const paymentAmt = 100
	carolFee := computeFee(lnwire.MilliAtom(baseFee), 1, paymentAmt)

	// testFeeCutoff is a helper closure that will ensure the different
	// types of fee limits work as intended when querying routes and sending
	// payments.
	testFeeCutoff := func(feeLimit *lnrpc.FeeLimit) {
		queryRoutesReq := &lnrpc.QueryRoutesRequest{
			PubKey:   dave.PubKeyStr,
			Amt:      paymentAmt,
			FeeLimit: feeLimit,
		}
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		routesResp, err := net.Alice.QueryRoutes(ctxt, queryRoutesReq)
		if err != nil {
			t.Fatalf("unable to get routes: %v", err)
		}

		checkRoute(routesResp.Routes[0])

		invoice := &lnrpc.Invoice{Value: paymentAmt}
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		invoiceResp, err := dave.AddInvoice(ctxt, invoice)
		if err != nil {
			t.Fatalf("unable to create invoice: %v", err)
		}

		sendReq := &routerrpc.SendPaymentRequest{
			PaymentRequest: invoiceResp.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMAtoms: noFeeLimitMAtoms,
		}
		switch limit := feeLimit.Limit.(type) {
		case *lnrpc.FeeLimit_Fixed:
			sendReq.FeeLimitMAtoms = 1000 * limit.Fixed
		case *lnrpc.FeeLimit_Percent:
			sendReq.FeeLimitMAtoms = 1000 * paymentAmt * limit.Percent / 100
		}

		result := sendAndAssertSuccess(t, net.Alice, sendReq)

		checkRoute(result.Htlcs[0].Route)
	}

	// We'll start off using percentages first. Since the fee along the
	// route using Carol as an intermediate hop is 10% of the payment's
	// amount, we'll use a lower percentage in order to invalid that route.
	feeLimitPercent := &lnrpc.FeeLimit{
		Limit: &lnrpc.FeeLimit_Percent{Percent: baseFee/1000 - 1},
	}
	testFeeCutoff(feeLimitPercent)

	// Now we'll test using fixed fee limit amounts. Since we computed the
	// fee for the route using Carol as an intermediate hop earlier, we can
	// use a smaller value in order to invalidate that route.
	feeLimitFixed := &lnrpc.FeeLimit{
		Limit: &lnrpc.FeeLimit_Fixed{Fixed: int64(carolFee.ToAtoms()) - 1},
	}
	testFeeCutoff(feeLimitFixed)

	// Once we're done, close the channels and shut down the nodes created
	// throughout this test.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAliceBob, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAliceCarol, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Bob, chanPointBobDave, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarolDave, false)
}

// testSendUpdateDisableChannel ensures that a channel update with the disable
// flag set is sent once a channel has been either unilaterally or cooperatively
// closed.
func testSendUpdateDisableChannel(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const (
		chanAmt = 100000
	)

	// Open a channel between Alice and Bob and Alice and Carol. These will
	// be closed later on in order to trigger channel update messages
	// marking the channels as disabled.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointAliceBob := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	carol, err := net.NewNode("Carol", []string{
		"--minbackoff=10s",
		"--chan-enable-timeout=1.5s",
		"--chan-disable-timeout=3s",
		"--chan-status-sample-interval=.5s",
	})
	if err != nil {
		t.Fatalf("unable to create carol's node: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect alice to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointAliceCarol := openChannelAndAssert(
		ctxt, t, net, net.Alice, carol,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// We create a new node Eve that has an inactive channel timeout of
	// just 2 seconds (down from the default 20m). It will be used to test
	// channel updates for channels going inactive.
	eve, err := net.NewNode("Eve", []string{
		"--minbackoff=10s",
		"--chan-enable-timeout=1.5s",
		"--chan-disable-timeout=3s",
		"--chan-status-sample-interval=.5s",
	})
	if err != nil {
		t.Fatalf("unable to create eve's node: %v", err)
	}
	defer shutdownAndAssert(net, t, eve)

	// Give Eve some coins.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, eve)
	if err != nil {
		t.Fatalf("unable to send coins to eve: %v", err)
	}

	// Connect Eve to Carol and Bob, and open a channel to carol.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, eve, carol); err != nil {
		t.Fatalf("unable to connect alice to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, eve, net.Bob); err != nil {
		t.Fatalf("unable to connect eve to bob: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointEveCarol := openChannelAndAssert(
		ctxt, t, net, eve, carol,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Wait for bob to have seen this channel so he will relay updates of
	// it to Dave. This prevents a race condition on the gossiper where on
	// rare ocasions the Carol->Eve channel annoucement will arrive after
	// Dave has setup his updateHorizon but with a timestamp in the past
	// (and therefore gets ignored by Dave).
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPointEveCarol)
	if err != nil {
		t.Fatalf("Bob did not see Carol->Eve channel open: %v", err)
	}

	// Launch a node for Dave which will connect to Bob in order to receive
	// graph updates from. This will ensure that the channel updates are
	// propagated throughout the network.
	dave, err := net.NewNode("Dave", nil)
	if err != nil {
		t.Fatalf("unable to create dave's node: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, net.Bob, dave); err != nil {
		t.Fatalf("unable to connect bob to dave: %v", err)
	}

	daveSub := subscribeGraphNotifications(t, ctxb, dave)
	defer close(daveSub.quit)

	// We should expect to see a channel update with the default routing
	// policy, except that it should indicate the channel is disabled.
	expectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMAtoms:      int64(dcrlnd.DefaultDecredBaseFeeMAtoms),
		FeeRateMilliMAtoms: int64(dcrlnd.DefaultDecredFeeRate),
		TimeLockDelta:      dcrlnd.DefaultDecredTimeLockDelta,
		MinHtlc:            1000, // default value
		MaxHtlcMAtoms:      calculateMaxHtlc(chanAmt),
		Disabled:           true,
	}

	// Let Carol go offline. Since Eve has an inactive timeout of 2s, we
	// expect her to send an update disabling the channel.
	restartCarol, err := net.SuspendNode(carol)
	if err != nil {
		t.Fatalf("unable to suspend carol: %v", err)
	}
	waitForChannelUpdate(
		t, daveSub,
		[]expectedChanUpdate{
			{eve.PubKeyStr, expectedPolicy, chanPointEveCarol},
		},
	)

	// We restart Carol. Since the channel now becomes active again, Eve
	// should send a ChannelUpdate setting the channel no longer disabled.
	if err := restartCarol(); err != nil {
		t.Fatalf("unable to restart carol: %v", err)
	}

	expectedPolicy.Disabled = false
	waitForChannelUpdate(
		t, daveSub,
		[]expectedChanUpdate{
			{eve.PubKeyStr, expectedPolicy, chanPointEveCarol},
		},
	)

	// Now we'll test a long disconnection. Disconnect Carol and Eve and
	// ensure they both detect each other as disabled. Their min backoffs
	// are high enough to not interfere with disabling logic.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.DisconnectNodes(ctxt, carol, eve); err != nil {
		t.Fatalf("unable to disconnect Carol from Eve: %v", err)
	}

	// Wait for a disable from both Carol and Eve to come through.
	expectedPolicy.Disabled = true
	waitForChannelUpdate(
		t, daveSub,
		[]expectedChanUpdate{
			{eve.PubKeyStr, expectedPolicy, chanPointEveCarol},
			{carol.PubKeyStr, expectedPolicy, chanPointEveCarol},
		},
	)

	// Reconnect Carol and Eve, this should cause them to reenable the
	// channel from both ends after a short delay.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.EnsureConnected(ctxt, carol, eve); err != nil {
		t.Fatalf("unable to reconnect Carol to Eve: %v", err)
	}

	expectedPolicy.Disabled = false
	waitForChannelUpdate(
		t, daveSub,
		[]expectedChanUpdate{
			{eve.PubKeyStr, expectedPolicy, chanPointEveCarol},
			{carol.PubKeyStr, expectedPolicy, chanPointEveCarol},
		},
	)

	// Now we'll test a short disconnection. Disconnect Carol and Eve, then
	// reconnect them after one second so that their scheduled disables are
	// aborted. One second is twice the status sample interval, so this
	// should allow for the disconnect to be detected, but still leave time
	// to cancel the announcement before the 3 second inactive timeout is
	// hit.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.DisconnectNodes(ctxt, carol, eve); err != nil {
		t.Fatalf("unable to disconnect Carol from Eve: %v", err)
	}
	time.Sleep(time.Second)
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.EnsureConnected(ctxt, eve, carol); err != nil {
		t.Fatalf("unable to reconnect Carol to Eve: %v", err)
	}

	// Since the disable should have been canceled by both Carol and Eve, we
	// expect no channel updates to appear on the network.
	assertNoChannelUpdates(t, daveSub, 4*time.Second)

	// Close Alice's channels with Bob and Carol cooperatively and
	// unilaterally respectively.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	_, _, err = net.CloseChannel(ctxt, net.Alice, chanPointAliceBob, false)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	_, _, err = net.CloseChannel(ctxt, net.Alice, chanPointAliceCarol, true)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}

	// Now that the channel close processes have been started, we should
	// receive an update marking each as disabled.
	expectedPolicy.Disabled = true
	waitForChannelUpdate(
		t, daveSub,
		[]expectedChanUpdate{
			{net.Alice.PubKeyStr, expectedPolicy, chanPointAliceBob},
			{net.Alice.PubKeyStr, expectedPolicy, chanPointAliceCarol},
		},
	)

	// Finally, close the channels by mining the closing transactions.
	mineBlocks(t, net, 1, 2)

	// Also do this check for Eve's channel with Carol.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	_, _, err = net.CloseChannel(ctxt, eve, chanPointEveCarol, false)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}

	waitForChannelUpdate(
		t, daveSub,
		[]expectedChanUpdate{
			{eve.PubKeyStr, expectedPolicy, chanPointEveCarol},
		},
	)
	mineBlocks(t, net, 1, 1)

	// Since we force-closed the Alice->Carol channel, mine enough blocks
	// for the resulting sweep tx to be broadcast and confirmed (taking
	// into account we already mined one block after closing that channel).
	mineBlocks(t, net, defaultCSV-2, 0)
	mineBlocks(t, net, 1, 1)
}

// testAbandonChannel abandones a channel and asserts that it is no
// longer open and not in one of the pending closure states. It also
// verifies that the abandoned channel is reported as closed with close
// type 'abandoned'.
func testAbandonChannel(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// First establish a channel between Alice and Bob.
	channelParam := lntest.OpenChannelParams{
		Amt:     defaultChanAmt,
		PushAmt: dcrutil.Amount(100000),
	}

	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob, channelParam,
	)
	txid, err := dcrlnd.GetChanPointFundingTxid(chanPoint)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	chanPointStr := fmt.Sprintf("%v:%v", txid, chanPoint.OutputIndex)

	// Wait for channel to be confirmed open.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't report channel: %v", err)
	}
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
	}

	// Now that the channel is open, we'll obtain its channel ID real quick
	// so we can use it to query the graph below.
	listReq := &lnrpc.ListChannelsRequest{}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	aliceChannelList, err := net.Alice.ListChannels(ctxt, listReq)
	if err != nil {
		t.Fatalf("unable to fetch alice's channels: %v", err)
	}
	var chanID uint64
	for _, channel := range aliceChannelList.Channels {
		if channel.ChannelPoint == chanPointStr {
			chanID = channel.ChanId
		}
	}

	if chanID == 0 {
		t.Fatalf("unable to find channel")
	}

	// To make sure the channel is removed from the backup file as well
	// when being abandoned, grab a backup snapshot so we can compare it
	// with the later state.
	bkupBefore, err := ioutil.ReadFile(net.Alice.ChanBackupPath())
	if err != nil {
		t.Fatalf("could not get channel backup before abandoning "+
			"channel: %v", err)

	}

	// Send request to abandon channel.
	abandonChannelRequest := &lnrpc.AbandonChannelRequest{
		ChannelPoint: chanPoint,
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = net.Alice.AbandonChannel(ctxt, abandonChannelRequest)
	if err != nil {
		t.Fatalf("unable to abandon channel: %v", err)
	}

	// Assert that channel in no longer open.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	aliceChannelList, err = net.Alice.ListChannels(ctxt, listReq)
	if err != nil {
		t.Fatalf("unable to list channels: %v", err)
	}
	if len(aliceChannelList.Channels) != 0 {
		t.Fatalf("alice should only have no channels open, "+
			"instead she has %v",
			len(aliceChannelList.Channels))
	}

	// Assert that channel is not pending closure.
	pendingReq := &lnrpc.PendingChannelsRequest{}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	alicePendingList, err := net.Alice.PendingChannels(ctxt, pendingReq)
	if err != nil {
		t.Fatalf("unable to list pending channels: %v", err)
	}
	if len(alicePendingList.PendingClosingChannels) != 0 {
		t.Fatalf("alice should only have no pending closing channels, "+
			"instead she has %v",
			len(alicePendingList.PendingClosingChannels))
	}
	if len(alicePendingList.PendingForceClosingChannels) != 0 {
		t.Fatalf("alice should only have no pending force closing "+
			"channels instead she has %v",
			len(alicePendingList.PendingForceClosingChannels))
	}
	if len(alicePendingList.WaitingCloseChannels) != 0 {
		t.Fatalf("alice should only have no waiting close "+
			"channels instead she has %v",
			len(alicePendingList.WaitingCloseChannels))
	}

	// Assert that channel is listed as abandoned.
	closedReq := &lnrpc.ClosedChannelsRequest{
		Abandoned: true,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	aliceClosedList, err := net.Alice.ClosedChannels(ctxt, closedReq)
	if err != nil {
		t.Fatalf("unable to list closed channels: %v", err)
	}
	if len(aliceClosedList.Channels) != 1 {
		t.Fatalf("alice should only have a single abandoned channel, "+
			"instead she has %v",
			len(aliceClosedList.Channels))
	}

	// Ensure that the channel can no longer be found in the channel graph.
	_, err = net.Alice.GetChanInfo(ctxb, &lnrpc.ChanInfoRequest{
		ChanId: chanID,
	})
	if !strings.Contains(err.Error(), "marked as zombie") {
		t.Fatalf("channel shouldn't be found in the channel " +
			"graph!")
	}

	// Make sure the channel is no longer in the channel backup list.
	err = wait.Predicate(func() bool {
		bkupAfter, err := ioutil.ReadFile(net.Alice.ChanBackupPath())
		if err != nil {
			t.Fatalf("could not get channel backup before "+
				"abandoning channel: %v", err)
		}

		return len(bkupAfter) < len(bkupBefore)
	}, defaultTimeout)
	if err != nil {
		t.Fatalf("channel wasn't removed from channel backup file")
	}

	// Calling AbandonChannel again, should result in no new errors, as the
	// channel has already been removed.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = net.Alice.AbandonChannel(ctxt, abandonChannelRequest)
	if err != nil {
		t.Fatalf("unable to abandon channel a second time: %v", err)
	}

	// Now that we're done with the test, the channel can be closed. This is
	// necessary to avoid unexpected outcomes of other tests that use Bob's
	// lnd instance.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Bob, chanPoint, true)

	// Cleanup by mining the force close and sweep transaction.
	cleanupForceClose(t, net, net.Bob, chanPoint)
}

// testSweepAllCoins tests that we're able to properly sweep all coins from the
// wallet into a single target address at the specified fee rate.
func testSweepAllCoins(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// First, we'll make a new node, Carol who'll we'll use to test wallet
	// sweeping.
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	// Next, we'll give Carol exactly 2 utxos of 1 DCR each, both using
	// p2pkh addresses.
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}

	// Ensure that we can't send coins to our own Pubkey.
	info, err := carol.GetInfo(ctxt, &lnrpc.GetInfoRequest{})
	if err != nil {
		t.Fatalf("unable to get node info: %v", err)
	}

	// Create a label that we will used to label the transaction with.
	sendCoinsLabel := "send all coins"

	sweepReq := &lnrpc.SendCoinsRequest{
		Addr:    info.IdentityPubkey,
		SendAll: true,
		Label:   sendCoinsLabel,
	}
	_, err = carol.SendCoins(ctxt, sweepReq)
	if err == nil {
		t.Fatalf("expected SendCoins to users own pubkey to fail")
	}

	// Ensure that we can't send coins to another users Pubkey.
	info, err = net.Alice.GetInfo(ctxt, &lnrpc.GetInfoRequest{})
	if err != nil {
		t.Fatalf("unable to get node info: %v", err)
	}

	sweepReq = &lnrpc.SendCoinsRequest{
		Addr:    info.IdentityPubkey,
		SendAll: true,
		Label:   sendCoinsLabel,
	}
	_, err = carol.SendCoins(ctxt, sweepReq)
	if err == nil {
		t.Fatalf("expected SendCoins to Alices pubkey to fail")
	}

	// With the two coins above mined, we'll now instruct ainz to sweep all
	// the coins to an external address not under its control.
	// We will first attempt to send the coins to addresses that are not
	// compatible with the current network. This is to test that the wallet
	// will prevent any onchain transactions to addresses that are not on
	// the same network as the user.

	// Send coins to a testnet3 address.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	sweepReq = &lnrpc.SendCoinsRequest{
		Addr:    "Tsi6gGYNSMmFwi7JoL5Li39SrERZTTMu6vY",
		SendAll: true,
		Label:   sendCoinsLabel,
	}
	_, err = carol.SendCoins(ctxt, sweepReq)
	if err == nil {
		t.Fatalf("expected SendCoins to different network to fail")
	}

	// Send coins to a mainnet address.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	sweepReq = &lnrpc.SendCoinsRequest{
		Addr:    "DsaAKsMvZ6HrqhmbhLjV9qVbPkkzF5daowT",
		SendAll: true,
		Label:   sendCoinsLabel,
	}
	_, err = carol.SendCoins(ctxt, sweepReq)
	if err == nil {
		t.Fatalf("expected SendCoins to different network to fail")
	}

	// Send coins to a compatible address.
	minerAddr, err := net.Miner.NewAddress()
	if err != nil {
		t.Fatalf("unable to create new miner addr: %v", err)
	}

	sweepReq = &lnrpc.SendCoinsRequest{
		Addr:    minerAddr.String(),
		SendAll: true,
		Label:   sendCoinsLabel,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = carol.SendCoins(ctxt, sweepReq)
	if err != nil {
		t.Fatalf("unable to sweep coins: %v", err)
	}

	// We'll mine a block which should include the sweep transaction we
	// generated above.
	block := mineBlocks(t, net, 1, 1)[0]

	// The sweep transaction should have exactly two inputs as we only had
	// two UTXOs in the wallet.
	sweepTx := block.Transactions[1]
	if len(sweepTx.TxIn) != 2 {
		t.Fatalf("expected 2 inputs instead have %v", len(sweepTx.TxIn))
	}

	sweepTxStr := sweepTx.TxHash().String()
	assertTxLabel(ctxb, t, carol, sweepTxStr, sendCoinsLabel)

	// While we are looking at labels, we test our label transaction command
	// to make sure it is behaving as expected. First, we try to label our
	// transaction with an empty label, and check that we fail as expected.
	sweepHash := sweepTx.TxHash()
	_, err = carol.WalletKitClient.LabelTransaction(
		ctxt, &walletrpc.LabelTransactionRequest{
			Txid:      sweepHash[:],
			Label:     "",
			Overwrite: false,
		},
	)
	if err == nil {
		t.Fatalf("expected error for zero transaction label")
	}

	// Our error will be wrapped in a rpc error, so we check that it
	// contains the error we expect.
	if !strings.Contains(err.Error(), walletrpc.ErrZeroLabel.Error()) {
		t.Fatalf("expected: zero label error, got: %v", err)
	}

	// Next, we try to relabel our transaction without setting the overwrite
	// boolean. We expect this to fail, because the wallet requires setting
	// of this param to prevent accidental overwrite of labels.
	_, err = carol.WalletKitClient.LabelTransaction(
		ctxt, &walletrpc.LabelTransactionRequest{
			Txid:      sweepHash[:],
			Label:     "label that will not work",
			Overwrite: false,
		},
	)
	if err == nil {
		t.Fatalf("expected error for tx already labelled")
	}

	// Our error will be wrapped in a rpc error, so we check that it
	// contains the error we expect.
	if !strings.Contains(err.Error(), "unimplemented") {
		t.Fatalf("expected: label exists, got: %v", err)
	}

	// Finally, we overwrite our label with a new label, which should not
	// fail.
	newLabel := "new sweep tx label"
	_, err = carol.WalletKitClient.LabelTransaction(
		ctxt, &walletrpc.LabelTransactionRequest{
			Txid:      sweepHash[:],
			Label:     newLabel,
			Overwrite: true,
		},
	)
	if err == nil || !strings.Contains(err.Error(), "unimplemented") {
		t.Fatalf("could not label tx: %v", err)
	}

	assertTxLabel(ctxb, t, carol, sweepTxStr, newLabel)

	// Finally, Ainz should now have no coins at all within his wallet.
	balReq := &lnrpc.WalletBalanceRequest{}
	resp, err := carol.WalletBalance(ctxt, balReq)
	if err != nil {
		t.Fatalf("unable to get ainz's balance: %v", err)
	}
	switch {
	case resp.ConfirmedBalance != 0:
		t.Fatalf("expected no confirmed balance, instead have %v",
			resp.ConfirmedBalance)

	case resp.UnconfirmedBalance != 0:
		t.Fatalf("expected no unconfirmed balance, instead have %v",
			resp.UnconfirmedBalance)
	}

	// If we try again, but this time specifying an amount, then the call
	// should fail.
	sweepReq.Amount = 10000
	_, err = carol.SendCoins(ctxt, sweepReq)
	if err == nil {
		t.Fatalf("sweep attempt should fail")
	}
}

// assertTxLabel is a helper function which finds a target tx in our set
// of transactions and checks that it has the desired label.
func assertTxLabel(ctx context.Context, t *harnessTest,
	node *lntest.HarnessNode, targetTx, label string) {

	// List all transactions relevant to our wallet, and find the tx so that
	// we can check the correct label has been set.
	ctxt, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	txResp, err := node.GetTransactions(
		ctxt, &lnrpc.GetTransactionsRequest{},
	)
	if err != nil {
		t.Fatalf("could not get transactions: %v", err)
	}

	// Find our transaction in the set of transactions returned and check
	// its label.
	for _, txn := range txResp.Transactions {
		if txn.TxHash == targetTx {
			if txn.Label != label {
				// This error is ignored in dcrlnd because
				// dcrwallet does not support tx labeling.
				// t.Fatalf("expected label: %v, got: %v",
				//	label, txn.Label)
			}
		}
	}
}

// testHoldInvoicePersistence tests that a sender to a hold-invoice, can be
// restarted before the payment gets settled, and still be able to receive the
// preimage.
func testHoldInvoicePersistence(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const (
		chanAmt     = dcrutil.Amount(1000000)
		numPayments = 10
	)

	// Create carol, and clean up when the test finishes.
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	// Connect Alice to Carol.
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxb, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect alice to carol: %v", err)
	}

	// Open a channel between Alice and Carol.
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointAlice := openChannelAndAssert(
		ctxt, t, net, net.Alice, carol,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Wait for Alice and Carol to receive the channel edge from the
	// funding manager.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPointAlice)
	if err != nil {
		t.Fatalf("alice didn't see the alice->carol channel before "+
			"timeout: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointAlice)
	if err != nil {
		t.Fatalf("carol didn't see the carol->alice channel before "+
			"timeout: %v", err)
	}

	// Create preimages for all payments we are going to initiate.
	var preimages []lntypes.Preimage
	for i := 0; i < numPayments; i++ {
		var preimage lntypes.Preimage
		_, err = rand.Read(preimage[:])
		if err != nil {
			t.Fatalf("unable to generate preimage: %v", err)
		}

		preimages = append(preimages, preimage)
	}

	// Let Carol create hold-invoices for all the payments.
	var (
		payAmt         = dcrutil.Amount(4)
		payReqs        []string
		invoiceStreams []invoicesrpc.Invoices_SubscribeSingleInvoiceClient
	)

	for _, preimage := range preimages {
		payHash := preimage.Hash()
		invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
			Memo:  "testing",
			Value: int64(payAmt),
			Hash:  payHash[:],
		}
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		resp, err := carol.AddHoldInvoice(ctxt, invoiceReq)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		ctx, cancel := context.WithCancel(ctxb)
		defer cancel()

		stream, err := carol.SubscribeSingleInvoice(
			ctx,
			&invoicesrpc.SubscribeSingleInvoiceRequest{
				RHash: payHash[:],
			},
		)
		if err != nil {
			t.Fatalf("unable to subscribe to invoice: %v", err)
		}

		invoiceStreams = append(invoiceStreams, stream)
		payReqs = append(payReqs, resp.PaymentRequest)
	}

	// Wait for all the invoices to reach the OPEN state.
	for _, stream := range invoiceStreams {
		invoice, err := stream.Recv()
		if err != nil {
			t.Fatalf("err: %v", err)
		}

		if invoice.State != lnrpc.Invoice_OPEN {
			t.Fatalf("expected OPEN, got state: %v", invoice.State)
		}
	}

	// Let Alice initiate payments for all the created invoices.
	var paymentStreams []routerrpc.Router_SendPaymentV2Client
	for _, payReq := range payReqs {
		ctx, cancel := context.WithCancel(ctxb)
		defer cancel()

		payStream, err := net.Alice.RouterClient.SendPaymentV2(
			ctx, &routerrpc.SendPaymentRequest{
				PaymentRequest: payReq,
				TimeoutSeconds: 60,
				FeeLimitAtoms:  1000000,
			},
		)
		if err != nil {
			t.Fatalf("unable to send alice htlc: %v", err)
		}

		paymentStreams = append(paymentStreams, payStream)
	}

	// Wait for inlight status update.
	for _, payStream := range paymentStreams {
		payment, err := payStream.Recv()
		if err != nil {
			t.Fatalf("Failed receiving status update: %v", err)
		}

		if payment.Status != lnrpc.Payment_IN_FLIGHT {
			t.Fatalf("state not in flight: %v", payment.Status)
		}
	}

	// The payments should now show up in Alice's ListInvoices, with a zero
	// preimage, indicating they are not yet settled.
	err = wait.NoError(func() error {
		req := &lnrpc.ListPaymentsRequest{
			IncludeIncomplete: true,
		}
		ctxt, _ = context.WithTimeout(ctxt, defaultTimeout)
		paymentsResp, err := net.Alice.ListPayments(ctxt, req)
		if err != nil {
			return fmt.Errorf("error when obtaining payments: %v",
				err)
		}

		// Gather the payment hashes we are looking for in the
		// response.
		payHashes := make(map[string]struct{})
		for _, preimg := range preimages {
			payHashes[preimg.Hash().String()] = struct{}{}
		}

		var zeroPreimg lntypes.Preimage
		for _, payment := range paymentsResp.Payments {
			_, ok := payHashes[payment.PaymentHash]
			if !ok {
				continue
			}

			// The preimage should NEVER be non-zero at this point.
			if payment.PaymentPreimage != zeroPreimg.String() {
				t.Fatalf("expected zero preimage, got %v",
					payment.PaymentPreimage)
			}

			// We wait for the payment attempt to have been
			// properly recorded in the DB.
			if len(payment.Htlcs) == 0 {
				return fmt.Errorf("no attempt recorded")
			}

			delete(payHashes, payment.PaymentHash)
		}

		if len(payHashes) != 0 {
			return fmt.Errorf("payhash not found in response")
		}

		return nil
	}, time.Second*15)
	if err != nil {
		t.Fatalf("predicate not satisfied: %v", err)
	}

	// Wait for all invoices to be accepted.
	for _, stream := range invoiceStreams {
		invoice, err := stream.Recv()
		if err != nil {
			t.Fatalf("err: %v", err)
		}

		if invoice.State != lnrpc.Invoice_ACCEPTED {
			t.Fatalf("expected ACCEPTED, got state: %v",
				invoice.State)
		}
	}

	// Restart alice. This to ensure she will still be able to handle
	// settling the invoices after a restart.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Now after a restart, we must re-track the payments. We set up a
	// goroutine for each to track thir status updates.
	var (
		statusUpdates []chan *lnrpc.Payment
		wg            sync.WaitGroup
		quit          = make(chan struct{})
	)

	defer close(quit)
	for _, preimg := range preimages {
		hash := preimg.Hash()

		ctx, cancel := context.WithCancel(ctxb)
		defer cancel()

		payStream, err := net.Alice.RouterClient.TrackPaymentV2(
			ctx, &routerrpc.TrackPaymentRequest{
				PaymentHash: hash[:],
			},
		)
		if err != nil {
			t.Fatalf("unable to send track payment: %v", err)
		}

		// We set up a channel where we'll forward any status update.
		upd := make(chan *lnrpc.Payment)
		wg.Add(1)
		go func() {
			defer wg.Done()

			for {
				payment, err := payStream.Recv()
				if err != nil {
					close(upd)
					return
				}

				select {
				case upd <- payment:
				case <-quit:
					return
				}
			}
		}()

		statusUpdates = append(statusUpdates, upd)
	}

	// Wait for the in-flight status update.
	for _, upd := range statusUpdates {
		select {
		case payment, ok := <-upd:
			if !ok {
				t.Fatalf("failed getting payment update")
			}

			if payment.Status != lnrpc.Payment_IN_FLIGHT {
				t.Fatalf("state not in in flight: %v",
					payment.Status)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("in flight status not recevied")
		}
	}

	// Settle invoices half the invoices, cancel the rest.
	for i, preimage := range preimages {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		if i%2 == 0 {
			settle := &invoicesrpc.SettleInvoiceMsg{
				Preimage: preimage[:],
			}
			_, err = carol.SettleInvoice(ctxt, settle)
		} else {
			hash := preimage.Hash()
			settle := &invoicesrpc.CancelInvoiceMsg{
				PaymentHash: hash[:],
			}
			_, err = carol.CancelInvoice(ctxt, settle)
		}
		if err != nil {
			t.Fatalf("unable to cancel/settle invoice: %v", err)
		}
	}

	// Make sure we get the expected status update.
	for i, upd := range statusUpdates {
		// Read until the payment is in a terminal state.
		var payment *lnrpc.Payment
		for payment == nil {
			select {
			case p, ok := <-upd:
				if !ok {
					t.Fatalf("failed getting payment update")
				}

				if p.Status == lnrpc.Payment_IN_FLIGHT {
					continue
				}

				payment = p
			case <-time.After(5 * time.Second):
				t.Fatalf("in flight status not recevied")
			}
		}

		// Assert terminal payment state.
		if i%2 == 0 {
			if payment.Status != lnrpc.Payment_SUCCEEDED {
				t.Fatalf("state not suceeded : %v",
					payment.Status)
			}
		} else {
			if payment.FailureReason !=
				lnrpc.PaymentFailureReason_FAILURE_REASON_INCORRECT_PAYMENT_DETAILS {

				t.Fatalf("state not failed: %v",
					payment.FailureReason)
			}
		}
	}

	// Check that Alice's invoices to be shown as settled and failed
	// accordingly, and preimages matching up.
	req := &lnrpc.ListPaymentsRequest{
		IncludeIncomplete: true,
	}
	ctxt, _ = context.WithTimeout(ctxt, defaultTimeout)
	paymentsResp, err := net.Alice.ListPayments(ctxt, req)
	if err != nil {
		t.Fatalf("error when obtaining Alice payments: %v", err)
	}
	for i, preimage := range preimages {
		paymentHash := preimage.Hash()
		var p string
		for _, resp := range paymentsResp.Payments {
			if resp.PaymentHash == paymentHash.String() {
				p = resp.PaymentPreimage
				break
			}
		}
		if p == "" {
			t.Fatalf("payment not found")
		}

		if i%2 == 0 {
			if p != preimage.String() {
				t.Fatalf("preimage doesn't match: %v vs %v",
					p, preimage.String())
			}
		} else {
			if p != lntypes.ZeroHash.String() {
				t.Fatalf("preimage not zero: %v", p)
			}
		}
	}

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointAlice, false)

}

// testExternalFundingChanPoint tests that we're able to carry out a normal
// channel funding workflow given a channel point that was constructed outside
// the main daemon.
func testExternalFundingChanPoint(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// First, we'll create two new nodes that we'll use to open channel
	// between for this test.
	carol, err := net.NewNode("carol", nil)
	require.NoError(t.t, err)
	defer shutdownAndAssert(net, t, carol)

	dave, err := net.NewNode("dave", nil)
	require.NoError(t.t, err)
	defer shutdownAndAssert(net, t, dave)

	// Carol will be funding the channel, so we'll send some coins over to
	// her and ensure they have enough confirmations before we proceed.
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, dcrutil.AtomsPerCoin, carol)
	require.NoError(t.t, err)

	// Before we start the test, we'll ensure both sides are connected to
	// the funding flow can properly be executed.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.EnsureConnected(ctxt, carol, dave)
	require.NoError(t.t, err)

	// At this point, we're ready to simulate our external channel funding
	// flow. To start with, we'll create a pending channel with a shim for
	// a transaction that will never be published.
	const thawHeight uint32 = 10
	const chanSize = defaultChanAmt
	fundingShim1, chanPoint1, _ := deriveFundingShim(
		net, t, carol, dave, chanSize, thawHeight, 1, false,
	)
	_ = openChannelStream(
		ctxb, t, net, carol, dave, lntest.OpenChannelParams{
			Amt:         chanSize,
			FundingShim: fundingShim1,
		},
	)
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	assertNumOpenChannelsPending(ctxt, t, carol, dave, 1)

	// That channel is now pending forever and normally would saturate the
	// max pending channel limit for both nodes. But because the channel is
	// externally funded, we should still be able to open another one. Let's
	// do exactly that now. For this one we publish the transaction so we
	// can mine it later.
	fundingShim2, chanPoint2, _ := deriveFundingShim(
		net, t, carol, dave, chanSize, thawHeight, 2, true,
	)

	// At this point, we'll now carry out the normal basic channel funding
	// test as everything should now proceed as normal (a regular channel
	// funding flow).
	carolChan, daveChan, _, err := basicChannelFundingTest(
		t, net, carol, dave, fundingShim2,
	)
	require.NoError(t.t, err)

	// Both channels should be marked as frozen with the proper thaw
	// height.
	if carolChan.ThawHeight != thawHeight {
		t.Fatalf("expected thaw height of %v, got %v",
			carolChan.ThawHeight, thawHeight)
	}
	if daveChan.ThawHeight != thawHeight {
		t.Fatalf("expected thaw height of %v, got %v",
			daveChan.ThawHeight, thawHeight)
	}

	// Next, to make sure the channel functions as normal, we'll make some
	// payments within the channel.
	payAmt := dcrutil.Amount(100000)
	invoice := &lnrpc.Invoice{
		Memo:  "new chans",
		Value: int64(payAmt),
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	resp, err := dave.AddInvoice(ctxt, invoice)
	require.NoError(t.t, err)
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(
		ctxt, carol, carol.RouterClient, []string{resp.PaymentRequest},
		true,
	)
	require.NoError(t.t, err)

	// Now that the channels are open, and we've confirmed that they're
	// operational, we'll now ensure that the channels are frozen as
	// intended (if requested).
	//
	// First, we'll try to close the channel as Carol, the initiator. This
	// should fail as a frozen channel only allows the responder to
	// initiate a channel close.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	_, _, err = net.CloseChannel(ctxt, carol, chanPoint2, false)
	if err == nil {
		t.Fatalf("carol wasn't denied a co-op close attempt for a " +
			"frozen channel")
	}

	// Next we'll try but this time with Dave (the responder) as the
	// initiator. This time the channel should be closed as normal.
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, dave, chanPoint2, false)

	// As a last step, we check if we still have the pending channel hanging
	// around because we never published the funding TX.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	assertNumOpenChannelsPending(ctxt, t, carol, dave, 1)

	// Let's make sure we can abandon it.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = carol.AbandonChannel(ctxt, &lnrpc.AbandonChannelRequest{
		ChannelPoint:           chanPoint1,
		PendingFundingShimOnly: true,
	})
	require.NoError(t.t, err)
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = dave.AbandonChannel(ctxt, &lnrpc.AbandonChannelRequest{
		ChannelPoint:           chanPoint1,
		PendingFundingShimOnly: true,
	})
	require.NoError(t.t, err)

	// It should now not appear in the pending channels anymore.
	assertNumOpenChannelsPending(ctxt, t, carol, dave, 0)
}

// deriveFundingShim creates a channel funding shim by deriving the necessary
// keys on both sides.
func deriveFundingShim(net *lntest.NetworkHarness, t *harnessTest,
	carol, dave *lntest.HarnessNode, chanSize dcrutil.Amount,
	thawHeight uint32, keyIndex int32, publish bool) (*lnrpc.FundingShim,
	*lnrpc.ChannelPoint, *chainhash.Hash) {

	ctxb := context.Background()
	keyLoc := &signrpc.KeyLocator{
		KeyFamily: 0,
		KeyIndex:  keyIndex,
	}
	carolFundingKey, err := carol.WalletKitClient.DeriveKey(ctxb, keyLoc)
	require.NoError(t.t, err)
	daveFundingKey, err := dave.WalletKitClient.DeriveKey(ctxb, keyLoc)
	require.NoError(t.t, err)

	// Now that we have the multi-sig keys for each party, we can manually
	// construct the funding transaction. We'll instruct the backend to
	// immediately create and broadcast a transaction paying out an exact
	// amount. Normally this would reside in the mempool, but we just
	// confirm it now for simplicity.
	_, fundingOutput, err := input.GenFundingPkScript(
		carolFundingKey.RawKeyBytes, daveFundingKey.RawKeyBytes,
		int64(chanSize),
	)
	require.NoError(t.t, err)

	var txid *chainhash.Hash
	targetOutputs := []*wire.TxOut{fundingOutput}
	if publish {
		txid, err = net.Miner.SendOutputs(
			targetOutputs, 1e4,
		)
		require.NoError(t.t, err)
	} else {
		tx, err := net.Miner.CreateTransaction(targetOutputs, 1e4)
		require.NoError(t.t, err)

		txHash := tx.TxHash()
		txid = &txHash
	}

	// At this point, we can being our external channel funding workflow.
	// We'll start by generating a pending channel ID externally that will
	// be used to track this new funding type.
	var pendingChanID [32]byte
	_, err = rand.Read(pendingChanID[:])
	require.NoError(t.t, err)

	// Now that we have the pending channel ID, Dave (our responder) will
	// register the intent to receive a new channel funding workflow using
	// the pending channel ID.
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: txid[:],
		},
	}
	chanPointShim := &lnrpc.ChanPointShim{
		Amt:       int64(chanSize),
		ChanPoint: chanPoint,
		LocalKey: &lnrpc.KeyDescriptor{
			RawKeyBytes: daveFundingKey.RawKeyBytes,
			KeyLoc: &lnrpc.KeyLocator{
				KeyFamily: daveFundingKey.KeyLoc.KeyFamily,
				KeyIndex:  daveFundingKey.KeyLoc.KeyIndex,
			},
		},
		RemoteKey:     carolFundingKey.RawKeyBytes,
		PendingChanId: pendingChanID[:],
		ThawHeight:    thawHeight,
	}
	fundingShim := &lnrpc.FundingShim{
		Shim: &lnrpc.FundingShim_ChanPointShim{
			ChanPointShim: chanPointShim,
		},
	}
	_, err = dave.FundingStateStep(ctxb, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_ShimRegister{
			ShimRegister: fundingShim,
		},
	})
	require.NoError(t.t, err)

	// If we attempt to register the same shim (has the same pending chan
	// ID), then we should get an error.
	_, err = dave.FundingStateStep(ctxb, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_ShimRegister{
			ShimRegister: fundingShim,
		},
	})
	if err == nil {
		t.Fatalf("duplicate pending channel ID funding shim " +
			"registration should trigger an error")
	}

	// We'll take the chan point shim we just registered for Dave (the
	// responder), and swap the local/remote keys before we feed it in as
	// Carol's funding shim as the initiator.
	fundingShim.GetChanPointShim().LocalKey = &lnrpc.KeyDescriptor{
		RawKeyBytes: carolFundingKey.RawKeyBytes,
		KeyLoc: &lnrpc.KeyLocator{
			KeyFamily: carolFundingKey.KeyLoc.KeyFamily,
			KeyIndex:  carolFundingKey.KeyLoc.KeyIndex,
		},
	}
	fundingShim.GetChanPointShim().RemoteKey = daveFundingKey.RawKeyBytes

	return fundingShim, chanPoint, txid
}

// sendAndAssertSuccess sends the given payment requests and asserts that the
// payment completes successfully.
func sendAndAssertSuccess(t *harnessTest, node *lntest.HarnessNode,
	req *routerrpc.SendPaymentRequest) *lnrpc.Payment {

	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	stream, err := node.RouterClient.SendPaymentV2(ctx, req)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	result, err := getPaymentResult(stream)
	if err != nil {
		t.Fatalf("unable to get payment result: %v", err)
	}

	if result.Status != lnrpc.Payment_SUCCEEDED {
		t.Fatalf("payment failed: %v", result.Status)
	}

	return result
}

// sendAndAssertFailure sends the given payment requests and asserts that the
// payment fails with the expected reason.
func sendAndAssertFailure(t *harnessTest, node *lntest.HarnessNode,
	req *routerrpc.SendPaymentRequest,
	failureReason lnrpc.PaymentFailureReason) *lnrpc.Payment {

	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	stream, err := node.RouterClient.SendPaymentV2(ctx, req)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	result, err := getPaymentResult(stream)
	if err != nil {
		t.Fatalf("unable to get payment result: %v", err)
	}

	if result.Status != lnrpc.Payment_FAILED {
		t.Fatalf("payment was expected to fail, but succeeded")
	}

	if result.FailureReason != failureReason {
		t.Fatalf("payment should have been rejected due to "+
			"%v, but got %v", failureReason, result.Status)
	}

	return result
}

// getPaymentResult reads a final result from the stream and returns it.
func getPaymentResult(stream routerrpc.Router_SendPaymentV2Client) (
	*lnrpc.Payment, error) {

	for {
		payment, err := stream.Recv()
		if err != nil {
			return nil, err
		}

		if payment.Status != lnrpc.Payment_IN_FLIGHT {
			return payment, nil
		}
	}
}

type testCase struct {
	name string
	test func(net *lntest.NetworkHarness, t *harnessTest)
}

var testsCases = []*testCase{
	// This test needs to happen before SVH, so that we don't have to run a
	// second voter on the second chain to observe the reorg.
	{
		name: "open channel reorg test",
		test: testOpenChannelAfterReorg,
	},
	// TODO(decred) review this test again.
	// {
	// 	name: "onchain fund recovery",
	// 	test: testOnchainFundRecovery,
	// },
	{
		name: "test concurrent node connection",
		test: testConcurrentNodeConnection,
	},
	{
		name: "sweep coins",
		test: testSweepAllCoins,
	},
	{
		name: "recovery info",
		test: testGetRecoveryInfo,
	},
	{
		name: "basic funding flow",
		test: testBasicChannelFunding,
	},
	{
		name: "unconfirmed channel funding",
		test: testUnconfirmedChannelFunding,
	},
	{
		name: "update channel policy",
		test: testUpdateChannelPolicy,
	},
	{
		name: "send update disable channel",
		test: testSendUpdateDisableChannel,
	},
	{
		name: "invoice routing hints",
		test: testInvoiceRoutingHints,
	},
	{
		name: "disconnecting target peer",
		test: testDisconnectingTargetPeer,
	},
	{
		name: "graph topology notifications",
		test: testGraphTopologyNotifications,
	},
	{
		name: "funding flow persistence",
		test: testChannelFundingPersistence,
	},
	{
		name: "channel force closure",
		test: testChannelForceClosure,
	},
	{
		name: "channel balance",
		test: testChannelBalance,
	},
	{
		name: "channel unsettled balance",
		test: testChannelUnsettledBalance,
	},
	{
		name: "single hop invoice",
		test: testSingleHopInvoice,
	},
	// TODO(decred) Re-enable after fixing.
	//{
	//	name: "offline hop invoice",
	//	test: testOfflineHopInvoice,
	//},
	{
		name: "sphinx replay persistence",
		test: testSphinxReplayPersistence,
	},
	{
		name: "list channels",
		test: testListChannels,
	},
	{
		name: "list outgoing payments",
		test: testListPayments,
	},
	{
		name: "max pending channel",
		test: testMaxPendingChannels,
	},
	{
		name: "multi-hop payments",
		test: testMultiHopPayments,
	},
	{
		name: "single-hop send to route",
		test: testSingleHopSendToRoute,
	},
	{
		name: "multi-hop send to route",
		test: testMultiHopSendToRoute,
	},
	{
		name: "send to route error propagation",
		test: testSendToRouteErrorPropagation,
	},
	{
		name: "unannounced channels",
		test: testUnannouncedChannels,
	},
	{
		name: "private channels",
		test: testPrivateChannels,
	},
	{
		name: "multi-hop payments over private channels",
		test: testMultiHopOverPrivateChannels,
	},
	{
		name: "multiple channel creation and update subscription",
		test: testBasicChannelCreationAndUpdates,
	},
	{
		name: "invoice update subscription",
		test: testInvoiceSubscriptions,
	},
	{
		name: "multi-hop htlc error propagation",
		test: testHtlcErrorPropagation,
	},
	{
		name: "reject onward htlc",
		test: testRejectHTLC,
	},
	// TODO(roasbeef): multi-path integration test
	{
		name: "node announcement",
		test: testNodeAnnouncement,
	},
	{
		name: "node sign verify",
		test: testNodeSignVerify,
	},
	{
		name: "async payments benchmark",
		test: testAsyncPayments,
	},
	{
		name: "async bidirectional payments",
		test: testBidirectionalAsyncPayments,
	},
	{
		name: "test multi-hop htlc",
		test: testMultiHopHtlcClaims,
	},
	{
		name: "switch circuit persistence",
		test: testSwitchCircuitPersistence,
	},
	{
		name: "switch offline delivery",
		test: testSwitchOfflineDelivery,
	},
	{
		name: "switch offline delivery persistence",
		test: testSwitchOfflineDeliveryPersistence,
	},
	{
		name: "switch offline delivery outgoing offline",
		test: testSwitchOfflineDeliveryOutgoingOffline,
	},
	{
		// TODO(roasbeef): test always needs to be last as Bob's state
		// is borked since we trick him into attempting to cheat Alice?
		name: "revoked uncooperative close retribution",
		test: testRevokedCloseRetribution,
	},
	{
		name: "failing link",
		test: testFailingChannel,
	},
	{
		name: "garbage collect link nodes",
		test: testGarbageCollectLinkNodes,
	},
	{
		name: "abandonchannel",
		test: testAbandonChannel,
	},
	{
		name: "revoked uncooperative close retribution zero value remote output",
		test: testRevokedCloseRetributionZeroValueRemoteOutput,
	},
	{
		name: "revoked uncooperative close retribution remote hodl",
		test: testRevokedCloseRetributionRemoteHodl,
	},
	{
		name: "revoked uncooperative close retribution remote hodl second level",
		test: testRevokedCloseRetributionRemoteHodlSecondLevel,
	},
	{
		name: "revoked uncooperative close retribution altruist watchtower",
		test: testRevokedCloseRetributionAltruistWatchtower,
	},
	{
		name: "data loss protection",
		test: testDataLossProtection,
	},
	{
		name: "query routes",
		test: testQueryRoutes,
	},
	{
		name: "route fee cutoff",
		test: testRouteFeeCutoff,
	},
	{
		name: "add invoice max inbound amount",
		test: testAddInvoiceMaxInboundAmt,
	},
	{
		name: "add and receive invoice at max inbound amount",
		test: testAddReceiveInvoiceMaxInboundAmt,
	},
	{
		name: "send payment max outbound amt",
		test: testSendPaymentMaxOutboundAmt,
	},
	{
		name: "max io channel balance",
		test: testMaxIOChannelBalances,
	},
	{
		name: "streaming channel backup update",
		test: testChannelBackupUpdates,
	},
	{
		name: "export channel backup",
		test: testExportChannelBackup,
	},
	{
		name: "channel backup restore",
		test: testChannelBackupRestore,
	},
	{
		name: "hold invoice sender persistence",
		test: testHoldInvoicePersistence,
	},
	{
		name: "cpfp",
		test: testCPFP,
	},
	{
		name: "macaroon authentication",
		test: testMacaroonAuthentication,
	},
	{
		name: "bake macaroon",
		test: testBakeMacaroon,
	},
	{
		name: "delete macaroon id",
		test: testDeleteMacaroonID,
	},
	{
		name: "immediate payment after channel opened",
		test: testPaymentFollowingChannelOpen,
	},
	{
		name: "external channel funding",
		test: testExternalFundingChanPoint,
	},
	{
		name: "psbt channel funding",
		test: testPsbtChanFunding,
	},
	{
		name: "sendtoroute multi path payment",
		test: testSendToRouteMultiPath,
	},
	{
		name: "send multi path payment",
		test: testSendMultiPathPayment,
	},
	{
		name: "REST API",
		test: testRestApi,
	},
	{
		name: "intercept forwarded htlc packets",
		test: testForwardInterceptor,
	},
	{
		name: "wumbo channels",
		test: testWumboChannels,
	},
	{
		name: "maximum channel size",
		test: testMaxChannelSize,
	},
}

// TestLightningNetworkDaemon performs a series of integration tests amongst a
// programmatically driven network of lnd nodes.
func TestLightningNetworkDaemon(t *testing.T) {
	ht := newHarnessTest(t, nil)

	// Declare the network harness here to gain access to its
	// 'OnTxAccepted' call back.
	var lndHarness *lntest.NetworkHarness

	// Create an instance of dcrd's rpctest.Harness that will act as the
	// miner for all tests. This will be used to fund the wallets of the
	// nodes within the test network and to drive blockchain related events
	// within the network. Revert the default setting of accepting
	// non-standard transactions on simnet to reject them. Transactions on
	// the lightning network should always be standard to get better
	// guarantees of getting included in to blocks.
	//
	// We will also connect it to our chain backend.
	minerLogDir := "./.minerlogs"
	args := []string{
		"--rejectnonstd",
		"--txindex",
		"--debuglevel=debug",
		"--logdir=" + minerLogDir,
		"--maxorphantx=0",
		"--nobanning",
		// "--connect=" + chainBackend.P2PAddr(),
	}
	handlers := &rpcclient.NotificationHandlers{
		OnTxAccepted: func(hash *chainhash.Hash, amt dcrutilv3.Amount) {
			lndHarness.OnTxAccepted(hash)
		},
	}
	miner, err := testutils.NewSetupRPCTest(t, 5, harnessNetParams, handlers,
		args, false, 0)
	if err != nil {
		ht.Fatalf("unable to create mining node: %v", err)
	}
	defer func() {
		miner.TearDown()

		// After shutting down the miner, we'll make a copy of the log
		// file before deleting the temporary log dir.
		logFile := fmt.Sprintf(
			"%s/%s/dcrd.log", minerLogDir, harnessNetParams.Name,
		)
		err := lntest.CopyFile("./output_dcrd_miner.log", logFile)
		if err != nil {
			fmt.Printf("unable to copy file: %v\n", err)
		}
		if err = os.RemoveAll(minerLogDir); err != nil {
			fmt.Printf("Cannot remove dir %s: %v\n",
				minerLogDir, err)
		}
	}()

	if err := miner.Node.NotifyNewTransactions(context.Background(), false); err != nil {
		ht.Fatalf("unable to request transaction notifications: %v", err)
	}

	// Start a chain backend.
	chainBackend, cleanUp, err := lntest.NewBackend(t, miner)
	if err != nil {
		ht.Fatalf("unable to start dcrd: %v", err)
	}
	defer cleanUp()

	binary := itestLndBinary
	if runtime.GOOS == "windows" {
		// Windows (even in a bash like environment like git bash as on
		// Travis) doesn't seem to like relative paths to exe files...
		currentDir, err := os.Getwd()
		if err != nil {
			ht.Fatalf("unable to get working directory: %v", err)
		}
		targetPath := filepath.Join(currentDir, itestLndBinary)
		binary, err = filepath.Abs(targetPath)
		if err != nil {
			ht.Fatalf("unable to get absolute path: %v", err)
		}
	}

	// Now we can set up our test harness (LND instance), with the chain
	// backend we just created.
	lndHarness, err = lntest.NewNetworkHarness(miner, chainBackend, binary)
	if err != nil {
		ht.Fatalf("unable to create lightning network harness: %v", err)
	}
	defer lndHarness.TearDownAll()

	// Spawn a new goroutine to watch for any fatal errors that any of the
	// running lnd processes encounter. If an error occurs, then the test
	// case should naturally as a result and we log the server error here to
	// help debug.
	go func() {
		for err := range lndHarness.ProcessErrors() {
			ht.Logf("lnd finished with error (stderr):\n%v", err)
		}
	}()

	// With the dcrd harness created, we can now complete the
	// initialization of the network. args - list of lnd arguments,
	// example: "--debuglevel=debug"
	// TODO(roasbeef): create master balanced channel with all the monies?
	aliceBobArgs := []string{
		"--default-remote-max-htlcs=150",
	}
	if err = lndHarness.SetUp(aliceBobArgs); err != nil {
		ht.Fatalf("unable to set up test lightning network: %v", err)
	}

	t.Logf("Running %v integration tests", len(testsCases))
	for i, testCase := range testsCases {
		logLine := fmt.Sprintf("STARTING %d ============ %v ============\n",
			i, testCase.name)

		err := lndHarness.EnsureConnected(
			context.Background(), lndHarness.Alice, lndHarness.Bob,
		)
		if err != nil {
			t.Fatalf("unable to connect alice to bob: %v", err)
		}

		if err := lndHarness.Alice.AddToLog(logLine); err != nil {
			t.Fatalf("unable to add to log: %v", err)
		}
		if err := lndHarness.Bob.AddToLog(logLine); err != nil {
			t.Fatalf("unable to add to log: %v", err)
		}

		success := t.Run(testCase.name, func(t1 *testing.T) {
			ht := newHarnessTest(t1, lndHarness)
			ht.RunTestCase(testCase)
		})

		// Stop at the first failure. Mimic behavior of original test
		// framework.
		if !success {
			// Log failure time to help relate the lnd logs to the
			// failure.
			t.Logf("Failure time: %v",
				time.Now().Format("2006-01-02 15:04:05.000"))
			break
		}
	}
}
