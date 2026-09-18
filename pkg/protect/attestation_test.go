package protect

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The DSSE pre-authentication encoding must match the spec byte for byte:
// interoperability with cosign/in-toto verifiers depends on it.
func TestPAEEncoding(t *testing.T) {
	t.Parallel()

	// The DSSE spec's own test vector.
	assert.Equal(t, "DSSEv1 29 http://example.com/HelloWorld 11 hello world",
		string(paeEncode("http://example.com/HelloWorld", []byte("hello world"))))
	assert.Equal(t, "DSSEv1 28 application/vnd.in-toto+json 0 ", string(paeEncode(PayloadType, nil)))

	// Length-prefixing must make the encoding unambiguous: two different
	// (type, body) pairs can never produce the same bytes.
	assert.NotEqual(t, string(paeEncode("ab", []byte("cd"))), string(paeEncode("ab c", []byte("d"))))
}

// The stored annotation must be a DSSE envelope a third-party verifier can
// consume: a JSON object with payload/payloadType/signatures, whose payload is
// an in-toto Statement v1 with bare-hex digests.
func TestAttestation_WireFormat(t *testing.T) {
	t.Parallel()

	kp := keyPairs(t)["ed25519/pkcs8+pkix"]
	priv := mustParse(t, kp.priv)
	annotations := map[string]string{}
	require.NoError(t, protect(t, priv, annotations, []byte(payload), ModeSign))

	env := envelopeIn(t, annotations)
	assert.Equal(t, PayloadType, env.PayloadType)
	require.Len(t, env.Signatures, 1)
	assert.Equal(t, priv.Fingerprint(), env.Signatures[0].KeyID)

	body, err := base64.StdEncoding.DecodeString(env.Payload)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	assert.Equal(t, StatementType, raw["_type"])
	assert.Equal(t, PredicateTypePublication, raw["predicateType"])

	subjects, ok := raw["subject"].([]any)
	require.True(t, ok)
	require.Len(t, subjects, 1)
	subject := subjects[0].(map[string]any)
	assert.Equal(t, testRef, subject["name"])
	digest := subject["digest"].(map[string]any)["sha256"].(string)
	// in-toto digests are bare hex, never "sha256:"-prefixed.
	assert.NotContains(t, digest, ":")
	sum := sha256.Sum256([]byte(payload))
	assert.Equal(t, hex.EncodeToString(sum[:]), digest)

	predicate := raw["predicate"].(map[string]any)
	assert.Equal(t, "index.docker.io", predicate["registry"])
	assert.Equal(t, "library/agent", predicate["repository"])
	assert.Equal(t, "v1", predicate["tag"])

	// The predicate type is advertised in clear so a consumer can filter
	// attestations without decoding the envelope.
	assert.Equal(t, PredicateTypePublication, annotations[AnnotationPredicateType])
	assert.Equal(t, raw["predicateType"], annotations[AnnotationPredicateType])
}

// The predicate-type annotation is a convenience hint, not evidence: it is
// outside the signature, so a verifier must ignore it and report the signed
// value instead.
func TestPredicateTypeAnnotation_IsNotTrusted(t *testing.T) {
	t.Parallel()

	kp := keyPairs(t)["ecdsa/pkcs8+pkix"]
	priv, pub := mustParse(t, kp.priv), mustParse(t, kp.pub)
	annotations := map[string]string{}
	require.NoError(t, protect(t, priv, annotations, []byte(payload), ModeSign))

	// Lying about the predicate type must not change the outcome, and must not
	// make the predicate look unreadable: the signed statement still says what
	// it says.
	lying := maps.Clone(annotations)
	lying[AnnotationPredicateType] = "https://example.com/something-else/v1"
	v, err := pub.VerifyAnnotations(lying, []byte(payload))
	require.NoError(t, err)
	assert.Equal(t, PredicateTypePublication, v.Statement.PredicateType)
	assert.True(t, v.Statement.PredicateUnderstood)

	// Removing it entirely is equally harmless.
	stripped := maps.Clone(annotations)
	delete(stripped, AnnotationPredicateType)
	v, err = pub.VerifyAnnotations(stripped, []byte(payload))
	require.NoError(t, err)
	assert.Equal(t, PredicateTypePublication, v.Statement.PredicateType)
}

