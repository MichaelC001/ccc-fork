package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/nacl/box"
)

// Hub identity is a Curve25519 keypair. The public key hex is the stable
// instance/device id the hub routes on. The hub never sees the private key.

type hubIdentity struct {
	Private [32]byte
	Public  [32]byte
}

func (id hubIdentity) ID() string { return hex.EncodeToString(id.Public[:]) }

func (id hubIdentity) PublicHex() string { return id.ID() }

func generateHubIdentity() (hubIdentity, error) {
	pub, priv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return hubIdentity{}, err
	}
	return hubIdentity{Private: *priv, Public: *pub}, nil
}

func parseHubPublic(hexKey string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(hexKey)
	if err != nil || len(b) != 32 {
		return out, fmt.Errorf("hub public key must be 32 bytes hex")
	}
	copy(out[:], b)
	return out, nil
}

type hubIdentityFile struct {
	PrivateHex string `json:"private"`
	PublicHex  string `json:"public"`
}

func hubIdentityPath() string {
	return filepath.Join(configDir(), "hub-identity.json")
}

func loadOrCreateHubIdentity() (hubIdentity, error) {
	path := hubIdentityPath()
	data, err := os.ReadFile(path)
	if err == nil {
		var f hubIdentityFile
		if err := json.Unmarshal(data, &f); err != nil {
			return hubIdentity{}, fmt.Errorf("hub identity: %w", err)
		}
		priv, err := hex.DecodeString(f.PrivateHex)
		if err != nil || len(priv) != 32 {
			return hubIdentity{}, fmt.Errorf("hub identity: bad private key")
		}
		pub, err := hex.DecodeString(f.PublicHex)
		if err != nil || len(pub) != 32 {
			return hubIdentity{}, fmt.Errorf("hub identity: bad public key")
		}
		var id hubIdentity
		copy(id.Private[:], priv)
		copy(id.Public[:], pub)
		return id, nil
	}
	if !os.IsNotExist(err) {
		return hubIdentity{}, err
	}
	id, err := generateHubIdentity()
	if err != nil {
		return hubIdentity{}, err
	}
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return hubIdentity{}, err
	}
	raw, _ := json.MarshalIndent(hubIdentityFile{
		PrivateHex: hex.EncodeToString(id.Private[:]),
		PublicHex:  hex.EncodeToString(id.Public[:]),
	}, "", "  ")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return hubIdentity{}, err
	}
	return id, nil
}

func hubSeal(from hubIdentity, toPub [32]byte, plaintext []byte) (nonce [24]byte, ciphertext []byte, err error) {
	if _, err = rand.Read(nonce[:]); err != nil {
		return nonce, nil, err
	}
	out := box.Seal(nil, plaintext, &nonce, &toPub, &from.Private)
	return nonce, out, nil
}

func hubOpen(self hubIdentity, fromPub [32]byte, nonce [24]byte, ciphertext []byte) ([]byte, error) {
	out, ok := box.Open(nil, ciphertext, &nonce, &fromPub, &self.Private)
	if !ok {
		return nil, fmt.Errorf("hub box: open failed")
	}
	return out, nil
}
