package chainview

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/decred/dcrd/chaincfg/chainhash"
	jsontypes "github.com/decred/dcrd/rpc/jsonrpc/types/v3"
	"github.com/decred/dcrd/rpcclient/v7"
	"github.com/decred/dcrd/wire"
	"github.com/decred/dcrlnd/channeldb"
)

// DcrdFilteredChainView is an implementation of the FilteredChainView
// interface which is backed by an active websockets connection to dcrd.
type DcrdFilteredChainView struct {
	started int32 // To be used atomically.
	stopped int32 // To be used atomically.

	// bestHeight is the height of the latest block added to the
	// blockQueue from the onFilteredConnectedMethod. It is used to
	// determine up to what height we would need to rescan in case
	// of a filter update.
	bestHeightMtx sync.Mutex
	bestHeight    int64

	dcrdConn *rpcclient.Client

	// blockEventQueue is the ordered queue used to keep the order
	// of connected and disconnected blocks sent to the reader of the
	// chainView.
	blockQueue *blockEventQueue

	// filterUpdates is a channel in which updates to the utxo filter
	// attached to this instance are sent over.
	filterUpdates chan filterUpdate

	// chainFilter is the set of utox's that we're currently watching
	// spends for within the chain.
	filterMtx   sync.RWMutex
	chainFilter map[wire.OutPoint]struct{}

	// filterBlockReqs is a channel in which requests to filter select
	// blocks will be sent over.
	filterBlockReqs chan *filterBlockReq

	quit chan struct{}
	wg   sync.WaitGroup
}

// A compile time check to ensure DcrdFilteredChainView implements the
// chainview.FilteredChainView.
var _ FilteredChainView = (*DcrdFilteredChainView)(nil)

// NewDcrdFilteredChainView creates a new instance of a FilteredChainView from
// RPC credentials for an active dcrd instance.
func NewDcrdFilteredChainView(config rpcclient.ConnConfig) (*DcrdFilteredChainView, error) {
	chainView := &DcrdFilteredChainView{
		chainFilter:     make(map[wire.OutPoint]struct{}),
		filterUpdates:   make(chan filterUpdate),
		filterBlockReqs: make(chan *filterBlockReq),
		quit:            make(chan struct{}),
	}

	ntfnCallbacks := &rpcclient.NotificationHandlers{
		OnBlockConnected:    chainView.onBlockConnected,
		OnBlockDisconnected: chainView.onBlockDisconnected,
	}

	// Disable connecting to dcrd within the rpcclient.New method. We
	// defer establishing the connection to our .Start() method.
	config.DisableConnectOnNew = true
	config.DisableAutoReconnect = false
	chainConn, err := rpcclient.New(&config, ntfnCallbacks)
	if err != nil {
		return nil, err
	}
	chainView.dcrdConn = chainConn

	chainView.blockQueue = newBlockEventQueue()

	return chainView, nil
}

// Start starts all goroutines necessary for normal operation.
//
// NOTE: This is part of the FilteredChainView interface.
func (b *DcrdFilteredChainView) Start() error {
	// Already started?
	if atomic.AddInt32(&b.started, 1) != 1 {
		return nil
	}

	log.Infof("FilteredChainView starting")

	// Connect to dcrd, and register for notifications on connected, and
	// disconnected blocks.
	if err := b.dcrdConn.Connect(context.Background(), true); err != nil {
		return err
	}
	if err := b.dcrdConn.NotifyBlocks(context.TODO()); err != nil {
		return err
	}

	_, bestHeight, err := b.dcrdConn.GetBestBlock(context.TODO())
	if err != nil {
		return err
	}

	b.bestHeightMtx.Lock()
	b.bestHeight = bestHeight
	b.bestHeightMtx.Unlock()

	b.blockQueue.Start()

	b.wg.Add(1)
	go b.chainFilterer()

	return nil
}

