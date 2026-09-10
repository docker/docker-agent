package root

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/cli/cli"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
)

// newPlansTestService builds a hermetic plans.Service over a temp directory,
// returning it so tests can plant files directly. No test ever touches the
// real user data directory.
func newPlansTestService(t *testing.T) (svc plans.Service, sharedDir string) {
	t.Helper()
	sharedDir = t.TempDir()
	svc = plans.NewService(plan.NewFilesystemStorage(sharedDir))
	return svc, sharedDir
}

func executePlansIn(t *testing.T, svc plans.Service, stdin io.Reader, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newPlansCmd(withPlansService(svc))
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetIn(stdin)
	cmd.SetArgs(args)
	cmd.SetContext(t.Context())
	err = cmd.Execute()
	return outBuf.String(), errBuf.String(), err
}

func executePlans(t *testing.T, svc plans.Service, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	return executePlansIn(t, svc, strings.NewReader(""), args...)
}

func mustCreatePlan(t *testing.T, svc plans.Service, name, content string) plans.Plan {
	t.Helper()
	p, err := svc.Create(t.Context(), plans.CreateRequest{Ref: plans.SharedRef(name), Content: content})
	require.NoError(t, err)
	return p
}

func mustGetPlan(t *testing.T, svc plans.Service, ref plans.Ref) plans.Plan {
	t.Helper()
	p, err := svc.Get(t.Context(), ref)
	require.NoError(t, err)
	return p
}

func writePlanContentFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "content.md")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func requirePlansStatusCode(t *testing.T, err error, want int) {
	t.Helper()
	require.Error(t, err)
	statusErr, ok := errors.AsType[cli.StatusError](err)
	require.True(t, ok, "expected a cli.StatusError, got %T", err)
	assert.Equal(t, want, statusErr.StatusCode)
}

type plansTestError struct {
	Code            string `json:"code"`
	Message         string `json:"message"`
	Scope           string `json:"scope"`
	Name            string `json:"name"`
	Op              string `json:"op"`
	ExpectedVersion *int   `json:"expected_version"`
	CurrentVersion  *int   `json:"current_version"`
}

