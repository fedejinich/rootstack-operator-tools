package rskblocks

import (
	"bytes"
	"fmt"
	"io"
	"math/big"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
	"golang.org/x/crypto/sha3"
)

// Transaction represents an RSK transaction.
type Transaction struct {
	data txdata
	// caches
	hash atomic.Value
	size atomic.Value
}

type txdata struct {
	AccountNonce uint64          `json:"nonce"    gencodec:"required"`
	Price        *big.Int        `json:"gasPrice" gencodec:"required"`
	GasLimit     uint64          `json:"gas"      gencodec:"required"`
	Recipient    *common.Address `json:"to"       rlp:"nil"` // nil means contract creation
	Amount       *big.Int        `json:"value"    gencodec:"required"`
	Payload      []byte          `json:"input"    gencodec:"required"`

	// Signature values
	V *big.Int `json:"v" gencodec:"required"`
	R *big.Int `json:"r" gencodec:"required"`
	S *big.Int `json:"s" gencodec:"required"`

	// This is only used when marshaling to JSON.
	Hash *common.Hash `json:"hash" rlp:"-"`
}

func NewTransaction(nonce uint64, to common.Address, amount *big.Int, gasLimit uint64, gasPrice *big.Int, data []byte) *Transaction {
	return newTransaction(nonce, &to, amount, gasLimit, gasPrice, data)
}

func NewContractCreation(nonce uint64, amount *big.Int, gasLimit uint64, gasPrice *big.Int, data []byte) *Transaction {
	return newTransaction(nonce, nil, amount, gasLimit, gasPrice, data)
}

func newTransaction(nonce uint64, to *common.Address, amount *big.Int, gasLimit uint64, gasPrice *big.Int, data []byte) *Transaction {
	if len(data) > 0 {
		data = common.CopyBytes(data)
	}
	d := txdata{
		AccountNonce: nonce,
		Recipient:    to,
		Payload:      data,
		GasLimit:     gasLimit,
		Price:        new(big.Int),
		Amount:       new(big.Int),
		V:            new(big.Int),
		R:            new(big.Int),
		S:            new(big.Int),
	}
	if amount != nil {
		d.Amount.Set(amount)
	}
	if gasPrice != nil {
		d.Price.Set(gasPrice)
	}

	return &Transaction{data: d}
}

// NewSignedTransaction creates a transaction with signature values (V, R, S) already set.
// This is useful when reconstructing transactions from RPC data.
func NewSignedTransaction(nonce uint64, to *common.Address, amount *big.Int, gasLimit uint64, gasPrice *big.Int, data []byte, v, r, s *big.Int) *Transaction {
	tx := newTransaction(nonce, to, amount, gasLimit, gasPrice, data)
	if v != nil {
		tx.data.V.Set(v)
	}
	if r != nil {
		tx.data.R.Set(r)
	}
	if s != nil {
		tx.data.S.Set(s)
	}
	return tx
}

// EncodeRLP implements rlp.Encoder
// This uses RSK's custom encoding for internal transactions (like REMASC)
// or standard Ethereum encoding for external signed transactions.
// The detection is based on whether the transaction has a signature.
func (tx *Transaction) EncodeRLP(w io.Writer) error {
	// If this is a signed external transaction, use standard Ethereum encoding
	// REMASC and other internal RSK transactions have V=0, R=0, S=0
	if tx.isSignedExternal() {
		return rlp.Encode(w, tx.ethRLPFields())
	}
	// Use RSK's custom encoding for internal transactions
	return rlp.Encode(w, tx.rskRLPFields())
}

// isSignedExternal returns true if this transaction has a valid external signature
// (i.e., not a REMASC or other internal RSK transaction)
func (tx *Transaction) isSignedExternal() bool {
	// External transactions have non-zero R and S values
	return tx.data.R != nil && tx.data.R.Sign() != 0 &&
		tx.data.S != nil && tx.data.S.Sign() != 0
}

// ethRLPFields returns fields formatted for standard Ethereum RLP encoding
// This is used for transactions created by external tools like cast/foundry
func (tx *Transaction) ethRLPFields() []interface{} {
	// Standard Ethereum encoding: zeros are encoded as empty (0x80)
	var nonce interface{}
	if tx.data.AccountNonce == 0 {
		nonce = []byte{}
	} else {
		nonce = tx.data.AccountNonce
	}

	var gasPrice interface{}
	if tx.data.Price == nil || tx.data.Price.Sign() == 0 {
		gasPrice = []byte{} // Standard: empty for zero
	} else {
		gasPrice = tx.data.Price.Bytes()
	}

	gasLimit := tx.data.GasLimit

	var to interface{}
	if tx.data.Recipient == nil {
		to = []byte{}
	} else {
		to = tx.data.Recipient.Bytes()
	}

	var value interface{}
	if tx.data.Amount == nil || tx.data.Amount.Sign() == 0 {
		value = []byte{}
	} else {
		value = tx.data.Amount.Bytes()
	}

	data := tx.data.Payload
	if data == nil {
		data = []byte{}
	}

	var v, r, s interface{}
	if tx.data.V == nil || tx.data.V.Sign() == 0 {
		v = []byte{}
	} else {
		v = tx.data.V.Bytes()
	}
	if tx.data.R == nil || tx.data.R.Sign() == 0 {
		r = []byte{}
	} else {
		r = tx.data.R.Bytes()
	}
	if tx.data.S == nil || tx.data.S.Sign() == 0 {
		s = []byte{}
	} else {
		s = tx.data.S.Bytes()
	}

	return []interface{}{nonce, gasPrice, gasLimit, to, value, data, v, r, s}
}

