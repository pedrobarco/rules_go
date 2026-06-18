// This file is adapted from rillig/gobco (https://github.com/rillig/gobco),
// distributed under the BSD 2-Clause License, Copyright (c) 2024, Roland
// Illig. The full license text is reproduced at the top of gobco_codegen.go.
//
// Only the subset of gobco required for source-to-source branch (decision)
// coverage instrumentation has been vendored, and it has been adapted to run
// inside the rules_go GoCompilePkg builder. Notable adaptations:
//   - The whole-directory driver, TestMain rewriting and black-box test bridge
//     have been removed; rules_go owns test-main generation and finalization.
//   - Type resolution is best-effort: dependency sources are unavailable in a
//     Bazel action, so type checking may fail partially without aborting.
//   - Only gobco's branch-coverage mode is used.

package main

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/printer"
	"go/token"
	"go/types"
	"reflect"
	"strings"
)

// cond is a condition from the code that is instrumented.
type cond struct {
	pos  string // for example "main.go:17:13"
	text string // for example "i > 0"
}

// exprSubst prepares to later replace '*ref' with 'expr'.
type exprSubst struct {
	ref  *ast.Expr
	expr ast.Expr
	pos  token.Pos
	text string
}

// instrumenter rewrites the code of a Go package by instrumenting all
// controlling conditions in the code for branch (decision) coverage.
type instrumenter struct {
	branch bool // branch coverage, not condition coverage

	fset    *token.FileSet
	typ     map[ast.Expr]types.Type
	typePkg *types.Package // while instrumenting, the current package

	// Generates variable names that are unique per function.
	varname int

	// The conditions are first marked as relevant, then some complex
	// conditions are unmarked if they are redundant, and finally they are
	// instrumented in source code order.
	marked map[ast.Expr]bool

	// All conditions and their planned replacements.
	exprSubst map[ast.Expr]*exprSubst

	// Records for each statement the single place where it is referenced.
	stmtRef map[ast.Stmt]*ast.Stmt

	// All statements (expression switch and type switch) and their planned
	// replacements. For simplicity of implementation, a statement can only be
	// replaced with a single other statement, but not with a slice.
	stmtSubst map[ast.Stmt]ast.Stmt

	// The conditions from the original code that were instrumented, from all
	// files of the package.
	conds []cond
}

// newBranchInstrumenter returns an instrumenter configured for branch coverage.
func newBranchInstrumenter(fset *token.FileSet) *instrumenter {
	return &instrumenter{
		branch:    true,
		fset:      fset,
		typ:       make(map[ast.Expr]types.Type),
		marked:    make(map[ast.Expr]bool),
		exprSubst: make(map[ast.Expr]*exprSubst),
		stmtRef:   make(map[ast.Stmt]*ast.Stmt),
		stmtSubst: make(map[ast.Stmt]ast.Stmt),
	}
}

// resolveTypes performs best-effort type resolution over the given files.
//
// In a Bazel action the source of dependencies is usually unavailable, so type
// checking is expected to fail partially. Any type information that could be
// resolved is recorded; the rest is omitted. Types are only needed for the
// rare case of a named boolean type (such as 'type MyBool bool'), so degrading
// gracefully here is acceptable.
func (i *instrumenter) resolveTypes(files []*ast.File) {
	if len(files) == 0 {
		return
	}
	defer func() { _ = recover() }()

	imp := importer.ForCompiler(i.fset, "source", nil)
	conf := types.Config{
		Importer: imp,
		Error:    func(error) {}, // keep going past unresolved imports
	}
	info := types.Info{Types: make(map[ast.Expr]types.TypeAndValue)}
	pkg, _ := conf.Check(files[0].Name.Name, i.fset, files, &info)
	i.typePkg = pkg
	for expr, tv := range info.Types {
		i.typ[expr] = tv.Type
	}
}

// instrumentFileNode rewrites astFile in place, wrapping each controlling
// condition in a call to GobcoCover.
func (i *instrumenter) instrumentFileNode(astFile *ast.File) {
	ast.Inspect(astFile, i.markConds)
	ast.Inspect(astFile, i.findRefs)
	ast.Inspect(astFile, i.prepareStmts)
	ast.Inspect(astFile, i.replace)
}

