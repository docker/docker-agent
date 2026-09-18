package root

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/docker/cli/cli"
	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
)

// plansSchemaVersion stamps every JSON document the plans commands emit
// (stdout results and stderr errors) so consumers can detect the format.
const plansSchemaVersion = "1"

// plansConflictExitCode is the exit code for version conflicts, distinct from
// the generic failure code 1 so scripts can branch on "re-read and retry".
const plansConflictExitCode = 3

// Stable machine-readable error codes of the --json stderr contract.
const (
	plansErrCodeConflict = "conflict"
	plansErrCodeNotFound = "not_found"
	plansErrCodeInvalid  = "invalid_argument"
	plansErrCodeCorrupt  = "corrupt"
	plansErrCodeStorage  = "storage"
	plansErrCodeUnknown  = "error"
)

// plansCmdOption customizes newPlansCmd; used by tests to inject the service.
type plansCmdOption func(*plansOptions)

type plansOptions struct {
	service plans.Service
}

// withPlansService injects the plans.Service every subcommand uses, so tests
// run hermetically against temp-backed storage instead of the user's data.
func withPlansService(svc plans.Service) plansCmdOption {
	return func(o *plansOptions) { o.service = svc }
}

// resolveService builds the production service. It must only run at RunE
// time: plan.SharedStorage() resolves the plans directory on first use, so
// touching it before the root persistent pre-run has applied --data-dir would
// capture (and memoize process-wide) the wrong default directory.
func (o *plansOptions) resolveService() plans.Service {
	if o.service != nil {
		return o.service
	}
	return plans.NewService(plan.SharedStorage())
}

func newPlansCmd(opts ...plansCmdOption) *cobra.Command {
	options := &plansOptions{}
	for _, opt := range opts {
		opt(options)
	}

	cmd := &cobra.Command{
		Use:   "plans",
		Short: "Manage shared plans",
		Long: `Manage the plans agents collaborate on, from the host: the named,
versioned documents of the plan toolset, collaborated on across sessions.

Mutations guard against concurrent edits: pass --expected-version <n> (the
version from a previous get or list) to fail with exit code 3 when the plan
changed in the meantime, or pass --force to deliberately write without the
guard. The CLI never prompts.

Every subcommand accepts --json for stable machine-readable output on stdout;
failures are then reported as a single JSON object on stderr.`,
		Example: `  docker-agent plans list
  docker-agent plans create release --file ./plan.md --title "Release plan"
  docker-agent plans get release
  docker-agent plans update release --file ./plan.md --expected-version 1
  docker-agent plans status release done --expected-version 2
  docker-agent plans export release --output ./plan.md
  docker-agent plans delete release --expected-version 3`,
		GroupID:      "advanced",
		SilenceUsage: true,
	}

	cmd.AddCommand(
		newPlansListCmd(options),
		newPlansGetCmd(options),
		newPlansCreateCmd(options),
		newPlansUpdateCmd(options),
		newPlansStatusCmd(options),
		newPlansExportCmd(options),
		newPlansDeleteCmd(options),
	)

	// Flag-parse failures (unknown flag, bad flag value) short-circuit before
	// RunE; route them through the --json error contract. Scoped to the plans
	// tree: subcommands inherit this func, the root command is untouched.
	cmd.SetFlagErrorFunc(plansValidationError)
	for _, sub := range cmd.Commands() {
		hardenPlansValidation(sub)
	}

	return cmd
}

// runPlans adapts a subcommand handler into a cobra RunE: it tracks
// telemetry, renders failures (human text, or the JSON error contract when
// --json is set), and maps them to process exit codes via cli.StatusError.
func (o *plansOptions) runPlans(sub string, jsonOut *bool, handler func(cmd *cobra.Command, svc plans.Service, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		// Telemetry carries the subcommand only: positional args are plan
		// names, and raw errors embed names and filesystem paths, so
		// failures are reduced to their stable code by plansTelemetryError.
		trackArgs := []string{sub}
		telemetry.TrackCommand(ctx, "plans", trackArgs)
		err := handler(cmd, o.resolveService(), args)
		telemetry.TrackCommandError(ctx, "plans", trackArgs, plansTelemetryError(err))
		if err == nil {
			return nil
		}
		// The failure is fully rendered here; silence cobra so stderr stays a
		// single message (a single JSON object in --json mode). Only the exit
		// code travels up: main honours cli.StatusError.
		cmd.SilenceErrors = true
		printPlansError(cmd.ErrOrStderr(), err, *jsonOut)
		return cli.StatusError{StatusCode: plansExitCode(err), Cause: err}
	}
}

