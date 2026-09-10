package main

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	moirai "github.com/october-dev/moirai"
)

// Inspect declarations in their immediate lexical scope. In particular, the
// three archive cases each own a different fs, while importSession owns one fs
// that serves two commands. Unknown constructions fail rather than disappearing.
func TestCompletionDrift(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	var functions []*ast.FuncDecl
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, parsed)
		for _, decl := range parsed.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				functions = append(functions, fn)
			}
		}
	}
	commands := map[string]bool{}
	actual := map[string]map[string]bool{}
	handled := map[*ast.CallExpr]bool{}
	var mutationAliases []string
	literal := func(expr ast.Expr) string {
		t.Helper()
		lit, ok := expr.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Fatalf("expected string literal, got %T", expr)
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	var team *ast.FuncDecl
	// Discover dispatch before flags: shared handlers may live in earlier files.
	for _, fn := range functions {
		if fn.Name.Name == "team" {
			if team != nil {
				t.Fatal("duplicate team dispatch")
			}
			team = fn
		}
		if fn.Name.Name == "run" || fn.Name.Name == "archive" {
			for _, stmt := range fn.Body.List {
				sw, ok := stmt.(*ast.SwitchStmt)
				if !ok {
					continue
				}
				for _, stmt := range sw.Body.List {
					clause := stmt.(*ast.CaseClause)
					sharedMutation := false
					if fn.Name.Name == "run" {
						ast.Inspect(clause, func(n ast.Node) bool {
							call, ok := n.(*ast.CallExpr)
							if !ok {
								return true
							}
							sel, ok := call.Fun.(*ast.SelectorExpr)
							if !ok || sel.Sel.Name != "cloudMutation" {
								return true
							}
							receiver, ok := sel.X.(*ast.Ident)
							if !ok || receiver.Name != "a" || len(call.Args) != 3 || !completionArgZero(call.Args[1]) || len(clause.List) == 0 {
								t.Fatal("unrecognized cloudMutation dispatch")
							}
							sharedMutation = true
							return true
						})
					}
					for _, expr := range clause.List {
						name := literal(expr)
						if sharedMutation {
							mutationAliases = append(mutationAliases, name)
						}
						switch name {
						case "-h", "--help":
							name = "help"
						case "--version":
							name = "version"
						}
						if fn.Name.Name == "archive" {
							name = "archive " + name
						}
						commands[name] = true
					}
				}
			}
		}
	}
	if team == nil {
		t.Fatal("team dispatch missing")
	}
	testCompletionTeamDispatch(t, team, literal)
	for _, fn := range functions {
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			var statements []ast.Stmt
			switch n := node.(type) {
			case *ast.BlockStmt:
				statements = n.List
			case *ast.CaseClause:
				statements = n.Body
			default:
				return true
			}
			for _, stmt := range statements {
				assign, ok := stmt.(*ast.AssignStmt)
				if !ok {
					continue
				}
				for _, rhs := range assign.Rhs {
					call, ok := rhs.(*ast.CallExpr)
					if !ok {
						continue
					}
					id, ok := call.Fun.(*ast.Ident)
					if !ok || id.Name != "newFlags" {
						continue
					}
					handled[call] = true
					if len(assign.Lhs) != 1 || len(assign.Rhs) != 1 || len(call.Args) != 2 {
						t.Fatal("unrecognized newFlags assignment")
					}
					variable, ok := assign.Lhs[0].(*ast.Ident)
					if !ok {
						t.Fatal("unrecognized flag set binding")
					}
					var names []string
					switch arg := call.Args[0].(type) {
					case *ast.BasicLit:
						names = []string{literal(arg)}
					case *ast.Ident:
						switch {
						case fn.Name.Name == "importSession" && arg.Name == "name":
							names = []string{"import", "continue"}
						case fn.Name.Name == "cloudMutation" && arg.Name == "op":
							if len(mutationAliases) == 0 {
								t.Fatal("cloudMutation dispatch aliases missing")
							}
							names = mutationAliases
						default:
							t.Fatalf("unrecognized dynamic newFlags in %s", fn.Name.Name)
						}
					default:
						t.Fatalf("unrecognized newFlags argument %T", arg)
					}
					flags := map[string]bool{}
					for _, sibling := range statements {
						ast.Inspect(sibling, func(n ast.Node) bool {
							// Never cross into a nested lexical scope.
							switch n.(type) {
							case *ast.BlockStmt, *ast.CaseClause, *ast.FuncLit:
								return false
							}
							invocation, ok := n.(*ast.CallExpr)
							if !ok {
								return true
							}
							sel, ok := invocation.Fun.(*ast.SelectorExpr)
							if !ok {
								return true
							}
							receiver, ok := sel.X.(*ast.Ident)
							if !ok || receiver.Name != variable.Name {
								return true
							}
							switch sel.Sel.Name {
							case "Bool", "String", "Int", "Int64", "Duration":
								if len(invocation.Args) == 0 {
									t.Fatal("flag declaration without name")
								}
								name := literal(invocation.Args[0])
								if _, exists := flags[name]; exists {
									t.Fatalf("duplicate flag %s", name)
								}
								flags[name] = sel.Sel.Name == "Bool"
							default:
								// Reading or parsing a FlagSet is fine; new registration methods
								// must be explicitly taught to this test.
								switch sel.Sel.Name {
								case "NArg", "Arg", "Args", "Parse", "SetOutput", "Lookup":
								default:
									t.Fatalf("unrecognized fs method %s", sel.Sel.Name)
								}
							}
							return true
						})
					}
					for _, name := range names {
						if _, exists := actual[name]; exists {
							t.Fatalf("duplicate flag set %s", name)
						}
						actual[name] = flags
					}
				}
			}
			return true
		})
	}
	for _, parsed := range files {
		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if ok && id.Name == "newFlags" && !handled[call] {
				t.Fatal("newFlags construction not accounted for")
			}
			return true
		})
	}
	expectedCommands := map[string]bool{}
	eachCommand(func(name string, c commandSpec) {
		if expectedCommands[name] {
			t.Fatalf("duplicate spec %s", name)
		}
		expectedCommands[name] = true
		expected := map[string]bool{}
		for _, f := range c.flags {
			if _, exists := expected[f.name]; exists {
				t.Fatalf("duplicate spec flag %s %s", name, f.name)
			}
			switch f.valueType {
			case boolValue, formatValue, fileValue, opaqueValue:
			default:
				t.Fatalf("unknown flag type %q", f.valueType)
			}
			expected[f.name] = f.valueType == boolValue
		}
		got := actual[name]
		if got == nil {
			got = map[string]bool{}
		}
		if !reflect.DeepEqual(got, expected) {
			t.Errorf("%s flag arity drift: CLI %v, completion %v", name, got, expected)
		}
	})
	if !reflect.DeepEqual(commands, expectedCommands) {
		t.Errorf("command drift: CLI %v, completion %v", commands, expectedCommands)
	}
	for name := range actual {
		if !expectedCommands[name] {
			t.Errorf("flag set %q missing from spec", name)
		}
	}
	shells := []string{}
	for _, fn := range functions {
		if fn.Name.Name != "completion" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			clause, ok := n.(*ast.CaseClause)
			if ok {
				for _, expr := range clause.List {
					shells = append(shells, literal(expr))
				}
			}
			return true
		})
	}
	slices.Sort(shells)
	expectedShells := slices.Clone(completionShells)
	slices.Sort(expectedShells)
	if !slices.Equal(shells, expectedShells) {
		t.Fatalf("shell dispatch drift: %v != %v", shells, expectedShells)
	}
}