// markConds remembers the conditions that will be instrumented later.
//
// In branch coverage mode, only the whole controlling condition of an 'if',
// 'for' or 'switch' is instrumented.
func (i *instrumenter) markConds(n ast.Node) bool {
	// The order of the cases matches the order in ast.Walk.
	switch n := n.(type) {

	case *ast.ParenExpr:
		if i.marked[n] {
			delete(i.marked, n)
			i.marked[n.X] = true
		}

	case *ast.UnaryExpr:
		if i.branch {
			break
		}
		if n.Op == token.NOT {
			delete(i.marked, n)
			i.marked[n.X] = true
		}

	case *ast.BinaryExpr:
		if i.branch {
			break
		}
		if n.Op == token.LAND || n.Op == token.LOR {
			delete(i.marked, n)
			i.marked[n.X] = true
			i.marked[n.Y] = true
		}
		if n.Op.Precedence() == token.EQL.Precedence() {
			i.marked[n] = true
		}

	case *ast.IfStmt:
		i.marked[n.Cond] = true

	case *ast.SwitchStmt:
		if n.Tag == nil {
			for _, clause := range n.Body.List {
				for _, expr := range clause.(*ast.CaseClause).List {
					i.marked[expr] = true
				}
			}
		}

	case *ast.ForStmt:
		if n.Cond != nil {
			i.marked[n.Cond] = true
		}

	case *ast.GenDecl:
		if n.Tok == token.CONST {
			return false
		}
	}

	return true
}

// findRefs remembers, for each relevant expression or statement, from which
// single location it is referenced. This information is later used to replace
// expressions or statements with their instrumented counterparts.
func (i *instrumenter) findRefs(n ast.Node) bool {
	if n == nil {
		return true
	}

	// For each struct field and slice element, remember the reference that
	// points to it. Since there are many ast.Node types that have ast.Expr
	// fields, it is simpler to use reflection to find all these fields instead
	// of listing the known types and their fields explicitly.
	if node := reflect.ValueOf(n); node.Type().Kind() == reflect.Ptr {
		if typ := node.Type().Elem(); typ.Kind() == reflect.Struct {
			str := node.Elem()
			for fi, nf := 0, str.NumField(); fi < nf; fi++ {
				i.findRefsField(str.Field(fi))
			}
		}
	}

	return true
}

// findRefsField remembers, for a particular field of a node in the AST, from
// which single location it is referenced.
func (i *instrumenter) findRefsField(field reflect.Value) {
	switch val := field.Interface().(type) {

	case ast.Expr:
		expr := val
		if i.marked[expr] {
			delete(i.marked, expr)
			ref := field.Addr().Interface().(*ast.Expr)
			i.exprSubst[expr] = &exprSubst{
				ref, expr, expr.Pos(), i.str(expr),
			}
		}

	case []ast.Expr:
		for ei, expr := range val {
			if i.marked[expr] {
				delete(i.marked, expr)
				i.exprSubst[expr] = &exprSubst{
					&val[ei], expr, expr.Pos(), i.str(expr),
				}
			}
		}

	case ast.Stmt:
		if field.Type() == reflect.TypeOf((*ast.Stmt)(nil)).Elem() {
			i.stmtRef[val] = field.Addr().Interface().(*ast.Stmt)
		}

	case []ast.Stmt:
		for si, stmt := range val {
			i.stmtRef[stmt] = &val[si]
		}
	}
}

func (i *instrumenter) prepareStmts(n ast.Node) bool {
	switch n := n.(type) {

	case *ast.SwitchStmt:
		i.prepareSwitchStmt(n)

	case *ast.TypeSwitchStmt:
		i.prepareTypeSwitchStmt(n)

	case *ast.FuncDecl:
		i.varname = 0
	}

	return true
}

