// Copyright (c) 2013-2016 The btcsuite developers
// Copyright (c) 2015-2018 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/decred/dcrd/blockchain/stake"
	"github.com/decred/dcrd/chaincfg"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/database"
	"github.com/decred/dcrd/dcrutil"
	"github.com/decred/dcrd/txscript"
	"github.com/decred/dcrd/wire"
)

const (
	// MaxSigOpsPerBlock is the maximum number of signature operations
	// allowed for a block.  This really should be based upon the max
	// allowed block size for a network and any votes that might change it,
	// however, since it was not updated to be based upon it before
	// release, it will require a hard fork and associated vote agenda to
	// change it.  The original max block size for the protocol was 1MiB,
	// so that is what this is based on.
	MaxSigOpsPerBlock = 1000000 / 200

	// MaxTimeOffsetSeconds is the maximum number of seconds a block time
	// is allowed to be ahead of the current time.  This is currently 2
	// hours.
	MaxTimeOffsetSeconds = 2 * 60 * 60

	// MinCoinbaseScriptLen is the minimum length a coinbase script can be.
	MinCoinbaseScriptLen = 2

	// MaxCoinbaseScriptLen is the maximum length a coinbase script can be.
	MaxCoinbaseScriptLen = 100

	// medianTimeBlocks is the number of previous blocks which should be
	// used to calculate the median time used to validate block timestamps.
	medianTimeBlocks = 11

	// earlyVoteBitsValue is the only value of VoteBits allowed in a block
	// header before stake validation height.
	earlyVoteBitsValue = 0x0001

	// maxRevocationsPerBlock is the maximum number of revocations that are
	// allowed per block.
	maxRevocationsPerBlock = 255

	// A ticket commitment output is an OP_RETURN script with a 30-byte data
	// push that consists of a 20-byte hash for the payment hash, 8 bytes
	// for the amount to commit to (with the upper bit flag set to indicate
	// the hash is for a pay-to-script-hash address, otherwise the hash is a
	// pay-to-pubkey-hash), and 2 bytes for the fee limits.  Thus, 1 byte
	// for the OP_RETURN + 1 byte for the data push + 20 bytes for the
	// payment hash means the encoded amount is at offset 22.  Then, 8 bytes
	// for the amount means the encoded fee limits are at offset 30.
	commitHashStartIdx     = 2
	commitHashEndIdx       = commitHashStartIdx + 20
	commitAmountStartIdx   = commitHashEndIdx
	commitAmountEndIdx     = commitAmountStartIdx + 8
	commitFeeLimitStartIdx = commitAmountEndIdx
	commitFeeLimitEndIdx   = commitFeeLimitStartIdx + 2

	// commitP2SHFlag specifies the bitmask to apply to an amount decoded from
	// a ticket commitment in order to determine if it is a pay-to-script-hash
	// commitment.  The value is derived from the fact it is encoded as the most
	// significant bit in the amount.
	commitP2SHFlag = uint64(1 << 63)

	// submissionOutputIdx is the index of the stake submission output of a
	// ticket transaction.
	submissionOutputIdx = 0
)

var (
	// zeroHash is the zero value for a chainhash.Hash and is defined as a
	// package level variable to avoid the need to create a new instance
	// every time a check is needed.
	zeroHash = &chainhash.Hash{}

	// earlyFinalState is the only value of the final state allowed in a
	// block header before stake validation height.
	earlyFinalState = [6]byte{0x00}
)

// voteBitsApproveParent returns whether or not the passed vote bits indicate
// the regular transaction tree of the parent block should be considered valid.
func voteBitsApproveParent(voteBits uint16) bool {
	return dcrutil.IsFlagSet16(voteBits, dcrutil.BlockValid)
}

// headerApprovesParent returns whether or not the vote bits in the passed
// header indicate the regular transaction tree of the parent block should be
// considered valid.
func headerApprovesParent(header *wire.BlockHeader) bool {
	return voteBitsApproveParent(header.VoteBits)
}

// isNullOutpoint determines whether or not a previous transaction output point
// is set.
func isNullOutpoint(outpoint *wire.OutPoint) bool {
	if outpoint.Index == math.MaxUint32 &&
		outpoint.Hash.IsEqual(zeroHash) &&
		outpoint.Tree == wire.TxTreeRegular {
		return true
	}
	return false
}

// isNullFraudProof determines whether or not a previous transaction fraud
// proof is set.
func isNullFraudProof(txIn *wire.TxIn) bool {
	switch {
	case txIn.BlockHeight != wire.NullBlockHeight:
		return false
	case txIn.BlockIndex != wire.NullBlockIndex:
		return false
	}

	return true
}

// IsCoinBaseTx determines whether or not a transaction is a coinbase.  A
// coinbase is a special transaction created by miners that has no inputs.
// This is represented in the block chain by a transaction with a single input
// that has a previous output transaction index set to the maximum value along
// with a zero hash.
//
// This function only differs from IsCoinBase in that it works with a raw wire
// transaction as opposed to a higher level util transaction.
func IsCoinBaseTx(msgTx *wire.MsgTx) bool {
	// A coin base must only have one transaction input.
	if len(msgTx.TxIn) != 1 {
		return false
	}

	// The previous output of a coin base must have a max value index and a
	// zero hash.
	prevOut := &msgTx.TxIn[0].PreviousOutPoint
	if prevOut.Index != math.MaxUint32 || !prevOut.Hash.IsEqual(zeroHash) {
		return false
	}

	return true
}

// IsCoinBase determines whether or not a transaction is a coinbase.  A
// coinbase is a special transaction created by miners that has no inputs.
// This is represented in the block chain by a transaction with a single input
// that has a previous output transaction index set to the maximum value along
// with a zero hash.
//
// This function only differs from IsCoinBaseTx in that it works with a higher
// level util transaction as opposed to a raw wire transaction.
func IsCoinBase(tx *dcrutil.Tx) bool {
	return IsCoinBaseTx(tx.MsgTx())
}

// IsExpiredTx returns where or not the passed transaction is expired according
// to the given block height.
//
// This function only differs from IsExpired in that it works with a raw wire
// transaction as opposed to a higher level util transaction.
func IsExpiredTx(tx *wire.MsgTx, blockHeight int64) bool {
	expiry := tx.Expiry
	return expiry != wire.NoExpiryValue && blockHeight >= int64(expiry)
}

// IsExpired returns where or not the passed transaction is expired according to
// the given block height.
//
// This function only differs from IsExpiredTx in that it works with a higher
// level util transaction as opposed to a raw wire transaction.
func IsExpired(tx *dcrutil.Tx, blockHeight int64) bool {
	return IsExpiredTx(tx.MsgTx(), blockHeight)
}

// SequenceLockActive determines if all of the inputs to a given transaction
// have achieved a relative age that surpasses the requirements specified by
// their respective sequence locks as calculated by CalcSequenceLock.  A single
// sequence lock is sufficient because the calculated lock selects the minimum
// required time and block height from all of the non-disabled inputs after
// which the transaction can be included.
func SequenceLockActive(lock *SequenceLock, blockHeight int64, medianTime time.Time) bool {
	// The transaction is not yet mature if it has not yet reached the
	// required minimum time and block height according to its sequence
	// locks.
	if blockHeight <= lock.MinHeight || medianTime.Unix() <= lock.MinTime {
		return false
	}

	return true
}

// IsFinalizedTransaction determines whether or not a transaction is finalized.
func IsFinalizedTransaction(tx *dcrutil.Tx, blockHeight int64, blockTime time.Time) bool {
	// Lock time of zero means the transaction is finalized.
	msgTx := tx.MsgTx()
	lockTime := msgTx.LockTime
	if lockTime == 0 {
		return true
	}

	// The lock time field of a transaction is either a block height at
	// which the transaction is finalized or a timestamp depending on if the
	// value is before the txscript.LockTimeThreshold.  When it is under the
	// threshold it is a block height.
	var blockTimeOrHeight int64
	if lockTime < txscript.LockTimeThreshold {
		blockTimeOrHeight = blockHeight
	} else {
		blockTimeOrHeight = blockTime.Unix()
	}
	if int64(lockTime) < blockTimeOrHeight {
		return true
	}

	// At this point, the transaction's lock time hasn't occurred yet, but
	// the transaction might still be finalized if the sequence number
	// for all transaction inputs is maxed out.
	for _, txIn := range msgTx.TxIn {
		if txIn.Sequence != math.MaxUint32 {
			return false
		}
	}
	return true
}

// CheckTransactionSanity performs some preliminary checks on a transaction to
// ensure it is sane.  These checks are context free.
func CheckTransactionSanity(tx *wire.MsgTx, params *chaincfg.Params) error {
	// A transaction must have at least one input.
	if len(tx.TxIn) == 0 {
		return ruleError(ErrNoTxInputs, "transaction has no inputs")
	}

	// A transaction must have at least one output.
	if len(tx.TxOut) == 0 {
		return ruleError(ErrNoTxOutputs, "transaction has no outputs")
	}

	// A transaction must not exceed the maximum allowed size when
	// serialized.
	serializedTxSize := tx.SerializeSize()
	if serializedTxSize > params.MaxTxSize {
		str := fmt.Sprintf("serialized transaction is too big - got "+
			"%d, max %d", serializedTxSize, params.MaxTxSize)
		return ruleError(ErrTxTooBig, str)
	}

	// Coinbase script length must be between min and max length.
	isVote := stake.IsSSGen(tx)
	if IsCoinBaseTx(tx) {
		// The referenced outpoint must be null.
		if !isNullOutpoint(&tx.TxIn[0].PreviousOutPoint) {
			str := fmt.Sprintf("coinbase transaction did not use " +
				"a null outpoint")
			return ruleError(ErrBadCoinbaseOutpoint, str)
		}

		// The fraud proof must also be null.
		if !isNullFraudProof(tx.TxIn[0]) {
			str := fmt.Sprintf("coinbase transaction fraud proof " +
				"was non-null")
			return ruleError(ErrBadCoinbaseFraudProof, str)
		}

		slen := len(tx.TxIn[0].SignatureScript)
		if slen < MinCoinbaseScriptLen || slen > MaxCoinbaseScriptLen {
			str := fmt.Sprintf("coinbase transaction script "+
				"length of %d is out of range (min: %d, max: "+
				"%d)", slen, MinCoinbaseScriptLen,
				MaxCoinbaseScriptLen)
			return ruleError(ErrBadCoinbaseScriptLen, str)
		}
	} else if isVote {
		// Check script length of stake base signature.
		slen := len(tx.TxIn[0].SignatureScript)
		if slen < MinCoinbaseScriptLen || slen > MaxCoinbaseScriptLen {
			str := fmt.Sprintf("stakebase transaction script "+
				"length of %d is out of range (min: %d, max: "+
				"%d)", slen, MinCoinbaseScriptLen,
				MaxCoinbaseScriptLen)
			return ruleError(ErrBadStakebaseScriptLen, str)
		}

		// The script must be set to the one specified by the network.
		// Check script length of stake base signature.
		if !bytes.Equal(tx.TxIn[0].SignatureScript,
			params.StakeBaseSigScript) {
			str := fmt.Sprintf("stakebase transaction signature "+
				"script was set to disallowed value (got %x, "+
				"want %x)", tx.TxIn[0].SignatureScript,
				params.StakeBaseSigScript)
			return ruleError(ErrBadStakebaseScrVal, str)
		}

		// The ticket reference hash in an SSGen tx must not be null.
		ticketHash := &tx.TxIn[1].PreviousOutPoint
		if isNullOutpoint(ticketHash) {
			return ruleError(ErrBadTxInput, "ssgen tx ticket input"+
				" refers to previous output that is null")
		}
	} else {
		// Previous transaction outputs referenced by the inputs to
		// this transaction must not be null except in the case of
		// stakebases for votes.
		for _, txIn := range tx.TxIn {
			prevOut := &txIn.PreviousOutPoint
			if isNullOutpoint(prevOut) {
				return ruleError(ErrBadTxInput, "transaction "+
					"input refers to previous output that "+
					"is null")
			}
		}
	}

	// Ensure the transaction amounts are in range.  Each transaction output
	// must not be negative or more than the max allowed per transaction.  Also,
	// the total of all outputs must abide by the same restrictions.  All
	// amounts in a transaction are in a unit value known as an atom.  One
	// Decred is a quantity of atoms as defined by the AtomsPerCoin constant.
	//
	// Also ensure that non-stake transaction output scripts do not contain any
	// stake opcodes.
	isTicket := !isVote && stake.IsSStx(tx)
	isRevocation := !isVote && !isTicket && stake.IsSSRtx(tx)
	isStakeTx := isVote || isTicket || isRevocation
	var totalAtom int64
	for txOutIdx, txOut := range tx.TxOut {
		atom := txOut.Value
		if atom < 0 {
			str := fmt.Sprintf("transaction output has negative value of %v",
				atom)
			return ruleError(ErrBadTxOutValue, str)
		}
		if atom > dcrutil.MaxAmount {
			str := fmt.Sprintf("transaction output value of %v is higher than "+
				"max allowed value of %v", atom, dcrutil.MaxAmount)
			return ruleError(ErrBadTxOutValue, str)
		}

		// Two's complement int64 overflow guarantees that any overflow is
		// detected and reported.  This is impossible for Decred, but perhaps
		// possible if an alt increases the total money supply.
		totalAtom += atom
		if totalAtom < 0 {
			str := fmt.Sprintf("total value of all transaction outputs "+
				"exceeds max allowed value of %v", dcrutil.MaxAmount)
			return ruleError(ErrBadTxOutValue, str)
		}
		if totalAtom > dcrutil.MaxAmount {
			str := fmt.Sprintf("total value of all transaction outputs is %v "+
				"which is higher than max allowed value of %v", totalAtom,
				dcrutil.MaxAmount)
			return ruleError(ErrBadTxOutValue, str)
		}

		// Ensure that non-stake transactions have no outputs with opcodes
		// OP_SSTX, OP_SSRTX, OP_SSGEN, or OP_SSTX_CHANGE.
		if !isStakeTx {
			hasOp, err := txscript.ContainsStakeOpCodes(txOut.PkScript)
			if err != nil {
				return ruleError(ErrScriptMalformed, err.Error())
			}
			if hasOp {
				str := fmt.Sprintf("non-stake transaction output %d contains "+
					"stake opcode", txOutIdx)
				return ruleError(ErrRegTxCreateStakeOut, str)
			}
		}
	}

	// Check for duplicate transaction inputs.
	existingTxOut := make(map[wire.OutPoint]struct{})
	for _, txIn := range tx.TxIn {
		if _, exists := existingTxOut[txIn.PreviousOutPoint]; exists {
			return ruleError(ErrDuplicateTxInputs, "transaction "+
				"contains duplicate inputs")
		}
		existingTxOut[txIn.PreviousOutPoint] = struct{}{}
	}

	return nil
}

