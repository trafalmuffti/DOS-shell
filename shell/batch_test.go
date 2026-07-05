package shell

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runBatch writes content to a .bat file in a temp dir, executes it with the
// given params, and returns captured stdout.  The shell's cwd is the temp dir.
func runBatch(t *testing.T, content string, params ...string) string {
	t.Helper()
	dir := t.TempDir()
	bat := filepath.Join(dir, "script.bat")
	if err := os.WriteFile(bat, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	s := New()
	s.cwd = dir
	return captureOutput(func() { s.runBatchFile(bat, params...) })
}

// ---------------------------------------------------------------------------
// Expansion helpers
// ---------------------------------------------------------------------------

func TestSubstring(t *testing.T) {
	cases := []struct {
		base, spec, want string
	}{
		{"Hello World", "0,5", "Hello"},
		{"Hello World", "6", "World"},
		{"Hello World", "6,3", "Wor"},
		{"Hello World", "-5", "World"},
		{"Hello World", "0,-6", "Hello"},
		{"abc", "10", ""},
	}
	for _, c := range cases {
		if got := substring(c.base, c.spec); got != c.want {
			t.Errorf("substring(%q,%q) = %q, want %q", c.base, c.spec, got, c.want)
		}
	}
}

func TestSubstitute(t *testing.T) {
	if got := substitute("Hello World", "World", "There"); got != "Hello There" {
		t.Errorf("got %q", got)
	}
	// Case-insensitive find.
	if got := substitute("Hello World", "world", "There"); got != "Hello There" {
		t.Errorf("case-insensitive: got %q", got)
	}
	// Replace-all.
	if got := substitute("a.b.c", ".", "-"); got != "a-b-c" {
		t.Errorf("replace-all: got %q", got)
	}
	// *prefix form: replace everything up to and including the FIRST match.
	if got := substitute("path/to/file", "*/", "X"); got != "Xto/file" {
		t.Errorf("star form: got %q", got)
	}
}

func TestExpandVarsSubstringAndSubst(t *testing.T) {
	s := New()
	s.env["V"] = "abcdef"
	if got := s.expandVars("%V:~1,3%"); got != "bcd" {
		t.Errorf("substring expand: got %q", got)
	}
	if got := s.expandVars("%V:cd=XY%"); got != "abXYef" {
		t.Errorf("subst expand: got %q", got)
	}
}

func TestExpandVarsDynamic(t *testing.T) {
	s := New()
	s.code = 7
	if got := s.expandVars("%ERRORLEVEL%"); got != "7" {
		t.Errorf("ERRORLEVEL: got %q", got)
	}
	if got := s.expandVars("%CD%"); !strings.HasPrefix(got, `C:`) {
		t.Errorf("CD: got %q", got)
	}
}

func TestExpandVarsLiteralPercent(t *testing.T) {
	s := New()
	if got := s.expandVars("100%%"); got != "100%" {
		t.Errorf("literal %%%%: got %q", got)
	}
}

func TestExpandVarsParams(t *testing.T) {
	s := New()
	s.params = []string{"script.bat", "one", "two", "three"}
	if got := s.expandVars("%1-%2-%3"); got != "one-two-three" {
		t.Errorf("positional: got %q", got)
	}
	if got := s.expandVars("%*"); got != "one two three" {
		t.Errorf("%%*: got %q", got)
	}
}

func TestDelayedExpansion(t *testing.T) {
	s := New()
	s.delayed = true
	s.env["V"] = "value"
	if got := s.expandDelayed("!V!"); got != "value" {
		t.Errorf("delayed: got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Arithmetic
// ---------------------------------------------------------------------------

func TestSetArithmetic(t *testing.T) {
	s := New()
	captureOutput(func() { s.setArithmetic("x=2+3*4") })
	if s.env["X"] != "14" {
		t.Errorf("precedence: X=%q, want 14", s.env["X"])
	}
	captureOutput(func() { s.setArithmetic("y=(2+3)*4") })
	if s.env["Y"] != "20" {
		t.Errorf("parens: Y=%q, want 20", s.env["Y"])
	}
	captureOutput(func() { s.setArithmetic("z=17%5") })
	if s.env["Z"] != "2" {
		t.Errorf("modulo: Z=%q, want 2", s.env["Z"])
	}
}

func TestSetArithmeticCompound(t *testing.T) {
	s := New()
	s.env["N"] = "10"
	captureOutput(func() { s.setArithmetic("N+=5") })
	if s.env["N"] != "15" {
		t.Errorf("compound +=: N=%q, want 15", s.env["N"])
	}
}

func TestSetArithmeticVariables(t *testing.T) {
	s := New()
	s.env["A"] = "6"
	s.env["B"] = "7"
	captureOutput(func() { s.setArithmetic("c=a*b") })
	if s.env["C"] != "42" {
		t.Errorf("var refs: C=%q, want 42", s.env["C"])
	}
}

// ---------------------------------------------------------------------------
// IF comparisons
// ---------------------------------------------------------------------------

func TestIfComparisonOperators(t *testing.T) {
	out := runBatch(t, `@ECHO OFF
IF 5 GEQ 3 ECHO ge
IF 2 LSS 10 ECHO lt
IF 4 EQU 4 ECHO eq
IF 4 NEQ 5 ECHO ne
IF 9 LEQ 9 ECHO le
IF 8 GTR 2 ECHO gt
`)
	for _, want := range []string{"ge", "lt", "eq", "ne", "le", "gt"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestIfNumericVsString(t *testing.T) {
	// 10 GTR 9 numerically is true; lexically "10" < "9" would be false.
	out := runBatch(t, "@ECHO OFF\nIF 10 GTR 9 ECHO numeric-correct\n")
	if !strings.Contains(out, "numeric-correct") {
		t.Errorf("numeric comparison failed:\n%s", out)
	}
}

func TestIfElseBlock(t *testing.T) {
	out := runBatch(t, `@ECHO OFF
IF 1 EQU 2 (
  ECHO then-branch
) ELSE (
  ECHO else-branch
)
`)
	if strings.Contains(out, "then-branch") {
		t.Error("then-branch should not run")
	}
	if !strings.Contains(out, "else-branch") {
		t.Errorf("else-branch missing:\n%s", out)
	}
}

func TestIfDefined(t *testing.T) {
	out := runBatch(t, `@ECHO OFF
SET foo=bar
IF DEFINED foo ECHO is-defined
IF NOT DEFINED baz ECHO not-defined
`)
	if !strings.Contains(out, "is-defined") || !strings.Contains(out, "not-defined") {
		t.Errorf("IF DEFINED failed:\n%s", out)
	}
}

func TestIfCaseInsensitive(t *testing.T) {
	out := runBatch(t, "@ECHO OFF\nIF /I \"ABC\"==\"abc\" ECHO ci-match\n")
	if !strings.Contains(out, "ci-match") {
		t.Errorf("IF /I failed:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// FOR variants
// ---------------------------------------------------------------------------

func TestForList(t *testing.T) {
	out := runBatch(t, "@ECHO OFF\nFOR %%A IN (x y z) DO ECHO item-%%A\n")
	for _, w := range []string{"item-x", "item-y", "item-z"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q:\n%s", w, out)
		}
	}
}

func TestForL(t *testing.T) {
	out := runBatch(t, "@ECHO OFF\nFOR /L %%I IN (1,1,4) DO ECHO n=%%I\n")
	for _, w := range []string{"n=1", "n=2", "n=3", "n=4"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "n=5") {
		t.Error("loop overran")
	}
}

func TestForLReverse(t *testing.T) {
	out := runBatch(t, "@ECHO OFF\nFOR /L %%I IN (3,-1,1) DO ECHO r=%%I\n")
	idx3 := strings.Index(out, "r=3")
	idx1 := strings.Index(out, "r=1")
	if idx3 < 0 || idx1 < 0 || idx3 > idx1 {
		t.Errorf("reverse loop wrong order:\n%s", out)
	}
}

func TestForFTokens(t *testing.T) {
	dir := t.TempDir()
	csv := filepath.Join(dir, "d.csv")
	os.WriteFile(csv, []byte("alpha;1\nbeta;2\n"), 0644)

	s := New()
	s.cwd = dir
	out := captureOutput(func() {
		bat := filepath.Join(dir, "s.bat")
		os.WriteFile(bat, []byte("@ECHO OFF\nFOR /F \"tokens=1,2 delims=;\" %%A IN (d.csv) DO ECHO %%A=%%B\n"), 0644)
		s.runBatchFile(bat)
	})
	if !strings.Contains(out, "alpha=1") || !strings.Contains(out, "beta=2") {
		t.Errorf("FOR /F tokens/delims failed:\n%s", out)
	}
}

func TestForFString(t *testing.T) {
	out := runBatch(t, "@ECHO OFF\nFOR /F \"tokens=2\" %%W IN (\"one two three\") DO ECHO w=%%W\n")
	if !strings.Contains(out, "w=two") {
		t.Errorf("FOR /F string failed:\n%s", out)
	}
}

func TestForFSkip(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "lines.txt")
	os.WriteFile(f, []byte("header\nreal1\nreal2\n"), 0644)
	s := New()
	s.cwd = dir
	out := captureOutput(func() {
		bat := filepath.Join(dir, "s.bat")
		os.WriteFile(bat, []byte("@ECHO OFF\nFOR /F \"skip=1\" %%L IN (lines.txt) DO ECHO got-%%L\n"), 0644)
		s.runBatchFile(bat)
	})
	if strings.Contains(out, "got-header") {
		t.Error("skip=1 should skip the header")
	}
	if !strings.Contains(out, "got-real1") || !strings.Contains(out, "got-real2") {
		t.Errorf("FOR /F skip failed:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// CALL / GOTO / EXIT /B
// ---------------------------------------------------------------------------

func TestCallSubroutine(t *testing.T) {
	out := runBatch(t, `@ECHO OFF
CALL :sub first
ECHO after-call
GOTO :eof
:sub
ECHO in-sub-%1
EXIT /B 0
`)
	if !strings.Contains(out, "in-sub-first") {
		t.Errorf("subroutine not called:\n%s", out)
	}
	if !strings.Contains(out, "after-call") {
		t.Errorf("did not return from CALL:\n%s", out)
	}
}

func TestGotoEofEndsScript(t *testing.T) {
	out := runBatch(t, `@ECHO OFF
ECHO before
GOTO :eof
ECHO should-not-print
`)
	if !strings.Contains(out, "before") {
		t.Errorf("missing before:\n%s", out)
	}
	if strings.Contains(out, "should-not-print") {
		t.Error("GOTO :EOF did not end the script")
	}
}

func TestGotoLabel(t *testing.T) {
	out := runBatch(t, `@ECHO OFF
GOTO target
ECHO skipped
:target
ECHO reached
`)
	if strings.Contains(out, "skipped") {
		t.Error("GOTO did not skip")
	}
	if !strings.Contains(out, "reached") {
		t.Errorf("label not reached:\n%s", out)
	}
}

func TestShift(t *testing.T) {
	out := runBatch(t, `@ECHO OFF
ECHO %1
SHIFT
ECHO %1
`, "a", "b", "c")
	// After SHIFT, %1 becomes the old %2.
	if !strings.Contains(out, "a") || !strings.Contains(out, "b") {
		t.Errorf("SHIFT failed:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// SETLOCAL / delayed expansion in a loop
// ---------------------------------------------------------------------------

func TestSetlocalDelayedInLoop(t *testing.T) {
	out := runBatch(t, `@ECHO OFF
SETLOCAL ENABLEDELAYEDEXPANSION
SET total=0
FOR %%N IN (1 2 3 4) DO (
  SET /A total+=%%N >NUL
  ECHO running=!total!
)
ECHO final=!total!
ENDLOCAL
`)
	if !strings.Contains(out, "running=1") || !strings.Contains(out, "running=10") {
		t.Errorf("delayed expansion in loop failed:\n%s", out)
	}
	if !strings.Contains(out, "final=10") {
		t.Errorf("accumulation failed:\n%s", out)
	}
}

func TestSetlocalRestoresEnv(t *testing.T) {
	out := runBatch(t, `@ECHO OFF
SET keep=original
SETLOCAL
SET keep=changed
ECHO inside=%keep%
ENDLOCAL
ECHO outside=%keep%
`)
	if !strings.Contains(out, "inside=changed") {
		t.Errorf("setlocal change not visible:\n%s", out)
	}
	if !strings.Contains(out, "outside=original") {
		t.Errorf("ENDLOCAL did not restore env:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// Redirection
// ---------------------------------------------------------------------------

func TestRedirectToFile(t *testing.T) {
	dir := t.TempDir()
	s := New()
	s.cwd = dir
	s.execute("ECHO hello > out.txt")
	data, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "hello" {
		t.Errorf("redirect content = %q", data)
	}
}

func TestRedirectAppend(t *testing.T) {
	dir := t.TempDir()
	s := New()
	s.cwd = dir
	s.execute("ECHO line1 > out.txt")
	s.execute("ECHO line2 >> out.txt")
	data, _ := os.ReadFile(filepath.Join(dir, "out.txt"))
	if !strings.Contains(string(data), "line1") || !strings.Contains(string(data), "line2") {
		t.Errorf("append failed: %q", data)
	}
}

func TestRedirectToNul(t *testing.T) {
	s := New()
	// Output should be swallowed, not appear on our captured stdout.
	out := captureOutput(func() { s.execute("ECHO secret > NUL") })
	if strings.Contains(out, "secret") {
		t.Errorf("NUL redirect leaked output: %q", out)
	}
}

func TestParseRedirection(t *testing.T) {
	cmd, r := parseRedirection("echo hi > file.txt")
	if r == nil || r.outPath != "file.txt" || strings.TrimSpace(cmd) != "echo hi" {
		t.Errorf("basic: cmd=%q redir=%+v", cmd, r)
	}
	cmd2, r2 := parseRedirection("cmd 2>&1")
	if r2 == nil || !r2.errToOut || strings.TrimSpace(cmd2) != "cmd" {
		t.Errorf("2>&1: cmd=%q redir=%+v", cmd2, r2)
	}
	_, r3 := parseRedirection("plain command")
	if r3 != nil {
		t.Errorf("no redirection should return nil, got %+v", r3)
	}
}

// ---------------------------------------------------------------------------
// Operators & escaping
// ---------------------------------------------------------------------------

func TestOrOperator(t *testing.T) {
	// First command fails (missing file) -> the || branch runs.
	out := runBatch(t, "@ECHO OFF\nTYPE nosuchfile.xyz 2>NUL || ECHO fallback\n")
	if !strings.Contains(out, "fallback") {
		t.Errorf("|| did not run on failure:\n%s", out)
	}
}

func TestOrOperatorSkipOnSuccess(t *testing.T) {
	out := runBatch(t, "@ECHO OFF\nECHO ok || ECHO should-not-run\n")
	if strings.Contains(out, "should-not-run") {
		t.Error("|| ran after success")
	}
}

func TestCaretEscape(t *testing.T) {
	// The caret escapes the & so it is not treated as a command separator.
	out := runBatch(t, "@ECHO OFF\nECHO A ^& B\n")
	if !strings.Contains(out, "A & B") {
		t.Errorf("caret escape failed:\n%s", out)
	}
}

func TestColonColonComment(t *testing.T) {
	out := runBatch(t, "@ECHO OFF\n:: this is a comment\nECHO visible\n")
	if strings.Contains(out, "this is a comment") {
		t.Error(":: comment was executed")
	}
	if !strings.Contains(out, "visible") {
		t.Errorf("line after :: comment missing:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// Parenthesis / paren-depth helpers
// ---------------------------------------------------------------------------

func TestParenDepth(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"echo hi (", 1},
		{") else (", 0},
		{"balanced (x)", 0},
		{")", -1},
		{`echo "(" `, 0}, // paren in quotes ignored
	}
	for _, c := range cases {
		if got := parenDepth(c.s); got != c.want {
			t.Errorf("parenDepth(%q) = %d, want %d", c.s, got, c.want)
		}
	}
}

func TestExtractBlock(t *testing.T) {
	inner, after := extractBlock("(a b c) rest")
	if inner != "a b c" || strings.TrimSpace(after) != "rest" {
		t.Errorf("extractBlock: inner=%q after=%q", inner, after)
	}
}
