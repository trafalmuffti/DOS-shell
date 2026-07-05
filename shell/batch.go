package shell

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ===========================================================================
// Batch file execution engine.
//
// This aims for practical compatibility with Windows .bat / .cmd scripts:
// positional parameters and %~ modifiers, CALL :label subroutines, GOTO :EOF,
// EXIT /B, SETLOCAL/ENDLOCAL with delayed !VAR! expansion, IF with comparison
// operators and ELSE blocks, and the FOR /L /F /D /R variants.
// ===========================================================================

// batchProcessor executes one batch file (or CALLed subroutine).
type batchProcessor struct {
	shell *Shell
	lines []string
	pc    int
	echo  bool
	stop  bool // set by EXIT /B or GOTO :EOF to end this processor
}

// runBatchFile loads and executes a batch file.  Any extra arguments become the
// script's positional parameters (%1, %2, ...); %0 is the script name.
func (s *Shell) runBatchFile(path string, params ...string) {
	abs := s.absPath(path)
	data, err := os.ReadFile(abs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "The system cannot find the file specified.\n")
		s.code = 1
		return
	}

	// Save and restore surrounding batch context so nested CALLs of other
	// files behave correctly.
	savedBatch := s.curBatch
	savedParams := s.params
	savedLocalDepth := len(s.local)

	s.params = append([]string{path}, params...)

	bp := &batchProcessor{shell: s, lines: splitLines(string(data)), echo: true}
	s.curBatch = bp
	bp.run()

	// Auto-ENDLOCAL any scopes the script left open.
	for len(s.local) > savedLocalDepth {
		s.cmdEndlocal()
	}

	s.curBatch = savedBatch
	s.params = savedParams
}

// splitLines splits batch text into lines, trimming trailing CR.
func splitLines(text string) []string {
	raw := strings.Split(text, "\n")
	for i := range raw {
		raw[i] = strings.TrimRight(raw[i], "\r")
	}
	return raw
}

// run is the main interpreter loop.
func (bp *batchProcessor) run() {
	for bp.pc < len(bp.lines) && !bp.shell.exit && !bp.stop {
		line := bp.lines[bp.pc]
		bp.pc++

		// Caret line-continuation: a line ending in an unescaped ^ continues.
		for endsWithCaret(line) && bp.pc < len(bp.lines) {
			line = line[:len(line)-1] + bp.lines[bp.pc]
			bp.pc++
		}

		work := strings.TrimSpace(line)
		hadAt := strings.HasPrefix(work, "@")
		if hadAt {
			work = strings.TrimSpace(work[1:])
		}

		if work == "" || isRem(work) || strings.HasPrefix(work, "::") {
			continue
		}
		// Label definition line.
		if strings.HasPrefix(work, ":") {
			continue
		}

		upper := strings.ToUpper(work)

		// ECHO ON / OFF toggles command echoing.
		if upper == "ECHO ON" {
			bp.echo = true
			continue
		}
		if upper == "ECHO OFF" {
			bp.echo = false
			continue
		}

		// IF and FOR may span multiple physical lines via ( ) blocks.
		if isWord(upper, "IF") || isWord(upper, "FOR") {
			stmt := work
			if parenDepth(stmt) > 0 {
				stmt = bp.collect(stmt)
			}
			if bp.echo && !hadAt {
				fmt.Printf("%s%s\n", bp.shell.prompt(), stmt)
			}
			if isWord(upper, "IF") {
				bp.shell.evalIf(stmt, bp)
			} else {
				bp.shell.evalFor(stmt, bp)
			}
			continue
		}

		// Everything else (including GOTO, CALL, SET, EXIT /B) runs through the
		// normal command path so redirection and expansion apply uniformly.
		bp.shell.executeLineWithEcho(line, bp.echo)
	}
}

// collect consumes further lines until parentheses balance, joining them with
// newlines.  first is the already-read opening line.
func (bp *batchProcessor) collect(first string) string {
	text := first
	depth := parenDepth(first)
	for depth > 0 && bp.pc < len(bp.lines) {
		next := bp.lines[bp.pc]
		bp.pc++
		text += "\n" + next
		depth += parenDepth(next)
	}
	return text
}

// endsWithCaret reports whether a line ends with an odd number of carets (an
// unescaped continuation caret).
func endsWithCaret(line string) bool {
	n := 0
	for i := len(line) - 1; i >= 0 && line[i] == '^'; i-- {
		n++
	}
	return n%2 == 1
}

// parenDepth returns the net (signed) paren balance of s, ignoring parens inside
// double quotes or escaped with ^.  A line that closes more than it opens (for
// example a continuation line ") ELSE (") yields a negative or zero delta so the
// block collector can track depth across physical lines correctly.
func parenDepth(s string) int {
	depth := 0
	inQ := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '^' && i+1 < len(s) {
			i++
			continue
		}
		if c == '"' {
			inQ = !inQ
			continue
		}
		if inQ {
			continue
		}
		if c == '(' {
			depth++
		} else if c == ')' {
			depth--
		}
	}
	return depth
}

// isWord reports whether upper (an already-upper-cased string) begins with word
// followed by a space, tab, or end-of-string.
func isWord(upper, word string) bool {
	if !strings.HasPrefix(upper, word) {
		return false
	}
	if len(upper) == len(word) {
		return true
	}
	c := upper[len(word)]
	return c == ' ' || c == '\t'
}

