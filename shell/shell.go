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
	env     map[string]string // environment variables
	aliases map[string]string // DOSKEY-style macros
	cwd     string            // current working directory
	exit    bool              // set to true to exit
	code    int               // exit code
	rng     *rand.Rand        // ChaCha8-backed RNG for %RANDOM%
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
	case len(args) == 1 && !strings.HasPrefix(args[0], "/"):
		// Treat a single arg as a batch file.
		s.runBatchFile(args[0])
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
	if echo && !noEcho && line != "" {
		fmt.Printf("%s%s\n", s.prompt(), line)
	}

	if line == "" || strings.HasPrefix(line, "REM ") || strings.HasPrefix(line, "rem ") || strings.EqualFold(line, "REM") {
		return
	}

	// Expand environment variables (%VAR%).
	line = s.expandVars(line)

	// Split into individual commands separated by &&, & or |.
	// For simplicity we support & (sequential) and && (on-success).
	s.executePipeline(line)
}

// expandVars replaces %VAR% references with their values.
func (s *Shell) expandVars(line string) string {
	var b strings.Builder
	for i := 0; i < len(line); i++ {
		if line[i] == '%' {
			j := strings.Index(line[i+1:], "%")
			if j >= 0 {
				name := strings.ToUpper(line[i+1 : i+1+j])
				var val string
				if name == "RANDOM" {
					val = strconv.Itoa(s.rng.IntN(32768))
				} else {
					val = s.env[name]
				}
				b.WriteString(val)
				i += j + 1
				continue
			}
		}
		b.WriteByte(line[i])
	}
	return b.String()
}

// executePipeline handles & and && command chaining.
func (s *Shell) executePipeline(line string) {
	// We do a simple token scan to respect quoted strings.
	cmds := splitCommands(line)
	prevOK := true
	for _, seg := range cmds {
		if seg.op == "&&" && !prevOK {
			continue
		}
		s.execute(strings.TrimSpace(seg.cmd))
		prevOK = (s.code == 0)
	}
}

type cmdSeg struct {
	cmd string
	op  string // "" | "&" | "&&"
}

// splitCommands splits a line on & and && operators (outside quotes).
func splitCommands(line string) []cmdSeg {
	var segs []cmdSeg
	var buf strings.Builder
	inQ := false
	i := 0
	for i < len(line) {
		c := line[i]
		if c == '"' {
			inQ = !inQ
			buf.WriteByte(c)
			i++
			continue
		}
		if !inQ && c == '&' {
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

// execute dispatches a single command.
func (s *Shell) execute(line string) {
	if line == "" {
		return
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