// decodePlansError asserts stderr is exactly one JSON object honouring the
// error contract and returns its "error" body.
func decodePlansError(t *testing.T, stderr string) plansTestError {
	t.Helper()
	assert.Equal(t, 1, strings.Count(stderr, "\n"), "stderr must be a single JSON line: %q", stderr)
	var doc struct {
		SchemaVersion string         `json:"schema_version"`
		Error         plansTestError `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(stderr), &doc), "stderr must be JSON: %q", stderr)
	assert.Equal(t, "1", doc.SchemaVersion)
	require.NotEmpty(t, doc.Error.Code)
	require.NotEmpty(t, doc.Error.Message)
	return doc.Error
}

type plansTestListDocument struct {
	SchemaVersion string       `json:"schema_version"`
	Plans         []plans.Plan `json:"plans"`
	Warnings      []string     `json:"warnings"`
}

type plansTestPlanDocument struct {
	SchemaVersion string     `json:"schema_version"`
	Plan          plans.Plan `json:"plan"`
}

// --- Registration --------------------------------------------------------------

func TestPlansCommand_RegisteredOnRoot(t *testing.T) {
	t.Parallel()

	cmd, _, err := NewRootCmd().Find([]string{"plans"})
	require.NoError(t, err)
	require.Equal(t, "plans", cmd.Name())

	var subs []string
	for _, sub := range cmd.Commands() {
		subs = append(subs, sub.Name())
	}
	for _, want := range []string{"list", "get", "create", "update", "status", "export", "delete"} {
		assert.Contains(t, subs, want)
	}
}

// --- List ----------------------------------------------------------------------

func TestPlansList_EmptyJSON(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)

	stdout, stderr, err := executePlans(t, svc, "list", "--json")
	require.NoError(t, err)
	assert.Empty(t, stderr)

	var doc plansTestListDocument
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc))
	assert.Equal(t, "1", doc.SchemaVersion)
	assert.Empty(t, doc.Plans)
	assert.Empty(t, doc.Warnings)
	// The empty listing must encode as [], never null.
	assert.Contains(t, stdout, `"plans": []`)
}

func TestPlansList_HumanShowsMetadataAndSendsWarningsToStderr(t *testing.T) {
	t.Parallel()
	svc, sharedDir := newPlansTestService(t)
	_, err := svc.Create(t.Context(), plans.CreateRequest{
		Ref: plans.SharedRef("alpha"), Content: "body", Title: "Alpha plan", Status: "draft",
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(sharedDir, "bad.json"), []byte("{nope"), 0o600))

	stdout, stderr, err := executePlans(t, svc, "list")
	require.NoError(t, err)

	assert.Regexp(t, `SCOPE\s+NAME\s+STATUS\s+VERSION\s+UPDATED\s+TITLE`, stdout)
	assert.Regexp(t, `shared\s+alpha\s+draft\s+1\s+\S+\s+Alpha plan`, stdout)
	assert.NotContains(t, stdout, "\x1b[", "human output must be ANSI-free")

	// Human-mode warnings go to stderr, not stdout.
	assert.Contains(t, stderr, "Warning:")
	assert.Contains(t, stderr, "bad")
	assert.NotContains(t, stdout, "Warning")
}

func TestPlansList_JSONMetadataAndWarnings(t *testing.T) {
	t.Parallel()
	svc, sharedDir := newPlansTestService(t)
	_, err := svc.Create(t.Context(), plans.CreateRequest{
		Ref: plans.SharedRef("alpha"), Content: "body", Title: "Alpha plan", Author: "alice", Status: "draft",
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(sharedDir, "bad.json"), []byte("{nope"), 0o600))

	stdout, stderr, err := executePlans(t, svc, "list", "--json")
	require.NoError(t, err)
	assert.Empty(t, stderr, "JSON mode must not write warnings to stderr")

	var doc plansTestListDocument
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc))
	require.Len(t, doc.Plans, 1)

	shared := doc.Plans[0]
	assert.Equal(t, plans.ScopeShared, shared.Scope)
	assert.Equal(t, "alpha", shared.Name)
	assert.Equal(t, "Alpha plan", shared.Title)
	assert.Equal(t, "alice", shared.Author)
	assert.Equal(t, "draft", shared.Status)
	require.NotNil(t, shared.Version)
	assert.Equal(t, 1, *shared.Version)
	assert.Empty(t, shared.Content, "list is metadata only")

	require.Len(t, doc.Warnings, 1)
	assert.Contains(t, doc.Warnings[0], "bad")
}

// --- Get -----------------------------------------------------------------------

func TestPlansGet_SharedHuman(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	_, err := svc.Create(t.Context(), plans.CreateRequest{
		Ref: plans.SharedRef("release"), Content: "# release\n\nstep 1\n", Title: "Release", Status: "draft",
	})
	require.NoError(t, err)

	stdout, stderr, err := executePlans(t, svc, "get", "release")
	require.NoError(t, err)
	// Content alone on stdout; metadata stays visible on stderr.
	assert.Equal(t, "# release\n\nstep 1\n", stdout)
	assert.Contains(t, stderr, `shared plan "release"`)
	assert.Contains(t, stderr, "title: Release")
	assert.Contains(t, stderr, "status: draft")
	assert.Contains(t, stderr, "version: 1")
}

func TestPlansGet_SharedJSON(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	_, err := svc.Create(t.Context(), plans.CreateRequest{
		Ref: plans.SharedRef("release"), Content: "the body", Title: "Release", Author: "alice", Status: "draft",
	})
	require.NoError(t, err)

	stdout, stderr, err := executePlans(t, svc, "get", "release", "--json")
	require.NoError(t, err)
	assert.Empty(t, stderr, "JSON mode keeps metadata in the document, not on stderr")

	var doc plansTestPlanDocument
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc))
	assert.Equal(t, "1", doc.SchemaVersion)
	assert.Equal(t, plans.ScopeShared, doc.Plan.Scope)
	assert.Equal(t, "release", doc.Plan.Name)
	assert.Equal(t, "the body", doc.Plan.Content)
	assert.Equal(t, "Release", doc.Plan.Title)
	assert.Equal(t, "alice", doc.Plan.Author)
	assert.Equal(t, "draft", doc.Plan.Status)
	require.NotNil(t, doc.Plan.Version)
	assert.Equal(t, 1, *doc.Plan.Version)
	assert.False(t, doc.Plan.UpdatedAt.IsZero())

	// The host contract is snake_case; the agent tools' camelCase keys must
	// never leak into it.
	assert.Contains(t, stdout, `"updated_at"`)
	assert.NotContains(t, stdout, `"updatedAt"`)
}

func TestPlansGet_RequiresName(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)

	stdout, stderr, err := executePlans(t, svc, "get", "--json")
	requirePlansStatusCode(t, err, 1)
	assert.Empty(t, stdout)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "invalid_argument", body.Code)
	assert.Contains(t, body.Message, "a plan name is required")
}

func TestPlansGet_NotFoundJSON(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)

	stdout, stderr, err := executePlans(t, svc, "get", "missing", "--json")
	requirePlansStatusCode(t, err, 1)
	assert.Empty(t, stdout)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "not_found", body.Code)
	assert.Equal(t, "shared", body.Scope)
	assert.Equal(t, "missing", body.Name)
}

func TestPlansGet_NotFoundHuman(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)

	stdout, stderr, err := executePlans(t, svc, "get", "missing")
	requirePlansStatusCode(t, err, 1)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, `shared plan "missing" not found`)
	// The error is rendered exactly once, by the command itself.
	assert.Equal(t, 1, strings.Count(stderr, "not found"))
}

func TestPlansGet_InvalidName(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)

	_, stderr, err := executePlans(t, svc, "get", "UPPER", "--json")
	requirePlansStatusCode(t, err, 1)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "invalid_argument", body.Code)
	assert.Contains(t, body.Message, "invalid plan name")
}

func TestPlansGet_Corrupt(t *testing.T) {
	t.Parallel()
	svc, sharedDir := newPlansTestService(t)
	require.NoError(t, os.WriteFile(filepath.Join(sharedDir, "broken.json"), []byte("{not json"), 0o600))

	_, stderr, err := executePlans(t, svc, "get", "broken", "--json")
	requirePlansStatusCode(t, err, 1)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "corrupt", body.Code, "a corrupt plan must never read as missing")
	assert.Equal(t, "shared", body.Scope)
	assert.Equal(t, "broken", body.Name)
}

// --- Create --------------------------------------------------------------------

func TestPlansCreate_FromFile(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	file := writePlanContentFile(t, "plan body")

	stdout, stderr, err := executePlans(t, svc,
		"create", "release", "--file", file, "--title", "Release", "--author", "alice", "--status", "draft")
	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Equal(t, "Created shared plan \"release\" (version 1)\n", stdout)

	p := mustGetPlan(t, svc, plans.SharedRef("release"))
	assert.Equal(t, "plan body", p.Content)
	assert.Equal(t, "Release", p.Title)
	assert.Equal(t, "alice", p.Author)
	assert.Equal(t, "draft", p.Status)
}

func TestPlansCreate_FromStdin(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)

	// Headless: content is piped through a non-TTY stdin via --file -.
	stdout, _, err := executePlansIn(t, svc, strings.NewReader("piped body"), "create", "p", "--file", "-", "--json")
	require.NoError(t, err)

	var doc plansTestPlanDocument
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc))
	assert.Equal(t, "1", doc.SchemaVersion)
	assert.Equal(t, "piped body", doc.Plan.Content)
	require.NotNil(t, doc.Plan.Version)
	assert.Equal(t, 1, *doc.Plan.Version)
}

func TestPlansCreate_StdinAtSizeLimit(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)

	// Exactly the cap is not "over" it: the bounded reader must pass the
	// content through (no off-by-one) and the real filesystem storage must
	// persist it, since the advertised content cap is separate from the
	// stored file's encoded bound.
	content := strings.Repeat("a", plan.MaxPlanContentSize)
	stdout, stderr, err := executePlansIn(t, svc, strings.NewReader(content), "create", "p", "--file", "-", "--json")
	require.NoError(t, err, "stderr: %s", stderr)

	var doc plansTestPlanDocument
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc))
	require.NotNil(t, doc.Plan.Version)
	assert.Equal(t, 1, *doc.Plan.Version)

	p := mustGetPlan(t, svc, plans.SharedRef("p"))
	assert.Len(t, p.Content, plan.MaxPlanContentSize)
}

func TestPlansCreate_FileAtSizeLimit(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	file := writePlanContentFile(t, strings.Repeat("b", plan.MaxPlanContentSize))

	_, stderr, err := executePlans(t, svc, "create", "p", "--file", file)
	require.NoError(t, err, "stderr: %s", stderr)
	assert.Len(t, mustGetPlan(t, svc, plans.SharedRef("p")).Content, plan.MaxPlanContentSize)
}

func TestPlansCreate_StdinOverSizeLimit(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)

	// One byte over the cap must be detected and refused.
	content := strings.Repeat("a", plan.MaxPlanContentSize+1)
	stdout, stderr, err := executePlansIn(t, svc, strings.NewReader(content), "create", "p", "--file", "-", "--json")
	requirePlansStatusCode(t, err, 1)
	assert.Empty(t, stdout)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "invalid_argument", body.Code)
	assert.Contains(t, body.Message, "stdin")
	assert.Contains(t, body.Message, "exceeds the maximum plan size")

	_, err = svc.Get(t.Context(), plans.SharedRef("p"))
	require.Error(t, err, "the refused create must not store a plan")
}

func TestPlansCreate_EmptyStdin(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)

	// Empty piped content reaches the service and is rejected there, like an
	// empty --file.
	_, stderr, err := executePlansIn(t, svc, strings.NewReader(""), "create", "p", "--file", "-", "--json")
	requirePlansStatusCode(t, err, 1)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "invalid_argument", body.Code)
	assert.Contains(t, body.Message, "content must not be empty")
}

func TestPlansCreate_OversizedFile(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	big := filepath.Join(t.TempDir(), "big.md")
	require.NoError(t, os.WriteFile(big, make([]byte, plan.MaxPlanContentSize+1), 0o600))

	_, stderr, err := executePlans(t, svc, "create", "p", "--file", big, "--json")
	requirePlansStatusCode(t, err, 1)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "invalid_argument", body.Code)
	assert.Contains(t, body.Message, "exceeds the maximum plan size")
}

func TestPlansCreate_DirectoryAsFile(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)

	_, stderr, err := executePlans(t, svc, "create", "p", "--file", t.TempDir(), "--json")
	requirePlansStatusCode(t, err, 1)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "invalid_argument", body.Code)
	assert.Contains(t, body.Message, "is a directory, not a file")
}

// stdinMustNotBeRead fails the test when a command reads stdin without being
// asked to via --file -.
type stdinMustNotBeRead struct{ t *testing.T }

func (r stdinMustNotBeRead) Read([]byte) (int, error) {
	r.t.Error("stdin must not be read when --file is not given")
	return 0, io.EOF
}

func TestPlansCreate_RequiresFileWithoutPrompting(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)

	// Without --file the command must fail immediately instead of waiting on
	// an interactive terminal for content.
	_, _, err := executePlansIn(t, svc, stdinMustNotBeRead{t: t}, "create", "p")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"file" not set`)
}

