package secret

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestBoxRoundTrip(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	b, err := NewBox(key)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := b.Encrypt("s3cr3t")
	enc2, _ := b.Encrypt("s3cr3t")
	if enc == "s3cr3t" || enc == enc2 {
		t.Fatal("must be encrypted with random nonce")
	}
	got, err := b.Decrypt(enc)
	if err != nil || got != "s3cr3t" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestBoxRejectsBadKey(t *testing.T) {
	if _, err := NewBox(base64.StdEncoding.EncodeToString(make([]byte, 16))); err == nil {
		t.Fatal("16-byte key accepted")
	}
	if _, err := NewBox("not base64!"); err == nil {
		t.Fatal("bad base64 accepted")
	}
}

func TestBoxWrongKeyFails(t *testing.T) {
	a, _ := NewBox(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	b, _ := NewBox(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)))
	enc, _ := a.Encrypt("x")
	if _, err := b.Decrypt(enc); err == nil {
		t.Fatal("decrypt with wrong key succeeded")
	}
}