// ---------------------------------------------------------------------------
// Positional parameters
// ---------------------------------------------------------------------------

// param returns positional parameter n with the given ~ modifiers applied.
func (s *Shell) param(n int, mods string) string {
	val := ""
	if n >= 0 && n < len(s.params) {
		val = s.params[n]
	}
	if mods == "" {
		return strings.Trim(val, `"`)
	}
	return s.applyMods(val, mods)
}

// paramStar returns all parameters from %1 onward, space-joined (%*).
func (s *Shell) paramStar() string {
	if len(s.params) <= 1 {
		return ""
	}
	return strings.Join(s.params[1:], " ")
}

// applyMods applies %~ path modifiers (f d p n x s z) to a value.
func (s *Shell) applyMods(value, mods string) string {
	v := strings.Trim(value, `"`)
	abs := s.absPath(v)
	full := s.dosPath(abs)
	dir := s.dosPath(filepath.Dir(abs))
	if !strings.HasSuffix(dir, `\`) {
		dir += `\`
	}
	base := filepath.Base(abs)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	const drive = "C:"

	var b strings.Builder
	for i := 0; i < len(mods); i++ {
		switch mods[i] {
		case 'f', 's':
			b.WriteString(full)
		case 'd':
			b.WriteString(drive)
		case 'p':
			b.WriteString(strings.TrimPrefix(dir, drive))
		case 'n':
			b.WriteString(name)
		case 'x':
			b.WriteString(ext)
		case 'z':
			if fi, err := os.Stat(abs); err == nil {
				b.WriteString(strconv.FormatInt(fi.Size(), 10))
			}
		case 'a', 't':
			// attributes / timestamp not modeled
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// GOTO / CALL / EXIT / SHIFT
// ---------------------------------------------------------------------------

func (s *Shell) cmdGoto(args []string) {
	if len(args) == 0 {
		s.code = 1
		return
	}
	label := strings.TrimPrefix(strings.ToLower(args[0]), ":")
	bp := s.curBatch
	if bp == nil {
		s.code = 1
		return
	}
	if label == "eof" {
		bp.stop = true
		return
	}
	if idx := findLabel(bp.lines, label); idx >= 0 {
		bp.pc = idx + 1
		s.code = 0
		return
	}
	fmt.Fprintf(os.Stderr, "The system cannot find the batch label specified - %s\n", label)
	bp.stop = true
	s.code = 1
}

// findLabel returns the index of a :label line, or -1.
func findLabel(lines []string, label string) int {
	target := ":" + label
	for i, l := range lines {
		t := strings.TrimSpace(l)
		// A label ends at the first space; ":foo bar" defines label foo.
		if len(t) > 0 && t[0] == ':' {
			name := t
			if sp := strings.IndexAny(t, " \t"); sp >= 0 {
				name = t[:sp]
			}
			if strings.EqualFold(name, target) {
				return i
			}
		}
	}
	return -1
}

// cmdCall implements CALL — either a :label subroutine in the current batch or
// another batch file.
func (s *Shell) cmdCall(args []string) {
	if len(args) == 0 {
		s.code = 0
		return
	}

	// CALL :label [params...]  — run a subroutine within the current file.
	if strings.HasPrefix(args[0], ":") {
		bp := s.curBatch
		if bp == nil {
			s.code = 1
			return
		}
		label := strings.TrimPrefix(strings.ToLower(args[0]), ":")
		idx := findLabel(bp.lines, label)
		if idx < 0 {
			fmt.Fprintf(os.Stderr, "The system cannot find the batch label specified - %s\n", label)
			s.code = 1
			return
		}

		savedParams := s.params
		savedBatch := s.curBatch
		savedLocalDepth := len(s.local)

		s.params = append([]string{args[0]}, args[1:]...)
		sub := &batchProcessor{shell: s, lines: bp.lines, pc: idx + 1, echo: bp.echo}
		s.curBatch = sub
		sub.run()

		for len(s.local) > savedLocalDepth {
			s.cmdEndlocal()
		}
		s.curBatch = savedBatch
		s.params = savedParams
		return
	}

	// CALL other.bat [params...]
	s.runBatchFile(args[0], args[1:]...)
}

// cmdExit implements EXIT and EXIT /B.
func (s *Shell) cmdExit(args []string) {
	batchOnly := false
	code := s.code
	codeSet := false
	for _, a := range args {
		if strings.EqualFold(a, "/B") {
			batchOnly = true
			continue
		}
		if n, err := strconv.Atoi(a); err == nil {
			code = n
			codeSet = true
		}
	}
	if codeSet {
		s.code = code
	}
	if batchOnly && s.curBatch != nil {
		s.curBatch.stop = true
		return
	}
	s.exit = true
}

// cmdShift discards %1 and shifts higher parameters down (SHIFT).
func (s *Shell) cmdShift(args []string) {
	// SHIFT [/n] — shift starting at parameter n (default 1).
	start := 1
	if len(args) > 0 && strings.HasPrefix(strings.ToLower(args[0]), "/") {
		if n, err := strconv.Atoi(args[0][1:]); err == nil {
			start = n + 1 // %0 is index 0
		}
	}
	if start < len(s.params) {
		s.params = append(s.params[:start], s.params[start+1:]...)
	}
	s.code = 0
}

// ---------------------------------------------------------------------------
// SETLOCAL / ENDLOCAL
// ---------------------------------------------------------------------------

func (s *Shell) cmdSetlocal(args []string) {
	// Snapshot the current environment.
	snap := make(map[string]string, len(s.env))
	for k, v := range s.env {
		snap[k] = v
	}
	s.local = append(s.local, localScope{env: snap, delayed: s.delayed})

	for _, a := range args {
		switch strings.ToUpper(a) {
		case "ENABLEDELAYEDEXPANSION":
			s.delayed = true
		case "DISABLEDELAYEDEXPANSION":
			s.delayed = false
		}
	}
	s.code = 0
}

func (s *Shell) cmdEndlocal() {
	if len(s.local) == 0 {
		s.code = 0
		return
	}
	scope := s.local[len(s.local)-1]
	s.local = s.local[:len(s.local)-1]
	s.env = scope.env
	s.delayed = scope.delayed
	s.code = 0
}

// ---------------------------------------------------------------------------
// PUSHD / POPD
// ---------------------------------------------------------------------------

func (s *Shell) cmdPushd(args []string) {
	if len(args) == 0 {
		s.code = 0
		return
	}
	s.pushd = append(s.pushd, s.cwd)
	s.cmdCd(args)
}

func (s *Shell) cmdPopd() {
	if len(s.pushd) == 0 {
		s.code = 0
		return
	}
	dir := s.pushd[len(s.pushd)-1]
	s.pushd = s.pushd[:len(s.pushd)-1]
	s.cwd = dir
	os.Chdir(dir)
	s.code = 0
}

// ---------------------------------------------------------------------------
// SET /A  and  SET /P
// ---------------------------------------------------------------------------

// setArithmetic evaluates one or more comma-separated arithmetic expressions
// (SET /A), assigning to variables and printing the final value.
func (s *Shell) setArithmetic(expr string) {
	var last int
	for _, part := range splitTopLevel(expr, ',') {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		last = s.evalAssign(part)
	}
	fmt.Printf("%d", last)
	s.code = 0
}

// evalAssign handles "var=expr" and compound "var op= expr" as well as bare
// expressions, returning the resulting value.
func (s *Shell) evalAssign(part string) int {
	// Compound assignment operators.
	for _, op := range []string{"+=", "-=", "*=", "/=", "%=", "&=", "|=", "^="} {
		if idx := strings.Index(part, op); idx >= 0 {
			name := strings.ToUpper(strings.TrimSpace(part[:idx]))
			rhs := s.evalArith(part[idx+2:])
			cur, _ := strconv.Atoi(s.env[name])
			res := applyArith(cur, rhs, op[0])
			s.env[name] = strconv.Itoa(res)
			return res
		}
	}
	if idx := strings.Index(part, "="); idx >= 0 {
		name := strings.ToUpper(strings.TrimSpace(part[:idx]))
		res := s.evalArith(part[idx+1:])
		s.env[name] = strconv.Itoa(res)
		return res
	}
	return s.evalArith(part)
}

func applyArith(a, b int, op byte) int {
	switch op {
	case '+':
		return a + b
	case '-':
		return a - b
	case '*':
		return a * b
	case '/':
		if b == 0 {
			return 0
		}
		return a / b
	case '%':
		if b == 0 {
			return 0
		}
		return a % b
	case '&':
		return a & b
	case '|':
		return a | b
	case '^':
		return a ^ b
	}
	return b
}

// evalArith evaluates an integer arithmetic expression with + - * / % and
// parentheses.  Bare names resolve to their (numeric) variable value.
func (s *Shell) evalArith(expr string) int {
	p := &arithParser{shell: s, src: strings.TrimSpace(expr)}
	return p.parseExpr()
}

type arithParser struct {
	shell *Shell
	src   string
	pos   int
}

func (p *arithParser) skipSpace() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
}

// parseExpr := term (('+'|'-') term)*
func (p *arithParser) parseExpr() int {
	v := p.parseTerm()
	for {
		p.skipSpace()
		if p.pos >= len(p.src) {
			break
		}
		op := p.src[p.pos]
		if op != '+' && op != '-' {
			break
		}
		p.pos++
		r := p.parseTerm()
		if op == '+' {
			v += r
		} else {
			v -= r
		}
	}
	return v
}

// parseTerm := factor (('*'|'/'|'%') factor)*
func (p *arithParser) parseTerm() int {
	v := p.parseFactor()
	for {
		p.skipSpace()
		if p.pos >= len(p.src) {
			break
		}
		op := p.src[p.pos]
		if op != '*' && op != '/' && op != '%' {
			break
		}
		p.pos++
		r := p.parseFactor()
		v = applyArith(v, r, op)
	}
	return v
}

// parseFactor := ['-'] ( number | name | '(' expr ')' )
func (p *arithParser) parseFactor() int {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return 0
	}
	if p.src[p.pos] == '-' {
		p.pos++
		return -p.parseFactor()
	}
	if p.src[p.pos] == '+' {
		p.pos++
		return p.parseFactor()
	}
	if p.src[p.pos] == '(' {
		p.pos++
		v := p.parseExpr()
		p.skipSpace()
		if p.pos < len(p.src) && p.src[p.pos] == ')' {
			p.pos++
		}
		return v
	}
	// Number?
	start := p.pos
	for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
		p.pos++
	}
	if p.pos > start {
		n, _ := strconv.Atoi(p.src[start:p.pos])
		return n
	}
	// Identifier — resolve as a variable.
	for p.pos < len(p.src) && isNameChar(p.src[p.pos]) {
		p.pos++
	}
	name := strings.ToUpper(p.src[start:p.pos])
	n, _ := strconv.Atoi(p.shell.env[name])
	return n
}

func isNameChar(c byte) bool {
	return c == '_' ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}

// setPrompt implements SET /P var=prompt — display the prompt and read a line.
func (s *Shell) setPrompt(rest string) {
	eq := strings.Index(rest, "=")
	if eq < 0 {
		s.code = 1
		return
	}
	name := strings.ToUpper(strings.TrimSpace(rest[:eq]))
	prompt := rest[eq+1:]
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if line != "" {
		s.env[name] = line
	}
	s.code = 0
}

// ---------------------------------------------------------------------------
// IF
// ---------------------------------------------------------------------------

// evalIf parses and executes an IF statement.  bp may be nil (interactive).
func (s *Shell) evalIf(stmt string, bp *batchProcessor) {
	rest := strings.TrimSpace(stmt[2:]) // strip "IF"

	caseInsensitive := false
	if hasPrefixWord(rest, "/I") {
		caseInsensitive = true
		rest = strings.TrimSpace(rest[2:])
	}
	negate := false
	if hasPrefixWord(rest, "NOT") {
		negate = true
		rest = strings.TrimSpace(rest[3:])
	}

	cond, body := s.parseCondition(rest, caseInsensitive)
	if negate {
		cond = !cond
	}

	thenText, elseText, hasElse := splitThenElse(body)
	if cond {
		s.runStatements(thenText, bp)
	} else if hasElse {
		s.runStatements(elseText, bp)
	}
}

// parseCondition evaluates the condition at the start of rest and returns the
// result plus the remaining command/block text.  Operand values undergo %- and
// (when enabled) !-expansion, matching cmd's parse-time expansion of IF.
func (s *Shell) parseCondition(rest string, ci bool) (bool, string) {
	upper := strings.ToUpper(rest)

	switch {
	case strings.HasPrefix(upper, "ERRORLEVEL "):
		after := strings.TrimSpace(rest[len("ERRORLEVEL "):])
		tok, remainder := firstToken(after)
		n, _ := strconv.Atoi(s.expandValue(tok))
		return s.code >= n, remainder

	case strings.HasPrefix(upper, "EXIST "):
		after := strings.TrimSpace(rest[len("EXIST "):])
		tok, remainder := firstToken(after)
		path := s.absPath(unquote(s.expandValue(tok)))
		matches, _ := filepath.Glob(path)
		_, statErr := os.Stat(path)
		return statErr == nil || len(matches) > 0, remainder

	case strings.HasPrefix(upper, "DEFINED "):
		after := strings.TrimSpace(rest[len("DEFINED "):])
		tok, remainder := firstToken(after)
		_, ok := s.env[strings.ToUpper(s.expandValue(tok))]
		return ok, remainder
	}

	// Comparison forms.  First try the word operators (EQU, NEQ, ...).
	for _, opw := range []string{" EQU ", " NEQ ", " LSS ", " LEQ ", " GTR ", " GEQ "} {
		if idx := indexFold(rest, opw); idx >= 0 {
			lhs := s.expandValue(strings.TrimSpace(rest[:idx]))
			after := strings.TrimSpace(rest[idx+len(opw):])
			rhsTok, remainder := firstToken(after)
			return compareOp(strings.TrimSpace(opw), unquote(lhs), unquote(s.expandValue(rhsTok)), ci), remainder
		}
	}

	// String equality with ==.
	if idx := strings.Index(rest, "=="); idx >= 0 {
		lhs := unquote(s.expandValue(strings.TrimSpace(rest[:idx])))
		after := strings.TrimSpace(rest[idx+2:])
		rhsTok, remainder := firstToken(after)
		b := unquote(s.expandValue(rhsTok))
		if ci {
			return strings.EqualFold(lhs, b), remainder
		}
		return lhs == b, remainder
	}

	// Unrecognised — treat as false with the whole text as body.
	return false, ""
}

// expandValue applies %- and (when enabled) !-expansion to a single value.
func (s *Shell) expandValue(v string) string {
	v = s.expandVars(v)
	if s.delayed {
		v = s.expandDelayed(v)
	}
	return v
}

// compareOp evaluates one of the EQU/NEQ/LSS/LEQ/GTR/GEQ operators, numerically
// when both operands are integers, otherwise lexically.
func compareOp(op, a, b string, ci bool) bool {
	na, ea := strconv.Atoi(a)
	nb, eb := strconv.Atoi(b)
	numeric := ea == nil && eb == nil

	var cmp int
	if numeric {
		switch {
		case na < nb:
			cmp = -1
		case na > nb:
			cmp = 1
		}
	} else {
		x, y := a, b
		if ci {
			x, y = strings.ToLower(a), strings.ToLower(b)
		}
		cmp = strings.Compare(x, y)
	}

	switch op {
	case "EQU":
		return cmp == 0
	case "NEQ":
		return cmp != 0
	case "LSS":
		return cmp < 0
	case "LEQ":
		return cmp <= 0
	case "GTR":
		return cmp > 0
	case "GEQ":
		return cmp >= 0
	}
	return false
}

// splitThenElse separates the THEN and ELSE portions of an IF body, handling
// both single-command and parenthesised-block forms.
func splitThenElse(body string) (thenText, elseText string, hasElse bool) {
	body = strings.TrimLeft(body, " \t")
	if strings.HasPrefix(body, "(") {
		inner, after := extractBlock(body)
		thenText = inner
		after = strings.TrimLeft(after, " \t")
		if hasPrefixWord(after, "ELSE") {
			elseText = strings.TrimSpace(after[4:])
			elseText = stripOuterParens(elseText)
			hasElse = true
		}
		return
	}
	if idx := indexFold(body, " ELSE "); idx >= 0 {
		thenText = strings.TrimSpace(body[:idx])
		elseText = strings.TrimSpace(body[idx+len(" ELSE "):])
		elseText = stripOuterParens(elseText)
		hasElse = true
		return
	}
	return strings.TrimSpace(body), "", false
}

// extractBlock, given s starting with '(', returns the text inside the matching
// parentheses and the remainder after the closing ')'.
func extractBlock(s string) (inner, after string) {
	depth := 0
	inQ := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '^' && i+1 < len(s) {
			i++
			continue
		}
		if c == '"' {
			inQ = !inQ
			continue
		}
		if inQ {
			continue
		}
		if c == '(' {
			depth++
		} else if c == ')' {
			depth--
			if depth == 0 {
				return strings.TrimSpace(s[1:i]), s[i+1:]
			}
		}
	}
	return strings.TrimSpace(strings.TrimPrefix(s, "(")), ""
}

func stripOuterParens(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "(") {
		inner, _ := extractBlock(s)
		return inner
	}
	return s
}

// runStatements executes a THEN/ELSE/FOR body: possibly several lines, each of
// which may itself use & chaining.
func (s *Shell) runStatements(text string, bp *batchProcessor) {
	echo := bp != nil && bp.echo
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		s.executeLineWithEcho(line, echo)
		if s.exit || (bp != nil && bp.stop) {
			return
		}
	}
}

// ---------------------------------------------------------------------------
// FOR
// ---------------------------------------------------------------------------

// evalFor parses and executes a FOR statement.
func (s *Shell) evalFor(stmt string, bp *batchProcessor) {
	rest := strings.TrimSpace(stmt[3:]) // strip "FOR"

	mode := ""     // "", "L", "F", "D", "R"
	rootPath := "" // for /R
	fOptions := "" // for /F option string
	upper := strings.ToUpper(rest)

	switch {
	case strings.HasPrefix(upper, "/L"):
		mode = "L"
		rest = strings.TrimSpace(rest[2:])
	case strings.HasPrefix(upper, "/D"):
		mode = "D"
		rest = strings.TrimSpace(rest[2:])
	case strings.HasPrefix(upper, "/R"):
		mode = "R"
		rest = strings.TrimSpace(rest[2:])
		// Optional root path before the variable.
		if !strings.HasPrefix(rest, "%") {
			rootPath, rest = firstToken(rest)
		}
	case strings.HasPrefix(upper, "/F"):
		mode = "F"
		rest = strings.TrimSpace(rest[2:])
		if strings.HasPrefix(rest, `"`) {
			end := strings.Index(rest[1:], `"`)
			if end >= 0 {
				fOptions = rest[1 : 1+end]
				rest = strings.TrimSpace(rest[end+2:])
			}
		}
	}

	// Variable: %v (interactive) or %%v (batch file).
	varLetter, rest, ok := parseForVar(rest)
	if !ok {
		fmt.Fprintln(os.Stderr, "The syntax of the command is incorrect.")
		s.code = 1
		return
	}

	// IN (set) DO body
	inIdx := indexFold(rest, "IN")
	if inIdx < 0 {
		fmt.Fprintln(os.Stderr, "The syntax of the command is incorrect.")
		s.code = 1
		return
	}
	rest = strings.TrimSpace(rest[inIdx+2:])
	if !strings.HasPrefix(rest, "(") {
		fmt.Fprintln(os.Stderr, "The syntax of the command is incorrect.")
		s.code = 1
		return
	}
	setStr, after := extractBlock(rest)
	after = strings.TrimSpace(after)
	if !hasPrefixWord(after, "DO") {
		fmt.Fprintln(os.Stderr, "The syntax of the command is incorrect.")
		s.code = 1
		return
	}
	body := strings.TrimSpace(after[2:])

	switch mode {
	case "L":
		s.forL(varLetter, setStr, body, bp)
	case "F":
		s.forF(varLetter, fOptions, setStr, body, bp)
	case "D":
		s.forEachItem(varLetter, s.globItems(setStr, true), body, bp)
	case "R":
		s.forR(varLetter, rootPath, setStr, body, bp)
	default:
		s.forEachItem(varLetter, s.defaultItems(setStr), body, bp)
	}
}