func TestPlansCreate_ExistingNameConflicts(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "original")
	file := writePlanContentFile(t, "clobber")

	stdout, stderr, err := executePlans(t, svc, "create", "p", "--file", file, "--json")
	requirePlansStatusCode(t, err, plansConflictExitCode)
	assert.Empty(t, stdout)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "conflict", body.Code)
	assert.Equal(t, "p", body.Name)
	require.NotNil(t, body.CurrentVersion)
	assert.Equal(t, 1, *body.CurrentVersion)
	// The advice must be actionable for a create: there is no create --force,
	// so the message points at a different name or an update instead.
	assert.Contains(t, body.Message, "already exists")
	assert.Contains(t, body.Message, "update")
	assert.NotContains(t, body.Message, "force")

	p := mustGetPlan(t, svc, plans.SharedRef("p"))
	assert.Equal(t, "original", p.Content, "a conflicting create must not overwrite")
}

func TestPlansCreate_ExistingNameConflictHuman(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "original")
	file := writePlanContentFile(t, "clobber")

	stdout, stderr, err := executePlans(t, svc, "create", "p", "--file", file)
	requirePlansStatusCode(t, err, plansConflictExitCode)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "already exists")
	assert.Contains(t, stderr, "update")
	assert.NotContains(t, stderr, "force", "create has no --force; the human message must not suggest one")
}