func (i *instrumenter) prepareSwitchStmt(n *ast.SwitchStmt) {
	if n.Tag == nil {
		return // Already handled in instrumenter.markConds.
	}

	// In a switch statement with a tag expression, the expression is evaluated
	// once and is then compared to each expression from the case clauses.
	//
	// In the instrumented switch statement, the tag expression always has
	// boolean type, and the expressions in the case clauses are replaced with
	// calls of the form 'GobcoCover(id++, tag == expr)'.
	tagExprName := i.nextVarname()
	tagExprUsed := false

	for _, clause := range n.Body.List {
		clause := clause.(*ast.CaseClause)
		for j, expr := range clause.List {
			gen := codeGenerator{expr.Pos()}
			i.exprSubst[expr] = &exprSubst{
				&clause.List[j],
				gen.eql(tagExprName, expr),
				expr.Pos(),
				i.strEql(n.Tag, expr),
			}
			tagExprUsed = true
		}
	}

	gen := codeGenerator{n.Pos()}
	var newBody []ast.Stmt
	if n.Init != nil {
		newBody = append(newBody, n.Init)
	}
	tagRef := []ast.Expr{n.Tag}
	newBody = append(newBody, gen.defineExprs(tagExprName, tagRef))
	if !tagExprUsed {
		newBody = append(newBody, gen.use(gen.ident(tagExprName)))
	}
	newBody = append(newBody, gen.switchStmt(nil, n.Body))
	i.fixStmtRefs(newBody)

	// The initialization statements are executed in a new scope. Reuse the
	// same scope for storing the tag expression in a variable, as the variable
	// names don't overlap.
	i.stmtSubst[n] = gen.block(newBody)

	// n.Tag moves from the switch statement to an assignment, so update the
	// reference to it.
	if s := i.exprSubst[n.Tag]; s != nil {
		s.ref = &tagRef[0]
	}
}

func (i *instrumenter) prepareTypeSwitchStmt(ts *ast.TypeSwitchStmt) {
	gen := codeGenerator{ts.Switch}

	// Get access to the tag expression and the optional variable name from
	// 'switch name := expr.(type) {}'.
	tagExprName := ""
	var tagExpr *ast.TypeAssertExpr
	if assign, ok := ts.Assign.(*ast.AssignStmt); ok {
		tagExprName = assign.Lhs[0].(*ast.Ident).Name
		tagExpr = assign.Rhs[0].(*ast.TypeAssertExpr)
	} else {
		tagExpr = ts.Assign.(*ast.ExprStmt).X.(*ast.TypeAssertExpr)
	}

	tag := "" // The evaluated TypeSwitchStmt.Tag

	// Collect the type tests from all case clauses, to keep the following
	// switch statement simple and uniform.
	type typeTest struct {
		pos     token.Pos
		varname string
		code    string
	}
	var tests []typeTest
	var assignments []ast.Stmt
	for _, stmt := range ts.Body.List {
		for _, typ := range stmt.(*ast.CaseClause).List {
			if tag == "" {
				tag = i.nextVarname()
			}
			v := i.nextVarname()
			test := typeTest{typ.Pos(), v, i.strEql(tagExpr, typ)}
			tests = append(tests, test)

			posTyp := gen.reposition(typ)
			def := gen.defineIsType(v, tag, posTyp)
			assignments = append(assignments, def)
		}
	}

	// Now handle the collected type tests in a single switch statement.
	var newClauses []ast.Stmt
	for _, stmt := range ts.Body.List {
		clause := stmt.(*ast.CaseClause)

		var newList []ast.Expr
		var newBody []ast.Stmt

		if tagExprName != "" {
			if tag == "" {
				tag = i.nextVarname()
			}
			if len(clause.List) == 1 && !isNilIdent(clause.List[0]) {
				expr := gen.typeAssertExpr(tag, clause.List[0])
				def := gen.define(tagExprName, expr)
				newBody = append(newBody, def)
			} else {
				def := gen.define(tagExprName, gen.ident(tag))
				newBody = append(newBody, def)
			}

			newBody = append(newBody, gen.use(gen.ident(tagExprName)))
		}
		newBody = append(newBody, clause.Body...)
		i.fixStmtRefs(newBody)

		for range clause.List {
			test := tests[0]
			tests = tests[1:]

			gen := codeGenerator{test.pos}
			ident := gen.ident(test.varname)
			wrapped := i.callCover(ident, test.pos, test.code)
			newList = append(newList, wrapped)
		}

		gen := codeGenerator{clause.Pos()}
		newClauses = append(newClauses, gen.caseClause(newList, newBody))
	}

	if tag == "" {
		return
	}

	var newBody []ast.Stmt
	if ts.Init != nil {
		newBody = append(newBody, ts.Init)
	}
	newBody = append(newBody, gen.define(tag, tagExpr.X))
	newBody = append(newBody, assignments...)
	newBody = append(newBody, gen.switchStmt(nil, gen.block(newClauses)))
	i.fixStmtRefs(newBody)

	i.stmtSubst[ts] = gen.block(newBody)
}