// Stop stops all goroutines which we launched by the prior call to the Start
// method.
//
// NOTE: This is part of the FilteredChainView interface.
func (b *DcrdFilteredChainView) Stop() error {
	// Already shutting down?
	if atomic.AddInt32(&b.stopped, 1) != 1 {
		return nil
	}

	// Shutdown the rpc client, this gracefully disconnects from dcrd, and
	// cleans up all related resources.
	b.dcrdConn.Shutdown()

	b.blockQueue.Stop()

	log.Infof("FilteredChainView stopping")

	close(b.quit)
	b.wg.Wait()

	return nil
}

// onBlockConnected is called for each block that's connected to the end of the
// main chain. Based on our current chain filter, the block may or may not
// include any relevant transactions.
func (b *DcrdFilteredChainView) onBlockConnected(blockHeader []byte, txns [][]byte) {
	var header wire.BlockHeader
	if err := header.FromBytes(blockHeader); err != nil {
		log.Warnf("Received block connected with malformed header: %v", err)
		return
	}

	mtxs := make([]*wire.MsgTx, len(txns))
	b.filterMtx.Lock()
	for i, txBytes := range txns {
		var mtx wire.MsgTx
		if err := mtx.FromBytes(txBytes); err != nil {
			log.Warnf("Received block connected with malformed tx: %v", err)
			return
		}
		mtxs[i] = &mtx
	}

	for _, mtx := range mtxs {
		for _, txIn := range mtx.TxIn {
			// We can delete this outpoint from the chainFilter, as
			// we just received a block where it was spent. In case
			// of a reorg, this outpoint might get "un-spent", but
			// that's okay since it would never be wise to consider
			// the channel open again (since a spending transaction
			// exists on the network).
			delete(b.chainFilter, txIn.PreviousOutPoint)
		}
	}
	b.filterMtx.Unlock()

	// We record the height of the last connected block added to the
	// blockQueue such that we can scan up to this height in case of
	// a rescan. It must be protected by a mutex since a filter update
	// might be trying to read it concurrently.
	b.bestHeightMtx.Lock()
	b.bestHeight = int64(header.Height)
	b.bestHeightMtx.Unlock()

	block := &FilteredBlock{
		Hash:         header.BlockHash(),
		Height:       int64(header.Height),
		Transactions: mtxs,
	}

	b.blockQueue.Add(&blockEvent{
		eventType: connected,
		block:     block,
	})

}

// onBlockDisconnected is a callback which is executed once a block is
// disconnected from the end of the main chain.
func (b *DcrdFilteredChainView) onBlockDisconnected(blockHeader []byte) {
	var header wire.BlockHeader
	if err := header.FromBytes(blockHeader); err != nil {
		log.Warnf("Received block disconnected with malformed header: %v", err)
		return
	}

	log.Debugf("got disconnected block at height %d: %v", header.Height,
		header.BlockHash())

	filteredBlock := &FilteredBlock{
		Hash:   header.BlockHash(),
		Height: int64(header.Height),
	}

	b.blockQueue.Add(&blockEvent{
		eventType: disconnected,
		block:     filteredBlock,
	})
}

// filterBlockReq houses a request to manually filter a block specified by
// block hash.
type filterBlockReq struct {
	blockHash *chainhash.Hash
	resp      chan *FilteredBlock
	err       chan error
}

// FilterBlock takes a block hash, and returns a FilteredBlocks which is the
// result of applying the current registered UTXO sub-set on the block
// corresponding to that block hash. If any watched UTOX's are spent by the
// selected lock, then the internal chainFilter will also be updated.
//
// NOTE: This is part of the FilteredChainView interface.
func (b *DcrdFilteredChainView) FilterBlock(blockHash *chainhash.Hash) (*FilteredBlock, error) {
	req := &filterBlockReq{
		blockHash: blockHash,
		resp:      make(chan *FilteredBlock, 1),
		err:       make(chan error, 1),
	}

	select {
	case b.filterBlockReqs <- req:
	case <-b.quit:
		return nil, fmt.Errorf("FilteredChainView shutting down")
	}

	return <-req.resp, <-req.err
}