// An independent DSSE verifier must accept our signature: recompute the PAE by
// hand and check it with the raw crypto primitive, the way cosign would — no
// code from this package involved in the verification itself.
func TestAttestation_VerifiableWithoutThisPackage(t *testing.T) {
	t.Parallel()

	kp := keyPairs(t)["ed25519/pkcs8+pkix"]
	priv := mustParse(t, kp.priv)
	annotations := map[string]string{}
	require.NoError(t, protect(t, priv, annotations, []byte(payload), ModeSign))

	env := envelopeIn(t, annotations)
	body, err := base64.StdEncoding.DecodeString(env.Payload)
	require.NoError(t, err)
	sig, err := base64.StdEncoding.DecodeString(env.Signatures[0].Sig)
	require.NoError(t, err)

	pae := fmt.Sprintf("DSSEv1 %d %s %d %s", len(env.PayloadType), env.PayloadType, len(body), body)
	assert.True(t, ed25519.Verify(priv.pub.(ed25519.PublicKey), []byte(pae), sig))
	// And the signature is over the PAE, not over the bare body.
	assert.False(t, ed25519.Verify(priv.pub.(ed25519.PublicKey), body, sig))
}

// Both base64 alphabets are accepted on the way in, since DSSE producers differ.
func TestEnvelope_AcceptsURLSafeBase64(t *testing.T) {
	t.Parallel()

	kp := keyPairs(t)["ecdsa/pkcs8+pkix"]
	priv, pub := mustParse(t, kp.priv), mustParse(t, kp.pub)
	annotations := map[string]string{}
	require.NoError(t, protect(t, priv, annotations, []byte(payload), ModeSign))

	env := envelopeIn(t, annotations)
	body, err := base64.StdEncoding.DecodeString(env.Payload)
	require.NoError(t, err)
	sig, err := base64.StdEncoding.DecodeString(env.Signatures[0].Sig)
	require.NoError(t, err)

	for name, encoding := range map[string]*base64.Encoding{
		"raw std": base64.RawStdEncoding,
		"url":     base64.URLEncoding,
		"raw url": base64.RawURLEncoding,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reencoded := Envelope{
				Payload:     encoding.EncodeToString(body),
				PayloadType: PayloadType,
				Signatures:  []Signature{{Sig: encoding.EncodeToString(sig)}},
			}
			verified, err := pub.VerifyEnvelope(reencoded)
			require.NoError(t, err)
			assert.Equal(t, body, verified)
		})
	}
}

// keyid is an unauthenticated hint: it must never decide whether a signature
// counts, and a wrong or missing one must not affect the outcome.
func TestEnvelope_KeyIDIsNotTrusted(t *testing.T) {
	t.Parallel()

	kp := keyPairs(t)["ecdsa/pkcs8+pkix"]
	priv, pub := mustParse(t, kp.priv), mustParse(t, kp.pub)
	annotations := map[string]string{}
	require.NoError(t, protect(t, priv, annotations, []byte(payload), ModeSign))
	env := envelopeIn(t, annotations)

	lying := env
	lying.Signatures = []Signature{{KeyID: "not-our-key-id", Sig: env.Signatures[0].Sig}}
	_, err := pub.VerifyEnvelope(lying)
	require.NoError(t, err, "a wrong keyid must not reject a valid signature")

	// Conversely, a matching keyid over a bogus signature proves nothing.
	forged := env
	forged.Signatures = []Signature{{KeyID: priv.Fingerprint(), Sig: base64.StdEncoding.EncodeToString([]byte("garbage"))}}
	_, err = pub.VerifyEnvelope(forged)
	require.ErrorIs(t, err, ErrInvalidSignature)

	// Every signature is tried, so a valid one among junk still verifies.
	mixed := env
	mixed.Signatures = []Signature{
		{Sig: "not base64!"},
		{KeyID: "someone-else", Sig: base64.StdEncoding.EncodeToString([]byte("garbage"))},
		env.Signatures[0],
	}
	_, err = pub.VerifyEnvelope(mixed)
	require.NoError(t, err)
}

