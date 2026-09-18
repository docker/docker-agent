package protect

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
)

const (
	// PayloadType is the DSSE payloadType of the signed body: an in-toto
	// Statement. It is part of the pre-authentication encoding (see
	// paeEncode), so it cannot be swapped without invalidating the signature.
	PayloadType = "application/vnd.in-toto+json"
	// StatementType is the in-toto Statement v1 `_type`.
	StatementType = "https://in-toto.io/Statement/v1"
	// PredicateTypePublication identifies the predicate holding the
	// publication metadata of a shared agent. The URI is the version: a
	// breaking change to the predicate shape gets a new URI, and verifiers
	// that do not know a URI report the predicate as not understood instead of
	// rejecting the artifact.
	PredicateTypePublication = "https://docker.com/docker-agent/share/publication/v1"
	// digestAlgorithm is the only digest algorithm we produce in a subject's
	// digest set. Values are bare hex, as in-toto requires.
	digestAlgorithm = "sha256"
)

var (
	ErrMalformedAttestation = errors.New("malformed attestation annotation")
	ErrUnknownStatementType = errors.New("unsupported in-toto statement type")
	ErrStatementMismatch    = errors.New("attestation does not describe this artifact")
	ErrSubjectMismatch      = errors.New("attested subject does not match the reference being read")
)

// Envelope is a DSSE envelope (Payload holds the base64 in-toto Statement).
// It is stored verbatim in [AnnotationAttestation].
type Envelope struct {
	// Payload is the base64 SERIALIZED_BODY. Verification covers the exact
	// bytes it decodes to, never a re-serialized copy.
	Payload string `json:"payload"`
	// PayloadType must be [PayloadType]; it is authenticated through the PAE.
	PayloadType string `json:"payloadType"`
	// Signatures holds one entry per signer. At least one must verify.
	Signatures []Signature `json:"signatures"`
}

// Signature is one DSSE signature over PAE(payloadType, body).
type Signature struct {
	// KeyID is an unauthenticated hint identifying the signing key. It must
	// never drive a security decision: verification tries every signature.
	KeyID string `json:"keyid,omitempty"`
	// Sig is the base64 raw signature (or MAC).
	Sig string `json:"sig"`
}

// Subject is one in-toto subject: a name and a digest set of bare-hex digests.
type Subject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

// Statement is an in-toto Statement v1: the signed body of the envelope.
//
// The security-critical part is parsed strictly: `_type`, `subject` (each with
// its digest set) and `predicateType` bind the artifact to a location and to
// its bytes, so unknown fields there are refused rather than ignored. The
// predicate is parsed leniently — see [Predicate].
type Statement struct {
	// Type is always [StatementType].
	Type string
	// Subject lists the artifacts the predicate is about. A statement attests
	// all of its subjects equally: a match against any entry is a match.
	Subject []Subject
	// PredicateType names the predicate shape, and is its version.
	PredicateType string
	// Predicate is the publication metadata, populated only when
	// PredicateUnderstood is set.
	Predicate Predicate
	// PredicateRaw is the predicate exactly as received (nil when absent). It
	// is what Marshal re-emits, so unknown fields survive a round-trip.
	PredicateRaw json.RawMessage
	// PredicateUnderstood reports whether PredicateType is a shape this
	// version knows how to read. A false value is a valid outcome: the
	// signature is still verified, the metadata is simply opaque.
	PredicateUnderstood bool
}

// Predicate is the publication metadata of a shared agent: where the artifact
// was published and when.
//
// It is deliberately lenient. Fields a future publisher adds must not break a
// deployed verifier, so anything this version does not know is surfaced in
// Unknown rather than rejected, and a known field carrying an unexpected JSON
// type is treated the same way instead of failing the artifact. Everything
// security-critical lives in the statement instead (see [Statement]).
type Predicate struct {
	// Registry is the registry host, e.g. "index.docker.io".
	Registry string `json:"registry"`
	// Repository is the namespace and repository, e.g. "gtardif/myagent".
	Repository string `json:"repository"`
	// Tag is the tag, e.g. "v1". Empty for a digest reference.
	Tag string `json:"tag,omitempty"`
	// Created is the publication time, RFC 3339, UTC.
	Created string `json:"created"`
	// Unknown holds the fields this version does not understand, as the
	// compact JSON text they were received as. Never written.
	Unknown map[string]string `json:"-"`
}

// statementWire is the strict on-the-wire shape of a statement. The predicate
// stays raw so the strict decoder (DisallowUnknownFields, which recurses into
// nested structs) never touches it.
type statementWire struct {
	Type          string          `json:"_type"`
	Subject       []Subject       `json:"subject"`
	PredicateType string          `json:"predicateType"`
	Predicate     json.RawMessage `json:"predicate,omitempty"`
}

