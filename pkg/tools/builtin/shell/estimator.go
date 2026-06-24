package shell

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/tools"
)

// estimateKind is the autonomous estimator's top-level verdict for a
// shell command. It drives how ValidateShellToolCall routes the call:
//
//   - estimateReadOnly: every segment is confidently read-only (no
//     destructive operation, no overwrite redirect, no piped-to-
//     interpreter, no command substitution). Safer mode does NOT force a
//     confirmation for these — the call goes through the normal approval
//     flow like any other tool.
//   - estimateDestructive: at least one segment is a recognized
//     destructive operation whose scope the estimator fully resolved
//     (literal paths, no unresolved variables/substitution). The Level
//     is trusted directly; no LLM round-trip is needed.
//   - estimateUncertain: the command is (or contains) something the
//     estimator could not fully resolve — an unresolved variable or
//     command substitution in a destructive target, an xargs-fed target,
//     a piped-to-interpreter segment, or an unrecognized program. These
//     fall through to the residual LLM judge (when wired) and then to a
//     tier-enriched, fail-closed confirmation. Level carries the best
//     structural guess and may be empty/Unknown.
type estimateKind int

const (
	estimateUncertain estimateKind = iota
	estimateReadOnly
	estimateDestructive
)

// blastEstimate is the result of estimateBlastRadius.
type blastEstimate struct {
	kind   estimateKind
	level  tools.BlastRadiusLevel
	reason string
}

// Bounds for the read-only filesystem probe. The probe runs inside the
// safety validator (before the user is asked), so it must stay cheap and
// must never follow symlinks or recurse without bound.
const (
	probeMaxFiles  = 5000 // stop counting a recursive target after this many entries
	probeMaxDepth  = 8    // do not descend deeper than this when counting
	probeManyFiles = 200  // a recursive/glob target above this is treated as "broad"
	maxScriptDepth = 3    // recursion cap for `sh -c "<script>"` unpacking
)

// Score thresholds mapping the additive danger score to a blast-radius
// tier. Calibrated to roughly agree with safety_patterns.json on the
// commands both cover (the estimator only runs on pattern misses, so
// exact agreement is not required, only sanity).
func tierFromScore(score int) tools.BlastRadiusLevel {
	switch {
	case score >= 4:
		return tools.BlastRadiusHigh
	case score >= 2:
		return tools.BlastRadiusMedium
	default:
		return tools.BlastRadiusLow
	}
}

// estimateBlastRadius autonomously estimates the blast radius of a shell
// command from its structure and, when workdir is non-empty, from a
// bounded read-only probe of the filesystem it would touch. It is the
// hybrid estimator's deterministic core: the residual LLM judge is only
// consulted (by the caller) when this returns estimateUncertain.
//
// workdir is the resolved working directory the command will run in; it
// anchors relative paths and bounds the filesystem probe. An empty
// workdir disables probing and path-escape detection but keeps the
// purely structural analysis (flags, literal absolute paths, operations).
func estimateBlastRadius(command, workdir string) blastEstimate {
	return estimateWithDepth(command, workdir, 0)
}

func estimateWithDepth(command, workdir string, depth int) blastEstimate {
	segments, dynamic := lexCommand(command)
	if len(segments) == 0 {
		if dynamic {
			// A command that is nothing but command substitution(s)
			// (e.g. `` `blkdiscard /dev/sdb` ``) produces no segments but
			// still runs unclassified code. Never treat it as a read-only
			// no-op: route it through the fail-closed uncertain path.
			return blastEstimate{
				kind:   estimateUncertain,
				reason: "command is a bare command substitution; contents unresolved",
			}
		}
		// Empty or whitespace-only command: nothing to do. Treat as
		// read-only so safer mode does not gate a no-op.
		return blastEstimate{kind: estimateReadOnly}
	}

	var (
		anyDestructive bool
		anyUncertain   bool
		anyNonReadOnly bool
		maxScore       int
		topReason      string
	)

	for _, seg := range segments {
		res := classifySegment(seg, workdir, depth)
		switch res.kind {
		case estimateReadOnly:
			// contributes nothing
		case estimateDestructive:
			anyDestructive = true
			anyNonReadOnly = true
		case estimateUncertain:
			anyUncertain = true
			anyNonReadOnly = true
		}
		if res.score > maxScore {
			maxScore = res.score
			topReason = res.reason
		} else if topReason == "" && res.reason != "" {
			topReason = res.reason
		}
	}

	// A command substitution ($(...) or backticks) anywhere means we
	// cannot fully reason about what runs (it may feed a destructive
	// target or be a destructive command itself). Never treat such a
	// command as confidently read-only OR confidently destructive — route
	// it through the uncertain path (judge + fail-closed gate), carrying
	// whatever tier the resolved parts suggest.
	if dynamic {
		anyUncertain = true
		anyNonReadOnly = true
	}

	switch {
	case !anyNonReadOnly && !dynamic:
		return blastEstimate{kind: estimateReadOnly}
	case anyUncertain:
		level := tools.BlastRadiusLevel("")
		if anyDestructive || maxScore > 0 {
			level = tierFromScore(maxScore)
		}
		reason := topReason
		if reason == "" {
			reason = "Command could not be fully resolved; safer-mode confirmation required."
		}
		return blastEstimate{kind: estimateUncertain, level: level, reason: reason}
	default: // anyDestructive, fully resolved
		return blastEstimate{
			kind:   estimateDestructive,
			level:  tierFromScore(maxScore),
			reason: topReason,
		}
	}
}

// segResult is the per-segment classification.
type segResult struct {
	kind   estimateKind
	score  int
	reason string
}

// classifySegment classifies one simple command (a single segment of a
// pipeline / list) into read-only, destructive, or uncertain, computing
// an additive danger score for the destructive/uncertain cases.
//
// The program-driven verdict and the redirect-driven verdict are combined
// with worst(): even a read-only program (`cat a > b`) is destructive
// because of its overwrite redirect.
func classifySegment(seg simpleCommand, workdir string, depth int) segResult {
	base := classifyProgram(seg, workdir, depth)
	red := redResult(classifyRedirects(seg.redirects, workdir))
	return worst(base, red)
}

// commandValuedFlags are long-flag names whose value a program executes as
// a command. They turn an otherwise read-only program into arbitrary code
// execution (ripgrep --pre, sort --compress-program, git --upload-pack,
// git grep --open-files-in-pager, bat --pager, ...). Matched by exact name
// against any program's long flags, so the whole class is gated in one place.
var commandValuedFlags = map[string]bool{
	"pre":                  true, // ripgrep preprocessor command
	"hostname-bin":         true, // ripgrep hostname program (run eagerly)
	"compress-program":     true, // sort / GNU coreutils
	"use-compress-program": true, // tar
	"upload-pack":          true, // git
	"uploadpack":           true,
	"receive-pack":         true, // git
	"receivepack":          true,
	"exec":                 true, // git (--upload-pack synonym), others
	"open-files-in-pager":  true, // git grep
	"pager":                true, // bat, ...
	"editor":               true,
	"rsh":                  true, // rsync / cvs remote shell
	"ssh-command":          true,
	"to-command":           true, // git send-email
	"filter-process":       true, // git
}

// hasCommandValuedFlag reports whether args carry a long flag whose value
// names a command to execute (see commandValuedFlags).
func hasCommandValuedFlag(args []string) bool {
	for _, a := range args {
		name, ok := strings.CutPrefix(a, "--")
		if !ok {
			continue
		}
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
		}
		if commandValuedFlags[strings.ToLower(name)] {
			return true
		}
	}
	return false
}

// safeLeadingEnvVars are environment variables whose assignment cannot
// cause code execution (locale/timezone). A leading assignment to any
// other variable blocks the read-only fast path.
var safeLeadingEnvVars = map[string]bool{
	"lang": true, "language": true, "lc_all": true, "lc_ctype": true,
	"lc_collate": true, "lc_messages": true, "lc_numeric": true,
	"lc_time": true, "lc_monetary": true, "lc_paper": true,
	"lc_name": true, "lc_address": true, "lc_telephone": true,
	"lc_measurement": true, "lc_identification": true, "tz": true,
}

// classifyProgram is the program-driven half of classifySegment
// (redirections are handled separately by the caller). It refuses a
// read-only verdict when an environment assignment (leading or carried by
// an env/sudo wrapper) could inject code (LD_PRELOAD, BASH_ENV, GIT_SSH,
// ...); locale-only prefixes (LANG/LC_*/TZ) stay read-only.
func classifyProgram(seg simpleCommand, workdir string, depth int) segResult {
	prog, args, stdinFed, envAssigns := effectiveProgram(seg.words)
	res := classifyProgramCore(seg, prog, args, stdinFed, workdir, depth)
	if res.kind == estimateReadOnly && hasUnsafeAssignment(envAssigns) {
		return segResult{kind: estimateUncertain, reason: "environment assignment can alter the command (e.g. LD_PRELOAD)"}
	}
	return res
}

