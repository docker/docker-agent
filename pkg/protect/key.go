// Package protect signs or encrypts agent YAML shipped in OCI artifacts and
// verifies it on the way back.
//
// A key is either a symmetric secret or an asymmetric key (private, or
// public-only). The kind is detected from the file contents: PEM-encoded
// (RFC 7468) or OpenSSH keys are asymmetric; anything else is a raw secret.
//
// The YAML always stays in clear in the artifact layer. Depending on the
// Mode chosen by the publisher, the manifest annotations carry a signature
// (MAC or digital signature) and optionally an authenticated encrypted copy
// of the whole YAML that key holders can decrypt.
//
// # Security model
//
// Verification answers "was this produced by a holder of the key?". For a
// symmetric secret, both the MAC and the AEAD ciphertext require the secret,
// so either annotation alone is proof. For an asymmetric key, only a
// signature proves possession of the private key: anyone holding the public
// key can encrypt. Encrypt mode with an asymmetric key therefore requires the
// private key and records both a signature and an encrypted copy, and
// verification with an asymmetric key always requires a signature. This also
// rules out downgrading a signed artifact to an encrypted-only one.
//
// Signatures cover the layer bytes only. Re-tagging a signed artifact or
// serving an older signed version under a tag is not detected; pin digests
// when that matters.
package protect

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"

	pathx "github.com/docker/docker-agent/pkg/path"
)

// MinSecretLen is the minimum accepted length of a symmetric secret. The
// clear YAML plus its MAC/ciphertext is an offline oracle for guessing the
// secret, and HKDF adds no entropy, so short secrets are refused outright.
const MinSecretLen = 16

var ErrSecretTooShort = fmt.Errorf("symmetric secret must be at least %d bytes (generate one with `openssl rand -hex 32`)", MinSecretLen)

// Key is a symmetric secret or an asymmetric key (private, or public-only).
type Key struct {
	secret []byte
	// priv is nil for public-only keys. One of: *rsa.PrivateKey,
	// *ecdsa.PrivateKey, ed25519.PrivateKey.
	priv crypto.PrivateKey
	// pub is always set for asymmetric keys.
	pub crypto.PublicKey
}

// FilePrefix marks a ResolveKey value as a path to a key file. It is a
// literal prefix, not a URL scheme: no percent-decoding or authority parsing.
const FilePrefix = "file://"

// ResolveKey parses a key given on the command line: a value prefixed with
// FilePrefix names a key file (a leading ~ is expanded, since shells do not
// expand it mid-word), anything else is the key material itself.
func ResolveKey(value string) (*Key, error) {
	if path, ok := strings.CutPrefix(value, FilePrefix); ok {
		expanded, err := pathx.ExpandHomeDir(path)
		if err != nil {
			return nil, fmt.Errorf("resolving key file %s: %w", path, err)
		}
		return LoadKey(expanded)
	}
	return ParseKey([]byte(value))
}

// LoadKey reads and parses a key file. See ParseKey.
func LoadKey(path string) (*Key, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading key file: %w", err)
	}
	key, err := ParseKey(data)
	if err != nil {
		return nil, fmt.Errorf("parsing key file %s: %w", path, err)
	}
	return key, nil
}

// ParseKey detects the key kind from its encoding. PEM blocks and OpenSSH
// authorized_keys lines (with or without options) are parsed as asymmetric
// keys. Anything else is a raw symmetric secret (surrounding whitespace is
// trimmed so a trailing newline does not change the key) — unless it
// contains a PEM boundary or an OpenSSH key-type token, in which case it is
// treated as a broken key file and rejected. Failing closed here matters: a
// truncated or BOM-prefixed public key must never silently become an HMAC
// key made of public material. Random secrets never contain those markers.
func ParseKey(data []byte) (*Key, error) {
	if block, _ := pem.Decode(data); block != nil {
		return parsePEM(block, data)
	}
	pub, sshErr := parseAuthorizedKey(data)
	if sshErr == nil {
		return fromPublic(pub)
	}
	if bytes.Contains(data, []byte(pemMarker)) {
		return nil, fmt.Errorf("malformed PEM key (a raw secret must not contain %q)", pemMarker)
	}
	if hasOpenSSHKeyType(data) {
		return nil, fmt.Errorf("malformed OpenSSH public key: %w (a raw secret must not contain an OpenSSH key type such as \"ssh-\")", sshErr)
	}

	secret := bytes.Clone(bytes.TrimSpace(data))
	if len(secret) < MinSecretLen {
		return nil, ErrSecretTooShort
	}
	return &Key{secret: secret}, nil
}

const pemMarker = "-----BEGIN"

func parseAuthorizedKey(data []byte) (crypto.PublicKey, error) {
	pub, _, _, _, err := ssh.ParseAuthorizedKey(data) //nolint:dogsled // comment, options and rest are irrelevant
	if err != nil {
		return nil, err
	}
	cpk, ok := pub.(ssh.CryptoPublicKey)
	if !ok {
		return nil, fmt.Errorf("unsupported ssh public key type %s", pub.Type())
	}
	return cpk.CryptoPublicKey(), nil
}

