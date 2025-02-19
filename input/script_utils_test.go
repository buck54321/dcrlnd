package input

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/dcrec/secp256k1/v3"
	"github.com/decred/dcrd/dcrec/secp256k1/v3/ecdsa"
	"github.com/decred/dcrd/dcrutil/v4"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/wire"
	"github.com/decred/dcrlnd/keychain"
)

const scriptVersion uint16 = 0

func privKeyFromBytes(b []byte) (*secp256k1.PrivateKey, *secp256k1.PublicKey) {
	priv := secp256k1.PrivKeyFromBytes(b)
	return priv, priv.PubKey()
}

// assertEngineExecution executes the VM returned by the newEngine closure,
// asserting the result matches the validity expectation. In the case where it
// doesn't match the expectation, it executes the script step-by-step and
// prints debug information to stdout.
func assertEngineExecution(t *testing.T, testNum int, valid bool,
	newEngine func() (*txscript.Engine, error)) {
	t.Helper()

	// Get a new VM to execute.
	vm, err := newEngine()
	if err != nil {
		t.Fatalf("unable to create engine: %v", err)
	}

	// Execute the VM, only go on to the step-by-step execution if
	// it doesn't validate as expected.
	vmErr := vm.Execute()
	if valid == (vmErr == nil) {
		return
	}

	// Now that the execution didn't match what we expected, fetch a new VM
	// to step through.
	vm, err = newEngine()
	if err != nil {
		t.Fatalf("unable to create engine: %v", err)
	}

	// This buffer will trace execution of the Script, dumping out
	// to stdout.
	var debugBuf bytes.Buffer

	done := false
	for !done {
		dis, err := vm.DisasmPC()
		if err != nil {
			t.Fatalf("stepping (%v)\n", err)
		}
		debugBuf.WriteString(fmt.Sprintf("stepping %v\n", dis))

		done, err = vm.Step()
		if err != nil && valid {
			fmt.Println(debugBuf.String())
			t.Fatalf("spend test case #%v failed, spend "+
				"should be valid: %v", testNum, err)
		} else if err == nil && !valid && done {
			fmt.Println(debugBuf.String())
			t.Fatalf("spend test case #%v succeed, spend "+
				"should be invalid: %v", testNum, err)
		}

		debugBuf.WriteString(fmt.Sprintf("Stack: %v", vm.GetStack()))
		debugBuf.WriteString(fmt.Sprintf("AltStack: %v", vm.GetAltStack()))
	}

	// If we get to this point the unexpected case was not reached
	// during step execution, which happens for some checks, like
	// the clean-stack rule.
	validity := "invalid"
	if valid {
		validity = "valid"
	}

	fmt.Println(debugBuf.String())
	t.Fatalf("%v spend test case #%v execution ended with: %v", validity, testNum, vmErr)
}

// TestRevocationKeyDerivation tests that given a public key, and a revocation
// hash, the homomorphic revocation public and private key derivation work
// properly.
func TestRevocationKeyDerivation(t *testing.T) {
	t.Parallel()

	// First, we'll generate a commitment point, and a commitment secret.
	// These will be used to derive the ultimate revocation keys.
	revocationPreimage := testHdSeed.CloneBytes()
	commitSecret, commitPoint := privKeyFromBytes(revocationPreimage)

	// With the commitment secrets generated, we'll now create the base
	// keys we'll use to derive the revocation key from.
	basePriv, basePub := privKeyFromBytes(testWalletPrivKey)

	// With the point and key obtained, we can now derive the revocation
	// key itself.
	revocationPub := DeriveRevocationPubkey(basePub, commitPoint)

	// The revocation public key derived from the original public key, and
	// the one derived from the private key should be identical.
	revocationPriv := DeriveRevocationPrivKey(basePriv, commitSecret)
	if !revocationPub.IsEqual(revocationPriv.PubKey()) {
		t.Fatalf("derived public keys don't match!")
	}
}

// TestTweakKeyDerivation tests that given a public key, and commitment tweak,
// then we're able to properly derive a tweaked private key that corresponds to
// the computed tweak public key. This scenario ensure that our key derivation
// for any of the non revocation keys on the commitment transaction is correct.
func TestTweakKeyDerivation(t *testing.T) {
	t.Parallel()

	// First, we'll generate a base public key that we'll be "tweaking".
	baseSecret := testHdSeed.CloneBytes()
	basePriv, basePub := privKeyFromBytes(baseSecret)

	// With the base key create, we'll now create a commitment point, and
	// from that derive the bytes we'll used to tweak the base public key.
	commitPoint := ComputeCommitmentPoint(bobsPrivKey)
	commitTweak := SingleTweakBytes(commitPoint, basePub)

	// Next, we'll modify the public key. When we apply the same operation
	// to the private key we should get a key that matches.
	tweakedPub := TweakPubKey(basePub, commitPoint)

	// Finally, attempt to re-generate the private key that matches the
	// tweaked public key. The derived key should match exactly.
	derivedPriv := TweakPrivKey(basePriv, commitTweak)
	if !derivedPriv.PubKey().IsEqual(tweakedPub) {
		t.Fatalf("pub keys don't match")
	}
}

// makeWitnessTestCase is a helper function used within test cases involving
// the validity of a crafted witness. This function is a wrapper function which
// allows constructing table-driven tests. In the case of an error while
// constructing the witness, the test fails fatally.
func makeWitnessTestCase(t *testing.T,
	f func() (TxWitness, error)) func() TxWitness {

	return func() TxWitness {
		witness, err := f()
		if err != nil {
			t.Fatalf("unable to create witness test case: %v", err)
		}

		return witness
	}
}

