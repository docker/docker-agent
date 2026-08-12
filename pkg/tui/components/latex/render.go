// Package latex renders supported LaTeX math expressions as terminal-friendly Unicode text.
package latex

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	runewidth "github.com/mattn/go-runewidth"
)

var symbols = map[string]string{
	"alpha": "α", "beta": "β", "gamma": "γ", "delta": "δ", "epsilon": "ϵ", "varepsilon": "ε", "zeta": "ζ", "eta": "η", "theta": "θ", "vartheta": "ϑ", "iota": "ι", "kappa": "κ", "lambda": "λ", "mu": "μ", "nu": "ν", "xi": "ξ", "pi": "π", "rho": "ρ", "sigma": "σ", "tau": "τ", "upsilon": "υ", "phi": "ϕ", "varphi": "φ", "chi": "χ", "psi": "ψ", "omega": "ω",
	"Gamma": "Γ", "Delta": "Δ", "Theta": "Θ", "Lambda": "Λ", "Xi": "Ξ", "Pi": "Π", "Sigma": "Σ", "Phi": "Φ", "Psi": "Ψ", "Omega": "Ω",
	"pm": "±", "mp": "∓", "times": "×", "div": "÷", "cdot": "·", "ast": "∗", "star": "⋆", "circ": "∘", "bullet": "•", "oplus": "⊕", "ominus": "⊖", "otimes": "⊗", "oslash": "⊘", "odot": "⊙",
	"cap": "∩", "cup": "∪", "bigcap": "⋂", "bigcup": "⋃", "bigwedge": "⋀", "bigvee": "⋁", "setminus": "∖", "in": "∈", "notin": "∉", "ni": "∋", "subset": "⊂", "supset": "⊃", "subseteq": "⊆", "supseteq": "⊇",
	"le": "≤", "leq": "≤", "leqslant": "≤", "ge": "≥", "geq": "≥", "geqslant": "≥", "ne": "≠", "neq": "≠", "equiv": "≡", "approx": "≈", "sim": "∼", "simeq": "≃", "cong": "≅", "propto": "∝", "parallel": "∥", "perp": "⊥", "mid": "∣",
	"forall": "∀", "exists": "∃", "nexists": "∄", "neg": "¬", "land": "∧", "wedge": "∧", "lor": "∨", "vee": "∨", "to": "→", "rightarrow": "→", "longrightarrow": "→", "leftarrow": "←", "longleftarrow": "←", "gets": "←", "leftrightarrow": "↔", "mapsto": "↦", "Rightarrow": "⇒", "Leftarrow": "⇐", "Leftrightarrow": "⇔", "implies": "⇒", "iff": "⇔",
	"partial": "∂", "nabla": "∇", "int": "∫", "iint": "∬", "iiint": "∭", "oint": "∮", "sum": "∑", "prod": "∏", "coprod": "∐", "infty": "∞", "emptyset": "∅", "varnothing": "∅", "angle": "∠", "therefore": "∴", "because": "∵", "aleph": "ℵ", "hbar": "ℏ", "ell": "ℓ",
	"ldots": "…", "dots": "…", "cdots": "⋯", "vdots": "⋮", "ddots": "⋱", "langle": "⟨", "rangle": "⟩", "vert": "|", "lvert": "|", "rvert": "|", "Vert": "‖", "lVert": "‖", "rVert": "‖", "lbrace": "{", "rbrace": "}", "lfloor": "⌊", "rfloor": "⌋", "lceil": "⌈", "rceil": "⌉", "prime": "′",
}

var (
	relations = set("le leq leqslant ge geq geqslant ne neq equiv approx sim simeq cong propto parallel perp mid in notin ni subset supset subseteq supseteq to rightarrow longrightarrow leftarrow longleftarrow gets leftrightarrow mapsto Rightarrow Leftarrow Leftrightarrow implies iff")
	operators = set("arccos arcsin arctan arg cos cosh cot coth csc deg det dim exp gcd hom inf ker lg lim liminf limsup ln log max min Pr sec sin sinh sup tan tanh")
	wrappers  = set("emph mathcal mathbf mathfrak mathit mathrm mathnormal mathscr mathsf mathtt mathup mbox overbrace pmb smash substack text textbf textit textmd textnormal textrm textsc textsf textsl texttt textup underbrace bm boldsymbol")
	spacing   = set(", : ; space > enspace enskip medspace quad qquad thickspace thinspace")
	ignored   = set("displaystyle limits nolimits scriptstyle scriptscriptstyle textstyle big Big bigg Bigg bigl Bigl biggl Biggl bigr Bigr biggr Biggr")
)

