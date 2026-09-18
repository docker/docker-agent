package root

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/protect"
)

// printAttestation reports the metadata a pull verified, and stays honest about
// a predicate it cannot read: the signature is valid either way.
func TestPrintAttestation(t *testing.T) {
	t.Parallel()

	data := []byte("version: \"2\"\n")
	stmt, err := protect.NewStatement("gtardif/myagent:v1", data, time.Date(2026, 9, 11, 8, 30, 0, 0, time.UTC))
	require.NoError(t, err)

	var buf bytes.Buffer
	printAttestation(cli.NewPrinter(&buf), stmt)
	out := buf.String()
	assert.Contains(t, out, "image:   index.docker.io/gtardif/myagent:v1")
	assert.Contains(t, out, "digest:  sha256:"+protect.SubjectDigest(data)["sha256"])
	assert.Contains(t, out, "created: 2026-09-11T08:30:00Z")

	// Nothing signed (symmetric encrypt-only artifact): nothing to print.
	buf.Reset()
	printAttestation(cli.NewPrinter(&buf), protect.Statement{})
	assert.Empty(t, buf.String())

	// An unknown predicate type is surfaced, not hidden behind blank metadata.
	unknown := stmt
	unknown.PredicateType = "https://docker.com/docker-agent/share/publication/v99"
	unknown.PredicateUnderstood = false
	buf.Reset()
	printAttestation(cli.NewPrinter(&buf), unknown)
	out = buf.String()
	assert.Contains(t, out, "image:   index.docker.io/gtardif/myagent:v1")
	assert.Contains(t, out, "not understood")
	assert.NotContains(t, out, "created:")

	// Unknown predicate fields of a known predicate type are shown, sorted, so
	// metadata added by a newer publisher is not silently swallowed.
	extended := stmt
	extended.Predicate.Unknown = map[string]string{"zeta": `"z"`, "builder": `"docker-agent/9.9.9"`}
	buf.Reset()
	printAttestation(cli.NewPrinter(&buf), extended)
	out = buf.String()
	assert.Less(t, strings.Index(out, "builder"), strings.Index(out, "zeta"))
	assert.Contains(t, out, `builder: "docker-agent/9.9.9"`)
}