// chainFilterer is the primary goroutine which: listens for new blocks coming
// and dispatches the relevant FilteredBlock notifications, updates the filter
// due to requests by callers, and finally is able to preform targeted block
// filtration.
//
// TODO(roasbeef): change to use loadfilter RPC's
func (b *DcrdFilteredChainView) chainFilterer() {
	defer b.wg.Done()

	// filterBlock is a helper function that scans the given block, and
	// notes which transactions spend outputs which are currently being
	// watched. Additionally, the chain filter will also be updated by
	// removing any spent outputs.
	filterBlock := func(blk *wire.MsgBlock) []*wire.MsgTx {
		b.filterMtx.Lock()
		defer b.filterMtx.Unlock()

		var filteredTxns []*wire.MsgTx
		for _, tx := range blk.Transactions {
			var txAlreadyFiltered bool
			for _, txIn := range tx.TxIn {
				prevOp := txIn.PreviousOutPoint
				if _, ok := b.chainFilter[prevOp]; !ok {
					continue
				}

				delete(b.chainFilter, prevOp)

				// Only add this txn to our list of filtered
				// txns if it is the first previous outpoint to
				// cause a match.
				if txAlreadyFiltered {
					continue
				}

				filteredTxns = append(filteredTxns, tx)
				txAlreadyFiltered = true

			}
		}

		return filteredTxns
	}

	decodeJSONBlock := func(block *jsontypes.RescannedBlock,
		height int64) (*FilteredBlock, error) {
		hash, err := chainhash.NewHashFromStr(block.Hash)
		if err != nil {
			return nil, err
		}
		txs := make([]*wire.MsgTx, 0, len(block.Transactions))
		for _, str := range block.Transactions {
			b, err := hex.DecodeString(str)
			if err != nil {
				return nil, err
			}
			tx := &wire.MsgTx{}
			err = tx.Deserialize(bytes.NewReader(b))
			if err != nil {
				return nil, err
			}
			txs = append(txs, tx)
		}
		return &FilteredBlock{
			Hash:         *hash,
			Height:       height,
			Transactions: txs,
		}, nil
	}

	for {
		select {
		// The caller has just sent an update to the current chain
		// filter, so we'll apply the update, possibly rewinding our
		// state partially.
		case update := <-b.filterUpdates:

			// First, we'll add all the new UTXO's to the set of
			// watched UTXO's, eliminating any duplicates in the
			// process.
			log.Tracef("Updating chain filter with new UTXO's: %v",
				update.newUtxos)

			b.filterMtx.Lock()
			for _, newOp := range update.newUtxos {
				b.chainFilter[newOp] = struct{}{}
			}
			b.filterMtx.Unlock()

			// Apply the new TX filter to dcrd, which will cause
			// all following notifications from and calls to it
			// return blocks filtered with the new filter.
			b.dcrdConn.LoadTxFilter(context.TODO(), false, nil, update.newUtxos)

			// All blocks gotten after we loaded the filter will
			// have the filter applied, but we will need to rescan
			// the blocks up to the height of the block we last
			// added to the blockQueue.
			b.bestHeightMtx.Lock()
			bestHeight := b.bestHeight
			b.bestHeightMtx.Unlock()

			// If the update height matches our best known height,
			// then we don't need to do any rewinding.
			if update.updateHeight == bestHeight {
				continue
			}

			// Otherwise, we'll rewind the state to ensure the
			// caller doesn't miss any relevant notifications.
			// Starting from the height _after_ the update height,
			// we'll walk forwards, rescanning one block at a time
			// with dcrd applying the newly loaded filter to each
			// block.
			for i := update.updateHeight + 1; i < bestHeight+1; i++ {
				blockHash, err := b.dcrdConn.GetBlockHash(context.TODO(), i)
				if err != nil {
					log.Warnf("Unable to get block hash "+
						"for block at height %d: %v",
						i, err)
					continue
				}

				// To avoid dealing with the case where a reorg
				// is happening while we rescan, we scan one
				// block at a time, skipping blocks that might
				// have gone missing.
				rescanned, err := b.dcrdConn.Rescan(
					context.TODO(), []chainhash.Hash{*blockHash})
				if err != nil {
					log.Warnf("Unable to rescan block "+
						"with hash %v at height %d: %v",
						blockHash, i, err)
					continue
				}

				// If no block was returned from the rescan, it
				// means no matching transactions were found.
				if len(rescanned.DiscoveredData) != 1 {
					log.Tracef("rescan of block %v at "+
						"height=%d yielded no "+
						"transactions", blockHash, i)
					continue
				}
				decoded, err := decodeJSONBlock(
					&rescanned.DiscoveredData[0], i)
				if err != nil {
					log.Errorf("Unable to decode block: %v",
						err)
					continue
				}
				b.blockQueue.Add(&blockEvent{
					eventType: connected,
					block:     decoded,
				})
			}

		// We've received a new request to manually filter a block.
		case req := <-b.filterBlockReqs:
			// First we'll fetch the block itself as well as some
			// additional information including its height.
			block, err := b.dcrdConn.GetBlock(context.TODO(), req.blockHash)
			if err != nil {
				req.err <- err
				req.resp <- nil
				continue
			}
			header, err := b.dcrdConn.GetBlockHeaderVerbose(context.TODO(), req.blockHash)
			if err != nil {
				req.err <- err
				req.resp <- nil
				continue
			}

			// Once we have this info, we can directly filter the
			// block and dispatch the proper notification.
			req.resp <- &FilteredBlock{
				Hash:         *req.blockHash,
				Height:       int64(header.Height),
				Transactions: filterBlock(block),
			}
			req.err <- err

		case <-b.quit:
			return
		}
	}
}