func completionArgZero(expr ast.Expr) bool {
	index, ok := expr.(*ast.IndexExpr)
	if !ok {
		return false
	}
	args, ok := index.X.(*ast.Ident)
	if !ok || args.Name != "args" {
		return false
	}
	zero, ok := index.Index.(*ast.BasicLit)
	return ok && zero.Kind == token.INT && zero.Value == "0"
}

// Team has one flat flag set. Its dispatch is specifically a switch on args[0]
// plus literal ==/!= comparisons; fail if that shape changes instead of trying
// to infer arbitrary control flow or silently losing an enum member.
func testCompletionTeamDispatch(t *testing.T, fn *ast.FuncDecl, literal func(ast.Expr) string) {
	t.Helper()
	commands := map[string]bool{}
	handled := map[ast.Expr]bool{}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.SwitchStmt:
			if completionArgZero(n.Tag) {
				handled[n.Tag] = true
				for _, stmt := range n.Body.List {
					for _, expr := range stmt.(*ast.CaseClause).List {
						commands[literal(expr)] = true
					}
				}
			}
		case *ast.BinaryExpr:
			if completionArgZero(n.X) || completionArgZero(n.Y) {
				if !completionArgZero(n.X) || (n.Op != token.EQL && n.Op != token.NEQ) {
					t.Fatal("unrecognized team dispatch comparison")
				}
				handled[n.X] = true
				commands[literal(n.Y)] = true
			}
		}
		return true
	})
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if expr, ok := node.(ast.Expr); ok && completionArgZero(expr) && !handled[expr] {
			t.Fatal("unrecognized team dispatch use of args[0]")
		}
		return true
	})
	expected := map[string]bool{}
	for _, name := range completionTeamCommands {
		if expected[name] {
			t.Fatalf("duplicate team completion %q", name)
		}
		expected[name] = true
	}
	if !reflect.DeepEqual(commands, expected) {
		t.Errorf("team dispatch drift: CLI %v, completion %v", commands, expected)
	}
}