// checkProofOfStake ensures that all ticket purchases in the block pay at least
// the amount required by the block header stake bits which indicate the target
// stake difficulty (aka ticket price) as claimed.
func checkProofOfStake(block *dcrutil.Block, posLimit int64) error {
	msgBlock := block.MsgBlock()
	for _, staketx := range block.STransactions() {
		msgTx := staketx.MsgTx()
		if stake.IsSStx(msgTx) {
			commitValue := msgTx.TxOut[0].Value

			// Check for underflow block sbits.
			if commitValue < msgBlock.Header.SBits {
				errStr := fmt.Sprintf("Stake tx %v has a "+
					"commitment value less than the "+
					"minimum stake difficulty specified in"+
					" the block (%v)", staketx.Hash(),
					msgBlock.Header.SBits)
				return ruleError(ErrNotEnoughStake, errStr)
			}

			// Check if it's above the PoS limit.
			if commitValue < posLimit {
				errStr := fmt.Sprintf("Stake tx %v has a "+
					"commitment value less than the "+
					"minimum stake difficulty for the "+
					"network (%v)", staketx.Hash(),
					posLimit)
				return ruleError(ErrStakeBelowMinimum, errStr)
			}
		}
	}

	return nil
}

// CheckProofOfStake ensures that all ticket purchases in the block pay at least
// the amount required by the block header stake bits which indicate the target
// stake difficulty (aka ticket price) as claimed.
func CheckProofOfStake(block *dcrutil.Block, posLimit int64) error {
	return checkProofOfStake(block, posLimit)
}

// checkProofOfWork ensures the block header bits which indicate the target
// difficulty is in min/max range and that the block hash is less than the
// target difficulty as claimed.
//
// The flags modify the behavior of this function as follows:
//  - BFNoPoWCheck: The check to ensure the block hash is less than the target
//    difficulty is not performed.
func checkProofOfWork(header *wire.BlockHeader, powLimit *big.Int, flags BehaviorFlags) error {
	// The target difficulty must be larger than zero.
	target := CompactToBig(header.Bits)
	if target.Sign() <= 0 {
		str := fmt.Sprintf("block target difficulty of %064x is too "+
			"low", target)
		return ruleError(ErrUnexpectedDifficulty, str)
	}

	// The target difficulty must be less than the maximum allowed.
	if target.Cmp(powLimit) > 0 {
		str := fmt.Sprintf("block target difficulty of %064x is "+
			"higher than max of %064x", target, powLimit)
		return ruleError(ErrUnexpectedDifficulty, str)
	}

	// The block hash must be less than the claimed target unless the flag
	// to avoid proof of work checks is set.
	if flags&BFNoPoWCheck != BFNoPoWCheck {
		// The block hash must be less than the claimed target.
		hash := header.BlockHash()
		hashNum := HashToBig(&hash)
		if hashNum.Cmp(target) > 0 {
			str := fmt.Sprintf("block hash of %064x is higher than"+
				" expected max of %064x", hashNum, target)
			return ruleError(ErrHighHash, str)
		}
	}

	return nil
}

// CheckProofOfWork ensures the block header bits which indicate the target
// difficulty is in min/max range and that the block hash is less than the
// target difficulty as claimed.
func CheckProofOfWork(header *wire.BlockHeader, powLimit *big.Int) error {
	return checkProofOfWork(header, powLimit, BFNone)
}

// checkBlockHeaderSanity performs some preliminary checks on a block header to
// ensure it is sane before continuing with processing.  These checks are
// context free.
//
// The flags do not modify the behavior of this function directly, however they
// are needed to pass along to checkProofOfWork.
func checkBlockHeaderSanity(header *wire.BlockHeader, timeSource MedianTimeSource, flags BehaviorFlags, chainParams *chaincfg.Params) error {
	// The stake validation height should always be at least stake enabled
	// height, so assert it because the code below relies on that assumption.
	stakeValidationHeight := uint32(chainParams.StakeValidationHeight)
	stakeEnabledHeight := uint32(chainParams.StakeEnabledHeight)
	if stakeEnabledHeight > stakeValidationHeight {
		return AssertError(fmt.Sprintf("checkBlockHeaderSanity called "+
			"with stake enabled height %d after stake validation "+
			"height %d", stakeEnabledHeight, stakeValidationHeight))
	}

	// Ensure the proof of work bits in the block header is in min/max
	// range and the block hash is less than the target value described by
	// the bits.
	err := checkProofOfWork(header, chainParams.PowLimit, flags)
	if err != nil {
		return err
	}

	// A block timestamp must not have a greater precision than one second.
	// This check is necessary because Go time.Time values support
	// nanosecond precision whereas the consensus rules only apply to
	// seconds and it's much nicer to deal with standard Go time values
	// instead of converting to seconds everywhere.
	if !header.Timestamp.Equal(time.Unix(header.Timestamp.Unix(), 0)) {
		str := fmt.Sprintf("block timestamp of %v has a higher "+
			"precision than one second", header.Timestamp)
		return ruleError(ErrInvalidTime, str)
	}

	// Ensure the block time is not too far in the future.
	maxTimestamp := timeSource.AdjustedTime().Add(time.Second *
		MaxTimeOffsetSeconds)
	if header.Timestamp.After(maxTimestamp) {
		str := fmt.Sprintf("block timestamp of %v is too far in the "+
			"future", header.Timestamp)
		return ruleError(ErrTimeTooNew, str)
	}

	// A block must not contain any votes or revocations, its vote bits
	// must be 0x0001, and its final state must be all zeroes before
	// stake validation begins.
	if header.Height < stakeValidationHeight {
		if header.Voters > 0 {
			errStr := fmt.Sprintf("block at height %d commits to "+
				"%d votes before stake validation height %d",
				header.Height, header.Voters,
				stakeValidationHeight)
			return ruleError(ErrInvalidEarlyStakeTx, errStr)
		}

		if header.Revocations > 0 {
			errStr := fmt.Sprintf("block at height %d commits to "+
				"%d revocations before stake validation height %d",
				header.Height, header.Revocations,
				stakeValidationHeight)
			return ruleError(ErrInvalidEarlyStakeTx, errStr)
		}

		if header.VoteBits != earlyVoteBitsValue {
			errStr := fmt.Sprintf("block at height %d commits to "+
				"invalid vote bits before stake validation "+
				"height %d (expected %x, got %x)",
				header.Height, stakeValidationHeight,
				earlyVoteBitsValue, header.VoteBits)
			return ruleError(ErrInvalidEarlyVoteBits, errStr)
		}

		if header.FinalState != earlyFinalState {
			errStr := fmt.Sprintf("block at height %d commits to "+
				"invalid final state before stake validation "+
				"height %d (expected %x, got %x)",
				header.Height, stakeValidationHeight,
				earlyFinalState, header.FinalState)
			return ruleError(ErrInvalidEarlyFinalState, errStr)
		}
	}

	// A block must not contain fewer votes than the minimum required to
	// reach majority once stake validation height has been reached.
	if header.Height >= stakeValidationHeight {
		majority := (chainParams.TicketsPerBlock / 2) + 1
		if header.Voters < majority {
			errStr := fmt.Sprintf("block does not commit to enough "+
				"votes (min: %d, got %d)", majority,
				header.Voters)
			return ruleError(ErrNotEnoughVotes, errStr)
		}
	}

	// The block header must not claim to contain more votes than the
	// maximum allowed.
	if header.Voters > chainParams.TicketsPerBlock {
		errStr := fmt.Sprintf("block commits to too many votes (max: "+
			"%d, got %d)", chainParams.TicketsPerBlock, header.Voters)
		return ruleError(ErrTooManyVotes, errStr)
	}

	// The block must not contain more ticket purchases than the maximum
	// allowed.
	if header.FreshStake > chainParams.MaxFreshStakePerBlock {
		errStr := fmt.Sprintf("block commits to too many ticket "+
			"purchases (max: %d, got %d)",
			chainParams.MaxFreshStakePerBlock, header.FreshStake)
		return ruleError(ErrTooManySStxs, errStr)
	}

	return nil
}

// checkBlockSanity performs some preliminary checks on a block to ensure it is
// sane before continuing with block processing.  These checks are context
// free.
//
// The flags do not modify the behavior of this function directly, however they
// are needed to pass along to checkBlockHeaderSanity.
func checkBlockSanity(block *dcrutil.Block, timeSource MedianTimeSource, flags BehaviorFlags, chainParams *chaincfg.Params) error {
	msgBlock := block.MsgBlock()
	header := &msgBlock.Header
	err := checkBlockHeaderSanity(header, timeSource, flags, chainParams)
	if err != nil {
		return err
	}

	// All ticket purchases must meet the difficulty specified by the block
	// header.
	err = checkProofOfStake(block, chainParams.MinimumStakeDiff)
	if err != nil {
		return err
	}

	// A block must have at least one regular transaction.
	numTx := len(msgBlock.Transactions)
	if numTx == 0 {
		return ruleError(ErrNoTransactions, "block does not contain "+
			"any transactions")
	}

	// A block must not exceed the maximum allowed block payload when
	// serialized.
	//
	// This is a quick and context-free sanity check of the maximum block
	// size according to the wire protocol.  Even though the wire protocol
	// already prevents blocks bigger than this limit, there are other
	// methods of receiving a block that might not have been checked
	// already.  A separate block size is enforced later that takes into
	// account the network-specific block size and the results of block
	// size votes.  Typically that block size is more restrictive than this
	// one.
	serializedSize := msgBlock.SerializeSize()
	if serializedSize > wire.MaxBlockPayload {
		str := fmt.Sprintf("serialized block is too big - got %d, "+
			"max %d", serializedSize, wire.MaxBlockPayload)
		return ruleError(ErrBlockTooBig, str)
	}
	if header.Size != uint32(serializedSize) {
		str := fmt.Sprintf("serialized block is not size indicated in "+
			"header - got %d, expected %d", header.Size,
			serializedSize)
		return ruleError(ErrWrongBlockSize, str)
	}

	// The first transaction in a block's regular tree must be a coinbase.
	transactions := block.Transactions()
	if !IsCoinBaseTx(transactions[0].MsgTx()) {
		return ruleError(ErrFirstTxNotCoinbase, "first transaction in "+
			"block is not a coinbase")
	}

	// A block must not have more than one coinbase.
	for i, tx := range transactions[1:] {
		if IsCoinBaseTx(tx.MsgTx()) {
			str := fmt.Sprintf("block contains second coinbase at "+
				"index %d", i+1)
			return ruleError(ErrMultipleCoinbases, str)
		}
	}

	// Do some preliminary checks on each regular transaction to ensure they
	// are sane before continuing.
	for i, tx := range transactions {
		// A block must not have stake transactions in the regular
		// transaction tree.
		msgTx := tx.MsgTx()
		txType := stake.DetermineTxType(msgTx)
		if txType != stake.TxTypeRegular {
			errStr := fmt.Sprintf("block contains a stake "+
				"transaction in the regular transaction tree at "+
				"index %d", i)
			return ruleError(ErrStakeTxInRegularTree, errStr)
		}

		err := CheckTransactionSanity(msgTx, chainParams)
		if err != nil {
			return err
		}
	}

	// Do some preliminary checks on each stake transaction to ensure they
	// are sane while tallying each type before continuing.
	stakeValidationHeight := uint32(chainParams.StakeValidationHeight)
	var totalTickets, totalVotes, totalRevocations int64
	var totalYesVotes int64
	for txIdx, stx := range msgBlock.STransactions {
		err := CheckTransactionSanity(stx, chainParams)
		if err != nil {
			return err
		}

		// A block must not have regular transactions in the stake
		// transaction tree.
		txType := stake.DetermineTxType(stx)
		if txType == stake.TxTypeRegular {
			errStr := fmt.Sprintf("block contains regular "+
				"transaction in stake transaction tree at "+
				"index %d", txIdx)
			return ruleError(ErrRegTxInStakeTree, errStr)
		}

		switch txType {
		case stake.TxTypeSStx:
			totalTickets++

		case stake.TxTypeSSGen:
			totalVotes++

			// All votes in a block must commit to the parent of the
			// block once stake validation height has been reached.
			if header.Height >= stakeValidationHeight {
				votedHash, votedHeight := stake.SSGenBlockVotedOn(stx)
				if (votedHash != header.PrevBlock) || (votedHeight !=
					header.Height-1) {

					errStr := fmt.Sprintf("vote %s at index %d is "+
						"for parent block %s (height %d) versus "+
						"expected parent block %s (height %d)",
						stx.TxHash(), txIdx, votedHash,
						votedHeight, header.PrevBlock,
						header.Height-1)
					return ruleError(ErrVotesOnWrongBlock, errStr)
				}

				// Tally how many votes approve the previous block for use
				// when validating the header commitment.
				if voteBitsApproveParent(stake.SSGenVoteBits(stx)) {
					totalYesVotes++
				}
			}

		case stake.TxTypeSSRtx:
			totalRevocations++
		}
	}

	// A block must not contain more than the maximum allowed number of
	// revocations.
	if totalRevocations > maxRevocationsPerBlock {
		errStr := fmt.Sprintf("block contains %d revocations which "+
			"exceeds the maximum allowed amount of %d",
			totalRevocations, maxRevocationsPerBlock)
		return ruleError(ErrTooManyRevocations, errStr)
	}

	// A block must only contain stake transactions of the the allowed
	// types.
	//
	// NOTE: This is not possible to hit at the time this comment was
	// written because all transactions which are not specifically one of
	// the recognized stake transaction forms are considered regular
	// transactions and those are rejected above.  However, if a new stake
	// transaction type is added, that implicit condition would no longer
	// hold and therefore an explicit check is performed here.
	numStakeTx := int64(len(msgBlock.STransactions))
	calcStakeTx := totalTickets + totalVotes + totalRevocations
	if numStakeTx != calcStakeTx {
		errStr := fmt.Sprintf("block contains an unexpected number "+
			"of stake transactions (contains %d, expected %d)",
			numStakeTx, calcStakeTx)
		return ruleError(ErrNonstandardStakeTx, errStr)
	}

	// A block header must commit to the actual number of tickets purchases that
	// are in the block.
	if int64(header.FreshStake) != totalTickets {
		errStr := fmt.Sprintf("block header commitment to %d ticket "+
			"purchases does not match %d contained in the block",
			header.FreshStake, totalTickets)
		return ruleError(ErrFreshStakeMismatch, errStr)
	}

	// A block header must commit to the the actual number of votes that are
	// in the block.
	if int64(header.Voters) != totalVotes {
		errStr := fmt.Sprintf("block header commitment to %d votes "+
			"does not match %d contained in the block",
			header.Voters, totalVotes)
		return ruleError(ErrVotesMismatch, errStr)
	}

	// A block header must commit to the actual number of revocations that
	// are in the block.
	if int64(header.Revocations) != totalRevocations {
		errStr := fmt.Sprintf("block header commitment to %d revocations "+
			"does not match %d contained in the block",
			header.Revocations, totalRevocations)
		return ruleError(ErrRevocationsMismatch, errStr)
	}

	// A block header must commit to the same previous block acceptance
	// semantics expressed by the votes once stake validation height has
	// been reached.
	if header.Height >= stakeValidationHeight {
		totalNoVotes := totalVotes - totalYesVotes
		headerApproves := headerApprovesParent(header)
		votesApprove := totalYesVotes > totalNoVotes
		if headerApproves != votesApprove {
			errStr := fmt.Sprintf("block header commitment to previous "+
				"block approval does not match votes (header claims: %v, "+
				"votes: %v)", headerApproves, votesApprove)
			return ruleError(ErrIncongruentVotebit, errStr)
		}
	}

	// A block must not contain anything other than ticket purchases prior to
	// stake validation height.
	//
	// NOTE: This case is impossible to hit at this point at the time this
	// comment was written since the votes and revocations have already been
	// proven to be zero before stake validation height and the only other
	// type at the current time is ticket purchases, however, if another
	// stake type is ever added, consensus would break without this check.
	// It's better to be safe and it's a cheap check.
	if header.Height < stakeValidationHeight {
		if int64(len(msgBlock.STransactions)) != totalTickets {
			errStr := fmt.Sprintf("block contains stake "+
				"transactions other than ticket purchases before "+
				"stake validation height %d (total: %d, expected %d)",
				uint32(chainParams.StakeValidationHeight),
				len(msgBlock.STransactions), header.FreshStake)
			return ruleError(ErrInvalidEarlyStakeTx, errStr)
		}
	}

	// Build merkle tree and ensure the calculated merkle root matches the
	// entry in the block header.  This also has the effect of caching all
	// of the transaction hashes in the block to speed up future hash
	// checks.  Bitcoind builds the tree here and checks the merkle root
	// after the following checks, but there is no reason not to check the
	// merkle root matches here.
	merkles := BuildMerkleTreeStore(block.Transactions())
	calculatedMerkleRoot := merkles[len(merkles)-1]
	if !header.MerkleRoot.IsEqual(calculatedMerkleRoot) {
		str := fmt.Sprintf("block merkle root is invalid - block "+
			"header indicates %v, but calculated value is %v",
			header.MerkleRoot, calculatedMerkleRoot)
		return ruleError(ErrBadMerkleRoot, str)
	}

	// Build the stake tx tree merkle root too and check it.
	merkleStake := BuildMerkleTreeStore(block.STransactions())
	calculatedStakeMerkleRoot := merkleStake[len(merkleStake)-1]
	if !header.StakeRoot.IsEqual(calculatedStakeMerkleRoot) {
		str := fmt.Sprintf("block stake merkle root is invalid - block"+
			" header indicates %v, but calculated value is %v",
			header.StakeRoot, calculatedStakeMerkleRoot)
		return ruleError(ErrBadMerkleRoot, str)
	}

	// Check for duplicate transactions.  This check will be fairly quick
	// since the transaction hashes are already cached due to building the
	// merkle trees above.
	existingTxHashes := make(map[chainhash.Hash]struct{})
	stakeTransactions := block.STransactions()
	allTransactions := append(transactions, stakeTransactions...)

	for _, tx := range allTransactions {
		hash := tx.Hash()
		if _, exists := existingTxHashes[*hash]; exists {
			str := fmt.Sprintf("block contains duplicate "+
				"transaction %v", hash)
			return ruleError(ErrDuplicateTx, str)
		}
		existingTxHashes[*hash] = struct{}{}
	}

	// The number of signature operations must be less than the maximum
	// allowed per block.
	totalSigOps := 0
	for _, tx := range allTransactions {
		// We could potentially overflow the accumulator so check for
		// overflow.
		lastSigOps := totalSigOps

		msgTx := tx.MsgTx()
		isCoinBase := IsCoinBaseTx(msgTx)
		isSSGen := stake.IsSSGen(msgTx)
		totalSigOps += CountSigOps(tx, isCoinBase, isSSGen)
		if totalSigOps < lastSigOps || totalSigOps > MaxSigOpsPerBlock {
			str := fmt.Sprintf("block contains too many signature "+
				"operations - got %v, max %v", totalSigOps,
				MaxSigOpsPerBlock)
			return ruleError(ErrTooManySigOps, str)
		}
	}

	return nil
}