var (
	superscripts = map[rune]rune{'0': '⁰', '1': '¹', '2': '²', '3': '³', '4': '⁴', '5': '⁵', '6': '⁶', '7': '⁷', '8': '⁸', '9': '⁹', '+': '⁺', '-': '⁻', '=': '⁼', '(': '⁽', ')': '⁾', 'a': 'ᵃ', 'b': 'ᵇ', 'c': 'ᶜ', 'd': 'ᵈ', 'e': 'ᵉ', 'f': 'ᶠ', 'g': 'ᵍ', 'h': 'ʰ', 'i': 'ⁱ', 'j': 'ʲ', 'k': 'ᵏ', 'l': 'ˡ', 'm': 'ᵐ', 'n': 'ⁿ', 'o': 'ᵒ', 'p': 'ᵖ', 'r': 'ʳ', 's': 'ˢ', 't': 'ᵗ', 'u': 'ᵘ', 'v': 'ᵛ', 'w': 'ʷ', 'x': 'ˣ', 'y': 'ʸ', 'z': 'ᶻ'}
	subscripts   = map[rune]rune{'0': '₀', '1': '₁', '2': '₂', '3': '₃', '4': '₄', '5': '₅', '6': '₆', '7': '₇', '8': '₈', '9': '₉', '+': '₊', '-': '₋', '=': '₌', '(': '₍', ')': '₎', 'a': 'ₐ', 'e': 'ₑ', 'h': 'ₕ', 'i': 'ᵢ', 'j': 'ⱼ', 'k': 'ₖ', 'l': 'ₗ', 'm': 'ₘ', 'n': 'ₙ', 'o': 'ₒ', 'p': 'ₚ', 'r': 'ᵣ', 's': 'ₛ', 't': 'ₜ', 'u': 'ᵤ', 'v': 'ᵥ', 'x': 'ₓ'}
	blackboard   = map[rune]rune{'C': 'ℂ', 'H': 'ℍ', 'N': 'ℕ', 'P': 'ℙ', 'Q': 'ℚ', 'R': 'ℝ', 'Z': 'ℤ'}
)

func set(values string) map[string]bool {
	r := map[string]bool{}
	for v := range strings.FieldsSeq(values) {
		r[v] = true
	}
	return r
}

type parser struct {
	source  string
	pos     int
	ok      bool
	display bool
}

// Render renders source, returning false for unsupported or malformed syntax.
func Render(source string, display bool) (string, bool) {
	p := parser{source: source, ok: true, display: display}
	result := p.sequence(0)
	if !p.ok || p.pos != len(source) {
		return "", false
	}
	return strings.ReplaceAll(normalize(result), "\uf002", " "), true
}

func (p *parser) sequence(end byte) string {
	var out strings.Builder
	for p.pos < len(p.source) {
		c := p.source[p.pos]
		if end != 0 && c == end {
			p.pos++
			return out.String()
		}
		switch c {
		case '}':
			p.ok = false
			return out.String()
		case '{':
			p.pos++
			out.WriteString(p.sequence('}'))
		case '\\':
			out.WriteString(p.command())
		case '^', '_':
			p.pos++
			value := p.argument()
			current := strings.TrimRightFunc(out.String(), unicode.IsSpace)
			out.Reset()
			out.WriteString(current)
			out.WriteString(script(value, c == '_'))
		case '=', '<', '>':
			out.WriteByte(' ')
			out.WriteByte(c)
			out.WriteByte(' ')
			p.pos++
		case '&':
			p.pos++
		case '~':
			out.WriteByte(' ')
			p.pos++
		default:
			if unicode.IsSpace(rune(c)) {
				for p.pos < len(p.source) && unicode.IsSpace(rune(p.source[p.pos])) {
					p.pos++
				}
				out.WriteByte(' ')
			} else {
				out.WriteByte(c)
				p.pos++
			}
		}
	}
	if end != 0 {
		p.ok = false
	}
	return out.String()
}