// The attested metadata must be authenticated: editing any field invalidates
// the signature, and a statement lifted from another artifact does not attest
// this one.
func TestAttestation_IsAuthenticated(t *testing.T) {
	t.Parallel()

	kp := keyPairs(t)["ecdsa/pkcs8+pkix"]
	priv, pub := mustParse(t, kp.priv), mustParse(t, kp.pub)
	annotations := map[string]string{}
	require.NoError(t, protect(t, priv, annotations, []byte(payload), ModeSign))

	// Tamper with the subject while keeping the signature.
	forged := maps.Clone(annotations)
	stmt := stmtFor(t, []byte(payload))
	stmt.Subject = []Subject{{Name: "index.docker.io/attacker/agent:v1", Digest: SubjectDigest([]byte(payload))}}
	body, err := stmt.Marshal()
	require.NoError(t, err)
	env := envelopeIn(t, annotations)
	env.Payload = base64.StdEncoding.EncodeToString(body)
	rawEnv, err := json.Marshal(env)
	require.NoError(t, err)
	forged[AnnotationAttestation] = base64.StdEncoding.EncodeToString(rawEnv)
	require.ErrorIs(t, verifyErr(pub, forged, []byte(payload)), ErrInvalidSignature)

	// Strip the attestation: an encrypted-only artifact from an asymmetric key
	// is not proof, and a signature cannot be recovered from nothing.
	stripped := maps.Clone(annotations)
	delete(stripped, AnnotationAttestation)
	require.ErrorIs(t, verifyErr(pub, stripped, []byte(payload)), ErrNotProtected)

	// An attestation validly signed for other content does not attest this one.
	other := []byte("agents:\n  root:\n    model: other\n")
	otherAnnotations := map[string]string{}
	require.NoError(t, protect(t, priv, otherAnnotations, other, ModeSign))
	require.ErrorIs(t, verifyErr(pub, otherAnnotations, []byte(payload)), ErrStatementMismatch)
	require.NoError(t, verifyErr(pub, otherAnnotations, other))

	// A foreign statement type is refused rather than guessed at, even when
	// validly signed: `_type` is what makes the subject binding meaningful.
	alien := maps.Clone(annotations)
	body, err = json.Marshal(map[string]any{
		"_type":         "https://example.com/NotAStatement/v1",
		"subject":       []Subject{{Name: testRef, Digest: SubjectDigest([]byte(payload))}},
		"predicateType": PredicateTypePublication,
		"predicate":     map[string]string{},
	})
	require.NoError(t, err)
	reseal(t, priv, alien, body)
	require.ErrorIs(t, verifyErr(pub, alien, []byte(payload)), ErrUnknownStatementType)
}

// Forward compatibility, the whole point of the lenient predicate: an
// artifact published by a future version that adds predicate fields must still
// verify here, and the unknown metadata must be surfaced rather than dropped.
func TestPredicate_UnknownFieldsStillVerify(t *testing.T) {
	t.Parallel()

	kp := keyPairs(t)["ecdsa/pkcs8+pkix"]
	priv, pub := mustParse(t, kp.priv), mustParse(t, kp.pub)
	annotations := map[string]string{}
	require.NoError(t, protect(t, priv, annotations, []byte(payload), ModeSign))

	// What a future publisher would emit: the same predicateType with extra
	// fields this version has never heard of.
	body, err := json.Marshal(map[string]any{
		"_type":         StatementType,
		"subject":       []Subject{{Name: testRef, Digest: SubjectDigest([]byte(payload))}},
		"predicateType": PredicateTypePublication,
		"predicate": map[string]any{
			"registry":   "index.docker.io",
			"repository": "library/agent",
			"tag":        "v1",
			"created":    "2026-09-11T08:30:00Z",
			"builder":    "docker-agent/9.9.9",
			"provenance": map[string]any{"workflow": "release.yml"},
		},
	})
	require.NoError(t, err)
	reseal(t, priv, annotations, body)

	v, err := pub.VerifyAnnotations(annotations, []byte(payload))
	require.NoError(t, err)
	assert.True(t, v.Statement.PredicateUnderstood)
	assert.Equal(t, "2026-09-11T08:30:00Z", v.Statement.Predicate.Created)
	assert.Equal(t, "library/agent", v.Statement.Predicate.Repository)
	// Unknown fields are kept as opaque JSON, not silently dropped.
	assert.Equal(t, `"docker-agent/9.9.9"`, v.Statement.Predicate.Unknown["builder"])
	assert.JSONEq(t, `{"workflow":"release.yml"}`, v.Statement.Predicate.Unknown["provenance"])
	require.NoError(t, v.CheckSubject(testRef))

	// A known field carrying an unexpected type is opaque too, never fatal.
	body, err = json.Marshal(map[string]any{
		"_type":         StatementType,
		"subject":       []Subject{{Name: testRef, Digest: SubjectDigest([]byte(payload))}},
		"predicateType": PredicateTypePublication,
		"predicate":     map[string]any{"registry": "index.docker.io", "tag": 3},
	})
	require.NoError(t, err)
	reseal(t, priv, annotations, body)
	v, err = pub.VerifyAnnotations(annotations, []byte(payload))
	require.NoError(t, err)
	assert.Equal(t, "index.docker.io", v.Statement.Predicate.Registry)
	assert.Empty(t, v.Statement.Predicate.Tag)
	assert.Equal(t, "3", v.Statement.Predicate.Unknown["tag"])
}

