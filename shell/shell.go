// Package shell implements a DOS-like shell for Linux.
package shell

import (
	"bufio"
	cryptorand "crypto/rand"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const version = "DOS Shell 1.0 [Linux]"

// Shell holds the state of the running shell.
type Shell struct {
	env      map[string]string // environment variables
	aliases  map[string]string // DOSKEY-style macros
	cwd      string            // current working directory
	exit     bool              // set to true to exit
	code     int               // exit code
	rng      *rand.Rand        // ChaCha8-backed RNG for %RANDOM%
	params   []string          // batch positional parameters (%0..%n)
	delayed  bool              // delayed !VAR! expansion enabled (SETLOCAL)
	local    []localScope      // SETLOCAL/ENDLOCAL stack
	pushd    []string          // PUSHD/POPD directory stack
	curBatch *batchProcessor   // batch file currently executing (nil if interactive)
}

// localScope is one SETLOCAL frame: a snapshot of the environment restored on
// the matching ENDLOCAL.
type localScope struct {
	env     map[string]string
	delayed bool
}

// New creates and initializes a new Shell.
func New() *Shell {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "/"
	}

	var seed [32]byte
	if _, err := cryptorand.Read(seed[:]); err != nil {
		// Fallback: leave seed as zero — ChaCha8 still works.
		_ = err
	}
	chacha := rand.NewChaCha8(seed)

	s := &Shell{
		env:     make(map[string]string),
		aliases: make(map[string]string),
		cwd:     cwd,
		rng:     rand.New(chacha),
	}

	// Seed env from the OS environment.
	for _, kv := range os.Environ() {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			s.env[strings.ToUpper(parts[0])] = parts[1]
		}
	}

	return s
}

// Run starts the shell.  If args contains a /C or /K flag followed by a
// command string, that command is executed first (mirroring cmd.exe behaviour).
// Otherwise an interactive REPL is started.
func (s *Shell) Run(args []string) {
	switch {
	case len(args) >= 2 && strings.EqualFold(args[0], "/C"):
		// Execute command then exit.
		s.executeLineWithEcho(strings.Join(args[1:], " "), false)
		os.Exit(s.code)
	case len(args) >= 2 && strings.EqualFold(args[0], "/K"):
		// Execute command then drop into interactive mode.
		s.executeLineWithEcho(strings.Join(args[1:], " "), false)
	case len(args) >= 1 && !strings.EqualFold(args[0], "/C") && !strings.EqualFold(args[0], "/K"):
		// Treat the first arg as a batch file and the rest as its parameters.
		// (A leading "/" is fine here — on Linux that is just an absolute path.)
		s.runBatchFile(args[0], args[1:]...)
		os.Exit(s.code)
	}

	s.interactive()
}

// interactive runs the read-eval-print loop.
func (s *Shell) interactive() {
	fmt.Println(version)
	fmt.Println(`Type HELP for a list of commands.`)
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)
	for !s.exit {
		fmt.Print(s.prompt())
		line, err := reader.ReadString('\n')
		if err != nil {
			// EOF (Ctrl-D)
			fmt.Println()
			break
		}
		line = strings.TrimRight(line, "\r\n")
		s.executeLineWithEcho(line, false)
	}
	os.Exit(s.code)
}

// prompt returns the DOS-style prompt string (e.g. "C:\Users\foo>").
func (s *Shell) prompt() string {
	p, ok := s.env["PROMPT"]
	if !ok || p == "" {
		p = "$P$G"
	}
	return s.expandPrompt(p)
}

// expandPrompt converts DOS prompt meta-characters.
func (s *Shell) expandPrompt(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] == '$' && i+1 < len(p) {
			i++
			switch p[i] {
			case 'P', 'p':
				b.WriteString(s.dosPath(s.cwd))
			case 'G', 'g':
				b.WriteByte('>')
			case 'L', 'l':
				b.WriteByte('<')
			case 'B', 'b':
				b.WriteByte('|')
			case 'N', 'n':
				b.WriteByte('C') // "drive letter"
			case 'D', 'd':
				b.WriteString(dateString())
			case 'T', 't':
				b.WriteString(timeString())
			case 'V', 'v':
				b.WriteString(version)
			case '$':
				b.WriteByte('$')
			case '_':
				b.WriteByte('\n')
			case 'S', 's':
				b.WriteByte(' ')
			case 'Q', 'q':
				b.WriteByte('=')
			default:
				b.WriteByte('$')
				b.WriteByte(p[i])
			}
		} else {
			b.WriteByte(p[i])
		}
	}
	return b.String()
}