// TestPlansCreate_ExistingRevisionZeroFileConflicts proves the create-only
// guard is existence-driven end to end: a valid stored plan whose revision
// field is omitted (reading back as revision 0) still conflicts with exit
// code 3 and stays byte-identical, and a fresh name keeps working.
func TestPlansCreate_ExistingRevisionZeroFileConflicts(t *testing.T) {
	t.Parallel()
	svc, sharedDir := newPlansTestService(t)

	original := `{"name":"planted","content":"precious content"}`
	plantedPath := filepath.Join(sharedDir, "planted.json")
	require.NoError(t, os.WriteFile(plantedPath, []byte(original), 0o600))
	file := writePlanContentFile(t, "clobber")

	stdout, stderr, err := executePlans(t, svc, "create", "planted", "--file", file, "--json")
	requirePlansStatusCode(t, err, plansConflictExitCode)
	assert.Empty(t, stdout)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "conflict", body.Code)
	assert.Equal(t, "planted", body.Name)
	require.NotNil(t, body.ExpectedVersion)
	assert.Equal(t, 0, *body.ExpectedVersion)
	require.NotNil(t, body.CurrentVersion)
	assert.Equal(t, 0, *body.CurrentVersion)
	assert.Contains(t, body.Message, "already exists")

	data, err := os.ReadFile(plantedPath)
	require.NoError(t, err)
	assert.Equal(t, original, string(data), "the refused create must leave the existing file byte-identical")

	// A normal create is unaffected by the guard.
	stdout, _, err = executePlans(t, svc, "create", "fresh", "--file", file, "--json")
	require.NoError(t, err)
	assert.Contains(t, stdout, `"version": 1`)
}

func TestPlansCreate_Validation(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)

	// Empty content is rejected by the service.
	empty := writePlanContentFile(t, "")
	_, stderr, err := executePlans(t, svc, "create", "p", "--file", empty, "--json")
	requirePlansStatusCode(t, err, 1)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "invalid_argument", body.Code)
	assert.Contains(t, body.Message, "content must not be empty")

	// An unreadable content file is a clear input error.
	_, stderr, err = executePlans(t, svc, "create", "p", "--file", filepath.Join(t.TempDir(), "missing.md"), "--json")
	requirePlansStatusCode(t, err, 1)
	body = decodePlansError(t, stderr)
	assert.Equal(t, "invalid_argument", body.Code)
	assert.Contains(t, body.Message, "reading plan content")

	// Invalid plan names are rejected by the service's canonical rule.
	file := writePlanContentFile(t, "x")
	_, stderr, err = executePlans(t, svc, "create", "../escape", "--file", file, "--json")
	requirePlansStatusCode(t, err, 1)
	body = decodePlansError(t, stderr)
	assert.Equal(t, "invalid_argument", body.Code)
	assert.Contains(t, body.Message, "invalid plan name")
}

// --- Update --------------------------------------------------------------------

func TestPlansUpdate_Success(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	_, err := svc.Create(t.Context(), plans.CreateRequest{
		Ref: plans.SharedRef("p"), Content: "v1", Title: "Original", Status: "draft",
	})
	require.NoError(t, err)
	file := writePlanContentFile(t, "v2")

	stdout, _, err := executePlans(t, svc, "update", "p", "--file", file, "--expected-version", "1", "--author", "bob")
	require.NoError(t, err)
	assert.Equal(t, "Updated shared plan \"p\" (version 2)\n", stdout)

	p := mustGetPlan(t, svc, plans.SharedRef("p"))
	assert.Equal(t, "v2", p.Content)
	assert.Equal(t, "Original", p.Title, "omitted metadata flags preserve the previous value")
	assert.Equal(t, "bob", p.Author)
	assert.Equal(t, "draft", p.Status)

	// An explicitly empty metadata flag overwrites rather than preserves.
	stdout, _, err = executePlans(t, svc, "update", "p", "--file", file, "--expected-version", "2", "--title", "")
	require.NoError(t, err)
	assert.Contains(t, stdout, "version 3")
	assert.Empty(t, mustGetPlan(t, svc, plans.SharedRef("p")).Title)
}

func TestPlansUpdate_StaleConflict(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "v1")
	_, err := svc.Update(t.Context(), plans.UpdateRequest{Ref: plans.SharedRef("p"), Content: "v2", ExpectedVersion: new(1)})
	require.NoError(t, err)
	file := writePlanContentFile(t, "stale write")

	stdout, stderr, err := executePlans(t, svc, "update", "p", "--file", file, "--expected-version", "1", "--json")
	requirePlansStatusCode(t, err, plansConflictExitCode)
	assert.Empty(t, stdout, "JSON mode must keep stdout free of prose")

	// The typed conflict stays reachable through the returned error.
	conflictErr, ok := errors.AsType[*plans.ConflictError](err)
	require.True(t, ok, "the cli.StatusError must wrap the typed conflict")
	assert.Equal(t, 2, conflictErr.Current)

	body := decodePlansError(t, stderr)
	assert.Equal(t, "conflict", body.Code)
	assert.Equal(t, "shared", body.Scope)
	assert.Equal(t, "p", body.Name)
	require.NotNil(t, body.ExpectedVersion)
	assert.Equal(t, 1, *body.ExpectedVersion)
	require.NotNil(t, body.CurrentVersion)
	assert.Equal(t, 2, *body.CurrentVersion)

	p := mustGetPlan(t, svc, plans.SharedRef("p"))
	assert.Equal(t, "v2", p.Content, "the losing write must not touch the plan")
}

