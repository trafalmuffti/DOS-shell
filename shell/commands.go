package shell

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// CLS – clear screen
// ---------------------------------------------------------------------------

func (s *Shell) cmdCls() {
	fmt.Print("\033[2J\033[H")
	s.code = 0
}

// ---------------------------------------------------------------------------
// VER – version
// ---------------------------------------------------------------------------

func (s *Shell) cmdVer() {
	fmt.Println()
	fmt.Println(version)
	fmt.Println()
	s.code = 0
}

// ---------------------------------------------------------------------------
// DATE / TIME
// ---------------------------------------------------------------------------

func dateString() string {
	return time.Now().Format("Mon 01/02/2006")
}

func timeString() string {
	return time.Now().Format("15:04:05.00")
}

func (s *Shell) cmdDate() {
	fmt.Printf("The current date is: %s\n", dateString())
	s.code = 0
}

func (s *Shell) cmdTime() {
	fmt.Printf("The current time is: %s\n", timeString())
	s.code = 0
}

// ---------------------------------------------------------------------------
// ECHO
// ---------------------------------------------------------------------------

func (s *Shell) cmdEcho(args []string, rawLine string) {
	if len(args) == 0 {
		fmt.Println("ECHO is on.")
		s.code = 0
		return
	}
	// Preserve original spacing after ECHO.
	idx := indexFold(rawLine, "ECHO")
	if idx >= 0 {
		rest := strings.TrimLeft(rawLine[idx+4:], " \t")
		fmt.Println(rest)
	} else {
		fmt.Println(strings.Join(args, " "))
	}
	s.code = 0
}

// indexFold returns the position of needle in s (case-insensitive), or -1.
func indexFold(s, needle string) int {
	return strings.Index(strings.ToUpper(s), strings.ToUpper(needle))
}

// ---------------------------------------------------------------------------
// SET – display or set environment variables
// ---------------------------------------------------------------------------

func (s *Shell) cmdSet(args []string) {
	if len(args) == 0 {
		// Display all variables sorted.
		keys := make([]string, 0, len(s.env))
		for k := range s.env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("%s=%s\n", k, s.env[k])
		}
		s.code = 0
		return
	}

	expr := strings.Join(args, " ")
	eq := strings.Index(expr, "=")
	if eq < 0 {
		// Display variables matching prefix.
		prefix := strings.ToUpper(expr)
		found := false
		for k, v := range s.env {
			if strings.HasPrefix(k, prefix) {
				fmt.Printf("%s=%s\n", k, v)
				found = true
			}
		}
		if !found {
			fmt.Fprintf(os.Stderr, "Environment variable %s not defined\n", expr)
			s.code = 1
		}
		return
	}

	key := strings.ToUpper(strings.TrimSpace(expr[:eq]))
	val := expr[eq+1:]
	if val == "" {
		delete(s.env, key)
	} else {
		s.env[key] = val
	}
	// Keep OS env in sync for child processes.
	os.Setenv(key, val)
	s.code = 0
}

// ---------------------------------------------------------------------------
// PATH
// ---------------------------------------------------------------------------

func (s *Shell) cmdPath(args []string) {
	if len(args) == 0 {
		fmt.Printf("PATH=%s\n", s.env["PATH"])
		s.code = 0
		return
	}
	p := strings.Join(args, " ")
	s.env["PATH"] = p
	os.Setenv("PATH", p)
	s.code = 0
}

// ---------------------------------------------------------------------------
// CD / CHDIR
// ---------------------------------------------------------------------------

func (s *Shell) cmdCd(args []string) {
	if len(args) == 0 {
		fmt.Println(s.dosPath(s.cwd))
		s.code = 0
		return
	}

	target := s.absPath(args[0])
	info, err := os.Stat(target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "The system cannot find the path specified.\n")
		s.code = 1
		return
	}
	if !info.IsDir() {
		fmt.Fprintf(os.Stderr, "The directory name is invalid.\n")
		s.code = 1
		return
	}
	s.cwd = target
	os.Chdir(target)
	s.code = 0
}

// ---------------------------------------------------------------------------
// MD / MKDIR
// ---------------------------------------------------------------------------

func (s *Shell) cmdMkdir(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "The syntax of the command is incorrect.")
		s.code = 1
		return
	}
	path := s.absPath(args[0])
	if err := os.MkdirAll(path, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "A subdirectory or file %s already exists.\n", args[0])
		s.code = 1
		return
	}
	s.code = 0
}

// ---------------------------------------------------------------------------
// RD / RMDIR
// ---------------------------------------------------------------------------

