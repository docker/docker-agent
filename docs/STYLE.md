# Documentation style guide

A short reference for writers and reviewers. Goal: keep voice, naming
and examples consistent across every page on this site.

## Product naming

| Context | Use | Don't use |
|---|---|---|
| Prose, headings, marketing | **Docker Agent** (two words, both capitalised — the proper name of the product) | docker-agent, Docker-Agent, docker agent (in prose) |
| The CLI command | `docker agent` (lower-case, two words, in monospace) | `docker-agent`, `Docker Agent run` |
| The repository / module path | `docker/docker-agent` | docker/Docker-Agent |
| Internal identifiers / package names | as defined in code (e.g. `cagent`) — never invent new spellings in prose | mixing internal identifiers into user-facing copy |

A simple rule of thumb:

- **Talking about the product?** → "Docker Agent"
- **Showing a command the user types?** → `docker agent run agent.yaml`

## Voice

- Address the reader as **you**, not "we" or "the user".
- Prefer present tense and active voice ("the agent reads files",
  not "files will be read by the agent").
- Keep sentences short. Two short sentences usually beat one compound
  one.
- Avoid "simply", "just", "easily" — they're rarely accurate and
  often condescending.

## Code samples

- All shell prompts use a dollar sign followed by a space (`$`) and the
  command on the same line. Output, when shown, has no prompt.
- YAML/HCL examples should be runnable as-is when reasonable, or end
  in `# ...` to make truncation explicit.
- The canonical example agent uses `model: anthropic/claude-sonnet-4-5`.
  Use a different model only when the example is *about* that model.
- File names in prose are in `monospace` (`agent.yaml`, not "agent.yaml").

## Callouts

Callouts are written as portable GitHub-style alerts so the same
Markdown renders correctly on docs.docker.com (Hugo), GitHub, and this
site (a blockquote render hook upgrades them to the styled panels):

```markdown
> [!TIP]
> **When to use it**
>
> Body text.
```

- `[!NOTE]` — neutral context
- `[!TIP]` — positive, "consider this"
- `[!IMPORTANT]` — must-read to succeed
- `[!WARNING]` — caution, breaking, security

The bold line directly after the marker is an optional title; omit it
when the default label (Note, Tip, …) is enough. Don't prefix the
title with an emoji — the icon badge already provides one.

## Links

Internal links are plain relative Markdown paths to the target file,
including the `index.md` filename:

```markdown
See the [Quick Start](../../getting-started/quickstart/index.md).
```

Both this site and docs.docker.com render them through a Hugo link
render hook that resolves the path to the target page's URL. Never
use absolute `/path/` links in `docs/**` content — they break when
the page is mounted on docs.docker.com.

## Canonical URLs

Every content page is mounted on docs.docker.com, and this site
tracks `main` while docs.docker.com is pinned to a release — so the
stable page is the authoritative copy. Each
`docs/<section>/<page>/index.md` sets `canonical:` in its front
matter (after `weight:`), derived from its path:

```yaml
canonical: https://docs.docker.com/ai/docker-agent/<section>/<page>/
```

The github.io layout renders it as the page's `rel=canonical` link;
docs.docker.com ignores the value and self-canonicalizes. CI
(`docs / canonical`, `scripts/docs-check-canonical.sh`) fails when the
value is missing or doesn't match the page path — mind it when
scaffolding a new page from an existing one. The homepage, `404.md`
and section `_index.md` files are not mirrored pages and don't set
one.

## llms.txt

`/llms.txt` is generated at build time from `data/nav.yml` (see
`layouts/home.llms.txt`): adding a page to the nav automatically adds
it to llms.txt, and pages absent from `nav.yml` never appear there.
Every nav entry must resolve to a real page with a non-empty
`description:` in its front matter (whitespace-only counts as empty)
— the build fails with an `errorf` naming the offending title/url
otherwise, since the spec's `- [title](url): note` shape requires a
note. CI (`docs / llms-txt`, `scripts/docs-check-llms-txt.sh`) additionally
rebuilds the site and asserts the generated `llms.txt` matches
`nav.yml`'s sections, titles, order, count **and per-entry URL**
(each rendered link must match the nav url at the same position, not
just share the site's base URL), catching regressions (e.g. a broken
`groups[].items` traversal, or every entry rendering the same link)
that a successful build alone would not.

