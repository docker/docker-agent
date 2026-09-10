package protect

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

const (
	payload = "version: \"2\"\nagents:\n  root:\n    model: auto\n"
	secret  = "correct horse battery staple"
)

func pemEncode(t *testing.T, typ string, der []byte) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

func pkcs8(t *testing.T, priv any) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	return pemEncode(t, "PRIVATE KEY", der)
}

func pkix(t *testing.T, pub any) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	return pemEncode(t, "PUBLIC KEY", der)
}

// verifyErr discards the Verification report for tests that only care about the error.
func verifyErr(k *Key, annotations map[string]string, data []byte) error {
	_, err := k.VerifyAnnotations(annotations, data)
	return err
}

func mustParse(t *testing.T, data []byte) *Key {
	t.Helper()
	key, err := ParseKey(data)
	require.NoError(t, err)
	return key
}

type keyPair struct {
	priv, pub  []byte
	canEncrypt bool
	signAlg    string
	encAlg     string
}

// keyPairs returns every supported asymmetric key type and encoding.
func keyPairs(t *testing.T) map[string]keyPair {
	t.Helper()

	edPub, edPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	ecPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	rsaPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	ecSEC1, err := x509.MarshalECPrivateKey(ecPriv)
	require.NoError(t, err)
	openssh, err := ssh.MarshalPrivateKey(edPriv, "")
	require.NoError(t, err)
	sshPub, err := ssh.NewPublicKey(edPub)
	require.NoError(t, err)

	ed := keyPair{signAlg: AlgEd25519}
	ec := keyPair{canEncrypt: true, signAlg: AlgECDSASHA256, encAlg: "ecies-p256-aes-256-gcm"}
	rs := keyPair{canEncrypt: true, signAlg: AlgRSAPSSSHA256, encAlg: AlgRSAOAEP}

	with := func(kp keyPair, priv, pub []byte) keyPair { kp.priv, kp.pub = priv, pub; return kp }

	return map[string]keyPair{
		"ed25519/pkcs8+pkix": with(ed, pkcs8(t, edPriv), pkix(t, edPub)),
		"ed25519/openssh":    with(ed, pem.EncodeToMemory(openssh), ssh.MarshalAuthorizedKey(sshPub)),
		"ecdsa/pkcs8+pkix":   with(ec, pkcs8(t, ecPriv), pkix(t, &ecPriv.PublicKey)),
		"ecdsa/sec1":         with(ec, pemEncode(t, "EC PRIVATE KEY", ecSEC1), pkix(t, &ecPriv.PublicKey)),
		"rsa/pkcs8+pkix":     with(rs, pkcs8(t, rsaPriv), pkix(t, &rsaPriv.PublicKey)),
		"rsa/pkcs1":          with(rs, pemEncode(t, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(rsaPriv)), pemEncode(t, "RSA PUBLIC KEY", x509.MarshalPKCS1PublicKey(&rsaPriv.PublicKey))),
	}
}

func TestParseKey_Symmetric(t *testing.T) {
	t.Parallel()

	key := mustParse(t, []byte(secret+"\n"))
	assert.True(t, key.Symmetric())
	assert.True(t, key.Private())
	assert.True(t, key.CanSign())
	assert.True(t, key.CanEncrypt())
	assert.True(t, key.CanDecrypt())
	assert.Equal(t, AlgHMACSHA256, key.SignAlgorithm())
	assert.Equal(t, AlgAESGCM, key.EncryptAlgorithm())

	// Trailing newline must not change the key.
	assert.Equal(t, key.Fingerprint(), mustParse(t, []byte(secret)).Fingerprint())

	// The key must not alias the caller's buffer.
	buf := []byte(secret)
	aliased := mustParse(t, buf)
	buf[0] ^= 0xff
	assert.Equal(t, key.Fingerprint(), aliased.Fingerprint())
}

func TestParseKey_RejectsShortSecret(t *testing.T) {
	t.Parallel()
	_, err := ParseKey([]byte("  \n"))
	require.ErrorIs(t, err, ErrSecretTooShort)
	_, err = ParseKey([]byte(strings.Repeat("x", MinSecretLen-1)))
	require.ErrorIs(t, err, ErrSecretTooShort)
	_, err = ParseKey([]byte(strings.Repeat("x", MinSecretLen)))
	require.NoError(t, err)
}