func TestPlansUpdate_Force(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "v1")
	_, err := svc.Update(t.Context(), plans.UpdateRequest{Ref: plans.SharedRef("p"), Content: "v2", ExpectedVersion: new(1)})
	require.NoError(t, err)
	file := writePlanContentFile(t, "forced")

	stdout, _, err := executePlans(t, svc, "update", "p", "--file", file, "--force")
	require.NoError(t, err)
	assert.Contains(t, stdout, "version 3")
	assert.Equal(t, "forced", mustGetPlan(t, svc, plans.SharedRef("p")).Content)
}

func TestPlansUpdate_RequiresGuard(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "v1")
	file := writePlanContentFile(t, "v2")

	// Neither --expected-version nor --force: refuse instead of passing a
	// nil expected version by accident.
	_, _, err := executePlans(t, svc, "update", "p", "--file", file)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one of the flags in the group [expected-version force] is required")
	assert.Equal(t, "v1", mustGetPlan(t, svc, plans.SharedRef("p")).Content)
}

func TestPlansUpdate_NotFound(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	file := writePlanContentFile(t, "x")

	_, stderr, err := executePlans(t, svc, "update", "ghost", "--file", file, "--force", "--json")
	requirePlansStatusCode(t, err, 1)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "not_found", body.Code)
	assert.Equal(t, "ghost", body.Name)
}

func TestPlansMutations_ExpectedVersionMustBePositive(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "v1")
	file := writePlanContentFile(t, "v2")

	for _, args := range [][]string{
		{"update", "p", "--file", file, "--expected-version", "0"},
		{"status", "p", "done", "--expected-version", "-1"},
		{"delete", "p", "--expected-version", "0"},
	} {
		_, stderr, err := executePlans(t, svc, append(args, "--json")...)
		requirePlansStatusCode(t, err, 1)
		body := decodePlansError(t, stderr)
		assert.Equal(t, "invalid_argument", body.Code, "args %v", args)
		assert.Contains(t, body.Message, "--expected-version must be at least 1", "args %v", args)
	}
}

// --- Status --------------------------------------------------------------------

func TestPlansStatus_Success(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "body")

	stdout, _, err := executePlans(t, svc, "status", "p", "in-progress", "--expected-version", "1")
	require.NoError(t, err)
	assert.Equal(t, "Set status of shared plan \"p\" to \"in-progress\" (version 2)\n", stdout)

	p := mustGetPlan(t, svc, plans.SharedRef("p"))
	assert.Equal(t, "in-progress", p.Status)
	assert.Equal(t, "body", p.Content, "setting the status must preserve the body")
}

func TestPlansStatus_StaleConflict(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	_, err := svc.Create(t.Context(), plans.CreateRequest{Ref: plans.SharedRef("p"), Content: "body", Status: "draft"})
	require.NoError(t, err)
	_, err = svc.Update(t.Context(), plans.UpdateRequest{Ref: plans.SharedRef("p"), Content: "v2", ExpectedVersion: new(1)})
	require.NoError(t, err)

	_, stderr, err := executePlans(t, svc, "status", "p", "done", "--expected-version", "1", "--json")
	requirePlansStatusCode(t, err, plansConflictExitCode)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "conflict", body.Code)
	require.NotNil(t, body.CurrentVersion)
	assert.Equal(t, 2, *body.CurrentVersion)

	assert.Equal(t, "draft", mustGetPlan(t, svc, plans.SharedRef("p")).Status, "the losing status write must not touch the plan")
}

func TestPlansStatus_Validation(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "body")

	// An empty status string is rejected by the service.
	_, stderr, err := executePlans(t, svc, "status", "p", "", "--force", "--json")
	requirePlansStatusCode(t, err, 1)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "invalid_argument", body.Code)
	assert.Contains(t, body.Message, "status must not be empty")

	// A single positional in shared scope is missing either name or status.
	_, stderr, err = executePlans(t, svc, "status", "p", "--force", "--json")
	requirePlansStatusCode(t, err, 1)
	body = decodePlansError(t, stderr)
	assert.Equal(t, "invalid_argument", body.Code)
	assert.Contains(t, body.Message, "plans status <name> <status>")
}

// TestPlansMetadata_LargeLabelsAccepted proves metadata stays free-form
// through the CLI: title, author, and status beyond 4 KiB are accepted by
// create and status, preserved across a content-only update, and never
// misclassified as invalid_argument (issue #3844: labels are user-defined).
func TestPlansMetadata_LargeLabelsAccepted(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)

	bigTitle := strings.Repeat("t", 5<<10)
	bigAuthor := strings.Repeat("a", 5<<10)
	bigStatus := strings.Repeat("s", 5<<10)

	file := writePlanContentFile(t, "body")
	_, stderr, err := executePlans(t, svc, "create", "p", "--file", file,
		"--title", bigTitle, "--author", bigAuthor, "--status", bigStatus)
	require.NoError(t, err, "large metadata must be accepted: %s", stderr)

	biggerStatus := strings.Repeat("z", 6<<10)
	_, stderr, err = executePlans(t, svc, "status", "p", biggerStatus, "--expected-version", "1")
	require.NoError(t, err, "a large status must be accepted: %s", stderr)

	// A content-only update preserves the large labels.
	newFile := writePlanContentFile(t, "new body")
	_, stderr, err = executePlans(t, svc, "update", "p", "--file", newFile, "--expected-version", "2")
	require.NoError(t, err, "stderr: %s", stderr)

	p := mustGetPlan(t, svc, plans.SharedRef("p"))
	assert.Equal(t, bigTitle, p.Title)
	assert.Equal(t, bigAuthor, p.Author)
	assert.Equal(t, biggerStatus, p.Status)
	assert.Equal(t, "new body", p.Content)
}