// CheckBlockSanity performs some preliminary checks on a block to ensure it is
// sane before continuing with block processing.  These checks are context
// free.
func CheckBlockSanity(block *dcrutil.Block, timeSource MedianTimeSource, chainParams *chaincfg.Params) error {
	return checkBlockSanity(block, timeSource, BFNone, chainParams)
}

// checkBlockHeaderPositional performs several validation checks on the block
// header which depend on its position within the block chain and having the
// headers of all ancestors available.  These checks do not, and must not, rely
// on having the full block data of all ancestors available.
//
// The flags modify the behavior of this function as follows:
//  - BFFastAdd: All checks except those involving comparing the header against
//    the checkpoints and expected height are not performed.
//
// This function MUST be called with the chain state lock held (for reads).
func (b *BlockChain) checkBlockHeaderPositional(header *wire.BlockHeader, prevNode *blockNode, flags BehaviorFlags) error {
	// The genesis block is valid by definition.
	if prevNode == nil {
		return nil
	}

	fastAdd := flags&BFFastAdd == BFFastAdd
	if !fastAdd {
		// Ensure the difficulty specified in the block header matches
		// the calculated difficulty based on the previous block and
		// difficulty retarget rules.
		expDiff, err := b.calcNextRequiredDifficulty(prevNode,
			header.Timestamp)
		if err != nil {
			return err
		}
		blockDifficulty := header.Bits
		if blockDifficulty != expDiff {
			str := fmt.Sprintf("block difficulty of %d is not the"+
				" expected value of %d", blockDifficulty,
				expDiff)
			return ruleError(ErrUnexpectedDifficulty, str)
		}

		// Ensure the timestamp for the block header is after the
		// median time of the last several blocks (medianTimeBlocks).
		medianTime := prevNode.CalcPastMedianTime()
		if !header.Timestamp.After(medianTime) {
			str := "block timestamp of %v is not after expected %v"
			str = fmt.Sprintf(str, header.Timestamp, medianTime)
			return ruleError(ErrTimeTooOld, str)
		}
	}

	// The height of this block is one more than the referenced previous
	// block.
	blockHeight := prevNode.height + 1

	// Ensure the header commits to the correct height based on the height it
	// actually connects in the blockchain.
	if int64(header.Height) != blockHeight {
		errStr := fmt.Sprintf("block header commitment to height %d "+
			"does not match chain height %d", header.Height,
			blockHeight)
		return ruleError(ErrBadBlockHeight, errStr)
	}

	// Ensure chain matches up to predetermined checkpoints.
	blockHash := header.BlockHash()
	if !b.verifyCheckpoint(blockHeight, &blockHash) {
		str := fmt.Sprintf("block at height %d does not match "+
			"checkpoint hash", blockHeight)
		return ruleError(ErrBadCheckpoint, str)
	}

	// Find the previous checkpoint and prevent blocks which fork the main
	// chain before it.  This prevents storage of new, otherwise valid,
	// blocks which build off of old blocks that are likely at a much easier
	// difficulty and therefore could be used to waste cache and disk space.
	checkpointNode, err := b.findPreviousCheckpoint()
	if err != nil {
		return err
	}
	if checkpointNode != nil && blockHeight < checkpointNode.height {
		str := fmt.Sprintf("block at height %d forks the main chain "+
			"before the previous checkpoint at height %d",
			blockHeight, checkpointNode.height)
		return ruleError(ErrForkTooOld, str)
	}

	if !fastAdd {
		// Reject version 5 blocks for networks other than the main
		// network once a majority of the network has upgraded.
		if b.chainParams.Net != wire.MainNet && header.Version < 6 &&
			b.isMajorityVersion(6, prevNode, b.chainParams.BlockRejectNumRequired) {

			str := "new blocks with version %d are no longer valid"
			str = fmt.Sprintf(str, header.Version)
			return ruleError(ErrBlockVersionTooOld, str)
		}

		// Reject version 4 blocks once a majority of the network has
		// upgraded.
		if header.Version < 5 && b.isMajorityVersion(5, prevNode,
			b.chainParams.BlockRejectNumRequired) {

			str := "new blocks with version %d are no longer valid"
			str = fmt.Sprintf(str, header.Version)
			return ruleError(ErrBlockVersionTooOld, str)
		}

		// Reject version 3 blocks once a majority of the network has
		// upgraded.
		if header.Version < 4 && b.isMajorityVersion(4, prevNode,
			b.chainParams.BlockRejectNumRequired) {

			str := "new blocks with version %d are no longer valid"
			str = fmt.Sprintf(str, header.Version)
			return ruleError(ErrBlockVersionTooOld, str)
		}

		// Reject version 2 blocks once a majority of the network has
		// upgraded.
		if header.Version < 3 && b.isMajorityVersion(3, prevNode,
			b.chainParams.BlockRejectNumRequired) {

			str := "new blocks with version %d are no longer valid"
			str = fmt.Sprintf(str, header.Version)
			return ruleError(ErrBlockVersionTooOld, str)
		}

		// Reject version 1 blocks once a majority of the network has
		// upgraded.
		if header.Version < 2 && b.isMajorityVersion(2, prevNode,
			b.chainParams.BlockRejectNumRequired) {

			str := "new blocks with version %d are no longer valid"
			str = fmt.Sprintf(str, header.Version)
			return ruleError(ErrBlockVersionTooOld, str)
		}
	}

	return nil
}