## Availability badges

When a page documents a feature that is merged on `main` but not yet
in a tagged release, mark it so readers of the stable docs know what
to expect:

```markdown
> [!NOTE]
> **Coming in v1.99**
>
> This feature is available on `main` and ships in v1.99.
```

Remove the badge in the release PR that tags the version (the
CHANGELOG update is a good reminder to sweep for `Coming in` markers).

## Glossary one-liners

When a page first introduces a term, link to its concept page or use
one of these standard one-liners:

- **Agent** — an LLM with instructions, tools, and (optionally)
  sub-agents, defined in YAML or HCL.
- **Toolset** — a group of related tools the agent can call (e.g.
  `filesystem`, `shell`, `mcp`).
- **MCP** — Model Context Protocol, an open standard for tool servers.
- **A2A** — Agent-to-Agent protocol, used to talk to other agents
  over HTTP.
- **TUI** — Terminal User Interface, the default interactive front end
  Docker Agent ships with.
- **OCI** — Open Container Initiative; the same registry format used
  for Docker images. Docker Agent reuses it for sharing agents.

## Accessibility (site chrome: `css/style.css`, `js/app.js`, `layouts/`)

The github.io layout targets WCAG 2.1 AA in both themes. When touching
tokens, templates, or JS, keep these conventions:

- **Never de-emphasize text with `opacity`.** It multiplies through to
  contrast against the background and silently drops already-muted
  text below AA (this bit `.sidebar-subheading`). Use a token with a
  real, computed contrast ratio instead — the muted-but-legible look
  comes from choosing the right gray, not from fading a legible one.
- **Recompute contrast for both themes whenever a shared token or rule
  changes.** Dark is `:root`'s default; light is the `[data-theme="light"]`
  override block. A value that passes on one background can fail badly
  on the other — see the token table below.
- **The same token can pass or fail depending on *which* surface it
  sits on.** `--text-muted`'s dark value (gray-400) reads a comfortable
  6.37:1 against the page background (`--bg-dark`), but only 4.42:1
  against the lighter `--bg-hover` gray-800 used by `#search-input` —
  a marginal AA fail nobody caught until a component-level scan ran.
  Don't assume a token that's fine for body text is fine on every
  background it's ever applied to; check the *actual* background of
  the specific element.
- **Watch for a more-specific rule shadowing a component's own color.**
  `.content a` (specificity of a class + a type selector) silently
  overrode `.btn-primary`/`.btn-secondary`'s own `color` for any button
  rendered as a link inside `.content` — invisible in light mode only
  because `--accent` and `--blue-500` happened to be the same hex
  there, but a real 2.55:1 fail in dark. Fixed by excluding button
  links from the prose-link rule (`.content a:not(.btn)`) rather than
  trying to out-specificity it per button.
- **The same shadowing bug recurred for `:hover`.** `.content
  a:not(.btn):hover` has higher specificity than component link rules
  like `.preview-banner a` and `.usecase a`, so it — not the
  component's own color — decided the hover color for card links too.
  It also hardcoded a single blue (`--docker-blue-light`, blue-400)
  for both themes, which reads only 4.08:1 on a dark `--bg-card` and
  3.94:1 on white — both AA fails, while the default (non-hover)
  states passed. Fixed with a dedicated `--link-hover` token, themed
  like the other accent-* tokens (blue-200 in dark, blue-600 in
  light) and verified against `--bg-card` specifically, since it's
  lighter than `--bg-content` in dark and so the harder case to pass.