func (p *parser) command() string {
	p.pos++
	if p.pos >= len(p.source) {
		p.ok = false
		return ""
	}
	start := p.pos
	if isLetter(p.source[p.pos]) {
		for p.pos < len(p.source) && isLetter(p.source[p.pos]) {
			p.pos++
		}
	} else {
		p.pos++
	}
	cmd := p.source[start:p.pos]
	if cmd == "\\" {
		return "\n"
	}
	if cmd == " " || spacing[cmd] {
		return " "
	}
	if ignored[cmd] {
		return ""
	}
	switch cmd {
	case "{", "}", "$", "%", "#", "_", "&":
		return cmd
	}
	if cmd == "!" {
		return "\x00"
	}
	if cmd == "|" {
		return "‖"
	}
	if cmd == "left" || cmd == "middle" || cmd == "right" {
		if p.pos < len(p.source) && p.source[p.pos] == '.' {
			p.pos++
		}
		return ""
	}
	if symbol, found := symbols[cmd]; found {
		if relations[cmd] || cmd == "times" || cmd == "cdot" {
			return " " + symbol + " "
		}
		return symbol
	}
	if operators[cmd] {
		return "\uf004" + cmd + "\uf005"
	}
	switch cmd {
	case "frac", "dfrac", "tfrac":
		n, d := normalize(p.argument()), normalize(p.argument())
		if p.display && cmd != "tfrac" {
			return stackFraction(n, d)
		}
		return fraction(n, d)
	case "sqrt":
		degree, has := p.optional()
		value := normalize(p.argument())
		root := "√"
		switch {
		case has && degree == "3":
			root = "∛"
		case has && degree == "4":
			root = "∜"
		case has && degree != "2":
			root = script(degree, false) + root
		}
		if simple(value) {
			return root + value
		}
		return root + "(" + value + ")"
	case "boxed", "fbox":
		return "[" + strings.TrimSpace(p.argument()) + "]"
	case "binom", "dbinom", "tbinom":
		return "(" + normalize(p.argument()) + " choose " + normalize(p.argument()) + ")"
	case "mathbb":
		var b strings.Builder
		for _, r := range p.argument() {
			if x, ok := blackboard[r]; ok {
				b.WriteRune(x)
			} else {
				b.WriteRune(r)
			}
		}
		return b.String()
	case "operatorname":
		if p.pos < len(p.source) && p.source[p.pos] == '*' {
			p.pos++
		}
		return "\uf004" + normalize(p.argument()) + "\uf005"
	case "mod", "bmod":
		return " mod "
	case "pmod", "pod":
		return " (mod " + normalize(p.argument()) + ")"
	case "overset", "stackrel":
		upper := p.argument()
		value := strings.TrimSpace(p.argument())
		return value + script(upper, false)
	case "underset":
		lower := p.argument()
		value := strings.TrimSpace(p.argument())
		return value + script(lower, true)
	case "begin":
		return p.environment()
	case "not":
		value := strings.TrimSpace(p.argument())
		neg := map[string]string{"=": "≠", "∈": "∉", "≤": "≰", "≥": "≱", "⊂": "⊄", "⊆": "⊈", "→": "↛"}
		if n, ok := neg[value]; ok {
			return " " + n + " "
		}
		return value + "̸"
	}
	if wrappers[cmd] {
		return p.argument()
	}
	accents := map[string]string{"acute": "́", "bar": "̅", "breve": "̆", "check": "̌", "ddot": "̈", "dot": "̇", "grave": "̀", "hat": "̂", "overline": "̅", "overrightarrow": "⃗", "tilde": "̃", "underline": "̲", "vec": "⃗", "widehat": "̂", "widetilde": "̃"}
	if accent, ok := accents[cmd]; ok {
		value := p.argument()
		if utf8.RuneCountInString(value) == 1 {
			return value + accent
		}
		return cmd + "(" + value + ")"
	}
	p.ok = false
	return "\\" + cmd
}

