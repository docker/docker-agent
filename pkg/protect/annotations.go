package protect

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const (
	// AnnotationSignature holds the base64 signature (or MAC) of the YAML layer.
	AnnotationSignature = "io.docker.agent.signature"
	// AnnotationSignatureAlgorithm names the algorithm behind AnnotationSignature.
	AnnotationSignatureAlgorithm = "io.docker.agent.signature.algorithm"
	// AnnotationEncrypted holds a base64 authenticated-encrypted copy of the
	// whole YAML layer.
	AnnotationEncrypted = "io.docker.agent.encrypted"
	// AnnotationEncryptedAlgorithm names the algorithm behind AnnotationEncrypted.
	AnnotationEncryptedAlgorithm = "io.docker.agent.encrypted.algorithm"
)

var (
	ErrNotProtected     = errors.New("artifact is neither signed nor encrypted")
	ErrNotEncrypted     = errors.New("artifact has no encrypted copy")
	ErrNotSigned        = errors.New("artifact is not signed: an encrypted copy alone does not prove who published it when the key is asymmetric")
	ErrTampered         = errors.New("encrypted copy does not match the artifact content")
	ErrAlgorithmMism    = errors.New("algorithm mismatch")
	ErrEncryptNeedsPriv = errors.New("encrypt mode with an asymmetric key requires the private key, so the artifact can also be signed")
)

// Mode selects what the publisher records in the annotations.
type Mode string

const (
	// ModeSign records a signature (asymmetric key) or MAC (secret).
	// Holders of the matching public key or secret can verify integrity.
	ModeSign Mode = "sign"
	// ModeEncrypt records an encrypted copy of the whole YAML. Holders of the
	// secret or private key can both verify integrity and recover the YAML
	// from the annotation alone. With an asymmetric key a signature is
	// recorded as well (see the package security model).
	ModeEncrypt Mode = "encrypt"
)

// Supports reports whether the key can publish in mode, with a descriptive
// error when it cannot.
func (k *Key) Supports(mode Mode) error {
	switch mode {
	case ModeSign:
		if !k.CanSign() {
			return fmt.Errorf("%w (%s)", ErrCannotSign, k.Describe())
		}
	case ModeEncrypt:
		if !k.CanEncrypt() {
			return fmt.Errorf("%w (%s)", ErrCannotEncrypt, k.Describe())
		}
		if !k.Symmetric() && !k.CanSign() {
			return fmt.Errorf("%w (%s)", ErrEncryptNeedsPriv, k.Describe())
		}
	default:
		return fmt.Errorf("unknown protection mode %q", mode)
	}
	return nil
}

// Protect records the protection for data in annotations according to mode.
func (k *Key) Protect(annotations map[string]string, data []byte, mode Mode) error {
	if err := k.Supports(mode); err != nil {
		return err
	}
	if mode == ModeSign || !k.Symmetric() {
		if err := k.sign(annotations, data); err != nil {
			return err
		}
	}
	if mode == ModeEncrypt {
		blob, err := k.Encrypt(data)
		if err != nil {
			return err
		}
		annotations[AnnotationEncrypted] = base64.StdEncoding.EncodeToString(blob)
		annotations[AnnotationEncryptedAlgorithm] = k.EncryptAlgorithm()
	}
	return nil
}

func (k *Key) sign(annotations map[string]string, data []byte) error {
	sig, err := k.Sign(data)
	if err != nil {
		return err
	}
	annotations[AnnotationSignature] = base64.StdEncoding.EncodeToString(sig)
	annotations[AnnotationSignatureAlgorithm] = k.SignAlgorithm()
	return nil
}

// IsProtected reports whether annotations carry a signature or encrypted copy.
func IsProtected(annotations map[string]string) bool {
	return annotations[AnnotationSignature] != "" || annotations[AnnotationEncrypted] != ""
}

// Verification reports which protections VerifyAnnotations actually checked.
type Verification struct {
	// SignatureAlgorithm is set when a signature was verified.
	SignatureAlgorithm string
	// EncryptedAlgorithm is set when the encrypted copy was decrypted and
	// matched the content. It stays empty for a public key, which can only
	// check the copy's algorithm label.
	EncryptedAlgorithm string
}