// TestHTLCSenderSpendValidation tests all possible valid+invalid redemption
// paths in the script used within the sender's commitment transaction for an
// outgoing HTLC.
//
// The following cases are exercised by this test:
// sender script:
//  * receiver spends
//    * revoke w/ sig
//    * HTLC with invalid preimage size
//    * HTLC with valid preimage size + sig
//  * sender spends
//    * invalid lock-time for CLTV
//    * invalid sequence for CSV
//    * valid lock-time+sequence, valid sig
func TestHTLCSenderSpendValidation(t *testing.T) {
	t.Parallel()

	// We generate a fake output, and the corresponding txin. This output
	// doesn't need to exist, as we'll only be validating spending from the
	// transaction that references this.
	txid, err := chainhash.NewHash(testHdSeed.CloneBytes())
	if err != nil {
		t.Fatalf("unable to create txid: %v", err)
	}
	fundingOut := &wire.OutPoint{
		Hash:  *txid,
		Index: 50,
	}
	fakeFundingTxIn := wire.NewTxIn(fundingOut, 0, nil) // TODO(decred): Need correct input value

	// Next we'll the commitment secret for our commitment tx and also the
	// revocation key that we'll use as well.
	revokePreimage := testHdSeed.CloneBytes()
	commitSecret, commitPoint := privKeyFromBytes(revokePreimage)

	// Generate a payment preimage to be used below.
	paymentPreimage := revokePreimage
	paymentPreimage[0] ^= 1
	paymentHash := sha256.Sum256(paymentPreimage)

	// We'll also need some tests keys for alice and bob, and metadata of
	// the HTLC output.
	aliceKeyPriv, aliceKeyPub := privKeyFromBytes(testWalletPrivKey)
	bobKeyPriv, bobKeyPub := privKeyFromBytes(bobsPrivKey)
	paymentAmt := dcrutil.Amount(1 * 10e8)

	aliceLocalKey := TweakPubKey(aliceKeyPub, commitPoint)
	bobLocalKey := TweakPubKey(bobKeyPub, commitPoint)

	// As we'll be modeling spends from Alice's commitment transaction,
	// we'll be using Bob's base point for the revocation key.
	revocationKey := DeriveRevocationPubkey(bobKeyPub, commitPoint)

	bobCommitTweak := SingleTweakBytes(commitPoint, bobKeyPub)
	aliceCommitTweak := SingleTweakBytes(commitPoint, aliceKeyPub)

	// Finally, we'll create mock signers for both of them based on their
	// private keys. This test simplifies a bit and uses the same key as
	// the base point for all scripts and derivations.
	bobSigner := &MockSigner{Privkeys: []*secp256k1.PrivateKey{bobKeyPriv}}
	aliceSigner := &MockSigner{Privkeys: []*secp256k1.PrivateKey{aliceKeyPriv}}

	var (
		htlcWitnessScript, htlcPkScript []byte
		htlcOutput                      *wire.TxOut
		senderCommitTx, sweepTx         *wire.MsgTx
		bobRecvrSig                     *ecdsa.Signature
		bobSigHash                      txscript.SigHashType
	)

	// genCommitTx generates a commitment tx where the htlc output requires
	// confirmation to be spent according to 'confirmed'.
	genCommitTx := func(confirmed bool) {
		// Generate the raw HTLC redemption scripts, and its p2wsh
		// counterpart.
		htlcWitnessScript, err = SenderHTLCScript(
			aliceLocalKey, bobLocalKey, revocationKey,
			paymentHash[:], confirmed,
		)
		if err != nil {
			t.Fatalf("unable to create htlc sender script: %v", err)
		}
		htlcPkScript, err = ScriptHashPkScript(htlcWitnessScript)
		if err != nil {
			t.Fatalf("unable to create p2wsh htlc script: %v", err)
		}

		// This will be Alice's commitment transaction. In this
		// scenario Alice is sending an HTLC to a node she has a path
		// to (could be Bob, could be multiple hops down, it doesn't
		// really matter).
		htlcOutput = &wire.TxOut{
			Value:    int64(paymentAmt),
			PkScript: htlcPkScript,
			Version:  scriptVersion,
		}
		senderCommitTx = wire.NewMsgTx()
		senderCommitTx.Version = LNTxVersion
		senderCommitTx.AddTxIn(fakeFundingTxIn)
		senderCommitTx.AddTxOut(htlcOutput)
	}

	// genSweepTx generates a sweep of the senderCommitTx, and sets the
	// sequence and sighash single|anyonecanspend if confirmed is true.
	genSweepTx := func(confirmed bool) {
		prevOut := &wire.OutPoint{
			Hash:  senderCommitTx.TxHash(),
			Index: 0,
		}

		sweepTx = wire.NewMsgTx()
		sweepTx.Version = LNTxVersion

		sweepTx.AddTxIn(wire.NewTxIn(prevOut, 0, nil)) // TODO(decred) need correct input value
		if confirmed {
			sweepTx.TxIn[0].Sequence = LockTimeToSequence(false, 1)
		}

		sweepTx.AddTxOut(
			&wire.TxOut{
				PkScript: []byte("doesn't matter"),
				Value:    1 * 10e8,
			},
		)

		bobSigHash = txscript.SigHashAll
		if confirmed {
			bobSigHash = txscript.SigHashSingle | txscript.SigHashAnyOneCanPay
		}

		// We'll also generate a signature on the sweep transaction above
		// that will act as Bob's signature to Alice for the second level HTLC
		// transaction.
		bobSignDesc := SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: bobKeyPub,
			},
			SingleTweak:   bobCommitTweak,
			WitnessScript: htlcWitnessScript,
			Output:        htlcOutput,
			HashType:      bobSigHash,
			InputIndex:    0,
		}
		bobSig, err := bobSigner.SignOutputRaw(sweepTx, &bobSignDesc)
		if err != nil {
			t.Fatalf("unable to generate alice signature: %v", err)
		}

		bobRecvrSig, err = ecdsa.ParseDERSignature(
			bobSig.Serialize(),
		)
		if err != nil {
			t.Fatalf("unable to parse signature: %v", err)
		}
	}

	testCases := []struct {
		witness func() TxWitness
		valid   bool
	}{
		{
			// revoke w/ sig
			// TODO(roasbeef): test invalid revoke
			makeWitnessTestCase(t, func() (TxWitness, error) {
				genCommitTx(false)
				genSweepTx(false)

				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: bobKeyPub,
					},
					DoubleTweak:   commitSecret,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return senderHtlcSpendRevoke(bobSigner, signDesc,
					revocationKey, sweepTx)
			}),
			true,
		},
		{
			// HTLC with invalid preimage size
			makeWitnessTestCase(t, func() (TxWitness, error) {
				genCommitTx(false)
				genSweepTx(false)

				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: bobKeyPub,
					},
					SingleTweak:   bobCommitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return SenderHtlcSpendRedeem(bobSigner, signDesc,
					sweepTx,
					// Invalid preimage length
					bytes.Repeat([]byte{1}, 45))
			}),
			false,
		},
		{
			// HTLC with valid preimage size + sig
			// TODO(roasbeef): invalid preimage
			makeWitnessTestCase(t, func() (TxWitness, error) {
				genCommitTx(false)
				genSweepTx(false)

				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: bobKeyPub,
					},
					SingleTweak:   bobCommitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return SenderHtlcSpendRedeem(bobSigner, signDesc,
					sweepTx, paymentPreimage)
			}),
			true,
		},
		{
			// HTLC with valid preimage size + sig, and with
			// enforced locktime in HTLC script.
			makeWitnessTestCase(t, func() (TxWitness, error) {
				// Make a commit tx that needs confirmation for
				// HTLC output to be spent.
				genCommitTx(true)

				// Generate a sweep with the locktime set.
				genSweepTx(true)

				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: bobKeyPub,
					},
					SingleTweak:   bobCommitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return SenderHtlcSpendRedeem(bobSigner, signDesc,
					sweepTx, paymentPreimage)
			}),
			true,
		},
		{
			// HTLC with valid preimage size + sig, but trying to
			// spend CSV output without sequence set.
			makeWitnessTestCase(t, func() (TxWitness, error) {
				// Generate commitment tx with 1 CSV locked
				// HTLC.
				genCommitTx(true)

				// Generate sweep tx that doesn't have locktime
				// enabled.
				genSweepTx(false)

				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: bobKeyPub,
					},
					SingleTweak:   bobCommitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return SenderHtlcSpendRedeem(bobSigner, signDesc,
					sweepTx, paymentPreimage)
			}),
			false,
		},

		{
			// valid spend to the transition the state of the HTLC
			// output with the second level HTLC timeout
			// transaction.
			makeWitnessTestCase(t, func() (TxWitness, error) {
				genCommitTx(false)
				genSweepTx(false)

				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: aliceKeyPub,
					},
					SingleTweak:   aliceCommitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return SenderHtlcSpendTimeout(
					bobRecvrSig, bobSigHash, aliceSigner,
					signDesc, sweepTx,
				)
			}),
			true,
		},
		{
			// valid spend to the transition the state of the HTLC
			// output with the second level HTLC timeout
			// transaction.
			makeWitnessTestCase(t, func() (TxWitness, error) {
				// Make a commit tx that needs confirmation for
				// HTLC output to be spent.
				genCommitTx(true)

				// Generate a sweep with the locktime set.
				genSweepTx(true)

				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: aliceKeyPub,
					},
					SingleTweak:   aliceCommitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return SenderHtlcSpendTimeout(
					bobRecvrSig, bobSigHash, aliceSigner,
					signDesc, sweepTx,
				)
			}),
			true,
		},
		{
			// valid spend to the transition the state of the HTLC
			// output with the second level HTLC timeout
			// transaction.
			makeWitnessTestCase(t, func() (TxWitness, error) {
				// Generate commitment tx with 1 CSV locked
				// HTLC.
				genCommitTx(true)

				// Generate sweep tx that doesn't have locktime
				// enabled.
				genSweepTx(false)

				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: aliceKeyPub,
					},
					SingleTweak:   aliceCommitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return SenderHtlcSpendTimeout(
					bobRecvrSig, bobSigHash, aliceSigner,
					signDesc, sweepTx,
				)
			}),
			false,
		},
	}

	// TODO(roasbeef): set of cases to ensure able to sign w/ keypath and
	// not

	for i, testCase := range testCases {
		sweepTx.TxIn[0].SignatureScript, err = WitnessStackToSigScript(testCase.witness())
		if err != nil {
			t.Fatalf("unable to convert witness stack to sigScript: %v", err)
		}

		newEngine := func() (*txscript.Engine, error) {
			return txscript.NewEngine(htlcPkScript,
				sweepTx, 0, ScriptVerifyFlags, htlcOutput.Version,
				nil)
		}

		assertEngineExecution(t, i, testCase.valid, newEngine)
	}
}