// classifyProgramCore is the program classification before the
// environment-assignment safety check applied by classifyProgram.
func classifyProgramCore(seg simpleCommand, prog string, args []string, stdinFed bool, workdir string, depth int) segResult {
	if prog == "" {
		if len(seg.words) > 0 {
			// A wrapper consumed every token without leaving a program —
			// e.g. `env -S'blkdiscard /dev/sda'` (--split-string smuggles a
			// whole command into one token) or a wrapper with an embedded
			// command flag. We cannot tell what runs; gate it (fail-closed).
			return segResult{kind: estimateUncertain, reason: "wrapped command could not be resolved"}
		}
		// Pure redirection or genuinely empty segment: no program-driven danger.
		return segResult{kind: estimateReadOnly}
	}

	// Interpreter reading from a pipe (`curl ... | sh`) executes code we
	// did not classify; treat as the highest concern.
	if seg.stdinFromPipe && isInterpreter(prog) && !interpreterHasScriptArg(prog, args) {
		return segResult{
			kind:   estimateUncertain,
			score:  4,
			reason: "pipes into a shell interpreter (" + prog + "); executes unclassified code",
		}
	}
	if isInterpreter(prog) {
		// Only a SHELL interpreter's -c body is shell we can re-analyse.
		// A python/ruby/perl/node -c/-e body is that language, not shell —
		// re-lexing it as shell would, e.g., read
		// `python3 -c 'true and __import__("shutil").rmtree("/")'` as the
		// read-only program `true`. Gate non-shell inline scripts.
		if isShellInterpreter(prog) {
			if script, ok := interpreterInlineScript(prog, args); ok && depth < maxScriptDepth {
				return fromInnerEstimate(prog, estimateWithDepth(script, workdir, depth+1))
			}
		}
		// Interpreter running a script file, a non-shell inline script, or
		// interactively: contents unknown -> uncertain (fail-closed).
		return segResult{kind: estimateUncertain, reason: prog + " runs an unclassified script"}
	}

	// Recognized destructive operation?
	if op, ok := lookupOp(prog); ok {
		res := op(prog, args, seg, workdir)
		if stdinFed && res.kind == estimateDestructive {
			// Targets are fed from stdin (xargs): the real scope is
			// whatever the upstream command emits, which we cannot see.
			res.kind = estimateUncertain
			if res.score < 3 {
				res.score = 3
			}
			res.reason = "targets read from stdin (xargs), scope unknown — " + res.reason
		}
		return res
	}

	// Read-only program (overwrite redirects are handled by the caller).
	if isReadOnlyProgram(prog, args) {
		return segResult{kind: estimateReadOnly}
	}

	// Genuinely unrecognized program -> uncertain with no tentative tier
	// (fail-closed; matches prior safer-mode behaviour of gating unknowns).
	return segResult{kind: estimateUncertain}
}

// lookupOp resolves a program name to its destructive handler, including
// the mkfs.<fs> family (mkfs.ext4, mkfs.xfs, ...).
func lookupOp(prog string) (opFunc, bool) {
	if op, ok := destructiveOps[prog]; ok {
		return op, true
	}
	if strings.HasPrefix(prog, "mkfs.") {
		return opFormat, true
	}
	return nil, false
}

// worst returns the more severe of two segment results, preferring
// destructive>uncertain>readonly and the higher score.
func worst(a, b segResult) segResult {
	rank := func(k estimateKind) int {
		switch k {
		case estimateDestructive:
			return 2
		case estimateUncertain:
			return 1
		default:
			return 0
		}
	}
	out := a
	if rank(b.kind) > rank(out.kind) {
		out.kind = b.kind
	}
	if b.score > out.score {
		out.score = b.score
		out.reason = b.reason
	}
	if out.reason == "" {
		out.reason = b.reason
	}
	return out
}

func redResult(score int, reason string, resolved bool) segResult {
	if score == 0 {
		return segResult{kind: estimateReadOnly}
	}
	kind := estimateDestructive
	if !resolved {
		kind = estimateUncertain
	}
	return segResult{kind: kind, score: score, reason: reason}
}

// fromInnerEstimate maps the recursive estimate of an inline `-c` script
// back onto the interpreter segment.
func fromInnerEstimate(prog string, inner blastEstimate) segResult {
	switch inner.kind {
	case estimateReadOnly:
		return segResult{kind: estimateReadOnly}
	case estimateDestructive:
		return segResult{
			kind: estimateDestructive, score: scoreFromTier(inner.level),
			reason: prog + " -c runs: " + inner.reason,
		}
	default:
		return segResult{
			kind: estimateUncertain, score: scoreFromTier(inner.level),
			reason: prog + " -c runs an unresolved command.",
		}
	}
}