// parseForVar extracts the loop variable letter from the start of rest.
func parseForVar(rest string) (byte, string, bool) {
	rest = strings.TrimLeft(rest, " \t")
	i := 0
	// Accept one or two leading % (batch doubles them).
	for i < len(rest) && rest[i] == '%' {
		i++
	}
	if i == 0 || i >= len(rest) {
		return 0, rest, false
	}
	letter := rest[i]
	return letter, rest[i+1:], true
}

// runForBody executes one iteration of a FOR body, which may be a single
// command or a parenthesised multi-line block.
func (s *Shell) runForBody(body string, bp *batchProcessor) {
	s.runStatements(stripOuterParens(body), bp)
}

// forEachItem runs body once per item, binding the loop variable.
func (s *Shell) forEachItem(letter byte, items []string, body string, bp *batchProcessor) {
	for _, item := range items {
		s.runForBody(expandForVar(body, letter, item, s), bp)
		if s.exit || (bp != nil && bp.stop) {
			return
		}
	}
	s.code = 0
}

// defaultItems splits a plain FOR set on whitespace/comma/semicolon, expanding
// any wildcard entries against the filesystem.
func (s *Shell) defaultItems(set string) []string {
	fields := splitSet(set)
	var out []string
	for _, f := range fields {
		if strings.ContainsAny(f, "*?") {
			matches, _ := filepath.Glob(s.absPath(f))
			for _, m := range matches {
				out = append(out, filepath.Base(m))
			}
		} else {
			out = append(out, f)
		}
	}
	return out
}