// TestHTLCReceiverSpendValidation tests all possible valid+invalid redemption
// paths in the script used within the receiver's commitment transaction for an
// incoming HTLC.
//
// The following cases are exercised by this test:
//  * receiver spends
//     * HTLC redemption w/ invalid preimage size
//     * HTLC redemption w/ invalid sequence
//     * HTLC redemption w/ valid preimage size
//  * sender spends
//     * revoke w/ sig
//     * refund w/ invalid lock time
//     * refund w/ valid lock time
func TestHTLCReceiverSpendValidation(t *testing.T) {
	t.Parallel()

	// We generate a fake output, and the corresponding txin. This output
	// doesn't need to exist, as we'll only be validating spending from the
	// transaction that references this.
	txid, err := chainhash.NewHash(testHdSeed.CloneBytes())
	if err != nil {
		t.Fatalf("unable to create txid: %v", err)
	}
	fundingOut := &wire.OutPoint{
		Hash:  *txid,
		Index: 50,
	}
	fakeFundingTxIn := wire.NewTxIn(fundingOut, 0, nil) // TODO(decred): Need correct input value

	// Next we'll the commitment secret for our commitment tx and also the
	// revocation key that we'll use as well.
	revokePreimage := testHdSeed.CloneBytes()
	commitSecret, commitPoint := privKeyFromBytes(revokePreimage)

	// Generate a payment preimage to be used below.
	paymentPreimage := revokePreimage
	paymentPreimage[0] ^= 1
	paymentHash := sha256.Sum256(paymentPreimage)

	// We'll also need some tests keys for alice and bob, and metadata of
	// the HTLC output.
	aliceKeyPriv, aliceKeyPub := privKeyFromBytes(
		testWalletPrivKey)
	bobKeyPriv, bobKeyPub := privKeyFromBytes(bobsPrivKey)
	paymentAmt := dcrutil.Amount(1 * 10e8)
	cltvTimeout := uint32(8)

	aliceLocalKey := TweakPubKey(aliceKeyPub, commitPoint)
	bobLocalKey := TweakPubKey(bobKeyPub, commitPoint)

	// As we'll be modeling spends from Bob's commitment transaction, we'll
	// be using Alice's base point for the revocation key.
	revocationKey := DeriveRevocationPubkey(aliceKeyPub, commitPoint)

	bobCommitTweak := SingleTweakBytes(commitPoint, bobKeyPub)
	aliceCommitTweak := SingleTweakBytes(commitPoint, aliceKeyPub)

	// Finally, we'll create mock signers for both of them based on their
	// private keys. This test simplifies a bit and uses the same key as
	// the base point for all scripts and derivations.
	bobSigner := &MockSigner{Privkeys: []*secp256k1.PrivateKey{bobKeyPriv}}
	aliceSigner := &MockSigner{Privkeys: []*secp256k1.PrivateKey{aliceKeyPriv}}

	var (
		htlcWitnessScript, htlcPkScript []byte
		htlcOutput                      *wire.TxOut
		receiverCommitTx, sweepTx       *wire.MsgTx
		aliceSenderSig                  *ecdsa.Signature
		aliceSigHash                    txscript.SigHashType
	)

	genCommitTx := func(confirmed bool) {
		// Generate the raw HTLC redemption scripts, and its p2wsh
		// counterpart.
		htlcWitnessScript, err = ReceiverHTLCScript(
			cltvTimeout, aliceLocalKey, bobLocalKey, revocationKey,
			paymentHash[:], confirmed,
		)
		if err != nil {
			t.Fatalf("unable to create htlc sender script: %v", err)
		}
		htlcPkScript, err = ScriptHashPkScript(htlcWitnessScript)
		if err != nil {
			t.Fatalf("unable to create p2wsh htlc script: %v", err)
		}

		// This will be Bob's commitment transaction. In this scenario Alice is
		// sending an HTLC to a node she has a path to (could be Bob, could be
		// multiple hops down, it doesn't really matter).
		htlcOutput = &wire.TxOut{
			Value:    int64(paymentAmt),
			PkScript: htlcWitnessScript,
		}

		receiverCommitTx = wire.NewMsgTx()
		receiverCommitTx.Version = LNTxVersion
		receiverCommitTx.AddTxIn(fakeFundingTxIn)
		receiverCommitTx.AddTxOut(htlcOutput)
	}

	genSweepTx := func(confirmed bool) {
		prevOut := &wire.OutPoint{
			Hash:  receiverCommitTx.TxHash(),
			Index: 0,
		}

		sweepTx = wire.NewMsgTx()
		sweepTx.Version = LNTxVersion
		sweepTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: *prevOut,
		})
		if confirmed {
			sweepTx.TxIn[0].Sequence = LockTimeToSequence(false, 1)
		}

		sweepTx.AddTxOut(
			&wire.TxOut{
				PkScript: []byte("doesn't matter"),
				Value:    1 * 10e8,
			},
		)

		aliceSigHash = txscript.SigHashAll
		if confirmed {
			aliceSigHash = txscript.SigHashSingle | txscript.SigHashAnyOneCanPay
		}

		// We'll also generate a signature on the sweep transaction above
		// that will act as Alice's signature to Bob for the second level HTLC
		// transaction.
		aliceSignDesc := SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: aliceKeyPub,
			},
			SingleTweak:   aliceCommitTweak,
			WitnessScript: htlcWitnessScript,
			Output:        htlcOutput,
			HashType:      aliceSigHash,
			InputIndex:    0,
		}
		aliceSigBytes, err := aliceSigner.SignOutputRaw(sweepTx, &aliceSignDesc)
		if err != nil {
			t.Fatalf("unable to generate alice signature: %v", err)
		}

		aliceSenderSig, err = ecdsa.ParseDERSignature(
			aliceSigBytes.Serialize(),
		)
		if err != nil {
			t.Fatalf("unable to parse signature: %v", err)
		}
	}

	// TODO(roasbeef): modify valid to check precise script errors?
	testCases := []struct {
		witness func() TxWitness
		valid   bool
	}{
		{
			// HTLC redemption w/ invalid preimage size
			makeWitnessTestCase(t, func() (TxWitness, error) {
				genCommitTx(false)
				genSweepTx(false)

				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: bobKeyPub,
					},
					SingleTweak:   bobCommitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return ReceiverHtlcSpendRedeem(
					aliceSenderSig, aliceSigHash,
					bytes.Repeat([]byte{1}, 45), bobSigner,
					signDesc, sweepTx,
				)

			}),
			false,
		},
		{
			// HTLC redemption w/ valid preimage size
			makeWitnessTestCase(t, func() (TxWitness, error) {
				genCommitTx(false)
				genSweepTx(false)

				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: bobKeyPub,
					},
					SingleTweak:   bobCommitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return ReceiverHtlcSpendRedeem(
					aliceSenderSig, aliceSigHash,
					paymentPreimage, bobSigner,
					signDesc, sweepTx,
				)
			}),
			true,
		},
		{
			// revoke w/ sig
			makeWitnessTestCase(t, func() (TxWitness, error) {
				genCommitTx(false)
				genSweepTx(false)

				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: aliceKeyPub,
					},
					DoubleTweak:   commitSecret,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return receiverHtlcSpendRevoke(aliceSigner,
					signDesc, revocationKey, sweepTx)
			}),
			true,
		},
		{
			// HTLC redemption w/ valid preimage size, and with
			// enforced locktime in HTLC scripts.
			makeWitnessTestCase(t, func() (TxWitness, error) {
				// Make a commit tx that needs confirmation for
				// HTLC output to be spent.
				genCommitTx(true)

				// Generate a sweep with the locktime set.
				genSweepTx(true)

				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: bobKeyPub,
					},
					SingleTweak:   bobCommitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return ReceiverHtlcSpendRedeem(
					aliceSenderSig, aliceSigHash,
					paymentPreimage, bobSigner,
					signDesc, sweepTx,
				)
			}),
			true,
		},
		{
			// HTLC redemption w/ valid preimage size, but trying
			// to spend CSV output without sequence set.
			makeWitnessTestCase(t, func() (TxWitness, error) {
				// Generate commitment tx with 1 CSV locked
				// HTLC.
				genCommitTx(true)

				// Generate sweep tx that doesn't have locktime
				// enabled.
				genSweepTx(false)

				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: bobKeyPub,
					},
					SingleTweak:   bobCommitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return ReceiverHtlcSpendRedeem(
					aliceSenderSig, aliceSigHash,
					paymentPreimage, bobSigner, signDesc,
					sweepTx,
				)
			}),
			false,
		},

		{
			// refund w/ invalid lock time
			makeWitnessTestCase(t, func() (TxWitness, error) {
				genCommitTx(false)
				genSweepTx(false)

				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: aliceKeyPub,
					},
					SingleTweak:   aliceCommitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return ReceiverHtlcSpendTimeout(aliceSigner, signDesc,
					sweepTx, int32(cltvTimeout-2))
			}),
			false,
		},
		{
			// refund w/ valid lock time
			makeWitnessTestCase(t, func() (TxWitness, error) {
				genCommitTx(false)
				genSweepTx(false)

				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: aliceKeyPub,
					},
					SingleTweak:   aliceCommitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return ReceiverHtlcSpendTimeout(aliceSigner, signDesc,
					sweepTx, int32(cltvTimeout))
			}),
			true,
		},
		{
			// refund w/ valid lock time, and enforced locktime in
			// HTLC scripts.
			makeWitnessTestCase(t, func() (TxWitness, error) {
				// Make a commit tx that needs confirmation for
				// HTLC output to be spent.
				genCommitTx(true)

				// Generate a sweep with the locktime set.
				genSweepTx(true)

				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: aliceKeyPub,
					},
					SingleTweak:   aliceCommitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return ReceiverHtlcSpendTimeout(aliceSigner, signDesc,
					sweepTx, int32(cltvTimeout))
			}),
			true,
		},
		{
			// refund w/ valid lock time, but no sequence set in
			// sweep tx trying to spend CSV locked HTLC output.
			makeWitnessTestCase(t, func() (TxWitness, error) {
				// Generate commitment tx with 1 CSV locked
				// HTLC.
				genCommitTx(true)

				// Generate sweep tx that doesn't have locktime
				// enabled.
				genSweepTx(false)

				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: aliceKeyPub,
					},
					SingleTweak:   aliceCommitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return ReceiverHtlcSpendTimeout(aliceSigner, signDesc,
					sweepTx, int32(cltvTimeout))
			}),
			false,
		},
	}

	for i, testCase := range testCases {
		sweepTx.TxIn[0].SignatureScript, err = WitnessStackToSigScript(testCase.witness())
		if err != nil {
			t.Fatalf("unable to convert witness stack to sigScript: %v", err)
		}

		newEngine := func() (*txscript.Engine, error) {
			return txscript.NewEngine(htlcPkScript,
				sweepTx, 0, ScriptVerifyFlags, scriptVersion,
				nil)
		}

		assertEngineExecution(t, i, testCase.valid, newEngine)
	}
}