func scoreFromTier(level tools.BlastRadiusLevel) int {
	switch level {
	case tools.BlastRadiusHigh:
		return 4
	case tools.BlastRadiusMedium:
		return 2
	case tools.BlastRadiusLow:
		return 1
	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// Destructive operation handlers
// ---------------------------------------------------------------------------

type opFunc func(prog string, args []string, seg simpleCommand, workdir string) segResult

var destructiveOps = map[string]opFunc{
	"rm":         opRm,
	"rmdir":      opRmdir,
	"shred":      opShred,
	"dd":         opDd,
	"mkfs":       opFormat,
	"wipefs":     opFormat,
	"fdisk":      opFormat,
	"parted":     opFormat,
	"sgdisk":     opFormat,
	"blkdiscard": opFormat,
	"truncate":   opTruncate,
	"mv":         opMoveCopy,
	"cp":         opMoveCopy,
	"rsync":      opRsync,
	"chmod":      opPerm,
	"chown":      opPerm,
	"chgrp":      opPerm,
	"find":       opFind,
	"tee":        opTee,
	"git":        opGit,
	"docker":     opDocker,
}

// mkfs.<fs> variants (mkfs.ext4, mkfs.xfs, ...) share opFormat; handled in
// isDestructiveProgram via prefix match, see classifySegment lookup below.

func opRm(_ string, args []string, _ simpleCommand, workdir string) segResult {
	recursive := hasAnyFlag(args, "r", "R", "recursive")
	noPreserveRoot := hasLongFlag(args, "no-preserve-root")
	targets := positionalArgs(args)

	score := 1
	if recursive {
		score++
	}
	reason := "rm deletes files"
	if recursive {
		reason = "rm -r recursively deletes a directory tree"
	}

	pScore, presolved, ploc := worstPathScope(targets, workdir)
	score += pScore
	if noPreserveRoot {
		score += 4
	}
	if ploc != "" {
		reason += " (" + ploc + ")"
	}

	// Filesystem probe sharpens recursive/glob deletes.
	bScore, bnote, bresolved := breadthOfTargets(targets, recursive, workdir)
	score += bScore
	if bnote != "" {
		reason += "; " + bnote
	}

	resolved := presolved && bresolved && !hasUnresolvedTarget(targets)
	return scopedResult(score, reason, resolved)
}

func opRmdir(_ string, args []string, _ simpleCommand, workdir string) segResult {
	// rmdir only removes empty directories.
	targets := positionalArgs(args)
	pScore, presolved, ploc := worstPathScope(targets, workdir)
	score := 1 + pScore
	reason := "rmdir removes an (empty) directory"
	if ploc != "" {
		reason += " (" + ploc + ")"
	}
	return scopedResult(score, reason, presolved && !hasUnresolvedTarget(targets))
}

func opShred(_ string, args []string, _ simpleCommand, workdir string) segResult {
	targets := positionalArgs(args)
	pScore, presolved, _ := worstPathScope(targets, workdir)
	// shred is irreversible by design.
	return scopedResult(3+pScore, "shred irreversibly overwrites file contents", presolved && !hasUnresolvedTarget(targets))
}

func opDd(_ string, args []string, _ simpleCommand, workdir string) segResult {
	of := ddOperand(args, "of")
	if of == "" {
		// dd without an output operand only reads.
		return segResult{kind: estimateReadOnly}
	}
	score := 2
	reason := "dd overwrites " + of
	if isBlockDevice(of) {
		score = 5
		reason = "dd writes directly to block device " + of + " (wipes it)"
	} else {
		pScore, _, ploc := worstPathScope([]string{of}, workdir)
		score += pScore
		if ploc != "" {
			reason += " (" + ploc + ")"
		}
	}
	return scopedResult(score, reason, !strings.Contains(of, "$"))
}

func opFormat(prog string, args []string, _ simpleCommand, workdir string) segResult {
	targets := positionalArgs(args)
	pScore, _, _ := worstPathScope(targets, workdir)
	return scopedResult(4+pScore, prog+" formats/erases a storage device", !hasUnresolvedTarget(targets))
}

func opTruncate(_ string, args []string, _ simpleCommand, workdir string) segResult {
	size := flagValue(args, "s", "size")
	targets := positionalArgs(args)
	pScore, presolved, ploc := worstPathScope(targets, workdir)
	score := 2 + pScore
	reason := "truncate resizes a file"
	if size == "0" {
		reason = "truncate -s 0 empties a file"
	}
	if ploc != "" {
		reason += " (" + ploc + ")"
	}
	return scopedResult(score, reason, presolved && !hasUnresolvedTarget(targets))
}

func opMoveCopy(prog string, args []string, _ simpleCommand, workdir string) segResult {
	recursive := hasAnyFlag(args, "r", "R", "recursive", "a", "archive")
	targets := positionalArgs(args)

	// The destination (last positional) is what may be overwritten.
	dst := ""
	if len(targets) > 0 {
		dst = targets[len(targets)-1]
	}
	score := 1
	if recursive {
		score++
	}
	reason := prog + " can overwrite the destination"
	if recursive {
		reason = prog + " -r can overwrite a destination tree"
	}

	pScore, presolved, ploc := worstPathScope([]string{dst}, workdir)
	score += pScore
	if ploc != "" {
		reason += " (" + ploc + ")"
	}
	// A move/copy whose destination does not exist overwrites nothing.
	if abs := resolvePath(dst, workdir); probeableInWorkdir(abs, workdir) {
		if exists, _ := probeExists(abs); !exists {
			score--
			reason = prog + " writes to a new path"
		}
	}
	return scopedResult(score, reason, presolved && !hasUnresolvedTarget(targets))
}

func opRsync(_ string, args []string, _ simpleCommand, workdir string) segResult {
	del := hasLongFlag(args, "delete") || hasLongFlag(args, "delete-after") || hasLongFlag(args, "delete-before") || hasLongFlag(args, "del")
	targets := positionalArgs(args)
	dst := ""
	if len(targets) > 0 {
		dst = targets[len(targets)-1]
	}
	score := 1
	reason := "rsync overwrites files in the destination"
	if del {
		score = 3
		reason = "rsync --delete removes destination files missing from the source"
	}
	pScore, _, ploc := worstPathScope([]string{dst}, workdir)
	score += pScore
	if ploc != "" {
		reason += " (" + ploc + ")"
	}
	return scopedResult(score, reason, !hasUnresolvedTarget(targets))
}

func opPerm(prog string, args []string, _ simpleCommand, workdir string) segResult {
	recursive := hasAnyFlag(args, "R", "recursive")
	targets := positionalArgs(args)
	score := 0
	reason := prog + " changes file metadata (reversible)"
	if recursive {
		score = 2
		reason = prog + " -R recursively changes file metadata"
	}
	pScore, presolved, ploc := worstPathScope(targets, workdir)
	score += pScore
	if ploc != "" {
		reason += " (" + ploc + ")"
	}
	if score == 0 {
		// A single non-recursive chmod/chown inside the workdir is low
		// blast radius but not nothing: chown to another user is not
		// reversible without root, and chmod 000 can lock out a key/config.
		score = 1
	}
	return scopedResult(score, reason, presolved && !hasUnresolvedTarget(targets))
}

func opFind(_ string, args []string, _ simpleCommand, workdir string) segResult {
	if !findHasDestructiveAction(args) {
		// Pure search: read-only.
		return segResult{kind: estimateReadOnly}
	}
	// find ... -delete / -exec rm ... walks a tree and acts on each match.
	roots := findRoots(args)
	pScore, presolved, ploc := worstPathScope(roots, workdir)
	score := 3 + pScore
	reason := "find performs a recursive delete/exec over a tree"
	if ploc != "" {
		reason += " (" + ploc + ")"
	}
	bScore, bnote, bresolved := breadthOfTargets(roots, true, workdir)
	score += bScore
	if bnote != "" {
		reason += "; " + bnote
	}
	return scopedResult(score, reason, presolved && bresolved && !hasUnresolvedTarget(roots))
}

func opTee(_ string, args []string, _ simpleCommand, workdir string) segResult {
	targets := positionalArgs(args)
	if len(targets) == 0 {
		return segResult{kind: estimateReadOnly}
	}
	append_ := hasAnyFlag(args, "a", "append")
	pScore, presolved, ploc := worstPathScope(targets, workdir)
	score := 1 + pScore
	reason := "tee writes to file(s)"
	if !append_ {
		score++
		reason = "tee overwrites file(s)"
	}
	if ploc != "" {
		reason += " (" + ploc + ")"
	}
	return scopedResult(score, reason, presolved && !hasUnresolvedTarget(targets))
}

func opGit(_ string, rawArgs []string, _ simpleCommand, workdir string) segResult {
	// `git -c key=value` / `--config-env=key=ENV` inject config for this one
	// invocation; an executable-valued key (core.sshCommand, core.pager,
	// alias.*, *.external, *.cmd, ...) runs arbitrary code even on an
	// otherwise read-only subcommand (e.g. ls-remote, fetch). Gate it.
	if gitInjectsExecConfig(rawArgs) {
		return scopedResult(4, "git -c injects an executable config key (runs arbitrary code)", true)
	}
	// CLI options whose value git runs as a command against a (possibly
	// local) remote: --upload-pack / --receive-pack / --exec / --to-command.
	if hasCommandValuedFlag(rawArgs) {
		return scopedResult(4, "git runs a command named by a CLI flag (--upload-pack/--receive-pack/--exec)", true)
	}
	// `--output=<file>` (diff/show/log/...) writes a file; gate when it
	// lands outside the working directory or on a system path.
	if out := flagValue(rawArgs, "", "output"); out != "" {
		if s, _, loc := worstPathScope([]string{out}, workdir); s >= 2 {
			reason := "git --output writes to " + out
			if loc != "" {
				reason += " (" + loc + ")"
			}
			return scopedResult(s, reason, true)
		}
	}

	// Strip git's global options (-C <dir>, -c k=v, --git-dir=..., ...) so
	// the real subcommand is identified even behind `git -C /repo ...`.
	args := stripGitGlobalOpts(rawArgs)
	sub := firstPositional(args)
	switch sub {
	case "status", "log", "show", "diff",
		"describe", "blame", "ls-files", "ls-remote", "rev-parse",
		"shortlog", "cat-file", "whatchanged", "version", "":
		return segResult{kind: estimateReadOnly}
	case "grep":
		// `git grep -O[<pager>]` / --open-files-in-pager runs the pager
		// program on matches (the long form is caught above by
		// hasCommandValuedFlag; -O is short).
		if hasShortClusterFlag(args, 'O') {
			return scopedResult(4, "git grep -O runs the pager program on matched files", true)
		}
		return segResult{kind: estimateReadOnly}
	case "branch":
		if hasAnyFlag(args, "D", "d", "delete") {
			return scopedResult(2, "git branch delete removes a ref", true)
		}
		return segResult{kind: estimateReadOnly}
	case "tag":
		if hasAnyFlag(args, "d", "delete") {
			return scopedResult(2, "git tag delete removes a ref", true)
		}
		return segResult{kind: estimateReadOnly}
	case "fetch":
		// A fetch that writes local refs (a force "+" refspec, or one whose
		// destination is refs/heads/) can clobber local branches; a plain
		// fetch (or one targeting refs/remotes/) is read-only.
		if fetchWritesLocalRefs(args) {
			return scopedResult(1, "git fetch with a force/branch-writing refspec can move local refs", true)
		}
		return segResult{kind: estimateReadOnly}
	case "config":
		// A config write can enable arbitrary command execution on the
		// next git operation (core.sshCommand, core.pager, alias.*=!cmd).
		// Only treat pure reads as read-only.
		if gitConfigIsRead(args) {
			return segResult{kind: estimateReadOnly}
		}
		return scopedResult(3, "git config write can set executable hooks (core.sshCommand, pager, aliases)", true)
	case "remote":
		// add / set-url / rename / remove redirect where the repo fetches
		// and pushes (supply-chain / exfiltration); reads are safe.
		switch nthPositional(args, 1) {
		case "", "-v", "show", "get-url":
			return segResult{kind: estimateReadOnly}
		default:
			return scopedResult(3, "git remote change redirects the repository's origin", true)
		}
	case "reflog":
		if v := nthPositional(args, 1); v == "expire" || v == "delete" {
			return scopedResult(3, "git reflog "+v+" purges the recovery log that makes resets reversible", true)
		}
		return segResult{kind: estimateReadOnly}
	case "reset":
		if hasLongFlag(args, "hard") {
			return scopedResult(3, "git reset --hard discards uncommitted changes", true)
		}
		return segResult{kind: estimateReadOnly}
	case "clean":
		if hasAnyFlag(args, "f", "force") {
			return scopedResult(3, "git clean removes untracked files", true)
		}
		return segResult{kind: estimateReadOnly}
	case "checkout", "restore", "switch":
		// Can discard local changes; medium and recoverable-ish.
		return scopedResult(2, "git "+sub+" can overwrite local changes", true)
	case "push":
		if hasLongFlag(args, "force") || hasShortClusterFlag(args, 'f') {
			if hasLongFlag(args, "force-with-lease") {
				return scopedResult(2, "git push --force-with-lease rewrites remote history (lease-guarded)", true)
			}
			return scopedResult(4, "git push --force rewrites remote history", true)
		}
		if hasLongFlag(args, "mirror") {
			return scopedResult(4, "git push --mirror deletes every remote ref absent locally", true)
		}
		if hasLongFlag(args, "prune") {
			return scopedResult(3, "git push --prune deletes remote refs with no local match", true)
		}
		if hasAnyFlag(args, "d", "delete") || pushHasRefspecWrite(args) {
			return scopedResult(3, "git push deletes or force-updates a remote ref", true)
		}
		return segResult{kind: estimateReadOnly}
	case "stash":
		if p := nthPositional(args, 1); p == "drop" || p == "clear" {
			return scopedResult(2, "git stash "+p+" discards stashed changes", true)
		}
		return segResult{kind: estimateReadOnly}
	case "rebase", "filter-branch", "filter-repo":
		return scopedResult(4, "git "+sub+" rewrites history", true)
	default:
		// Unknown git subcommand: don't claim read-only.
		return segResult{kind: estimateUncertain}
	}
}

// gitConfigIsRead reports whether a `git config` invocation only reads
// configuration (so it is safe). A write is any invocation that is not a
// recognized read and supplies a value or a mutating flag.
func gitConfigIsRead(args []string) bool {
	for _, a := range args {
		switch a {
		case "--add", "--unset", "--unset-all", "--replace-all",
			"--rename-section", "--remove-section", "-e", "--edit":
			return false
		}
	}
	if hasAnyFlag(args, "l", "list") ||
		hasLongFlag(args, "get") || hasLongFlag(args, "get-all") ||
		hasLongFlag(args, "get-regexp") || hasLongFlag(args, "get-urlmatch") {
		return true
	}
	// `git config <key>` (read) has one positional after "config";
	// `git config <key> <value>` (write) has two or more.
	return nthPositional(args, 2) == ""
}

// gitExecConfigKeyMarkers are substrings of git config keys whose value is
// executed as a command. Setting any of them (via `-c key=val` or
// `--config-env`) turns an otherwise read-only git invocation into
// arbitrary code execution.
var gitExecConfigKeyMarkers = []string{
	"sshcommand", "pager", "editor", "fsmonitor", "hookspath",
	"alias.", ".external", ".cmd", ".process", ".program", ".helper",
	".command", ".textconv", ".clean", ".smudge", ".driver", "packobjectshook",
}

// gitInjectsExecConfig reports whether args inject (via -c / --config-env) a
// config key whose value git will execute.
func gitInjectsExecConfig(args []string) bool {
	check := func(kv string) bool {
		key := kv
		if k, _, ok := strings.Cut(kv, "="); ok {
			key = k
		}
		key = strings.ToLower(key)
		for _, m := range gitExecConfigKeyMarkers {
			if strings.Contains(key, m) {
				return true
			}
		}
		return false
	}
	for i, a := range args {
		switch {
		case a == "-c" && i+1 < len(args):
			if check(args[i+1]) {
				return true
			}
		case strings.HasPrefix(a, "-c") && len(a) > 2:
			if check(a[2:]) {
				return true
			}
		case strings.HasPrefix(a, "--config-env="):
			if check(strings.TrimPrefix(a, "--config-env=")) {
				return true
			}
		}
	}
	return false
}

// pushHasRefspecWrite reports whether a push operand is a delete refspec
// (":dst") or a force refspec ("+src:dst").
func pushHasRefspecWrite(args []string) bool {
	for _, p := range positionalArgs(args) {
		if p == "push" {
			continue
		}
		if strings.HasPrefix(p, ":") || strings.HasPrefix(p, "+") {
			return true
		}
	}
	return false
}

// fetchWritesLocalRefs reports whether a `git fetch` refspec operand can
// move local refs: a "+" force prefix, or a destination under refs/heads/.
// (A normal refspec writing refs/remotes/ is not gated.)
func fetchWritesLocalRefs(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if strings.Contains(a, ":refs/heads/") || strings.Contains(a, ":heads/") {
			return true
		}
		if strings.HasPrefix(a, "+") && strings.Contains(a, ":") {
			return true
		}
	}
	return false
}

// gitGlobalValueOpts are git's pre-subcommand options that take a separate
// value word; they must be skipped to find the real subcommand. Options
// whose value is only attached with '=' (e.g. --exec-path[=path],
// --config-env=name=var) are NOT listed: consuming a following word for
// them would swallow the real subcommand and is handled by the '=' branch
// in stripGitGlobalOpts.
var gitGlobalValueOpts = map[string]bool{
	"-C": true, "-c": true, "--git-dir": true, "--work-tree": true,
	"--namespace": true, "--super-prefix": true,
}

func stripGitGlobalOpts(args []string) []string {
	i := 0
	for i < len(args) {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			break // the subcommand
		}
		if strings.Contains(a, "=") { // --git-dir=... attached form
			i++
			continue
		}
		if gitGlobalValueOpts[a] {
			i += 2 // option + its value word
			continue
		}
		i++ // boolean global (-p, --paginate, --bare, ...)
	}
	return args[i:]
}