// An unknown predicateType is "signature valid, predicate not understood" —
// an honest outcome, not a failure. The subject binding still holds.
func TestPredicate_UnknownTypeIsNotFatal(t *testing.T) {
	t.Parallel()

	kp := keyPairs(t)["ecdsa/pkcs8+pkix"]
	priv, pub := mustParse(t, kp.priv), mustParse(t, kp.pub)
	annotations := map[string]string{}
	require.NoError(t, protect(t, priv, annotations, []byte(payload), ModeSign))

	body, err := json.Marshal(map[string]any{
		"_type":         StatementType,
		"subject":       []Subject{{Name: testRef, Digest: SubjectDigest([]byte(payload))}},
		"predicateType": "https://docker.com/docker-agent/share/publication/v2",
		"predicate":     map[string]any{"shape": "we cannot know"},
	})
	require.NoError(t, err)
	reseal(t, priv, annotations, body)

	v, err := pub.VerifyAnnotations(annotations, []byte(payload))
	require.NoError(t, err)
	assert.False(t, v.Statement.PredicateUnderstood)
	assert.Empty(t, v.Statement.Predicate.Created)
	assert.Contains(t, v.String(), "not understood")
	// Everything security-critical still applies.
	require.NoError(t, v.CheckSubject(testRef))
	require.ErrorIs(t, v.CheckSubject("attacker/agent:v1"), ErrSubjectMismatch)
	require.ErrorIs(t, verifyErr(pub, annotations, []byte("other")), ErrStatementMismatch)
}

// The predicate is re-emitted verbatim, so a verifier that re-signs or stores a
// parsed statement cannot silently drop a field it did not understand.
func TestStatement_RoundTripPreservesUnknownPredicateFields(t *testing.T) {
	t.Parallel()

	original := []byte(`{"_type":"https://in-toto.io/Statement/v1","subject":[{"name":"index.docker.io/library/agent:v1","digest":{"sha256":"` +
		hexDigest([]byte(payload)) + `"}}],"predicateType":"https://docker.com/docker-agent/share/publication/v1","predicate":{"registry":"index.docker.io","future":"kept"}}`)

	stmt, err := ParseStatement(original)
	require.NoError(t, err)
	assert.Equal(t, `"kept"`, stmt.Predicate.Unknown["future"])

	remarshaled, err := stmt.Marshal()
	require.NoError(t, err)
	assert.Contains(t, string(remarshaled), `"future":"kept"`)

	again, err := ParseStatement(remarshaled)
	require.NoError(t, err)
	assert.Equal(t, stmt, again)
}

func hexDigest(data []byte) string {
	return SubjectDigest(data)["sha256"]
}

