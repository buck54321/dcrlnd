package lnwallet

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"io"
	"io/ioutil"
	prand "math/rand"
	"net"
	"os"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrec/secp256k1/v3"
	"github.com/decred/dcrd/dcrutil/v4"
	"github.com/decred/dcrd/wire"
	"github.com/decred/dcrlnd/channeldb"
	"github.com/decred/dcrlnd/input"
	"github.com/decred/dcrlnd/keychain"
	"github.com/decred/dcrlnd/lnwallet/chainfee"
	"github.com/decred/dcrlnd/lnwire"
	"github.com/decred/dcrlnd/shachain"
)

var (
	// For simplicity a single priv key controls all of our test outputs.
	testWalletPrivKey = []byte{
		0x2b, 0xd8, 0x06, 0xc9, 0x7f, 0x0e, 0x00, 0xaf,
		0x1a, 0x1f, 0xc3, 0x32, 0x8f, 0xa7, 0x63, 0xa9,
		0x26, 0x97, 0x23, 0xc8, 0xdb, 0x8f, 0xac, 0x4f,
		0x93, 0xaf, 0x71, 0xdb, 0x18, 0x6d, 0x6e, 0x90,
	}

	// We're alice :)
	bobsPrivKey = []byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x63, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x95, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xfd, 0x9e, 0xc5, 0x8c, 0xe9,
	}

	// Use a hard-coded HD seed.
	testHdSeed = chainhash.Hash{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
		0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}

	alicePkScript = []byte{
		0x76, 0xa9, 0x14, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xac}

	bobPkScript = []byte{
		0x76, 0xa9, 0x14, 0x11, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xac}

	// A serializable txn for testing funding txn.
	testTx = &wire.MsgTx{
		Version: 1,
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: wire.OutPoint{
					Hash:  chainhash.Hash{},
					Index: 0xffffffff,
				},
				SignatureScript: []byte{0x04, 0x31, 0xdc, 0x00, 0x1b, 0x01, 0x62},
				Sequence:        0xffffffff,
			},
		},
		TxOut: []*wire.TxOut{
			{
				Value: 5000000000,
				PkScript: []byte{
					0x41, // OP_DATA_65
					0x04, 0xd6, 0x4b, 0xdf, 0xd0, 0x9e, 0xb1, 0xc5,
					0xfe, 0x29, 0x5a, 0xbd, 0xeb, 0x1d, 0xca, 0x42,
					0x81, 0xbe, 0x98, 0x8e, 0x2d, 0xa0, 0xb6, 0xc1,
					0xc6, 0xa5, 0x9d, 0xc2, 0x26, 0xc2, 0x86, 0x24,
					0xe1, 0x81, 0x75, 0xe8, 0x51, 0xc9, 0x6b, 0x97,
					0x3d, 0x81, 0xb0, 0x1c, 0xc3, 0x1f, 0x04, 0x78,
					0x34, 0xbc, 0x06, 0xd6, 0xd6, 0xed, 0xf6, 0x20,
					0xd1, 0x84, 0x24, 0x1a, 0x6a, 0xed, 0x8b, 0x63,
					0xa6, // 65-byte signature
					0xac, // OP_CHECKSIG
				},
			},
		},
		LockTime: 5,
	}

	// A valid, DER-encoded signature (taken from secp256k1 unit tests).
	testSigBytes = []byte{
		0x30, 0x44, 0x02, 0x20, 0x4e, 0x45, 0xe1, 0x69,
		0x32, 0xb8, 0xaf, 0x51, 0x49, 0x61, 0xa1, 0xd3,
		0xa1, 0xa2, 0x5f, 0xdf, 0x3f, 0x4f, 0x77, 0x32,
		0xe9, 0xd6, 0x24, 0xc6, 0xc6, 0x15, 0x48, 0xab,
		0x5f, 0xb8, 0xcd, 0x41, 0x02, 0x20, 0x18, 0x15,
		0x22, 0xec, 0x8e, 0xca, 0x07, 0xde, 0x48, 0x60,
		0xa4, 0xac, 0xdd, 0x12, 0x90, 0x9d, 0x83, 0x1c,
		0xc5, 0x6c, 0xbb, 0xac, 0x46, 0x22, 0x08, 0x22,
		0x21, 0xa8, 0x76, 0x8d, 0x1d, 0x09,
	}
)