func opDocker(_ string, args []string, _ simpleCommand, _ string) segResult {
	pos := positionalArgs(args)
	if len(pos) == 0 {
		return segResult{kind: estimateUncertain}
	}
	// Normalize "docker volume rm" / "docker rm" etc.
	noun := pos[0]
	verb := ""
	if len(pos) > 1 {
		verb = pos[1]
	}
	readonlyVerbs := map[string]bool{
		"ps": true, "images": true, "inspect": true, "logs": true, "version": true,
		"info": true, "stats": true, "top": true, "port": true, "diff": true,
		"history": true, "events": true, "search": true, "ls": true, "list": true,
	}
	// pos[1] is only a real subcommand when pos[0] is a management noun
	// (`docker container ls`). For operand-taking verbs (`docker run <image>`,
	// `docker exec <ctr> <prog>`, `docker cp <src> <dst>`), pos[1] is an
	// operand the attacker controls; honouring readonlyVerbs[verb] there lets
	// `docker run ls` (run an image literally named "ls") slip through as
	// read-only. Only trust the verb position under a management noun.
	managementNouns := map[string]bool{
		"container": true, "image": true, "volume": true, "network": true,
		"system": true, "compose": true, "builder": true, "buildx": true,
		"context": true, "plugin": true, "node": true, "service": true,
		"stack": true, "swarm": true, "config": true, "secret": true,
		"trust": true, "manifest": true, "checkpoint": true,
	}
	if readonlyVerbs[noun] || (managementNouns[noun] && readonlyVerbs[verb]) {
		return segResult{kind: estimateReadOnly}
	}

	removesVolumes := hasAnyFlag(args, "v", "volumes") || hasLongFlag(args, "volumes")
	switch noun {
	case "volume":
		if verb == "rm" || verb == "remove" {
			return scopedResult(4, "docker volume rm irreversibly deletes a named volume", true)
		}
		if verb == "prune" {
			return scopedResult(2, "docker volume prune deletes unused volumes", true)
		}
	case "system":
		if verb == "prune" {
			if removesVolumes {
				return scopedResult(4, "docker system prune --volumes deletes named volumes", true)
			}
			return scopedResult(3, "docker system prune deletes containers/images/networks", true)
		}
	case "compose":
		if verb == "down" {
			if removesVolumes {
				return scopedResult(4, "docker compose down -v deletes named volumes", true)
			}
			return scopedResult(2, "docker compose down stops and removes containers", true)
		}
	case "rm", "container":
		if removesVolumes {
			return scopedResult(4, "docker rm -v removes containers and their volumes", true)
		}
		return scopedResult(2, "docker rm removes containers", true)
	case "rmi", "image":
		if verb == "prune" {
			return scopedResult(2, "docker image prune deletes images", true)
		}
		return scopedResult(2, "docker rmi removes images (rebuildable)", true)
	case "network":
		return scopedResult(2, "docker network change can break connectivity", true)
	case "kill", "stop":
		return scopedResult(1, "docker "+noun+" stops containers (no data loss)", true)
	}
	// Other docker subcommands (run/build/exec/pull/...) are not
	// inherently destructive to local state; leave to the pattern set /
	// uncertain path rather than claiming safe.
	return segResult{kind: estimateUncertain}
}

// scopedResult builds a destructive/uncertain segResult from a score and
// whether the scope was fully resolved.
func scopedResult(score int, reason string, resolved bool) segResult {
	if score < 0 {
		score = 0
	}
	kind := estimateDestructive
	if !resolved {
		kind = estimateUncertain
	}
	return segResult{kind: kind, score: score, reason: reason}
}