// TestSecondLevelHtlcSpends tests all the possible redemption clauses from the
// HTLC success and timeout covenant transactions.
func TestSecondLevelHtlcSpends(t *testing.T) {
	t.Parallel()

	// We'll start be creating a creating a 2 DCR HTLC.
	const htlcAmt = dcrutil.Amount(2 * 10e8)

	// In all of our scenarios, the CSV timeout to claim a self output will
	// be 5 blocks.
	const claimDelay = 5

	// First we'll set up some initial key state for Alice and Bob that
	// will be used in the scripts we created below.
	aliceKeyPriv, aliceKeyPub := privKeyFromBytes(testWalletPrivKey)
	bobKeyPriv, bobKeyPub := privKeyFromBytes(bobsPrivKey)

	revokePreimage := testHdSeed.CloneBytes()
	commitSecret, commitPoint := privKeyFromBytes(revokePreimage)

	// As we're modeling this as Bob sweeping the HTLC on-chain from his
	// commitment transaction after a period of time, we'll be using a
	// revocation key derived from Alice's base point and his secret.
	revocationKey := DeriveRevocationPubkey(aliceKeyPub, commitPoint)

	// Next, craft a fake HTLC outpoint that we'll use to generate the
	// sweeping transaction using.
	txid, err := chainhash.NewHash(testHdSeed.CloneBytes())
	if err != nil {
		t.Fatalf("unable to create txid: %v", err)
	}
	htlcOutPoint := &wire.OutPoint{
		Hash:  *txid,
		Index: 0,
	}
	sweepTx := wire.NewMsgTx()
	sweepTx.Version = LNTxVersion
	sweepTx.AddTxIn(wire.NewTxIn(htlcOutPoint, 0, nil)) // TODO(decred): Need correct input value
	sweepTx.AddTxOut(
		&wire.TxOut{
			PkScript: []byte("doesn't matter"),
			Value:    1 * 10e8,
		},
	)

	// The delay key will be crafted using Bob's public key as the output
	// we created will be spending from Alice's commitment transaction.
	delayKey := TweakPubKey(bobKeyPub, commitPoint)

	// The commit tweak will be required in order for Bob to derive the
	// proper key need to spend the output.
	commitTweak := SingleTweakBytes(commitPoint, bobKeyPub)

	// Finally we'll generate the HTLC script itself that we'll be spending
	// from. The revocation clause can be claimed by Alice, while Bob can
	// sweep the output after a particular delay.
	htlcWitnessScript, err := SecondLevelHtlcScript(revocationKey,
		delayKey, claimDelay)
	if err != nil {
		t.Fatalf("unable to create htlc script: %v", err)
	}
	htlcPkScript, err := ScriptHashPkScript(htlcWitnessScript)
	if err != nil {
		t.Fatalf("unable to create htlc output: %v", err)
	}

	htlcOutput := &wire.TxOut{
		PkScript: htlcPkScript,
		Value:    int64(htlcAmt),
	}

	// TODO(roasbeef): make actually use timeout/success txns?

	// Finally, we'll create mock signers for both of them based on their
	// private keys. This test simplifies a bit and uses the same key as
	// the base point for all scripts and derivations.
	bobSigner := &MockSigner{Privkeys: []*secp256k1.PrivateKey{bobKeyPriv}}
	aliceSigner := &MockSigner{Privkeys: []*secp256k1.PrivateKey{aliceKeyPriv}}

	testCases := []struct {
		witness func() TxWitness
		valid   bool
	}{
		{
			// Sender of the HTLC attempts to activate the
			// revocation clause, but uses the wrong key (fails to
			// use the double tweak in this case).
			makeWitnessTestCase(t, func() (TxWitness, error) {
				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: aliceKeyPub,
					},
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return HtlcSpendRevoke(aliceSigner, signDesc,
					sweepTx)
			}),
			false,
		},
		{
			// Sender of HTLC activates the revocation clause.
			makeWitnessTestCase(t, func() (TxWitness, error) {
				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: aliceKeyPub,
					},
					DoubleTweak:   commitSecret,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return HtlcSpendRevoke(aliceSigner, signDesc,
					sweepTx)
			}),
			true,
		},
		{
			// Receiver of the HTLC attempts to sweep, but tries to
			// do so pre-maturely with a smaller CSV delay (2
			// blocks instead of 5 blocks).
			makeWitnessTestCase(t, func() (TxWitness, error) {
				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: bobKeyPub,
					},
					SingleTweak:   commitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return HtlcSpendSuccess(bobSigner, signDesc,
					sweepTx, claimDelay-3)
			}),
			false,
		},
		{
			// Receiver of the HTLC sweeps with the proper CSV
			// delay, but uses the wrong key (leaves off the single
			// tweak).
			makeWitnessTestCase(t, func() (TxWitness, error) {
				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: bobKeyPub,
					},
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return HtlcSpendSuccess(bobSigner, signDesc,
					sweepTx, claimDelay)
			}),
			false,
		},
		{
			// Receiver of the HTLC sweeps with the proper CSV
			// delay, and the correct key.
			makeWitnessTestCase(t, func() (TxWitness, error) {
				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: bobKeyPub,
					},
					SingleTweak:   commitTweak,
					WitnessScript: htlcWitnessScript,
					Output:        htlcOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return HtlcSpendSuccess(bobSigner, signDesc,
					sweepTx, claimDelay)
			}),
			true,
		},
	}

	for i, testCase := range testCases {
		sweepTx.TxIn[0].SignatureScript, err = WitnessStackToSigScript(testCase.witness())
		if err != nil {
			t.Fatalf("unable to convert witness stack to sigScript: %v", err)
		}

		newEngine := func() (*txscript.Engine, error) {
			return txscript.NewEngine(htlcPkScript,
				sweepTx, 0, ScriptVerifyFlags, scriptVersion,
				nil)
		}

		assertEngineExecution(t, i, testCase.valid, newEngine)
	}
}