func plansExitCode(err error) int {
	var conflict *plans.ConflictError
	if errors.As(err, &conflict) {
		return plansConflictExitCode
	}
	return 1
}

// plansUsageError marks CLI-level input mistakes (bad flag combinations,
// unreadable --file paths) so they map to the invalid_argument error code.
type plansUsageError struct{ msg string }

func (e *plansUsageError) Error() string { return e.msg }

func plansUsagef(format string, a ...any) error {
	return &plansUsageError{msg: fmt.Sprintf(format, a...)}
}

// plansJSONRequested reports whether the subcommand's --json flag has been
// parsed to true. Flags are parsed left-to-right and parsing stops at the
// first bad token, so during a flag-parse failure --json is only detected
// when it precedes the failing flag (a documented residual of the contract).
func plansJSONRequested(cmd *cobra.Command) bool {
	jsonOut, err := cmd.Flags().GetBool("json")
	return err == nil && jsonOut
}

// plansValidationError adapts a cobra-level validation failure (unknown
// flag, bad positional args, missing required flag, violated flag group) to
// the --json error contract. Without --json the error passes through for
// cobra's usual plain-text rendering; with it, the failure is printed as the
// single JSON error object and cobra's own printing is silenced so stderr
// carries no duplicate prose.
func plansValidationError(cmd *cobra.Command, err error) error {
	if err == nil || !plansJSONRequested(cmd) {
		return err
	}
	cmd.SilenceErrors = true
	usageErr := &plansUsageError{msg: err.Error()}
	printPlansError(cmd.ErrOrStderr(), usageErr, true)
	return cli.StatusError{StatusCode: plansExitCode(usageErr), Cause: usageErr}
}

// hardenPlansValidation re-renders the validations cobra runs before RunE
// through plansValidationError. Positional args are covered by wrapping the
// command's Args; required flags and flag groups are checked from PreRunE,
// immediately before cobra's own — then passing — checks, so nothing is
// validated twice with two different renderings. A PreRunE the command
// already had keeps running first — matching cobra's order, where PreRunE
// precedes the required/group checks — with its errors routed through the
// same rendering.
func hardenPlansValidation(cmd *cobra.Command) {
	if inner := cmd.Args; inner != nil {
		cmd.Args = func(cmd *cobra.Command, args []string) error {
			return plansValidationError(cmd, inner(cmd, args))
		}
	}
	innerPreRun := cmd.PreRunE
	cmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		if innerPreRun != nil {
			if err := innerPreRun(cmd, args); err != nil {
				return plansValidationError(cmd, err)
			}
		}
		if err := cmd.ValidateRequiredFlags(); err != nil {
			return plansValidationError(cmd, err)
		}
		return plansValidationError(cmd, cmd.ValidateFlagGroups())
	}
}

// plansErrorBody is the "error" object of the JSON stderr contract. Scope,
// name, op, and the conflict versions are included where the failure carries
// them.
type plansErrorBody struct {
	Code            string      `json:"code"`
	Message         string      `json:"message"`
	Scope           plans.Scope `json:"scope,omitempty"`
	Name            string      `json:"name,omitempty"`
	Op              string      `json:"op,omitempty"`
	ExpectedVersion *int        `json:"expected_version,omitempty"`
	CurrentVersion  *int        `json:"current_version,omitempty"`
}

type plansErrorDocument struct {
	SchemaVersion string         `json:"schema_version"`
	Error         plansErrorBody `json:"error"`
}