// ---------------------------------------------------------------------------
// Path scope
// ---------------------------------------------------------------------------

// criticalSystemPrefixes are absolute paths whose deletion/modification
// has system-wide blast radius. Unix-centric; on other platforms these
// simply never match and the structural signals (recursion, breadth)
// still apply.
var criticalSystemPrefixes = []string{
	"/", "/etc", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/var", "/boot",
	"/dev", "/sys", "/proc", "/root", "/opt", "/srv", "/run",
	"/System", "/Library", "/Applications", "/private", "/Volumes", "/cores",
}

// worstPathScope returns the additive danger score of the most dangerous
// target location, whether all targets resolved, and a short label for
// the worst location.
func worstPathScope(targets []string, workdir string) (score int, resolved bool, label string) {
	resolved = true
	for _, t := range targets {
		if t == "" {
			continue
		}
		if strings.Contains(t, "$") || strings.Contains(t, "`") {
			resolved = false
			continue
		}
		s, l := pathScopeScore(t, workdir)
		if s > score {
			score = s
			label = l
		}
	}
	return score, resolved, label
}

func pathScopeScore(raw, workdir string) (int, string) {
	p := expandHome(raw)

	// Glob/wildcard: score the directory it lives in, breadth handled
	// separately. Strip the glob component for scope purposes.
	if containsGlob(p) {
		p = globParent(p)
	}
	clean := filepath.Clean(p)

	// Anchor the target to an absolute path rooted at the working
	// directory so that a relative climb (../../etc) and an in-workdir
	// symlink to a system location are both scored by where they land.
	wasAbs := filepath.IsAbs(clean)
	var abs string
	switch {
	case wasAbs:
		abs = clean
	case workdir != "":
		abs = filepath.Clean(filepath.Join(workdir, clean))
	default:
		// Relative path with no working directory to anchor it: a path
		// that climbs out via ".." is still suspicious.
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return 2, "escapes the working directory"
		}
		return 0, ""
	}

	// A target that stays inside the working directory (checked both
	// literally and through symlink resolution) is scored by the breadth
	// probe, not by location.
	if workdir != "" {
		if !escapesWorkdir(abs, workdir) &&
			!escapesWorkdir(evalSymlinksBestEffort(abs), evalSymlinksBestEffort(workdir)) {
			return 0, ""
		}
	}

	// Outside the working directory (or workdir unknown): score by where
	// the path actually lands, following symlinks.
	for _, c := range pathScopeCandidates(abs, wasAbs) {
		switch {
		case c == "/":
			return 6, "filesystem root"
		case isCriticalSystemPath(c):
			return 5, "system path " + c
		case isHomeRoot(raw, c):
			return 4, "home directory root"
		}
	}
	if workdir == "" {
		return 2, "absolute path " + clean
	}
	return 2, "outside the working directory"
}

// pathScopeCandidates returns the paths a scope check should consider. For
// a path the user typed as absolute, that is the literal path and its
// symlink-resolved real path. For a path anchored to the working directory
// (relative input), only the symlink-resolved real destination is scored —
// the literal anchored path inherits the workdir's location (which may
// itself sit under /var, /private, ... and would otherwise read as a false
// "system path").
func pathScopeCandidates(abs string, wasAbs bool) []string {
	if abs == "" {
		return nil
	}
	resolved := evalSymlinksBestEffort(abs)
	if !wasAbs {
		return []string{resolved}
	}
	if resolved != abs {
		return []string{abs, resolved}
	}
	return []string{abs}
}

// evalSymlinksBestEffort resolves symlinks in p, falling back to resolving
// the longest existing ancestor and re-appending the (non-existent) tail
// so that a not-yet-created file inside a symlinked directory still
// resolves through the link.
func evalSymlinksBestEffort(p string) string {
	if p == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	dir := filepath.Dir(p)
	if dir == p {
		return p
	}
	return filepath.Join(evalSymlinksBestEffort(dir), filepath.Base(p))
}

func isCriticalSystemPath(clean string) bool {
	for _, prefix := range criticalSystemPrefixes {
		if prefix == "/" {
			continue // handled separately (exact root only)
		}
		if clean == prefix || strings.HasPrefix(clean, prefix+"/") {
			return true
		}
	}
	return false
}

func isHomeRoot(raw, clean string) bool {
	if raw == "~" || raw == "~/" || raw == "$HOME" || raw == "${HOME}" {
		return true
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" && clean == filepath.Clean(home) {
		return true
	}
	// /home/<user> or /Users/<user> with exactly one component below.
	for _, base := range []string{"/home", "/Users"} {
		if rest, ok := strings.CutPrefix(clean, base+"/"); ok {
			if rest != "" && !strings.Contains(rest, "/") {
				return true
			}
		}
	}
	return false
}

// escapesWorkdir reports whether abs is outside workdir (a sibling,
// parent, or unrelated absolute path). Both are cleaned absolute paths.
func escapesWorkdir(abs, workdir string) bool {
	wd := filepath.Clean(workdir)
	if abs == wd {
		return false
	}
	rel, err := filepath.Rel(wd, abs)
	if err != nil {
		return true
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ---------------------------------------------------------------------------
// Filesystem probing (read-only, bounded)
// ---------------------------------------------------------------------------

// breadthOfTargets probes the filesystem to estimate how much a target
// (or glob) actually covers. Returns an additive score, a human note,
// and whether the probe could resolve the targets. Read-only and bounded.
//
// Probing is confined to paths inside the working directory: the
// validator never walks or globs arbitrary absolute paths the model
// named (avoids touching /, /etc, ... and bounds the cost). Targets
// outside the workdir are scored by path scope alone.
func breadthOfTargets(targets []string, recursive bool, workdir string) (score int, note string, resolved bool) {
	resolved = true
	var total int
	var sawMissing, sawExisting bool
	for _, t := range targets {
		if t == "" {
			continue
		}
		if strings.Contains(t, "$") || strings.Contains(t, "`") {
			resolved = false
			continue
		}
		abs := resolvePath(t, workdir)
		if !probeableInWorkdir(globParent(abs), workdir) {
			continue // outside workdir: scored by path scope only
		}
		if containsGlob(t) {
			matches, _ := filepath.Glob(abs)
			if len(matches) == 0 {
				sawMissing = true
				continue
			}
			sawExisting = true
			total += len(matches)
			for _, m := range matches {
				if recursive {
					total += countEntries(m)
				}
			}
			continue
		}
		exists, isDir := probeExists(abs)
		if !exists {
			sawMissing = true
			continue
		}
		sawExisting = true
		if recursive && isDir {
			total += countEntries(abs)
		} else {
			total++
		}
	}

	switch {
	case total >= probeManyFiles:
		return 2, "affects many files (~" + countLabel(total) + ")", resolved
	case sawExisting && total > 1:
		return 1, "affects " + countLabel(total) + " files", resolved
	case sawMissing && !sawExisting:
		// Nothing to lose: the target does not exist.
		return -2, "target does not exist", resolved
	default:
		return 0, "", resolved
	}
}

// countEntries walks abs counting filesystem entries, bounded by
// probeMaxFiles and probeMaxDepth and never following symlinks.
func countEntries(abs string) int {
	count := 0
	base := strings.Count(filepath.Clean(abs), string(filepath.Separator))
	_ = filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries; keep walking
		}
		count++
		if count >= probeMaxFiles {
			return filepath.SkipAll
		}
		if d.IsDir() {
			depth := strings.Count(filepath.Clean(path), string(filepath.Separator)) - base
			if depth >= probeMaxDepth {
				return filepath.SkipDir
			}
			// Do not descend into symlinked directories (WalkDir already
			// does not follow symlinks, but guard against deep trees).
		}
		return nil
	})
	return count
}

func countLabel(n int) string {
	if n >= probeMaxFiles {
		return "5000+"
	}
	return itoa(n)
}

// probeableInWorkdir reports whether abs is a non-empty path inside
// workdir and therefore safe (and cheap) to stat/walk during validation.
func probeableInWorkdir(abs, workdir string) bool {
	if abs == "" || workdir == "" {
		return false
	}
	return !escapesWorkdir(abs, workdir)
}

func probeExists(abs string) (exists, isDir bool) {
	if abs == "" {
		return false, false
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return false, false
	}
	return true, info.IsDir()
}

func resolvePath(raw, workdir string) string {
	if raw == "" {
		return ""
	}
	p := expandHome(raw)
	// Globs are left verbatim here; the caller expands them via filepath.Glob.
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if workdir == "" {
		return ""
	}
	return filepath.Clean(filepath.Join(workdir, p))
}

func expandHome(raw string) string {
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if raw == "~" {
				return home
			}
			return filepath.Join(home, raw[2:])
		}
	}
	return raw
}

// ---------------------------------------------------------------------------
// Read-only program allowlist
// ---------------------------------------------------------------------------