// The attested subject is what makes a copied artifact detectable.
func TestAttestation_CheckSubject(t *testing.T) {
	t.Parallel()

	kp := keyPairs(t)["ecdsa/pkcs8+pkix"]
	priv, pub := mustParse(t, kp.priv), mustParse(t, kp.pub)
	annotations := map[string]string{}
	require.NoError(t, protect(t, priv, annotations, []byte(payload), ModeSign))

	v, err := pub.VerifyAnnotations(annotations, []byte(payload))
	require.NoError(t, err)

	// Equivalent shorthands for the same reference all match.
	for _, ref := range []string{testRef, "library/agent:v1", "docker.io/library/agent:v1"} {
		require.NoError(t, v.CheckSubject(ref), ref)
	}
	require.ErrorIs(t, v.CheckSubject("library/agent:v2"), ErrSubjectMismatch)
	require.ErrorIs(t, v.CheckSubject("attacker/agent:v1"), ErrSubjectMismatch)
	require.ErrorIs(t, v.CheckSubject("ghcr.io/library/agent:v1"), ErrSubjectMismatch)
	// An unparseable reference is not reported as a mismatch.
	require.NoError(t, v.CheckSubject("not a reference"))

	// Nothing signed, nothing to check: a symmetric encrypt-only artifact
	// carries no attestation, so CheckSubject must stay silent.
	key := mustParse(t, []byte(secret))
	encrypted := map[string]string{}
	require.NoError(t, protect(t, key, encrypted, []byte(payload), ModeEncrypt))
	ev, err := key.VerifyAnnotations(encrypted, []byte(payload))
	require.NoError(t, err)
	assert.Empty(t, ev.Statement.SubjectName())
	require.NoError(t, ev.CheckSubject("anything/at:all"))
}

// A statement may attest several subjects; a match against any of them is a
// match, and the digest binding works the same way.
func TestStatement_MultipleSubjects(t *testing.T) {
	t.Parallel()

	kp := keyPairs(t)["ed25519/pkcs8+pkix"]
	priv, pub := mustParse(t, kp.priv), mustParse(t, kp.pub)
	annotations := map[string]string{}
	require.NoError(t, protect(t, priv, annotations, []byte(payload), ModeSign))

	body, err := json.Marshal(map[string]any{
		"_type": StatementType,
		"subject": []Subject{
			{Name: "index.docker.io/library/agent:v1", Digest: SubjectDigest([]byte(payload))},
			{Name: "ghcr.io/library/agent:v1", Digest: SubjectDigest([]byte(payload))},
		},
		"predicateType": PredicateTypePublication,
		"predicate":     map[string]string{"registry": "index.docker.io"},
	})
	require.NoError(t, err)
	reseal(t, priv, annotations, body)

	v, err := pub.VerifyAnnotations(annotations, []byte(payload))
	require.NoError(t, err)
	require.NoError(t, v.CheckSubject("library/agent:v1"))
	require.NoError(t, v.CheckSubject("ghcr.io/library/agent:v1"))
	require.ErrorIs(t, v.CheckSubject("quay.io/library/agent:v1"), ErrSubjectMismatch)
}