- **Inline `style="...var(--accent)..."` on a content page** (e.g.
  `404.md`'s "Back to Docs" button) should reach for `--accent-strong`
  (a fixed `--blue-500`, identical in both themes, 5.0:1 for white
  text) rather than the theme-varying `--accent` when the other color
  in the pair is hardcoded (here, white) — `--accent`'s dark value
  (blue-400) only reads 3.94:1 against white.
- **`--bg-content`** is the content column's background (`.main`):
  white in light mode, the same `--bg-dark` as the page in dark mode
  (so dark is unaffected). Header/sidebar/footer chrome stays on
  `--bg`/`--bg-sidebar` (gray-100 in light) — don't apply `--bg-content`
  to chrome elements, or the light chrome-vs-content contrast is lost.
- **`--accent-on-card`** is for brand-blue text sitting on `--bg-card`
  (elevator labels, ecosystem tile headings, usecase links, glossary
  terms): blue-300 in dark (blue-400 only reads 4.08:1 on the dark
  card), blue-500 in light (already 5.0:1 there). Use it instead of
  the bare `--accent` for any *new* text-on-card component.
- **Links inside a callout need their own color, not the prose-link
  one.** Callout panels (warning/info/tip) sit on their own
  variant-colored background, not the page background, so
  `--accent-hover` (the dark prose-link color) can fail there even
  where it's fine everywhere else — it read only 3.95:1 on the dark
  warning callout (`#643700`). `.content .callout a:not(.btn)` scopes
  `--blue-200` to callout links in dark (6.46:1 warning / 11.50:1 info /
  8.51:1 tip); light already passed with `--accent` (4.60–4.83:1) and
  is restored explicitly rather than left to inherit the dark value.
  Because that selector's specificity ties with `.content
  a:not(.btn):hover`'s, the hover state needed its own explicit
  `.content .callout a:not(.btn):hover` rule too — an unconditional,
  equal-specificity selector coming later in the stylesheet otherwise
  wins over a `:hover` one regardless of hover state, flattening the
  hover color. **The same shadowing hit the light override too**: the
  `[data-theme="light"]` default sits after the shared `:hover` rule,
  so light hover silently fell back to `--accent` instead of
  `--link-hover` (still AA — 4.60–4.83:1 — but not the intended token).
  Fixed with a `[data-theme="light"] ... :hover` rule of its own, right
  after the light default. Same lesson as the specificity bullets
  above: always check what a new equal-or-higher-specificity rule
  shadows nearby — including rules added later for the *other* theme.
- **SVGs embedded via `<img>` can't inherit `currentColor` or CSS
  custom properties from the page**, so a single hardcoded palette has
  to clear AA against *both* a near-black dark card (`#1E2129`) and a
  white light card at once — a neutral gray tops out around ~4:1
  against both simultaneously, which fails 4.5:1 normal-text AA in
  both themes. `assets/how-it-works.svg` used to ship exactly that
  compromise palette (a mistake, not a real fix — see git history);
  it's now two theme-specific files, `how-it-works.svg` (light: gray-600
  body text 5.88:1, blue-500 accent 5.00:1 — both on white) and
  `how-it-works-dark.svg` (dark: gray-400 body text 5.59:1, blue-300
  accent 5.89:1 — both on `#1E2129`), toggled purely with CSS
  (`.flow-diagram img.theme-light-only` / `.theme-dark-only`, hidden
  with `display: none` so only one is ever in the accessibility tree).
  Prefer this per-theme-variant approach — or inlining the SVG in the
  Markdown so CSS custom properties/`currentColor` apply — over a
  single compromise palette for any new diagram; a midpoint gray
  cannot pass both surfaces per WCAG 1.4.3.
- **Watch CSS specificity when a rule sets `color` on `code`/`pre`
  elements.** A same-specificity, theme-unconditional rule that comes
  *later* in the stylesheet silently wins over an earlier
  `[data-theme="light"]` override in both themes — this is exactly how
  the old "Rouge / highlighter-rouge overrides" block broke every
  untyped Chroma syntax token (`.l`, `.sd`, `.cl`, ...) in light mode
  (1.47:1) despite the correctly-themed rule sitting right above it.
  Caught by the `pa11y-ci` gate below — when in doubt, let the scanner
  decide rather than eyeballing the cascade.
- **Grid/flex items default to `min-width: auto`**, which sizes them to
  their content's intrinsic (min-content) width — long unbreakable
  content (e.g. code in `.compare-side`) then refuses to shrink with
  its column, forcing page-level horizontal scroll at narrow viewports
  (WCAG 1.4.10 reflow) even though the code block itself has its own
  `overflow-x: auto`. `min-width: 0` on the grid/flex item fixes this;
  scanners like `pa11y-ci` can't catch it since it only shows up as an
  actual viewport/zoom measurement, not a static HTML/CSS violation —
  verify with a real narrow-viewport check (see the checklist below).