func (v Verification) String() string {
	var parts []string
	if v.SignatureAlgorithm != "" {
		parts = append(parts, "signature ("+v.SignatureAlgorithm+")")
	}
	if v.EncryptedAlgorithm != "" {
		parts = append(parts, "encrypted copy ("+v.EncryptedAlgorithm+")")
	}
	return strings.Join(parts, " and ")
}

// VerifyAnnotations checks that data is what a holder of this key published,
// using the protection annotations carry, and reports what was checked.
//
// A signature, when present, is always verified. An encrypted copy is
// decrypted and compared to data when the key can decrypt; a public key only
// checks its algorithm label and relies on the signature. With an asymmetric
// key a signature is mandatory, since anyone holding the public key could
// have produced the encrypted copy. ErrNotProtected is returned when the
// artifact carries no protection at all.
func (k *Key) VerifyAnnotations(annotations map[string]string, data []byte) (Verification, error) {
	var v Verification
	signed := annotations[AnnotationSignature] != ""
	encrypted := annotations[AnnotationEncrypted] != ""
	if !signed && !encrypted {
		return v, ErrNotProtected
	}
	if !signed && !k.Symmetric() {
		return v, ErrNotSigned
	}

	if signed {
		if err := k.verifySignature(annotations, data); err != nil {
			return v, err
		}
		v.SignatureAlgorithm = k.SignAlgorithm()
	}
	if encrypted {
		if err := k.checkEncryptedAlgorithm(annotations); err != nil {
			return Verification{}, err
		}
		if !k.CanDecrypt() {
			return v, nil
		}
		plain, err := k.Recover(annotations)
		if err != nil {
			return Verification{}, err
		}
		if subtle.ConstantTimeCompare(plain, data) != 1 {
			return Verification{}, ErrTampered
		}
		v.EncryptedAlgorithm = k.EncryptAlgorithm()
	}
	return v, nil
}

func (k *Key) verifySignature(annotations map[string]string, data []byte) error {
	if !k.CanVerify() {
		return fmt.Errorf("%w (%s)", ErrCannotVerify, k.Describe())
	}
	if alg := annotations[AnnotationSignatureAlgorithm]; alg != k.SignAlgorithm() {
		return fmt.Errorf("%w: artifact signed with %q but key supports %q", ErrAlgorithmMism, alg, k.SignAlgorithm())
	}
	sig, err := base64.StdEncoding.DecodeString(annotations[AnnotationSignature])
	if err != nil {
		return fmt.Errorf("%w: malformed signature annotation: %w", ErrInvalidSignature, err)
	}
	return k.Verify(data, sig)
}

func (k *Key) checkEncryptedAlgorithm(annotations map[string]string) error {
	alg := annotations[AnnotationEncryptedAlgorithm]
	if !k.CanEncrypt() {
		return fmt.Errorf("%w: artifact carries an encrypted copy (%q) but %s cannot have produced one", ErrAlgorithmMism, alg, k.Describe())
	}
	if alg != k.EncryptAlgorithm() {
		return fmt.Errorf("%w: artifact encrypted with %q but key supports %q", ErrAlgorithmMism, alg, k.EncryptAlgorithm())
	}
	return nil
}

// Recover decrypts the encrypted copy carried in annotations, returning the
// clear YAML. It works from the annotations alone, without the layer. Note
// that it does not check the signature; use VerifyAnnotations for that.
func (k *Key) Recover(annotations map[string]string) ([]byte, error) {
	encoded := annotations[AnnotationEncrypted]
	if encoded == "" {
		return nil, ErrNotEncrypted
	}
	if !k.CanDecrypt() {
		return nil, fmt.Errorf("%w (%s)", ErrCannotDecrypt, k.Describe())
	}
	if err := k.checkEncryptedAlgorithm(annotations); err != nil {
		return nil, err
	}
	blob, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed encrypted annotation: %w", ErrDecryption, err)
	}
	return k.Decrypt(blob)
}