func (s *Shell) cmdRmdir(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "The syntax of the command is incorrect.")
		s.code = 1
		return
	}
	// /S flag removes tree.
	recursive := false
	var dirs []string
	for _, a := range args {
		if strings.EqualFold(a, "/S") {
			recursive = true
		} else {
			dirs = append(dirs, a)
		}
	}

	for _, d := range dirs {
		path := s.absPath(d)
		var err error
		if recursive {
			err = os.RemoveAll(path)
		} else {
			err = os.Remove(path)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "The directory is not empty.\n")
			s.code = 1
			return
		}
	}
	s.code = 0
}

// ---------------------------------------------------------------------------
// DIR
// ---------------------------------------------------------------------------

func (s *Shell) cmdDir(args []string) {
	// Parse flags.
	wide := false
	bareNames := false
	var patterns []string

	for _, a := range args {
		switch strings.ToUpper(a) {
		case "/W":
			wide = true
		case "/B":
			bareNames = true
		default:
			patterns = append(patterns, a)
		}
	}

	if len(patterns) == 0 {
		patterns = []string{s.cwd}
	}

	for _, pat := range patterns {
		target := s.absPath(pat)
		info, err := os.Stat(target)
		if err != nil {
			// Try as glob.
			matches, _ := filepath.Glob(target)
			if len(matches) == 0 {
				fmt.Fprintf(os.Stderr, "File Not Found\n")
				s.code = 1
				return
			}
			s.printDirListing(filepath.Dir(target), matches, wide, bareNames)
			continue
		}

		if info.IsDir() {
			entries, err := os.ReadDir(target)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Access denied.\n")
				s.code = 1
				return
			}
			var paths []string
			for _, e := range entries {
				paths = append(paths, filepath.Join(target, e.Name()))
			}
			s.printDirListing(target, paths, wide, bareNames)
		} else {
			s.printDirListing(filepath.Dir(target), []string{target}, wide, bareNames)
		}
	}
	s.code = 0
}

func (s *Shell) printDirListing(dir string, paths []string, wide, bareNames bool) {
	if !bareNames {
		fmt.Printf("\n Directory of %s\n\n", s.dosPath(dir))
	}

	var totalFiles, totalDirs int
	var totalSize int64

	type entry struct {
		info os.FileInfo
		path string
	}
	var entries []entry
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		entries = append(entries, entry{fi, p})
	}

	if wide {
		// Print in columns.
		const cols = 5
		col := 0
		for _, e := range entries {
			name := e.info.Name()
			if e.info.IsDir() {
				name = "[" + name + "]"
			}
			fmt.Printf("%-16s", name)
			col++
			if col == cols {
				fmt.Println()
				col = 0
			}
		}
		if col != 0 {
			fmt.Println()
		}
	} else {
		for _, e := range entries {
			if bareNames {
				fmt.Println(e.info.Name())
				continue
			}
			mod := e.info.ModTime().Format("01/02/2006  03:04 PM")
			if e.info.IsDir() {
				fmt.Printf("%s    <DIR>          %s\n", mod, e.info.Name())
				totalDirs++
			} else {
				sz := e.info.Size()
				totalSize += sz
				totalFiles++
				fmt.Printf("%s    %14s %s\n", mod, formatSize(sz), e.info.Name())
			}
		}
	}

	if !bareNames {
		fmt.Printf("%16d File(s)  %s bytes\n", totalFiles, formatSize(totalSize))
		fmt.Printf("%16d Dir(s)\n", totalDirs)
		fmt.Println()
	}
}

func formatSize(n int64) string {
	s := strconv.FormatInt(n, 10)
	// Insert thousands separators.
	var out strings.Builder
	for i, c := range s {
		rem := len(s) - i
		if i > 0 && rem%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(c)
	}
	return out.String()
}

// ---------------------------------------------------------------------------
// DEL / ERASE
// ---------------------------------------------------------------------------

func (s *Shell) cmdDel(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "The syntax of the command is incorrect.")
		s.code = 1
		return
	}

	quiet := false
	var patterns []string
	for _, a := range args {
		if strings.EqualFold(a, "/Q") {
			quiet = true
		} else {
			patterns = append(patterns, a)
		}
	}
	_ = quiet

	for _, pat := range patterns {
		abs := s.absPath(pat)
		matches, err := filepath.Glob(abs)
		if err != nil || len(matches) == 0 {
			// Try direct path.
			if e2 := os.Remove(abs); e2 != nil {
				fmt.Fprintf(os.Stderr, "Could Not Find %s\n", pat)
				s.code = 1
				return
			}
			continue
		}
		for _, m := range matches {
			if err := os.Remove(m); err != nil {
				fmt.Fprintf(os.Stderr, "Access is denied - %s\n", m)
				s.code = 1
				return
			}
		}
	}
	s.code = 0
}