func (i *instrumenter) fixStmtRefs(stmts []ast.Stmt) {
	for si, stmt := range stmts {
		i.stmtRef[stmt] = &stmts[si]
	}
}

// replace replaces each prepared node with the instrumentation code, in
// declaration order.
func (i *instrumenter) replace(n ast.Node) bool {
	switch n := n.(type) {

	case ast.Expr:
		if s := i.exprSubst[n]; s != nil {
			*s.ref = i.callCover(s.expr, s.pos, s.text)
		}

	case ast.Stmt:
		if stmt := i.stmtSubst[n]; stmt != nil {
			*i.stmtRef[n] = stmt
		}
	}

	return true
}

// callCover returns expr surrounded by a function call to GobcoCover and
// remembers the location and text of the expression, for later generating the
// table of coverage points.
//
// The position pos must point to the uninstrumented code that is most closely
// related to the instrumented condition. Especially for switch statements, the
// position may differ from the expression that is wrapped.
func (i *instrumenter) callCover(expr ast.Expr, pos token.Pos, code string) ast.Expr {
	assert(pos.IsValid(), "pos must refer to the code from before instrumentation")

	start := i.fset.Position(pos)
	if !strings.HasSuffix(start.Filename, ".go") {
		// don't instrument generated code, such as yacc parsers
		return expr
	}

	i.conds = append(i.conds, cond{start.String(), code})
	idx := len(i.conds) - 1

	gen := codeGenerator{pos}
	return gen.callGobcoCover(idx, expr, i.typ[expr], i.typePkg)
}

// strEql returns the string representation of (lhs == rhs).
func (i *instrumenter) strEql(lhs ast.Expr, rhs ast.Expr) string {
	// Do not use printer.Fprint here, as that would add unnecessary whitespace
	// after the '==' (due to the position information in the nodes) and would
	// also compress the space inside the operands.

	lp := needsParenthesesForEql(lhs)
	rp := needsParenthesesForEql(rhs)

	opening := map[bool]string{true: "("}
	closing := map[bool]string{true: ")"}

	return fmt.Sprintf("%s%s%s == %s%s%s",
		opening[lp], i.str(lhs), closing[lp],
		opening[rp], i.str(rhs), closing[rp])
}

func needsParenthesesForEql(expr ast.Expr) bool {
	switch expr := expr.(type) {
	case *ast.Ident,
		*ast.BasicLit,
		*ast.CompositeLit,
		*ast.ParenExpr,
		*ast.SelectorExpr,
		*ast.IndexExpr,
		*ast.SliceExpr,
		*ast.TypeAssertExpr,
		*ast.CallExpr,
		*ast.StarExpr,
		*ast.UnaryExpr,
		*ast.ArrayType,
		*ast.StructType,
		*ast.FuncType,
		*ast.InterfaceType,
		*ast.MapType,
		*ast.ChanType:
		return false
	case *ast.BinaryExpr:
		return expr.Op.Precedence() <= token.EQL.Precedence()
	}
	return true
}

func (i *instrumenter) str(expr ast.Expr) string {
	var sb strings.Builder
	ok(printer.Fprint(&sb, i.fset, expr))
	return sb.String()
}

func (i *instrumenter) nextVarname() string {
	varname := fmt.Sprintf("gobco%d", i.varname)
	i.varname++
	return varname
}

func ok(err error) {
	if err != nil {
		panic(err)
	}
}

func assert(cond bool, msg string) {
	if !cond {
		panic(msg)
	}
}