// plansErrorBodyFor classifies by the typed errors of pkg/plans, never by
// error text.
func plansErrorBodyFor(err error) plansErrorBody {
	var (
		conflict   *plans.ConflictError
		notFound   *plans.NotFoundError
		corrupt    *plans.CorruptError
		storageErr *plans.StorageError
		validation *plans.ValidationError
		usage      *plansUsageError
	)
	switch {
	case errors.As(err, &conflict):
		// Conflicts only exist for shared plans, which are the only versioned
		// scope.
		return plansErrorBody{
			Code:            plansErrCodeConflict,
			Message:         err.Error(),
			Scope:           plans.ScopeShared,
			Name:            conflict.Name,
			ExpectedVersion: new(conflict.Expected),
			CurrentVersion:  new(conflict.Current),
		}
	case errors.As(err, &notFound):
		return plansErrorBody{Code: plansErrCodeNotFound, Message: err.Error(), Scope: notFound.Scope, Name: notFound.Name}
	case errors.As(err, &corrupt):
		return plansErrorBody{Code: plansErrCodeCorrupt, Message: err.Error(), Scope: corrupt.Scope, Name: corrupt.Name}
	case errors.As(err, &storageErr):
		return plansErrorBody{Code: plansErrCodeStorage, Message: err.Error(), Scope: storageErr.Scope, Op: storageErr.Op}
	case errors.As(err, &validation), errors.As(err, &usage):
		return plansErrorBody{Code: plansErrCodeInvalid, Message: err.Error()}
	default:
		return plansErrorBody{Code: plansErrCodeUnknown, Message: err.Error()}
	}
}

// plansTelemetryError sanitizes a failure for telemetry by reducing it to
// its stable machine-readable code (plansErrorBodyFor): raw error text
// carries plan names and filesystem paths that must never be sent. Nil stays
// nil so a success is never tracked as an error.
func plansTelemetryError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(plansErrorBodyFor(err).Code)
}

func printPlansError(w io.Writer, err error, jsonOut bool) {
	if jsonOut {
		// Single-line JSON so stderr is one machine-detectable object.
		_ = json.NewEncoder(w).Encode(plansErrorDocument{SchemaVersion: plansSchemaVersion, Error: plansErrorBodyFor(err)})
		return
	}
	fmt.Fprintln(w, "Error:", err.Error())
}

// resolvePlanRef addresses the plan named by the positional argument.
func resolvePlanRef(name string) (plans.Ref, error) {
	if name == "" {
		return plans.Ref{}, plansUsagef("a plan name is required")
	}
	return plans.SharedRef(name), nil
}

// planGuardFlags implements the mutation write-guard: --expected-version
// enables optimistic locking, --force deliberately opts out of it. Exactly
// one must be given (enforced by cobra before RunE), so a nil expected
// version is never sent by accident.
type planGuardFlags struct {
	expectedVersion int
	force           bool
}

func (f *planGuardFlags) register(cmd *cobra.Command) {
	cmd.Flags().IntVar(&f.expectedVersion, "expected-version", 0, "Version the plan is expected to be at; the write fails with a version conflict (exit code 3) when it changed")
	cmd.Flags().BoolVar(&f.force, "force", false, "Write unconditionally, without the optimistic-lock guard")
	cmd.MarkFlagsOneRequired("expected-version", "force")
	cmd.MarkFlagsMutuallyExclusive("expected-version", "force")
}

// expected returns the guard to send to the service: the validated expected
// version, or nil for a deliberate --force.
func (f *planGuardFlags) expected() (*int, error) {
	if f.force {
		return nil, nil
	}
	if f.expectedVersion < 1 {
		return nil, plansUsagef("--expected-version must be at least 1, got %d", f.expectedVersion)
	}
	return &f.expectedVersion, nil
}