// ---------------------------------------------------------------------------
// COPY
// ---------------------------------------------------------------------------

func (s *Shell) cmdCopy(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "The syntax of the command is incorrect.")
		s.code = 1
		return
	}

	src := s.absPath(args[0])
	dst := s.absPath(args[1])

	// If dst is a directory, copy file into it.
	dstInfo, err := os.Stat(dst)
	if err == nil && dstInfo.IsDir() {
		dst = filepath.Join(dst, filepath.Base(src))
	}

	count, err := copyGlob(src, dst)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		s.code = 1
		return
	}
	fmt.Printf("        %d file(s) copied.\n", count)
	s.code = 0
}

func copyGlob(src, dst string) (int, error) {
	matches, _ := filepath.Glob(src)
	if len(matches) == 0 {
		matches = []string{src}
	}
	count := 0
	for _, m := range matches {
		var target string
		dstInfo, err := os.Stat(dst)
		if err == nil && dstInfo.IsDir() {
			target = filepath.Join(dst, filepath.Base(m))
		} else if len(matches) > 1 {
			target = filepath.Join(dst, filepath.Base(m))
		} else {
			target = dst
		}
		if err := copyFile(m, target); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// ---------------------------------------------------------------------------
// MOVE
// ---------------------------------------------------------------------------

func (s *Shell) cmdMove(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "The syntax of the command is incorrect.")
		s.code = 1
		return
	}

	src := s.absPath(args[0])
	dst := s.absPath(args[1])

	// If dst is a directory, move src into it.
	dstInfo, err := os.Stat(dst)
	if err == nil && dstInfo.IsDir() {
		dst = filepath.Join(dst, filepath.Base(src))
	}

	matches, _ := filepath.Glob(src)
	if len(matches) == 0 {
		matches = []string{src}
	}
	for _, m := range matches {
		target := dst
		if len(matches) > 1 {
			target = filepath.Join(dst, filepath.Base(m))
		}
		if err := os.Rename(m, target); err != nil {
			fmt.Fprintf(os.Stderr, "The system cannot find the file specified.\n")
			s.code = 1
			return
		}
		fmt.Printf("%s => %s\n", m, target)
	}
	s.code = 0
}

// ---------------------------------------------------------------------------
// REN / RENAME
// ---------------------------------------------------------------------------

func (s *Shell) cmdRename(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "The syntax of the command is incorrect.")
		s.code = 1
		return
	}
	src := s.absPath(args[0])
	dst := filepath.Join(filepath.Dir(src), args[1])
	if err := os.Rename(src, dst); err != nil {
		fmt.Fprintf(os.Stderr, "The system cannot find the file specified.\n")
		s.code = 1
		return
	}
	s.code = 0
}

// ---------------------------------------------------------------------------
// TYPE
// ---------------------------------------------------------------------------

func (s *Shell) cmdType(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "The syntax of the command is incorrect.")
		s.code = 1
		return
	}
	for _, a := range args {
		path := s.absPath(a)
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "The system cannot find the file specified.\n")
			s.code = 1
			return
		}
		os.Stdout.Write(data)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			fmt.Println()
		}
	}
	s.code = 0
}

// ---------------------------------------------------------------------------
// MORE – paginated output
// ---------------------------------------------------------------------------

func (s *Shell) cmdMore(args []string) {
	var r io.Reader

	if len(args) == 0 {
		r = os.Stdin
	} else {
		path := s.absPath(args[0])
		f, err := os.Open(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "File not found - %s\n", args[0])
			s.code = 1
			return
		}
		defer f.Close()
		r = f
	}

	scanner := bufio.NewScanner(r)
	term, _ := os.Open("/dev/tty")
	if term == nil {
		term = os.Stdin
	}
	defer term.Close()
	termReader := bufio.NewReader(term)

	lineCount := 0
	pageSize := 23
	for scanner.Scan() {
		fmt.Println(scanner.Text())
		lineCount++
		if lineCount%pageSize == 0 {
			fmt.Print("-- More -- ")
			termReader.ReadString('\n')
		}
	}
	s.code = 0
}

// ---------------------------------------------------------------------------
// ATTRIB
// ---------------------------------------------------------------------------