// rskRLPFields returns the fields formatted for RSK's RLP encoding
func (tx *Transaction) rskRLPFields() []interface{} {
	// Nonce: 0 is encoded as nil (empty)
	var nonce interface{}
	if tx.data.AccountNonce == 0 {
		nonce = []byte{} // RLP encodes empty slice as 0x80
	} else {
		nonce = tx.data.AccountNonce
	}

	// GasPrice: RSK's encodeCoinNonNullZero
	// - nil -> empty
	// - 0 -> [0x00] (single zero byte, NOT the RLP empty encoding)
	var gasPrice interface{}
	if tx.data.Price == nil || tx.data.Price.Sign() == 0 {
		gasPrice = []byte{0x00} // Single zero byte
	} else {
		gasPrice = tx.data.Price.Bytes()
	}

	// GasLimit: standard encoding
	var gasLimit interface{}
	if tx.data.GasLimit == 0 {
		gasLimit = []byte{0x00} // RSK encodes gas limit [0] as single zero byte
	} else {
		gasLimit = tx.data.GasLimit
	}

	// Recipient/To address: RSK's encodeRskAddress
	// - null address (all zeros) or nil -> empty
	var to interface{}
	if tx.data.Recipient == nil || *tx.data.Recipient == (common.Address{}) {
		to = []byte{} // Empty for null address
	} else {
		to = tx.data.Recipient.Bytes()
	}

	// Value: RSK's encodeCoinNullZero
	// - 0 -> encoded as RLP byte 0 which becomes 0x80 (empty string)
	var value interface{}
	if tx.data.Amount == nil || tx.data.Amount.Sign() == 0 {
		value = []byte{}
	} else {
		value = tx.data.Amount.Bytes()
	}

	// Data/Input: standard encoding
	data := tx.data.Payload
	if data == nil {
		data = []byte{}
	}

	// V, R, S: for REMASC transactions, all are 0
	var v, r, s interface{}

	if tx.data.V == nil || tx.data.V.Sign() == 0 {
		v = []byte{} // Empty for v=0
	} else {
		v = tx.data.V.Bytes()
	}

	if tx.data.R == nil || tx.data.R.Sign() == 0 {
		r = []byte{} // Empty for r=0
	} else {
		r = tx.data.R.Bytes()
	}

	if tx.data.S == nil || tx.data.S.Sign() == 0 {
		s = []byte{} // Empty for s=0
	} else {
		s = tx.data.S.Bytes()
	}

	return []interface{}{nonce, gasPrice, gasLimit, to, value, data, v, r, s}
}

// GetEncodedRLP returns the RLP encoded bytes of the transaction
func (tx *Transaction) GetEncodedRLP() ([]byte, error) {
	var buf bytes.Buffer
	if err := tx.EncodeRLP(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DecodeRLP implements rlp.Decoder
func (tx *Transaction) DecodeRLP(s *rlp.Stream) error {
	_, size, _ := s.Kind()
	err := s.Decode(&tx.data)
	if err == nil {
		tx.size.Store(common.StorageSize(rlp.ListSize(size)))
	}
	return err
}

func (tx *Transaction) Hash() common.Hash {
	if hash := tx.hash.Load(); hash != nil {
		return hash.(common.Hash)
	}
	v := rlpHash(tx)
	tx.hash.Store(v)
	return v
}

func rlpHash(x interface{}) (h common.Hash) {
	hw := sha3.NewLegacyKeccak256()
	if err := rlp.Encode(hw, x); err != nil {
		// A partial write would leave the hasher with truncated input and
		// produce a wrong-but-cached Hash() — surface the programming error
		// loudly instead of returning a corrupted hash.
		panic(fmt.Errorf("rlp encode for hash: %w", err))
	}
	hw.Sum(h[:0])
	return h
}

func (tx *Transaction) Nonce() uint64 { return tx.data.AccountNonce }
func (tx *Transaction) GasPrice() *big.Int {
	if tx.data.Price == nil {
		return new(big.Int)
	}
	return new(big.Int).Set(tx.data.Price)
}
func (tx *Transaction) Gas() uint64 { return tx.data.GasLimit }
func (tx *Transaction) Value() *big.Int {
	if tx.data.Amount == nil {
		return new(big.Int)
	}
	return new(big.Int).Set(tx.data.Amount)
}
func (tx *Transaction) Data() []byte { return common.CopyBytes(tx.data.Payload) }
func (tx *Transaction) To() *common.Address {
	if tx.data.Recipient == nil {
		return nil
	}
	to := *tx.data.Recipient
	return &to
}