var readOnlyPrograms = map[string]bool{
	"ls": true, "dir": true, "vdir": true, "pwd": true, "echo": true,
	"printf": true, "cat": true, "bat": true, "head": true, "tail": true,
	"wc": true, "stat": true, "file": true, "basename": true, "dirname": true,
	"readlink": true, "realpath": true, "true": true, "false": true,
	"test": true, "date": true, "cal": true, "uptime": true, "uname": true,
	"hostname": true, "whoami": true, "id": true, "groups": true,
	"printenv": true, "which": true, "type": true, "df": true, "du": true,
	"free": true, "ps": true, "pgrep": true, "lscpu": true, "lsblk": true,
	"lsof": true, "netstat": true, "ss": true, "ifconfig": true,
	"ping": true, "dig": true, "nslookup": true, "host": true,
	"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true,
	"tree": true, "sort": true, "uniq": true, "cut": true, "column": true,
	"diff": true, "cmp": true, "md5sum": true, "sha1sum": true,
	"sha256sum": true, "cksum": true, "od": true, "xxd": true, "hexdump": true,
	"strings": true, "nl": true, "tac": true, "rev": true, "fold": true,
	"jq": true, "yq": true, "less": true, "more": true, "tr": true,
	"seq": true, "yes": true, "sleep": true,
	// Harmless shell builtins / navigation: change dirs, query job state.
	// None of these destroy filesystem data or execute a command.
	"cd": true, "pushd": true, "popd": true, "dirs": true, ":": true,
	"unset": true, "umask": true, "history": true,
	"jobs": true, "fg": true, "bg": true, "wait": true, "hash": true,
	"help": true, "let": true, "read": true, "ulimit": true, "times": true,
	// NOTE: the following are deliberately NOT here — each is a code-exec
	// or state-injection vector that must stay gated:
	//   enable   (`enable -f <obj.so>` dlopen()s native code)
	//   trap     (`trap '<cmd>' EXIT` runs a command on shell exit)
	//   export / declare / typeset / local / set
	//            (set a code-injecting var such as LD_PRELOAD/PS4, or
	//             `set -x` which makes PS4 expand a command substitution)
	//   alias / unalias / shopt
	//            (redefine a later command, or enable alias expansion)
}

func isReadOnlyProgram(prog string, args []string) bool {
	if prog == "" {
		return false
	}
	// A command-executing flag (ripgrep --pre, sort --compress-program,
	// bat --pager, ...) turns any read-only program into code execution.
	if hasCommandValuedFlag(args) {
		return false
	}
	if !readOnlyProgramsSafe(prog, args) {
		return false
	}
	return readOnlyPrograms[prog]
}

// readOnlyProgramsSafe handles read-only programs that become destructive
// (or state-changing) with specific flags or operands. Returning false
// drops the program out of the read-only allowlist so it is gated.
func readOnlyProgramsSafe(prog string, args []string) bool {
	switch prog {
	case "ln":
		return false // creating/overwriting symlinks
	case "rg":
		// `rg --pre <COMMAND>` runs COMMAND for every file searched (the
		// preprocessor feature), passing the filename as an argument: an
		// arbitrary-code-execution vector (e.g. `rg --pre=reboot x .`).
		return !hasLongFlag(args, "pre")
	case "sort":
		return !hasAnyFlag(args, "o", "output") // `sort -o file` writes a file
	case "yq":
		return !hasAnyFlag(args, "i", "inplace") // `yq -i` edits in place
	case "tree":
		return !hasAnyFlag(args, "o", "output") // `tree -o file` writes a file
	case "uniq":
		// `uniq INPUT OUTPUT` truncates and overwrites the second operand.
		return len(positionalArgs(args)) < 2
	case "xxd":
		// `xxd INFILE OUTFILE` and `xxd -r DUMP OUTFILE` write the second
		// operand (reverse mode patches arbitrary binary into it).
		return len(positionalArgs(args)) < 2
	case "history":
		// `history -w/-a FILE` overwrites/appends an arbitrary file.
		return !hasAnyFlag(args, "w", "write", "a", "append")
	case "date":
		return !hasAnyFlag(args, "s", "set") // `date -s` sets the system clock
	case "hostname":
		// A trailing name operand (or -F/--file) sets the hostname.
		return len(positionalArgs(args)) == 0 && !hasAnyFlag(args, "F", "file")
	case "ifconfig":
		// `ifconfig IFACE up/down/ADDR` mutates; a single (or no) operand
		// is a query.
		return len(positionalArgs(args)) <= 1
	}
	return true
}

// ---------------------------------------------------------------------------
// Redirections
// ---------------------------------------------------------------------------

// harmlessRedirectTargets are write targets that discard or echo and
// carry no blast radius.
var harmlessRedirectTargets = map[string]bool{
	"/dev/null": true, "/dev/stdout": true, "/dev/stderr": true, "/dev/tty": true,
}

func classifyRedirects(redirects []redirect, workdir string) (score int, reason string, resolved bool) {
	resolved = true
	for _, r := range redirects {
		if !r.overwrite() {
			continue
		}
		target := r.target
		if target == "" {
			continue
		}
		if harmlessRedirectTargets[target] {
			continue
		}
		if strings.Contains(target, "$") || strings.Contains(target, "`") {
			resolved = false
			if score < 2 {
				score = 2
				reason = "redirects over a path built from a variable"
			}
			continue
		}
		s := 1 // overwrite of a plain file is low/medium
		if r.appends() {
			s = 0 // append does not destroy existing content
		}
		pScore, _, ploc := worstPathScope([]string{target}, workdir)
		s += pScore
		if isBlockDevice(target) {
			s = 5
		}
		if abs := resolvePath(target, workdir); !r.appends() && probeableInWorkdir(abs, workdir) {
			if exists, _ := probeExists(abs); !exists {
				// Writing to a new file destroys nothing.
				s--
			}
		}
		if s > score {
			score = s
			verb := "overwrites"
			if r.appends() {
				verb = "appends to"
			}
			reason = "redirects (" + verb + ") " + target
			if isBlockDevice(target) {
				reason = "redirects into block device " + target
			}
			if ploc != "" {
				reason += " (" + ploc + ")"
			}
		}
	}
	if score < 0 {
		score = 0
	}
	return score, reason, resolved
}

// ---------------------------------------------------------------------------
// Tokenizer / lexer
// ---------------------------------------------------------------------------

type redirect struct {
	op     string // ">", ">>", ">|", "&>", "&>>", "<", "<<", "<<<"
	fd     string // optional leading fd ("2" in "2>")
	target string
	dup    bool // file-descriptor duplication (2>&1, >&-): no file target
}

func (r redirect) overwrite() bool {
	if r.dup {
		return false // fd duplication touches no file
	}
	switch r.op {
	case ">", ">|", "&>", "&>>", ">>":
		return true
	}
	return false
}

func (r redirect) appends() bool {
	return r.op == ">>" || r.op == "&>>"
}

type simpleCommand struct {
	words         []string
	redirects     []redirect
	stdinFromPipe bool
}

// lexCommand splits a shell command line into simple commands (segments
// of pipelines and lists) and reports whether it contains a command
// substitution ($(...) or backticks). The lexer respects single and
// double quotes and extracts redirections; it is intentionally
// conservative — anything it cannot parse degrades to a lower-confidence
// (gated) verdict rather than a false-safe one.
func lexCommand(cmd string) (cmds []simpleCommand, dynamic bool) {
	var elems []element
	elems, dynamic = lexElements(cmd)

	var cur []element
	flush := func(fromPipe bool) {
		sc, ok := buildSimpleCommand(cur, fromPipe)
		if ok {
			cmds = append(cmds, sc)
		}
		cur = nil
	}
	curFromPipe := false
	for _, e := range elems {
		switch e.kind {
		case elemSepSemantic:
			flush(curFromPipe)
			curFromPipe = false
		case elemSepPipe:
			flush(curFromPipe)
			curFromPipe = true
		default:
			cur = append(cur, e)
		}
	}
	flush(curFromPipe)
	return cmds, dynamic
}

func buildSimpleCommand(elems []element, fromPipe bool) (simpleCommand, bool) {
	sc := simpleCommand{stdinFromPipe: fromPipe}
	for i := 0; i < len(elems); i++ {
		e := elems[i]
		switch e.kind {
		case elemWord:
			sc.words = append(sc.words, e.text)
		case elemRedirect:
			r := redirect{op: e.text, fd: e.fd, dup: e.dup}
			// A non-dup redirect takes the next word element as its target.
			if !e.dup && i+1 < len(elems) && elems[i+1].kind == elemWord {
				r.target = elems[i+1].text
				i++
			}
			sc.redirects = append(sc.redirects, r)
		}
	}
	if len(sc.words) == 0 && len(sc.redirects) == 0 {
		return sc, false
	}
	return sc, true
}

type elemKind int

const (
	elemWord        elemKind = iota
	elemSepSemantic          // ; && || & newline -> hard segment boundary
	elemSepPipe              // | |& -> the next segment reads from a pipe
	elemRedirect
)

