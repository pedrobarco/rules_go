// Copyright 2017 The Bazel Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const writeFileMode = 0o666

// instrumentForCoverage runs "go tool cover" on a source file to produce
// a coverage-instrumented version of the file. It also registers the file
// with the coverdata package.
func instrumentForCoverage(
		goenv *env,
		importPath string,
		pkgName string,
		infiles []string,
		coverVar string,
		coverMode string,
		outfiles []string,
		workDir string,
		relCoverPath map[string]string,
		srcPathMapping map[string]string,
	) ([]string, error) {
	// This implementation follows the go toolchain's setup of the pkgcfg file
	// https://github.com/golang/go/blob/go1.24.5/src/cmd/go/internal/work/exec.go#L1954
	pkgcfg := workDir + "pkgcfg.txt"
	covoutputs := workDir + "coveroutfiles.txt"
	odir := filepath.Dir(outfiles[0])
	cv := filepath.Join(odir, "covervars.go")
	outputFiles := append([]string{cv}, outfiles...)

	pcfg := coverPkgConfig{
		PkgPath:   importPath,
		PkgName:   pkgName,
		Granularity: "perblock",
		OutConfig: pkgcfg,
		Local:     false,
	}
	data, err := json.Marshal(pcfg)
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if err := os.WriteFile(pkgcfg, data, writeFileMode); err != nil {
		return nil, err
	}
	var sb strings.Builder
	for i := range outputFiles {
		fmt.Fprintf(&sb, "%s\n", outputFiles[i])
	}
	if err := os.WriteFile(covoutputs, []byte(sb.String()), writeFileMode); err != nil {
		return nil, err
	}

	goargs := goenv.goTool("cover", "-pkgcfg", pkgcfg, "-var", coverVar, "-mode", coverMode, "-outfilelist", covoutputs)
	goargs = append(goargs, infiles...)
	if err := goenv.runCommand(goargs); err != nil {
		return nil, err
	}

	for i, outfile := range outfiles {
		srcName := relCoverPath[infiles[i]]
		importPathFile := srcPathMapping[srcName]
		// Augment coverage source files to store a mapping of <importpath>/<filename> -> <execroot_relative_path>
		// as this information is only known during compilation but is required when the rules_go generated
		// test main exits and go coverage files are converted to lcov format.
		if err := registerCoverage(outfile, importPathFile, srcName); err != nil {
			return nil, err
		}
	}
	return outputFiles, nil
}

// instrumentForBranchCoverage rewrites the given Go source files for branch
// (decision) coverage using the vendored gobco instrumenter (see
// gobco_instrumenter.go), writing the instrumented sources to outfiles. It also
// writes a per-package runtime file into the same directory as outfiles[0]
// (defining GobcoCover and the package's condition table) and returns its path,
// so the caller can add it to the package's sources.
//
// This path is gated behind the experimental_branch_coverage build setting and
// currently runs as an alternative to "go tool cover" statement instrumentation
// (which cannot emit branch coverage). Collecting the counts and converting them
// to LCOV BRDA/BRF/BRH records is handled in a later phase.
func instrumentForBranchCoverage(infiles, outfiles []string) (string, error) {
	if len(infiles) != len(outfiles) {
		return "", fmt.Errorf("instrumentForBranchCoverage: %d input files but %d output files", len(infiles), len(outfiles))
	}

	fset := token.NewFileSet()
	parsed := make([]*ast.File, len(infiles))
	for idx, in := range infiles {
		src, err := os.ReadFile(in)
		if err != nil {
			return "", fmt.Errorf("instrumentForBranchCoverage: reading source: %w", err)
		}
		f, err := parser.ParseFile(fset, in, src, parser.ParseComments)
		if err != nil {
			return "", fmt.Errorf("instrumentForBranchCoverage: parsing source: %w", err)
		}
		parsed[idx] = f
	}

	// Resolve types once over the original (uninstrumented) files, before any
	// rewriting invalidates the expression nodes used as map keys.
	inst := newBranchInstrumenter(fset)
	inst.resolveTypes(parsed)

	pkgName := parsed[0].Name.Name
	for idx, f := range parsed {
		inst.instrumentFileNode(f)
		var out bytes.Buffer
		if err := printer.Fprint(&out, fset, f); err != nil {
			return "", fmt.Errorf("instrumentForBranchCoverage: formatting instrumented source: %w", err)
		}
		if err := os.WriteFile(outfiles[idx], out.Bytes(), writeFileMode); err != nil {
			return "", fmt.Errorf("instrumentForBranchCoverage: writing instrumented source: %w", err)
		}
	}

	runtimeFile := filepath.Join(filepath.Dir(outfiles[0]), "gobco_runtime.go")
	runtime := generateBranchRuntime(pkgName, inst.conds)
	if err := os.WriteFile(runtimeFile, []byte(runtime), writeFileMode); err != nil {
		return "", fmt.Errorf("instrumentForBranchCoverage: writing runtime: %w", err)
	}
	return runtimeFile, nil
}

