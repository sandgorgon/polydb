package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	auth9 "github.com/sandgorgon/9auth"
	"github.com/sandgorgon/polydb"
	"github.com/sandgorgon/polydb/client"
	"github.com/sandgorgon/polydb/internal/lang"
	"github.com/sandgorgon/polydb/value"
	"golang.org/x/term"
)

// connectFlags are the flags shell and run share.
type connectFlags struct {
	fingerprint string
	systemCAs   bool
	insecure    bool
	trace       bool
	memory      bool
	noRC        bool
	format      string
}

func (c *connectFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.fingerprint, "fingerprint", "", "trust only a server with this 9auth fingerprint")
	fs.BoolVar(&c.systemCAs, "system-cas", false, "verify the server against the system's certificate authorities")
	fs.BoolVar(&c.insecure, "insecure", false, "connect over TCP without TLS (loopback servers only)")
	fs.BoolVar(&c.trace, "trace", false, "print every protocol message (secrets redacted)")
	fs.BoolVar(&c.memory, "memory", false, "use a temporary in-memory engine instead of a server")
	fs.BoolVar(&c.noRC, "no-rc", false, "don't run ~/.polydbrc")
	fs.StringVar(&c.format, "format", "table", "result format: table or tsv")
}

// tty is the terminal for prompts while the shell owns standard input.
func tty() (*os.File, error) { return os.OpenFile("/dev/tty", os.O_RDWR, 0) }

// ask prompts on the terminal and reads a line.
func ask(prompt string) (string, error) {
	t, err := tty()
	if err != nil {
		return "", errors.New("no terminal to ask on")
	}
	defer t.Close()
	fmt.Fprint(t, prompt)
	line, err := bufio.NewReader(t).ReadString('\n')
	return strings.TrimSpace(line), err
}

// askPassword prompts for a password without echoing it.
func askPassword(prompt string) (string, error) {
	t, err := tty()
	if err != nil {
		return "", errors.New("no terminal to ask for a password on; set POLYDB_PASSWORD")
	}
	defer t.Close()
	fmt.Fprint(t, prompt)
	pw, err := term.ReadPassword(int(t.Fd()))
	fmt.Fprintln(t)
	return string(pw), err
}

// connect opens the database named by url, or an in-memory engine.
func connect(ctx context.Context, url string, f *connectFlags, errOut io.Writer) (polydb.DB, string, error) {
	if f.memory {
		db, err := polydb.OpenMemory(&polydb.Options{Database: "mem"})
		return db, "mem", err
	}
	target, err := client.ParseURL(url)
	if err != nil {
		return nil, "", err
	}
	var opts []client.Option
	if id, err := auth9.Load(); err == nil {
		opts = append(opts, client.WithIdentity(id))
	}
	switch {
	case f.fingerprint != "":
		opts = append(opts, client.WithServerFingerprint(f.fingerprint))
	case f.systemCAs:
		opts = append(opts, client.WithSystemCAs())
	default:
		if path, err := auth9.KnownPeersPath(); err == nil {
			opts = append(opts, client.WithKnownPeers(path), client.WithConfirmPeer(func(addr, fp string) bool {
				a, err := ask(fmt.Sprintf("The server at %s has fingerprint\n  %s\nTrust it and remember it? [y/N] ", addr, fp))
				return err == nil && (a == "y" || a == "yes")
			}))
		}
	}
	if f.insecure {
		opts = append(opts, client.WithInsecure())
	}
	if pw := os.Getenv("POLYDB_PASSWORD"); pw != "" {
		opts = append(opts, client.WithPassword(pw))
	} else if target.Password == "" {
		who := target.User
		opts = append(opts, client.WithPasswordFunc(func() (string, error) {
			return askPassword(fmt.Sprintf("password for %s: ", who))
		}))
	}
	if f.trace {
		opts = append(opts, client.WithTrace(func(dir string, stream uint32, msg string) {
			fmt.Fprintf(errOut, "%s %3d %s\n", dir, stream, msg)
		}))
	}
	db, err := client.Dial(ctx, url, opts...)
	if err != nil {
		return nil, "", err
	}
	return db, db.Database(), nil
}