type element struct {
	kind elemKind
	text string
	fd   string
	dup  bool
}

func lexElements(cmd string) (elems []element, dynamic bool) {
	var w strings.Builder
	wordActive := false
	flush := func() {
		if wordActive {
			elems = append(elems, element{kind: elemWord, text: w.String()})
			w.Reset()
			wordActive = false
		}
	}
	i, n := 0, len(cmd)
	for i < n {
		c := cmd[i]
		switch c {
		case '\'':
			wordActive = true
			if j := strings.IndexByte(cmd[i+1:], '\''); j >= 0 {
				w.WriteString(cmd[i+1 : i+1+j])
				i += j + 2
			} else {
				w.WriteString(cmd[i+1:])
				i = n
			}
		case '"':
			wordActive = true
			i++
			for i < n && cmd[i] != '"' {
				switch {
				case cmd[i] == '\\' && i+1 < n:
					w.WriteByte(cmd[i+1])
					i += 2
				case cmd[i] == '$' && i+1 < n && cmd[i+1] == '(':
					dynamic = true
					i = skipParens(cmd, i+1)
				case cmd[i] == '`':
					dynamic = true
					if j := strings.IndexByte(cmd[i+1:], '`'); j >= 0 {
						i += j + 2
					} else {
						i = n
					}
				default:
					w.WriteByte(cmd[i])
					i++
				}
			}
			if i < n {
				i++ // closing quote
			}
		case '\\':
			wordActive = true
			if i+1 < n {
				if cmd[i+1] == '\n' {
					i += 2 // line continuation
				} else {
					w.WriteByte(cmd[i+1])
					i += 2
				}
			} else {
				i++
			}
		case ' ', '\t', '\r':
			flush()
			i++
		case '\n', ';':
			flush()
			elems = append(elems, element{kind: elemSepSemantic})
			i++
		case '&':
			flush()
			switch {
			case i+1 < n && cmd[i+1] == '&':
				elems = append(elems, element{kind: elemSepSemantic})
				i += 2
			case i+1 < n && cmd[i+1] == '>':
				op := "&>"
				i += 2
				if i < n && cmd[i] == '>' {
					op = "&>>"
					i++
				}
				// `&>(cmd)` / `&>>(cmd)` is a combined redirect plus an
				// output process substitution — unclassified code; force the
				// fail-closed uncertain path like `>(`, `<(`, and `$(`.
				if i < n && cmd[i] == '(' {
					dynamic = true
					i = skipParens(cmd, i)
					continue
				}
				elems = append(elems, element{kind: elemRedirect, text: op})
			default:
				elems = append(elems, element{kind: elemSepSemantic}) // background
				i++
			}
		case '|':
			flush()
			switch {
			case i+1 < n && cmd[i+1] == '|':
				elems = append(elems, element{kind: elemSepSemantic})
				i += 2
			case i+1 < n && cmd[i+1] == '&':
				elems = append(elems, element{kind: elemSepPipe})
				i += 2
			default:
				elems = append(elems, element{kind: elemSepPipe})
				i++
			}
		case '>':
			// Output process substitution `>(cmd)` runs cmd as a
			// sub-process; treat it like $(...) — unclassified code, so the
			// whole command is forced onto the fail-closed uncertain path.
			if i+1 < n && cmd[i+1] == '(' {
				flush()
				dynamic = true
				i = skipParens(cmd, i+1)
				continue
			}
			fd := ""
			if wordActive && isAllDigits(w.String()) {
				fd = w.String()
				w.Reset()
				wordActive = false
			} else {
				flush()
			}
			op := ">"
			i++
			if i < n && cmd[i] == '>' {
				op = ">>"
				i++
			} else if i < n && cmd[i] == '|' {
				op = ">|"
				i++
			}
			// File-descriptor duplication: >&1, 2>&1, >&- (no file target).
			if i < n && cmd[i] == '&' {
				j := i + 1
				for j < n && (cmd[j] == '-' || (cmd[j] >= '0' && cmd[j] <= '9')) {
					j++
				}
				if j > i+1 {
					i = j
					elems = append(elems, element{kind: elemRedirect, text: op, fd: fd, dup: true})
					continue
				}
			}
			elems = append(elems, element{kind: elemRedirect, text: op, fd: fd})
		case '<':
			// Input process substitution `<(cmd)` runs cmd as a sub-process
			// whose output the outer program reads; treat it like $(...) —
			// unclassified code, forced onto the fail-closed uncertain path.
			if i+1 < n && cmd[i+1] == '(' {
				flush()
				dynamic = true
				i = skipParens(cmd, i+1)
				continue
			}
			// input redirect; consume operator + (later) target word, but
			// input redirects carry no destructive blast radius.
			if wordActive && isAllDigits(w.String()) {
				w.Reset()
				wordActive = false
			} else {
				flush()
			}
			op := "<"
			i++
			if i < n && cmd[i] == '<' {
				op = "<<"
				i++
				if i < n && cmd[i] == '<' {
					op = "<<<"
					i++
				}
			}
			elems = append(elems, element{kind: elemRedirect, text: op})
		case '$':
			wordActive = true
			if i+1 < n && cmd[i+1] == '(' {
				dynamic = true
				i = skipParens(cmd, i+1)
			} else {
				w.WriteByte(c)
				i++
			}
		case '`':
			// Backtick command substitution: mirror the `$(` arm and mark
			// the word active so a bare `` `cmd` `` still yields a segment.
			wordActive = true
			dynamic = true
			if j := strings.IndexByte(cmd[i+1:], '`'); j >= 0 {
				i += j + 2
			} else {
				i = n
			}
		default:
			wordActive = true
			w.WriteByte(c)
			i++
		}
	}
	flush()
	return elems, dynamic
}