// The statement records the parsed components of the reference so consumers can
// display them without re-parsing.
func TestStatement_Fields(t *testing.T) {
	t.Parallel()

	data := []byte(payload)
	stmt, err := NewStatement("gtardif/myagent:v3", data, time.Date(2026, 9, 11, 8, 30, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Equal(t, StatementType, stmt.Type)
	assert.Equal(t, PredicateTypePublication, stmt.PredicateType)
	assert.Equal(t, "index.docker.io/gtardif/myagent:v3", stmt.SubjectName())
	assert.Equal(t, "index.docker.io", stmt.Predicate.Registry)
	assert.Equal(t, "gtardif/myagent", stmt.Predicate.Repository)
	assert.Equal(t, "v3", stmt.Predicate.Tag)
	assert.Equal(t, "sha256:"+hexDigest(data), stmt.Digest())
	assert.Equal(t, "2026-09-11T08:30:00Z", stmt.Predicate.Created)

	// Creation dates are normalized to UTC so the attested value is unambiguous.
	east := time.FixedZone("UTC+5", 5*60*60)
	stmt, err = NewStatement("gtardif/myagent:v3", data, time.Date(2026, 9, 11, 13, 30, 0, 0, east))
	require.NoError(t, err)
	assert.Equal(t, "2026-09-11T08:30:00Z", stmt.Predicate.Created)

	// A digest reference has no tag.
	stmt, err = NewStatement("gtardif/myagent@sha256:"+hexDigest(data), data, time.Now())
	require.NoError(t, err)
	assert.Empty(t, stmt.Predicate.Tag)
	assert.Equal(t, "gtardif/myagent", stmt.Predicate.Repository)

	_, err = NewStatement("not a reference", data, time.Now())
	require.Error(t, err)
}

// Protect must refuse to sign a statement that does not attest the data.
func TestStatement_MustDescribeData(t *testing.T) {
	t.Parallel()

	key := mustParse(t, []byte(secret))
	mismatched := stmtFor(t, []byte("some other content"))
	err := key.Protect(map[string]string{}, []byte(payload), mismatched, ModeSign)
	require.ErrorIs(t, err, ErrStatementMismatch)
}

// The security-critical half of the statement is strict: anything that would
// weaken the subject↔digest binding is refused.
func TestParseStatement_StrictEnvelope(t *testing.T) {
	t.Parallel()

	digest := hexDigest([]byte(payload))
	valid := `{"_type":"https://in-toto.io/Statement/v1","subject":[{"name":"a:b","digest":{"sha256":"` + digest + `"}}],"predicateType":"x","predicate":{}}`
	_, err := ParseStatement([]byte(valid))
	require.NoError(t, err)

	for name, raw := range map[string]string{
		"empty":               "",
		"truncated":           "{",
		"array":               "[]",
		"unknown top field":   strings.Replace(valid, `"predicate":{}`, `"predicate":{},"extra":true`, 1),
		"unknown subj field":  strings.Replace(valid, `"name":"a:b"`, `"name":"a:b","extra":1`, 1),
		"no subject":          strings.Replace(valid, `[{"name":"a:b","digest":{"sha256":"`+digest+`"}}]`, `[]`, 1),
		"no subject name":     strings.Replace(valid, `"name":"a:b",`, ``, 1),
		"no digest":           strings.Replace(valid, `"digest":{"sha256":"`+digest+`"}`, `"digest":{}`, 1),
		"foreign digest only": strings.Replace(valid, `"sha256":"`+digest+`"`, `"sha512":"`+digest+`"`, 1),
		"short digest":        strings.Replace(valid, digest, "abcd", 1),
		"non-hex digest":      strings.Replace(valid, digest, strings.Repeat("z", 64), 1),
		"wrong type":          strings.Replace(valid, StatementType, "https://example.com/v1", 1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseStatement([]byte(raw))
			require.Error(t, err, raw)
		})
	}
}

// An envelope must be a well-formed DSSE envelope of the expected payload type
// before anything in it is looked at.
func TestParseEnvelope_Malformed(t *testing.T) {
	t.Parallel()

	for name, raw := range map[string]string{
		"empty":            "",
		"truncated":        "{",
		"unknown field":    `{"payload":"e30=","payloadType":"` + PayloadType + `","signatures":[{"sig":"AA"}],"extra":1}`,
		"wrong type":       `{"payload":"e30=","payloadType":"application/json","signatures":[{"sig":"AA"}]}`,
		"no payload type":  `{"payload":"e30=","signatures":[{"sig":"AA"}]}`,
		"no signatures":    `{"payload":"e30=","payloadType":"` + PayloadType + `","signatures":[]}`,
		"null signatures":  `{"payload":"e30=","payloadType":"` + PayloadType + `"}`,
		"signature is str": `{"payload":"e30=","payloadType":"` + PayloadType + `","signatures":"AA"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseEnvelope([]byte(raw))
			require.ErrorIs(t, err, ErrMalformedAttestation)
		})
	}

	// A malformed payload is caught at verification, not at parse time.
	key := mustParse(t, []byte(secret))
	env, err := ParseEnvelope([]byte(`{"payload":"not base64!","payloadType":"` + PayloadType + `","signatures":[{"sig":"AA"}]}`))
	require.NoError(t, err)
	_, err = key.VerifyEnvelope(env)
	require.ErrorIs(t, err, ErrMalformedAttestation)
}