// readPlanContent loads the new plan body from path, or from stdin when path
// is "-" so scripts can pipe content in. Both sources are bounded by the plan
// storage's own content cap (plan.MaxPlanContentSize): content of exactly the
// cap is accepted, anything beyond it is refused as invalid input since the
// storage would refuse to persist it anyway. A regular file is required — a
// directory, device, or named pipe is rejected — and every check runs on the
// opened descriptor (File.Stat), never on the path, so the file that is
// validated is exactly the file that is read and a concurrent path swap
// cannot slip a non-regular file past the checks.
// The open itself cannot hang on a FIFO with no writer (see
// plan.OpenContentFile), and a device like /dev/zero is rejected before any
// read. The CLI never prompts: content only ever arrives through --file.
func readPlanContent(cmd *cobra.Command, path string) (string, error) {
	if path == "-" {
		content, err := readBoundedPlanContent(cmd.InOrStdin())
		if err != nil {
			return "", plansUsagef("reading plan content from stdin: %v", err)
		}
		return content, nil
	}

	f, err := plan.OpenContentFile(filepath.Clean(path))
	if err != nil {
		return "", plansUsagef("reading plan content from %q: %v", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", plansUsagef("reading plan content from %q: %v", path, err)
	}
	if info.IsDir() {
		return "", plansUsagef("plan content path %q is a directory, not a file", path)
	}
	if !info.Mode().IsRegular() {
		return "", plansUsagef("plan content path %q is not a regular file (e.g. a device or named pipe)", path)
	}
	if info.Size() > plan.MaxPlanContentSize {
		return "", plansUsagef("reading plan content from %q: content exceeds the maximum plan size (%d bytes)", path, plan.MaxPlanContentSize)
	}

	content, err := readBoundedPlanContent(f)
	if err != nil {
		return "", plansUsagef("reading plan content from %q: %v", path, err)
	}
	return content, nil
}

// readBoundedPlanContent reads r up to the shared plan content cap. It reads
// one byte past the cap so an over-cap source is detected without trusting
// any pre-declared size (stdin has none, and a file can grow between the
// descriptor size check and the read).
func readBoundedPlanContent(r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, plan.MaxPlanContentSize+1))
	if err != nil {
		return "", err
	}
	if len(data) > plan.MaxPlanContentSize {
		return "", fmt.Errorf("content exceeds the maximum plan size (%d bytes)", plan.MaxPlanContentSize)
	}
	return string(data), nil
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func formatPlanVersion(v *int) string {
	if v == nil {
		return "-"
	}
	return strconv.Itoa(*v)
}

func formatPlanTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func writePlansJSON(w io.Writer, doc any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

type plansListDocument struct {
	SchemaVersion string       `json:"schema_version"`
	Plans         []plans.Plan `json:"plans"`
	Warnings      []string     `json:"warnings,omitempty"`
}

type plansPlanDocument struct {
	SchemaVersion string     `json:"schema_version"`
	Plan          plans.Plan `json:"plan"`
}

type plansExportDocument struct {
	SchemaVersion string             `json:"schema_version"`
	Export        plans.ExportResult `json:"export"`
}

type plansDeletedDocument struct {
	SchemaVersion string       `json:"schema_version"`
	Deleted       plansRefBody `json:"deleted"`
}

type plansRefBody struct {
	Scope plans.Scope `json:"scope"`
	Name  string      `json:"name"`
}

func printPlanMutation(cmd *cobra.Command, jsonOut bool, p plans.Plan, verb string) error {
	if jsonOut {
		return writePlansJSON(cmd.OutOrStdout(), plansPlanDocument{SchemaVersion: plansSchemaVersion, Plan: p})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s %s plan %q (version %s)\n", verb, p.Scope, p.Name, formatPlanVersion(p.Version))
	return nil
}

// printPlanMetadata renders the concise one-line metadata of a plan. `get`
// sends it to stderr so stdout stays pure content while the metadata remains
// visible.
func printPlanMetadata(w io.Writer, p plans.Plan) {
	details := make([]string, 0, 4)
	if p.Title != "" {
		details = append(details, "title: "+p.Title)
	}
	if p.Status != "" {
		details = append(details, "status: "+p.Status)
	}
	details = append(details, "version: "+formatPlanVersion(p.Version))
	if !p.UpdatedAt.IsZero() {
		details = append(details, "updated: "+formatPlanTime(p.UpdatedAt))
	}
	fmt.Fprintf(w, "%s plan %q (%s)\n", p.Scope, p.Name, strings.Join(details, ", "))
}

func printPlansTable(w io.Writer, list []plans.Plan) {
	if len(list) == 0 {
		fmt.Fprintln(w, "No plans found.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 3, ' ', 0)
	fmt.Fprintln(tw, "SCOPE\tNAME\tSTATUS\tVERSION\tUPDATED\tTITLE")
	for _, p := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			p.Scope, p.Name, orDash(p.Status), formatPlanVersion(p.Version), formatPlanTime(p.UpdatedAt), orDash(p.Title))
	}
	tw.Flush()
}

func newPlansListCmd(o *plansOptions) *cobra.Command {
	var flags struct {
		json bool
	}

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List plans",
		Long: `List every shared plan with its metadata (content is not included).

Plans that exist but cannot be read are reported as warnings on stderr (in
the "warnings" field with --json) so they are never mistaken for missing.`,
		Args: cobra.NoArgs,
	}
	cmd.RunE = o.runPlans("list", &flags.json, func(cmd *cobra.Command, svc plans.Service, _ []string) error {
		result, err := svc.List(cmd.Context())
		if err != nil {
			return err
		}
		// Defensive against injected services: an empty listing must encode
		// as [] rather than null.
		if result.Plans == nil {
			result.Plans = []plans.Plan{}
		}
		if flags.json {
			return writePlansJSON(cmd.OutOrStdout(), plansListDocument{
				SchemaVersion: plansSchemaVersion,
				Plans:         result.Plans,
				Warnings:      result.Warnings,
			})
		}
		printPlansTable(cmd.OutOrStdout(), result.Plans)
		for _, warning := range result.Warnings {
			fmt.Fprintln(cmd.ErrOrStderr(), "Warning:", warning)
		}
		return nil
	})

	cmd.Flags().BoolVar(&flags.json, "json", false, "Output as JSON")

	return cmd
}

func newPlansGetCmd(o *plansOptions) *cobra.Command {
	var flags struct {
		json bool
	}

	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Print a plan's content and metadata",
		Long: `Print a plan: its content goes to stdout and a concise metadata line goes
to stderr, so redirecting stdout captures the content alone (use export for a
byte-exact file copy).`,
		Example: `  docker-agent plans get release
  docker-agent plans get release --json`,
		Args: cobra.MaximumNArgs(1),
	}
	cmd.RunE = o.runPlans("get", &flags.json, func(cmd *cobra.Command, svc plans.Service, args []string) error {
		ref, err := resolvePlanRef(firstArg(args))
		if err != nil {
			return err
		}
		p, err := svc.Get(cmd.Context(), ref)
		if err != nil {
			return err
		}
		if flags.json {
			return writePlansJSON(cmd.OutOrStdout(), plansPlanDocument{SchemaVersion: plansSchemaVersion, Plan: p})
		}
		printPlanMetadata(cmd.ErrOrStderr(), p)
		out := cmd.OutOrStdout()
		fmt.Fprint(out, p.Content)
		if !strings.HasSuffix(p.Content, "\n") {
			fmt.Fprintln(out)
		}
		return nil
	})

	cmd.Flags().BoolVar(&flags.json, "json", false, "Output as JSON")

	return cmd
}

func newPlansCreateCmd(o *plansOptions) *cobra.Command {
	var flags struct {
		json   bool
		file   string
		title  string
		author string
		status string
	}

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new shared plan",
		Long: `Create a new shared plan with content from --file (required; use "-" to
read stdin). The CLI never prompts for content.

Create is create-only: when a plan with the same name already exists the
command fails with a version conflict (exit code 3) instead of overwriting.`,
		Example: `  docker-agent plans create release --file ./plan.md --title "Release plan"
  cat plan.md | docker-agent plans create release --file -`,
		Args: cobra.ExactArgs(1),
	}
	cmd.RunE = o.runPlans("create", &flags.json, func(cmd *cobra.Command, svc plans.Service, args []string) error {
		content, err := readPlanContent(cmd, flags.file)
		if err != nil {
			return err
		}
		p, err := svc.Create(cmd.Context(), plans.CreateRequest{
			Ref:     plans.SharedRef(args[0]),
			Content: content,
			Title:   flags.title,
			Author:  flags.author,
			Status:  flags.status,
		})
		if err != nil {
			return err
		}
		return printPlanMutation(cmd, flags.json, p, "Created")
	})

	cmd.Flags().StringVar(&flags.file, "file", "", `File with the plan content ("-" reads stdin); required`)
	cmd.Flags().StringVar(&flags.title, "title", "", "Human-readable plan title")
	cmd.Flags().StringVar(&flags.author, "author", "", "Label identifying who wrote the plan")
	cmd.Flags().StringVar(&flags.status, "status", "", "Free-form lifecycle status (e.g. draft, in-progress)")
	cmd.Flags().BoolVar(&flags.json, "json", false, "Output as JSON")
	_ = cmd.MarkFlagRequired("file")

	return cmd
}