// dosPath converts a Linux path to DOS-style backslash notation prefixed with
// a virtual "C:" drive.
func (s *Shell) dosPath(path string) string {
	rel, err := filepath.Rel("/", path)
	if err != nil || rel == "." {
		return `C:\`
	}
	return `C:\` + strings.ReplaceAll(rel, "/", `\`)
}

// executeLineWithEcho runs a single input line, optionally echoing it first
// (used by batch processing).
func (s *Shell) executeLineWithEcho(line string, echo bool) {
	// Strip leading @ which suppresses echo in batch mode.
	noEcho := false
	if strings.HasPrefix(line, "@") {
		line = line[1:]
		noEcho = true
	}

	line = strings.TrimSpace(line)

	// Comments: REM ... and the label-style :: ...
	if line == "" || isRem(line) || strings.HasPrefix(line, "::") {
		return
	}

	// IF and FOR must be parsed BEFORE %-expansion so that FOR loop variables
	// (%%v) and comparison operands survive intact.
	upper := strings.ToUpper(line)
	if isWord(upper, "IF") {
		if echo && !noEcho {
			fmt.Printf("%s%s\n", s.prompt(), line)
		}
		s.evalIf(line, s.curBatch)
		return
	}
	if isWord(upper, "FOR") {
		if echo && !noEcho {
			fmt.Printf("%s%s\n", s.prompt(), line)
		}
		s.evalFor(line, s.curBatch)
		return
	}

	// %VAR%, %1, %~dp0, %VAR:~a,b%, %VAR:x=y% expansion.
	line = s.expandVars(line)
	// !VAR! delayed expansion, when enabled by SETLOCAL ENABLEDELAYEDEXPANSION.
	if s.delayed {
		line = s.expandDelayed(line)
	}

	if echo && !noEcho && line != "" {
		fmt.Printf("%s%s\n", s.prompt(), line)
	}

	// Split into individual commands separated by &, && and ||.
	s.executePipeline(line)
}

// isRem reports whether a line is a REM comment.
func isRem(line string) bool {
	if len(line) < 3 {
		return strings.EqualFold(line, "rem")
	}
	if !strings.EqualFold(line[:3], "rem") {
		return false
	}
	return len(line) == 3 || line[3] == ' ' || line[3] == '\t'
}

// expandVars performs %-expansion: literal %%, positional parameters (%0..%9,
// %*, %~modifiers), dynamic variables, substring (%V:~a,b%) and substitution
// (%V:x=y%) forms, and plain %NAME%.
func (s *Shell) expandVars(line string) string {
	var b strings.Builder
	i := 0
	for i < len(line) {
		c := line[i]
		if c != '%' {
			b.WriteByte(c)
			i++
			continue
		}
		// %% -> literal %
		if i+1 < len(line) && line[i+1] == '%' {
			b.WriteByte('%')
			i += 2
			continue
		}
		// %* -> all parameters
		if i+1 < len(line) && line[i+1] == '*' {
			b.WriteString(s.paramStar())
			i += 2
			continue
		}
		// %<digit> -> positional parameter
		if i+1 < len(line) && line[i+1] >= '0' && line[i+1] <= '9' {
			b.WriteString(s.param(int(line[i+1]-'0'), ""))
			i += 2
			continue
		}
		// %~modifiers<digit> -> modified positional parameter
		if i+1 < len(line) && line[i+1] == '~' {
			j := i + 2
			for j < len(line) && isModChar(line[j]) {
				j++
			}
			if j < len(line) && line[j] >= '0' && line[j] <= '9' {
				mods := line[i+2 : j]
				b.WriteString(s.param(int(line[j]-'0'), mods))
				i = j + 1
				continue
			}
			// Not a parameter reference — emit literally.
			b.WriteByte('%')
			i++
			continue
		}
		// %NAME%, %NAME:~a,b%, %NAME:x=y%
		if end := strings.IndexByte(line[i+1:], '%'); end >= 0 {
			inner := line[i+1 : i+1+end]
			b.WriteString(s.expandNamed(inner))
			i = i + 1 + end + 1
			continue
		}
		// Unterminated % — literal.
		b.WriteByte('%')
		i++
	}
	return b.String()
}

// expandDelayed performs !VAR!, !VAR:~a,b! and !VAR:x=y! expansion.
func (s *Shell) expandDelayed(line string) string {
	var b strings.Builder
	i := 0
	for i < len(line) {
		if line[i] == '!' {
			if end := strings.IndexByte(line[i+1:], '!'); end >= 0 {
				inner := line[i+1 : i+1+end]
				b.WriteString(s.expandNamed(inner))
				i = i + 1 + end + 1
				continue
			}
		}
		b.WriteByte(line[i])
		i++
	}
	return b.String()
}

// isModChar reports whether c is a valid %~ modifier letter.
func isModChar(c byte) bool {
	switch c {
	case 'f', 'd', 'p', 'n', 'x', 's', 'a', 't', 'z':
		return true
	}
	return false
}

// expandNamed resolves the text between a pair of % (or !) delimiters, handling
// substring and substitution modifiers.
func (s *Shell) expandNamed(inner string) string {
	if ci := strings.IndexByte(inner, ':'); ci >= 0 {
		name := inner[:ci]
		spec := inner[ci+1:]
		base := s.lookup(name)
		if strings.HasPrefix(spec, "~") {
			return substring(base, spec[1:])
		}
		// Substitution: find=replace (case-insensitive on find, like cmd).
		if eq := strings.IndexByte(spec, '='); eq >= 0 {
			find := spec[:eq]
			repl := spec[eq+1:]
			return substitute(base, find, repl)
		}
		return base
	}
	return s.lookup(inner)
}

// lookup resolves a variable name, including cmd's dynamic pseudo-variables.
func (s *Shell) lookup(name string) string {
	switch strings.ToUpper(name) {
	case "RANDOM":
		return strconv.Itoa(s.rng.IntN(32768))
	case "ERRORLEVEL":
		return strconv.Itoa(s.code)
	case "CD":
		return s.dosPath(s.cwd)
	case "DATE":
		return dateString()
	case "TIME":
		return timeString()
	case "CMDCMDLINE":
		return version
	}
	return s.env[strings.ToUpper(name)]
}

// substring implements the %VAR:~start,len% offset syntax.
func substring(base, spec string) string {
	r := []rune(base)
	n := len(r)
	start, length := 0, n
	hasLen := false
	if comma := strings.IndexByte(spec, ','); comma >= 0 {
		start, _ = strconv.Atoi(strings.TrimSpace(spec[:comma]))
		length, _ = strconv.Atoi(strings.TrimSpace(spec[comma+1:]))
		hasLen = true
	} else {
		start, _ = strconv.Atoi(strings.TrimSpace(spec))
	}
	if start < 0 {
		start = n + start
		if start < 0 {
			start = 0
		}
	}
	if start > n {
		return ""
	}
	end := n
	if hasLen {
		if length < 0 {
			end = n + length
		} else {
			end = start + length
		}
	}
	if end > n {
		end = n
	}
	if end < start {
		return ""
	}
	return string(r[start:end])
}

// substitute implements %VAR:find=replace% (case-insensitive find). A find that
// begins with * replaces everything up to and including the first match.
func substitute(base, find, repl string) string {
	if find == "" {
		return base
	}
	if strings.HasPrefix(find, "*") {
		needle := find[1:]
		if needle == "" {
			return base
		}
		if idx := indexFold(base, needle); idx >= 0 {
			return repl + base[idx+len(needle):]
		}
		return base
	}
	// Case-insensitive replace-all.
	var b strings.Builder
	for {
		idx := indexFold(base, find)
		if idx < 0 {
			b.WriteString(base)
			break
		}
		b.WriteString(base[:idx])
		b.WriteString(repl)
		base = base[idx+len(find):]
	}
	return b.String()
}

// executePipeline handles &, && and || command chaining.
func (s *Shell) executePipeline(line string) {
	cmds := splitCommands(line)
	prevOK := true
	prevOp := ""
	for _, seg := range cmds {
		// op is stored on the LEFT segment of each operator, so to decide
		// whether to run THIS segment we check the previous segment's op:
		//   &&  run only if the previous command succeeded
		//   ||  run only if the previous command failed
		if prevOp == "&&" && !prevOK {
			prevOp = seg.op
			continue
		}
		if prevOp == "||" && prevOK {
			prevOp = seg.op
			continue
		}
		s.execute(strings.TrimSpace(seg.cmd))
		prevOK = (s.code == 0)
		prevOp = seg.op
	}
}

type cmdSeg struct {
	cmd string
	op  string // "" | "&" | "&&" | "||"
}

// splitCommands splits a line on &, && and || operators (outside quotes and
// outside parenthesised blocks, and respecting the ^ escape character).
func splitCommands(line string) []cmdSeg {
	var segs []cmdSeg
	var buf strings.Builder
	inQ := false
	depth := 0
	i := 0
	for i < len(line) {
		c := line[i]
		if c == '^' && i+1 < len(line) {
			// Caret escapes the next character.
			buf.WriteByte(line[i+1])
			i += 2
			continue
		}
		if c == '"' {
			inQ = !inQ
			buf.WriteByte(c)
			i++
			continue
		}
		if !inQ && c == '(' {
			depth++
			buf.WriteByte(c)
			i++
			continue
		}
		if !inQ && c == ')' && depth > 0 {
			depth--
			buf.WriteByte(c)
			i++
			continue
		}
		if !inQ && depth == 0 && c == '|' && i+1 < len(line) && line[i+1] == '|' {
			segs = append(segs, cmdSeg{cmd: buf.String(), op: "||"})
			buf.Reset()
			i += 2
			continue
		}
		if !inQ && depth == 0 && c == '&' {
			if i+1 < len(line) && line[i+1] == '&' {
				segs = append(segs, cmdSeg{cmd: buf.String(), op: "&&"})
				buf.Reset()
				i += 2
				continue
			}
			segs = append(segs, cmdSeg{cmd: buf.String(), op: "&"})
			buf.Reset()
			i++
			continue
		}
		buf.WriteByte(c)
		i++
	}
	segs = append(segs, cmdSeg{cmd: buf.String(), op: ""})
	return segs
}

// execute runs a single command, applying any I/O redirection it carries.
func (s *Shell) execute(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	cmd, redir := parseRedirection(line)
	if redir == nil {
		s.dispatch(cmd)
		return
	}
	s.runWithRedirection(cmd, redir)
}

// dispatch tokenises and runs a single command with no redirection handling.
func (s *Shell) dispatch(line string) {
	if line == "" {
		return
	}

	// ECHO. and ECHO: (and ECHO(): print the remainder verbatim, or a blank
	// line, without the intervening separator character.
	if len(line) >= 5 && strings.EqualFold(line[:4], "ECHO") {
		if c := line[4]; c == '.' || c == ':' || c == '(' {
			fmt.Println(line[5:])
			s.code = 0
			return
		}
	}

	tokens := tokenize(line)
	if len(tokens) == 0 {
		return
	}

	cmd := strings.ToUpper(tokens[0])
	args := tokens[1:]

	// Check user-defined macros first.
	if macro, ok := s.aliases[cmd]; ok {
		s.executeLineWithEcho(macro, false)
		return
	}

	switch cmd {
	case "CLS":
		s.cmdCls()
	case "DIR":
		s.cmdDir(args)
	case "CD", "CHDIR":
		s.cmdCd(args)
	case "MD", "MKDIR":
		s.cmdMkdir(args)
	case "RD", "RMDIR":
		s.cmdRmdir(args)
	case "DEL", "ERASE":
		s.cmdDel(args)
	case "COPY":
		s.cmdCopy(args)
	case "MOVE":
		s.cmdMove(args)
	case "REN", "RENAME":
		s.cmdRename(args)
	case "TYPE":
		s.cmdType(args)
	case "ECHO":
		s.cmdEcho(args, line)
	case "SET":
		s.cmdSet(args)
	case "PATH":
		s.cmdPath(args)
	case "VER":
		s.cmdVer()
	case "DATE":
		s.cmdDate()
	case "TIME":
		s.cmdTime()
	case "PAUSE":
		s.cmdPause()
	case "MEM":
		s.cmdMem()
	case "TREE":
		s.cmdTree(args)
	case "ATTRIB":
		s.cmdAttrib(args)
	case "FIND":
		s.cmdFind(args)
	case "FINDSTR":
		s.cmdFindstr(args)
	case "MORE":
		s.cmdMore(args)
	case "DL":
		s.cmdDl(args)
	case "DOSKEY":
		s.cmdDoskey(args)
	case "CALL":
		s.cmdCall(args)
	case "IF":
		s.evalIf(line, s.curBatch)
	case "FOR":
		s.evalFor(line, s.curBatch)
	case "GOTO":
		s.cmdGoto(args)
	case "SETLOCAL":
		s.cmdSetlocal(args)
	case "ENDLOCAL":
		s.cmdEndlocal()
	case "SHIFT":
		s.cmdShift(args)
	case "PUSHD":
		s.cmdPushd(args)
	case "POPD":
		s.cmdPopd()
	case "TITLE":
		s.code = 0 // window title has no meaning here; accept silently
	case "HELP", "/?":
		s.cmdHelp(args)
	case "EXIT":
		s.cmdExit(args)
	default:
		// Try to run as an external command or batch file.
		if !s.runExternal(tokens) {
			fmt.Fprintf(os.Stderr, "'%s' is not recognized as an internal or external command,\noperable program or batch file.\n", tokens[0])
			s.code = 1
		}
	}
}

// tokenize splits a line into tokens, respecting double-quoted strings.
func tokenize(line string) []string {
	var tokens []string
	var buf strings.Builder
	inQ := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '"' {
			inQ = !inQ
			continue
		}
		if c == ' ' && !inQ {
			if buf.Len() > 0 {
				tokens = append(tokens, buf.String())
				buf.Reset()
			}
			continue
		}
		buf.WriteByte(c)
	}
	if buf.Len() > 0 {
		tokens = append(tokens, buf.String())
	}
	return tokens
}

// absPath resolves a path relative to the shell's cwd.
func (s *Shell) absPath(p string) string {
	// Convert DOS backslashes.
	p = strings.ReplaceAll(p, `\`, "/")
	// Strip virtual drive prefix.
	if len(p) >= 2 && p[1] == ':' {
		p = p[2:]
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(s.cwd, p))
}
