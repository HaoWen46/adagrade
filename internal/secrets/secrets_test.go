package secrets

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateKey_CreatesThenReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "secret.key")

	k1, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if k1 == ([32]byte{}) {
		t.Fatal("key is all zeros")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("key file perms: got %o want 600", info.Mode().Perm())
	}

	k2, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if k1 != k2 {
		t.Error("second load must return the same key")
	}
}

func TestLoadOrCreateKey_RejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	if err := os.WriteFile(path, []byte("too short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(path); err == nil {
		t.Fatal("corrupt key file must error, not be silently replaced")
	}
}

func TestSealOpen_RoundTrip(t *testing.T) {
	var key [32]byte
	copy(key[:], bytes.Repeat([]byte{7}, 32))

	ct, err := Seal(key, []byte("sk-super-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("sk-super-secret")) {
		t.Fatal("ciphertext contains plaintext")
	}
	pt, err := Open(key, ct)
	if err != nil {
		t.Fatal(err)
	}
	if string(pt) != "sk-super-secret" {
		t.Errorf("round trip: got %q", pt)
	}

	// Every seal uses a fresh nonce.
	ct2, _ := Seal(key, []byte("sk-super-secret"))
	if bytes.Equal(ct, ct2) {
		t.Error("two seals of the same plaintext must differ (nonce reuse?)")
	}
}

func TestDerive_DeterministicPerInfo(t *testing.T) {
	var key [32]byte
	copy(key[:], bytes.Repeat([]byte{7}, 32))

	sub1 := Derive(key, "regrade-token-v1")
	sub2 := Derive(key, "regrade-token-v1")
	if !bytes.Equal(sub1, sub2) {
		t.Error("Derive must be deterministic for the same key+info")
	}
	if len(sub1) != 32 {
		t.Errorf("Derive length: got %d want 32", len(sub1))
	}
}

func TestDerive_DifferentInfoDifferentSubkey(t *testing.T) {
	var key [32]byte
	copy(key[:], bytes.Repeat([]byte{7}, 32))

	a := Derive(key, "regrade-token-v1")
	b := Derive(key, "some-other-purpose")
	if bytes.Equal(a, b) {
		t.Error("different info strings must yield different subkeys")
	}
}

func TestDerive_DifferentMasterKeyDifferentSubkey(t *testing.T) {
	var key1, key2 [32]byte
	copy(key1[:], bytes.Repeat([]byte{7}, 32))
	copy(key2[:], bytes.Repeat([]byte{9}, 32))

	a := Derive(key1, "regrade-token-v1")
	b := Derive(key2, "regrade-token-v1")
	if bytes.Equal(a, b) {
		t.Error("different master keys must yield different subkeys")
	}
}

func TestDerive_SubkeyDoesNotContainMasterKey(t *testing.T) {
	var key [32]byte
	copy(key[:], bytes.Repeat([]byte{7}, 32))

	sub := Derive(key, "regrade-token-v1")
	if bytes.Contains(sub, key[:]) {
		t.Fatal("derived subkey must not leak the master key bytes")
	}
}

func TestOpen_RejectsTamperAndWrongKey(t *testing.T) {
	var key, other [32]byte
	copy(key[:], bytes.Repeat([]byte{7}, 32))
	copy(other[:], bytes.Repeat([]byte{9}, 32))

	ct, _ := Seal(key, []byte("payload"))
	ct[len(ct)-1] ^= 0xff
	if _, err := Open(key, ct); err == nil {
		t.Error("tampered ciphertext must not open")
	}

	ct2, _ := Seal(key, []byte("payload"))
	if _, err := Open(other, ct2); err == nil {
		t.Error("wrong key must not open")
	}
	if _, err := Open(key, []byte("short")); err == nil {
		t.Error("truncated ciphertext must not open")
	}
}