func completionScript(t *testing.T, shell string) string {
	t.Helper()
	var out bytes.Buffer
	if err := (app{out: &out, err: io.Discard}).run(context.Background(), []string{"completion", shell}); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

type completionFailWriter struct{ err error }

func (w completionFailWriter) Write([]byte) (int, error) { return 0, w.err }
func TestCompletionHandler(t *testing.T) {
	for _, args := range [][]string{{"completion"}, {"completion", "nope"}, {"completion", "bash", "extra"}} {
		var out bytes.Buffer
		if err := (app{out: &out, err: io.Discard}).run(context.Background(), args); err == nil || out.Len() != 0 {
			t.Fatalf("args %v: error %v, stdout %q", args, err, out.String())
		}
	}
	for _, shell := range completionShells {
		t.Run(shell, func(t *testing.T) {
			first := completionScript(t, shell)
			if first != completionScript(t, shell) {
				t.Fatal("nondeterministic output")
			}
			for _, format := range moirai.Formats {
				if !strings.Contains(first, string(format)) {
					t.Errorf("missing format %s", format)
				}
			}
			failure := errors.New("write failed")
			if err := (app{out: completionFailWriter{failure}, err: io.Discard}).run(context.Background(), []string{"completion", shell}); !errors.Is(err, failure) {
				t.Fatalf("write error not propagated: %v", err)
			}
		})
	}
}

func completionSandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"a file.json", "-input.json", "create", "archive"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	fake := "#!/bin/sh\nprintf invoked >> \"$MOIRAI_COMPLETION_LOG\"\nexit 97\n"
	if err := os.WriteFile(filepath.Join(dir, "moirai"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "invocations")
	t.Setenv("MOIRAI_COMPLETION_LOG", log)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() {
		if data, err := os.ReadFile(log); err == nil {
			t.Errorf("completion invoked moirai: %s", data)
		} else if !os.IsNotExist(err) {
			t.Error(err)
		}
	})
	return dir
}
func assertCandidates(t *testing.T, got, want, absent []string, empty bool) {
	t.Helper()
	for _, v := range want {
		if !slices.Contains(got, v) {
			t.Errorf("missing %q in %q", v, got)
		}
	}
	for _, v := range absent {
		if slices.Contains(got, v) {
			t.Errorf("unexpected %q in %q", v, got)
		}
	}
	if empty && len(got) > 0 {
		t.Errorf("expected no candidates, got %q", got)
	}
}
func TestBashCompletion(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	dir := completionSandbox(t)
	script := completionScript(t, "bash")
	formats := []string{}
	for _, f := range moirai.Formats {
		formats = append(formats, string(f))
	}
	tests := []struct {
		name                string
		words, want, absent []string
		empty               bool
	}{
		{name: "top", words: []string{"moirai", "co"}, want: []string{"convert", "continue", "completion"}},
		{name: "archive", words: []string{"moirai", "archive", ""}, want: []string{"create", "verify", "inspect"}},
		{name: "create flags", words: []string{"moirai", "archive", "create", "--"}, want: []string{"--from", "--out"}},
		{name: "verify flags", words: []string{"moirai", "archive", "verify", "--"}, want: []string{"--max-input-bytes"}, absent: []string{"--from", "--out"}},
		{name: "inspect flags", words: []string{"moirai", "archive", "inspect", "--"}, want: []string{"--json", "--max-input-bytes"}, absent: []string{"--from", "--out"}},
		{name: "format", words: []string{"moirai", "convert", "--from", ""}, want: formats},
		{name: "equals", words: []string{"moirai", "convert", "--from=co"}, want: []string{"--from=codex", "--from=cowork"}},
		{name: "split equals", words: []string{"moirai", "convert", "--from", "=", "co"}, want: []string{"codex", "cowork"}},
		{name: "split equals empty", words: []string{"moirai", "convert", "--from", "="}, want: formats},
		{name: "single dash", words: []string{"moirai", "convert", "-from", "co"}, want: []string{"codex"}},
		{name: "single dash equals", words: []string{"moirai", "convert", "-from=co"}, want: []string{"-from=codex"}},
		{name: "positional then flag", words: []string{"moirai", "inspect", "a file.json", "--"}, want: []string{"--from", "--json"}},
		{name: "search collision", words: []string{"moirai", "search", "archive", "--"}, want: []string{"--format", "--limit"}, absent: []string{"--from", "--out"}},
		{name: "convert collision", words: []string{"moirai", "convert", "create", "--"}, want: []string{"--from", "--to"}},
		{name: "value collision", words: []string{"moirai", "search", "--format", "archive", "--"}, want: []string{"--limit"}, absent: []string{"--out"}},
		{name: "bool consumes nothing", words: []string{"moirai", "show", "--json", "--"}, want: []string{"--format"}},
		{name: "opaque value", words: []string{"moirai", "search", "--limit", ""}, empty: true},
		{name: "out file", words: []string{"moirai", "export", "--out", "a"}, want: []string{"a file.json"}},
		{name: "space file", words: []string{"moirai", "inspect", "a "}, want: []string{"a file.json"}},
		{name: "after terminator file", words: []string{"moirai", "convert", "--", "-"}, want: []string{"-input.json"}, absent: []string{"--from", "--out"}},
		{name: "after terminator format", words: []string{"moirai", "convert", "--", "--from", "co"}, empty: true},
		{name: "after terminator id", words: []string{"moirai", "show", "--", ""}, empty: true},
		{name: "after terminator query", words: []string{"moirai", "search", "--", ""}, empty: true},
		{name: "continue file", words: []string{"moirai", "continue", "--", "a "}, want: []string{"a file.json"}},
		{name: "shell enum", words: []string{"moirai", "completion", ""}, want: completionShells},
		{name: "cloud commands", words: []string{"moirai", ""}, want: []string{"login", "logout", "whoami", "doctor", "publish", "pull", "fork", "unpublish", "cloud-delete", "invite", "team"}},
		{name: "login flags", words: []string{"moirai", "login", "--"}, want: []string{"--server"}},
		{name: "login value", words: []string{"moirai", "login", "--server", ""}, empty: true},
		{name: "doctor flags", words: []string{"moirai", "doctor", "--"}, want: []string{"--json"}},
		{name: "publish flags", words: []string{"moirai", "publish", "--"}, want: []string{"--from", "--visibility", "--expires", "--parent", "--team", "--yes", "--preview-out", "--include-thinking", "--idempotency-key"}},
		{name: "publish format", words: []string{"moirai", "publish", "--from", "co"}, want: []string{"codex", "cowork"}},
		{name: "duration value", words: []string{"moirai", "publish", "--expires", ""}, empty: true},
		{name: "duration equals", words: []string{"moirai", "publish", "--expires="}, empty: true},
		{name: "duration consumes value", words: []string{"moirai", "publish", "--expires", "24h", "--"}, want: []string{"--preview-out", "--yes"}},
		{name: "visibility value", words: []string{"moirai", "publish", "--visibility", ""}, empty: true},
		{name: "preview file", words: []string{"moirai", "publish", "--preview-out", "a"}, want: []string{"a file.json"}},
		{name: "publish file", words: []string{"moirai", "publish", "a"}, want: []string{"a file.json"}},
		{name: "pull file", words: []string{"moirai", "pull", "--out", "a"}, want: []string{"a file.json"}},
		{name: "fork flags", words: []string{"moirai", "fork", "ID", "--"}, want: []string{"--yes"}},
		{name: "unpublish flags", words: []string{"moirai", "unpublish", "ID", "--"}, want: []string{"--yes", "--login"}},
		{name: "cloud delete flags", words: []string{"moirai", "cloud-delete", "ID", "--"}, want: []string{"--yes", "--login"}},
		{name: "invite flags", words: []string{"moirai", "invite", "ID", "--"}, want: []string{"--yes", "--login"}},
		{name: "mutation bool", words: []string{"moirai", "unpublish", "ID", "--yes", "--"}, want: []string{"--login"}},
		{name: "mutation value", words: []string{"moirai", "invite", "ID", "--login", ""}, empty: true},
		{name: "import flags", words: []string{"moirai", "import", "--"}, want: []string{"--dry-run", "--json"}},
		{name: "continue flags", words: []string{"moirai", "continue", "--"}, want: []string{"--dry-run", "--json"}},
		{name: "import bool", words: []string{"moirai", "import", "--dry-run", "--"}, want: []string{"--json", "--from"}},
		{name: "continue bool", words: []string{"moirai", "continue", "--json", "--"}, want: []string{"--dry-run", "--with"}},
		{name: "team enum", words: []string{"moirai", "team", ""}, want: []string{"list", "create", "members", "invite", "remove", "--user", "--role"}},
		{name: "team name", words: []string{"moirai", "team", "create", ""}, absent: completionTeamCommands},
		{name: "team flags", words: []string{"moirai", "team", "invite", "ID", "--"}, want: []string{"--user", "--role"}},
		{name: "team flags before ID", words: []string{"moirai", "team", "invite", "--"}, want: []string{"--user", "--role"}},
		{name: "team user", words: []string{"moirai", "team", "invite", "ID", "--user", ""}, empty: true},
		{name: "team value collision", words: []string{"moirai", "team", "invite", "ID", "--user", "create", "--"}, want: []string{"--role"}, absent: []string{"--from", "--out"}},
		{name: "team value position", words: []string{"moirai", "team", "--user", "create", ""}, want: completionTeamCommands},
		{name: "team terminator", words: []string{"moirai", "team", "invite", "--", ""}, empty: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			quoted := make([]string, len(tt.words))
			for i, w := range tt.words {
				quoted[i] = shellQuote(w)
			}
			fixture := script + "\nCOMP_WORDS=(" + strings.Join(quoted, " ") + ")\nCOMP_CWORD=" + strconv.Itoa(len(tt.words)-1) + "\nCOMP_LINE=" + shellQuote(strings.Join(tt.words, " ")) + "\nCOMP_POINT=${#COMP_LINE}\n_moirai\nif (( ${#COMPREPLY[@]} )); then printf '%s\\0' \"${COMPREPLY[@]}\"; fi\n"
			cmd := exec.Command(bash, "--noprofile", "--norc")
			cmd.Stdin = strings.NewReader(fixture)
			cmd.Dir = dir
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			data, err := cmd.Output()
			if err != nil {
				t.Fatalf("bash: %v: %s", err, stderr.String())
			}
			var got []string
			if len(data) > 0 {
				got = strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
			}
			assertCandidates(t, got, tt.want, tt.absent, tt.empty)
		})
	}
}

