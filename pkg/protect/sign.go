package protect

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"errors"
)

const (
	AlgHMACSHA256   = "hmac-sha256"
	AlgEd25519      = "ed25519"
	AlgECDSASHA256  = "ecdsa-sha256"
	AlgRSAPSSSHA256 = "rsa-pss-sha256"
)

var (
	ErrInvalidSignature = errors.New("signature verification failed")
	ErrCannotSign       = errors.New("key cannot sign: a private key or secret is required")
	ErrCannotVerify     = errors.New("key cannot verify signatures")
)

// SignAlgorithm returns the signature algorithm this key supports.
func (k *Key) SignAlgorithm() string {
	if k.Symmetric() {
		return AlgHMACSHA256
	}
	switch k.pub.(type) {
	case ed25519.PublicKey:
		return AlgEd25519
	case *ecdsa.PublicKey:
		return AlgECDSASHA256
	case *rsa.PublicKey:
		return AlgRSAPSSSHA256
	default:
		return ""
	}
}

// CanSign reports whether the key can produce signatures.
func (k *Key) CanSign() bool { return k.SignAlgorithm() != "" && k.Private() }

// CanVerify reports whether the key can verify signatures.
func (k *Key) CanVerify() bool { return k.SignAlgorithm() != "" }

// Sign returns the raw signature (or MAC) of data. The signed message is
// domain-separated (see domainInput) so a signature cannot be reused in
// another protocol or under another algorithm label.
func (k *Key) Sign(data []byte) ([]byte, error) {
	if !k.CanSign() {
		return nil, ErrCannotSign
	}
	msg := domainInput("sign", k.SignAlgorithm(), data)
	switch p := k.priv.(type) {
	case nil:
		mac := hmac.New(sha256.New, k.secret)
		mac.Write(msg)
		return mac.Sum(nil), nil
	case ed25519.PrivateKey:
		return ed25519.Sign(p, msg), nil
	case *ecdsa.PrivateKey:
		h := sha256.Sum256(msg)
		return ecdsa.SignASN1(rand.Reader, p, h[:])
	case *rsa.PrivateKey:
		h := sha256.Sum256(msg)
		return rsa.SignPSS(rand.Reader, p, crypto.SHA256, h[:], rsaPSSOptions())
	default:
		return nil, ErrCannotSign
	}
}

// Verify checks that sig is a valid signature of data for this key.
func (k *Key) Verify(data, sig []byte) error {
	if !k.CanVerify() {
		return ErrCannotVerify
	}
	msg := domainInput("sign", k.SignAlgorithm(), data)
	var ok bool
	switch p := k.pub.(type) {
	case nil:
		expected, err := k.Sign(data)
		if err != nil {
			return err
		}
		ok = hmac.Equal(expected, sig)
	case ed25519.PublicKey:
		ok = ed25519.Verify(p, msg, sig)
	case *ecdsa.PublicKey:
		h := sha256.Sum256(msg)
		ok = ecdsa.VerifyASN1(p, h[:], sig)
	case *rsa.PublicKey:
		h := sha256.Sum256(msg)
		ok = rsa.VerifyPSS(p, crypto.SHA256, h[:], sig, rsaPSSOptions()) == nil
	}
	if !ok {
		return ErrInvalidSignature
	}
	return nil
}

// domainInput prefixes data with a protocol/purpose/algorithm header. NUL
// separators keep the header unambiguous since none of the fields contain NUL.
func domainInput(purpose, alg string, data []byte) []byte {
	header := "docker-agent/agent-yaml/v1\x00" + purpose + "\x00" + alg + "\x00"
	return append([]byte(header), data...)
}

func rsaPSSOptions() *rsa.PSSOptions {
	return &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA256}
}
