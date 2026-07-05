package shell

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// captureOutput redirects os.Stdout for the duration of fn and returns what
// was written.  It restores Stdout even if fn panics.
func captureOutput(fn func()) string {
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() {
		os.Stdout = old
	}()
	fn()
	w.Close()
	var buf strings.Builder
	tmp := make([]byte, 4096)
	for {
		n, _ := r.Read(tmp)
		if n == 0 {
			break
		}
		buf.Write(tmp[:n])
	}
	r.Close()
	return buf.String()
}

// newTestShell creates a Shell rooted in a fresh temp directory.
func newTestShell(t *testing.T) (*Shell, string) {
	t.Helper()
	dir := t.TempDir()
	s := New()
	s.cwd = dir
	return s, dir
}

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

func TestTokenize(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"ECHO hello", []string{"ECHO", "hello"}},
		{`ECHO "hello world"`, []string{"ECHO", "hello world"}},
		{`COPY "a b" dest`, []string{"COPY", "a b", "dest"}},
		{"  SET  X=1  ", []string{"SET", "X=1"}},
	}
	for _, c := range cases {
		got := tokenize(c.input)
		if len(got) != len(c.want) {
			t.Errorf("tokenize(%q) = %v, want %v", c.input, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("tokenize(%q)[%d] = %q, want %q", c.input, i, got[i], c.want[i])
			}
		}
	}
}

func TestSplitCommands(t *testing.T) {
	segs := splitCommands("DIR & ECHO hi")
	if len(segs) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(segs))
	}
	if strings.TrimSpace(segs[0].cmd) != "DIR" || segs[0].op != "&" {
		t.Errorf("seg[0] = %+v", segs[0])
	}
	if strings.TrimSpace(segs[1].cmd) != "ECHO hi" || segs[1].op != "" {
		t.Errorf("seg[1] = %+v", segs[1])
	}

	// && operator
	segs2 := splitCommands("CD foo && ECHO ok")
	if len(segs2) != 2 || segs2[0].op != "&&" {
		t.Errorf("&&: got %+v", segs2)
	}

	// & inside quotes must not split
	segs3 := splitCommands(`ECHO "a & b"`)
	if len(segs3) != 1 {
		t.Errorf("quoted & should not split, got %d segments", len(segs3))
	}
}

func TestSplitFirstToken(t *testing.T) {
	tok, rest := splitFirstToken("hello world more")
	if tok != "hello" || rest != "world more" {
		t.Errorf("got %q, %q", tok, rest)
	}
	tok2, rest2 := splitFirstToken("single")
	if tok2 != "single" || rest2 != "" {
		t.Errorf("got %q, %q", tok2, rest2)
	}
	tok3, rest3 := splitFirstToken("")
	if tok3 != "" || rest3 != "" {
		t.Errorf("got %q, %q", tok3, rest3)
	}
}