// TestCommitSpendToRemoteConfirmed checks that the delayed version of the
// to_remote version can only be spent by the owner, and after one
// confirmation.
func TestCommitSpendToRemoteConfirmed(t *testing.T) {
	t.Parallel()

	const outputVal = dcrutil.Amount(2 * 10e8)

	aliceKeyPriv, aliceKeyPub := privKeyFromBytes(testWalletPrivKey)

	txid, err := chainhash.NewHash(testHdSeed.CloneBytes())
	if err != nil {
		t.Fatalf("unable to create txid: %v", err)
	}
	commitOut := &wire.OutPoint{
		Hash:  *txid,
		Index: 0,
	}
	commitScript, err := CommitScriptToRemoteConfirmed(aliceKeyPub)
	if err != nil {
		t.Fatalf("unable to create htlc script: %v", err)
	}
	commitPkScript, err := ScriptHashPkScript(commitScript)
	if err != nil {
		t.Fatalf("unable to create htlc output: %v", err)
	}

	commitOutput := &wire.TxOut{
		PkScript: commitPkScript,
		Value:    int64(outputVal),
		Version:  scriptVersion,
	}

	sweepTx := wire.NewMsgTx()
	sweepTx.Version = LNTxVersion
	sweepTx.AddTxIn(wire.NewTxIn(commitOut, 0, nil)) // TODO(decred) fill correct value
	sweepTx.AddTxOut(
		&wire.TxOut{
			PkScript: []byte("doesn't matter"),
			Value:    1 * 10e8,
			Version:  scriptVersion,
		},
	)

	aliceSigner := &MockSigner{Privkeys: []*secp256k1.PrivateKey{aliceKeyPriv}}

	testCases := []struct {
		witness func() TxWitness
		valid   bool
	}{
		{
			// Alice can spend after the a CSV delay has passed.
			makeWitnessTestCase(t, func() (TxWitness, error) {
				sweepTx.TxIn[0].Sequence = LockTimeToSequence(false, 1)

				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: aliceKeyPub,
					},
					WitnessScript: commitScript,
					Output:        commitOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return CommitSpendToRemoteConfirmed(aliceSigner, signDesc,
					sweepTx)
			}),
			true,
		},
		{
			// Alice cannot spend output without sequence set.
			makeWitnessTestCase(t, func() (TxWitness, error) {
				sweepTx.TxIn[0].Sequence = wire.MaxTxInSequenceNum

				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: aliceKeyPub,
					},
					WitnessScript: commitScript,
					Output:        commitOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return CommitSpendToRemoteConfirmed(aliceSigner, signDesc,
					sweepTx)
			}),
			false,
		},
	}

	for i, testCase := range testCases {
		sweepTx.TxIn[0].SignatureScript, err = WitnessStackToSigScript(testCase.witness())
		if err != nil {
			t.Fatal(err)
		}

		newEngine := func() (*txscript.Engine, error) {
			return txscript.NewEngine(commitPkScript,
				sweepTx, 0, ScriptVerifyFlags, scriptVersion,
				nil)
		}

		assertEngineExecution(t, i, testCase.valid, newEngine)
	}
}