func (s *Shell) cmdAttrib(args []string) {
	if len(args) == 0 {
		// List attributes of cwd contents.
		entries, _ := os.ReadDir(s.cwd)
		for _, e := range entries {
			info, _ := e.Info()
			printAttrib(info, filepath.Join(s.cwd, e.Name()))
		}
		s.code = 0
		return
	}

	// Filter out attribute flags (+R, -R, etc.).
	var targets []string
	for _, a := range args {
		if strings.HasPrefix(a, "+") || strings.HasPrefix(a, "-") {
			// We don't actually change Linux attrs here (no-op for write-protection demo).
			continue
		}
		targets = append(targets, a)
	}

	for _, t := range targets {
		path := s.absPath(t)
		info, err := os.Stat(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "File not found - %s\n", t)
			s.code = 1
			return
		}
		printAttrib(info, path)
	}
	s.code = 0
}

func printAttrib(info os.FileInfo, path string) {
	if info == nil {
		return
	}
	// DOS attributes: A=archive, R=read-only, H=hidden, S=system
	r := ' '
	if info.Mode().Perm()&0200 == 0 {
		r = 'R'
	}
	h := ' '
	if strings.HasPrefix(info.Name(), ".") {
		h = 'H'
	}
	fmt.Printf("  %c  %c              %s\n", r, h, path)
}

// ---------------------------------------------------------------------------
// FIND – search for text in files (simplified FIND /I)
// ---------------------------------------------------------------------------

func (s *Shell) cmdFind(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, `FIND: Parameter format not correct.
Usage: FIND [/I] [/N] [/C] "string" file...`)
		s.code = 1
		return
	}

	caseInsensitive := false
	showLineNums := false
	countOnly := false
	searchStr := ""
	var files []string

	for _, a := range args {
		switch strings.ToUpper(a) {
		case "/I":
			caseInsensitive = true
		case "/N":
			showLineNums = true
		case "/C":
			countOnly = true
		default:
			if strings.HasPrefix(a, `"`) || strings.HasPrefix(a, "'") {
				searchStr = strings.Trim(a, `"'`)
			} else {
				files = append(files, a)
			}
		}
	}

	if searchStr == "" {
		fmt.Fprintln(os.Stderr, "FIND: Parameter format not correct.")
		s.code = 1
		return
	}

	for _, f := range files {
		path := s.absPath(f)
		file, err := os.Open(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "File not found - %s\n", f)
			s.code = 1
			continue
		}

		fmt.Printf("---------- %s\n", strings.ToUpper(f))
		scanner := bufio.NewScanner(file)
		lineNum := 0
		matchCount := 0
		for scanner.Scan() {
			lineNum++
			line := scanner.Text()
			needle := searchStr
			haystack := line
			if caseInsensitive {
				needle = strings.ToLower(needle)
				haystack = strings.ToLower(line)
			}
			if strings.Contains(haystack, needle) {
				matchCount++
				if !countOnly {
					if showLineNums {
						fmt.Printf("[%d]%s\n", lineNum, line)
					} else {
						fmt.Println(line)
					}
				}
			}
		}
		file.Close()
		if countOnly {
			fmt.Printf("---------- %s: %d\n", strings.ToUpper(f), matchCount)
		}
	}
	s.code = 0
}

// ---------------------------------------------------------------------------
// FINDSTR – grep-like search supporting regex and multiple patterns
// ---------------------------------------------------------------------------