// --- Export --------------------------------------------------------------------

func TestPlansExport_Shared(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "release", "the body")
	dest := filepath.Join(t.TempDir(), "nested", "plan.md")

	stdout, stderr, err := executePlans(t, svc, "export", "release", "--output", dest)
	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Equal(t, "Exported shared plan \"release\" (version 1) to "+dest+" (8 bytes)\n", stdout)

	data, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "the body", string(data))
}

func TestPlansExport_JSON(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "release", "the body")
	dest := filepath.Join(t.TempDir(), "plan.md")

	stdout, _, err := executePlans(t, svc, "export", "release", "--output", dest, "--json")
	require.NoError(t, err)

	var doc struct {
		SchemaVersion string             `json:"schema_version"`
		Export        plans.ExportResult `json:"export"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc))
	assert.Equal(t, "1", doc.SchemaVersion)
	assert.Equal(t, plans.ScopeShared, doc.Export.Scope)
	assert.Equal(t, "release", doc.Export.Name)
	assert.Equal(t, dest, doc.Export.Path)
	require.NotNil(t, doc.Export.Version)
	assert.Equal(t, 1, *doc.Export.Version)
	assert.Equal(t, len("the body"), doc.Export.BytesWritten)
	assert.Contains(t, stdout, `"bytes_written"`)
	assert.NotContains(t, stdout, `"bytesWritten"`)

	data, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "the body", string(data))
}

func TestPlansExport_NotFound(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	dest := filepath.Join(t.TempDir(), "plan.md")

	_, stderr, err := executePlans(t, svc, "export", "ghost", "--output", dest, "--json")
	requirePlansStatusCode(t, err, 1)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "not_found", body.Code)
	assert.NoFileExists(t, dest)
}

func TestPlansExport_RequiresOutput(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "body")

	_, _, err := executePlans(t, svc, "export", "p")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"output" not set`)
}

func TestPlansExport_RefusesExistingDestination(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "release", "new body")
	dest := filepath.Join(t.TempDir(), "plan.md")
	require.NoError(t, os.WriteFile(dest, []byte("precious"), 0o600))

	stdout, stderr, err := executePlans(t, svc, "export", "release", "--output", dest, "--json")
	requirePlansStatusCode(t, err, 1)
	assert.Empty(t, stdout)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "invalid_argument", body.Code)
	assert.Contains(t, body.Message, "already exists")

	data, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "precious", string(data), "a refused export must preserve the destination")
}

func TestPlansExport_ForceReplacesExistingFile(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "release", "new body")
	dest := filepath.Join(t.TempDir(), "plan.md")
	require.NoError(t, os.WriteFile(dest, []byte("old"), 0o600))

	stdout, stderr, err := executePlans(t, svc, "export", "release", "--output", dest, "--force")
	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Contains(t, stdout, "Exported shared plan \"release\"")

	data, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "new body", string(data))
}

// --- Delete --------------------------------------------------------------------

func TestPlansDelete_WithExpectedVersion(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "body")

	stdout, _, err := executePlans(t, svc, "delete", "p", "--expected-version", "1")
	require.NoError(t, err)
	assert.Equal(t, "Deleted shared plan \"p\"\n", stdout)

	_, err = svc.Get(t.Context(), plans.SharedRef("p"))
	notFound, ok := errors.AsType[*plans.NotFoundError](err)
	require.True(t, ok, "expected a *plans.NotFoundError, got %v", err)
	assert.Equal(t, "p", notFound.Name)
}