// checkBlockPositional performs several validation checks on the block which
// depend on its position within the block chain and having the headers of all
// ancestors available.  These checks do not, and must not, rely on having the
// full block data of all ancestors available.
//
// The flags modify the behavior of this function as follows:
//  - BFFastAdd: The transactions are not checked to see if they are expired and
//    the coinbae height check is not performed.
//
// The flags are also passed to checkBlockHeaderPositional.  See its
// documentation for how the flags modify its behavior.
func (b *BlockChain) checkBlockPositional(block *dcrutil.Block, prevNode *blockNode, flags BehaviorFlags) error {
	// The genesis block is valid by definition.
	if prevNode == nil {
		return nil
	}

	// Perform all block header related validation checks that depend on its
	// position within the block chain and having the headers of all
	// ancestors available, but do not rely on having the full block data of
	// all ancestors available.
	header := &block.MsgBlock().Header
	err := b.checkBlockHeaderPositional(header, prevNode, flags)
	if err != nil {
		return err
	}

	fastAdd := flags&BFFastAdd == BFFastAdd
	if !fastAdd {
		// The height of this block is one more than the referenced
		// previous block.
		blockHeight := prevNode.height + 1

		// Ensure all transactions in the block are not expired.
		for _, tx := range block.Transactions() {
			if IsExpired(tx, blockHeight) {
				errStr := fmt.Sprintf("block contains expired regular "+
					"transaction %v (expiration height %d)", tx.Hash(),
					tx.MsgTx().Expiry)
				return ruleError(ErrExpiredTx, errStr)
			}
		}
		for _, stx := range block.STransactions() {
			if IsExpired(stx, blockHeight) {
				errStr := fmt.Sprintf("block contains expired stake "+
					"transaction %v (expiration height %d)", stx.Hash(),
					stx.MsgTx().Expiry)
				return ruleError(ErrExpiredTx, errStr)
			}
		}

		// Check that the coinbase contains at minimum the block
		// height in output 1.
		if blockHeight > 1 {
			err := checkCoinbaseUniqueHeight(blockHeight, block)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// checkBlockHeaderContext performs several validation checks on the block
// header which depend on having the full block data for all of its ancestors
// available.  This includes checks which depend on tallying the results of
// votes, because votes are part of the block data.
//
// It should be noted that rule changes that have become buried deep enough
// typically will eventually be transitioned to using well-known activation
// points for efficiency purposes at which point the associated checks no longer
// require having direct access to the historical votes, and therefore may be
// transitioned to checkBlockHeaderPositional at that time.  Conversely, any
// checks in that function which become conditional based on the results of a
// vote will necessarily need to be transitioned to this function.
//
// The flags modify the behavior of this function as follows:
//  - BFFastAdd: No check are performed.
//
// This function MUST be called with the chain state lock held (for writes).
func (b *BlockChain) checkBlockHeaderContext(header *wire.BlockHeader, prevNode *blockNode, flags BehaviorFlags) error {
	// The genesis block is valid by definition.
	if prevNode == nil {
		return nil
	}

	fastAdd := flags&BFFastAdd == BFFastAdd
	if !fastAdd {
		// Ensure the stake difficulty specified in the block header
		// matches the calculated difficulty based on the previous block
		// and difficulty retarget rules.
		expSDiff, err := b.calcNextRequiredStakeDifficulty(prevNode)
		if err != nil {
			return err
		}
		if header.SBits != expSDiff {
			errStr := fmt.Sprintf("block stake difficulty of %d "+
				"is not the expected value of %d", header.SBits,
				expSDiff)
			return ruleError(ErrUnexpectedDifficulty, errStr)
		}

		// Enforce the stake version in the header once a majority of
		// the network has upgraded to version 3 blocks.
		if header.Version >= 3 && b.isMajorityVersion(3, prevNode,
			b.chainParams.BlockEnforceNumRequired) {

			expectedStakeVer := b.calcStakeVersion(prevNode)
			if header.StakeVersion != expectedStakeVer {
				str := fmt.Sprintf("block stake version of %d "+
					"is not the expected version of %d",
					header.StakeVersion, expectedStakeVer)
				return ruleError(ErrBadStakeVersion, str)
			}
		}

		// Ensure the header commits to the correct pool size based on
		// its position within the chain.
		parentStakeNode, err := b.fetchStakeNode(prevNode)
		if err != nil {
			return err
		}
		calcPoolSize := uint32(parentStakeNode.PoolSize())
		if header.PoolSize != calcPoolSize {
			errStr := fmt.Sprintf("block header commitment to "+
				"pool size %d does not match expected size %d",
				header.PoolSize, calcPoolSize)
			return ruleError(ErrPoolSize, errStr)
		}

		// Ensure the header commits to the correct final state of the
		// ticket lottery.
		calcFinalState := parentStakeNode.FinalState()
		if header.FinalState != calcFinalState {
			errStr := fmt.Sprintf("block header commitment to "+
				"final state of the ticket lottery %x does not "+
				"match expected value %x", header.FinalState,
				calcFinalState)
			return ruleError(ErrInvalidFinalState, errStr)
		}
	}

	return nil
}

// checkCoinbaseUniqueHeight checks to ensure that for all blocks height > 1 the
// coinbase contains the height encoding to make coinbase hash collisions
// impossible.
func checkCoinbaseUniqueHeight(blockHeight int64, block *dcrutil.Block) error {
	// Coinbase TxOut[0] is always tax, TxOut[1] is always
	// height + extranonce, so at least two outputs must
	// exist.
	if len(block.MsgBlock().Transactions[0].TxOut) < 2 {
		str := fmt.Sprintf("block %v is missing necessary coinbase "+
			"outputs", block.Hash())
		return ruleError(ErrFirstTxNotCoinbase, str)
	}

	// Only version 0 scripts are currently valid.
	nullDataOut := block.MsgBlock().Transactions[0].TxOut[1]
	if nullDataOut.Version != 0 {
		str := fmt.Sprintf("block %v output 1 has wrong script version",
			block.Hash())
		return ruleError(ErrFirstTxNotCoinbase, str)
	}

	// The first 4 bytes of the null data output must be the encoded height
	// of the block, so that every coinbase created has a unique transaction
	// hash.
	nullData, err := txscript.ExtractCoinbaseNullData(nullDataOut.PkScript)
	if err != nil {
		str := fmt.Sprintf("block %v output 1 has wrong script type",
			block.Hash())
		return ruleError(ErrFirstTxNotCoinbase, str)
	}
	if len(nullData) < 4 {
		str := fmt.Sprintf("block %v output 1 data push too short to "+
			"contain height", block.Hash())
		return ruleError(ErrFirstTxNotCoinbase, str)
	}

	// Check the height and ensure it is correct.
	cbHeight := binary.LittleEndian.Uint32(nullData[0:4])
	if cbHeight != uint32(blockHeight) {
		prevBlock := block.MsgBlock().Header.PrevBlock
		str := fmt.Sprintf("block %v output 1 has wrong height in "+
			"coinbase; want %v, got %v; prevBlock %v, header height %v",
			block.Hash(), blockHeight, cbHeight, prevBlock,
			block.MsgBlock().Header.Height)
		return ruleError(ErrCoinbaseHeight, str)
	}

	return nil
}

// checkAllowedVotes performs validation of all votes in the block to ensure
// they spend tickets that are actually allowed to vote per the lottery.
//
// This function is safe for concurrent access.
func (b *BlockChain) checkAllowedVotes(parentStakeNode *stake.Node, block *wire.MsgBlock) error {
	// Determine the winning ticket hashes and create a map for faster lookup.
	ticketsPerBlock := int(b.chainParams.TicketsPerBlock)
	winningHashes := make(map[chainhash.Hash]struct{}, ticketsPerBlock)
	for _, ticketHash := range parentStakeNode.Winners() {
		winningHashes[ticketHash] = struct{}{}
	}

	for _, stx := range block.STransactions {
		// Ignore non-vote stake transactions.
		if !stake.IsSSGen(stx) {
			continue
		}

		// Ensure the ticket being spent is actually eligible to vote in
		// this block.
		ticketHash := stx.TxIn[1].PreviousOutPoint.Hash
		if _, ok := winningHashes[ticketHash]; !ok {
			errStr := fmt.Sprintf("block contains vote for "+
				"ineligible ticket %s (eligible tickets: %s)",
				ticketHash, winningHashes)
			return ruleError(ErrTicketUnavailable, errStr)
		}
	}

	return nil
}

// checkAllowedRevocations performs validation of all revocations in the block
// to ensure they spend tickets that are actually allowed to be revoked per the
// lottery.  Tickets are only eligible to be revoked if they were missed or have
// expired.
//
// This function is safe for concurrent access.
func (b *BlockChain) checkAllowedRevocations(parentStakeNode *stake.Node, block *wire.MsgBlock) error {
	for _, stx := range block.STransactions {
		// Ignore non-revocation stake transactions.
		if !stake.IsSSRtx(stx) {
			continue
		}

		// Ensure the ticket being spent is actually eligible to be
		// revoked in this block.
		ticketHash := stx.TxIn[0].PreviousOutPoint.Hash
		if !parentStakeNode.ExistsMissedTicket(ticketHash) {
			errStr := fmt.Sprintf("block contains revocation of "+
				"ineligible ticket %s", ticketHash)
			return ruleError(ErrInvalidSSRtx, errStr)
		}
	}

	return nil
}

// checkBlockContext performs several validation checks on the block which depend
// on having the full block data for all of its ancestors available.  This
// includes checks which depend on tallying the results of votes, because votes
// are part of the block data.
//
// It should be noted that rule changes that have become buried deep enough
// typically will eventually be transitioned to using well-known activation
// points for efficiency purposes at which point the associated checks no longer
// require having direct access to the historical votes, and therefore may be
// transitioned to checkBlockPositional at that time.  Conversely, any checks
// in that function which become conditional based on the results of a vote will
// necessarily need to be transitioned to this function.
//
// The flags modify the behavior of this function as follows:
//  - BFFastAdd: The max block size is not checked, transactions are not checked
//    to see if they are finalized, and the included votes and revocations are
//    not verified to be allowed.
//
// The flags are also passed to checkBlockHeaderContext.  See its documentation
// for how the flags modify its behavior.
func (b *BlockChain) checkBlockContext(block *dcrutil.Block, prevNode *blockNode, flags BehaviorFlags) error {
	// The genesis block is valid by definition.
	if prevNode == nil {
		return nil
	}

	// Perform all block header related validation checks which depend on
	// having the full block data for all of its ancestors available.
	header := &block.MsgBlock().Header
	err := b.checkBlockHeaderContext(header, prevNode, flags)
	if err != nil {
		return err
	}

	fastAdd := flags&BFFastAdd == BFFastAdd
	if !fastAdd {
		// A block must not exceed the maximum allowed size as defined
		// by the network parameters and the current status of any hard
		// fork votes to change it when serialized.
		maxBlockSize, err := b.maxBlockSize(prevNode)
		if err != nil {
			return err
		}
		serializedSize := int64(block.MsgBlock().Header.Size)
		if serializedSize > maxBlockSize {
			str := fmt.Sprintf("serialized block is too big - "+
				"got %d, max %d", serializedSize,
				maxBlockSize)
			return ruleError(ErrBlockTooBig, str)
		}

		// Switch to using the past median time of the block prior to
		// the block being checked for all checks related to lock times
		// once the stake vote for the agenda is active.
		blockTime := header.Timestamp
		lnFeaturesActive, err := b.isLNFeaturesAgendaActive(prevNode)
		if err != nil {
			return err
		}
		if lnFeaturesActive {
			blockTime = prevNode.CalcPastMedianTime()
		}

		// The height of this block is one more than the referenced
		// previous block.
		blockHeight := prevNode.height + 1

		// Ensure all transactions in the block are finalized.
		for _, tx := range block.Transactions() {
			if !IsFinalizedTransaction(tx, blockHeight, blockTime) {
				str := fmt.Sprintf("block contains unfinalized regular "+
					"transaction %v", tx.Hash())
				return ruleError(ErrUnfinalizedTx, str)
			}
		}
		for _, stx := range block.STransactions() {
			if !IsFinalizedTransaction(stx, blockHeight, blockTime) {
				str := fmt.Sprintf("block contains unfinalized stake "+
					"transaction %v", stx.Hash())
				return ruleError(ErrUnfinalizedTx, str)
			}
		}

		// Ensure that all votes are only for winning tickets and all
		// revocations are actually eligible to be revoked once stake
		// validation height has been reached.
		if blockHeight >= b.chainParams.StakeValidationHeight {
			parentStakeNode, err := b.fetchStakeNode(prevNode)
			if err != nil {
				return err
			}
			err = b.checkAllowedVotes(parentStakeNode, block.MsgBlock())
			if err != nil {
				return err
			}

			err = b.checkAllowedRevocations(parentStakeNode,
				block.MsgBlock())
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// checkDupTxs ensures blocks do not contain duplicate transactions which
// 'overwrite' older transactions that are not fully spent.  This prevents an
// attack where a coinbase and all of its dependent transactions could be
// duplicated to effectively revert the overwritten transactions to a single
// confirmation thereby making them vulnerable to a double spend.
//
// For more details, see https://en.bitcoin.it/wiki/BIP_0030 and
// http://r6.ca/blog/20120206T005236Z.html.
//
// Decred: Check the stake transactions to make sure they don't have this txid
// too.
func (b *BlockChain) checkDupTxs(txSet []*dcrutil.Tx, view *UtxoViewpoint) error {
	if !chaincfg.CheckForDuplicateHashes {
		return nil
	}

	// Fetch utxo details for all of the transactions in this block.
	// Typically, there will not be any utxos for any of the transactions.
	filteredSet := make(viewFilteredSet)
	for _, tx := range txSet {
		filteredSet.add(view, tx.Hash())
	}
	err := view.fetchUtxosMain(b.db, filteredSet)
	if err != nil {
		return err
	}

	// Duplicate transactions are only allowed if the previous transaction
	// is fully spent.
	for _, tx := range txSet {
		txEntry := view.LookupEntry(tx.Hash())
		if txEntry != nil && !txEntry.IsFullySpent() {
			str := fmt.Sprintf("tried to overwrite transaction %v "+
				"at block height %d that is not fully spent",
				tx.Hash(), txEntry.BlockHeight())
			return ruleError(ErrOverwriteTx, str)
		}
	}

	return nil
}

// extractStakePubKeyHash extracts a pubkey hash from the passed public key
// script if it is a standard pay-to-pubkey-hash script tagged with the provided
// stake opcode.  It will return nil otherwise.
func extractStakePubKeyHash(script []byte, stakeOpcode byte) []byte {
	if len(script) == 26 &&
		script[0] == stakeOpcode &&
		script[1] == txscript.OP_DUP &&
		script[2] == txscript.OP_HASH160 &&
		script[3] == txscript.OP_DATA_20 &&
		script[24] == txscript.OP_EQUALVERIFY &&
		script[25] == txscript.OP_CHECKSIG {

		return script[4:24]
	}

	return nil
}

// isStakePubKeyHash returns whether or not the passed public key script is a
// standard pay-to-pubkey-hash script tagged with the provided stake opcode.
func isStakePubKeyHash(script []byte, stakeOpcode byte) bool {
	return extractStakePubKeyHash(script, stakeOpcode) != nil
}

// extractStakeScriptHash extracts a script hash from the passed public key
// script if it is a standard pay-to-script-hash script tagged with the provided
// stake opcode.  It will return nil otherwise.
func extractStakeScriptHash(script []byte, stakeOpcode byte) []byte {
	if len(script) == 24 &&
		script[0] == stakeOpcode &&
		script[1] == txscript.OP_HASH160 &&
		script[2] == txscript.OP_DATA_20 &&
		script[23] == txscript.OP_EQUAL {

		return script[3:23]
	}

	return nil
}

// isStakeScriptHash returns whether or not the passed public key script is a
// standard pay-to-script-hash script tagged with the provided stake opcode.
func isStakeScriptHash(script []byte, stakeOpcode byte) bool {
	return extractStakeScriptHash(script, stakeOpcode) != nil
}

// isAllowedTicketInputScriptForm returns whether or not the passed public key
// script is a one of the allowed forms for a ticket input.
func isAllowedTicketInputScriptForm(script []byte) bool {
	return isPubKeyHash(script) || isScriptHash(script) ||
		isStakePubKeyHash(script, txscript.OP_SSGEN) ||
		isStakeScriptHash(script, txscript.OP_SSGEN) ||
		isStakePubKeyHash(script, txscript.OP_SSRTX) ||
		isStakeScriptHash(script, txscript.OP_SSRTX) ||
		isStakePubKeyHash(script, txscript.OP_SSTXCHANGE) ||
		isStakeScriptHash(script, txscript.OP_SSTXCHANGE)
}

// extractTicketCommitAmount extracts and decodes the amount from a ticket
// output commitment script.
//
// NOTE: The caller MUST have already determined that the provided script is
// a commitment output script or the function may panic.
func extractTicketCommitAmount(script []byte) int64 {
	// Extract the encoded amount from the commitment output associated with
	// the input.  The MSB of the encoded amount specifies if the output is
	// P2SH, so it must be cleared to get the decoded amount.
	amtBytes := script[commitAmountStartIdx:commitAmountEndIdx]
	amtEncoded := binary.LittleEndian.Uint64(amtBytes)
	return int64(amtEncoded & ^commitP2SHFlag)
}

// checkTicketPurchaseInputs performs a series of checks on the inputs to a
// ticket purchase transaction.  An example of some of the checks include
// verifying all inputs exist, ensuring the input type requirements are met,
// and validating the output commitments coincide with the inputs.
//
// NOTE: The caller MUST have already determined that the provided transaction
// is a ticket purchase.
func checkTicketPurchaseInputs(msgTx *wire.MsgTx, view *UtxoViewpoint) error {
	// Assert there are two outputs for each input to the ticket as well as the
	// additional voting rights output.
	if (len(msgTx.TxIn)*2 + 1) != len(msgTx.TxOut) {
		panicf("attempt to check ticket purchase inputs on tx %s which does "+
			"not appear to be a ticket purchase (%d inputs, %d outputs)",
			msgTx.TxHash(), len(msgTx.TxIn), len(msgTx.TxOut))
	}

	for txInIdx := 0; txInIdx < len(msgTx.TxIn); txInIdx++ {
		txIn := msgTx.TxIn[txInIdx]
		originTxHash := &txIn.PreviousOutPoint.Hash
		originTxIndex := txIn.PreviousOutPoint.Index
		entry := view.LookupEntry(originTxHash)
		if entry == nil || entry.IsOutputSpent(originTxIndex) {
			str := fmt.Sprintf("output %v referenced from transaction %s:%d "+
				"either does not exist or has already been spent",
				txIn.PreviousOutPoint, msgTx.TxHash(), txInIdx)
			return ruleError(ErrMissingTxOut, str)
		}

		// Ensure the output being spent is one of the allowed script forms.  In
		// particular, the allowed forms are pay-to-pubkey-hash and
		// pay-to-script-hash either in the standard form (without a stake
		// opcode) or their stake-tagged variant (with a stake opcode).
		pkScriptVer := entry.ScriptVersionByIndex(originTxIndex)
		if pkScriptVer != 0 {
			str := fmt.Sprintf("output %v script version %d referenced by "+
				"ticket %s:%d is not supported", txIn.PreviousOutPoint,
				pkScriptVer, msgTx.TxHash(), txInIdx)
			return ruleError(ErrTicketInputScript, str)
		}
		pkScript := entry.PkScriptByIndex(originTxIndex)
		if !isAllowedTicketInputScriptForm(pkScript) {
			str := fmt.Sprintf("output %v referenced from ticket %s:%d "+
				"is not pay-to-pubkey-hash or pay-to-script-hash (script: %x)",
				txIn.PreviousOutPoint, msgTx.TxHash(), txInIdx, pkScript)
			return ruleError(ErrTicketInputScript, str)
		}

		// Extract the amount from the commitment output associated with the
		// input and ensure it matches the expected amount calculated from the
		// actual input amount and change.
		commitmentOutIdx := txInIdx*2 + 1
		commitmentScript := msgTx.TxOut[commitmentOutIdx].PkScript
		commitmentAmount := extractTicketCommitAmount(commitmentScript)
		inputAmount := entry.AmountByIndex(originTxIndex)
		change := msgTx.TxOut[commitmentOutIdx+1].Value
		adjustedAmount := commitmentAmount + change
		if adjustedAmount != inputAmount {
			str := fmt.Sprintf("ticket output %d pays a different amount than "+
				"the associated input %d (input: %v, commitment: %v, change: "+
				"%v)", commitmentOutIdx, txInIdx, inputAmount, commitmentAmount,
				change)
			return ruleError(ErrTicketCommitment, str)
		}
	}

	return nil
}

// isStakeSubmission returns whether or not the passed public key script is a
// standard pay-to-script-hash or pay-to-script-hash script tagged with the
// stake submission opcode.
func isStakeSubmission(script []byte) bool {
	return isStakePubKeyHash(script, txscript.OP_SSTX) ||
		isStakeScriptHash(script, txscript.OP_SSTX)
}

// extractTicketCommitHash extracts the commitment hash from a ticket output
// commitment script.
//
// NOTE: The caller MUST have already determined that the provided script is
// a commitment output script or the function may panic.
func extractTicketCommitHash(script []byte) []byte {
	return script[commitHashStartIdx:commitHashEndIdx]
}

// isTicketCommitP2SH returns whether or not the passed ticket output commitment
// script commits to a hash which represents a pay-to-script-hash output.  When
// false, it commits to a hash which represents a pay-to-pubkey-hash output.
//
// NOTE: The caller MUST have already determined that the provided script is
// a commitment output script or the function may panic.
func isTicketCommitP2SH(script []byte) bool {
	// The MSB of the encoded amount specifies if the output is P2SH.  Since
	// it is encoded with little endian, the MSB is in final byte in the encoded
	// amount.
	//
	// This is a faster equivalent of:
	//
	//	amtBytes := script[commitAmountStartIdx:commitAmountEndIdx]
	//	amtEncoded := binary.LittleEndian.Uint64(amtBytes)
	//	return (amtEncoded & commitP2SHFlag) != 0
	//
	return script[commitAmountEndIdx-1]&0x80 != 0
}

// calcTicketReturnAmount calculates the required amount to return from a ticket
// for a given original contribution amount, the price the ticket was purchased
// for, the vote subsidy (if any) to include, and the sum of all contributions
// used to purchase the ticket.
//
// Since multiple inputs can be used to purchase a ticket, each one contributes
// a portion of the overall ticket purchase, including the transaction fee.
// Thus, when claiming the ticket, either due to it being selected to vote, or
// being revoked, each output must receive the same proportion of the total
// amount returned.
//
// The vote subsidy must be 0 for revocations since, unlike votes, they do not
// produce any additional subsidy.
func calcTicketReturnAmount(contribAmount, ticketPurchaseAmount, voteSubsidy int64, contributionSumBig *big.Int) int64 {
	// This is effectively equivalent to:
	//
	//                  total output amount
	// return amount =  ------------------- * contribution amount
	//                  total contributions
	//
	// However, in order to avoid floating point math, it uses 64.32 fixed point
	// integer division to perform:
	//
	//                   --                                              --
	//                  | total output amount * contribution amount * 2^32 |
	//                  | ------------------------------------------------ |
	// return amount =  |                total contributions               |
	//                   --                                              --
	//                  ----------------------------------------------------
	//                                        2^32
	//
	totalOutputAmtBig := big.NewInt(ticketPurchaseAmount + voteSubsidy)
	returnAmtBig := big.NewInt(contribAmount)
	returnAmtBig.Mul(returnAmtBig, totalOutputAmtBig)
	returnAmtBig.Lsh(returnAmtBig, 32)
	returnAmtBig.Div(returnAmtBig, contributionSumBig)
	returnAmtBig.Rsh(returnAmtBig, 32)
	return returnAmtBig.Int64()
}

// extractTicketCommitFeeLimits extracts the encoded fee limits from a ticket
// output commitment script.
//
// NOTE: The caller MUST have already determined that the provided script is
// a commitment output script or the function may panic.
func extractTicketCommitFeeLimits(script []byte) uint16 {
	encodedLimits := script[commitFeeLimitStartIdx:commitFeeLimitEndIdx]
	return binary.LittleEndian.Uint16(encodedLimits)
}

// checkTicketSubmissionInput ensures the provided unspent transaction output is
// a supported ticket submission output in terms of the script version and type
// as well as ensuring the transaction that houses the output is a ticket.
//
// Note that the returned error is not a RuleError, so the it is up to the
// caller convert it accordingly.
func checkTicketSubmissionInput(ticketUtxo *UtxoEntry) error {
	submissionScriptVer := ticketUtxo.ScriptVersionByIndex(submissionOutputIdx)
	if submissionScriptVer != 0 {
		return fmt.Errorf("script version %d is not supported",
			submissionScriptVer)
	}
	submissionScript := ticketUtxo.PkScriptByIndex(submissionOutputIdx)
	if !isStakeSubmission(submissionScript) {
		return fmt.Errorf("not a supported stake submission script (script: "+
			"%x)", submissionScript)
	}

	// Ensure the referenced output is from a ticket.  This also proves the form
	// of the transaction and its outputs are as expected so subsequent code is
	// able to avoid a lot of additional overhead rechecking things.
	if ticketUtxo.TransactionType() != stake.TxTypeSStx {
		return fmt.Errorf("not a submission script")
	}

	return nil
}

// checkTicketRedeemerCommitments ensures the outputs of the provided
// transaction, which MUST be either a vote or revocation, adhere to the
// commitments in the provided ticket outputs.  The vote subsidy MUST be zero
// for revocations since they do not produce any additional subsidy.
//
// NOTE: This is only intended to be a helper to refactor out common code from
// checkVoteInputs and checkRevocationInputs.
func checkTicketRedeemerCommitments(ticketHash *chainhash.Hash, ticketOuts []*stake.MinimalOutput, msgTx *wire.MsgTx, isVote bool, voteSubsidy int64) error {
	// Make an initial pass over the ticket commitments to calculate the overall
	// contribution sum.  This is necessary because the output amounts are
	// required to be scaled to maintain the same proportions as the original
	// contributions to the ticket, and the overall contribution sum is needed
	// to calculate those proportions.
	//
	// The calculations also require more than 64-bits, so convert it to a big
	// integer here to avoid multiple conversions later.
	var contributionSum int64
	for i := 1; i < len(ticketOuts); i += 2 {
		contributionSum += extractTicketCommitAmount(ticketOuts[i].PkScript)
	}
	contributionSumBig := big.NewInt(contributionSum)

	// The outputs that satisify the commitments of the ticket start at offset
	// 2 for votes while they start at 0 for revocations.  Also, the payments
	// must be tagged with the appropriate stake opcode depending on whether it
	// is a vote or a revocation.  Finally, the fee limits in the original
	// ticket commitment differ for votes and revocations, so choose the correct
	// bit flag and mask accordingly.
	startIdx := 2
	reqStakeOpcode := byte(txscript.OP_SSGEN)
	hasFeeLimitFlag := uint16(stake.SStxVoteFractionFlag)
	feeLimitMask := uint16(stake.SStxVoteReturnFractionMask)
	if !isVote {
		startIdx = 0
		reqStakeOpcode = txscript.OP_SSRTX
		hasFeeLimitFlag = stake.SStxRevFractionFlag
		feeLimitMask = stake.SStxRevReturnFractionMask
	}
	ticketPaidAmt := ticketOuts[submissionOutputIdx].Value
	for txOutIdx := startIdx; txOutIdx < len(msgTx.TxOut); txOutIdx++ {
		// Ensure the output is paying to the address and type specified by the
		// original commitment in the ticket and is a version 0 script.
		//
		// Note that this specifically limits the types of allowed outputs to
		// P2PKH and P2SH, since the original commitment has the same
		// limitation.
		txOut := msgTx.TxOut[txOutIdx]
		if txOut.Version != 0 {
			str := fmt.Sprintf("output %s:%d script version %d is not "+
				"supported", msgTx.TxHash(), txOutIdx, txOut.Version)
			return ruleError(ErrBadPayeeScriptVersion, str)
		}

		commitmentOutIdx := (txOutIdx-startIdx)*2 + 1
		commitmentScript := ticketOuts[commitmentOutIdx].PkScript
		var paymentHash []byte
		if isTicketCommitP2SH(commitmentScript) {
			paymentHash = extractStakeScriptHash(txOut.PkScript, reqStakeOpcode)
			if paymentHash == nil {
				str := fmt.Sprintf("output %s:%d payment script type is not "+
					"pay-to-script-hash as required by ticket output "+
					"commitment %s:%d", msgTx.TxHash(), txOutIdx, ticketHash,
					commitmentOutIdx)
				return ruleError(ErrBadPayeeScriptType, str)
			}
		} else {
			paymentHash = extractStakePubKeyHash(txOut.PkScript, reqStakeOpcode)
			if paymentHash == nil {
				str := fmt.Sprintf("output %s:%d payment script type is not "+
					"pay-to-pubkey-hash as required by ticket output "+
					"commitment %s:%d", msgTx.TxHash(), txOutIdx, ticketHash,
					commitmentOutIdx)
				return ruleError(ErrBadPayeeScriptType, str)
			}
		}
		commitmentHash := extractTicketCommitHash(commitmentScript)
		if !bytes.Equal(paymentHash, commitmentHash) {
			str := fmt.Sprintf("output %s:%d does not pay to the hash "+
				"specified by ticket output commitment %s:%d (ticket commits "+
				"to %x, output pays %x)", msgTx.TxHash(), txOutIdx, ticketHash,
				commitmentOutIdx, commitmentHash, paymentHash)
			return ruleError(ErrMismatchedPayeeHash, str)
		}

		// Calculate the required payment amount for the output based on the
		// relative percentage of the associated contribution to the original
		// ticket and any additional subsidy produced by the vote (must be 0 for
		// revocations).
		//
		// It should be noted that, due to the scaling, the sum of the generated
		// amounts for mult-participant votes might be a few atoms less than
		// the full amount and the difference is treated as a standard
		// transaction fee.
		commitmentAmt := extractTicketCommitAmount(commitmentScript)
		expectedOutAmt := calcTicketReturnAmount(commitmentAmt, ticketPaidAmt,
			voteSubsidy, contributionSumBig)

		// Ensure the amount paid adheres to the commitment while taking into
		// account any fee limits that might be imposed.  The output amount must
		// exactly match the calculated amount when when not encumbered with a
		// fee limit.  On the other hand, when it is encumbered, it must be
		// between the minimum amount imposed by the fee limit and the
		// calculated amount.
		feeLimitsEncoded := extractTicketCommitFeeLimits(commitmentScript)
		hasFeeLimit := feeLimitsEncoded&hasFeeLimitFlag != 0
		if !hasFeeLimit {
			// The output amount must exactly match the calculated amount when
			// not encumbered with a fee limit.
			if txOut.Value != expectedOutAmt {
				str := fmt.Sprintf("output %s:%d does not pay the expected "+
					"amount per ticket output commitment %s:%d (expected %d, "+
					"output pays %d)", msgTx.TxHash(), txOutIdx, ticketHash, // PROBLEM:  ticketHash...
					commitmentOutIdx, expectedOutAmt, txOut.Value)
				return ruleError(ErrBadPayeeValue, str)
			}
		} else {
			// Calculate the minimum allowed amount based on the fee limit.
			//
			// Notice that, since the fee limit is in terms of a log2 value, and
			// amounts are int64, anything greater than or equal to 63 is
			// interpreted to mean allow spending the entire amount as a fee.
			// This allows fast left shifts to be used to calculate the fee
			// limit while preventing degenerate cases such as negative numbers
			// for 63.
			var amtLimitLow int64
			feeLimitLog2 := feeLimitsEncoded & feeLimitMask
			if feeLimitLog2 < 63 {
				feeLimit := int64(1 << uint64(feeLimitLog2))
				if feeLimit < expectedOutAmt {
					amtLimitLow = expectedOutAmt - feeLimit
				}
			}

			// The output must not be less than the minimum amount.
			if txOut.Value < amtLimitLow {
				str := fmt.Sprintf("output %s:%d pays less than the expected "+
					"expected amount per ticket output commitment %s:%d "+
					"(lowest allowed %d, output pays %d)", msgTx.TxHash(),
					txOutIdx, ticketHash, commitmentOutIdx, amtLimitLow, // PROBLEM:  ticketHash...
					txOut.Value)
				return ruleError(ErrBadPayeeValue, str)
			}

			// The output must not be more than the expected amount.
			if txOut.Value > expectedOutAmt {
				str := fmt.Sprintf("output %s:%d pays more than the expected "+
					"amount per ticket output commitment %s:%d (expected %d, "+
					"output pays %d)", msgTx.TxHash(), txOutIdx, ticketHash,
					commitmentOutIdx, expectedOutAmt, txOut.Value)
				return ruleError(ErrBadPayeeValue, str)
			}
		}
	}

	return nil
}

// checkVoteInputs performs a series of checks on the inputs to a vote
// transaction.  An example of some of the checks include verifying the
// referenced ticket exists, the stakebase input commits to correct subsidy,
// the output amounts adhere to the commitments of the referenced ticket, and
// the ticket maturity requirements are met.
//
// NOTE: The caller MUST have already determined that the provided transaction
// is a vote.
func checkVoteInputs(subsidyCache *SubsidyCache, tx *dcrutil.Tx, txHeight int64, view *UtxoViewpoint, params *chaincfg.Params) error {
	ticketMaturity := int64(params.TicketMaturity)
	voteHash := tx.Hash()
	msgTx := tx.MsgTx()

	// Calculate the theoretical stake vote subsidy by extracting the vote
	// height.
	//
	// WARNING: This really should be calculating the subsidy based on the
	// height of the block the vote is contained in as opposed to the block it
	// is voting on.  Unfortunately, this is now part of consensus, so changing
	// it requires a hard fork vote.
	_, heightVotingOn := stake.SSGenBlockVotedOn(msgTx)
	voteSubsidy := CalcStakeVoteSubsidy(subsidyCache, int64(heightVotingOn),
		params)

	// The input amount specified by the stakebase must commit to the subsidy
	// generated by the vote.
	stakebase := msgTx.TxIn[0]
	if stakebase.ValueIn != voteSubsidy {
		str := fmt.Sprintf("vote subsidy input value of %v is not %v",
			stakebase.ValueIn, voteSubsidy)
		return ruleError(ErrBadStakebaseAmountIn, str)
	}

	// The second input to a vote must be the first output of the ticket the
	// vote is associated with.
	const ticketInIdx = 1
	ticketIn := msgTx.TxIn[ticketInIdx]
	if ticketIn.PreviousOutPoint.Index != submissionOutputIdx {
		str := fmt.Sprintf("vote %s:%d references output %d instead of the "+
			"first output", voteHash, ticketInIdx,
			ticketIn.PreviousOutPoint.Index)
		return ruleError(ErrInvalidVoteInput, str)
	}

	// Ensure the referenced ticket is available.
	ticketHash := &ticketIn.PreviousOutPoint.Hash
	ticketUtxo := view.LookupEntry(ticketHash)
	if ticketUtxo == nil || ticketUtxo.IsFullySpent() {
		str := fmt.Sprintf("ticket output %v referenced by vote %s:%d  either "+
			"does not exist or has already been spent",
			ticketIn.PreviousOutPoint, voteHash, ticketInIdx)
		return ruleError(ErrMissingTxOut, str)
	}

	// Ensure the referenced transaction output is a supported ticket submission
	// output in terms of the script version and type as well as ensuring the
	// transaction that houses the output is a ticket.  This also proves the
	// form of the transaction and its outputs are as expected so subsequent
	// code is able to avoid a lot of additional overhead rechecking things.
	if err := checkTicketSubmissionInput(ticketUtxo); err != nil {
		str := fmt.Sprintf("output %v referenced by vote %s:%d consensus "+
			"violation: %s", ticketIn.PreviousOutPoint, voteHash, ticketInIdx,
			err.Error())
		return ruleError(ErrInvalidVoteInput, str)
	}

	// Ensure the vote is not spending a ticket which has not yet reached the
	// required ticket maturity.
	//
	// NOTE: A ticket stake submission (OP_SSTX tagged output) can only be spent
	// in the block AFTER the entire ticket maturity has passed, hence the +1.
	originHeight := ticketUtxo.BlockHeight()
	blocksSincePrev := txHeight - originHeight
	if blocksSincePrev < ticketMaturity+1 {
		str := fmt.Sprintf("tried to spend ticket output from transaction "+
			"%v from height %d at height %d before required ticket maturity "+
			"of %d+1 blocks", ticketHash, originHeight, txHeight,
			ticketMaturity)
		return ruleError(ErrImmatureTicketSpend, str)
	}

	ticketOuts := ConvertUtxosToMinimalOutputs(ticketUtxo)
	if len(ticketOuts) == 0 {
		panicf("missing extra stake data for ticket %v -- probable database "+
			"corruption", ticketHash)
	}

	// Ensure the number of payment outputs matches the number of commitments
	// made by the associated ticket.
	//
	// The vote transaction outputs must consist of an OP_RETURN output that
	// indicates the block being voted on, an OP_RETURN with the user-provided
	// vote bits, and an output that corresponds to every commitment output in
	// the ticket associated with the vote.  The associated ticket outputs
	// consist of a stake submission output, and two outputs for each payment
	// commitment such that the first one of the pair is a commitment amount and
	// the second one is the amount of change sent back to the contributor to
	// the ticket based on their original funding amount.
	numVotePayments := len(msgTx.TxOut) - 2
	if numVotePayments*2 != len(ticketOuts)-1 {
		str := fmt.Sprintf("vote %s makes %d payments when input ticket %s "+
			"has %d commitments", voteHash, numVotePayments, ticketHash,
			len(ticketOuts)-1)
		return ruleError(ErrBadNumPayees, str)
	}

	// Ensure the outputs adhere to the ticket commitments.
	return checkTicketRedeemerCommitments(ticketHash, ticketOuts, msgTx, true,
		voteSubsidy)
}

// checkRevocationInputs performs a series of checks on the inputs to a
// revocation transaction.  An example of some of the checks include verifying
// the referenced ticket exists, the output amounts adhere to the commitments of
// the ticket, and the ticket maturity requirements are met.
//
// NOTE: The caller MUST have already determined that the provided transaction
// is a revocation.
func checkRevocationInputs(tx *dcrutil.Tx, txHeight int64, view *UtxoViewpoint, params *chaincfg.Params) error {
	ticketMaturity := int64(params.TicketMaturity)
	revokeHash := tx.Hash()
	msgTx := tx.MsgTx()

	// The first input to a revocation must be the first output of the ticket
	// the revocation is associated with.
	const ticketInIdx = 0
	ticketIn := msgTx.TxIn[ticketInIdx]
	if ticketIn.PreviousOutPoint.Index != submissionOutputIdx {
		str := fmt.Sprintf("revocation %s:%d references output %d instead of "+
			"the first output", revokeHash, ticketInIdx,
			ticketIn.PreviousOutPoint.Index)
		return ruleError(ErrInvalidRevokeInput, str)
	}

	// Ensure the referenced ticket is available.
	ticketHash := &ticketIn.PreviousOutPoint.Hash
	ticketUtxo := view.LookupEntry(ticketHash)
	if ticketUtxo == nil || ticketUtxo.IsFullySpent() {
		str := fmt.Sprintf("ticket output %v referenced from revocation %s:%d "+
			"either does not exist or has already been spent",
			ticketIn.PreviousOutPoint, revokeHash, ticketInIdx)
		return ruleError(ErrMissingTxOut, str)
	}

	// Ensure the referenced transaction output is a supported ticket submission
	// output in terms of the script version and type as well as ensuring the
	// transaction that houses the output is a ticket.  This also proves the
	// form of the transaction and its outputs are as expected so subsequent
	// code is able to avoid a lot of additional overhead rechecking things.
	if err := checkTicketSubmissionInput(ticketUtxo); err != nil {
		str := fmt.Sprintf("output %v referenced by revocation %s:%d consensus "+
			"violation: %s", ticketIn.PreviousOutPoint, revokeHash, ticketInIdx,
			err.Error())
		return ruleError(ErrInvalidRevokeInput, str)
	}

	// Ensure the revocation is not spending a ticket which has not yet reached
	// the required ticket maturity.
	//
	// NOTE: A ticket stake submission (OP_SSTX tagged output) can only be spent
	// in the block AFTER the entire ticket maturity has passed, and, in order
	// to be revoked, the ticket must have been missed which can't possibly
	// happen for another block after that, hence the +2.
	originHeight := ticketUtxo.BlockHeight()
	blocksSincePrev := txHeight - originHeight
	if blocksSincePrev < ticketMaturity+2 {
		str := fmt.Sprintf("tried to spend ticket output from transaction "+
			"%v from height %d at height %d before required ticket maturity "+
			"of %d+2 blocks", ticketHash, originHeight, txHeight,
			ticketMaturity)
		return ruleError(ErrImmatureTicketSpend, str)
	}

	ticketOuts := ConvertUtxosToMinimalOutputs(ticketUtxo)
	if len(ticketOuts) == 0 {
		panicf("missing extra stake data for ticket %v -- probable database "+
			"corruption", ticketHash)
	}

	// Ensure the number of payment outputs matches the number of commitments
	// made by the associated ticket.
	//
	// The revocation transaction outputs must consist of an output that
	// corresponds to every commitment output in the ticket associated with the
	// revocation.  The associated ticket outputs consist of a stake submission
	// output, and two outputs for each payment commitment such that the first
	// one of the pair is a commitment amount and the second one is the amount
	// of change sent back to the contributor to the ticket based on their
	// original funding amount.
	numRevocationPayments := len(msgTx.TxOut)
	if numRevocationPayments*2 != len(ticketOuts)-1 {
		str := fmt.Sprintf("revocation %s makes %d payments when input ticket "+
			"%s has %d commitments", revokeHash, numRevocationPayments,
			ticketHash, len(ticketOuts)-1)
		return ruleError(ErrBadNumPayees, str)
	}

	// Ensure the outputs adhere to the ticket commitments.  Zero is passed for
	// the vote subsidy since revocations do not produce any subsidy.
	return checkTicketRedeemerCommitments(ticketHash, ticketOuts, msgTx, false,
		0)
}

// CheckTransactionInputs performs a series of checks on the inputs to a
// transaction to ensure they are valid.  An example of some of the checks
// include verifying all inputs exist, ensuring the coinbase seasoning
// requirements are met, detecting double spends, validating all values and
// fees are in the legal range and the total output amount doesn't exceed the
// input amount, and verifying the signatures to prove the spender was the
// owner of the Decred and therefore allowed to spend them.  As it checks the
// inputs, it also calculates the total fees for the transaction and returns
// that value.
//
// NOTE: The transaction MUST have already been sanity checked with the
// CheckTransactionSanity function prior to calling this function.
func CheckTransactionInputs(subsidyCache *SubsidyCache, tx *dcrutil.Tx, txHeight int64, view *UtxoViewpoint, checkFraudProof bool, chainParams *chaincfg.Params) (int64, error) {
	// Coinbase transactions have no inputs.
	if IsCoinBase(tx) {
		return 0, nil
	}

	// -------------------------------------------------------------------
	// Decred stake transaction testing.
	// -------------------------------------------------------------------

	// Perform additional checks on ticket purchase transactions such as
	// ensuring the input type requirements are met and the output commitments
	// coincide with the inputs.
	msgTx := tx.MsgTx()
	isTicket := stake.IsSStx(msgTx)
	if isTicket {
		if err := checkTicketPurchaseInputs(msgTx, view); err != nil {
			return 0, err
		}
	}

	// Perform additional checks on vote transactions such as verying that the
	// referenced ticket exists, the stakebase input commits to correct subsidy,
	// the output amounts adhere to the commitments of the referenced ticket,
	// and the ticket maturity requirements are met.
	//
	// Also keep track of whether or not it is a vote since some inputs need
	// to be skipped later.
	isVote := stake.IsSSGen(msgTx)
	if isVote {
		err := checkVoteInputs(subsidyCache, tx, txHeight, view, chainParams)
		if err != nil {
			return 0, err
		}
	}

	// Perform additional checks on revocation transactions such as verifying
	// the referenced ticket exists, the output amounts adhere to the
	// commitments of the ticket, and the ticket maturity requirements are met.
	//
	// Also keep track of whether or not it is a revocation since some inputs
	// need to be skipped later.
	isRevocation := stake.IsSSRtx(msgTx)
	if isRevocation {
		err := checkRevocationInputs(tx, txHeight, view, chainParams)
		if err != nil {
			return 0, err
		}
	}

	// -------------------------------------------------------------------
	// Decred general transaction testing (and a few stake exceptions).
	// -------------------------------------------------------------------

	txHash := tx.Hash()
	var totalAtomIn int64
	for idx, txIn := range msgTx.TxIn {
		// Inputs won't exist for stakebase tx, so ignore them.
		if isVote && idx == 0 {
			// However, do add the reward amount.
			_, heightVotingOn := stake.SSGenBlockVotedOn(msgTx)
			stakeVoteSubsidy := CalcStakeVoteSubsidy(subsidyCache,
				int64(heightVotingOn), chainParams)
			totalAtomIn += stakeVoteSubsidy
			continue
		}

		txInHash := &txIn.PreviousOutPoint.Hash
		originTxIndex := txIn.PreviousOutPoint.Index
		utxoEntry := view.LookupEntry(txInHash)
		if utxoEntry == nil || utxoEntry.IsOutputSpent(originTxIndex) {
			str := fmt.Sprintf("output %v referenced from "+
				"transaction %s:%d either does not exist or "+
				"has already been spent", txIn.PreviousOutPoint,
				txHash, idx)
			return 0, ruleError(ErrMissingTxOut, str)
		}

		// Check fraud proof witness data.

		// Using zero value outputs as inputs is banned.
		if utxoEntry.AmountByIndex(originTxIndex) == 0 {
			str := fmt.Sprintf("tried to spend zero value output "+
				"from input %v, idx %v", txInHash,
				originTxIndex)
			return 0, ruleError(ErrZeroValueOutputSpend, str)
		}

		if checkFraudProof {
			if txIn.ValueIn !=
				utxoEntry.AmountByIndex(originTxIndex) {
				str := fmt.Sprintf("bad fraud check value in "+
					"(expected %v, given %v) for txIn %v",
					utxoEntry.AmountByIndex(originTxIndex),
					txIn.ValueIn, idx)
				return 0, ruleError(ErrFraudAmountIn, str)
			}

			if int64(txIn.BlockHeight) != utxoEntry.BlockHeight() {
				str := fmt.Sprintf("bad fraud check block "+
					"height (expected %v, given %v) for "+
					"txIn %v", utxoEntry.BlockHeight(),
					txIn.BlockHeight, idx)
				return 0, ruleError(ErrFraudBlockHeight, str)
			}

			if txIn.BlockIndex != utxoEntry.BlockIndex() {
				str := fmt.Sprintf("bad fraud check block "+
					"index (expected %v, given %v) for "+
					"txIn %v", utxoEntry.BlockIndex(),
					txIn.BlockIndex, idx)
				return 0, ruleError(ErrFraudBlockIndex, str)
			}
		}

		// Ensure the transaction is not spending coins which have not
		// yet reached the required coinbase maturity.
		coinbaseMaturity := int64(chainParams.CoinbaseMaturity)
		if utxoEntry.IsCoinBase() {
			originHeight := utxoEntry.BlockHeight()
			blocksSincePrev := txHeight - originHeight
			if blocksSincePrev < coinbaseMaturity {
				str := fmt.Sprintf("tx %v tried to spend "+
					"coinbase transaction %v from height "+
					"%v at height %v before required "+
					"maturity of %v blocks", txHash,
					txInHash, originHeight, txHeight,
					coinbaseMaturity)
				return 0, ruleError(ErrImmatureSpend, str)
			}
		}

		// Ensure that the transaction is not spending coins from a
		// transaction that included an expiry but which has not yet
		// reached coinbase maturity many blocks.
		if utxoEntry.HasExpiry() {
			originHeight := utxoEntry.BlockHeight()
			blocksSincePrev := txHeight - originHeight
			if blocksSincePrev < coinbaseMaturity {
				str := fmt.Sprintf("tx %v tried to spend "+
					"transaction %v including an expiry "+
					"from height %v at height %v before "+
					"required maturity of %v blocks",
					txHash, txInHash, originHeight,
					txHeight, coinbaseMaturity)
				return 0, ruleError(ErrExpiryTxSpentEarly, str)
			}
		}

		// Ensure that the outpoint's tx tree makes sense.
		originTxOPTree := txIn.PreviousOutPoint.Tree
		originTxType := utxoEntry.TransactionType()
		indicatedTree := wire.TxTreeRegular
		if originTxType != stake.TxTypeRegular {
			indicatedTree = wire.TxTreeStake
		}
		if indicatedTree != originTxOPTree {
			errStr := fmt.Sprintf("tx %v attempted to spend from "+
				"a %v tx tree (hash %v), yet the outpoint "+
				"specified a %v tx tree instead", txHash,
				indicatedTree, txIn.PreviousOutPoint.Hash,
				originTxOPTree)
			return 0, ruleError(ErrDiscordantTxTree, errStr)
		}

		// The only transaction types that are allowed to spend from OP_SSTX
		// tagged outputs are votes and revocations.  So, check all the inputs
		// from non votes and revocations and make sure that they spend no
		// OP_SSTX tagged outputs.
		if !(isVote || isRevocation) {
			if txscript.GetScriptClass(
				utxoEntry.ScriptVersionByIndex(originTxIndex),
				utxoEntry.PkScriptByIndex(originTxIndex)) ==
				txscript.StakeSubmissionTy {

				errSSGen := stake.CheckSSGen(msgTx)
				errSSRtx := stake.CheckSSRtx(msgTx)
				errStr := fmt.Sprintf("Tx %v attempted to "+
					"spend an OP_SSTX tagged output, "+
					"however it was not an SSGen or SSRtx"+
					" tx; SSGen err: %v, SSRtx err: %v",
					txHash, errSSGen.Error(),
					errSSRtx.Error())
				return 0, ruleError(ErrTxSStxOutSpend, errStr)
			}
		}

		// OP_SSGEN and OP_SSRTX tagged outputs can only be spent after
		// coinbase maturity many blocks.
		scriptClass := txscript.GetScriptClass(
			utxoEntry.ScriptVersionByIndex(originTxIndex),
			utxoEntry.PkScriptByIndex(originTxIndex))
		if scriptClass == txscript.StakeGenTy ||
			scriptClass == txscript.StakeRevocationTy {
			originHeight := utxoEntry.BlockHeight()
			blocksSincePrev := txHeight - originHeight
			if blocksSincePrev <
				int64(chainParams.SStxChangeMaturity) {
				str := fmt.Sprintf("tried to spend OP_SSGEN or"+
					" OP_SSRTX output from tx %v from "+
					"height %v at height %v before "+
					"required maturity of %v blocks",
					txInHash, originHeight, txHeight,
					coinbaseMaturity)
				return 0, ruleError(ErrImmatureSpend, str)
			}
		}

		// Ticket change outputs may only be spent after ticket change
		// maturity many blocks.
		if scriptClass == txscript.StakeSubChangeTy {
			originHeight := utxoEntry.BlockHeight()
			blocksSincePrev := txHeight - originHeight
			if blocksSincePrev <
				int64(chainParams.SStxChangeMaturity) {
				str := fmt.Sprintf("tried to spend ticket change"+
					" output from tx %v from height %v at "+
					"height %v before required maturity "+
					"of %v blocks", txInHash, originHeight,
					txHeight, chainParams.SStxChangeMaturity)
				return 0, ruleError(ErrImmatureSpend, str)
			}
		}

		// Ensure the transaction amounts are in range.  Each of the
		// output values of the input transactions must not be negative
		// or more than the max allowed per transaction.  All amounts
		// in a transaction are in a unit value known as an atom.  One
		// Decred is a quantity of atoms as defined by the AtomPerCoin
		// constant.
		originTxAtom := utxoEntry.AmountByIndex(originTxIndex)
		if originTxAtom < 0 {
			str := fmt.Sprintf("transaction output has negative "+
				"value of %v", originTxAtom)
			return 0, ruleError(ErrBadTxOutValue, str)
		}
		if originTxAtom > dcrutil.MaxAmount {
			str := fmt.Sprintf("transaction output value of %v is "+
				"higher than max allowed value of %v",
				originTxAtom, dcrutil.MaxAmount)
			return 0, ruleError(ErrBadTxOutValue, str)
		}

		// The total of all outputs must not be more than the max
		// allowed per transaction.  Also, we could potentially
		// overflow the accumulator so check for overflow.
		lastAtomIn := totalAtomIn
		totalAtomIn += originTxAtom
		if totalAtomIn < lastAtomIn ||
			totalAtomIn > dcrutil.MaxAmount {
			str := fmt.Sprintf("total value of all transaction "+
				"inputs is %v which is higher than max "+
				"allowed value of %v", totalAtomIn,
				dcrutil.MaxAmount)
			return 0, ruleError(ErrBadTxOutValue, str)
		}
	}

	// Calculate the total output amount for this transaction.  It is safe
	// to ignore overflow and out of range errors here because those error
	// conditions would have already been caught by checkTransactionSanity.
	var totalAtomOut int64
	for _, txOut := range tx.MsgTx().TxOut {
		totalAtomOut += txOut.Value
	}

	// Ensure the transaction does not spend more than its inputs.
	if totalAtomIn < totalAtomOut {
		str := fmt.Sprintf("total value of all transaction inputs for "+
			"transaction %v is %v which is less than the amount "+
			"spent of %v", txHash, totalAtomIn, totalAtomOut)
		return 0, ruleError(ErrSpendTooHigh, str)
	}

	txFeeInAtom := totalAtomIn - totalAtomOut
	return txFeeInAtom, nil
}

// CountSigOps returns the number of signature operations for all transaction
// input and output scripts in the provided transaction.  This uses the
// quicker, but imprecise, signature operation counting mechanism from
// txscript.
func CountSigOps(tx *dcrutil.Tx, isCoinBaseTx bool, isSSGen bool) int {
	msgTx := tx.MsgTx()

	// Accumulate the number of signature operations in all transaction
	// inputs.
	totalSigOps := 0
	for i, txIn := range msgTx.TxIn {
		// Skip coinbase inputs.
		if isCoinBaseTx {
			continue
		}
		// Skip stakebase inputs.
		if isSSGen && i == 0 {
			continue
		}

		numSigOps := txscript.GetSigOpCount(txIn.SignatureScript)
		totalSigOps += numSigOps
	}

	// Accumulate the number of signature operations in all transaction
	// outputs.
	for _, txOut := range msgTx.TxOut {
		numSigOps := txscript.GetSigOpCount(txOut.PkScript)
		totalSigOps += numSigOps
	}

	return totalSigOps
}

// CountP2SHSigOps returns the number of signature operations for all input
// transactions which are of the pay-to-script-hash type.  This uses the
// precise, signature operation counting mechanism from the script engine which
// requires access to the input transaction scripts.
func CountP2SHSigOps(tx *dcrutil.Tx, isCoinBaseTx bool, isStakeBaseTx bool, view *UtxoViewpoint) (int, error) {
	// Coinbase transactions have no interesting inputs.
	if isCoinBaseTx {
		return 0, nil
	}

	// Stakebase (SSGen) transactions have no P2SH inputs.  Same with SSRtx,
	// but they will still pass the checks below.
	if isStakeBaseTx {
		return 0, nil
	}

	// Accumulate the number of signature operations in all transaction
	// inputs.
	msgTx := tx.MsgTx()
	totalSigOps := 0
	for txInIndex, txIn := range msgTx.TxIn {
		// Ensure the referenced input transaction is available.
		originTxHash := &txIn.PreviousOutPoint.Hash
		originTxIndex := txIn.PreviousOutPoint.Index
		utxoEntry := view.LookupEntry(originTxHash)
		if utxoEntry == nil || utxoEntry.IsOutputSpent(originTxIndex) {
			str := fmt.Sprintf("output %v referenced from "+
				"transaction %s:%d either does not exist or "+
				"has already been spent", txIn.PreviousOutPoint,
				tx.Hash(), txInIndex)
			return 0, ruleError(ErrMissingTxOut, str)
		}

		// We're only interested in pay-to-script-hash types, so skip
		// this input if it's not one.
		pkScript := utxoEntry.PkScriptByIndex(originTxIndex)
		if !txscript.IsPayToScriptHash(pkScript) {
			continue
		}

		// Count the precise number of signature operations in the
		// referenced public key script.
		sigScript := txIn.SignatureScript
		numSigOps := txscript.GetPreciseSigOpCount(sigScript, pkScript,
			true)

		// We could potentially overflow the accumulator so check for
		// overflow.
		lastSigOps := totalSigOps
		totalSigOps += numSigOps
		if totalSigOps < lastSigOps {
			str := fmt.Sprintf("the public key script from output "+
				"%v contains too many signature operations - "+
				"overflow", txIn.PreviousOutPoint)
			return 0, ruleError(ErrTooManySigOps, str)
		}
	}

	return totalSigOps, nil
}

// checkNumSigOps Checks the number of P2SH signature operations to make
// sure they don't overflow the limits.  It takes a cumulative number of sig
// ops as an argument and increments will each call.
// TxTree true == Regular, false == Stake
func checkNumSigOps(tx *dcrutil.Tx, view *UtxoViewpoint, index int, txTree bool, cumulativeSigOps int) (int, error) {
	msgTx := tx.MsgTx()
	isSSGen := stake.IsSSGen(msgTx)
	numsigOps := CountSigOps(tx, (index == 0) && txTree, isSSGen)

	// Since the first (and only the first) transaction has already been
	// verified to be a coinbase transaction, use (i == 0) && TxTree as an
	// optimization for the flag to countP2SHSigOps for whether or not the
	// transaction is a coinbase transaction rather than having to do a
	// full coinbase check again.
	numP2SHSigOps, err := CountP2SHSigOps(tx, (index == 0) && txTree,
		isSSGen, view)
	if err != nil {
		log.Tracef("CountP2SHSigOps failed; error returned %v", err)
		return 0, err
	}

	startCumSigOps := cumulativeSigOps
	cumulativeSigOps += numsigOps
	cumulativeSigOps += numP2SHSigOps

	// Check for overflow or going over the limits.  We have to do
	// this on every loop iteration to avoid overflow.
	if cumulativeSigOps < startCumSigOps ||
		cumulativeSigOps > MaxSigOpsPerBlock {
		str := fmt.Sprintf("block contains too many signature "+
			"operations - got %v, max %v", cumulativeSigOps,
			MaxSigOpsPerBlock)
		return 0, ruleError(ErrTooManySigOps, str)
	}

	return cumulativeSigOps, nil
}

// checkStakeBaseAmounts calculates the total amount given as subsidy from
// single stakebase transactions (votes) within a block.  This function skips a
// ton of checks already performed by CheckTransactionInputs.
func checkStakeBaseAmounts(subsidyCache *SubsidyCache, height int64, params *chaincfg.Params, txs []*dcrutil.Tx, view *UtxoViewpoint) error {
	for _, tx := range txs {
		msgTx := tx.MsgTx()
		if stake.IsSSGen(msgTx) {
			// Ensure the input is available.
			txInHash := &msgTx.TxIn[1].PreviousOutPoint.Hash
			utxoEntry, exists := view.entries[*txInHash]
			if !exists || utxoEntry == nil {
				str := fmt.Sprintf("couldn't find input tx %v "+
					"for stakebase amounts check", txInHash)
				return ruleError(ErrTicketUnavailable, str)
			}

			originTxIndex := msgTx.TxIn[1].PreviousOutPoint.Index
			originTxAtom := utxoEntry.AmountByIndex(originTxIndex)

			totalOutputs := int64(0)
			// Sum up the outputs.
			for _, out := range msgTx.TxOut {
				totalOutputs += out.Value
			}

			difference := totalOutputs - originTxAtom

			// Subsidy aligns with the height we're voting on, not
			// with the height of the current block.
			calcSubsidy := CalcStakeVoteSubsidy(subsidyCache,
				height-1, params)

			if difference > calcSubsidy {
				str := fmt.Sprintf("ssgen tx %v spent more "+
					"than allowed (spent %v, allowed %v)",
					tx.Hash(), difference, calcSubsidy)
				return ruleError(ErrSSGenSubsidy, str)
			}
		}
	}

	return nil
}

// getStakeBaseAmounts calculates the total amount given as subsidy from the
// collective stakebase transactions (votes) within a block.  This function
// skips a ton of checks already performed by CheckTransactionInputs.
func getStakeBaseAmounts(txs []*dcrutil.Tx, view *UtxoViewpoint) (int64, error) {
	totalInputs := int64(0)
	totalOutputs := int64(0)
	for _, tx := range txs {
		msgTx := tx.MsgTx()
		if stake.IsSSGen(msgTx) {
			// Ensure the input is available.
			txInHash := &msgTx.TxIn[1].PreviousOutPoint.Hash
			utxoEntry, exists := view.entries[*txInHash]
			if !exists || utxoEntry == nil {
				str := fmt.Sprintf("couldn't find input tx %v "+
					"for stakebase amounts get", txInHash)
				return 0, ruleError(ErrTicketUnavailable, str)
			}

			originTxIndex := msgTx.TxIn[1].PreviousOutPoint.Index
			originTxAtom := utxoEntry.AmountByIndex(originTxIndex)

			totalInputs += originTxAtom

			// Sum up the outputs.
			for _, out := range msgTx.TxOut {
				totalOutputs += out.Value
			}
		}
	}

	return totalOutputs - totalInputs, nil
}

// getStakeTreeFees determines the amount of fees for in the stake tx tree of
// some node given a transaction store.
func getStakeTreeFees(subsidyCache *SubsidyCache, height int64, params *chaincfg.Params, txs []*dcrutil.Tx, view *UtxoViewpoint) (dcrutil.Amount, error) {
	totalInputs := int64(0)
	totalOutputs := int64(0)
	for _, tx := range txs {
		msgTx := tx.MsgTx()
		isSSGen := stake.IsSSGen(msgTx)

		for i, in := range msgTx.TxIn {
			// Ignore stakebases.
			if isSSGen && i == 0 {
				continue
			}

			txInHash := &in.PreviousOutPoint.Hash
			utxoEntry, exists := view.entries[*txInHash]
			if !exists || utxoEntry == nil {
				str := fmt.Sprintf("couldn't find input tx "+
					"%v for stake tree fee calculation",
					txInHash)
				return 0, ruleError(ErrTicketUnavailable, str)
			}

			originTxIndex := in.PreviousOutPoint.Index
			originTxAtom := utxoEntry.AmountByIndex(originTxIndex)

			totalInputs += originTxAtom
		}

		for _, out := range msgTx.TxOut {
			totalOutputs += out.Value
		}

		// For votes, subtract the subsidy to determine actual fees.
		if isSSGen {
			// Subsidy aligns with the height we're voting on, not
			// with the height of the current block.
			totalOutputs -= CalcStakeVoteSubsidy(subsidyCache,
				height-1, params)
		}
	}

	if totalInputs < totalOutputs {
		str := fmt.Sprintf("negative cumulative fees found in stake " +
			"tx tree")
		return 0, ruleError(ErrStakeFees, str)
	}

	return dcrutil.Amount(totalInputs - totalOutputs), nil
}

// checkTransactionsAndConnect is the local function used to check the
// transaction inputs for a transaction list given a predetermined TxStore.
// After ensuring the transaction is valid, the transaction is connected to the
// UTXO viewpoint.  TxTree true == Regular, false == Stake
func (b *BlockChain) checkTransactionsAndConnect(subsidyCache *SubsidyCache, inputFees dcrutil.Amount, node *blockNode, txs []*dcrutil.Tx, view *UtxoViewpoint, stxos *[]spentTxOut, txTree bool) error {
	// Perform several checks on the inputs for each transaction.  Also
	// accumulate the total fees.  This could technically be combined with
	// the loop above instead of running another loop over the
	// transactions, but by separating it we can avoid running the more
	// expensive (though still relatively cheap as compared to running the
	// scripts) checks against all the inputs when the signature operations
	// are out of bounds.
	totalFees := int64(inputFees) // Stake tx tree carry forward
	var cumulativeSigOps int
	for idx, tx := range txs {
		// Ensure that the number of signature operations is not beyond
		// the consensus limit.
		var err error
		cumulativeSigOps, err = checkNumSigOps(tx, view, idx, txTree,
			cumulativeSigOps)
		if err != nil {
			return err
		}

		// This step modifies the txStore and marks the tx outs used
		// spent, so be aware of this.
		txFee, err := CheckTransactionInputs(b.subsidyCache, tx,
			node.height, view, true, /* check fraud proofs */
			b.chainParams)
		if err != nil {
			log.Tracef("CheckTransactionInputs failed; error "+
				"returned: %v", err)
			return err
		}

		// Sum the total fees and ensure we don't overflow the
		// accumulator.
		lastTotalFees := totalFees
		totalFees += txFee
		if totalFees < lastTotalFees {
			return ruleError(ErrBadFees, "total fees for block "+
				"overflows accumulator")
		}

		// Connect the transaction to the UTXO viewpoint, so that in
		// flight transactions may correctly validate.
		err = view.connectTransaction(tx, node.height, uint32(idx),
			stxos)
		if err != nil {
			return err
		}
	}

	// The total output values of the coinbase transaction must not exceed
	// the expected subsidy value plus total transaction fees gained from
	// mining the block.  It is safe to ignore overflow and out of range
	// errors here because those error conditions would have already been
	// caught by checkTransactionSanity.
	if txTree { //TxTreeRegular
		// Apply penalty to fees if we're at stake validation height.
		if node.height >= b.chainParams.StakeValidationHeight {
			totalFees *= int64(node.voters)
			totalFees /= int64(b.chainParams.TicketsPerBlock)
		}

		var totalAtomOutRegular int64

		for _, txOut := range txs[0].MsgTx().TxOut {
			totalAtomOutRegular += txOut.Value
		}

		var expAtomOut int64
		if node.height == 1 {
			expAtomOut = subsidyCache.CalcBlockSubsidy(node.height)
		} else {
			subsidyWork := CalcBlockWorkSubsidy(subsidyCache,
				node.height, node.voters, b.chainParams)
			subsidyTax := CalcBlockTaxSubsidy(subsidyCache,
				node.height, node.voters, b.chainParams)
			expAtomOut = subsidyWork + subsidyTax + totalFees
		}

		// AmountIn for the input should be equal to the subsidy.
		coinbaseIn := txs[0].MsgTx().TxIn[0]
		subsidyWithoutFees := expAtomOut - totalFees
		if (coinbaseIn.ValueIn != subsidyWithoutFees) &&
			(node.height > 0) {
			errStr := fmt.Sprintf("bad coinbase subsidy in input;"+
				" got %v, expected %v", coinbaseIn.ValueIn,
				subsidyWithoutFees)
			return ruleError(ErrBadCoinbaseAmountIn, errStr)
		}

		if totalAtomOutRegular > expAtomOut {
			str := fmt.Sprintf("coinbase transaction for block %v"+
				" pays %v which is more than expected value "+
				"of %v", node.hash, totalAtomOutRegular,
				expAtomOut)
			return ruleError(ErrBadCoinbaseValue, str)
		}
	} else { // TxTreeStake
		if len(txs) == 0 &&
			node.height < b.chainParams.StakeValidationHeight {
			return nil
		}
		if len(txs) == 0 &&
			node.height >= b.chainParams.StakeValidationHeight {
			str := fmt.Sprintf("empty tx tree stake in block " +
				"after stake validation height")
			return ruleError(ErrNoStakeTx, str)
		}

		err := checkStakeBaseAmounts(subsidyCache, node.height,
			b.chainParams, txs, view)
		if err != nil {
			return err
		}

		totalAtomOutStake, err := getStakeBaseAmounts(txs, view)
		if err != nil {
			return err
		}

		var expAtomOut int64
		if node.height >= b.chainParams.StakeValidationHeight {
			// Subsidy aligns with the height we're voting on, not
			// with the height of the current block.
			expAtomOut = CalcStakeVoteSubsidy(subsidyCache,
				node.height-1, b.chainParams) *
				int64(node.voters)
		} else {
			expAtomOut = totalFees
		}

		if totalAtomOutStake > expAtomOut {
			str := fmt.Sprintf("stakebase transactions for block "+
				"pays %v which is more than expected value "+
				"of %v", totalAtomOutStake, expAtomOut)
			return ruleError(ErrBadStakebaseValue, str)
		}
	}

	return nil
}

// consensusScriptVerifyFlags returns the script flags that must be used when
// executing transaction scripts to enforce the consensus rules. This includes
// any flags required as the result of any agendas that have passed and become
// active.
func (b *BlockChain) consensusScriptVerifyFlags(node *blockNode) (txscript.ScriptFlags, error) {
	scriptFlags := txscript.ScriptVerifyCleanStack |
		txscript.ScriptVerifyCheckLockTimeVerify

	// Enable enforcement of OP_CSV and OP_SHA256 if the stake vote
	// for the agenda is active.
	lnFeaturesActive, err := b.isLNFeaturesAgendaActive(node.parent)
	if err != nil {
		return 0, err
	}
	if lnFeaturesActive {
		scriptFlags |= txscript.ScriptVerifyCheckSequenceVerify
		scriptFlags |= txscript.ScriptVerifySHA256
	}
	return scriptFlags, err
}

// checkConnectBlock performs several checks to confirm connecting the passed
// block to the chain represented by the passed view does not violate any
// rules.  In addition, the passed view is updated to spend all of the
// referenced outputs and add all of the new utxos created by block.  Thus, the
// view will represent the state of the chain as if the block were actually
// connected and consequently the best hash for the view is also updated to
// passed block.
//
// An example of some of the checks performed are ensuring connecting the block
// would not cause any duplicate transaction hashes for old transactions that
// aren't already fully spent, double spends, exceeding the maximum allowed
// signature operations per block, invalid values in relation to the expected
// block subsidy, or fail transaction script validation.
//
// The CheckConnectBlockTemplate function makes use of this function to perform
// the bulk of its work.
//
// This function MUST be called with the chain state lock held (for writes).
func (b *BlockChain) checkConnectBlock(node *blockNode, block, parent *dcrutil.Block, view *UtxoViewpoint, stxos *[]spentTxOut) error {
	// If the side chain blocks end up in the database, a call to
	// CheckBlockSanity should be done here in case a previous version
	// allowed a block that is no longer valid.  However, since the
	// implementation only currently uses memory for the side chain blocks,
	// it isn't currently necessary.

	// Ensure the view is for the node being checked.
	parentHash := &block.MsgBlock().Header.PrevBlock
	if !view.BestHash().IsEqual(parentHash) {
		return AssertError(fmt.Sprintf("inconsistent view when "+
			"checking block connection: best hash is %v instead "+
			"of expected %v", view.BestHash(), parentHash))
	}

	// Check that the coinbase pays the tax, if applicable.
	err := CoinbasePaysTax(b.subsidyCache, block.Transactions()[0], node.height,
		node.voters, b.chainParams)
	if err != nil {
		return err
	}

	// Don't run scripts if this node is before the latest known good
	// checkpoint since the validity is verified via the checkpoints (all
	// transactions are included in the merkle root hash and any changes
	// will therefore be detected by the next checkpoint).  This is a huge
	// optimization because running the scripts is the most time consuming
	// portion of block handling.
	checkpoint := b.latestCheckpoint()
	runScripts := !b.noVerify
	if checkpoint != nil && node.height <= checkpoint.Height {
		runScripts = false
	}
	var scriptFlags txscript.ScriptFlags
	if runScripts {
		var err error
		scriptFlags, err = b.consensusScriptVerifyFlags(node)
		if err != nil {
			return err
		}
	}

	// Disconnect all of the transactions in the regular transaction tree of
	// the parent if the block being checked votes against it.
	if node.height > 1 && !voteBitsApproveParent(node.voteBits) {
		err := view.disconnectDisapprovedBlock(b.db, parent)
		if err != nil {
			return err
		}
	}

	// Ensure the stake transaction tree does not contain any transactions
	// that 'overwrite' older transactions which are not fully spent.
	err = b.checkDupTxs(block.STransactions(), view)
	if err != nil {
		log.Tracef("checkDupTxs failed for cur TxTreeStake: %v", err)
		return err
	}

	// Load all of the utxos referenced by the inputs for all transactions
	// in the block don't already exist in the utxo view from the database.
	//
	// These utxo entries are needed for verification of things such as
	// transaction inputs, counting pay-to-script-hashes, and scripts.
	err = view.fetchInputUtxos(b.db, block)
	if err != nil {
		return err
	}

	err = b.checkTransactionsAndConnect(b.subsidyCache, 0, node,
		block.STransactions(), view, stxos, false)
	if err != nil {
		log.Tracef("checkTransactionsAndConnect failed for "+
			"TxTreeStake: %v", err)
		return err
	}

	stakeTreeFees, err := getStakeTreeFees(b.subsidyCache, node.height,
		b.chainParams, block.STransactions(), view)
	if err != nil {
		log.Tracef("getStakeTreeFees failed for TxTreeStake: %v", err)
		return err
	}

	// Enforce all relative lock times via sequence numbers for the stake
	// transaction tree once the stake vote for the agenda is active.
	var prevMedianTime time.Time
	lnFeaturesActive, err := b.isLNFeaturesAgendaActive(node.parent)
	if err != nil {
		return err
	}
	if lnFeaturesActive {
		// Use the past median time of the *previous* block in order
		// to determine if the transactions in the current block are
		// final.
		prevMedianTime = node.parent.CalcPastMedianTime()

		for _, stx := range block.STransactions() {
			sequenceLock, err := b.calcSequenceLock(node, stx,
				view, true, true)
			if err != nil {
				return err
			}
			if !SequenceLockActive(sequenceLock, node.height,
				prevMedianTime) {

				str := fmt.Sprintf("block contains " +
					"stake transaction whose input " +
					"sequence locks are not met")
				return ruleError(ErrUnfinalizedTx, str)
			}
		}
	}

	if runScripts {
		err = checkBlockScripts(block, view, false, scriptFlags,
			b.sigCache)
		if err != nil {
			log.Tracef("checkBlockScripts failed; error returned "+
				"on txtreestake of cur block: %v", err)
			return err
		}
	}

	// Ensure the regular transaction tree does not contain any transactions
	// that 'overwrite' older transactions which are not fully spent.
	err = b.checkDupTxs(block.Transactions(), view)
	if err != nil {
		log.Tracef("checkDupTxs failed for cur TxTreeRegular: %v", err)
		return err
	}

	err = b.checkTransactionsAndConnect(b.subsidyCache, stakeTreeFees, node,
		block.Transactions(), view, stxos, true)
	if err != nil {
		log.Tracef("checkTransactionsAndConnect failed for cur "+
			"TxTreeRegular: %v", err)
		return err
	}

	// Enforce all relative lock times via sequence numbers for the regular
	// transaction tree once the stake vote for the agenda is active.
	if lnFeaturesActive {
		// Skip the coinbase since it does not have any inputs and thus
		// lock times do not apply.
		for _, tx := range block.Transactions()[1:] {
			sequenceLock, err := b.calcSequenceLock(node, tx, view,
				true, true)
			if err != nil {
				return err
			}
			if !SequenceLockActive(sequenceLock, node.height,
				prevMedianTime) {

				str := fmt.Sprintf("block contains " +
					"transaction whose input sequence " +
					"locks are not met")
				return ruleError(ErrUnfinalizedTx, str)
			}
		}
	}

	if runScripts {
		err = checkBlockScripts(block, view, true, scriptFlags,
			b.sigCache)
		if err != nil {
			log.Tracef("checkBlockScripts failed; error returned "+
				"on txtreeregular of cur block: %v", err)
			return err
		}
	}

	// First block has special rules concerning the ledger.
	if node.height == 1 {
		err := BlockOneCoinbasePaysTokens(block.Transactions()[0],
			b.chainParams)
		if err != nil {
			return err
		}
	}

	// Update the best hash for view to include this block since all of its
	// transactions have been connected.
	view.SetBestHash(&node.hash)

	return nil
}

// CheckConnectBlockTemplate fully validates that connecting the passed block to
// either the tip of the main chain or its parent does not violate any consensus
// rules, aside from the proof of work requirement.  The block must connect to
// the current tip of the main chain or its parent.
//
// This function is safe for concurrent access.
func (b *BlockChain) CheckConnectBlockTemplate(block *dcrutil.Block) error {
	b.chainLock.Lock()
	defer b.chainLock.Unlock()

	// Skip the proof of work check as this is just a block template.
	flags := BFNoPoWCheck

	// The block template must build off the current tip of the main chain
	// or its parent.
	tip := b.bestChain.Tip()
	var prevNode *blockNode
	parentHash := block.MsgBlock().Header.PrevBlock
	if parentHash == tip.hash {
		prevNode = tip
	} else if tip.parent != nil && parentHash == tip.parent.hash {
		prevNode = tip.parent
	}
	if prevNode == nil {
		var str string
		if tip.parent != nil {
			str = fmt.Sprintf("previous block must be the current chain tip "+
				"%s or its parent %s, but got %s", tip.hash, tip.parent.hash,
				parentHash)
		} else {
			str = fmt.Sprintf("previous block must be the current chain tip "+
				"%s, but got %s", tip.hash, parentHash)
		}
		return ruleError(ErrInvalidTemplateParent, str)
	}

	// Perform context-free sanity checks on the block and its transactions.
	err := checkBlockSanity(block, b.timeSource, flags, b.chainParams)
	if err != nil {
		return err
	}

	// The block must pass all of the validation rules which depend on having
	// the headers of all ancestors available, but do not rely on having the
	// full block data of all ancestors available.
	err = b.checkBlockPositional(block, prevNode, flags)
	if err != nil {
		return err
	}

	// The block must pass all of the validation rules which depend on having
	// the full block data for all of its ancestors available.
	err = b.checkBlockContext(block, prevNode, flags)
	if err != nil {
		return err
	}

	newNode := newBlockNode(&block.MsgBlock().Header, prevNode)
	newNode.populateTicketInfo(stake.FindSpentTicketsInBlock(block.MsgBlock()))

	// Use the chain state as is when extending the main (best) chain.
	if prevNode.hash == tip.hash {
		// Grab the parent block since it is required throughout the block
		// connection process.
		parent, err := b.fetchMainChainBlockByNode(prevNode)
		if err != nil {
			return ruleError(ErrMissingParent, err.Error())
		}

		view := NewUtxoViewpoint()
		view.SetBestHash(&tip.hash)

		return b.checkConnectBlock(newNode, block, parent, view, nil)
	}

	// At this point, the block template must be building on the parent of the
	// current tip due to the previous checks, so undo the transactions and
	// spend information for the tip block to reach the point of view of the
	// block template.
	view := NewUtxoViewpoint()
	view.SetBestHash(&tip.hash)
	tipBlock, err := b.fetchMainChainBlockByNode(tip)
	if err != nil {
		return err
	}
	parent, err := b.fetchMainChainBlockByNode(tip.parent)
	if err != nil {
		return err
	}

	// Load all of the spent txos for the tip block from the spend journal.
	var stxos []spentTxOut
	err = b.db.View(func(dbTx database.Tx) error {
		stxos, err = dbFetchSpendJournalEntry(dbTx, tipBlock)
		return err
	})
	if err != nil {
		return err
	}

	// Update the view to unspend all of the spent txos and remove the utxos
	// created by the tip block.  Also, if the block votes against its parent,
	// reconnect all of the regular transactions.
	err = view.disconnectBlock(b.db, tipBlock, parent, stxos)
	if err != nil {
		return err
	}

	// The view is now from the point of view of the parent of the current tip
	// block.  Ensure the block template can be connected without violating any
	// rules.
	return b.checkConnectBlock(newNode, block, parent, view, nil)
}