// globItems expands wildcards in a set; when dirsOnly is set, only directories
// are returned (FOR /D).
func (s *Shell) globItems(set string, dirsOnly bool) []string {
	var out []string
	for _, f := range splitSet(set) {
		matches, _ := filepath.Glob(s.absPath(f))
		for _, m := range matches {
			fi, err := os.Stat(m)
			if err != nil {
				continue
			}
			if dirsOnly && !fi.IsDir() {
				continue
			}
			out = append(out, filepath.Base(m))
		}
	}
	return out
}

// forL implements FOR /L %v IN (start,step,end).
func (s *Shell) forL(letter byte, set, body string, bp *batchProcessor) {
	parts := strings.Split(set, ",")
	if len(parts) != 3 {
		fmt.Fprintln(os.Stderr, "The syntax of the command is incorrect.")
		s.code = 1
		return
	}
	start, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
	step, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
	end, _ := strconv.Atoi(strings.TrimSpace(parts[2]))
	if step == 0 {
		return
	}
	for i := start; (step > 0 && i <= end) || (step < 0 && i >= end); i += step {
		s.runForBody(expandForVar(body, letter, strconv.Itoa(i), s), bp)
		if s.exit || (bp != nil && bp.stop) {
			return
		}
	}
	s.code = 0
}