func (s *Shell) cmdFindstr(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, `FINDSTR: Missing required parameter.
Usage: FINDSTR [/B] [/E] [/L] [/R] [/S] [/I] [/N] [/M] [/C:"string"] [/G:file]
       [strings] filename...
  /B       Match at beginning of line.
  /E       Match at end of line.
  /L       Uses search strings literally (default).
  /R       Uses search strings as regular expressions.
  /S       Searches for matching files in current dir and all subdirs.
  /I       Case-insensitive search.
  /N       Print the line number before each matching line.
  /M       Print only the filename if a file contains a match.
  /C:str   Use the specified string as a literal search string.
  /G:file  Gets search strings from the specified file.`)
		s.code = 1
		return
	}

	matchBegin := false
	matchEnd := false
	useRegex := false
	recursive := false
	caseInsensitive := false
	showLineNums := false
	filenameOnly := false
	var patterns []string
	var files []string

	for i := 0; i < len(args); i++ {
		a := args[i]
		upper := strings.ToUpper(a)
		switch {
		case upper == "/B":
			matchBegin = true
		case upper == "/E":
			matchEnd = true
		case upper == "/L":
			useRegex = false
		case upper == "/R":
			useRegex = true
		case upper == "/S":
			recursive = true
		case upper == "/I":
			caseInsensitive = true
		case upper == "/N":
			showLineNums = true
		case upper == "/M":
			filenameOnly = true
		case strings.HasPrefix(upper, "/C:"):
			patterns = append(patterns, a[3:])
		case strings.HasPrefix(upper, "/G:"):
			gfile := s.absPath(a[3:])
			data, err := os.ReadFile(gfile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "FINDSTR: Cannot open %s\n", a[3:])
				s.code = 1
				return
			}
			for _, line := range strings.Split(string(data), "\n") {
				if t := strings.TrimRight(line, "\r"); t != "" {
					patterns = append(patterns, t)
				}
			}
		default:
			if len(patterns) == 0 && !strings.HasPrefix(a, "/") {
				// First non-flag arg is the search pattern(s) — space-separated.
				patterns = append(patterns, strings.Fields(a)...)
			} else {
				files = append(files, a)
			}
		}
	}

	if len(patterns) == 0 {
		fmt.Fprintln(os.Stderr, "FINDSTR: Missing required parameter.")
		s.code = 1
		return
	}

	// Build regex or literal matchers.
	type matcher struct {
		re      *regexp.Regexp
		literal string
	}
	var matchers []matcher
	for _, p := range patterns {
		if useRegex {
			flags := ""
			if caseInsensitive {
				flags = "(?i)"
			}
			re, err := regexp.Compile(flags + p)
			if err != nil {
				fmt.Fprintf(os.Stderr, "FINDSTR: Invalid regular expression: %s\n", p)
				s.code = 2
				return
			}
			matchers = append(matchers, matcher{re: re})
		} else {
			matchers = append(matchers, matcher{literal: p})
		}
	}

	lineMatches := func(line string) bool {
		for _, m := range matchers {
			var matched bool
			if m.re != nil {
				check := line
				if matchBegin {
					check = "^" + regexp.QuoteMeta(line)
				}
				_ = check
				matched = m.re.MatchString(line)
				if matchBegin {
					loc := m.re.FindStringIndex(line)
					matched = loc != nil && loc[0] == 0
				}
				if matchEnd && matched {
					loc := m.re.FindStringIndex(line)
					matched = loc != nil && loc[1] == len(line)
				}
			} else {
				needle := m.literal
				haystack := line
				if caseInsensitive {
					needle = strings.ToLower(needle)
					haystack = strings.ToLower(line)
				}
				if matchBegin {
					matched = strings.HasPrefix(haystack, needle)
				} else if matchEnd {
					matched = strings.HasSuffix(haystack, needle)
				} else {
					matched = strings.Contains(haystack, needle)
				}
			}
			if matched {
				return true
			}
		}
		return false
	}

	searchFile := func(path, displayName string) bool {
		f, err := os.Open(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FINDSTR: Cannot open %s\n", displayName)
			return false
		}
		defer f.Close()

		found := false
		scanner := bufio.NewScanner(f)
		lineNum := 0
		for scanner.Scan() {
			lineNum++
			line := scanner.Text()
			if lineMatches(line) {
				found = true
				if filenameOnly {
					fmt.Println(displayName)
					return true
				}
				prefix := ""
				if showLineNums {
					prefix = fmt.Sprintf("%d:", lineNum)
				}
				fmt.Printf("%s%s\n", prefix, line)
			}
		}
		return found
	}

	// Collect target files (supporting globs and recursive).
	var targets []struct{ abs, display string }
	for _, pat := range files {
		abs := s.absPath(pat)
		if recursive {
			// Walk from the directory part of the pattern.
			dir := filepath.Dir(abs)
			glob := filepath.Base(abs)
			filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error { //nolint
				if err != nil || d.IsDir() {
					return nil
				}
				matched, _ := filepath.Match(glob, d.Name())
				if matched || glob == d.Name() || glob == "*" || glob == "*.*" {
					targets = append(targets, struct{ abs, display string }{p, p})
				}
				return nil
			})
		} else {
			matches, _ := filepath.Glob(abs)
			if len(matches) == 0 {
				matches = []string{abs}
			}
			for _, m := range matches {
				targets = append(targets, struct{ abs, display string }{m, m})
			}
		}
	}

	anyMatch := false
	for _, t := range targets {
		if searchFile(t.abs, t.display) {
			anyMatch = true
		}
	}

	if !anyMatch {
		s.code = 1
	} else {
		s.code = 0
	}
}

// ---------------------------------------------------------------------------
// TREE
// ---------------------------------------------------------------------------