func TestPlansDelete_JSON(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "body")

	stdout, _, err := executePlans(t, svc, "delete", "p", "--force", "--json")
	require.NoError(t, err)

	var doc struct {
		SchemaVersion string `json:"schema_version"`
		Deleted       struct {
			Scope string `json:"scope"`
			Name  string `json:"name"`
		} `json:"deleted"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc))
	assert.Equal(t, "1", doc.SchemaVersion)
	assert.Equal(t, "shared", doc.Deleted.Scope)
	assert.Equal(t, "p", doc.Deleted.Name)
}

func TestPlansDelete_StaleConflictPreservesPlan(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "v1")
	_, err := svc.Update(t.Context(), plans.UpdateRequest{Ref: plans.SharedRef("p"), Content: "v2", ExpectedVersion: new(1)})
	require.NoError(t, err)

	_, stderr, err := executePlans(t, svc, "delete", "p", "--expected-version", "1", "--json")
	requirePlansStatusCode(t, err, plansConflictExitCode)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "conflict", body.Code)
	require.NotNil(t, body.CurrentVersion)
	assert.Equal(t, 2, *body.CurrentVersion)

	assert.Equal(t, "v2", mustGetPlan(t, svc, plans.SharedRef("p")).Content, "the conflicting delete must leave the plan in place")
}

func TestPlansDelete_SafetyRequiresGuardOrForce(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "body")

	// The CLI never prompts, so a bare delete is refused.
	_, _, err := executePlans(t, svc, "delete", "p")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one of the flags in the group [expected-version force] is required")

	// The guard and the deliberate opt-out are mutually exclusive.
	_, _, err = executePlans(t, svc, "delete", "p", "--expected-version", "1", "--force")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "none of the others can be")

	_, err = svc.Get(t.Context(), plans.SharedRef("p"))
	require.NoError(t, err, "the refused deletes must not remove the plan")
}

func TestPlansDelete_NotFound(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)

	_, stderr, err := executePlans(t, svc, "delete", "ghost", "--force", "--json")
	requirePlansStatusCode(t, err, 1)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "not_found", body.Code)
	assert.Equal(t, "ghost", body.Name)
}

// --- Cobra validation errors in JSON mode ------------------------------------------

// TestPlansValidation_JSONContract proves the validation cobra performs
// before RunE (missing required flags, violated flag groups, bad positional
// args, unknown flags) also honours the --json error contract: a single
// schema-versioned JSON object on stderr, nothing on stdout, exit code 1.
func TestPlansValidation_JSONContract(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)
	file := writePlanContentFile(t, "body")

	tests := []struct {
		name    string
		args    []string
		wantMsg string
	}{
		{"missing required --file", []string{"create", "p", "--json"}, `"file" not set`},
		{"missing required --output", []string{"export", "p", "--json"}, `"output" not set`},
		{"missing guard flag", []string{"delete", "p", "--json"}, "at least one of the flags in the group [expected-version force] is required"},
		{"mutually exclusive guard flags", []string{"update", "p", "--file", file, "--expected-version", "1", "--force", "--json"}, "none of the others can be"},
		{"too many args", []string{"get", "a", "b", "--json"}, "accepts at most 1 arg(s), received 2"},
		{"unexpected arg", []string{"list", "nope", "--json"}, `unknown command "nope"`},
		{"unknown flag", []string{"list", "--json", "--frobnicate"}, "unknown flag: --frobnicate"},
		{"invalid flag value", []string{"delete", "p", "--json", "--expected-version", "abc"}, `invalid argument "abc"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stdout, stderr, err := executePlans(t, svc, tt.args...)
			requirePlansStatusCode(t, err, 1)
			assert.Empty(t, stdout, "JSON mode must keep stdout free of prose")
			body := decodePlansError(t, stderr)
			assert.Equal(t, "invalid_argument", body.Code)
			assert.Contains(t, body.Message, tt.wantMsg)
			// decodePlansError already pins stderr to a single JSON line, so
			// cobra's own "Error: ..." rendering cannot have run as well.
			assert.NotContains(t, stderr, "Error:")
			assert.NotContains(t, stderr, "Usage:")
		})
	}
}

// TestPlansValidation_UnknownFlagBeforeJSONIsPlainText documents the one
// residual of the contract: flag parsing stops at the first unknown flag, so
// --json is only honoured when it was parsed before the failure.
func TestPlansValidation_UnknownFlagBeforeJSONIsPlainText(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)

	stdout, stderr, err := executePlans(t, svc, "list", "--frobnicate", "--json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown flag: --frobnicate")
	assert.Empty(t, stdout)
	assert.NotContains(t, stderr, "schema_version", "an unparsed --json cannot enable the JSON contract")
}

// TestPlansValidation_HumanModeKeepsCobraRendering proves validation errors
// without --json keep cobra's plain-text behaviour: the error is returned to
// the caller and rendered once, never as JSON.
func TestPlansValidation_HumanModeKeepsCobraRendering(t *testing.T) {
	t.Parallel()
	svc, _ := newPlansTestService(t)

	_, stderr, err := executePlans(t, svc, "get", "a", "b")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "accepts at most 1 arg(s), received 2")
	assert.NotContains(t, stderr, "schema_version")
	assert.LessOrEqual(t, strings.Count(stderr, "accepts at most"), 1, "the error must not be rendered twice")
}

// TestPlansValidation_JSONThroughRootCommand exercises the full production
// command tree (NewRootCmd, not a plans tree built directly by the test) to
// prove the interception is wired where real invocations run. Only failures
// raised before the root persistent pre-run are used, so the test touches no
// global state (no telemetry setup, no data-dir resolution).
func TestPlansValidation_JSONThroughRootCommand(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{"plans", "get", "a", "b", "--json"},
		{"plans", "list", "--json", "--frobnicate"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			cmd := NewRootCmd()
			var outBuf, errBuf bytes.Buffer
			cmd.SetOut(&outBuf)
			cmd.SetErr(&errBuf)
			cmd.SetIn(strings.NewReader(""))
			cmd.SetArgs(args)
			cmd.SetContext(t.Context())

			err := cmd.Execute()
			requirePlansStatusCode(t, err, 1)
			assert.Empty(t, outBuf.String())
			body := decodePlansError(t, errBuf.String())
			assert.Equal(t, "invalid_argument", body.Code)
		})
	}
}

// TestRootCommand_FlagErrorFuncStaysDefault guards the scoping of the plans
// flag-error interception: other commands keep cobra's default plain-text
// flag errors.
func TestRootCommand_FlagErrorFuncStaysDefault(t *testing.T) {
	t.Parallel()

	cmd := NewRootCmd()
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"version", "--frobnicate"})
	cmd.SetContext(t.Context())

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown flag: --frobnicate")
	assert.NotContains(t, errBuf.String(), "schema_version")
}

// --- PreRunE preservation ----------------------------------------------------------

