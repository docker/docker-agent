# Editorial policy for the docker-agent skill

`docker-agent/SKILL.md` and `docker-agent/references/` (siblings of this
file, under `skill/`) are the redistributable master copy of an Agent Skill:
content meant to be installed into **someone else's** project/harness so
*their* agent can use the `docker-agent` CLI as an external tool. This file
itself lives one level up (`skill/AGENTS.md`, not `skill/docker-agent/`)
precisely so the whole `skill/docker-agent/` directory is the payload with
nothing to exclude at install time. A wrong line in the payload misleads
every agent that installs it. This file is addressed to whoever — human or
agent — edits the skill; it is never itself installed.

## The skill is for USING docker-agent, not for developing it

This is the mistake v1 shipped with and had to be corrected after review
(see task `65afbc73-1838-44b6-8485-02e3ecf5d52e`'s history): the bundle's
source briefly lived at `.agents/skills/docker-agent/` in this repo, the
same path/namespace as this repo's own hand-written *dev* skills (e.g.
`bump-config-version`, for people contributing to docker-agent's own Go
code). That both reads as "how to develop docker-agent" and is only ever
self-discovered when docker-agent runs from inside its own source tree —
irrelevant to the actual goal, which is an end user in some *other* project
installing this so their agent can delegate to / operate `docker-agent` as
an external tool (e.g. `docker-agent run <config> --exec` to run a
sub-agent, or `serve mcp`/`serve a2a` to expose one for delegation).

Concretely, this means:
- **Never reference a repo-relative path** (`examples/foo.yaml`, `docs/...`,
  `pkg/...`) in anything under `docker-agent/` — the installed reader has no
  such directory. Use self-contained inline YAML in `references/examples.md`
  and public URLs (`https://docs.docker.com/...`,
  `https://raw.githubusercontent.com/docker/docker-agent/main/...`) for
  everything else. Repo-relative paths are fine in *this* file, since it is
  never installed.
- **Never frame a recipe as if the reader is inside the docker-agent repo.**
  Every recipe should read as "you are an agent in some arbitrary project;
  `docker-agent` is available to you as a tool", not "see the example under
  `examples/`".
- **Delegation is the flagship use case, not an afterthought.** Keep
  SKILL.md's "Delegating to another agent" section prominent and keep at
  least one full worked delegation flow in `references/examples.md`.

## Never document what you have not observed

Help text and doc comments state intent and drift from behavior. Before
writing a claim about a command, flag, or config field, run it (or read the
struct/schema definition directly) rather than paraphrasing a `--help`
string or a doc page from memory. Corrections made while writing v1 came
from doing exactly this: omitting `version:` targets the *latest* schema
(verified via `debug config` on a version-less file — `pkg/config/
config.go`'s `cmp.Or(raw.Version, latest.Version)`), not an old pinned
version; `--yolo` does exist on `run` (it was just past an arbitrary `head`
cutoff during a first pass); `__askpass` is a `sudo_askpass` /
`SUDO_ASKPASS` bridge for the `shell` toolset, not a git-credential helper
and not sandbox-exclusive (`cmd/root/askpass.go`); and `fallback` is an
`AgentConfig` field (automatic failover/retry), not a `ModelConfig` field —
don't group it with `title_model`/`compaction_model` as if it were one
(`agent-schema.json`'s `definitions.AgentConfig` vs `definitions.ModelConfig`).
The same applies to eval fixtures: they are recorded-session JSON files
captured via the TUI's `/eval` slash command (`docs/features/evaluation/`),
**not** the cassette file `run --record` produces — those are two different
formats for two different purposes; don't conflate them.

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

Target `docker-agent/SKILL.md`'s body under ~500 lines / ~5,000 tokens
(currently ~160 lines); push anything exhaustive into `references/`, one
level deep. SKILL.md should read as judgment and workflow selection, not a
flag dump.

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