// CreateTestChannels creates to fully populated channels to be used within
// testing fixtures. The channels will be returned as if the funding process
// has just completed.  The channel itself is funded with 10 DCR, with 5 DCR
// allocated to each side. Within the channel, Alice is the initiator. The
// function also returns a "cleanup" function that is meant to be called once
// the test has been finalized. The clean up function will remote all temporary
// files created. If tweaklessCommits is true, then the commits within the
// channels will use the new format, otherwise the legacy format.
func CreateTestChannels(chanType channeldb.ChannelType) (
	*LightningChannel, *LightningChannel, func(), error) {

	channelCapacity, err := dcrutil.NewAmount(10)
	if err != nil {
		return nil, nil, nil, err
	}

	channelBal := channelCapacity / 2
	aliceDustLimit := dcrutil.Amount(6030)
	bobDustLimit := dcrutil.Amount(12060)
	csvTimeoutAlice := uint32(5)
	csvTimeoutBob := uint32(4)

	prevOut := &wire.OutPoint{
		Hash:  testHdSeed,
		Index: prand.Uint32(),
	}
	fundingTxIn := wire.NewTxIn(prevOut, 0, nil) // TODO(decred): Need correct input value

	// For each party, we'll create a distinct set of keys in order to
	// emulate the typical set up with live channels.
	var (
		aliceKeys []*secp256k1.PrivateKey
		bobKeys   []*secp256k1.PrivateKey
	)
	for i := 0; i < 5; i++ {
		key := make([]byte, len(testWalletPrivKey))
		copy(key, testWalletPrivKey)
		key[0] ^= byte(i + 1)

		aliceKey := secp256k1.PrivKeyFromBytes(key)
		aliceKeys = append(aliceKeys, aliceKey)

		key = make([]byte, len(bobsPrivKey))
		copy(key, bobsPrivKey)
		key[0] ^= byte(i + 1)

		bobKey := secp256k1.PrivKeyFromBytes(key)
		bobKeys = append(bobKeys, bobKey)
	}

	aliceCfg := channeldb.ChannelConfig{
		ChannelConstraints: channeldb.ChannelConstraints{
			DustLimit:        aliceDustLimit,
			MaxPendingAmount: lnwire.NewMAtomsFromAtoms(channelCapacity),
			ChanReserve:      channelCapacity / 100,
			MinHTLC:          0,
			MaxAcceptedHtlcs: input.MaxHTLCNumber / 2,
			CsvDelay:         uint16(csvTimeoutAlice),
		},
		MultiSigKey: keychain.KeyDescriptor{
			PubKey: aliceKeys[0].PubKey(),
		},
		RevocationBasePoint: keychain.KeyDescriptor{
			PubKey: aliceKeys[1].PubKey(),
		},
		PaymentBasePoint: keychain.KeyDescriptor{
			PubKey: aliceKeys[2].PubKey(),
		},
		DelayBasePoint: keychain.KeyDescriptor{
			PubKey: aliceKeys[3].PubKey(),
		},
		HtlcBasePoint: keychain.KeyDescriptor{
			PubKey: aliceKeys[4].PubKey(),
		},
	}
	bobCfg := channeldb.ChannelConfig{
		ChannelConstraints: channeldb.ChannelConstraints{
			DustLimit:        bobDustLimit,
			MaxPendingAmount: lnwire.NewMAtomsFromAtoms(channelCapacity),
			ChanReserve:      channelCapacity / 100,
			MinHTLC:          0,
			MaxAcceptedHtlcs: input.MaxHTLCNumber / 2,
			CsvDelay:         uint16(csvTimeoutBob),
		},
		MultiSigKey: keychain.KeyDescriptor{
			PubKey: bobKeys[0].PubKey(),
		},
		RevocationBasePoint: keychain.KeyDescriptor{
			PubKey: bobKeys[1].PubKey(),
		},
		PaymentBasePoint: keychain.KeyDescriptor{
			PubKey: bobKeys[2].PubKey(),
		},
		DelayBasePoint: keychain.KeyDescriptor{
			PubKey: bobKeys[3].PubKey(),
		},
		HtlcBasePoint: keychain.KeyDescriptor{
			PubKey: bobKeys[4].PubKey(),
		},
	}

	bobRoot, err := shachain.NewHash(bobKeys[0].Serialize())
	if err != nil {
		return nil, nil, nil, err
	}
	bobPreimageProducer := shachain.NewRevocationProducer(*bobRoot)
	bobFirstRevoke, err := bobPreimageProducer.AtIndex(0)
	if err != nil {
		return nil, nil, nil, err
	}
	bobCommitPoint := input.ComputeCommitmentPoint(bobFirstRevoke[:])

	aliceRoot, err := shachain.NewHash(aliceKeys[0].Serialize())
	if err != nil {
		return nil, nil, nil, err
	}
	alicePreimageProducer := shachain.NewRevocationProducer(*aliceRoot)
	aliceFirstRevoke, err := alicePreimageProducer.AtIndex(0)
	if err != nil {
		return nil, nil, nil, err
	}
	aliceCommitPoint := input.ComputeCommitmentPoint(aliceFirstRevoke[:])

	netParams := chaincfg.RegNetParams()

	aliceCommitTx, bobCommitTx, err := CreateCommitmentTxns(
		channelBal, channelBal, &aliceCfg, &bobCfg, aliceCommitPoint,
		bobCommitPoint, *fundingTxIn, chanType, netParams,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	alicePath, err := ioutil.TempDir("", "alicedb")
	if err != nil {
		return nil, nil, nil, err
	}
	dbAlice, err := channeldb.Open(alicePath)
	if err != nil {
		return nil, nil, nil, err
	}

	bobPath, err := ioutil.TempDir("", "bobdb")
	if err != nil {
		return nil, nil, nil, err
	}
	dbBob, err := channeldb.Open(bobPath)
	if err != nil {
		return nil, nil, nil, err
	}

	// The rate for this estimator must be the same as what is returned by
	// calcStaticFee().
	//
	// Note: This is purposefully higher than the feeKBFloor (that is, the
	// network standard relay fee) so that tests can try both lowering and
	// increasing the fee rate.
	estimator := chainfee.NewStaticEstimator(1e5, 0)
	feePerKB, err := estimator.EstimateFeePerKB(1)
	if err != nil {
		return nil, nil, nil, err
	}
	commitFee := calcStaticFee(chanType, 0)
	var anchorAmt dcrutil.Amount
	if chanType.HasAnchors() {
		anchorAmt += 2 * anchorSize
	}

	aliceBalance := lnwire.NewMAtomsFromAtoms(
		channelBal - commitFee - anchorAmt,
	)
	bobBalance := lnwire.NewMAtomsFromAtoms(channelBal)

	aliceCommit := channeldb.ChannelCommitment{
		CommitHeight:  0,
		LocalBalance:  aliceBalance,
		RemoteBalance: bobBalance,
		CommitFee:     commitFee,
		FeePerKB:      dcrutil.Amount(feePerKB),
		CommitTx:      aliceCommitTx,
		CommitSig:     testSigBytes,
	}
	bobCommit := channeldb.ChannelCommitment{
		CommitHeight:  0,
		LocalBalance:  bobBalance,
		RemoteBalance: aliceBalance,
		CommitFee:     commitFee,
		FeePerKB:      dcrutil.Amount(feePerKB),
		CommitTx:      bobCommitTx,
		CommitSig:     testSigBytes,
	}

	var chanIDBytes [8]byte
	if _, err := io.ReadFull(rand.Reader, chanIDBytes[:]); err != nil {
		return nil, nil, nil, err
	}

	shortChanID := lnwire.NewShortChanIDFromInt(
		binary.BigEndian.Uint64(chanIDBytes[:]),
	)

	aliceChannelState := &channeldb.OpenChannel{
		LocalChanCfg:            aliceCfg,
		RemoteChanCfg:           bobCfg,
		IdentityPub:             aliceKeys[0].PubKey(),
		FundingOutpoint:         *prevOut,
		ShortChannelID:          shortChanID,
		ChanType:                chanType,
		IsInitiator:             true,
		Capacity:                channelCapacity,
		RemoteCurrentRevocation: bobCommitPoint,
		RevocationProducer:      alicePreimageProducer,
		RevocationStore:         shachain.NewRevocationStore(),
		LocalCommitment:         aliceCommit,
		RemoteCommitment:        aliceCommit,
		Db:                      dbAlice,
		Packager:                channeldb.NewChannelPackager(shortChanID),
		FundingTxn:              testTx,
	}
	bobChannelState := &channeldb.OpenChannel{
		LocalChanCfg:            bobCfg,
		RemoteChanCfg:           aliceCfg,
		IdentityPub:             bobKeys[0].PubKey(),
		FundingOutpoint:         *prevOut,
		ShortChannelID:          shortChanID,
		ChanType:                chanType,
		IsInitiator:             false,
		Capacity:                channelCapacity,
		RemoteCurrentRevocation: aliceCommitPoint,
		RevocationProducer:      bobPreimageProducer,
		RevocationStore:         shachain.NewRevocationStore(),
		LocalCommitment:         bobCommit,
		RemoteCommitment:        bobCommit,
		Db:                      dbBob,
		Packager:                channeldb.NewChannelPackager(shortChanID),
	}

	aliceSigner := &input.MockSigner{Privkeys: aliceKeys}
	bobSigner := &input.MockSigner{Privkeys: bobKeys}

	// TODO(roasbeef): make mock version of pre-image store

	alicePool := NewSigPool(1, aliceSigner)
	channelAlice, err := NewLightningChannel(
		aliceSigner, aliceChannelState, alicePool, netParams,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	alicePool.Start()

	obfuscator := createStateHintObfuscator(aliceChannelState)

	bobPool := NewSigPool(1, bobSigner)
	channelBob, err := NewLightningChannel(
		bobSigner, bobChannelState, bobPool, netParams,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	if err = bobPool.Start(); err != nil {
		return nil, nil, nil, err
	}
	err = SetStateNumHint(
		aliceCommitTx, 0, obfuscator,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	err = SetStateNumHint(
		bobCommitTx, 0, obfuscator,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	addr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18556,
	}
	if err := channelAlice.channelState.SyncPending(addr, 101); err != nil {
		return nil, nil, nil, err
	}

	addr = &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18555,
	}

	if err := channelBob.channelState.SyncPending(addr, 101); err != nil {
		return nil, nil, nil, err
	}

	cleanUpFunc := func() {
		os.RemoveAll(bobPath)
		os.RemoveAll(alicePath)

		alicePool.Stop()
		bobPool.Stop()
	}

	// Now that the channel are open, simulate the start of a session by
	// having Alice and Bob extend their revocation windows to each other.
	err = initRevocationWindows(channelAlice, channelBob)
	if err != nil {
		return nil, nil, nil, err
	}

	return channelAlice, channelBob, cleanUpFunc, nil
}

// initRevocationWindows simulates a new channel being opened within the p2p
// network by populating the initial revocation windows of the passed
// commitment state machines.
func initRevocationWindows(chanA, chanB *LightningChannel) error {
	aliceNextRevoke, err := chanA.NextRevocationKey()
	if err != nil {
		return err
	}
	if err := chanB.InitNextRevocation(aliceNextRevoke); err != nil {
		return err
	}

	bobNextRevoke, err := chanB.NextRevocationKey()
	if err != nil {
		return err
	}
	if err := chanA.InitNextRevocation(bobNextRevoke); err != nil {
		return err
	}

	return nil
}

// privkeyFromHex parses a Decred private key from a hex encoded string.
func privkeyFromHex(keyHex string) (*secp256k1.PrivateKey, error) {
	bytes, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, err
	}
	key := secp256k1.PrivKeyFromBytes(bytes)
	return key, nil

}

func privKeyFromBytes(b []byte) (*secp256k1.PrivateKey, *secp256k1.PublicKey) {
	key := secp256k1.PrivKeyFromBytes(b)
	return key, key.PubKey()
}

// txFromHex parses a full Decred transaction from a hex encoded string.
func txFromHex(txHex string) (*dcrutil.Tx, error) {
	bytes, err := hex.DecodeString(txHex)
	if err != nil {
		return nil, err
	}
	return dcrutil.NewTxFromBytes(bytes)
}

// calcStaticFee calculates appropriate fees for commitment transactions.  This
// function provides a simple way to allow test balance assertions to take fee
// calculations into account.
//
// This uses a fixed, hard-coded value of 6000 Atoms/kB as fee.
//
// TODO(bvu): Refactor when dynamic fee estimation is added.
func calcStaticFee(chanType channeldb.ChannelType, numHTLCs int) dcrutil.Amount {
	const (
		feePerKB = dcrutil.Amount(1e5)
	)

	commitSize := CommitSize(chanType) + input.HTLCOutputSize*int64(numHTLCs)
	return feePerKB * dcrutil.Amount(commitSize) / 1000
}

// ForceStateTransition executes the necessary interaction between the two
// commitment state machines to transition to a new state locking in any
// pending updates. This method is useful when testing interactions between two
// live state machines.
func ForceStateTransition(chanA, chanB *LightningChannel) error {
	aliceSig, aliceHtlcSigs, _, err := chanA.SignNextCommitment()
	if err != nil {
		return err
	}
	if err = chanB.ReceiveNewCommitment(aliceSig, aliceHtlcSigs); err != nil {
		return err
	}

	bobRevocation, _, err := chanB.RevokeCurrentCommitment()
	if err != nil {
		return err
	}
	bobSig, bobHtlcSigs, _, err := chanB.SignNextCommitment()
	if err != nil {
		return err
	}

	if _, _, _, _, err := chanA.ReceiveRevocation(bobRevocation); err != nil {
		return err
	}
	if err := chanA.ReceiveNewCommitment(bobSig, bobHtlcSigs); err != nil {
		return err
	}

	aliceRevocation, _, err := chanA.RevokeCurrentCommitment()
	if err != nil {
		return err
	}
	if _, _, _, _, err := chanB.ReceiveRevocation(aliceRevocation); err != nil {
		return err
	}

	return nil
}