func (s *Shell) cmdTree(args []string) {
	root := s.cwd
	if len(args) > 0 {
		root = s.absPath(args[0])
	}

	showFiles := false
	start := 1
	if len(args) == 0 {
		start = 0
	}
	for _, a := range args[start:] {
		if strings.EqualFold(a, "/F") {
			showFiles = true
		}
	}

	fmt.Printf("Folder PATH listing for volume C\n")
	fmt.Printf("Volume serial number is 0000-0000\n")
	fmt.Println(s.dosPath(root))
	printTree(root, "", showFiles)
	s.code = 0
}

func printTree(path, prefix string, showFiles bool) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return
	}

	var dirs []os.DirEntry
	var files []os.DirEntry
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e)
		} else {
			files = append(files, e)
		}
	}

	if showFiles {
		for i, f := range files {
			connector := "├───"
			if i == len(files)-1 && len(dirs) == 0 {
				connector = "└───"
			}
			fmt.Printf("%s%s%s\n", prefix, connector, f.Name())
		}
	}

	for i, d := range dirs {
		connector := "├───"
		childPrefix := prefix + "│   "
		if i == len(dirs)-1 {
			connector = "└───"
			childPrefix = prefix + "    "
		}
		fmt.Printf("%s%s%s\n", prefix, connector, d.Name())
		printTree(filepath.Join(path, d.Name()), childPrefix, showFiles)
	}
}

// ---------------------------------------------------------------------------
// PAUSE
// ---------------------------------------------------------------------------

func (s *Shell) cmdPause() {
	fmt.Print("Press Enter to continue...")
	bufio.NewReader(os.Stdin).ReadString('\n')
	s.code = 0
}

// ---------------------------------------------------------------------------
// DOSKEY – define macros
// ---------------------------------------------------------------------------

func (s *Shell) cmdDoskey(args []string) {
	if len(args) == 0 {
		// List all macros.
		for k, v := range s.aliases {
			fmt.Printf("%s=%s\n", k, v)
		}
		s.code = 0
		return
	}

	expr := strings.Join(args, " ")
	eq := strings.Index(expr, "=")
	if eq < 0 {
		s.code = 1
		return
	}
	key := strings.ToUpper(strings.TrimSpace(expr[:eq]))
	val := strings.TrimSpace(expr[eq+1:])
	if val == "" {
		delete(s.aliases, key)
	} else {
		s.aliases[key] = val
	}
	s.code = 0
}

// ---------------------------------------------------------------------------
// HELP
// ---------------------------------------------------------------------------

func (s *Shell) cmdHelp(args []string) {
	if len(args) > 0 {
		s.helpFor(strings.ToUpper(args[0]))
		return
	}
	fmt.Print(`
For more information on a specific command, type HELP command-name.

ATTRIB    Displays or changes file attributes.
CALL      Calls one batch program from another.
CD        Displays the name of or changes the current directory.
CHDIR     Displays the name of or changes the current directory.
CLS       Clears the screen.
COPY      Copies one or more files to another location.
DATE      Displays or sets the date.
DEL       Deletes one or more files.
DIR       Displays a list of files and subdirectories in a directory.
DOSKEY    Edits command lines and creates macros.
ECHO      Displays messages, or turns command-echoing on or off.
ERASE     Deletes one or more files.
EXIT      Quits the shell.
FIND      Searches for a text string in a file or files.
FINDSTR   Searches for strings in files (supports regex, recursive).
HELP      Provides Help information for commands.
MD        Creates a directory.
MKDIR     Creates a directory.
MORE      Displays output one screen at a time.
MOVE      Moves files and renames files and directories.
PATH      Displays or sets a search path for executable files.
PAUSE     Suspends processing of a batch program and displays a message.
RD        Removes a directory.
REM       Records comments (remarks) in batch files.
REN       Renames a file or files.
RENAME    Renames a file or files.
RMDIR     Removes a directory.
SET       Displays, sets, or removes environment variables.
TIME      Displays or sets the system time.
TREE      Graphically displays the directory structure of a drive or path.
TYPE      Displays the contents of a text file.
VER       Displays the version.

`)
	s.code = 0
}