// TestSpendAnchor checks that we can spend the anchors using the various spend
// paths.
func TestSpendAnchor(t *testing.T) {
	t.Parallel()

	const anchorSize = 294

	// First we'll set up some initial key state for Alice.
	aliceKeyPriv, aliceKeyPub := privKeyFromBytes(testWalletPrivKey)

	// Create a fake anchor outpoint that we'll use to generate the
	// sweeping transaction.
	txid, err := chainhash.NewHash(testHdSeed.CloneBytes())
	if err != nil {
		t.Fatalf("unable to create txid: %v", err)
	}
	anchorOutPoint := &wire.OutPoint{
		Hash:  *txid,
		Index: 0,
	}

	sweepTx := wire.NewMsgTx()
	sweepTx.Version = LNTxVersion
	sweepTx.AddTxIn(wire.NewTxIn(anchorOutPoint, 0, nil)) // TODO(decred) Pass in correct value
	sweepTx.AddTxOut(
		&wire.TxOut{
			PkScript: []byte("doesn't matter"),
			Value:    1 * 10e8,
		},
	)

	// Generate the anchor script that can be spent by Alice immediately,
	// or by anyone after 16 blocks.
	anchorScript, err := CommitScriptAnchor(aliceKeyPub)
	if err != nil {
		t.Fatalf("unable to create htlc script: %v", err)
	}
	anchorPkScript, err := ScriptHashPkScript(anchorScript)
	if err != nil {
		t.Fatalf("unable to create htlc output: %v", err)
	}

	anchorOutput := &wire.TxOut{
		PkScript: anchorPkScript,
		Value:    int64(anchorSize),
	}

	// Create mock signer for Alice.
	aliceSigner := &MockSigner{Privkeys: []*secp256k1.PrivateKey{aliceKeyPriv}}

	testCases := []struct {
		witness func() TxWitness
		valid   bool
	}{
		{
			// Alice can spend immediately.
			makeWitnessTestCase(t, func() (TxWitness, error) {
				sweepTx.TxIn[0].Sequence = wire.MaxTxInSequenceNum

				signDesc := &SignDescriptor{
					KeyDesc: keychain.KeyDescriptor{
						PubKey: aliceKeyPub,
					},
					WitnessScript: anchorScript,
					Output:        anchorOutput,
					HashType:      txscript.SigHashAll,
					InputIndex:    0,
				}

				return CommitSpendAnchor(aliceSigner, signDesc,
					sweepTx)
			}),
			true,
		},
		{
			// Anyone can spend after 16 blocks.
			makeWitnessTestCase(t, func() (TxWitness, error) {
				sweepTx.TxIn[0].Sequence = LockTimeToSequence(false, 16)
				return CommitSpendAnchorAnyone(anchorScript)
			}),
			true,
		},
		{
			// Anyone cannot spend before 16 blocks.
			makeWitnessTestCase(t, func() (TxWitness, error) {
				sweepTx.TxIn[0].Sequence = LockTimeToSequence(false, 15)
				return CommitSpendAnchorAnyone(anchorScript)
			}),
			false,
		},
	}

	for i, testCase := range testCases {
		sweepTx.TxIn[0].SignatureScript, err = WitnessStackToSigScript(testCase.witness())
		if err != nil {
			t.Fatal(err)
		}

		newEngine := func() (*txscript.Engine, error) {
			return txscript.NewEngine(anchorPkScript,
				sweepTx, 0, ScriptVerifyFlags, scriptVersion,
				nil)
		}

		assertEngineExecution(t, i, testCase.valid, newEngine)
	}
}