func TestNativeCompletion(t *testing.T) {
	for _, shell := range []string{"zsh", "fish"} {
		t.Run(shell, func(t *testing.T) {
			executable, err := exec.LookPath(shell)
			if err != nil {
				if os.Getenv("MOIRAI_REQUIRE_NATIVE_COMPLETION") == "1" {
					t.Fatal(shell + " is required for native completion tests")
				}
				t.Skip(shell + " unavailable")
			}
			dir := completionSandbox(t)
			path := filepath.Join(dir, "completion."+shell)
			if shell == "zsh" {
				path = filepath.Join(dir, "_moirai")
			}
			if err := os.WriteFile(path, []byte(completionScript(t, shell)), 0600); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command(executable, "-n", path).CombinedOutput(); err != nil {
				t.Fatalf("syntax: %v: %s", err, out)
			}
			tests := []struct {
				line         string
				want, absent []string
				empty        bool
			}{
				{line: "moirai co", want: []string{"convert", "continue", "completion"}},
				{line: "moirai archive ", want: []string{"create", "verify", "inspect"}},
				{line: "moirai archive create --", want: []string{"--from", "--out"}},
				{line: "moirai archive verify --", want: []string{"--max-input-bytes"}, absent: []string{"--from", "--out"}},
				{line: "moirai archive inspect --", want: []string{"--json", "--max-input-bytes"}, absent: []string{"--from", "--out"}},
				{line: "moirai convert --from=co", want: []string{"codex", "cowork"}},
				{line: "moirai convert --from co", want: []string{"codex", "cowork"}},
				{line: "moirai search archive --", want: []string{"--format", "--limit"}, absent: []string{"--from", "--out"}},
				{line: "moirai convert create --", want: []string{"--from", "--to", "--out"}},
				{line: "moirai inspect 'a file.json' --", want: []string{"--from", "--json"}},
				{line: "moirai convert -- -", want: []string{"-input.json"}, absent: []string{"--from", "--to"}},
				{line: "moirai show -- ", empty: true},
				{line: "moirai search -- ", empty: true},
				{line: "moirai convert -- --from co", absent: []string{"codex", "cowork", "--from=codex", "--from=cowork"}},
				{line: "moirai search --limit ", empty: true},
				{line: "moirai inspect a", want: []string{"a file.json"}},
				{line: "moirai ", want: []string{"login", "logout", "whoami", "doctor", "publish", "pull", "fork", "unpublish", "cloud-delete", "invite", "team"}},
				{line: "moirai login --", want: []string{"--server"}},
				{line: "moirai login --server ", empty: true},
				{line: "moirai doctor --", want: []string{"--json"}},
				{line: "moirai publish --", want: []string{"--from", "--visibility", "--expires", "--parent", "--team", "--yes", "--preview-out", "--include-thinking", "--idempotency-key"}},
				{line: "moirai publish --from co", want: []string{"codex", "cowork"}},
				{line: "moirai publish --expires ", empty: true},
				{line: "moirai publish --expires=", empty: true},
				{line: "moirai publish --expires 24h --", want: []string{"--preview-out", "--yes"}},
				{line: "moirai publish --visibility ", empty: true},
				{line: "moirai publish --preview-out a", want: []string{"a file.json"}},
				{line: "moirai publish a", want: []string{"a file.json"}},
				{line: "moirai pull --out a", want: []string{"a file.json"}},
				{line: "moirai fork ID --", want: []string{"--yes"}},
				{line: "moirai unpublish ID --", want: []string{"--yes", "--login"}},
				{line: "moirai cloud-delete ID --", want: []string{"--yes", "--login"}},
				{line: "moirai invite ID --", want: []string{"--yes", "--login"}},
				{line: "moirai unpublish ID --yes --", want: []string{"--login"}},
				{line: "moirai invite ID --login ", empty: true},
				{line: "moirai import --", want: []string{"--dry-run", "--json"}},
				{line: "moirai continue --", want: []string{"--dry-run", "--json"}},
				{line: "moirai import --dry-run --", want: []string{"--json", "--from"}},
				{line: "moirai continue --json --", want: []string{"--dry-run", "--with"}},
				{line: "moirai team ", want: completionTeamCommands},
				{line: "moirai team create ", absent: completionTeamCommands},
				{line: "moirai team invite ID --", want: []string{"--user", "--role"}},
				{line: "moirai team invite --", want: []string{"--user", "--role"}},
				{line: "moirai team invite ID --user ", empty: true},
				{line: "moirai team invite ID --user create --", want: []string{"--role"}, absent: []string{"--from", "--out"}},
				{line: "moirai team invite -- ", empty: true},
			}
			for _, tt := range tests {
				t.Run(tt.line, func(t *testing.T) {
					var got []string
					if shell == "fish" {
						cmd := exec.Command(executable, "--no-config", "-c", "source "+shellQuote(path)+"; complete -C "+shellQuote(tt.line))
						cmd.Dir = dir
						data, err := cmd.CombinedOutput()
						if err != nil {
							t.Fatalf("fish: %v: %s", err, data)
						}
						for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
							if line == "" {
								continue
							}
							candidate, _, _ := strings.Cut(line, "\t")
							// Fish returns the whole equals word, while zsh compadd returns the value.
							if strings.Contains(tt.line, "--from=") {
								candidate = strings.TrimPrefix(candidate, "--from=")
							}
							got = append(got, candidate)
						}
					} else {
						got = zshCandidates(t, executable, dir, path, tt.line)
					}
					assertCandidates(t, got, tt.want, tt.absent, tt.empty)
				})
			}
		})
	}
}