func newPlansUpdateCmd(o *plansOptions) *cobra.Command {
	var flags struct {
		json   bool
		file   string
		title  string
		author string
		status string
		guard  planGuardFlags
	}

	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Replace the content of an existing shared plan",
		Long: `Replace the content of an existing shared plan with the contents of --file
(required; use "-" to read stdin). Update never creates a plan.

Metadata flags that are omitted keep their previous value; passing them
(including an empty value) overwrites it.

Exactly one of --expected-version or --force must be given: the former fails
with exit code 3 when the plan changed since it was read, the latter
deliberately replaces it unconditionally.`,
		Example: `  docker-agent plans update release --file ./plan.md --expected-version 1
  docker-agent plans update release --file ./plan.md --force --status in-progress`,
		Args: cobra.MaximumNArgs(1),
	}
	cmd.RunE = o.runPlans("update", &flags.json, func(cmd *cobra.Command, svc plans.Service, args []string) error {
		ref, err := resolvePlanRef(firstArg(args))
		if err != nil {
			return err
		}
		expected, err := flags.guard.expected()
		if err != nil {
			return err
		}
		content, err := readPlanContent(cmd, flags.file)
		if err != nil {
			return err
		}
		req := plans.UpdateRequest{Ref: ref, Content: content, ExpectedVersion: expected}
		// Only changed metadata flags overwrite; omitted ones are preserved.
		if cmd.Flags().Changed("title") {
			req.Title = &flags.title
		}
		if cmd.Flags().Changed("author") {
			req.Author = &flags.author
		}
		if cmd.Flags().Changed("status") {
			req.Status = &flags.status
		}
		p, err := svc.Update(cmd.Context(), req)
		if err != nil {
			return err
		}
		return printPlanMutation(cmd, flags.json, p, "Updated")
	})

	flags.guard.register(cmd)
	cmd.Flags().StringVar(&flags.file, "file", "", `File with the new plan content ("-" reads stdin); required`)
	cmd.Flags().StringVar(&flags.title, "title", "", "New plan title (omit to preserve the current one)")
	cmd.Flags().StringVar(&flags.author, "author", "", "New author label (omit to preserve the current one)")
	cmd.Flags().StringVar(&flags.status, "status", "", "New lifecycle status (omit to preserve the current one)")
	cmd.Flags().BoolVar(&flags.json, "json", false, "Output as JSON")
	_ = cmd.MarkFlagRequired("file")

	return cmd
}

func newPlansStatusCmd(o *plansOptions) *cobra.Command {
	var flags struct {
		json  bool
		guard planGuardFlags
	}

	cmd := &cobra.Command{
		Use:   "status <name> <status>",
		Short: "Set the status of an existing shared plan",
		Long: `Set a shared plan's free-form status (e.g. "in-progress", "blocked",
"done") without touching its body. Setting the status is a write and bumps
the version.

Exactly one of --expected-version or --force must be given.`,
		Example: `  docker-agent plans status release done --expected-version 2
  docker-agent plans status release blocked --force`,
		Args: cobra.RangeArgs(1, 2),
	}
	cmd.RunE = o.runPlans("status", &flags.json, func(cmd *cobra.Command, svc plans.Service, args []string) error {
		if len(args) != 2 {
			return plansUsagef("status takes a name and a status: plans status <name> <status>")
		}
		name, status := args[0], args[1]
		ref, err := resolvePlanRef(name)
		if err != nil {
			return err
		}
		expected, err := flags.guard.expected()
		if err != nil {
			return err
		}
		p, err := svc.SetStatus(cmd.Context(), plans.SetStatusRequest{Ref: ref, Status: status, ExpectedVersion: expected})
		if err != nil {
			return err
		}
		if flags.json {
			return writePlansJSON(cmd.OutOrStdout(), plansPlanDocument{SchemaVersion: plansSchemaVersion, Plan: p})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Set status of %s plan %q to %q (version %s)\n",
			p.Scope, p.Name, p.Status, formatPlanVersion(p.Version))
		return nil
	})

	flags.guard.register(cmd)
	cmd.Flags().BoolVar(&flags.json, "json", false, "Output as JSON")

	return cmd
}