// shell runs statements against a database and prints results.
type shell struct {
	db      polydb.DB
	tx      polydb.Tx
	failed  bool
	dbName  string
	params  map[string]value.Value
	timing  bool
	format  string
	out     io.Writer
	errOut  io.Writer
	running func(cancel context.CancelFunc) func() // set by the REPL to catch Ctrl-C
}

func newShell(db polydb.DB, dbName string, out, errOut io.Writer, format string) *shell {
	return &shell{db: db, dbName: dbName, params: map[string]value.Value{}, out: out, errOut: errOut, format: format}
}

// prompt shows the database and the transaction state: * inside a
// transaction, ! when it has failed and must be rolled back.
func (s *shell) prompt() string {
	p := "polydb"
	if s.dbName != "" {
		p += ":" + s.dbName
	}
	switch {
	case s.tx != nil && s.failed:
		p += "!"
	case s.tx != nil:
		p += "*"
	}
	return p + "> "
}

// errorf prints an error.
func (s *shell) errorf(format string, args ...any) {
	fmt.Fprintf(s.errOut, format+"\n", args...)
}

// runScript runs every statement of src, stopping at the first error if
// stop is set. It reports whether everything succeeded.
func (s *shell) runScript(ctx context.Context, src string, stop bool) bool {
	stmts, err := lang.ParseScript(src)
	if err != nil {
		s.showError(src, 0, err)
		return false
	}
	ok := true
	for i, st := range stmts {
		start := st.Position().Offset
		end := len(src)
		if i+1 < len(stmts) {
			end = stmts[i+1].Position().Offset
		}
		if err := s.exec(ctx, st, src[start:end]); err != nil {
			s.showError(src, start, err)
			ok = false
			if stop {
				return false
			}
		}
	}
	return ok
}

// exec runs one statement. Transaction statements go through the
// polydb.Tx so that later statements run inside it.
func (s *shell) exec(ctx context.Context, st lang.Stmt, text string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if s.running != nil {
		defer s.running(cancel)()
	}
	began := time.Now()
	defer func() {
		if s.timing {
			fmt.Fprintf(s.out, "time: %s\n", time.Since(began).Round(time.Microsecond))
		}
	}()
	switch st := st.(type) {
	case *lang.Begin:
		if s.tx != nil {
			return errors.New("a transaction is already open; commit or roll back first")
		}
		tx, err := s.db.Begin(ctx, polydb.TxOptions{ReadOnly: st.ReadOnly})
		if err != nil {
			return err
		}
		s.tx, s.failed = tx, false
		fmt.Fprintln(s.out, "ok")
		return nil
	case *lang.Commit, *lang.Rollback:
		if s.tx == nil {
			return errors.New("no transaction is open")
		}
		tx := s.tx
		s.tx, s.failed = nil, false
		var err error
		if _, isCommit := st.(*lang.Commit); isCommit {
			err = tx.Commit(ctx)
		} else {
			err = tx.Rollback(ctx)
		}
		if err == nil {
			fmt.Fprintln(s.out, "ok")
		}
		return err
	}
	var q polydb.Querier = s.db
	if s.tx != nil {
		q = s.tx
	}
	rows, err := q.Query(ctx, text, polydb.Params(toParams(s.params)))
	if err == nil {
		err = s.print(rows)
		rows.Close()
	}
	if err != nil {
		if s.tx != nil {
			s.failed = true // the server aborts the transaction on any error
		}
		return err
	}
	switch st := st.(type) {
	case *lang.Use:
		s.dbName = st.Name
	case *lang.DropDatabase:
		if st.Name == s.dbName {
			s.dbName = ""
		}
	}
	return nil
}