### Contrast reference (light / dark, WCAG AA: 4.5:1 normal text, 3:1 large text / non-text UI)

| Token / use | Light | Dark |
|---|---|---|
| `--text-muted` on chrome (`--bg`) / content (`--bg-content`) | gray-600, 4.87:1 / 5.88:1 | gray-400, 6.37:1 |
| Tiny uppercase labels (sidebar/footer/page-nav headings) | gray-700, 7.14:1 / 8.61:1 | gray-100 (unchanged) |
| `--accent` on `.content a` | blue-500, 5.00:1 (on white) | blue-400 + `--accent-hover` blue, 7.2:1 |
| `--link-hover` on `.content a:not(.btn):hover` (prose & card links) | blue-600, 6.76:1 (white content and white cards) | blue-200, 11.78:1 (page) / 10.34:1 (`--bg-card`, the harder case) — was `--docker-blue-light` blue-400, 3.94:1 / 4.08:1, both AA fails |
| `--accent-on-card` | blue-500, 5.0:1 | blue-300, 5.89:1 |
| Dark syntax comments | n/a | gray-400, 5.59:1 |
| `kbd` text | `--text-muted` (as above) | gray-300, 6.07:1 |
| `.copy-btn` text | gray-600, 5.52:1 | gray-400, 5.59:1 |
| `.pain-x` badge (white text) | `--error-strong` `#DC2626`, 4.83:1 | same |
| `#search-input` (search trigger button) text | `--text-muted` (gray-600), 5.88:1 on `--bg-hover` white | gray-300, 6.07:1 on `--bg-hover` gray-800 (was `--text-muted` gray-400, 4.42:1 — fail) |
| `.preview-banner a` | `--accent-on-card` (unchanged, blue-500 5.0:1) | `--accent-on-card` blue-300, 5.89:1 (was `--accent` blue-400, 4.08:1 — fail) |
| `.content .callout a:not(.btn)` (links inside a callout) | `--accent`, 4.60–4.83:1 (unchanged) | `--blue-200`, 6.46:1 warning / 11.50:1 info / 8.51:1 tip (was `--accent-hover` blue-400, 3.95:1 on warning — fail) |
| `.content .callout a:not(.btn):hover` | `--link-hover` blue-600, 6.21–6.52:1 (was silently `--accent`, same as default — not a fail, but not the intended token) | `--link-hover` blue-200 (unchanged, same token/ratios as the shared prose-link hover above) |
| `.btn-primary`/`.btn-secondary` as `.content a` links (Quick Start, Back to Docs) | unchanged (blue-500-on-white 5.00:1 / white-on-translucent) | own `color` restored via `.content a:not(.btn)`, was 2.55:1 (`--accent-hover` leaking onto a white button bg) |
| `how-it-works.svg` diagram text | gray-600 body 5.88:1 / blue-500 accent 5.00:1 (light-only file, on white) | gray-400 body 5.59:1 / blue-300 accent 5.89:1 (dark-only file, on `#1E2129`) |

### Motion

Every animation/transition (fade-in, hover transforms, sidebar slide,
smooth scroll) is disabled under `@media (prefers-reduced-motion: reduce)`
in `style.css`; `js/app.js`'s TOC scroll gates `scrollIntoView`'s
`behavior` on the same query. Animated media (the homepage demo) must
stay pausable/stoppable (WCAG 2.2.2) — it's a `<video controls>`, not
an auto-looping GIF with no user control.

The `<video>`'s `width`/`height` attributes must match the encoded
media's true aspect ratio (currently 1200×960, 5:4 — `demo.mp4` is
2000×1600). A mismatched ratio there causes visible stretching once
`.demo-container video { width: 100%; height: auto }` scales it, since
that rule preserves whatever ratio the attributes declare. The nested
`<img src="demo.gif">` inside `<video>` is HTML fallback *content*:
browsers only render it when they don't recognize the `<video>`
element at all, not on a network/decode failure of a supported one —
so it stays as-is for that genuine no-`<video>`-support case. A
decode/fetch failure of a *supported* `<video>` is a different,
far more common path, and must not fall back to that same auto-playing,
unpausable, infinitely-looping GIF — doing so would reintroduce
exactly the WCAG 2.2.2 issue the video swap was meant to fix.
`js/app.js`'s `handleVideoFallback()` instead swaps in the static
`demo-poster.png` frame on the video's `error` event: a still image
needs no pause control, so it's always a safe fallback regardless of
*why* playback failed.

