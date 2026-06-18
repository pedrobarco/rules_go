// This file is adapted from rillig/gobco (https://github.com/rillig/gobco),
// which is distributed under the following license:
//
//	BSD 2-Clause License
//
//	Copyright (c) 2024, Roland Illig
//
//	Redistribution and use in source and binary forms, with or without
//	modification, are permitted provided that the following conditions are met:
//
//	1. Redistributions of source code must retain the above copyright notice,
//	   this list of conditions and the following disclaimer.
//
//	2. Redistributions in binary form must reproduce the above copyright
//	   notice, this list of conditions and the following disclaimer in the
//	   documentation and/or other materials provided with the distribution.
//
//	THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
//	AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
//	IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
//	ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE
//	LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
//	CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
//	SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
//	INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
//	CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
//	ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
//	POSSIBILITY OF SUCH DAMAGE.
//
// Only the subset of gobco required for source-to-source branch (decision)
// coverage instrumentation has been vendored, and it has been adapted to run
// as part of the rules_go GoCompilePkg builder.

package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
)

// codeGenerator generates source code with correct position information.
// If the code were generated with [token.NoPos] instead, the comments would be
// moved to incorrect locations.
type codeGenerator struct {
	pos token.Pos
}

func (gen codeGenerator) ident(name string) *ast.Ident {
	return &ast.Ident{
		NamePos: gen.pos,
		Name:    name,
	}
}

func (gen codeGenerator) eql(x string, y ast.Expr) *ast.BinaryExpr {
	return &ast.BinaryExpr{
		X:     gen.ident(x),
		OpPos: gen.pos,
		Op:    token.EQL,
		Y:     y,
	}
}

func (gen codeGenerator) typeAssertExpr(x string, typ ast.Expr) ast.Expr {
	return &ast.TypeAssertExpr{
		X:      gen.ident(x),
		Lparen: gen.pos,
		Type:   typ,
		Rparen: gen.pos,
	}
}

func (gen codeGenerator) callGobcoCover(idx int, cond ast.Expr, typ types.Type, typePkg *types.Package) ast.Expr {
	convert := typ != nil && !types.Identical(typ, typ.Underlying())
	if convert {
		cond = gen.convert(cond, "bool")
	}
	var ret ast.Expr = &ast.CallExpr{
		Fun:    gen.ident("GobcoCover"),
		Lparen: gen.pos,
		Args: []ast.Expr{
			&ast.BasicLit{
				ValuePos: gen.pos,
				Kind:     token.INT,
				Value:    fmt.Sprint(idx),
			},
			cond,
		},
		Rparen: gen.pos,
	}
	if convert {
		typename := types.TypeString(typ, types.RelativeTo(typePkg))
		ret = gen.convert(ret, typename)
	}
	return ret
}

func (gen codeGenerator) convert(x ast.Expr, t string) ast.Expr {
	return &ast.CallExpr{
		Fun:  gen.ident(t),
		Args: []ast.Expr{x},
	}
}

func (gen codeGenerator) define(lhs string, rhs ast.Expr) *ast.AssignStmt {
	return gen.defineExprs(lhs, []ast.Expr{rhs})
}

func (gen codeGenerator) defineExprs(lhs string, rhs []ast.Expr) *ast.AssignStmt {
	return &ast.AssignStmt{
		Lhs:    []ast.Expr{gen.ident(lhs)},
		TokPos: gen.pos,
		Tok:    token.DEFINE,
		Rhs:    rhs,
	}
}

// defineIsType assigns to lhs whether rhs has the given type.
func (gen codeGenerator) defineIsType(lhs string, rhs string, typ ast.Expr) ast.Stmt {
	if isNilIdent(typ) {
		return gen.define(lhs, gen.eql(rhs, gen.ident("nil")))
	}
	return &ast.AssignStmt{
		Lhs:    []ast.Expr{gen.ident("_"), gen.ident(lhs)},
		TokPos: gen.pos,
		Tok:    token.DEFINE,
		Rhs: []ast.Expr{
			&ast.TypeAssertExpr{
				X:      gen.ident(rhs),
				Lparen: gen.pos,
				Type:   typ,
				Rparen: gen.pos,
			},
		},
	}
}

func (gen codeGenerator) use(rhs ast.Expr) *ast.AssignStmt {
	return &ast.AssignStmt{
		Lhs:    []ast.Expr{gen.ident("_")},
		TokPos: gen.pos,
		Tok:    token.ASSIGN,
		Rhs:    []ast.Expr{rhs},
	}
}

func (gen codeGenerator) block(stmts []ast.Stmt) *ast.BlockStmt {
	return &ast.BlockStmt{
		Lbrace: gen.pos,
		List:   stmts,
		Rbrace: gen.pos,
	}
}

func (gen codeGenerator) switchStmt(init ast.Stmt, body *ast.BlockStmt) *ast.SwitchStmt {
	return &ast.SwitchStmt{
		Switch: gen.pos,
		Init:   init,
		Tag:    nil,
		Body:   body,
	}
}

func (gen codeGenerator) caseClause(list []ast.Expr, body []ast.Stmt) *ast.CaseClause {
	return &ast.CaseClause{
		Case:  gen.pos,
		List:  list,
		Colon: gen.pos,
		Body:  body,
	}
}

// reposition returns a deep copy of e in which all token positions have been
// replaced with the code generator's position.
func (gen codeGenerator) reposition(e ast.Expr) ast.Expr {
	return subst(reflect.ValueOf(e), gen.reset).Interface().(ast.Expr)
}

func (gen codeGenerator) reset(x reflect.Value) reflect.Value {
	switch x.Interface().(type) {
	case *ast.Object, *ast.Scope:
		return reflect.Zero(x.Type())
	case token.Pos:
		return reflect.ValueOf(gen.pos)
	}
	return x
}

func subst(
	rx reflect.Value,
	pre func(reflect.Value) reflect.Value,
) reflect.Value {
	x := pre(rx)
	switch x.Kind() {

	case reflect.Interface:
		lv := reflect.New(x.Type()).Elem()
		if rv := x.Elem(); rv.IsValid() {
			lv.Set(subst(rv, pre))
		}
		return lv

	case reflect.Ptr:
		lv := reflect.New(x.Type()).Elem()
		if rv := x.Elem(); rv.IsValid() {
			lv.Set((subst(rv, pre)).Addr())
		}
		return lv

	case reflect.Slice:
		if x.IsNil() {
			return reflect.Zero(x.Type())
		}
		c := reflect.MakeSlice(x.Type(), x.Len(), x.Cap())
		for i := 0; i < x.Len(); i++ {
			c.Index(i).Set(subst(x.Index(i), pre))
		}
		return c

	case reflect.Struct:
		c := reflect.New(x.Type()).Elem()
		for i := 0; i < x.NumField(); i++ {
			c.Field(i).Set(subst(x.Field(i), pre))
		}
		return c

	default:
		// Assume that all other types can be copied trivially.
		c := reflect.New(x.Type()).Elem()
		c.Set(x)
		return c
	}
}

func isNilIdent(e ast.Expr) bool {
again:
	if p, ok := e.(*ast.ParenExpr); ok {
		e = p.X
		goto again
	}
	ident, ok := e.(*ast.Ident)
	return ok && ident.Name == "nil"
}