func (p *parser) argument() string {
	for p.pos < len(p.source) && (p.source[p.pos] == ' ' || p.source[p.pos] == '\t') {
		p.pos++
	}
	if p.pos >= len(p.source) {
		p.ok = false
		return ""
	}
	if p.source[p.pos] == '{' {
		p.pos++
		return p.sequence('}')
	}
	if p.source[p.pos] == '\\' {
		return p.command()
	}
	start := p.pos
	_, size := utf8.DecodeRuneInString(p.source[p.pos:])
	p.pos += size
	return p.source[start:p.pos]
}

func (p *parser) optional() (string, bool) {
	for p.pos < len(p.source) && unicode.IsSpace(rune(p.source[p.pos])) {
		p.pos++
	}
	if p.pos >= len(p.source) || p.source[p.pos] != '[' {
		return "", false
	}
	end := strings.IndexByte(p.source[p.pos+1:], ']')
	if end < 0 {
		p.ok = false
		return "", false
	}
	value := p.source[p.pos+1 : p.pos+1+end]
	p.pos += end + 2
	return normalize(value), true
}

func (p *parser) rawGroup() (string, bool) {
	for p.pos < len(p.source) && unicode.IsSpace(rune(p.source[p.pos])) {
		p.pos++
	}
	if p.pos >= len(p.source) || p.source[p.pos] != '{' {
		p.ok = false
		return "", false
	}
	start := p.pos + 1
	depth := 1
	p.pos++
	for p.pos < len(p.source) {
		if p.source[p.pos] == '\\' {
			p.pos += min(2, len(p.source)-p.pos)
			continue
		}
		if p.source[p.pos] == '{' {
			depth++
		}
		if p.source[p.pos] == '}' {
			depth--
			if depth == 0 {
				v := p.source[start:p.pos]
				p.pos++
				return v, true
			}
		}
		p.pos++
	}
	p.ok = false
	return "", false
}

func (p *parser) environment() string {
	env, ok := p.rawGroup()
	if !ok {
		return ""
	}
	marker := "\\end{" + env + "}"
	offset := strings.Index(p.source[p.pos:], marker)
	if offset < 0 {
		p.ok = false
		return ""
	}
	body := p.source[p.pos : p.pos+offset]
	p.pos += offset + len(marker)
	if env == "equation" || env == "equation*" || env == "displaymath" {
		return p.nested(body)
	}
	rows := splitRows(body)
	alignedEnvironments := set("aligned align align* alignedat alignat alignat* gather gathered multline multline* split")
	if alignedEnvironments[env] {
		out := make([]string, 0, len(rows))
		for _, row := range rows {
			row = strings.ReplaceAll(row, "&", "")
			if v := strings.TrimSpace(p.nested(row)); v != "" {
				out = append(out, v)
			}
		}
		return strings.Join(out, "\n")
	}
	if env == "cases" || env == "cases*" {
		out := make([]string, 0, len(rows))
		for i, row := range rows {
			cells := strings.Split(row, "&")
			value := strings.TrimSpace(p.nested(cells[0]))
			condition := ""
			if len(cells) > 1 {
				condition = strings.TrimSpace(p.nested(cells[1]))
			}
			delim := "⎨"
			if i == 0 {
				delim = "⎧"
			}
			if i == len(rows)-1 {
				delim = "⎩"
			}
			if condition != "" && !regexp.MustCompile(`(?i)^(if|when|for|otherwise)\b`).MatchString(condition) {
				condition = "if " + condition
			}
			out = append(out, delim+" "+value+" "+condition)
		}
		return strings.TrimSpace(strings.Join(out, "\n"))
	}
	matrixEnvs := set("array matrix smallmatrix pmatrix bmatrix Bmatrix vmatrix Vmatrix")
	if matrixEnvs[env] {
		return p.matrix(env, body)
	}
	p.ok = false
	return body
}

func (p *parser) nested(source string) string {
	child := parser{source: source, ok: true, display: p.display}
	v := child.sequence(0)
	if !child.ok || child.pos != len(source) {
		p.ok = false
		return source
	}
	return normalize(v)
}