// Exercise the real zsh completion system inside ZLE. Merely mocking
// _arguments would miss native value parsing, '--', and option placement.
func zshCandidates(t *testing.T, executable, dir, script, line string) []string {
	t.Helper()
	fixtureDir := t.TempDir()
	result := filepath.Join(fixtureDir, "result")
	setup := filepath.Join(fixtureDir, "setup.zsh")
	fixture := `fpath=( ` + shellQuote(filepath.Dir(script)) + ` $fpath )
autoload -Uz compinit
compinit -u -D
if [[ ${_comps[moirai]-} != _moirai ]]; then
    print -r -- "wrong moirai completion handler: ${_comps[moirai]-<unset>}"
    autoload -Uz compaudit
    compaudit
    exit 1
fi
compadd() {
    local -a matches
    local before=$compstate[nmatches] ret
    builtin compadd "$@"
    ret=$?
    # _arguments also uses compadd to query arrays without offering matches.
    # Probe only real additions, so queries do not filter those arrays twice.
    if (( compstate[nmatches] > before )); then
        builtin compadd -A matches "$@"
        collected+=( "${(@Q)matches}" )
    fi
    return $ret
}
_capture() {
    collected=()
    _main_complete
    if (( ${#collected} )); then
        print -rl -- "${collected[@]}" > ` + shellQuote(result) + `
    else
        : > ` + shellQuote(result) + `
    fi
    print -r -- '__MOIRAI_END__'
}
_run_capture() {
    BUFFER=` + shellQuote(line) + `
    CURSOR=${#BUFFER}
    zle _capture
}
zle -C _capture complete-word _capture
zle -N _run_capture
bindkey -e
bindkey '^X' _run_capture
_ready() { print -r -- '__MOIRAI_READY__'; }
zle -N zle-line-init _ready
`
	if err := os.WriteFile(setup, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	driver := `zmodload zsh/zpty || exit 1
zmodload zsh/datetime || exit 1
zpty -b worker ` + shellQuote(executable) + ` -f || exit 1
trap 'zpty -d worker 2>/dev/null' EXIT
transcript=''
wait_marker() {
    local marker=$1 chunk
    local -F deadline=$(( EPOCHREALTIME + 5 ))
    while (( EPOCHREALTIME < deadline )); do
        # No pattern read: partial output and child exit must not count as a
        # marker. Stream every chunk to Go even while waiting for more output.
        if zpty -r -t worker chunk; then
            transcript+=$chunk
            print -rn -- "$chunk"
            [[ $transcript == *$marker* ]] && return 0
        else
            if ! zpty -t worker; then
                print -r -- "child exited before $marker"
                return 1
            fi
            sleep 0.01
        fi
    done
    print -r -- "timed out waiting for $marker"
    return 1
}
zpty -w worker ` + shellQuote("source "+shellQuote(setup)) + ` || exit 1
wait_marker '__MOIRAI_READY__' || exit 1
zpty -w -n worker $'\x18' || exit 1
wait_marker '__MOIRAI_END__' || exit 1
`
	// Normal timeouts exit the driver and run its zpty cleanup. The longer Go
	// deadline is only an emergency kill; it does not guarantee child cleanup.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-f", "-c", driver)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.WaitDelay = time.Second
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("zsh ZLE harness: %v (context: %v): %s", err, ctx.Err(), data)
	}
	data, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}