// skipParens returns the index just past the ')' matching the '(' at
// position open (cmd[open] == '('), handling nesting. If unbalanced,
// returns len(cmd).
func skipParens(cmd string, open int) int {
	depth := 0
	for i := open; i < len(cmd); i++ {
		switch cmd[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(cmd)
}

// ---------------------------------------------------------------------------
// Argument helpers
// ---------------------------------------------------------------------------

// wrapperCommands are programs that prefix another command; the effective
// program is what follows them (after their own options).
var wrapperCommands = map[string]bool{
	"sudo": true, "doas": true, "env": true, "nice": true, "ionice": true,
	"nohup": true, "time": true, "stdbuf": true, "setsid": true,
	"command": true, "builtin": true, "exec": true, "timeout": true,
	"xargs": true, "watch": true, "eatmydata": true, "proxychains": true,
}

// effectiveProgram strips leading environment assignments and wrapper
// commands and returns the real program (basename, lowercased), its
// arguments, whether a stdin-feeding wrapper (xargs) was stripped (in
// which case the program's real targets come from stdin, not the args),
// and every environment assignment it stripped — including ones carried by
// an `env`/`sudo` wrapper (`env LD_PRELOAD=x ls`). The caller inspects
// those so a code-injecting assignment cannot ride through as read-only.
func effectiveProgram(words []string) (prog string, args []string, stdinFed bool, envAssigns []string) {
	idx := 0
	for idx < len(words) {
		w := words[idx]
		// FOO=bar assignment prefix (leading, or carried after an env/sudo
		// wrapper whose options were just consumed).
		if isAssignment(w) {
			envAssigns = append(envAssigns, w)
			idx++
			continue
		}
		base := programBase(w)
		if !wrapperCommands[base] {
			break
		}
		if base == "xargs" {
			stdinFed = true
		}
		// Skip the wrapper word and its own option arguments. Assignments
		// carried by the wrapper are left for the loop above to collect.
		idx++
		idx = skipWrapperOptions(base, words, idx)
	}
	if idx >= len(words) {
		return "", nil, stdinFed, envAssigns
	}
	return programBase(words[idx]), words[idx+1:], stdinFed, envAssigns
}

// hasUnsafeAssignment reports whether any of the given VAR=value tokens
// assigns a variable that is not a known-safe locale/timezone variable
// (and could therefore inject code, e.g. LD_PRELOAD, BASH_ENV, GIT_SSH).
func hasUnsafeAssignment(assigns []string) bool {
	for _, a := range assigns {
		name, _, ok := strings.Cut(a, "=")
		if !ok {
			continue
		}
		if !safeLeadingEnvVars[strings.ToLower(name)] {
			return true
		}
	}
	return false
}

// skipWrapperOptions advances past a wrapper's own options/arguments so
// the next word is the wrapped program. Conservative: only consumes
// option-looking tokens and the well-known value-taking options.
func skipWrapperOptions(wrapper string, words []string, idx int) int {
	switch wrapper {
	case "env":
		// Consume env's own flags only; stop at the first VAR=value (the
		// main loop in effectiveProgram collects assignments so a carried
		// LD_PRELOAD/BASH_ENV is not silently dropped).
		for idx < len(words) {
			w := words[idx]
			if w == "-u" || w == "-C" || w == "--unset" || w == "--chdir" {
				idx += 2
				continue
			}
			if strings.HasPrefix(w, "-") {
				idx++
				continue
			}
			break
		}
	case "timeout":
		// timeout [opts] DURATION CMD
		for idx < len(words) && strings.HasPrefix(words[idx], "-") {
			if words[idx] == "-s" || words[idx] == "--signal" || words[idx] == "-k" || words[idx] == "--kill-after" {
				idx += 2
				continue
			}
			idx++
		}
		if idx < len(words) {
			idx++ // DURATION
		}
	case "nice":
		for idx < len(words) && strings.HasPrefix(words[idx], "-") {
			if words[idx] == "-n" || words[idx] == "--adjustment" {
				idx += 2
				continue
			}
			idx++
		}
	case "ionice":
		for idx < len(words) && strings.HasPrefix(words[idx], "-") {
			if words[idx] == "-c" || words[idx] == "-n" || words[idx] == "-p" {
				idx += 2
				continue
			}
			idx++
		}
	case "stdbuf":
		for idx < len(words) && strings.HasPrefix(words[idx], "-") {
			idx++
		}
	case "sudo", "doas":
		for idx < len(words) && strings.HasPrefix(words[idx], "-") {
			if words[idx] == "-u" || words[idx] == "-g" || words[idx] == "-C" || words[idx] == "-p" || words[idx] == "-U" || words[idx] == "-r" || words[idx] == "-t" || words[idx] == "-h" {
				idx += 2
				continue
			}
			idx++
		}
	case "xargs":
		for idx < len(words) && strings.HasPrefix(words[idx], "-") {
			if words[idx] == "-n" || words[idx] == "-P" || words[idx] == "-I" || words[idx] == "-d" || words[idx] == "-E" || words[idx] == "-s" || words[idx] == "-L" || words[idx] == "--max-args" || words[idx] == "--max-procs" || words[idx] == "--replace" || words[idx] == "--delimiter" {
				idx += 2
				continue
			}
			idx++
		}
	default:
		// command/builtin/exec/nohup/time/setsid/watch/...: skip leading flags only.
		for idx < len(words) && strings.HasPrefix(words[idx], "-") {
			idx++
		}
	}
	return idx
}

func isAssignment(w string) bool {
	eq := strings.IndexByte(w, '=')
	if eq <= 0 {
		return false
	}
	for i := range eq {
		c := w[i]
		isLetter := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		isDigit := i > 0 && c >= '0' && c <= '9'
		if !isLetter && !isDigit {
			return false
		}
	}
	return true
}

func programBase(w string) string {
	w = strings.TrimPrefix(w, "\\")
	w = filepath.Base(w)
	return strings.ToLower(w)
}

// positionalArgs returns the non-flag arguments (file targets etc.).
// Flags and their attached values are skipped; "--" terminates option
// parsing.
func positionalArgs(args []string) []string {
	var out []string
	afterDD := false
	for _, a := range args {
		if afterDD {
			out = append(out, a)
			continue
		}
		if a == "--" {
			afterDD = true
			continue
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			continue
		}
		out = append(out, a)
	}
	return out
}

func firstPositional(args []string) string {
	return nthPositional(args, 0)
}

// nthPositional returns the n-th (0-based) non-flag argument.
func nthPositional(args []string, n int) string {
	i := 0
	for _, a := range args {
		if strings.HasPrefix(a, "-") && a != "-" {
			continue
		}
		if i == n {
			return a
		}
		i++
	}
	return ""
}

func hasUnresolvedTarget(targets []string) bool {
	for _, t := range targets {
		if strings.Contains(t, "$") || strings.Contains(t, "`") {
			return true
		}
	}
	return false
}

// hasAnyFlag reports whether any of names appears as a short flag (in a
// cluster like -rf) or long flag (--force). Single-char names are matched
// both as clustered short flags and as exact tokens.
func hasAnyFlag(args []string, names ...string) bool {
	for _, a := range args {
		if a == "--" {
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			continue
		}
		if long, ok := strings.CutPrefix(a, "--"); ok {
			if eq := strings.IndexByte(long, '='); eq >= 0 {
				long = long[:eq]
			}
			for _, nm := range names {
				if len(nm) > 1 && long == nm {
					return true
				}
			}
			continue
		}
		// short cluster, e.g. -rf
		cluster := a[1:]
		for _, nm := range names {
			if len(nm) == 1 && strings.ContainsRune(cluster, rune(nm[0])) {
				return true
			}
		}
	}
	return false
}

func hasShortClusterFlag(args []string, f byte) bool {
	for _, a := range args {
		if a == "--" {
			break
		}
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a[1:], rune(f)) {
			return true
		}
	}
	return false
}

func hasLongFlag(args []string, name string) bool {
	for _, a := range args {
		if a == "--" {
			break
		}
		long, ok := strings.CutPrefix(a, "--")
		if !ok {
			continue
		}
		if eq := strings.IndexByte(long, '='); eq >= 0 {
			long = long[:eq]
		}
		if long == name {
			return true
		}
	}
	return false
}

// flagValue returns the value of a short (-s 0 / -s0) or long
// (--size=0 / --size 0) flag.
func flagValue(args []string, short, long string) string {
	for i, a := range args {
		if a == "--" {
			break
		}
		if short != "" && a == "-"+short {
			if i+1 < len(args) {
				return args[i+1]
			}
		}
		if short != "" && strings.HasPrefix(a, "-"+short) && len(a) > len(short)+1 && !strings.HasPrefix(a, "--") {
			return a[len(short)+1:]
		}
		if long != "" {
			if v, ok := strings.CutPrefix(a, "--"+long+"="); ok {
				return v
			}
			if a == "--"+long && i+1 < len(args) {
				return args[i+1]
			}
		}
	}
	return ""
}

// ddOperand extracts an `of=` / `if=` operand from dd-style args.
func ddOperand(args []string, key string) string {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, key+"="); ok {
			return v
		}
	}
	return ""
}

func isBlockDevice(path string) bool {
	p := filepath.Clean(expandHome(path))
	return strings.HasPrefix(p, "/dev/") && !harmlessRedirectTargets[p]
}

func containsGlob(p string) bool {
	return strings.ContainsAny(p, "*?[")
}

func globParent(p string) string {
	// Return the longest leading path component free of glob metacharacters.
	dir := p
	for containsGlob(dir) {
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
	return dir
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// ---------------------------------------------------------------------------
// Interpreters and find
// ---------------------------------------------------------------------------

var interpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true,
	"fish": true, "csh": true, "tcsh": true, "ash": true,
	"python": true, "python2": true, "python3": true, "perl": true,
	"ruby": true, "node": true, "nodejs": true, "php": true, "lua": true,
	"rscript": true, "osascript": true, "pwsh": true, "powershell": true,
}

func isInterpreter(prog string) bool {
	return interpreters[prog]
}

// shellInterpreters are POSIX-ish shells whose -c argument is a shell
// command we can recursively analyse. Non-shell interpreters (python,
// perl, ...) carry their own language in -c/-e and must not be re-lexed
// as shell.
var shellInterpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true,
	"ksh": true, "ash": true, "csh": true, "tcsh": true, "fish": true,
}

func isShellInterpreter(prog string) bool {
	return shellInterpreters[prog]
}

// interpreterHasScriptArg reports whether the interpreter was given an
// inline script (-c / -e) or a script file to run (so it is not just
// executing whatever arrives on stdin).
func interpreterHasScriptArg(prog string, args []string) bool {
	if _, ok := interpreterInlineScript(prog, args); ok {
		return true
	}
	// A bare script-file argument.
	return firstPositional(args) != ""
}

// interpreterInlineScript returns the inline script passed via -c (sh,
// bash, ...) or -e (perl/ruby), if present.
func interpreterInlineScript(prog string, args []string) (string, bool) {
	flag := "-c"
	switch prog {
	case "perl", "ruby":
		flag = "-e"
	}
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
		if strings.HasPrefix(a, flag) && len(a) > len(flag) {
			return a[len(flag):], true
		}
	}
	return "", false
}

// findDestructiveActions are the find primaries that act on matches.
var findDestructiveActions = []string{
	"-delete", "-exec", "-execdir", "-ok", "-okdir",
	"-fprint", "-fprintf", "-fls",
}

func findHasDestructiveAction(args []string) bool {
	for _, a := range args {
		if slices.Contains(findDestructiveActions, a) {
			return true
		}
	}
	return false
}

// findRoots returns the starting path operands of a find invocation (the
// words before the first primary, which all begin with '-' or are
// expression operators).
func findRoots(args []string) []string {
	var roots []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") || a == "(" || a == "!" {
			break
		}
		roots = append(roots, a)
	}
	if len(roots) == 0 {
		roots = []string{"."}
	}
	return roots
}
