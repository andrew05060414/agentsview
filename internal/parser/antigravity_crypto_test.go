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

// agTestProto is a small protobuf payload with the long text the
// decrypted-transcript fallback exists to recover.
var agTestProto = encodePB([]pbField{
	{num: 1, wire: pbWireVarint, varint: 150},
	{
		num:   2,
		wire:  pbWireBytes,
		bytes: []byte("hello from a decrypted Antigravity transcript"),
	},
})

// agRealisticProto repeats well-formed fields to ~3KB, closer to real
// transcript sizes than the small agTestProto.
var agRealisticProto = func() []byte {
	var b []byte
	for len(b) < 3000 {
		b = append(b, agTestProto...)
	}
	return b
}()

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

func encryptAesGCM(t *testing.T, key, nonce, plaintext []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	return gcm.Seal(append([]byte{}, nonce...), nonce, plaintext, nil)
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

// TestDecryptAntigravityCTRAndCBC covers the dispatch loop across
// authenticated GCM, primary CTR, and CBC fallback candidates.
func TestDecryptAntigravityCTRAndCBC(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	setAntigravityKey(t, key)

	// CTR at skip=0 is deterministic once the earlier GCM attempts fail
	// authentication for a CTR ciphertext.
	t.Run("ctr", func(t *testing.T) {
		data := encryptAesCTR(t, key, bytes.Repeat([]byte{0x07}, 16), agTestProto)
		got, err := decryptAntigravity(data)
		require.NoError(t, err)
		assert.Equal(t, agTestProto, got)
	})

	// Regression for PX-2042: with leading bytes the correct (mode, skip)
	// is tried after wrong offsets that used to pass the weak proto sniff.
	t.Run("ctr with leading bytes", func(t *testing.T) {
		data := encryptAesCTR(t, key, bytes.Repeat([]byte{0xaa}, 16), agRealisticProto)
		data = append([]byte{0x00, 0x00, 0x00, 0x00}, data...)
		got, err := decryptAntigravity(data)
		require.NoError(t, err)
		assert.Equal(t, agRealisticProto, got)
	})

	// Regression for PX-2042: CTR is tried before CBC, so CTR-mode garbage
	// must not win before the real CBC candidate is reached.
	t.Run("cbc", func(t *testing.T) {
		data := encryptAesCBC(t, key, bytes.Repeat([]byte{0x00}, 16), agRealisticProto)
		got, err := decryptAntigravity(data)
		require.NoError(t, err)
		assert.Equal(t, agRealisticProto, got)
	})

	t.Run("gcm", func(t *testing.T) {
		data := encryptAesGCM(t, key, bytes.Repeat([]byte{0x03}, 12), agTestProto)
		got, err := decryptAntigravity(data)
		require.NoError(t, err)
		assert.Equal(t, agTestProto, got)
	})

	t.Run("gcm with leading bytes and plaintext header", func(t *testing.T) {
		plaintext := append([]byte{0x00, 0x00}, agRealisticProto...)
		data := encryptAesGCM(t, key, bytes.Repeat([]byte{0x03}, 12), plaintext)
		data = append([]byte{0xde, 0xad, 0xbe, 0xef}, data...)
		got, err := decryptAntigravity(data)
		require.NoError(t, err)
		assert.Equal(t, agRealisticProto, got)
	})

	t.Run("protobuf without transcript text", func(t *testing.T) {
		plaintext := encodePB([]pbField{
			{num: 1, wire: pbWireVarint, varint: 150},
			{num: 2, wire: pbWireVarint, varint: 1},
		})
		data := encryptAesCTR(t, key, bytes.Repeat([]byte{0x07}, 16), plaintext)
		got, err := decryptAntigravity(data)
		require.NoError(t, err)
		assert.Nil(t, got)
	})
}

// TestDecryptAntigravityWrongKey is the PX-2042 regression sweep:
// ciphertext nobody can read must come back as nil, nil — "not
// decryptable" — not as garbage bytes. It sweeps all 255 single-byte
// wrong keys over a realistic-size plaintext.
func TestDecryptAntigravityWrongKey(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	data := encryptAesCTR(t, key, bytes.Repeat([]byte{0x07}, 16), agRealisticProto)

	for v := range 256 {
		if v == 0x42 {
			continue
		}
		t.Setenv("ANTIGRAVITY_KEY",
			base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{byte(v)}, 32)))
		agKeyOnce = sync.Once{}
		agKeyVal = nil
		errAGKey = nil
		got, err := decryptAntigravity(data)
		require.NoError(t, err)
		assert.Nil(t, got, "wrong key 0x%02x returned %d bytes", v, len(got))
	}
}

// TestDecryptAntigravityNoKey: a missing key is an error the caller can
// surface, distinct from the nil, nil "not decryptable" answer.
func TestDecryptAntigravityNoKey(t *testing.T) {
	setAntigravityKey(t, nil)
	got, err := decryptAntigravity(agTestProto)
	require.Error(t, err)
	assert.Nil(t, got)
}
