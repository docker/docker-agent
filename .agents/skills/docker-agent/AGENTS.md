# Editorial policy for the docker-agent skill

`SKILL.md` and `references/` here are read by every agent that has skills
enabled and runs from (or above) this repo, or that installs this bundle in
a later phase. A wrong line misleads every agent that reads it. This file
is addressed to whoever — human or agent — edits the skill; it is not part
of the installed content.

## Never document what you have not observed

Help text and doc comments state intent and drift from behavior. Before
writing a claim about a command, flag, or config field, run it (or read the
struct/schema definition directly) rather than paraphrasing a `--help`
string or a doc page from memory. Two corrections made while writing v1
came from doing exactly this: omitting `version:` targets the *latest*
schema (verified via `debug config` on a version-less file — `pkg/config/
config.go`'s `cmp.Or(raw.Version, latest.Version)`), not an old pinned
version; and `--yolo` does exist on `run` (it was just past an arbitrary
`head` cutoff during a first pass).

## Never document a command that blocks on a TTY, or a hidden flag/command

`new`, `setup`, `getting-started`/`tour`, `board`, and bare `run` (no
`--exec`) hang forever if invoked non-interactively — an agent that trusts
a line telling it to run one of these will hang with no error. Likewise
never mention a `MarkHidden` flag (grep `cmd/root/*.go` for
`MarkHidden`) or the internal `__askpass` command. Both lists are enumerated
in `references/cli.md`; re-check them with a fresh `--help` walk whenever a
command changes, since a new hidden flag or a newly-unhidden one won't
announce itself.

## When a command or field changes, re-derive its guidance

Don't syntax-swap a flag name in place and leave the surrounding rationale
untouched — rationale written for the old shape can be actively wrong for
the new one. The same facts appear in more than one of these four files
(SKILL.md's rules restate cli.md's detail, briefly) — grep and update every
occurrence.

## Keep `SKILL.md` lean, and keep the frontmatter description safe

Target SKILL.md's body under ~500 lines / ~5,000 tokens (currently ~120
lines); push anything exhaustive into `references/`, one level deep.
SKILL.md should read as judgment and workflow selection, not a flag dump.

**`description` must be a single physical line — no YAML line-folding.**
`pkg/skills/frontmatter.go` is a line-based parser, not a real YAML parser:
a folded/continued `description:` line is silently dropped (verified
empirically) and can leave a stray leading quote in what survives. Don't
copy GitButler-style multi-line frontmatter descriptions verbatim; write it
as one line, however long, or split it — never wrap it.

**Target `description` around 300–500 characters**, front-loaded with
trigger keywords (`docker-agent`, `cagent`, `agent config`, `MCP`, `A2A`,
...) in the first ~100 characters. This is tighter than the schema's bare
validity ceiling: Claude Code enforces a *shared* per-installation listing
budget across every enabled skill (reportedly ~1% of the context window),
and when that shared budget overflows, the least-invoked skills'
descriptions are stripped first — which is exactly this skill, freshly
installed with zero invocations. A short, keyword-dense description survives
that trim; a long one is first to go.

## No install mechanism yet — don't add one silently

This bundle is content only. No `docker-agent skill install` command, no
`//go:embed` wiring, no CI drift gate, no agent-detection/freshness-nag
logic exists yet (see task `65afbc73-1838-44b6-8485-02e3ecf5d52e` and its
parent design doc). Don't add unsolicited terminal output or
auto-install/auto-repair behavior as a side effect of an unrelated change to
this directory — that is separately-scoped future work.

## Known overlap: `pkg/creator/instructions.txt`

`docker-agent new`'s system prompt (`pkg/creator/instructions.txt`, ~193
lines, `//go:embed`ed) already covers a meaningful slice of what
`references/config.md` covers — written for an LLM builder session rather
than progressive-disclosure documentation, so it is *not* a copy-paste
source. If the two ever disagree, `agent-schema.json` is authoritative;
fix whichever of the two is wrong rather than assuming they must match
line-for-line. Unifying them is an explicitly deferred follow-up, not
something to attempt opportunistically here.

## Don't generate the prose from `--help` or the schema

A generator faithfully reproduces every inaccuracy in a `Short:` string and
cannot produce judgment calls ("never run bare `new`", "use `--safety
restricted` for unattended runs") — that judgment is most of this skill's
value. Verify facts by running commands or reading source; write the prose
by hand.