func (s *Shell) helpFor(cmd string) {
	msgs := map[string]string{
		"DIR":    "DIR [drive:][path][filename] [/W] [/B]\n  /W  Wide list format.\n  /B  Bare format (no heading or summary).",
		"CD":     "CD [path]\n  Changes the current directory.",
		"COPY":   "COPY source destination\n  Copies files from source to destination.",
		"DEL":    "DEL [/Q] filename\n  Deletes files. /Q quiet mode.",
		"MOVE":   "MOVE source destination\n  Moves files from one location to another.",
		"REN":    "REN oldname newname\n  Renames a file.",
		"TYPE":   "TYPE filename\n  Displays the contents of a text file.",
		"FIND":    `FIND [/I] [/N] [/C] "string" filename...`,
		"FINDSTR": "FINDSTR [/B] [/E] [/L] [/R] [/S] [/I] [/N] [/M] [/C:str] [/G:file] strings filename...\n" +
			"  /B  Match at beginning of line.   /E  Match at end of line.\n" +
			"  /R  Use regular expressions.      /S  Search subdirectories.\n" +
			"  /I  Case insensitive.             /N  Print line numbers.\n" +
			"  /M  Print filenames only.         /C  Literal search string.\n" +
			"  /G  Read patterns from file.",
		"TREE":   "TREE [path] [/F]\n  /F  Display the names of the files in each folder.",
		"SET":    "SET [variable[=value]]\n  Displays or sets environment variables.",
		"ECHO":   "ECHO [message]\n  Displays a message or turns echo on/off.",
		"MKDIR":  "MD directory\n  Creates a directory.",
		"RMDIR":  "RD [/S] directory\n  Removes a directory. /S removes all subdirectories.",
		"EXIT":   "EXIT [/B] [exitcode]\n  Exits the shell.",
		"DOSKEY": "DOSKEY [name=macro]\n  Creates macros and recalls command history.",
	}
	if m, ok := msgs[cmd]; ok {
		fmt.Println(m)
	} else {
		fmt.Printf("No help available for %s.\n", cmd)
	}
	s.code = 0
}

// ---------------------------------------------------------------------------
// EXIT
// ---------------------------------------------------------------------------

func (s *Shell) cmdExit(args []string) {
	code := 0
	for _, a := range args {
		if n, err := strconv.Atoi(a); err == nil {
			code = n
		}
	}
	s.code = code
	s.exit = true
}

// ---------------------------------------------------------------------------
// CALL – call a batch file from within a batch file
// ---------------------------------------------------------------------------

func (s *Shell) cmdCall(args []string) {
	if len(args) == 0 {
		s.code = 0
		return
	}
	s.runBatchFile(args[0])
}

// ---------------------------------------------------------------------------
// External command / program execution
// ---------------------------------------------------------------------------

func (s *Shell) runExternal(tokens []string) bool {
	name := tokens[0]

	// Check if it's a batch file.
	if strings.HasSuffix(strings.ToLower(name), ".bat") {
		path := s.absPath(name)
		if _, err := os.Stat(path); err == nil {
			s.runBatchFile(path)
			return true
		}
	}

	// Try to find the binary in PATH.
	path, err := exec.LookPath(name)
	if err != nil {
		// Try with .bat extension.
		if _, err2 := exec.LookPath(name + ".bat"); err2 != nil {
			return false
		}
		path = name + ".bat"
	}

	cmd := exec.Command(path, tokens[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = s.cwd

	// Pass environment.
	for k, v := range s.env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				s.code = status.ExitStatus()
				return true
			}
		}
		s.code = 1
	} else {
		s.code = 0
	}
	return true
}

// ---------------------------------------------------------------------------
// Batch file execution
// ---------------------------------------------------------------------------

func (s *Shell) runBatchFile(path string) {
	abs := s.absPath(path)
	data, err := os.ReadFile(abs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "The system cannot find the file specified.\n")
		s.code = 1
		return
	}

	lines := strings.Split(string(data), "\n")
	bp := &batchProcessor{
		shell: s,
		lines: lines,
		pc:    0,
	}
	bp.run()
}

// batchProcessor tracks state for a running batch file.
type batchProcessor struct {
	shell *Shell
	lines []string
	pc    int
	echo  bool
}

func (bp *batchProcessor) run() {
	bp.echo = true
	for bp.pc < len(bp.lines) && !bp.shell.exit {
		line := strings.TrimRight(bp.lines[bp.pc], "\r")
		bp.pc++

		trimmed := strings.TrimSpace(line)

		// Handle GOTO.
		if strings.HasPrefix(strings.ToUpper(trimmed), "GOTO ") {
			label := strings.TrimSpace(trimmed[5:])
			bp.gotoLabel(label)
			continue
		}

		// Handle IF.
		if strings.HasPrefix(strings.ToUpper(trimmed), "IF ") {
			bp.handleIf(trimmed)
			continue
		}

		// Handle FOR.
		if strings.HasPrefix(strings.ToUpper(trimmed), "FOR ") {
			bp.handleFor(trimmed)
			continue
		}

		// Handle @ECHO ON/OFF.
		upper := strings.ToUpper(trimmed)
		if upper == "ECHO ON" || upper == "@ECHO ON" {
			bp.echo = true
			continue
		}
		if upper == "ECHO OFF" || upper == "@ECHO OFF" {
			bp.echo = false
			continue
		}

		bp.shell.executeLineWithEcho(line, bp.echo)
	}
}