// forR implements FOR /R [root] %v IN (globs) walking directories recursively.
func (s *Shell) forR(letter byte, root, set, body string, bp *batchProcessor) {
	base := s.cwd
	if root != "" {
		base = s.absPath(root)
	}
	globs := splitSet(set)

	filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		for _, g := range globs {
			if g == "*" || g == "*.*" {
				s.runForBody(expandForVar(body, letter, s.dosPath(path), s), bp)
				break
			}
			if ok, _ := filepath.Match(g, info.Name()); ok {
				s.runForBody(expandForVar(body, letter, s.dosPath(path), s), bp)
				break
			}
		}
		return nil
	})
	s.code = 0
}

// forF implements FOR /F token parsing over files, strings, or command output.
func (s *Shell) forF(letter byte, options, set, body string, bp *batchProcessor) {
	tokens, delims, skip, eol, usebackq := parseForFOptions(options)

	// Gather the input lines according to the source form.
	var lines []string
	src := strings.TrimSpace(set)
	switch {
	case usebackq && strings.HasPrefix(src, "`") && strings.HasSuffix(src, "`"):
		lines = s.captureCommand(strings.Trim(src, "`"))
	case usebackq && strings.HasPrefix(src, `"`) && strings.HasSuffix(src, `"`):
		lines = s.readFileLines(strings.Trim(src, `"`))
	case usebackq && strings.HasPrefix(src, "'") && strings.HasSuffix(src, "'"):
		lines = strings.Split(strings.Trim(src, "'"), "\n")
	case !usebackq && strings.HasPrefix(src, `"`) && strings.HasSuffix(src, `"`):
		lines = strings.Split(strings.Trim(src, `"`), "\n")
	case !usebackq && strings.HasPrefix(src, "'") && strings.HasSuffix(src, "'"):
		lines = s.captureCommand(strings.Trim(src, "'"))
	default:
		// One or more filenames.
		for _, f := range splitSet(src) {
			lines = append(lines, s.readFileLines(f)...)
		}
	}

	for i, line := range lines {
		if i < skip {
			continue
		}
		if eol != 0 && len(line) > 0 && line[0] == eol {
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := tokenizeDelims(line, delims)
		// Bind each requested token to successive loop variables.
		expanded := body
		for k, tokIdx := range tokens {
			val := ""
			if tokIdx == -1 { // '*' — rest of the line from this token on
				val = joinFrom(fields, len(tokens)-1)
			} else if tokIdx-1 < len(fields) {
				val = fields[tokIdx-1]
			}
			expanded = expandForVar(expanded, letter+byte(k), val, s)
		}
		s.runForBody(expanded, bp)
		if s.exit || (bp != nil && bp.stop) {
			return
		}
	}
	s.code = 0
}

// parseForFOptions parses the FOR /F option string.
func parseForFOptions(opts string) (tokens []int, delims string, skip int, eol byte, usebackq bool) {
	delims = " \t"
	eol = 0
	for _, field := range strings.Fields(opts) {
		lf := strings.ToLower(field)
		switch {
		case lf == "usebackq":
			usebackq = true
		case strings.HasPrefix(lf, "tokens="):
			tokens = parseTokenSpec(field[len("tokens="):])
		case strings.HasPrefix(lf, "delims="):
			delims = field[len("delims="):]
		case strings.HasPrefix(lf, "skip="):
			skip, _ = strconv.Atoi(field[len("skip="):])
		case strings.HasPrefix(lf, "eol="):
			if v := field[len("eol="):]; len(v) > 0 {
				eol = v[0]
			}
		}
	}
	if len(tokens) == 0 {
		tokens = []int{1}
	}
	return
}

// parseTokenSpec parses tokens=1,2,4 / tokens=1-3 / tokens=* / tokens=2,* forms.
func parseTokenSpec(spec string) []int {
	var out []int
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "*" {
			out = append(out, -1)
			continue
		}
		if dash := strings.Index(part, "-"); dash >= 0 {
			lo, _ := strconv.Atoi(part[:dash])
			hi, _ := strconv.Atoi(part[dash+1:])
			for i := lo; i <= hi; i++ {
				out = append(out, i)
			}
			continue
		}
		if n, err := strconv.Atoi(part); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// tokenizeDelims splits line on any delimiter character in delims.
func tokenizeDelims(line, delims string) []string {
	if delims == "" {
		return []string{line}
	}
	return strings.FieldsFunc(line, func(r rune) bool {
		return strings.ContainsRune(delims, r)
	})
}

func joinFrom(fields []string, from int) string {
	if from < 0 || from >= len(fields) {
		if from == len(fields)-1+1 && len(fields) > 0 {
			return ""
		}
	}
	if from < len(fields) {
		return strings.Join(fields[from:], " ")
	}
	return ""
}

// readFileLines reads a file relative to cwd, returning its lines.
func (s *Shell) readFileLines(name string) []string {
	data, err := os.ReadFile(s.absPath(strings.Trim(name, `"`)))
	if err != nil {
		return nil
	}
	return splitLines(string(data))
}

// captureCommand runs a command through the shell, capturing its stdout as
// lines (used by FOR /F over command output).
func (s *Shell) captureCommand(command string) []string {
	out := s.captureStdout(func() { s.executeLineWithEcho(command, false) })
	return splitLines(strings.TrimRight(out, "\n"))
}

// captureStdout redirects os.Stdout while fn runs and returns what was written.
func (s *Shell) captureStdout(fn func()) string {
	r, w, err := os.Pipe()
	if err != nil {
		fn()
		return ""
	}
	old := os.Stdout
	os.Stdout = w

	done := make(chan string)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, e := r.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
			}
			if e != nil {
				break
			}
		}
		done <- b.String()
	}()

	fn()
	w.Close()
	os.Stdout = old
	result := <-done
	r.Close()
	return result
}