// newHardenedPreRunECmd builds a plans-style subcommand (a --json flag, a
// required flag, its own PreRunE), hardened like the real subcommands. The
// production subcommands define no PreRunE today; these tests pin that
// hardenPlansValidation preserves one instead of silently replacing it.
func newHardenedPreRunECmd(preRunE func(*cobra.Command, []string) error, order *[]string) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "sub",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		PreRunE:      preRunE,
		RunE: func(*cobra.Command, []string) error {
			*order = append(*order, "run")
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "")
	cmd.Flags().String("file", "", "")
	_ = cmd.MarkFlagRequired("file")
	hardenPlansValidation(cmd)
	return cmd
}

func TestHardenPlansValidation_RunsExistingPreRunE(t *testing.T) {
	t.Parallel()

	var order []string
	cmd := newHardenedPreRunECmd(func(*cobra.Command, []string) error {
		order = append(order, "prerun")
		return nil
	}, &order)

	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--file", "x"})
	require.NoError(t, cmd.Execute())
	assert.Equal(t, []string{"prerun", "run"}, order, "the pre-existing PreRunE must run before RunE")
}

func TestHardenPlansValidation_PreRunEErrorShortCircuitsToJSON(t *testing.T) {
	t.Parallel()

	var order []string
	cmd := newHardenedPreRunECmd(func(*cobra.Command, []string) error {
		return errors.New("prerun boom")
	}, &order)

	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	// --file is deliberately missing: the PreRunE failure must win over the
	// required-flag check.
	cmd.SetArgs([]string{"--json"})

	err := cmd.Execute()
	requirePlansStatusCode(t, err, 1)
	assert.Empty(t, order, "RunE must not run after a failing PreRunE")
	assert.Empty(t, outBuf.String())
	body := decodePlansError(t, errBuf.String())
	assert.Equal(t, "invalid_argument", body.Code)
	assert.Contains(t, body.Message, "prerun boom")
	assert.NotContains(t, body.Message, "not set", "the PreRunE failure must short-circuit the required-flag check")
}

func TestHardenPlansValidation_PreRunEErrorHumanModePassesThrough(t *testing.T) {
	t.Parallel()

	var order []string
	cmd := newHardenedPreRunECmd(func(*cobra.Command, []string) error {
		return errors.New("prerun boom")
	}, &order)

	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{"--file", "x"})

	err := cmd.Execute()
	require.ErrorContains(t, err, "prerun boom")
	assert.Empty(t, order)
	assert.NotContains(t, errBuf.String(), "schema_version", "human mode must keep cobra's plain-text rendering")
}

// --- Telemetry sanitization ----------------------------------------------------------

// TestPlansTelemetryError_ReducesToStableCode pins the telemetry
// sanitization: the tracked error is exactly the stable machine-readable
// code of the JSON error contract, so plan names and filesystem paths
// embedded in error text never leave the machine.
func TestPlansTelemetryError_ReducesToStableCode(t *testing.T) {
	t.Parallel()

	require.NoError(t, plansTelemetryError(nil), "a success must stay untracked")

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"conflict", &plans.ConflictError{Name: "secret-plan", Expected: 1, Current: 2}, "conflict"},
		{"not found", &plans.NotFoundError{Scope: plans.ScopeShared, Name: "secret-plan"}, "not_found"},
		{"validation", &plans.ValidationError{Message: `invalid name "secret-plan"`}, "invalid_argument"},
		{"usage with path", plansUsagef("reading plan content from %q: boom", "/home/user/secret.md"), "invalid_argument"},
		{"storage with path", &plans.StorageError{Scope: plans.ScopeShared, Op: "list", Err: errors.New("open /home/user/.cagent/plans: denied")}, "storage"},
		{"unknown", errors.New("read /home/user/notes.md: i/o error"), "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := plansTelemetryError(tt.err)
			require.Error(t, got)
			assert.Equal(t, tt.want, got.Error(), "only the stable code may be tracked")
		})
	}
}

// --- Storage failures -------------------------------------------------------------

// failingPlanStorage is a plan.Storage whose every method fails, to verify
// backend failures surface through the CLI as the storage error code.
type failingPlanStorage struct{ err error }

var _ plan.Storage = failingPlanStorage{}

func (f failingPlanStorage) Get(context.Context, string) (plan.Plan, bool, error) {
	return plan.Plan{}, false, f.err
}

func (f failingPlanStorage) Upsert(context.Context, plan.UpsertRequest) (plan.Plan, error) {
	return plan.Plan{}, f.err
}

func (f failingPlanStorage) List(context.Context) ([]plan.Summary, []string, error) {
	return nil, nil, f.err
}

func (f failingPlanStorage) Delete(context.Context, string, *int) (bool, error) {
	return false, f.err
}

func TestPlansList_StorageErrorJSON(t *testing.T) {
	t.Parallel()
	svc := plans.NewService(failingPlanStorage{err: errors.New("backend boom")})

	stdout, stderr, err := executePlans(t, svc, "list", "--json")
	requirePlansStatusCode(t, err, 1)
	assert.Empty(t, stdout)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "storage", body.Code)
	assert.Equal(t, "shared", body.Scope)
	assert.Equal(t, "list", body.Op)
	assert.Contains(t, body.Message, "backend boom")
}

func TestPlansList_StorageErrorHuman(t *testing.T) {
	t.Parallel()
	svc := plans.NewService(failingPlanStorage{err: errors.New("backend boom")})

	stdout, stderr, err := executePlans(t, svc, "list")
	requirePlansStatusCode(t, err, 1)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "Error:")
	assert.Contains(t, stderr, "backend boom")
}