func (bp *batchProcessor) gotoLabel(label string) {
	target := ":" + strings.ToLower(label)
	for i, l := range bp.lines {
		if strings.ToLower(strings.TrimSpace(l)) == target {
			bp.pc = i + 1
			return
		}
	}
	fmt.Fprintf(os.Stderr, "Label not found - %s\n", label)
	bp.shell.code = 1
}

func (bp *batchProcessor) handleIf(line string) {
	// Supported syntax:
	//   IF [NOT] ERRORLEVEL n command
	//   IF [NOT] EXIST file command
	//   IF [NOT] "a"=="b" command
	rest := line[3:] // strip IF
	rest = strings.TrimSpace(rest)

	not := false
	if strings.HasPrefix(strings.ToUpper(rest), "NOT ") {
		not = true
		rest = strings.TrimSpace(rest[4:])
	}

	upper := strings.ToUpper(rest)
	var condition bool
	var cmd string

	switch {
	case strings.HasPrefix(upper, "ERRORLEVEL "):
		parts := strings.SplitN(rest[11:], " ", 2)
		if len(parts) < 2 {
			return
		}
		n, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
		condition = bp.shell.code >= n
		cmd = parts[1]

	case strings.HasPrefix(upper, "EXIST "):
		parts := strings.SplitN(rest[6:], " ", 2)
		if len(parts) < 2 {
			return
		}
		path := bp.shell.absPath(strings.TrimSpace(parts[0]))
		_, err := os.Stat(path)
		condition = (err == nil)
		cmd = parts[1]

	default:
		// String comparison: "a"=="b" or a==b
		idx := strings.Index(rest, "==")
		if idx < 0 {
			return
		}
		lhs := strings.Trim(strings.TrimSpace(rest[:idx]), `"`)
		afterEq := strings.TrimSpace(rest[idx+2:])
		// The command follows the second operand.
		// operand ends at first space that is not inside quotes.
		rhs, remainder := splitFirstToken(afterEq)
		rhs = strings.Trim(rhs, `"`)
		condition = (lhs == rhs)
		cmd = remainder
	}

	if not {
		condition = !condition
	}
	if condition {
		bp.shell.executeLineWithEcho(strings.TrimSpace(cmd), false)
	}
}

// splitFirstToken splits a string into the first whitespace-delimited token
// and the remainder.
func splitFirstToken(s string) (string, string) {
	s = strings.TrimSpace(s)
	i := strings.IndexByte(s, ' ')
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimSpace(s[i+1:])
}

func (bp *batchProcessor) handleFor(line string) {
	// FOR %%var IN (list) DO command
	// Simplified: only supports literal lists.
	upper := strings.ToUpper(line)
	inIdx := strings.Index(upper, " IN (")
	doIdx := strings.Index(upper, ") DO ")
	if inIdx < 0 || doIdx < 0 {
		fmt.Fprintln(os.Stderr, "The syntax of the command is incorrect.")
		return
	}

	// Extract variable name: FOR %%v ...
	varPart := strings.TrimSpace(line[4:inIdx])
	if len(varPart) < 2 || varPart[0] != '%' {
		fmt.Fprintln(os.Stderr, "The syntax of the command is incorrect.")
		return
	}
	varName := string(varPart[1])

	listStr := line[inIdx+5 : doIdx]
	cmd := strings.TrimSpace(line[doIdx+5:])

	items := strings.Fields(listStr)
	for _, item := range items {
		expanded := strings.ReplaceAll(cmd, "%"+varName, item)
		expanded = strings.ReplaceAll(expanded, "%%"+varName, item)
		bp.shell.executeLineWithEcho(expanded, false)
	}
}

// ---------------------------------------------------------------------------
// Utility
// ---------------------------------------------------------------------------

// runeCount returns the number of UTF-8 runes in s (used for padding).
func runeCount(s string) int {
	return utf8.RuneCountInString(s)
}

// padRight pads s with spaces to at least n runes.
func padRight(s string, n int) string {
	rc := runeCount(s)
	if rc >= n {
		return s
	}
	return s + strings.Repeat(" ", n-rc)
}

// silenceUnused is a compile-time reference to suppress "declared and not used"
// errors for helpers that may only be needed in certain build paths.
var _ = padRight