// generateBranchRuntime returns the source of a self-contained per-package
// runtime file that defines GobcoCover and the package's condition table.
//
// TODO(branch-coverage): replace this self-contained runtime with registration
// against a shared //go/tools/branchcoverdata package so that branch counts from
// all packages linked into a single test binary can be collected and converted
// to LCOV.
func generateBranchRuntime(pkgName string, conds []cond) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "package %s\n\n", pkgName)
	sb.WriteString("type gobcoCond struct {\n")
	sb.WriteString("\tStart      string\n")
	sb.WriteString("\tCode       string\n")
	sb.WriteString("\tTrueCount  int\n")
	sb.WriteString("\tFalseCount int\n")
	sb.WriteString("}\n\n")
	sb.WriteString("var gobcoCounts = []gobcoCond{\n")
	for _, c := range conds {
		fmt.Fprintf(&sb, "\t{%q, %q, 0, 0},\n", c.pos, c.text)
	}
	sb.WriteString("}\n\n")
	sb.WriteString("func GobcoCover(idx int, cond bool) bool {\n")
	sb.WriteString("\tif cond {\n")
	sb.WriteString("\t\tgobcoCounts[idx].TrueCount++\n")
	sb.WriteString("\t} else {\n")
	sb.WriteString("\t\tgobcoCounts[idx].FalseCount++\n")
	sb.WriteString("\t}\n")
	sb.WriteString("\treturn cond\n")
	sb.WriteString("}\n")
	return sb.String()
}

// coverPkgConfig matches https://cs.opensource.google/go/go/+/refs/tags/go1.24.4:src/cmd/internal/cov/covcmd/cmddefs.go;l=18
type coverPkgConfig struct {
	// File into which cmd/cover should emit summary info
	// when instrumentation is complete.
	OutConfig string

	// Import path for the package being instrumented.
	PkgPath string

	// Package name.
	PkgName string

	// Instrumentation granularity: one of "perfunc" or "perblock" (default)
	Granularity string

	// Module path for this package (empty if no go.mod in use)
	ModulePath string

	// Local mode indicates we're doing a coverage build or test of a
	// package selected via local import path, e.g. "./..." or
	// "./foo/bar" as opposed to a non-relative import path. See the
	// corresponding field in cmd/go's PackageInternal struct for more
	// info.
	Local bool

	// EmitMetaFile if non-empty is the path to which the cover tool should
	// directly emit a coverage meta-data file for the package, if the
	// package has any functions in it. The go command will pass in a value
	// here if we've been asked to run "go test -cover" on a package that
	// doesn't have any *_test.go files.
	EmitMetaFile string
}

// registerCoverage modifies coverSrcFilename, the output file from go tool cover.
// It adds a call to coverdata.RegisterSrcPathMapping, which ensures that rules_go
// can produce lcov files with exec root relative file paths.
func registerCoverage(coverSrcFilename, importPathFile, srcName string) error {
	coverSrc, err := os.ReadFile(coverSrcFilename)
	if err != nil {
		return fmt.Errorf("instrumentForCoverage: reading instrumented source: %w", err)
	}

	// Parse the file.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, coverSrcFilename, coverSrc, parser.ParseComments)
	if err != nil {
		return nil // parse error: proceed and let the compiler fail
	}

	// Perform edits using a byte buffer instead of the AST, because
	// we can not use go/format to write the AST back out without
	// changing line numbers.
	editor := NewBuffer(coverSrc)

	// Ensure coverdata is imported. Use an existing import if present
	// or add a new one.
	const coverdataPath = "github.com/bazelbuild/rules_go/go/tools/coverdata"
	var coverdataName string
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return nil // parse error: proceed and let the compiler fail
		}
		if path == coverdataPath {
			if imp.Name != nil {
				// renaming import
				if imp.Name.Name == "_" {
					// Change blank import to named import
					editor.Replace(
						fset.Position(imp.Name.Pos()).Offset,
						fset.Position(imp.Name.End()).Offset,
						"coverdata")
					coverdataName = "coverdata"
				} else {
					coverdataName = imp.Name.Name
				}
			} else {
				// default import
				coverdataName = "coverdata"
			}
			break
		}
	}
	if coverdataName == "" {
		// No existing import. Add a new one.
		coverdataName = "coverdata"
		editor.Insert(fset.Position(f.Name.End()).Offset, fmt.Sprintf("; import %q", coverdataPath))
	}

	// Append an init function.
	var buf = bytes.NewBuffer(editor.Bytes())
	fmt.Fprintf(buf, `
func init() {
	%s.RegisterSrcPathMapping(%q, %q)
}
`, coverdataName, importPathFile, srcName)
	if err := os.WriteFile(coverSrcFilename, buf.Bytes(), writeFileMode); err != nil {
		return fmt.Errorf("registerCoverage: %v", err)
	}
	return nil
}