// ---------------------------------------------------------------------------
// FOR-variable substitution
// ---------------------------------------------------------------------------

// expandForVar replaces references to a FOR loop variable with value, handling
// both the batch %%L form and the interactive %L form, with optional ~ path
// modifiers (for example %%~nxI).
func expandForVar(body string, letter byte, value string, s *Shell) string {
	var b strings.Builder
	i := 0
	for i < len(body) {
		if body[i] != '%' {
			b.WriteByte(body[i])
			i++
			continue
		}
		// Consume a run of one or two percent signs (batch doubles them).
		j := i + 1
		double := false
		if j < len(body) && body[j] == '%' {
			double = true
			j++
		}
		// Optional ~modifiers.
		k := j
		mods := ""
		if k < len(body) && body[k] == '~' {
			k++
			for k < len(body) && isModChar(body[k]) {
				mods += string(body[k])
				k++
			}
		}
		if k < len(body) && body[k] == letter {
			// For the single-% form, guard against matching the start of a
			// longer %NAME% reference (e.g. loop var I inside %INDEX%).
			nextIsName := k+1 < len(body) && isNameChar(body[k+1])
			if double || mods != "" || !nextIsName {
				out := value
				if mods != "" {
					out = s.applyMods(value, mods)
				}
				b.WriteString(out)
				i = k + 1
				continue
			}
		}
		// Not a loop-variable reference — emit the percent run verbatim.
		b.WriteString(body[i:j])
		i = j
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Small string helpers
// ---------------------------------------------------------------------------

// firstToken returns the first whitespace-delimited token (quote aware) and the
// remainder of s.
func firstToken(s string) (string, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	if s[0] == '"' {
		if end := strings.Index(s[1:], `"`); end >= 0 {
			return s[:end+2], strings.TrimSpace(s[end+2:])
		}
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], strings.TrimSpace(s[i+1:])
	}
	return s, ""
}

// hasPrefixWord reports whether s begins with word (case-insensitive) followed
// by whitespace or end-of-string.
func hasPrefixWord(s, word string) bool {
	if len(s) < len(word) {
		return false
	}
	if !strings.EqualFold(s[:len(word)], word) {
		return false
	}
	if len(s) == len(word) {
		return true
	}
	c := s[len(word)]
	return c == ' ' || c == '\t'
}

func unquote(s string) string { return strings.Trim(strings.TrimSpace(s), `"`) }

// splitSet splits a FOR set on spaces, commas and semicolons.
func splitSet(set string) []string {
	return strings.FieldsFunc(set, func(r rune) bool {
		return r == ' ' || r == '\t' || r == ',' || r == ';'
	})
}

// ---------------------------------------------------------------------------
// I/O redirection ( >  >>  <  2>  2>>  2>&1 , with the NUL device )
// ---------------------------------------------------------------------------

type redirection struct {
	outPath   string
	outAppend bool
	errPath   string
	errAppend bool
	errToOut  bool
	inPath    string
}

// parseRedirection extracts redirection operators from a command line, returning
// the cleaned command and a redirection (nil when none is present).
func parseRedirection(line string) (string, *redirection) {
	var b strings.Builder
	r := &redirection{}
	used := false
	inQ := false

	readTarget := func(pos int) (string, int) {
		for pos < len(line) && (line[pos] == ' ' || line[pos] == '\t') {
			pos++
		}
		start := pos
		q := false
		for pos < len(line) {
			ch := line[pos]
			if ch == '"' {
				q = !q
				pos++
				continue
			}
			if !q && (ch == ' ' || ch == '\t' || ch == '&' || ch == '|' || ch == '<' || ch == '>') {
				break
			}
			pos++
		}
		return strings.Trim(line[start:pos], `"`), pos
	}

	i := 0
	for i < len(line) {
		if line[i] == '^' && i+1 < len(line) {
			b.WriteByte(line[i])
			b.WriteByte(line[i+1])
			i += 2
			continue
		}
		if line[i] == '"' {
			inQ = !inQ
			b.WriteByte(line[i])
			i++
			continue
		}
		if !inQ {
			switch {
			case strings.HasPrefix(line[i:], "2>&1"):
				r.errToOut = true
				used = true
				i += 4
				continue
			case strings.HasPrefix(line[i:], "2>>"):
				t, np := readTarget(i + 3)
				r.errPath, r.errAppend, used, i = t, true, true, np
				continue
			case strings.HasPrefix(line[i:], "2>"):
				t, np := readTarget(i + 2)
				r.errPath, used, i = t, true, np
				continue
			case strings.HasPrefix(line[i:], "1>>"):
				t, np := readTarget(i + 3)
				r.outPath, r.outAppend, used, i = t, true, true, np
				continue
			case strings.HasPrefix(line[i:], "1>"):
				t, np := readTarget(i + 2)
				r.outPath, used, i = t, true, np
				continue
			case strings.HasPrefix(line[i:], ">>"):
				t, np := readTarget(i + 2)
				r.outPath, r.outAppend, used, i = t, true, true, np
				continue
			case line[i] == '>':
				t, np := readTarget(i + 1)
				r.outPath, used, i = t, true, np
				continue
			case line[i] == '<':
				t, np := readTarget(i + 1)
				r.inPath, used, i = t, true, np
				continue
			}
		}
		b.WriteByte(line[i])
		i++
	}

	if !used {
		return line, nil
	}
	return strings.TrimSpace(b.String()), r
}

// isNul reports whether a redirection target is the null device.
func isNul(path string) bool {
	return strings.EqualFold(path, "nul") || path == os.DevNull
}

// runWithRedirection runs cmd with os.Stdout/os.Stderr/os.Stdin temporarily
// pointed at the requested targets.
func (s *Shell) runWithRedirection(cmd string, r *redirection) {
	oldOut, oldErr, oldIn := os.Stdout, os.Stderr, os.Stdin
	var closers []*os.File

	openWrite := func(path string, appnd bool) *os.File {
		if isNul(path) {
			f, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
			return f
		}
		flag := os.O_CREATE | os.O_WRONLY
		if appnd {
			flag |= os.O_APPEND
		} else {
			flag |= os.O_TRUNC
		}
		f, err := os.OpenFile(s.absPath(path), flag, 0644)
		if err != nil {
			fmt.Fprintf(oldErr, "The system cannot open the file %s.\n", path)
			return nil
		}
		return f
	}

	if r.outPath != "" {
		if f := openWrite(r.outPath, r.outAppend); f != nil {
			os.Stdout = f
			closers = append(closers, f)
		}
	}
	if r.errToOut {
		os.Stderr = os.Stdout
	}
	if r.errPath != "" {
		if f := openWrite(r.errPath, r.errAppend); f != nil {
			os.Stderr = f
			closers = append(closers, f)
		}
	}
	if r.inPath != "" {
		var f *os.File
		if isNul(r.inPath) {
			f, _ = os.Open(os.DevNull)
		} else {
			f, _ = os.Open(s.absPath(r.inPath))
		}
		if f != nil {
			os.Stdin = f
			closers = append(closers, f)
		}
	}

	s.dispatch(cmd)

	os.Stdout, os.Stderr, os.Stdin = oldOut, oldErr, oldIn
	for _, f := range closers {
		f.Close()
	}
}

// splitTopLevel splits s on sep, ignoring sep inside parentheses.
func splitTopLevel(s string, sep byte) []string {
	var out []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case sep:
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}