var opensshKeyTypes = [][]byte{[]byte("ssh-"), []byte("ecdsa-sha2-"), []byte("sk-ssh-"), []byte("sk-ecdsa-")}

// hasOpenSSHKeyType reports whether data contains an OpenSSH key-type marker
// anywhere. A plain substring match, like the PEM check: anything subtler
// (field boundaries, trailing material) leaves a BOM- or garbage-prefixed
// public key free to pass as a secret.
func hasOpenSSHKeyType(data []byte) bool {
	for _, t := range opensshKeyTypes {
		if bytes.Contains(data, t) {
			return true
		}
	}
	return false
}

func parsePEM(block *pem.Block, data []byte) (*Key, error) {
	switch {
	case strings.Contains(block.Type, "ENCRYPTED"):
		return nil, errors.New("passphrase-protected keys are not supported")
	case strings.HasSuffix(block.Type, "PRIVATE KEY"):
		// Handles PKCS#1, SEC1, PKCS#8 and OpenSSH private keys.
		priv, err := ssh.ParseRawPrivateKey(data)
		if err != nil {
			if _, missing := errors.AsType[*ssh.PassphraseMissingError](err); missing {
				return nil, errors.New("passphrase-protected keys are not supported")
			}
			return nil, fmt.Errorf("parsing private key: %w", err)
		}
		return fromPrivate(priv)
	case block.Type == "PUBLIC KEY":
		pub, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing public key: %w", err)
		}
		return fromPublic(pub)
	case block.Type == "RSA PUBLIC KEY":
		pub, err := x509.ParsePKCS1PublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing RSA public key: %w", err)
		}
		return fromPublic(pub)
	default:
		return nil, fmt.Errorf("unsupported PEM block type %q", block.Type)
	}
}

// MinRSABits is the smallest accepted RSA modulus; smaller keys are
// considered broken.
const MinRSABits = 2048

func fromPrivate(priv any) (*Key, error) {
	switch p := priv.(type) {
	case *rsa.PrivateKey:
		if err := checkRSA(&p.PublicKey); err != nil {
			return nil, err
		}
		return &Key{priv: p, pub: &p.PublicKey}, nil
	case *ecdsa.PrivateKey:
		return &Key{priv: p, pub: &p.PublicKey}, nil
	case ed25519.PrivateKey:
		return &Key{priv: p, pub: p.Public()}, nil
	case *ed25519.PrivateKey:
		return fromPrivate(*p)
	default:
		return nil, unsupportedKeyType(priv)
	}
}

func fromPublic(pub crypto.PublicKey) (*Key, error) {
	switch p := pub.(type) {
	case *rsa.PublicKey:
		if err := checkRSA(p); err != nil {
			return nil, err
		}
		return &Key{pub: pub}, nil
	case *ecdsa.PublicKey, ed25519.PublicKey:
		return &Key{pub: pub}, nil
	default:
		return nil, unsupportedKeyType(pub)
	}
}

func checkRSA(pub *rsa.PublicKey) error {
	if bits := pub.N.BitLen(); bits < MinRSABits {
		return fmt.Errorf("RSA-%d key is too small: at least %d bits required", bits, MinRSABits)
	}
	return nil
}

func unsupportedKeyType(key any) error {
	return fmt.Errorf("unsupported key type %T: use an Ed25519, ECDSA or RSA key", key)
}

// Symmetric reports whether the key is a raw secret.
func (k *Key) Symmetric() bool { return k.secret != nil }

// Private reports whether the key holds private material (a secret or a
// private key), as opposed to a public-only key.
func (k *Key) Private() bool { return k.Symmetric() || k.priv != nil }

// Fingerprint returns a stable hex identifier for the key material: SHA-256 of
// the secret, or of the PKIX-encoded public key. Private and public halves of
// the same pair share a fingerprint.
func (k *Key) Fingerprint() string {
	material := k.secret
	if !k.Symmetric() {
		// Only fails for unsupported types, which fromPublic rejects.
		material, _ = x509.MarshalPKIXPublicKey(k.pub)
	}
	sum := sha256.Sum256(material)
	return hex.EncodeToString(sum[:])
}

// Identity extends Fingerprint with the key's role, so that the private and
// public halves of a pair — which have different verification capabilities —
// are told apart. Suitable as a cache key for verification results.
func (k *Key) Identity() string {
	if k.Private() {
		return k.Fingerprint() + "/private"
	}
	return k.Fingerprint() + "/public"
}

// Describe returns a short human-readable description of the key.
func (k *Key) Describe() string {
	if k.Symmetric() {
		return "symmetric secret"
	}
	half := "public"
	if k.priv != nil {
		half = "private"
	}
	switch p := k.pub.(type) {
	case *rsa.PublicKey:
		return fmt.Sprintf("RSA-%d %s key", p.N.BitLen(), half)
	case *ecdsa.PublicKey:
		return fmt.Sprintf("ECDSA %s %s key", p.Curve.Params().Name, half)
	case ed25519.PublicKey:
		return "Ed25519 " + half + " key"
	default:
		return "unknown key"
	}
}