### Manual verification checklist (JS-driven interactions)

This site has no JS test harness or `package.json` — it's a
dependency-free static Hugo build, and the only JS tooling in CI is an
ephemeral `npx pa11y-ci` scan of rendered HTML. `pa11y-ci` (or any
static scanner) can't drive keyboard/pointer interactions, so it can't
verify the search dialog's focus trap, the theme toggle, the sidebar
toggle, reduced-motion behavior, or responsive media. Adding a full
browser-test harness (e.g. Playwright) for a handful of interaction
checks on a static docs site would be disproportionate tooling for
what this repo otherwise carries — so until that changes, verify these
manually (in a real browser, both themes) before merging any change
that touches `js/app.js`, `layouts/`, or the demo media:

1. **Search dialog focus trap**: open search (click the trigger or
   press <kbd>⌘K</kbd>/<kbd>Ctrl</kbd>+<kbd>K</kbd>), then press
   <kbd>Tab</kbd> repeatedly — focus must cycle within the modal and
   never reach the page behind it. <kbd>Shift</kbd>+<kbd>Tab</kbd>
   from the input must wrap to the last result, not escape the modal.
2. **Search focus restore**: close the dialog (<kbd>Esc</kbd>, or
   click outside) — focus must return to whatever element opened it
   (the header trigger, or wherever <kbd>⌘K</kbd> was pressed from).
3. **Search arrow navigation**: with results showing, press
   <kbd>↓</kbd>/<kbd>↑</kbd> to move between the query input and each
   result link; <kbd>Enter</kbd> follows the focused result.
4. **Theme toggle**: click it — the page recolors instantly, the
   button's `aria-pressed` and `aria-label` update (inspect via DevTools
   or a screen reader), and the choice survives a reload (localStorage).
5. **Sidebar toggle** (≤1024px viewport): click it — the sidebar
   slides in/out, `aria-expanded` on the toggle matches the state, and
   a closed sidebar's links are not `Tab`-reachable.
6. **Reduced motion**: enable the OS "reduce motion" setting, reload —
   the content fade-in, hover transforms, and sidebar slide should be
   instant (no animation), and TOC link clicks should jump instead of
   smooth-scrolling.
7. **Responsive media**: resize the viewport down to ~320–375px on the
   homepage — the demo video must scale to the full column width at
   its true aspect ratio (no stretching, cropping, or overflow), and
   the how-it-works diagram must swap cleanly between the two
   theme-specific SVGs with the theme toggle.