func newPlansExportCmd(o *plansOptions) *cobra.Command {
	var flags struct {
		json   bool
		output string
		force  bool
	}

	cmd := &cobra.Command{
		Use:   "export <name>",
		Short: "Write a plan's content to a file",
		Long: `Write a plan's content, byte-exact, to --output (required). Parent
directories are created and the write is atomic, so a reader never observes
a partial export. An existing destination is refused and left untouched
unless --force is given, which replaces an existing regular file atomically.`,
		Example: `  docker-agent plans export release --output ./plan.md
  docker-agent plans export release --output ./plan.md --force`,
		Args: cobra.MaximumNArgs(1),
	}
	cmd.RunE = o.runPlans("export", &flags.json, func(cmd *cobra.Command, svc plans.Service, args []string) error {
		ref, err := resolvePlanRef(firstArg(args))
		if err != nil {
			return err
		}
		result, err := svc.Export(cmd.Context(), plans.ExportRequest{Ref: ref, Path: flags.output, Force: flags.force})
		if err != nil {
			return err
		}
		if flags.json {
			return writePlansJSON(cmd.OutOrStdout(), plansExportDocument{SchemaVersion: plansSchemaVersion, Export: result})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Exported %s plan %q (version %s) to %s (%d bytes)\n",
			result.Scope, result.Name, formatPlanVersion(result.Version), result.Path, result.BytesWritten)
		return nil
	})

	cmd.Flags().StringVar(&flags.output, "output", "", "Destination file for the plan content; required")
	cmd.Flags().BoolVar(&flags.force, "force", false, "Replace the destination file when it already exists")
	cmd.Flags().BoolVar(&flags.json, "json", false, "Output as JSON")
	_ = cmd.MarkFlagRequired("output")

	return cmd
}

func newPlansDeleteCmd(o *plansOptions) *cobra.Command {
	var flags struct {
		json  bool
		guard planGuardFlags
	}

	cmd := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a shared plan",
		Long: `Delete a shared plan. The CLI never prompts, so exactly one of
--expected-version or --force must be given: the former fails with exit
code 3 when the plan changed since it was read (leaving it in place), the
latter deletes unconditionally — which is also how a corrupt plan is
recovered.`,
		Example: `  docker-agent plans delete release --expected-version 3
  docker-agent plans delete release --force`,
		Args: cobra.MaximumNArgs(1),
	}
	cmd.RunE = o.runPlans("delete", &flags.json, func(cmd *cobra.Command, svc plans.Service, args []string) error {
		ref, err := resolvePlanRef(firstArg(args))
		if err != nil {
			return err
		}
		expected, err := flags.guard.expected()
		if err != nil {
			return err
		}
		if err := svc.Delete(cmd.Context(), plans.DeleteRequest{Ref: ref, ExpectedVersion: expected}); err != nil {
			return err
		}
		if flags.json {
			return writePlansJSON(cmd.OutOrStdout(), plansDeletedDocument{
				SchemaVersion: plansSchemaVersion,
				Deleted:       plansRefBody{Scope: ref.Scope, Name: ref.Name},
			})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Deleted %s plan %q\n", ref.Scope, ref.Name)
		return nil
	})

	flags.guard.register(cmd)
	cmd.Flags().BoolVar(&flags.json, "json", false, "Output as JSON")

	return cmd
}