func toParams(m map[string]value.Value) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// showError prints an error with the source line and a caret under its
// position. offset is where the statement starts in src.
func (s *shell) showError(src string, offset int, err error) {
	var pe *polydb.Error
	var le *lang.Error
	line, col := 0, 0
	msg := err.Error()
	switch {
	case errors.As(err, &pe):
		line, col, msg = pe.Line, pe.Column, pe.Code+": "+pe.Message
	case errors.As(err, &le):
		line, col, msg = le.Pos.Line, le.Pos.Col, le.Code+": "+le.Msg
		offset = 0
	}
	if line == 0 {
		s.errorf("error: %s", msg)
		return
	}
	// Map the position in the statement to the source.
	before := src[:offset]
	baseLine := strings.Count(before, "\n") + 1
	baseCol := len([]rune(before[strings.LastIndex(before, "\n")+1:])) + 1
	absLine := baseLine + line - 1
	if line == 1 {
		col += baseCol - 1
	}
	lines := strings.Split(src, "\n")
	if absLine >= 1 && absLine <= len(lines) {
		text := lines[absLine-1]
		s.errorf("error: %s", msg)
		if strings.Count(src, "\n") > 0 {
			s.errorf("  line %d: %s", absLine, text)
			s.errorf("  %s^", strings.Repeat(" ", len(fmt.Sprintf("line %d: ", absLine))+max(col-1, 0)))
		} else {
			s.errorf("  %s", text)
			s.errorf("  %s^", strings.Repeat(" ", max(col-1, 0)))
		}
		return
	}
	s.errorf("error: %s", msg)
}

// command runs a backslash command. It reports whether to quit.
func (s *shell) command(ctx context.Context, line string) bool {
	fields := strings.Fields(line)
	cmd, args := fields[0], fields[1:]
	switch cmd {
	case `\q`, `\quit`:
		return true
	case `\?`, `\h`, `\help`:
		fmt.Fprint(s.out, shellHelp)
	case `\set`:
		if len(args) == 0 {
			for _, k := range sortedNames(s.params) {
				fmt.Fprintf(s.out, "$%s = %s\n", k, s.params[k])
			}
			return false
		}
		name := strings.TrimPrefix(args[0], "$")
		expr := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line[len(cmd):]), args[0]))
		v, err := constant(expr)
		if err != nil {
			s.errorf("error: %v", err)
			return false
		}
		s.params[name] = v
	case `\unset`:
		for _, a := range args {
			delete(s.params, strings.TrimPrefix(a, "$"))
		}
	case `\timing`:
		s.timing = !s.timing
		fmt.Fprintf(s.out, "timing %s\n", map[bool]string{true: "on", false: "off"}[s.timing])
	case `\format`:
		if len(args) != 1 || (args[0] != "table" && args[0] != "tsv") {
			s.errorf(`usage: \format table|tsv`)
			return false
		}
		s.format = args[0]
	case `\i`:
		if len(args) != 1 {
			s.errorf(`usage: \i file.rhz`)
			return false
		}
		data, err := os.ReadFile(args[0])
		if err != nil {
			s.errorf("error: %v", err)
			return false
		}
		s.runScript(ctx, string(data), true)
	default:
		s.errorf(`unknown command %s; \? lists them`, cmd)
	}
	return false
}

const shellHelp = `Statements end with ";", which Enter adds when the statement is complete.
To continue a pipeline on the next line, end the line with "|". An empty
line runs what has been typed so far.

  \set name value   set parameter $name (value is a Rhizome literal or constant)
  \set              list parameters
  \unset name       remove a parameter
  \i file.rhz       run a script
  \timing           show how long each statement takes
  \format table|tsv choose the result format
  \q                quit (Ctrl-D works too)

The prompt shows * inside a transaction and ! when it has failed;
roll back to continue.
`

// constant evaluates a Rhizome constant expression for \set.
func constant(src string) (value.Value, error) {
	e, err := lang.ParseExpr(src)
	if err != nil {
		return nil, err
	}
	info, _, err := lang.CheckExpr(&lang.Env{}, e, nil)
	if err != nil {
		return nil, err
	}
	return (&lang.Evaluator{Info: info, Now: time.Now()}).Eval(e, nil)
}

// complete reports whether buf holds whole statements, so that Enter
// runs them, or ends mid-statement and needs more lines.
func complete(buf string) bool {
	trimmed := strings.TrimSpace(buf)
	if trimmed == "" {
		return false
	}
	_, err := lang.ParseScript(buf)
	var le *lang.Error
	if err == nil || !errors.As(err, &le) {
		return true
	}
	if strings.HasPrefix(le.Msg, "unterminated") {
		return false
	}
	return le.Pos.Offset < len(strings.TrimRight(buf, " \t\r\n"))
}