// NewStatement describes data published at ref at time created.
//
// The subject digest covers the agent YAML, not the manifest: annotations are
// part of the manifest, so a manifest digest could never be recorded inside
// one without a circular dependency. Attesting the manifest instead would mean
// storing the envelope as a referring artifact (OCI Referrers API).
func NewStatement(ref string, data []byte, created time.Time) (Statement, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return Statement{}, fmt.Errorf("parsing reference %s: %w", ref, err)
	}
	tag := ""
	if t, ok := parsed.(name.Tag); ok {
		tag = t.TagStr()
	}
	predicate := Predicate{
		Registry:   parsed.Context().RegistryStr(),
		Repository: parsed.Context().RepositoryStr(),
		Tag:        tag,
		Created:    created.UTC().Format(time.RFC3339),
	}
	raw, err := json.Marshal(predicate)
	if err != nil {
		return Statement{}, fmt.Errorf("marshaling predicate: %w", err)
	}
	return Statement{
		Type:                StatementType,
		Subject:             []Subject{{Name: parsed.Name(), Digest: SubjectDigest(data)}},
		PredicateType:       PredicateTypePublication,
		Predicate:           predicate,
		PredicateRaw:        raw,
		PredicateUnderstood: true,
	}, nil
}

// SubjectDigest returns the in-toto digest set of data: bare hex, no
// "sha256:" prefix.
func SubjectDigest(data []byte) map[string]string {
	return map[string]string{digestAlgorithm: sha256Hex(data)}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Marshal returns the statement JSON to sign and store. The predicate is
// re-emitted byte-for-byte when it came from the wire, so unknown fields are
// never dropped.
func (s Statement) Marshal() ([]byte, error) {
	predicate := s.PredicateRaw
	if predicate == nil {
		raw, err := json.Marshal(s.Predicate)
		if err != nil {
			return nil, fmt.Errorf("marshaling predicate: %w", err)
		}
		predicate = raw
	}
	return json.Marshal(statementWire{
		Type:          s.Type,
		Subject:       s.Subject,
		PredicateType: s.PredicateType,
		Predicate:     predicate,
	})
}

// ParseStatement decodes a statement from the exact bytes the signature
// covered. Callers must verify those bytes first: nothing here is trustworthy
// before that, and re-serializing to verify would make verification depend on
// this process's JSON encoder.
//
// The statement envelope is strict — unknown fields, a foreign `_type` or a
// subject without a usable digest are refused. The predicate is not: an
// unknown predicateType or unknown predicate fields yield a statement whose
// metadata is (partly) opaque, not an error.
func ParseStatement(raw []byte) (Statement, error) {
	var wire statementWire
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return Statement{}, fmt.Errorf("%w: %w", ErrMalformedAttestation, err)
	}
	if wire.Type != StatementType {
		return Statement{}, fmt.Errorf("%w: got %q, want %q", ErrUnknownStatementType, wire.Type, StatementType)
	}
	if len(wire.Subject) == 0 {
		return Statement{}, fmt.Errorf("%w: statement has no subject", ErrMalformedAttestation)
	}
	for _, subject := range wire.Subject {
		if err := subject.validate(); err != nil {
			return Statement{}, err
		}
	}
	stmt := Statement{
		Type:          wire.Type,
		Subject:       wire.Subject,
		PredicateType: wire.PredicateType,
		PredicateRaw:  wire.Predicate,
	}
	if wire.PredicateType == PredicateTypePublication {
		stmt.Predicate, stmt.PredicateUnderstood = parsePredicate(wire.Predicate)
	}
	return stmt, nil
}

// validate checks the part of a subject verification relies on: a name to
// compare against the reference read, and a digest to bind the payload.
func (s Subject) validate() error {
	if s.Name == "" {
		return fmt.Errorf("%w: subject has no name", ErrMalformedAttestation)
	}
	digest, ok := s.Digest[digestAlgorithm]
	if !ok {
		return fmt.Errorf("%w: subject %s has no %s digest", ErrMalformedAttestation, s.Name, digestAlgorithm)
	}
	if decoded, err := hex.DecodeString(digest); err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("%w: subject %s has a malformed %s digest %q", ErrMalformedAttestation, s.Name, digestAlgorithm, digest)
	}
	return nil
}

// parsePredicate reads the known fields of a publication predicate and keeps
// everything else opaque. It reports whether the predicate was an object at
// all; anything else is metadata this version cannot read.
func parsePredicate(raw json.RawMessage) (Predicate, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return Predicate{}, false
	}
	var p Predicate
	known := map[string]*string{
		"registry":   &p.Registry,
		"repository": &p.Repository,
		"tag":        &p.Tag,
		"created":    &p.Created,
	}
	for field, value := range fields {
		if target, ok := known[field]; ok {
			var s string
			if err := json.Unmarshal(value, &s); err == nil {
				*target = s
				continue
			}
		}
		if p.Unknown == nil {
			p.Unknown = map[string]string{}
		}
		p.Unknown[field] = string(value)
	}
	return p, true
}

