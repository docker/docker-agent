# Renders the JSON delta produced by models-delta.jq to markdown.
#
# Invoked as:
#   jq -r -f models-delta-render.jq delta.json
#
# Note: cost fields use `omitempty` in the Go schema that produces
# snapshot.json, so a model with genuinely free ($0) pricing is
# serialized identically to one with no cost data at all — both come
# through as null here. Rendering both as "–" matches the source data;
# there is no field-level way to tell them apart.
def mtok: if . == null or . == 0 then "–" else "$" + (.|tostring) end;
def ctx:
  if . == null or . == 0 then "–"
  elif . < 1000 then (.|tostring)
  else ((./1000)|floor|tostring) + "k"
  end;
def caps:
  [
    (if .reasoning then "reasoning" else empty end),
    (if .tool_call then "tools" else empty end),
    (if .attachment then "attachments" else empty end),
    (if .open_weights then "open-weights" else empty end)
  ] | if length == 0 then "–" else join(", ") end;

# id/name/field-value strings come straight from models.dev — untrusted
# third-party text that ends up in a PR body an automated review
# pipeline consumes downstream. Collapse every such leaf to a short,
# bounded, plain-ASCII-ish string before it's ever emitted: no control
# characters, no HTML, no unbounded length. This also makes esc_cell/
# esc_code defensive against unexpected non-string input, since
# `tostring` runs first — a malformed field can no longer abort the
# whole render with a jq type error.
def safe:
  tostring
  | gsub("[^A-Za-z0-9 ._/+:-]"; "?")
  | .[0:80];

# Narrower variant for `tojson`-rendered structured diffs (cost/limit
# before/after). tojson already escapes control characters and quotes,
# so the only remaining markdown hazards are backtick and pipe — using
# `safe`'s full allowlist here would needlessly mangle otherwise-legible
# JSON punctuation for no extra safety.
def safe_diff:
  tostring
  | gsub("[|`]"; "?")
  | .[0:80];

# Markdown-structural escaping, applied after `safe`. Kept as a
# separate, explicit step even though `safe`'s charset already
# excludes `|` and backtick, so the intent (don't let a table row or
# code span break) stays obvious at each call site.
def esc_cell: gsub("\\|"; "\\|");
def esc_code: gsub("`"; "'");

def name_or_dash: if .model.name then (.model.name | safe | esc_cell) else "–" end;

def row:
  "| `\(.id | safe | esc_cell | esc_code)` | \(name_or_dash) | \(.model.limit.context | ctx) | "
  + "\(.model.cost.input | mtok)/\(.model.cost.output | mtok) | \(.model | caps) |";

"## Added (\(.added|length))\n"
+ (if (.added|length) == 0 then "_none_\n" else
    "\n| id | name | context | in/out per Mtok | capabilities |\n|---|---|---|---|---|\n"
    + ([.added[] | row] | join("\n")) + "\n" end)
+ "\n## Removed (\(.removed|length))\n"
+ (if (.removed|length) == 0 then "_none_\n" else
    "\n| id | name | context | in/out per Mtok | capabilities |\n|---|---|---|---|---|\n"
    + ([.removed[] | row] | join("\n")) + "\n" end)
+ "\n## Changed (\(.changed|length))\n"
+ (if (.changed|length) == 0 then "_none_\n" else
    "\n" + ([.changed[] | "- `\(.id | safe | esc_cell | esc_code)`\n"
      + ([.fields[] | "  - **\(.field | safe)**: `\((.before|tojson) | safe_diff)` → `\((.after|tojson) | safe_diff)`"] | join("\n"))]
      | join("\n")) + "\n" end)