// TestSpecificationKeyDerivation implements the test vectors provided in
// BOLT-03, Appendix E.
func TestSpecificationKeyDerivation(t *testing.T) {
	const (
		baseSecretHex          = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
		perCommitmentSecretHex = "1f1e1d1c1b1a191817161514131211100f0e0d0c0b0a09080706050403020100"
		basePointHex           = "036d6caac248af96f6afa7f904f550253a0f3ef3f5aa2fe6838a95b216691468e2"
		perCommitmentPointHex  = "025f7117a78150fe2ef97db7cfc83bd57b2e2c0d0dd25eaf467a4a1c2a45ce1486"
	)

	baseSecret, err := privkeyFromHex(baseSecretHex)
	if err != nil {
		t.Fatalf("Failed to parse serialized privkey: %v", err)
	}
	perCommitmentSecret, err := privkeyFromHex(perCommitmentSecretHex)
	if err != nil {
		t.Fatalf("Failed to parse serialized privkey: %v", err)
	}
	basePoint, err := pubkeyFromHex(basePointHex)
	if err != nil {
		t.Fatalf("Failed to parse serialized pubkey: %v", err)
	}
	perCommitmentPoint, err := pubkeyFromHex(perCommitmentPointHex)
	if err != nil {
		t.Fatalf("Failed to parse serialized pubkey: %v", err)
	}

	// name: derivation of key from basepoint and per_commitment_point
	const expectedLocalKeyHex = "0235f2dbfaa89b57ec7b055afe29849ef7ddfeb1cefdb9ebdc43f5494984db29e5"
	actualLocalKey := TweakPubKey(basePoint, perCommitmentPoint)
	actualLocalKeyHex := pubkeyToHex(actualLocalKey)
	if actualLocalKeyHex != expectedLocalKeyHex {
		t.Errorf("Incorrect derivation of local public key: "+
			"expected %v, got %v", expectedLocalKeyHex, actualLocalKeyHex)
	}

	// name: derivation of secret key from basepoint secret and per_commitment_secret
	const expectedLocalPrivKeyHex = "cbced912d3b21bf196a766651e436aff192362621ce317704ea2f75d87e7be0f"
	tweak := SingleTweakBytes(perCommitmentPoint, basePoint)
	actualLocalPrivKey := TweakPrivKey(baseSecret, tweak)
	actualLocalPrivKeyHex := privkeyToHex(actualLocalPrivKey)
	if actualLocalPrivKeyHex != expectedLocalPrivKeyHex {
		t.Errorf("Incorrect derivation of local private key: "+
			"expected %v, got %v, %v", expectedLocalPrivKeyHex,
			actualLocalPrivKeyHex, hex.EncodeToString(tweak))
	}

	// name: derivation of revocation key from basepoint and per_commitment_point
	const expectedRevocationKeyHex = "02916e326636d19c33f13e8c0c3a03dd157f332f3e99c317c141dd865eb01f8ff0"
	actualRevocationKey := DeriveRevocationPubkey(basePoint, perCommitmentPoint)
	actualRevocationKeyHex := pubkeyToHex(actualRevocationKey)
	if actualRevocationKeyHex != expectedRevocationKeyHex {
		t.Errorf("Incorrect derivation of revocation public key: "+
			"expected %v, got %v", expectedRevocationKeyHex,
			actualRevocationKeyHex)
	}

	// name: derivation of revocation secret from basepoint_secret and per_commitment_secret
	const expectedRevocationPrivKeyHex = "d09ffff62ddb2297ab000cc85bcb4283fdeb6aa052affbc9dddcf33b61078110"
	actualRevocationPrivKey := DeriveRevocationPrivKey(baseSecret,
		perCommitmentSecret)
	actualRevocationPrivKeyHex := privkeyToHex(actualRevocationPrivKey)
	if actualRevocationPrivKeyHex != expectedRevocationPrivKeyHex {
		t.Errorf("Incorrect derivation of revocation private key: "+
			"expected %v, got %v", expectedRevocationPrivKeyHex,
			actualRevocationPrivKeyHex)
	}
}