func TestParseKey_OpenSSHWithOptions(t *testing.T) {
	t.Parallel()

	edPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	sshPub, err := ssh.NewPublicKey(edPub)
	require.NoError(t, err)
	line := `from="10.0.0.0/8",no-agent-forwarding ` + string(ssh.MarshalAuthorizedKey(sshPub))

	key := mustParse(t, []byte(line))
	assert.False(t, key.Symmetric(), "an authorized_keys line with options is a public key, not a secret")
	assert.Equal(t, AlgEd25519, key.SignAlgorithm())

	_, err = ParseKey([]byte(`from="10.0.0.0/8" ssh-ed25519 AAAAbroken comment`))
	require.ErrorContains(t, err, "OpenSSH")

	// An option containing the PEM marker must not shadow a valid key line.
	key = mustParse(t, []byte(`command="echo -----BEGIN" `+string(ssh.MarshalAuthorizedKey(sshPub))))
	assert.Equal(t, AlgEd25519, key.SignAlgorithm())
}

// A secret is never allowed to look like a key file: such input is far more
// likely a broken public key than a deliberate passphrase, and accepting it
// would protect artifacts with public material.
func TestParseKey_SecretsMustNotLookLikeKeys(t *testing.T) {
	t.Parallel()

	for _, s := range []string{
		"ecdsa-sha2-is-just-part-of-my-passphrase",
		"my passphrase ssh-rsa is long enough",
		"correct-horse-ssh-battery-staple",
		"prefix-----BEGIN-is-not-a-pem-header",
	} {
		_, err := ParseKey([]byte(s))
		require.ErrorContains(t, err, "a raw secret must not contain", s)
	}

	// While ordinary and random secrets are fine.
	for _, s := range []string{
		"correct horse battery staple",
		"a-secret-with-sshd-in-it-is-fine", // "sshd-" is not a key type
		"3f9c2a1b4e8d7c6f5a0b9e8d7c6f5a4b3c2d1e0f",
	} {
		assert.True(t, mustParse(t, []byte(s)).Symmetric(), s)
	}
}

