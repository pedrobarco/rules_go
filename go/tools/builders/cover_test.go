package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type test struct {
	name string
	in   string
	out  string
}

var tests = []test{
	{
		name: "no imports",
		in: `package main
`,
		out: `package main; import "github.com/bazelbuild/rules_go/go/tools/coverdata"

func init() {
	coverdata.RegisterSrcPathMapping("some.importh/path/file.go", "src/path/file.go")
}
`,
	},
	{
		name: "other imports",
		in: `package main

import (
	"os"
)
`,
		out: `package main; import "github.com/bazelbuild/rules_go/go/tools/coverdata"

import (
	"os"
)

func init() {
	coverdata.RegisterSrcPathMapping("some.importh/path/file.go", "src/path/file.go")
}
`,
	},
	{
		name: "existing import",
		in: `package main

import "github.com/bazelbuild/rules_go/go/tools/coverdata"
`,
		out: `package main

import "github.com/bazelbuild/rules_go/go/tools/coverdata"

func init() {
	coverdata.RegisterSrcPathMapping("some.importh/path/file.go", "src/path/file.go")
}
`,
	},
	{
		name: "existing _ import",
		in: `package main

import _ "github.com/bazelbuild/rules_go/go/tools/coverdata"
`,
		out: `package main

import coverdata "github.com/bazelbuild/rules_go/go/tools/coverdata"

func init() {
	coverdata.RegisterSrcPathMapping("some.importh/path/file.go", "src/path/file.go")
}
`,
	},
	{
		name: "existing renamed import",
		in: `package main

import cover0 "github.com/bazelbuild/rules_go/go/tools/coverdata"
`,
		out: `package main

import cover0 "github.com/bazelbuild/rules_go/go/tools/coverdata"

func init() {
	cover0.RegisterSrcPathMapping("some.importh/path/file.go", "src/path/file.go")
}
`,
	},
}

func TestRegisterCoverage(t *testing.T) {
	var filename = filepath.Join(t.TempDir(), "test_input.go")
	for _, test := range tests {
		if err := ioutil.WriteFile(filename, []byte(test.in), 0666); err != nil {
			t.Errorf("writing input file: %v", err)
			return
		}
		err := registerCoverage(filename, "some.importh/path/file.go", "src/path/file.go")
		if err != nil {
			t.Errorf("%q: %+v", test.name, err)
			continue
		}
		coverSrc, err := os.ReadFile(filename)
		if err != nil {
			t.Errorf("%q: %+v", test.name, err)
			continue
		}
		if got, want := string(coverSrc), test.out; got != want {
			t.Errorf("%q: got %v, want %v", test.name, got, want)
		}
	}
}

func TestInstrumentForBranchCoverage(t *testing.T) {
	const src = `package sample

func Classify(n int) string {
	if n == 0 {
		return "zero"
	}
	for i := 0; i < n; i++ {
		_ = i
	}
	switch n {
	case 1:
		return "one"
	case 2:
		return "two"
	}
	return "many"
}
`
	// The controlling conditions in branch mode are: the 'if' condition, the
	// 'for' condition, and one comparison per 'case' clause (2). All four must
	// be wrapped and recorded in the generated condition table.
	const wantConds = 4

	dir := t.TempDir()
	in := filepath.Join(dir, "sample.go")
	out := filepath.Join(dir, "cover_0.go")
	if err := os.WriteFile(in, []byte(src), 0o666); err != nil {
		t.Fatalf("writing input: %v", err)
	}

	runtimeFile, err := instrumentForBranchCoverage([]string{in}, []string{out})
	if err != nil {
		t.Fatalf("instrumentForBranchCoverage: %v", err)
	}

	instrumented, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading instrumented source: %v", err)
	}
	runtime, err := os.ReadFile(runtimeFile)
	if err != nil {
		t.Fatalf("reading runtime: %v", err)
	}

	// The instrumented source and the generated runtime together form a
	// self-contained package, which must type-check (i.e. be compilable).
	fset := token.NewFileSet()
	instrumentedAST, err := parser.ParseFile(fset, out, instrumented, parser.AllErrors)
	if err != nil {
		t.Fatalf("instrumented source does not parse: %v\n%s", err, instrumented)
	}
	runtimeAST, err := parser.ParseFile(fset, runtimeFile, runtime, parser.AllErrors)
	if err != nil {
		t.Fatalf("runtime does not parse: %v\n%s", err, runtime)
	}
	conf := types.Config{}
	if _, err := conf.Check("sample", fset, []*ast.File{instrumentedAST, runtimeAST}, nil); err != nil {
		t.Fatalf("instrumented package does not type-check: %v\ninstrumented:\n%s\nruntime:\n%s", err, instrumented, runtime)
	}

	if got := strings.Count(string(instrumented), "GobcoCover("); got != wantConds {
		t.Errorf("instrumented source has %d GobcoCover calls, want %d\n%s", got, wantConds, instrumented)
	}
	if got := strings.Count(string(runtime), "0, 0},"); got != wantConds {
		// Each condition table entry is emitted as `{<pos>, <code>, 0, 0},`.
		t.Errorf("runtime condition table has %d entries, want %d\n%s", got, wantConds, runtime)
	}
	if !strings.Contains(string(runtime), "func GobcoCover(idx int, cond bool) bool") {
		t.Errorf("runtime does not define GobcoCover\n%s", runtime)
	}
}