8. **Video-error fallback**: force the `<video>` element's `error`
   event (e.g. temporarily point its `src` at a missing file, or block
   `demo.mp4` in DevTools' Network tab and reload) — it must be
   replaced by the static `demo-poster.png` image, never by the
   looping `demo.gif` (WCAG 2.2.2: a decode/fetch failure must not
   reintroduce uncontrolled auto-playing motion).
9. **Link hover contrast**: hover a prose link and a card link (e.g.
   `.usecase a` or `.preview-banner a`) in both themes — the hover
   color must stay clearly readable (this is `--link-hover`; verify
   with a contrast checker if unsure, target ≥4.5:1) and must not
   regress to the flat blue that used to fail on white / dark cards.
10. **Narrow-viewport reflow**: at a 320–360px viewport, load the
    homepage and confirm the *page* never scrolls horizontally
    (`document.documentElement.scrollWidth` must equal `clientWidth`);
    only the `.compare-side` code blocks should scroll horizontally,
    and only within their own box.

### Running the accessibility scan locally

CI (`docs-a11y` workflow) runs [pa11y-ci](https://github.com/pa11y/pa11y-ci)
at the `WCAG2AA` standard against a two-tier URL list:

- **Tier 1 (always)** — a small static set of layout-archetype pages
  in `docs/.pa11yci.json`: home (`/`), `configuration/sandbox/`
  (wide tables + callouts), `getting-started/quickstart/` (tutorial
  prose), `404.html` (standalone layout), `features/tui/` (the
  densest page — images, inline HTML, many callouts/tables), and
  `concepts/multi-agent/` (deep heading structure, tables). Each is
  listed twice, once with `?theme=light` and once with `?theme=dark`
  — 12 URLs in total. This tier guards against systemic regressions
  (shared CSS/JS/templates/SVG) and runs on every push and PR.
- **Tier 2 (pull requests only)** — the content pages the PR actually
  modifies. `scripts/docs-a11y-urls.sh` maps changed Markdown to
  rendered URLs (dropping `_index.md`/`STYLE.md`/non-content files,
  de-duping against the Tier 1 list, live-probing to catch renames,
  and emitting both themes), deterministically capped at
  `${A11Y_MAX_PAGES:-10}` changed pages (lexicographic sort, so a
  PR touching more than that always scans the same subset and logs
  the rest as skipped). The CI workflow jq-merges these URLs into a
  generated config on top of `docs/.pa11yci.json`; a push to `main`
  or a PR that touches no content Markdown stays static-only (Tier 1).

`js/app.js`'s `initTheme()` honors the `?theme=` query param ahead of
`localStorage`/`prefers-color-scheme`, so the gate deterministically
exercises both themes regardless of the scanning browser's default
color scheme (observed to default to light headless, which is exactly
why earlier local runs only ever caught light-mode regressions).

To reproduce the full two-tier scan locally:

```bash
# Serve the site the same way the Dockerfile does
docker build -t docs docs/
docker run --rm -p 1313:1313 -v $(pwd)/docs:/src docs

# In another terminal, once it's serving at http://localhost:1313/docker-agent/:

# Tier 1 only (the static archetype list):
cd docs && npx --yes pa11y-ci@3.1.0 --config .pa11yci.json

# Tier 1 + Tier 2 (mirrors what a PR's CI run scans), from the repo root:
git diff --name-only main...HEAD | A11Y_BASE_URL=http://localhost:1313 ./scripts/docs-a11y-urls.sh > /tmp/changed-urls.txt
jq -Rn '[inputs]' /tmp/changed-urls.txt > /tmp/changed-urls.json
jq --slurpfile extra /tmp/changed-urls.json '.urls += $extra[0]' docs/.pa11yci.json > /tmp/pa11yci.generated.json
npx --yes pa11y-ci@3.1.0 --config /tmp/pa11yci.generated.json
```

`scripts/docs-a11y-urls.sh --self-test` exercises the mapping rules
against fixtures (no server needed) and is safe to run any time.

**Adding or retiring a Tier 1 archetype:** edit the `urls` array in
`docs/.pa11yci.json` (both the light and dark entries — never add
or remove just one theme). Before adding a page, run the full local
scan above with it included and confirm 0 errors in both themes
first (the "green-baseline" rule) — a latent shared-CSS bug that
happens to be invisible on the current six pages must be fixed
*before* the new archetype joins the always-on gate, or every future
PR starts failing on a pre-existing issue unrelated to its own change.

**Tuning the Tier-2 cap:** `A11Y_MAX_PAGES` (default 10) is the only
knob; raise it in the workflow's `Compute changed-page URLs` step if
PRs regularly touch more content pages than that.

`pa11y-ci` only fails the build on HTML_CodeSniffer `error`-level
results (warnings/notices are informational and don't fail CI), so a
local failure here is a real regression worth fixing before pushing.

pa11y-ci@3.1.0 bundles an old Puppeteer that doesn't download its own
Chromium under `npx --yes` ("Could not find expected browser (chrome)
locally"), including on the CI runner — that's why the `docs-a11y`
workflow installs a pinned Chrome (`browser-actions/setup-chrome`) and
points Puppeteer at it via `PUPPETEER_EXECUTABLE_PATH`, per [pa11y-ci's
guidance for Ubuntu > 20.04](https://github.com/pa11y/pa11y-ci#pa11y-ci-fails-with-couldnotfindchromium-or-similar-error).
Reproduce that locally by pointing the same env var at your own Chrome
or Chromium install before running the command above, e.g.:

```bash
export PUPPETEER_EXECUTABLE_PATH="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" # macOS
# or: export PUPPETEER_EXECUTABLE_PATH="$(command -v google-chrome || command -v chromium)" # Linux
```