// TestReplaceReceiverHtlcSpendRedeemPreimage tests whether the corresponding
// function works as intended.
func TestReplaceReceiverHtlcSpendRedeemPreimage(t *testing.T) {
	t.Parallel()

	// pushes generates nb data pushes of 1-byte length
	pushes := func(nb int) []byte {
		return bytes.Repeat([]byte{0x01, 0xff}, nb)
	}

	tests := []struct {
		name      string
		sigScript []byte
		preimage  []byte
		valid     bool
	}{
		{
			name:      "sigscript with few items fail",
			sigScript: pushes(3),
			preimage:  []byte{},
			valid:     false,
		},
		{
			name:      "sigscript with many items fail",
			sigScript: pushes(5),
			preimage:  []byte{},
			valid:     false,
		},
		{
			name:      "sigscript and preimage same size",
			sigScript: pushes(4),
			preimage:  []byte{0xee},
			valid:     true,
		},
		{
			name:      "sigscript and preimage different sizes",
			sigScript: pushes(4),
			preimage:  []byte{0xee, 0xdd},
			valid:     true,
		},
	}

	for _, tc := range tests {
		res, err := ReplaceReceiverHtlcSpendRedeemPreimage(tc.sigScript, tc.preimage)
		if err != nil && tc.valid {
			t.Errorf("case '%s' returned error when it should have been valid: %v",
				tc.name, err)
			continue
		}

		if err == nil && !tc.valid {
			t.Errorf("case '%s' should have returned error but passed", tc.name)
			continue
		}

		if err == nil {
			// Given we know all data pushes use 2 bytes (OP_DATA_1+0xff), we
			// can easily figure out where the preimage _should_ start.
			sigPreImage := res[5 : len(res)-2]
			if !bytes.Equal(sigPreImage, tc.preimage) {
				t.Errorf("case '%s' did not include a correct preimage: "+
					"preimage: %x return: %x\n", tc.name, tc.preimage, res)
			}
		}
	}
}