// filterUpdate is a message sent to the chainFilterer to update the current
// chainFilter state.
type filterUpdate struct {
	newUtxos     []wire.OutPoint
	updateHeight int64
}

// UpdateFilter updates the UTXO filter which is to be consulted when creating
// FilteredBlocks to be sent to subscribed clients. This method is cumulative
// meaning repeated calls to this method should _expand_ the size of the UTXO
// sub-set currently being watched.  If the set updateHeight is _lower_ than
// the best known height of the implementation, then the state should be
// rewound to ensure all relevant notifications are dispatched.
//
// NOTE: This is part of the FilteredChainView interface.
func (b *DcrdFilteredChainView) UpdateFilter(ops []channeldb.EdgePoint,
	updateHeight int64) error {

	newUtxos := make([]wire.OutPoint, len(ops))
	for i, op := range ops {
		newUtxos[i] = op.OutPoint
	}

	select {

	case b.filterUpdates <- filterUpdate{
		newUtxos:     newUtxos,
		updateHeight: updateHeight,
	}:
		return nil

	case <-b.quit:
		return fmt.Errorf("chain filter shutting down")
	}
}

// FilteredBlocks returns the channel that filtered blocks are to be sent over.
// Each time a block is connected to the end of a main chain, and appropriate
// FilteredBlock which contains the transactions which mutate our watched UTXO
// set is to be returned.
//
// NOTE: This is part of the FilteredChainView interface.
func (b *DcrdFilteredChainView) FilteredBlocks() <-chan *FilteredBlock {
	return b.blockQueue.newBlocks
}

// DisconnectedBlocks returns a receive only channel which will be sent upon
// with the empty filtered blocks of blocks which are disconnected from the
// main chain in the case of a re-org.
//
// NOTE: This is part of the FilteredChainView interface.
func (b *DcrdFilteredChainView) DisconnectedBlocks() <-chan *FilteredBlock {
	return b.blockQueue.staleBlocks
}