func TestParseKey_RejectsSmallRSA(t *testing.T) {
	t.Parallel()

	// Fixtures rather than rsa.GenerateKey: FIPS-only mode refuses to
	// generate keys this small, which is exactly what we assert on.
	const priv1024 = `-----BEGIN PRIVATE KEY-----
MIICdwIBADANBgkqhkiG9w0BAQEFAASCAmEwggJdAgEAAoGBAMGC3Svsn0pS9Plq
0H7O9s7HNbEBTpmdfJrLD2MVCsVH6HmAja7cVKqxgFZfPR03Mh30YvJ0oSGae/ak
ZIsBYz3uyBDzan6UKuFwlj9xKM6rKiS0QSNIkaTE8Bbb4boGd0LkPyR7gaCSkxCm
PjZjycXKjS7Ltulp2JAHg2gQX9HlAgMBAAECgYADgT5GRGPiMbx0JAYgtdjsh9km
GpL031BZcWIW9lOanSHNyZFHYIA8Ezjy14jA1bYXqsx7/bbJaAXkwrd7eQv2FCKJ
U+jBA1N/DYOMtrPNZHwWkdVyuZh7x4IIe1YJTeVzoGBIJH9blzmwuBw1GhL61ttJ
P6o/z6AA6Tab/pZWJQJBAOdN+6654PZ3DvfRurAYlizXkd9sD7Df9GBSKdamfPrm
sfrHA/6Fx5d5uFYGHjgsFyLZXhKrXA3C4AN8fTcinSsCQQDWK+ks8tfUX2nepkaW
P9z80Sj0UPdUk2oWJ09JRXPW/nHkh1PdfvStLn4ZPTmJ2JQpuWZofi+SCsnZbYgH
iWUvAkEAqXH9cFCHNsadVnpz8tDwIsWA/VViYUaO9Yj7UV4BrKQXugjVKj3Cq3rl
yU8OEERsZoEqYy7ZbtNV2/f0mtFmpQJANvav8cQk1bDi56v+g4LCQPOgsgqxXrgy
SpsuAtzbHLrSGdcNE9QIEQXUgL+wq4q0g3y8Jmbz6GPyZ2VvupdtKwJBAJ9TgAIz
Fgzgv26ejpkp297lNYAXLrgJIPWaBXgpBEFm/doqZhJep/w0Rt4eKG/OwjF2F3XA
ejTm/Yk3MH+ru0Y=
-----END PRIVATE KEY-----
`
	const pub1024 = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDBgt0r7J9KUvT5atB+zvbOxzWx
AU6ZnXyayw9jFQrFR+h5gI2u3FSqsYBWXz0dNzId9GLydKEhmnv2pGSLAWM97sgQ
82p+lCrhcJY/cSjOqyoktEEjSJGkxPAW2+G6BndC5D8ke4GgkpMQpj42Y8nFyo0u
y7bpadiQB4NoEF/R5QIDAQAB
-----END PUBLIC KEY-----
`

	_, err := ParseKey([]byte(priv1024))
	require.ErrorContains(t, err, "too small")
	_, err = ParseKey([]byte(pub1024))
	require.ErrorContains(t, err, "too small")
}

func TestParseKey_MalformedAsymmetricIsNotASecret(t *testing.T) {
	t.Parallel()

	// Long enough to pass as a secret, but claims to be PEM/OpenSSH.
	for name, data := range map[string]string{
		"truncated pem":    "-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE\n",
		"pem no end":       "-----BEGIN PRIVATE KEY-----\nAAAA",
		"bad ssh base64":   "ssh-ed25519 definitely-not-base64 comment",
		"bad ecdsa ssh":    "ecdsa-sha2-nistp256 AAAA garbage",
		"bad ssh options":  `command="x" ssh-rsa AAAA garbage`,
		"truncated ssh":    "ecdsa-sha2-nistp256",
		"truncated ssh 2":  "command=x ecdsa-sha2-nistp256\n",
		"bom ssh":          "\xef\xbb\xbfssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGb0l7v5+6E4CMbQ7v9k1Yl6vX0+dTn3vE0jvFq6gFqK",
		"garbage ssh":      "garbagessh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGb0l7v5+6E4CMbQ7v9k1Yl6vX0+dTn3vE0jvFq6gFqK",
		"bom pem":          "\xef\xbb\xbf-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE\n-----END PUBLIC KEY-----\n",
		"garbage pem":      "garbage-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE\n-----END PUBLIC KEY-----\n",
		"unsupported pem":  string(pemEncode(t, "CERTIFICATE", []byte("junk"))),
		"unsupported type": string(pemEncode(t, "DSA PARAMETERS", make([]byte, 40))),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			key, err := ParseKey([]byte(data))
			require.Error(t, err)
			assert.Nil(t, key)
		})
	}
}

func TestParseKey_RejectsEncryptedAndX25519(t *testing.T) {
	t.Parallel()

	_, edPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	block, err := ssh.MarshalPrivateKeyWithPassphrase(edPriv, "", []byte("pw"))
	require.NoError(t, err)
	_, err = ParseKey(pem.EncodeToMemory(block))
	require.ErrorContains(t, err, "passphrase-protected")

	// X25519 cannot sign, so it cannot prove authorship under this model.
	x25519PKCS8 := "-----BEGIN PRIVATE KEY-----\nMC4CAQAwBQYDK2VuBCIEIIikJ7f7vZ0pE4oSd0kkbPKt+N2eOrdKQ7yhqMXjMkFa\n-----END PRIVATE KEY-----\n"
	_, err = ParseKey([]byte(x25519PKCS8))
	require.Error(t, err) // "unsupported key type", or refused earlier by crypto/ecdh in FIPS-only mode
}

func TestAsymmetric_Capabilities(t *testing.T) {
	t.Parallel()

	for name, kp := range keyPairs(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			priv, pub := mustParse(t, kp.priv), mustParse(t, kp.pub)

			assert.False(t, priv.Symmetric())
			assert.True(t, priv.Private())
			assert.False(t, pub.Private())
			assert.Equal(t, priv.Fingerprint(), pub.Fingerprint())
			assert.NotEqual(t, priv.Identity(), pub.Identity())

			assert.True(t, priv.CanSign())
			assert.False(t, pub.CanSign(), "public keys never sign")
			assert.True(t, pub.CanVerify())
			assert.Equal(t, kp.signAlg, priv.SignAlgorithm())

			assert.Equal(t, kp.canEncrypt, priv.CanEncrypt())
			assert.Equal(t, kp.canEncrypt, pub.CanEncrypt())
			assert.Equal(t, kp.canEncrypt, priv.CanDecrypt())
			assert.False(t, pub.CanDecrypt(), "public keys never decrypt")
			assert.Equal(t, kp.encAlg, priv.EncryptAlgorithm())
		})
	}
}

func TestAsymmetric_SignMode(t *testing.T) {
	t.Parallel()

	for name, kp := range keyPairs(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			priv, pub := mustParse(t, kp.priv), mustParse(t, kp.pub)

			annotations := map[string]string{}
			require.ErrorIs(t, pub.Protect(annotations, []byte(payload), ModeSign), ErrCannotSign)
			require.NoError(t, priv.Protect(annotations, []byte(payload), ModeSign))
			assert.Equal(t, kp.signAlg, annotations[AnnotationSignatureAlgorithm])
			assert.NotContains(t, annotations, AnnotationEncrypted)

			require.NoError(t, verifyErr(pub, annotations, []byte(payload)))
			require.NoError(t, verifyErr(priv, annotations, []byte(payload)))
			require.ErrorIs(t, verifyErr(pub, annotations, []byte(payload+"#\n")), ErrInvalidSignature)

			_, err := priv.Recover(annotations)
			require.ErrorIs(t, err, ErrNotEncrypted)
		})
	}
}

func TestAsymmetric_EncryptMode(t *testing.T) {
	t.Parallel()

	for name, kp := range keyPairs(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			priv, pub := mustParse(t, kp.priv), mustParse(t, kp.pub)

			annotations := map[string]string{}
			if !kp.canEncrypt {
				require.ErrorIs(t, priv.Protect(annotations, []byte(payload), ModeEncrypt), ErrCannotEncrypt)

				// This key type never produces an encrypted copy, so a signed
				// artifact carrying one is inconsistent whatever its label.
				require.NoError(t, priv.Protect(annotations, []byte(payload), ModeSign))
				annotations[AnnotationEncrypted] = "AAAA"
				require.ErrorIs(t, verifyErr(pub, annotations, []byte(payload)), ErrAlgorithmMism)
				require.ErrorIs(t, verifyErr(priv, annotations, []byte(payload)), ErrAlgorithmMism)
				return
			}

			// Encrypting with only the public key would leave the artifact unauthenticated.
			require.ErrorIs(t, pub.Protect(annotations, []byte(payload), ModeEncrypt), ErrEncryptNeedsPriv)
			assert.Empty(t, annotations)

			require.NoError(t, priv.Protect(annotations, []byte(payload), ModeEncrypt))
			assert.Equal(t, kp.encAlg, annotations[AnnotationEncryptedAlgorithm])
			assert.Equal(t, kp.signAlg, annotations[AnnotationSignatureAlgorithm], "encrypt mode must also sign")

			// Private key: signature + decrypt-and-compare. Public key: signature only.
			v, err := priv.VerifyAnnotations(annotations, []byte(payload))
			require.NoError(t, err)
			assert.Equal(t, Verification{SignatureAlgorithm: kp.signAlg, EncryptedAlgorithm: kp.encAlg}, v)
			v, err = pub.VerifyAnnotations(annotations, []byte(payload))
			require.NoError(t, err)
			assert.Equal(t, Verification{SignatureAlgorithm: kp.signAlg}, v)
			assert.Equal(t, "signature ("+kp.signAlg+")", v.String())

			// Even a public key must notice an encrypted copy that this key could not have produced.
			relabeled := maps.Clone(annotations)
			relabeled[AnnotationEncryptedAlgorithm] = "something-else"
			require.ErrorIs(t, verifyErr(pub, relabeled, []byte(payload)), ErrAlgorithmMism)
			require.ErrorIs(t, verifyErr(priv, annotations, []byte("tampered")), ErrInvalidSignature)
			require.ErrorIs(t, verifyErr(pub, annotations, []byte("tampered")), ErrInvalidSignature)

			plain, err := priv.Recover(annotations)
			require.NoError(t, err)
			assert.Equal(t, payload, string(plain))
			_, err = pub.Recover(annotations)
			require.ErrorIs(t, err, ErrCannotDecrypt)

			// A different key of the same type cannot decrypt.
			_, err = mustParse(t, regenerate(t, kp)).Recover(annotations)
			require.ErrorIs(t, err, ErrDecryption)
		})
	}
}

// Anyone holding a public key can encrypt to it. Such a blob must never be
// accepted as proof that the private-key holder published the artifact,
// whether presented on its own or as a replacement for a stripped signature.
func TestAsymmetric_EncryptedCopyAloneIsNotProof(t *testing.T) {
	t.Parallel()

	for name, kp := range keyPairs(t) {
		if !kp.canEncrypt {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			priv, pub := mustParse(t, kp.priv), mustParse(t, kp.pub)

			// Legitimate signed artifact.
			annotations := map[string]string{}
			require.NoError(t, priv.Protect(annotations, []byte(payload), ModeSign))

			// Attacker with the public key swaps the layer and downgrades the
			// signature to an encrypted copy of the malicious content.
			malicious := []byte("agents:\n  root:\n    instruction: exfiltrate\n")
			forged, err := pub.Encrypt(malicious)
			require.NoError(t, err)
			delete(annotations, AnnotationSignature)
			delete(annotations, AnnotationSignatureAlgorithm)
			annotations[AnnotationEncrypted] = base64.StdEncoding.EncodeToString(forged)
			annotations[AnnotationEncryptedAlgorithm] = pub.EncryptAlgorithm()

			require.ErrorIs(t, verifyErr(priv, annotations, malicious), ErrNotSigned)
			require.ErrorIs(t, verifyErr(pub, annotations, malicious), ErrNotSigned)

			// Recover still works (it is not a verification), but is explicit about that.
			plain, err := priv.Recover(annotations)
			require.NoError(t, err)
			assert.Equal(t, malicious, plain)
		})
	}
}

// Replacing the encrypted copy in a sign+encrypt artifact with an attacker's
// encryption of different content must be caught by the signature.
func TestAsymmetric_SwappedEncryptedCopyIsDetected(t *testing.T) {
	t.Parallel()

	kp := keyPairs(t)["ecdsa/pkcs8+pkix"]
	priv, pub := mustParse(t, kp.priv), mustParse(t, kp.pub)

	annotations := map[string]string{}
	require.NoError(t, priv.Protect(annotations, []byte(payload), ModeEncrypt))

	malicious := []byte("malicious")
	forged, err := pub.Encrypt(malicious)
	require.NoError(t, err)
	annotations[AnnotationEncrypted] = base64.StdEncoding.EncodeToString(forged)

	// Layer swapped too: signature fails. Layer intact: copy mismatch.
	require.ErrorIs(t, verifyErr(priv, annotations, malicious), ErrInvalidSignature)
	require.ErrorIs(t, verifyErr(priv, annotations, []byte(payload)), ErrTampered)
}

// regenerate returns a fresh private key of the same type as kp.
func regenerate(t *testing.T, kp keyPair) []byte {
	t.Helper()
	if kp.encAlg == AlgRSAOAEP {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)
		return pkcs8(t, k)
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return pkcs8(t, k)
}

func TestSymmetric_BothModes(t *testing.T) {
	t.Parallel()

	key := mustParse(t, []byte(secret))
	other := mustParse(t, []byte("a completely different secret"))

	signed := map[string]string{}
	require.NoError(t, key.Protect(signed, []byte(payload), ModeSign))
	assert.NotContains(t, signed, AnnotationEncrypted)
	require.NoError(t, verifyErr(key, signed, []byte(payload)))
	require.ErrorIs(t, verifyErr(key, signed, []byte("changed")), ErrInvalidSignature)
	require.ErrorIs(t, verifyErr(other, signed, []byte(payload)), ErrInvalidSignature)

	// With a secret, the AEAD copy alone is proof: producing it needs the secret.
	encrypted := map[string]string{}
	require.NoError(t, key.Protect(encrypted, []byte(payload), ModeEncrypt))
	assert.NotContains(t, encrypted, AnnotationSignature)
	require.NoError(t, verifyErr(key, encrypted, []byte(payload)))
	require.ErrorIs(t, verifyErr(key, encrypted, []byte("changed")), ErrTampered)
	require.ErrorIs(t, verifyErr(other, encrypted, []byte(payload)), ErrDecryption)

	plain, err := key.Recover(encrypted)
	require.NoError(t, err)
	assert.Equal(t, payload, string(plain))

	// Each encryption uses a fresh nonce.
	again := map[string]string{}
	require.NoError(t, key.Protect(again, []byte(payload), ModeEncrypt))
	assert.NotEqual(t, encrypted[AnnotationEncrypted], again[AnnotationEncrypted])
}

// Signatures and ciphertexts are bound to this protocol and their algorithm
// label; raw primitives over the same bytes must not verify.
func TestDomainSeparation(t *testing.T) {
	t.Parallel()

	kp := keyPairs(t)["ed25519/pkcs8+pkix"]
	priv, pub := mustParse(t, kp.priv), mustParse(t, kp.pub)
	rawSig := ed25519.Sign(priv.priv.(ed25519.PrivateKey), []byte(payload))
	require.ErrorIs(t, pub.Verify([]byte(payload), rawSig), ErrInvalidSignature)

	key := mustParse(t, []byte(secret))
	annotations := map[string]string{}
	require.NoError(t, key.Protect(annotations, []byte(payload), ModeEncrypt))
	// Relabeling the algorithm changes the AAD and must break decryption, not
	// just the label check.
	blob, err := base64.StdEncoding.DecodeString(annotations[AnnotationEncrypted])
	require.NoError(t, err)
	derived, err := deriveKey(key.secret, "docker-agent/"+AlgAESGCM)
	require.NoError(t, err)
	_, err = aeadOpen(derived, blob, domainInput("encrypt", "other-alg", nil))
	require.ErrorIs(t, err, ErrDecryption)
}

func TestVerifyAnnotations_Errors(t *testing.T) {
	t.Parallel()

	key := mustParse(t, []byte(secret))

	require.ErrorIs(t, verifyErr(key, map[string]string{}, []byte(payload)), ErrNotProtected)

	err := verifyErr(key, map[string]string{
		AnnotationSignature:          "AAAA",
		AnnotationSignatureAlgorithm: AlgEd25519,
	}, []byte(payload))
	require.ErrorIs(t, err, ErrAlgorithmMism)

	err = verifyErr(key, map[string]string{
		AnnotationSignature:          "not base64!",
		AnnotationSignatureAlgorithm: AlgHMACSHA256,
	}, []byte(payload))
	require.ErrorIs(t, err, ErrInvalidSignature)

	err = verifyErr(key, map[string]string{
		AnnotationEncrypted:          "AAAA",
		AnnotationEncryptedAlgorithm: AlgRSAOAEP,
	}, []byte(payload))
	require.ErrorIs(t, err, ErrAlgorithmMism)

	for _, blob := range []string{"", "AAAA", "not base64!", base64.StdEncoding.EncodeToString(make([]byte, 11))} {
		err = verifyErr(key, map[string]string{
			AnnotationEncrypted:          blob,
			AnnotationEncryptedAlgorithm: AlgAESGCM,
		}, []byte(payload))
		if blob == "" {
			require.ErrorIs(t, err, ErrNotProtected)
		} else {
			require.ErrorIs(t, err, ErrDecryption)
		}
	}

	require.ErrorContains(t, key.Protect(map[string]string{}, nil, Mode("bogus")), "unknown protection mode")
}

func TestLoadKey(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(path, []byte(secret), 0o600))

	key, err := LoadKey(path)
	require.NoError(t, err)
	assert.True(t, key.Symmetric())

	_, err = LoadKey(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
}

func TestResolveKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, "key")
	require.NoError(t, os.WriteFile(path, []byte(secret), 0o600))

	fromFile, err := ResolveKey(FilePrefix + path)
	require.NoError(t, err)
	inline, err := ResolveKey(secret)
	require.NoError(t, err)
	assert.Equal(t, fromFile.Identity(), inline.Identity())

	fromHome, err := ResolveKey(FilePrefix + "~/key")
	require.NoError(t, err)
	assert.Equal(t, fromFile.Identity(), fromHome.Identity())

	// A bare path is key material, not a file reference.
	_, err = ResolveKey(path)
	require.NoError(t, err)

	_, err = ResolveKey(FilePrefix + filepath.Join(t.TempDir(), "missing"))
	require.ErrorContains(t, err, "reading key file")
}