// describes reports whether the statement attests data, i.e. whether any
// subject's digest matches it.
func (s Statement) describes(data []byte) bool {
	want := sha256Hex(data)
	for _, subject := range s.Subject {
		if subject.Digest[digestAlgorithm] == want {
			return true
		}
	}
	return false
}

// names reports whether any subject is the fully qualified reference ref.
func (s Statement) names(ref string) bool {
	for _, subject := range s.Subject {
		if subject.Name == ref {
			return true
		}
	}
	return false
}

// SubjectName returns the first subject's name, for display.
func (s Statement) SubjectName() string {
	if len(s.Subject) == 0 {
		return ""
	}
	return s.Subject[0].Name
}

// Digest returns the first subject's digest in the usual "sha256:<hex>" form,
// for display. In the statement itself digests are bare hex.
func (s Statement) Digest() string {
	if len(s.Subject) == 0 {
		return ""
	}
	digest, ok := s.Subject[0].Digest[digestAlgorithm]
	if !ok {
		return ""
	}
	return digestAlgorithm + ":" + digest
}

// String renders the statement for humans, in a stable field order.
func (s Statement) String() string {
	out := s.SubjectName()
	if digest := s.Digest(); digest != "" {
		out += " " + digest
	}
	if !s.PredicateUnderstood {
		return out + " (predicate " + s.PredicateType + " not understood)"
	}
	if s.Predicate.Created != "" {
		out += " created " + s.Predicate.Created
	}
	return out
}

// SignStatement returns a DSSE envelope over stmt, signed with this key.
func (k *Key) SignStatement(stmt Statement) (Envelope, error) {
	body, err := stmt.Marshal()
	if err != nil {
		return Envelope{}, err
	}
	sig, err := k.Sign(body)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		Payload:     base64.StdEncoding.EncodeToString(body),
		PayloadType: PayloadType,
		// A hint for key selection only; verification never consults it.
		Signatures: []Signature{{KeyID: k.Fingerprint(), Sig: base64.StdEncoding.EncodeToString(sig)}},
	}, nil
}

// ParseEnvelope decodes a DSSE envelope. Nothing in it is authenticated yet.
func ParseEnvelope(raw []byte) (Envelope, error) {
	var env Envelope
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&env); err != nil {
		return Envelope{}, fmt.Errorf("%w: %w", ErrMalformedAttestation, err)
	}
	if env.PayloadType != PayloadType {
		return Envelope{}, fmt.Errorf("%w: payloadType is %q, want %q", ErrMalformedAttestation, env.PayloadType, PayloadType)
	}
	if len(env.Signatures) == 0 {
		return Envelope{}, fmt.Errorf("%w: envelope carries no signature", ErrMalformedAttestation)
	}
	return env, nil
}

// VerifyEnvelope checks env against this key and returns the payload bytes it
// authenticated — the exact SERIALIZED_BODY, which is what callers must parse.
// Re-encoding the returned bytes, or reading the payload out of the envelope
// again afterwards, would break that guarantee.
//
// Every signature is tried: keyid is an unauthenticated hint and must not
// select which one counts.
func (k *Key) VerifyEnvelope(env Envelope) ([]byte, error) {
	if !k.CanVerify() {
		return nil, fmt.Errorf("%w (%s)", ErrCannotVerify, k.Describe())
	}
	body, err := decodeBase64(env.Payload)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed payload: %w", ErrMalformedAttestation, err)
	}
	for _, signature := range env.Signatures {
		sig, err := decodeBase64(signature.Sig)
		if err != nil {
			continue
		}
		if k.Verify(body, sig) == nil {
			return body, nil
		}
	}
	return nil, ErrInvalidSignature
}

// decodeBase64 accepts the four base64 alphabets DSSE producers use in
// practice: standard or URL-safe, padded or not.
func decodeBase64(s string) ([]byte, error) {
	encodings := []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	}
	for _, encoding := range encodings {
		if decoded, err := encoding.DecodeString(s); err == nil {
			return decoded, nil
		}
	}
	return nil, errors.New("not valid base64")
}

// fullyQualified normalizes an OCI reference to its registry-qualified form,
// so that equivalent shorthands ("repo:tag", "docker.io/repo:tag") compare
// equal. It mirrors remote.FullyQualifiedReference, duplicated here to keep
// this package free of the OCI client stack.
func fullyQualified(ref string) (string, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("parsing reference %s: %w", ref, err)
	}
	return parsed.Name(), nil
}
