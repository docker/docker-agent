package protect

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
)

const (
	AlgAESGCM  = "aes-256-gcm"
	AlgRSAOAEP = "rsa-oaep-sha256-aes-256-gcm"
	// ECIES algorithms are "ecies-<curve>-aes-256-gcm", see eciesAlgorithm.
	eciesPrefix = "ecies-"
	eciesSuffix = "-aes-256-gcm"
)

var (
	ErrCannotEncrypt = errors.New("key cannot encrypt")
	ErrCannotDecrypt = errors.New("key cannot decrypt: a private key or secret is required")
	ErrDecryption    = errors.New("decryption failed")
)

// EncryptAlgorithm returns the encryption algorithm this key supports, or ""
// if the key type cannot encrypt (Ed25519).
func (k *Key) EncryptAlgorithm() string {
	if k.Symmetric() {
		return AlgAESGCM
	}
	switch p := k.pub.(type) {
	case *rsa.PublicKey:
		return AlgRSAOAEP
	case *ecdsa.PublicKey:
		if e, err := p.ECDH(); err == nil {
			return eciesAlgorithm(e.Curve())
		}
	}
	return ""
}

func eciesAlgorithm(curve ecdh.Curve) string {
	name := strings.ToLower(strings.ReplaceAll(fmt.Sprint(curve), "-", ""))
	return eciesPrefix + name + eciesSuffix
}

// CanEncrypt reports whether the key can encrypt (any key of an
// encryption-capable type; a public key is enough).
func (k *Key) CanEncrypt() bool { return k.EncryptAlgorithm() != "" }

// CanDecrypt reports whether the key can decrypt (private material required).
func (k *Key) CanDecrypt() bool { return k.CanEncrypt() && k.Private() }

// Encrypt returns an authenticated ciphertext of data that only holders of
// the secret (symmetric) or of the private key (asymmetric) can open. The
// algorithm label is bound as AEAD additional data.
//
// Blob layouts:
//   - aes-256-gcm:      nonce || ciphertext
//   - ecies-*:          ephemeralPub || nonce || ciphertext
//   - rsa-oaep-*:       wrappedKey || nonce || ciphertext
func (k *Key) Encrypt(data []byte) ([]byte, error) {
	if !k.CanEncrypt() {
		return nil, ErrCannotEncrypt
	}
	aad := domainInput("encrypt", k.EncryptAlgorithm(), nil)
	switch {
	case k.Symmetric():
		key, err := deriveKey(k.secret, "docker-agent/"+AlgAESGCM)
		if err != nil {
			return nil, err
		}
		return aeadSeal(key, data, aad)
	case k.EncryptAlgorithm() == AlgRSAOAEP:
		return rsaEncrypt(k.pub.(*rsa.PublicKey), data, aad)
	default:
		recipient, err := k.pub.(*ecdsa.PublicKey).ECDH()
		if err != nil {
			return nil, err
		}
		return eciesEncrypt(recipient, data, aad)
	}
}

// Decrypt opens a blob produced by Encrypt with the matching key.
func (k *Key) Decrypt(blob []byte) ([]byte, error) {
	if !k.CanDecrypt() {
		return nil, ErrCannotDecrypt
	}
	aad := domainInput("encrypt", k.EncryptAlgorithm(), nil)
	switch p := k.priv.(type) {
	case nil:
		key, err := deriveKey(k.secret, "docker-agent/"+AlgAESGCM)
		if err != nil {
			return nil, err
		}
		return aeadOpen(key, blob, aad)
	case *rsa.PrivateKey:
		return rsaDecrypt(p, blob, aad)
	case *ecdsa.PrivateKey:
		priv, err := p.ECDH()
		if err != nil {
			return nil, err
		}
		return eciesDecrypt(priv, blob, aad)
	default:
		return nil, ErrCannotDecrypt
	}
}

const aesKeySize = 32

// deriveKey stretches secret material into an AES-256 key. The
// purpose-specific info keeps the AES key independent from other uses of the
// same material (e.g. HMAC on a shared secret).
func deriveKey(secret []byte, info string) ([]byte, error) {
	key, err := hkdf.Key(sha256.New, secret, nil, info, aesKeySize)
	if err != nil {
		return nil, fmt.Errorf("deriving key: %w", err)
	}
	return key, nil
}

// newGCM returns AES-GCM with an internally generated random nonce that is
// prepended to the ciphertext (and expected in front of it when opening).
func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

func aeadSeal(key, data, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return gcm.Seal(nil, nil, data, aad), nil
}

func aeadOpen(key, blob, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nil, blob, aad)
	if err != nil {
		return nil, ErrDecryption
	}
	return plain, nil
}

// eciesEncrypt performs ephemeral-static ECDH and encrypts under the derived
// key. Binding both public keys into the KDF info prevents key-substitution.
func eciesEncrypt(recipient *ecdh.PublicKey, data, aad []byte) ([]byte, error) {
	ephemeral, err := recipient.Curve().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := ephemeral.ECDH(recipient)
	if err != nil {
		return nil, err
	}
	ephPub := ephemeral.PublicKey().Bytes()
	key, err := deriveKey(shared, eciesInfo(ephPub, recipient.Bytes()))
	if err != nil {
		return nil, err
	}
	sealed, err := aeadSeal(key, data, aad)
	if err != nil {
		return nil, err
	}
	return append(ephPub, sealed...), nil
}

func eciesDecrypt(priv *ecdh.PrivateKey, blob, aad []byte) ([]byte, error) {
	pubLen := len(priv.PublicKey().Bytes())
	if len(blob) < pubLen {
		return nil, ErrDecryption
	}
	ephemeral, err := priv.Curve().NewPublicKey(blob[:pubLen])
	if err != nil {
		return nil, ErrDecryption
	}
	shared, err := priv.ECDH(ephemeral)
	if err != nil {
		return nil, ErrDecryption
	}
	key, err := deriveKey(shared, eciesInfo(blob[:pubLen], priv.PublicKey().Bytes()))
	if err != nil {
		return nil, err
	}
	return aeadOpen(key, blob[pubLen:], aad)
}

func eciesInfo(ephPub, recipientPub []byte) string {
	return "docker-agent/ecies" + string(ephPub) + string(recipientPub)
}

// rsaEncrypt wraps a fresh AES key with RSA-OAEP and encrypts data under it.
func rsaEncrypt(pub *rsa.PublicKey, data, aad []byte) ([]byte, error) {
	key := make([]byte, aesKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, key, []byte(AlgRSAOAEP))
	if err != nil {
		return nil, err
	}
	sealed, err := aeadSeal(key, data, aad)
	if err != nil {
		return nil, err
	}
	return append(wrapped, sealed...), nil
}

func rsaDecrypt(priv *rsa.PrivateKey, blob, aad []byte) ([]byte, error) {
	size := priv.Size()
	if len(blob) < size {
		return nil, ErrDecryption
	}
	key, err := rsa.DecryptOAEP(sha256.New(), nil, priv, blob[:size], []byte(AlgRSAOAEP))
	if err != nil {
		return nil, ErrDecryption
	}
	return aeadOpen(key, blob[size:], aad)
}