// repl reads statements and backslash commands until the input ends.
func (s *shell) repl(ctx context.Context, read func(prompt string) (string, error)) {
	var buf strings.Builder
	for {
		prompt := s.prompt()
		if buf.Len() > 0 {
			prompt = strings.Repeat(" ", len(prompt)-5) + "...> "
		}
		line, err := read(prompt)
		if err != nil {
			if buf.Len() > 0 {
				s.runScript(ctx, buf.String(), false)
			}
			return
		}
		if buf.Len() == 0 && strings.HasPrefix(strings.TrimSpace(line), `\`) {
			if s.command(ctx, strings.TrimSpace(line)) {
				return
			}
			continue
		}
		if strings.TrimSpace(line) == "" {
			if buf.Len() > 0 { // an empty line runs what's there
				s.runScript(ctx, buf.String(), false)
				buf.Reset()
			}
			continue
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
		if complete(buf.String()) {
			s.runScript(ctx, buf.String(), false)
			buf.Reset()
		}
	}
}

// shellCmd opens the interactive shell.
func shellCmd(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("polydb", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var f connectFlags
	f.register(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	url := defaultURL()
	if fs.NArg() > 0 {
		url = fs.Arg(0)
	}
	db, dbName, err := connect(ctx, url, &f, errOut)
	if err != nil {
		fmt.Fprintln(errOut, "polydb:", err)
		return 1
	}
	defer db.Close()

	file, isFile := in.(*os.File)
	interactive := isFile && term.IsTerminal(int(file.Fd()))
	if !interactive {
		sh := newShell(db, dbName, out, errOut, f.format)
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 1<<20), 64<<20)
		sh.repl(ctx, func(string) (string, error) {
			if scanner.Scan() {
				return scanner.Text(), nil
			}
			return "", io.EOF
		})
		return 0
	}

	fd := int(file.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		fmt.Fprintln(errOut, "polydb:", err)
		return 1
	}
	defer term.Restore(fd, state)
	t := term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{in, out}, "")
	t.History = loadHistory()
	if w, _, err := term.GetSize(fd); err == nil {
		t.SetSize(w, 0)
	}
	sh := newShell(db, dbName, t, t, f.format)
	// While a statement runs, the terminal is back in normal mode so that
	// Ctrl-C cancels the statement instead of ending the shell.
	sh.running = func(cancel context.CancelFunc) func() {
		term.Restore(fd, state)
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, os.Interrupt)
		done := make(chan struct{})
		go func() {
			select {
			case <-sigs:
				cancel()
			case <-done:
			}
		}()
		return func() {
			signal.Stop(sigs)
			close(done)
			term.MakeRaw(fd)
		}
	}
	fmt.Fprintf(t, "polydb %s — \\? for help, \\q to quit\n", polydb.Version())
	if !f.noRC {
		if home, err := os.UserHomeDir(); err == nil {
			if data, err := os.ReadFile(filepath.Join(home, ".polydbrc")); err == nil {
				sh.runScript(ctx, string(data), true)
			}
		}
	}
	sh.repl(ctx, func(prompt string) (string, error) {
		t.SetPrompt(prompt)
		return t.ReadLine()
	})
	fmt.Fprintln(t)
	return 0
}

// runCmd runs a script file and exits non-zero on the first error.
func runCmd(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var f connectFlags
	f.register(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	url, file := defaultURL(), ""
	switch fs.NArg() {
	case 1:
		file = fs.Arg(0)
	case 2:
		url, file = fs.Arg(0), fs.Arg(1)
	default:
		fmt.Fprintln(errOut, "usage: polydb run [flags] [url] file.rhz")
		return 2
	}
	var data []byte
	var err error
	if file == "-" {
		data, err = io.ReadAll(in)
	} else {
		data, err = os.ReadFile(file)
	}
	if err != nil {
		fmt.Fprintln(errOut, "polydb:", err)
		return 1
	}
	db, dbName, err := connect(ctx, url, &f, errOut)
	if err != nil {
		fmt.Fprintln(errOut, "polydb:", err)
		return 1
	}
	defer db.Close()
	sh := newShell(db, dbName, out, errOut, f.format)
	if !sh.runScript(ctx, string(data), true) {
		return 1
	}
	return 0
}