func TestFormatSize(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{1234567, "1,234,567"},
		{1000000000, "1,000,000,000"},
	}
	for _, c := range cases {
		if got := formatSize(c.n); got != c.want {
			t.Errorf("formatSize(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestIndexFold(t *testing.T) {
	if indexFold("Hello World", "world") != 6 {
		t.Error("expected 6")
	}
	if indexFold("Hello World", "HELLO") != 0 {
		t.Error("expected 0")
	}
	if indexFold("Hello", "xyz") != -1 {
		t.Error("expected -1")
	}
}

// ---------------------------------------------------------------------------
// Shell path helpers
// ---------------------------------------------------------------------------

func TestDosPath(t *testing.T) {
	s := New()
	if got := s.dosPath("/"); got != `C:\` {
		t.Errorf("root: got %q", got)
	}
	got := s.dosPath("/home/user")
	if got != `C:\home\user` {
		t.Errorf("got %q", got)
	}
}

func TestAbsPath(t *testing.T) {
	s, dir := newTestShell(t)

	// Absolute Linux path passes through unchanged.
	if got := s.absPath(dir); got != dir {
		t.Errorf("abs passthrough: got %q", got)
	}

	// Relative path is anchored to cwd.
	want := filepath.Join(dir, "sub")
	if got := s.absPath("sub"); got != want {
		t.Errorf("relative: got %q, want %q", got, want)
	}

	// DOS backslash is converted.
	if got := s.absPath(`sub\child`); got != filepath.Join(dir, "sub", "child") {
		t.Errorf("backslash: got %q", got)
	}

	// Virtual C: drive prefix is stripped.
	if got := s.absPath(`C:\tmp`); got != "/tmp" {
		t.Errorf("C: prefix: got %q", got)
	}
}

// ---------------------------------------------------------------------------
// %RANDOM% and expandVars
// ---------------------------------------------------------------------------

func TestExpandVarsSimple(t *testing.T) {
	s := New()
	s.env["NAME"] = "World"
	got := s.expandVars("Hello %NAME%!")
	if got != "Hello World!" {
		t.Errorf("got %q", got)
	}
}

func TestExpandVarsUndefined(t *testing.T) {
	s := New()
	got := s.expandVars("%UNDEFINED%")
	if got != "" {
		t.Errorf("undefined var should expand to empty, got %q", got)
	}
}

func TestExpandVarsRandom(t *testing.T) {
	s := New()
	vals := make(map[int]bool)
	for i := 0; i < 20; i++ {
		raw := s.expandVars("%RANDOM%")
		n, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("%%RANDOM%% produced non-integer %q", raw)
		}
		if n < 0 || n > 32767 {
			t.Errorf("%%RANDOM%% = %d, want [0,32767]", n)
		}
		vals[n] = true
	}
	// 20 draws from [0,32767] — extremely unlikely to all collide.
	if len(vals) < 5 {
		t.Errorf("%%RANDOM%% looks non-random: only %d distinct values in 20 draws", len(vals))
	}
}

func TestExpandVarsRandomNotOverridable(t *testing.T) {
	s := New()
	s.env["RANDOM"] = "999"
	raw := s.expandVars("%RANDOM%")
	if raw == "999" {
		t.Error("SET RANDOM should not override the dynamic %%RANDOM%% variable")
	}
}

// ---------------------------------------------------------------------------
// CD / CHDIR
// ---------------------------------------------------------------------------

func TestCmdCdValid(t *testing.T) {
	s, dir := newTestShell(t)
	sub := filepath.Join(dir, "subdir")
	os.Mkdir(sub, 0755)

	s.execute("CD subdir")
	if s.cwd != sub {
		t.Errorf("cwd = %q, want %q", s.cwd, sub)
	}
	if s.code != 0 {
		t.Errorf("code = %d", s.code)
	}
}

func TestCmdCdNonexistent(t *testing.T) {
	s, _ := newTestShell(t)
	s.execute("CD does_not_exist")
	if s.code == 0 {
		t.Error("expected non-zero exit code for missing directory")
	}
}

func TestCmdCdNoArgs(t *testing.T) {
	s, dir := newTestShell(t)
	out := captureOutput(func() { s.execute("CD") })
	if !strings.Contains(out, "C:\\") {
		t.Errorf("CD with no args should print cwd, got %q", out)
	}
	if s.cwd != dir {
		t.Error("CD with no args must not change cwd")
	}
}

// ---------------------------------------------------------------------------
// MD / MKDIR  and  RD / RMDIR
// ---------------------------------------------------------------------------

func TestCmdMkdirAndRmdir(t *testing.T) {
	s, dir := newTestShell(t)

	s.execute("MD newdir")
	if s.code != 0 {
		t.Fatalf("MD failed: code %d", s.code)
	}
	info, err := os.Stat(filepath.Join(dir, "newdir"))
	if err != nil || !info.IsDir() {
		t.Fatal("directory was not created")
	}

	s.execute("RD newdir")
	if s.code != 0 {
		t.Fatalf("RD failed: code %d", s.code)
	}
	if _, err := os.Stat(filepath.Join(dir, "newdir")); !os.IsNotExist(err) {
		t.Error("directory still exists after RD")
	}
}

func TestCmdRmdirRecursive(t *testing.T) {
	s, dir := newTestShell(t)
	tree := filepath.Join(dir, "a", "b")
	os.MkdirAll(tree, 0755)
	os.WriteFile(filepath.Join(tree, "f.txt"), []byte("x"), 0644)

	s.execute("RD /S a")
	if s.code != 0 {
		t.Fatalf("RD /S failed: code %d", s.code)
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); !os.IsNotExist(err) {
		t.Error("tree still exists after RD /S")
	}
}

// ---------------------------------------------------------------------------
// SET
// ---------------------------------------------------------------------------

func TestCmdSet(t *testing.T) {
	s := New()

	s.execute("SET FOO=bar")
	if s.env["FOO"] != "bar" {
		t.Errorf("SET FOO=bar: got %q", s.env["FOO"])
	}

	// Delete by setting to empty.
	s.execute("SET FOO=")
	if _, ok := s.env["FOO"]; ok {
		t.Error("SET FOO= should delete the variable")
	}
}

func TestCmdSetDisplay(t *testing.T) {
	s := New()
	s.env = map[string]string{"ALPHA": "1", "BETA": "2"}
	out := captureOutput(func() { s.execute("SET ALPHA") })
	if !strings.Contains(out, "ALPHA=1") {
		t.Errorf("SET ALPHA should display the variable, got %q", out)
	}
}

// ---------------------------------------------------------------------------
// DOSKEY
// ---------------------------------------------------------------------------

func TestCmdDoskey(t *testing.T) {
	s := New()
	s.execute("DOSKEY LL=DIR /W")
	if s.aliases["LL"] != "DIR /W" {
		t.Errorf("macro not set, got %q", s.aliases["LL"])
	}

	// Clear macro.
	s.execute("DOSKEY LL=")
	if _, ok := s.aliases["LL"]; ok {
		t.Error("clearing macro failed")
	}
}

// ---------------------------------------------------------------------------
// EXIT
// ---------------------------------------------------------------------------

func TestCmdExit(t *testing.T) {
	s := New()
	s.execute("EXIT 42")
	if !s.exit {
		t.Error("exit flag not set")
	}
	if s.code != 42 {
		t.Errorf("exit code = %d, want 42", s.code)
	}
}

// ---------------------------------------------------------------------------
// DEL / COPY / MOVE / REN
// ---------------------------------------------------------------------------

func TestCmdDel(t *testing.T) {
	s, dir := newTestShell(t)
	f := filepath.Join(dir, "todel.txt")
	os.WriteFile(f, []byte("bye"), 0644)

	s.execute("DEL todel.txt")
	if s.code != 0 {
		t.Fatalf("DEL failed: code %d", s.code)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Error("file still exists after DEL")
	}
}

func TestCmdDelMissing(t *testing.T) {
	s, _ := newTestShell(t)
	s.execute("DEL ghost.txt")
	if s.code == 0 {
		t.Error("expected non-zero code for missing file")
	}
}

func TestCmdCopy(t *testing.T) {
	s, dir := newTestShell(t)
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	os.WriteFile(src, []byte("hello"), 0644)

	s.execute("COPY src.txt dst.txt")
	if s.code != 0 {
		t.Fatalf("COPY failed: code %d", s.code)
	}
	data, _ := os.ReadFile(dst)
	if string(data) != "hello" {
		t.Errorf("COPY: dst content = %q", data)
	}
}

func TestCmdMove(t *testing.T) {
	s, dir := newTestShell(t)
	src := filepath.Join(dir, "old.txt")
	os.WriteFile(src, []byte("data"), 0644)

	s.execute("MOVE old.txt new.txt")
	if s.code != 0 {
		t.Fatalf("MOVE failed: code %d", s.code)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source still exists after MOVE")
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); err != nil {
		t.Error("destination does not exist after MOVE")
	}
}

func TestCmdRename(t *testing.T) {
	s, dir := newTestShell(t)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0644)

	s.execute("REN a.txt b.txt")
	if s.code != 0 {
		t.Fatalf("REN failed: code %d", s.code)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.txt")); err != nil {
		t.Error("renamed file does not exist")
	}
}

// ---------------------------------------------------------------------------
// ECHO
// ---------------------------------------------------------------------------

func TestCmdEcho(t *testing.T) {
	s, _ := newTestShell(t)
	out := captureOutput(func() { s.execute("ECHO Hello World") })
	if !strings.Contains(out, "Hello World") {
		t.Errorf("ECHO: got %q", out)
	}
}

func TestCmdEchoEmpty(t *testing.T) {
	s := New()
	out := captureOutput(func() { s.execute("ECHO") })
	if !strings.Contains(strings.ToUpper(out), "ECHO") {
		t.Errorf("ECHO with no args: got %q", out)
	}
}

// ---------------------------------------------------------------------------
// TYPE
// ---------------------------------------------------------------------------

func TestCmdType(t *testing.T) {
	s, dir := newTestShell(t)
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("line1\nline2\n"), 0644)

	out := captureOutput(func() { s.execute("TYPE hello.txt") })
	if !strings.Contains(out, "line1") || !strings.Contains(out, "line2") {
		t.Errorf("TYPE: got %q", out)
	}
}

func TestCmdTypeMissing(t *testing.T) {
	s, _ := newTestShell(t)
	s.execute("TYPE ghost.txt")
	if s.code == 0 {
		t.Error("expected non-zero code for missing file")
	}
}

// ---------------------------------------------------------------------------
// VER
// ---------------------------------------------------------------------------

func TestCmdVer(t *testing.T) {
	s := New()
	out := captureOutput(func() { s.execute("VER") })
	if !strings.Contains(out, "DOS Shell") {
		t.Errorf("VER: got %q", out)
	}
}

// ---------------------------------------------------------------------------
// MEM
// ---------------------------------------------------------------------------

func TestCmdMem(t *testing.T) {
	s := New()
	out := captureOutput(func() { s.execute("MEM") })
	for _, want := range []string{"Total memory", "Total in use", "Total free", "Physical Memory"} {
		if !strings.Contains(out, want) {
			t.Errorf("MEM output missing %q\nfull output:\n%s", want, out)
		}
	}
}

// ---------------------------------------------------------------------------
// FIND
// ---------------------------------------------------------------------------

func TestCmdFind(t *testing.T) {
	s, dir := newTestShell(t)
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("apple\nbanana\nApple\n"), 0644)

	// Exact match (case sensitive).
	out := captureOutput(func() { s.execute(`FIND "apple" f.txt`) })
	if !strings.Contains(out, "apple") {
		t.Errorf("FIND: missing match, got %q", out)
	}
	if strings.Contains(out, "Apple") {
		t.Error("FIND: should be case-sensitive by default")
	}

	// /I — case insensitive.
	out2 := captureOutput(func() { s.execute(`FIND /I "apple" f.txt`) })
	if !strings.Contains(out2, "Apple") {
		t.Errorf("FIND /I: missing case-insensitive match, got %q", out2)
	}

	// /C — count only.
	out3 := captureOutput(func() { s.execute(`FIND /C "apple" f.txt`) })
	if !strings.Contains(out3, "1") {
		t.Errorf("FIND /C: expected count 1, got %q", out3)
	}
}

// ---------------------------------------------------------------------------
// FINDSTR
// ---------------------------------------------------------------------------

func TestCmdFindstrLiteral(t *testing.T) {
	s, dir := newTestShell(t)
	os.WriteFile(filepath.Join(dir, "g.txt"), []byte("foo bar\nbaz qux\n"), 0644)

	out := captureOutput(func() { s.execute("FINDSTR foo g.txt") })
	if !strings.Contains(out, "foo bar") {
		t.Errorf("FINDSTR literal: got %q", out)
	}
	if strings.Contains(out, "baz") {
		t.Error("FINDSTR literal: non-matching line should not appear")
	}
}

func TestCmdFindstrRegex(t *testing.T) {
	s, dir := newTestShell(t)
	os.WriteFile(filepath.Join(dir, "h.txt"), []byte("cat\ncar\nbar\n"), 0644)

	out := captureOutput(func() { s.execute("FINDSTR /R ca. h.txt") })
	if !strings.Contains(out, "cat") || !strings.Contains(out, "car") {
		t.Errorf("FINDSTR /R: got %q", out)
	}
	if strings.Contains(out, "bar") {
		t.Error("FINDSTR /R: 'bar' should not match 'ca.'")
	}
}

func TestCmdFindstrLineNumbers(t *testing.T) {
	s, dir := newTestShell(t)
	os.WriteFile(filepath.Join(dir, "n.txt"), []byte("alpha\nbeta\ngamma\n"), 0644)

	out := captureOutput(func() { s.execute("FINDSTR /N beta n.txt") })
	if !strings.Contains(out, "2:") {
		t.Errorf("FINDSTR /N: expected line number 2, got %q", out)
	}
}

func TestCmdFindstrFilenameOnly(t *testing.T) {
	s, dir := newTestShell(t)
	os.WriteFile(filepath.Join(dir, "m.txt"), []byte("match here\n"), 0644)
	os.WriteFile(filepath.Join(dir, "no.txt"), []byte("nothing\n"), 0644)

	out := captureOutput(func() { s.execute("FINDSTR /M match m.txt no.txt") })
	if !strings.Contains(out, "m.txt") {
		t.Errorf("FINDSTR /M: missing filename, got %q", out)
	}
	if strings.Contains(out, "no.txt") {
		t.Error("FINDSTR /M: non-matching file should not appear")
	}
}

func TestCmdFindstrCaseInsensitive(t *testing.T) {
	s, dir := newTestShell(t)
	os.WriteFile(filepath.Join(dir, "i.txt"), []byte("Hello\nworld\n"), 0644)

	out := captureOutput(func() { s.execute("FINDSTR /I HELLO i.txt") })
	if !strings.Contains(out, "Hello") {
		t.Errorf("FINDSTR /I: got %q", out)
	}
}

func TestCmdFindstrNoMatch(t *testing.T) {
	s, dir := newTestShell(t)
	os.WriteFile(filepath.Join(dir, "z.txt"), []byte("nothing\n"), 0644)

	captureOutput(func() { s.execute("FINDSTR xyz z.txt") })
	if s.code == 0 {
		t.Error("FINDSTR with no match should set non-zero exit code")
	}
}

// ---------------------------------------------------------------------------
// && command chaining
// ---------------------------------------------------------------------------

func TestChainAndAnd(t *testing.T) {
	s, dir := newTestShell(t)
	os.WriteFile(filepath.Join(dir, "first.txt"), []byte("x"), 0644)

	// First command succeeds → second runs.
	out := captureOutput(func() { s.executeLineWithEcho("TYPE first.txt && ECHO after", false) })
	if !strings.Contains(out, "after") {
		t.Errorf("&& after success: second command did not run, got %q", out)
	}

	// First command fails → second must NOT run.
	out2 := captureOutput(func() { s.executeLineWithEcho("TYPE missing.txt && ECHO should_not_appear", false) })
	if strings.Contains(out2, "should_not_appear") {
		t.Error("&& after failure: second command should not have run")
	}
}

// ---------------------------------------------------------------------------
// Batch processor
// ---------------------------------------------------------------------------

func TestBatchEchoOff(t *testing.T) {
	s, dir := newTestShell(t)
	bat := filepath.Join(dir, "test.bat")
	os.WriteFile(bat, []byte("@ECHO OFF\nECHO visible\n"), 0644)

	out := captureOutput(func() { s.runBatchFile(bat) })
	// The prompt should NOT be echoed; only the ECHO output should appear.
	if strings.Contains(out, "@ECHO OFF") {
		t.Errorf("@ECHO OFF line should not appear in output, got %q", out)
	}
	if !strings.Contains(out, "visible") {
		t.Errorf("ECHO output should still appear, got %q", out)
	}
}

func TestBatchIfEquals(t *testing.T) {
	s, dir := newTestShell(t)
	bat := filepath.Join(dir, "iftest.bat")
	os.WriteFile(bat, []byte(`@ECHO OFF
IF "1"=="1" ECHO yes
IF "1"=="2" ECHO no
`), 0644)
	out := captureOutput(func() { s.runBatchFile(bat) })
	if !strings.Contains(out, "yes") {
		t.Errorf("IF true branch: got %q", out)
	}
	if strings.Contains(out, "no") {
		t.Errorf("IF false branch should not execute: got %q", out)
	}
}

func TestBatchIfNot(t *testing.T) {
	s, dir := newTestShell(t)
	bat := filepath.Join(dir, "ifnot.bat")
	os.WriteFile(bat, []byte("@ECHO OFF\nIF NOT \"a\"==\"b\" ECHO different\n"), 0644)
	out := captureOutput(func() { s.runBatchFile(bat) })
	if !strings.Contains(out, "different") {
		t.Errorf("IF NOT: got %q", out)
	}
}

func TestBatchIfExist(t *testing.T) {
	s, dir := newTestShell(t)
	target := filepath.Join(dir, "present.txt")
	os.WriteFile(target, []byte{}, 0644)

	bat := filepath.Join(dir, "exist.bat")
	os.WriteFile(bat, []byte("@ECHO OFF\nIF EXIST present.txt ECHO found\nIF EXIST absent.txt ECHO missing\n"), 0644)
	out := captureOutput(func() { s.runBatchFile(bat) })
	if !strings.Contains(out, "found") {
		t.Errorf("IF EXIST present: got %q", out)
	}
	if strings.Contains(out, "missing") {
		t.Errorf("IF EXIST absent: should not print, got %q", out)
	}
}

func TestBatchGoto(t *testing.T) {
	s, dir := newTestShell(t)
	bat := filepath.Join(dir, "goto.bat")
	os.WriteFile(bat, []byte(`@ECHO OFF
GOTO end
ECHO skipped
:end
ECHO reached
`), 0644)
	out := captureOutput(func() { s.runBatchFile(bat) })
	if strings.Contains(out, "skipped") {
		t.Errorf("GOTO: skipped line was executed, got %q", out)
	}
	if !strings.Contains(out, "reached") {
		t.Errorf("GOTO: target label not reached, got %q", out)
	}
}

func TestBatchFor(t *testing.T) {
	s, dir := newTestShell(t)
	bat := filepath.Join(dir, "for.bat")
	os.WriteFile(bat, []byte("@ECHO OFF\nFOR %%F IN (alpha beta gamma) DO ECHO %%F\n"), 0644)
	out := captureOutput(func() { s.runBatchFile(bat) })
	for _, word := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(out, word) {
			t.Errorf("FOR: missing %q in output %q", word, out)
		}
	}
}
