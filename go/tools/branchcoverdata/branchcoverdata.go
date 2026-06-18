/* Copyright 2026 The Bazel Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package branchcoverdata provides a registration function for packages
// instrumented for branch (decision) coverage by the rules_go builder. It is
// the branch-coverage analogue of go/tools/coverdata: every instrumented
// package registers its condition table and live counter slice via an injected
// init function, so that branch counts from all packages linked into a single
// test binary can be collected at exit.
//
// This package is part of the Bazel Go rules, and its interface should not be
// considered public. It may change without notice.
package branchcoverdata

import "sync"

// PkgConds holds the branch (decision) coverage data for a single instrumented
// package. Pos and Code are parallel, one entry per instrumented condition;
// Pos[i] is the source position ("file.go:line:col") and Code[i] the textual
// form of condition i. Counts is held by reference from the instrumented
// package and is updated in place: Counts[2*i] is the number of times condition
// i evaluated true and Counts[2*i+1] the number of times it evaluated false.
type PkgConds struct {
	Pos    []string
	Code   []string
	Counts []uint32
}

var (
	mu sync.Mutex

	// Conditions maps a package import path to its branch coverage data.
	Conditions = make(map[string]*PkgConds)
)

// RegisterCond records the branch (decision) coverage condition table for the
// package identified by importPath. counts is retained by reference; the
// instrumented package keeps updating it as conditions are evaluated. This
// should be called from an init function in an instrumented package. The first
// registration for a given importPath wins; later ones are ignored.
func RegisterCond(importPath string, pos, code []string, counts []uint32) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := Conditions[importPath]; ok {
		return
	}
	Conditions[importPath] = &PkgConds{Pos: pos, Code: code, Counts: counts}
}