func (p *parser) matrix(env, body string) string {
	rows := splitRows(body)
	cells := make([][]string, 0, len(rows))
	widths := []int{}
	for _, row := range rows {
		parts := strings.Split(row, "&")
		for i := range parts {
			parts[i] = strings.TrimSpace(p.nested(parts[i]))
			if i >= len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], runewidth.StringWidth(parts[i]))
		}
		cells = append(cells, parts)
	}
	lines := make([]string, len(cells))
	for i, row := range cells {
		parts := make([]string, len(widths))
		for j := range widths {
			v := ""
			if j < len(row) {
				v = row[j]
			}
			parts[j] = v + strings.Repeat("\uf002", widths[j]-runewidth.StringWidth(v))
		}
		content := strings.Join(parts, "\uf002\uf002")
		left, right := "", ""
		switch env {
		case "pmatrix":
			left, right = matrixDelims(i, len(cells), "⎛⎜⎝", "⎞⎟⎠")
		case "bmatrix":
			left, right = matrixDelims(i, len(cells), "⎡⎢⎣", "⎤⎥⎦")
		case "Bmatrix":
			left, right = matrixDelims(i, len(cells), "⎧⎨⎩", "⎫⎬⎭")
		case "vmatrix":
			left, right = "│", "│"
		case "Vmatrix":
			left, right = "║", "║"
		}
		if left != "" {
			content = left + " " + content + " " + right
		}
		lines[i] = strings.TrimRight(content, " ")
	}
	return strings.Join(lines, "\n")
}

func matrixDelims(i, n int, left, right string) (string, string) {
	idx := 1
	switch i {
	case 0:
		idx = 0
	case n - 1:
		idx = 2
	}
	return string([]rune(left)[idx]), string([]rune(right)[idx])
}

func splitRows(s string) []string {
	re := regexp.MustCompile(`\\\\(?:\[[^\]\n]*\])?`)
	return re.Split(s, -1)
}
func isLetter(b byte) bool { return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' }
func simple(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.' {
			return false
		}
	}
	return true
}

func fraction(n, d string) string {
	nn, dd := n, d
	if !simple(n) && utf8.RuneCountInString(n) > 1 {
		nn = "(" + n + ")"
	}
	denominatorIsSimple := regexp.MustCompile(`^[\p{N}.]+$`).MatchString(d) || utf8.RuneCountInString(d) == 1
	if !denominatorIsSimple {
		dd = "(" + d + ")"
	}
	return nn + "/" + dd
}

func stackFraction(n, d string) string {
	w := max(runewidth.StringWidth(n), runewidth.StringWidth(d))
	return center(n, w) + "\n" + strings.Repeat("─", w) + "\n" + center(d, w)
}

func center(s string, w int) string {
	pad := w - runewidth.StringWidth(s)
	return strings.Repeat(" ", pad/2) + s + strings.Repeat(" ", pad-pad/2)
}

func script(value string, sub bool) string {
	value = strings.TrimSpace(normalize(value))
	value = regexp.MustCompile(`\s*([=+\-])\s*`).ReplaceAllString(value, "$1")
	table := superscripts
	prefix := "^"
	if sub {
		table = subscripts
		prefix = "_"
	}
	var b strings.Builder
	for _, r := range value {
		mapped, ok := table[r]
		if !ok {
			if utf8.RuneCountInString(value) == 1 || (sub && allLetters(value)) {
				return prefix + value
			}
			return prefix + "(" + value + ")"
		}
		b.WriteRune(mapped)
	}
	return b.String()
}

func allLetters(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

func normalize(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ReplaceAll(s, "\uf004", "")
	s = strings.ReplaceAll(s, "\uf005", "")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(strings.Join(strings.Fields(lines[i]), " "))
		lines[i] = regexp.MustCompile(`\s*([=<>])\s*`).ReplaceAllString(lines[i], " $1 ")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func ExampleRender() {
	rendered, _ := Render(`\mathbb{C}^3 \to \mathbb{C}^3`, false)
	fmt.Println(rendered) // Output: ℂ³ → ℂ³
}
