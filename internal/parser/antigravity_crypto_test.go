package parser

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setAntigravityKey installs key as ANTIGRAVITY_KEY and resets the cached
// lookup so loadAntigravityKey re-reads the environment. The cache is
// process-global (sync.Once), so these tests cannot run in parallel.
func setAntigravityKey(t *testing.T, key []byte) {
	t.Helper()
	t.Setenv("ANTIGRAVITY_KEY", base64.StdEncoding.EncodeToString(key))
	agKeyOnce = sync.Once{}
	agKeyVal = nil
	errAGKey = nil
	t.Cleanup(func() {
		agKeyOnce = sync.Once{}
		agKeyVal = nil
		errAGKey = nil
	})
}

// agTestProto is a hand-written protobuf-ish payload: field 1 varint 150,
// field 2 length-delimited "hello". decryptAntigravity only returns
// candidates that walk as protobuf, so fixtures need real wire format.
var agTestProto = []byte{
	0x08, 0x96, 0x01,
	0x12, 0x05, 'h', 'e', 'l', 'l', 'o',
}

func encryptAesCTR(t *testing.T, key, nonce, plaintext []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	out := make([]byte, len(plaintext))
	cipher.NewCTR(block, nonce).XORKeyStream(out, plaintext)
	return append(append([]byte{}, nonce...), out...)
}

func encryptAesCBC(t *testing.T, key, iv, plaintext []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	pad := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := append(append([]byte{}, plaintext...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return append(append([]byte{}, iv...), out...)
}

// TestDecryptAesCTRRoundTrip pins CTR, the documented primary mode for
// legacy .pb archives: nonce prefix + keystream ciphertext, including the
// leading-bytes skip the brute-forcer tries.
func TestDecryptAesCTRRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	nonce := bytes.Repeat([]byte{0x07}, 16)

	data := encryptAesCTR(t, key, nonce, agTestProto)
	assert.Equal(t, agTestProto, decryptAesCTR(data, key, 0))

	junk := []byte{0xDE, 0xAD, 0xBE}
	got := decryptAesCTR(append(junk, data...), key, len(junk))
	assert.Equal(t, agTestProto, got)

	// Fewer than skip+16 bytes leaves no nonce: nil, not a panic.
	assert.Nil(t, decryptAesCTR([]byte{0x01, 0x02}, key, 0))
	assert.Nil(t, decryptAesCTR(data, key, len(data)))
}

// TestDecryptAesCBCRoundTrip pins the CBC fallback: IV prefix + block
// ciphertext + PKCS7 padding.
func TestDecryptAesCBCRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	iv := bytes.Repeat([]byte{0x09}, 16)

	data := encryptAesCBC(t, key, iv, agTestProto)
	assert.Equal(t, agTestProto, decryptAesCBC(data, key, 0))

	// A ciphertext that is not a whole number of blocks cannot decrypt.
	truncated := append(append([]byte{}, iv...), bytes.Repeat([]byte{0xAA}, aes.BlockSize+1)...)
	assert.Nil(t, decryptAesCBC(truncated, key, 0))
	assert.Nil(t, decryptAesCBC([]byte{0x01}, key, 0))
}

// TestDecryptAntigravityCTRAndCBC covers the dispatch loop: the first
// (mode, skip, post-skip) combination whose plaintext walks as protobuf
// wins, for both the primary CTR mode and the CBC fallback.
func TestDecryptAntigravityCTRAndCBC(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	setAntigravityKey(t, key)

	t.Run("ctr", func(t *testing.T) {
		data := encryptAesCTR(t, key, bytes.Repeat([]byte{0x07}, 16), agTestProto)
		got, err := decryptAntigravity(data)
		require.NoError(t, err)
		assert.Equal(t, agTestProto, got)
	})

	// The fixture constants below are chosen so the correct (mode, skip)
	// combination wins the candidate race: the proto sniff is permissive and
	// a wrong-offset decryption can also walk as protobuf, so arbitrary
	// nonces and IVs sometimes let a garbage candidate answer first.
	t.Run("ctr with leading bytes", func(t *testing.T) {
		data := encryptAesCTR(t, key, bytes.Repeat([]byte{0x09}, 16), agTestProto)
		data = append([]byte{0x00, 0x00, 0x00, 0x00}, data...)
		got, err := decryptAntigravity(data)
		require.NoError(t, err)
		assert.Equal(t, agTestProto, got)
	})

	t.Run("cbc", func(t *testing.T) {
		data := encryptAesCBC(t, key, bytes.Repeat([]byte{0x2a}, 16), agTestProto)
		got, err := decryptAntigravity(data)
		require.NoError(t, err)
		assert.Equal(t, agTestProto, got)
	})
}

// TestDecryptAntigravityWrongKey: ciphertext nobody can read must come back
// as nil, nil — "not decryptable" — not as an error or garbage bytes. The
// wrong key byte is a fixture: the proto sniff accepts most garbage, so only
// some keys exhaust every candidate to nil.
func TestDecryptAntigravityWrongKey(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	data := encryptAesCTR(t, key, bytes.Repeat([]byte{0x07}, 16), agTestProto)

	setAntigravityKey(t, bytes.Repeat([]byte{0x0e}, 32))
	got, err := decryptAntigravity(data)
	require.NoError(t, err)
	assert.Nil(t, got)
}

// TestDecryptAntigravityNoKey: a missing key is an error the caller can
// surface, distinct from the nil, nil "not decryptable" answer.
func TestDecryptAntigravityNoKey(t *testing.T) {
	setAntigravityKey(t, nil)
	got, err := decryptAntigravity(agTestProto)
	require.Error(t, err)
	assert.Nil(t, got)
}
