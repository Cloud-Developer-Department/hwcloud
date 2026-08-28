package crypto

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key, err := GenerateAESKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, plain := range [][]byte{
		[]byte("hello"),
		[]byte(""),
		bytes.Repeat([]byte("a"), 16),  // exactly one block
		bytes.Repeat([]byte("b"), 17),  // one block + 1
		bytes.Repeat([]byte("c"), 4096), // many blocks
	} {
		ct, err := EncryptAESECB(plain, key)
		if err != nil {
			t.Fatalf("encrypt len=%d: %v", len(plain), err)
		}
		if len(ct)%16 != 0 {
			t.Fatalf("ciphertext not block-aligned: %d", len(ct))
		}
		got, err := DecryptAESECB(ct, key)
		if err != nil {
			t.Fatalf("decrypt len=%d: %v", len(plain), err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("round trip mismatch: got %q want %q", got, plain)
		}
	}
}

func TestRejectsInvalidInputs(t *testing.T) {
	key, _ := GenerateAESKey()
	ct, err := EncryptAESECB([]byte("data"), key)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		ct   []byte
		key  []byte
	}{
		{"wrong key size", ct, []byte("short")},
		{"unpadded ciphertext", []byte("not 16 bytes!"), key},
		{"non-block-aligned", append(ct, 0x01), key},
	}
	for _, c := range cases {
		if _, err := DecryptAESECB(c.ct, c.key); err == nil {
			t.Errorf("%s: expected error", c.name)
		}
	}

	// Corrupt padding bytes must be rejected (strict unpad).
	bad := append([]byte{}, ct...)
	bad[len(bad)-1] = 9
	if _, err := DecryptAESECB(bad, key); err == nil {
		t.Error("corrupt padding accepted")
	}
}

func TestKeyEncodings(t *testing.T) {
	key := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	hexStr := hex.EncodeToString(key)

	// 1. direct hex
	got, err := DecodeAESKey(hexStr)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("hex decode: %x %v", got, err)
	}
	// 2. base64(raw 16 bytes)
	got, err = DecodeAESKey(base64.StdEncoding.EncodeToString(key))
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("base64(raw) decode: %x %v", got, err)
	}
	// 3. base64(hex string)
	got, err = DecodeAESKey(base64.StdEncoding.EncodeToString([]byte(hexStr)))
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("base64(hex) decode: %x %v", got, err)
	}
	// 4. URL-safe base64 variant
	got, err = DecodeAESKey(base64.URLEncoding.EncodeToString(key))
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("url-safe decode: %x %v", got, err)
	}
	// 5. garbage
	if _, err := DecodeAESKey("not-a-key!!!"); err == nil {
		t.Error("garbage key accepted")
	}

	if EncodeAESKeyHex(key) != hexStr {
		t.Error("EncodeAESKeyHex mismatch")
	}
	if EncodeAESKeyBase64(key) != base64.StdEncoding.EncodeToString([]byte(hexStr)) {
		t.Error("EncodeAESKeyBase64 mismatch")
	}
}
