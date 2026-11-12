package protect

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	// AnnotationAttestation holds the base64 DSSE [Envelope] whose payload is
	// the in-toto [Statement] describing this artifact: the reference it was
	// published as, the digest of the agent YAML, and the publication
	// metadata. Stored in clear so anyone (and any DSSE/in-toto tool) can read
	// it, and authenticated by the signature it carries so only a key holder
	// can have produced it.
	AnnotationAttestation = "io.docker.agent.attestation"
	// AnnotationPredicateType advertises the `predicateType` of the statement
	// inside AnnotationAttestation, so a consumer can tell what an attestation
	// is about without base64-decoding it. Same key BuildKit puts on its
	// in-toto attestation layers. Purely informational: it is not signed, so
	// verification always uses the value inside the envelope (see
	// [Statement.PredicateType]) and never this annotation.
	AnnotationPredicateType = "in-toto.io/predicate-type"
	// AnnotationSignatureAlgorithm names the algorithm of the signature inside
	// AnnotationAttestation.
	AnnotationSignatureAlgorithm = "io.docker.agent.signature.algorithm"
	// AnnotationEncrypted holds a base64 authenticated-encrypted copy of the
	// whole YAML layer.
	AnnotationEncrypted = "io.docker.agent.encrypted"
	// AnnotationEncryptedAlgorithm names the algorithm behind AnnotationEncrypted.
	AnnotationEncryptedAlgorithm = "io.docker.agent.encrypted.algorithm"
)

// EnvelopeMediaType is the media type of the value held in
// [AnnotationAttestation], for consumers that want to know what they are
// looking at. The annotation stores the base64 of exactly this object.
const EnvelopeMediaType = "application/vnd.dsse.envelope.v1+json"

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
// The statement is the in-toto attestation the signature covers; it is stored
// in clear inside a DSSE envelope.
func (k *Key) Protect(annotations map[string]string, data []byte, stmt Statement, mode Mode) error {
	if err := k.Supports(mode); err != nil {
		return err
	}
	if !stmt.describes(data) {
		return fmt.Errorf("%w: no subject digest matches the agent YAML", ErrStatementMismatch)
	}
	if mode == ModeSign || !k.Symmetric() {
		if err := k.sign(annotations, stmt); err != nil {
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

// sign records a DSSE envelope over the statement.
func (k *Key) sign(annotations map[string]string, stmt Statement) error {
	env, err := k.SignStatement(stmt)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshaling attestation: %w", err)
	}
	annotations[AnnotationAttestation] = base64.StdEncoding.EncodeToString(raw)
	// A hint for consumers filtering attestations; the signed copy inside the
	// envelope is the only one verification trusts.
	annotations[AnnotationPredicateType] = stmt.PredicateType
	annotations[AnnotationSignatureAlgorithm] = k.SignAlgorithm()
	return nil
}

// IsProtected reports whether annotations carry an attestation or encrypted copy.
func IsProtected(annotations map[string]string) bool {
	return annotations[AnnotationAttestation] != "" || annotations[AnnotationEncrypted] != ""
}

// Verification reports which protections VerifyAnnotations actually checked.
type Verification struct {
	// SignatureAlgorithm is set when a signature was verified.
	SignatureAlgorithm string
	// EncryptedAlgorithm is set when the encrypted copy was decrypted and
	// matched the content. It stays empty for a public key, which can only
	// check the copy's algorithm label.
	EncryptedAlgorithm string
	// Statement is the authenticated in-toto statement the publisher signed.
	// Set whenever SignatureAlgorithm is.
	Statement Statement
}

// String reports what was checked, including the attested metadata.
func (v Verification) String() string {
	out := v.SignatureAlgorithmSummary()
	if v.Statement.SubjectName() != "" {
		out += " for " + v.Statement.String()
	}
	return out
}

// SignatureAlgorithmSummary names the protections that were checked, without
// the attested metadata.
func (v Verification) SignatureAlgorithmSummary() string {
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
// A DSSE attestation, when present, is always verified: the signature covers
// the in-toto statement, whose subject digest must in turn match data — so a
// valid statement paired with a different layer is rejected. The authenticated
// statement is returned in the report, and callers that know which reference
// they requested should also call [Verification.CheckSubject].
//
// An encrypted copy is decrypted and compared to data when the key can
// decrypt; a public key only checks its algorithm label and relies on the
// signature. With an asymmetric key a signature is mandatory, since anyone
// holding the public key could have produced the encrypted copy.
// ErrNotProtected is returned when the artifact carries no protection at all.
func (k *Key) VerifyAnnotations(annotations map[string]string, data []byte) (Verification, error) {
	var v Verification
	signed := annotations[AnnotationAttestation] != ""
	encrypted := annotations[AnnotationEncrypted] != ""
	if !signed && !encrypted {
		return v, ErrNotProtected
	}
	if !signed && !k.Symmetric() {
		return v, ErrNotSigned
	}

	if signed {
		stmt, err := k.verifyAttestation(annotations, data)
		if err != nil {
			return Verification{}, err
		}
		v.SignatureAlgorithm = k.SignAlgorithm()
		v.Statement = stmt
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

// verifyAttestation checks the DSSE envelope against this key and that the
// in-toto statement it carries attests data. It returns the authenticated
// statement.
func (k *Key) verifyAttestation(annotations map[string]string, data []byte) (Statement, error) {
	if !k.CanVerify() {
		return Statement{}, fmt.Errorf("%w (%s)", ErrCannotVerify, k.Describe())
	}
	if alg := annotations[AnnotationSignatureAlgorithm]; alg != k.SignAlgorithm() {
		return Statement{}, fmt.Errorf("%w: artifact signed with %q but key supports %q", ErrAlgorithmMism, alg, k.SignAlgorithm())
	}
	raw, err := base64.StdEncoding.DecodeString(annotations[AnnotationAttestation])
	if err != nil {
		return Statement{}, fmt.Errorf("%w: %w", ErrMalformedAttestation, err)
	}
	env, err := ParseEnvelope(raw)
	if err != nil {
		return Statement{}, err
	}
	// Verify first, then parse the exact bytes the signature covered: DSSE
	// forbids re-reading the payload out of the envelope after verification,
	// and canonicalization must not depend on this process's JSON encoder.
	body, err := k.VerifyEnvelope(env)
	if err != nil {
		return Statement{}, err
	}
	stmt, err := ParseStatement(body)
	if err != nil {
		return Statement{}, err
	}
	if !stmt.describes(data) {
		return Statement{}, fmt.Errorf("%w: attested digest is %s but the agent YAML is %s", ErrStatementMismatch, stmt.Digest(), digestAlgorithm+":"+sha256Hex(data))
	}
	return stmt, nil
}

// CheckSubject reports whether the attestation names ref as a subject.
// Callers that know which reference they requested should use this to detect a
// signed artifact copied to another location, which the signature alone cannot
// catch. It is a no-op when the artifact carried no attestation (nothing was
// signed) or when ref cannot be parsed.
func (v Verification) CheckSubject(ref string) error {
	if v.Statement.SubjectName() == "" {
		return nil
	}
	normalized, err := fullyQualified(ref)
	if err != nil {
		// An unparseable reference is the caller's problem, not a mismatch.
		return nil //nolint:nilerr // best-effort check; the signature already verified
	}
	if !v.Statement.names(normalized) {
		return fmt.Errorf("%w: signed as %s but read from %s", ErrSubjectMismatch, v.Statement.SubjectName(), normalized)
	}
	return nil
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
