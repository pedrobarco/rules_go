// Copyright 2022 The Bazel Authors. All rights reserved.
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

package bzltestutil

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/bazelbuild/rules_go/go/tools/branchcoverdata"
	"github.com/bazelbuild/rules_go/go/tools/coverdata"
)

// Lock in the COVERAGE_DIR during test setup in case the test uses e.g. os.Clearenv.
var coverageDir = os.Getenv("COVERAGE_DIR")

// Also lock in the test flag set in case test overwrites it.
var testFlags = flag.CommandLine

// ConvertCoverToLcov converts the go coverprofile file coverage.dat.cover to
// the expectedLcov format and stores it in coverage.dat, where it is picked up by
// Bazel.
// The conversion emits line and branch coverage, but not function coverage.
func ConvertCoverToLcov() error {
	inPath := testFlags.Lookup("test.coverprofile").Value.String()
	in, err := os.Open(inPath)
	if err != nil {
		if len(branchcoverdata.Conditions) == 0 {
			// This can happen if there are no tests and should not be an error.
			log.Printf("Not collecting coverage: %s has not been created: %s", inPath, err)
			return nil
		}
		// There is branch coverage data to emit even though no statement
		// coverage profile was produced (e.g. in branch coverage mode).
		return ConvertCoverFromReaderToLcov(strings.NewReader(""))
	}
	defer in.Close()

	return ConvertCoverFromReaderToLcov(in)
}

func ConvertCoverFromReaderToLcov(in io.Reader) error {
	if coverageDir == "" {
		log.Printf("Not collecting coverage: COVERAGE_DIR is not set")
		return nil
	}
	// All *.dat files in $COVERAGE_DIR will be merged by Bazel's lcov_merger tool.
	out, err := os.CreateTemp(coverageDir, "go_coverage.*.dat")
	if err != nil {
		return err
	}
	defer out.Close()

	return convertCoverToLcov(in, out)
}

var _coverLinePattern = regexp.MustCompile(`^(?P<path>.+):(?P<startLine>\d+)\.(?P<startColumn>\d+),(?P<endLine>\d+)\.(?P<endColumn>\d+) (?P<numStmt>\d+) (?P<count>\d+)$`)

const (
	_pathIdx      = 1
	_startLineIdx = 2
	_endLineIdx   = 4
	_countIdx     = 7
)

func convertCoverToLcov(coverReader io.Reader, lcovWriter io.Writer) error {
	// Branch (decision) coverage is collected separately from the statement
	// coverage profile, via the branchcoverdata registry that instrumented
	// packages populate at runtime. It is grouped by source file name so it can
	// be merged into the per-file LCOV records below.
	branches, err := collectBranchData()
	if err != nil {
		return err
	}
	cover := bufio.NewScanner(coverReader)
	lcov := bufio.NewWriter(lcovWriter)
	defer lcov.Flush()
	currentPath := ""
	var lineCounts map[uint32]uint32
	for cover.Scan() {
		l := cover.Text()
		m := _coverLinePattern.FindStringSubmatch(l)
		if m == nil {
			if strings.HasPrefix(l, "mode: ") {
				continue
			}
			return fmt.Errorf("invalid go cover line: %s", l)
		}

		if m[_pathIdx] != currentPath {
			if currentPath != "" {
				if err := emitLcovLines(lcov, currentPath, lineCounts, branches); err != nil {
					return err
				}
			}
			currentPath = m[_pathIdx]
			lineCounts = make(map[uint32]uint32)
		}

		startLine, err := strconv.ParseUint(m[_startLineIdx], 10, 32)
		if err != nil {
			return err
		}
		endLine, err := strconv.ParseUint(m[_endLineIdx], 10, 32)
		if err != nil {
			return err
		}
		count, err := strconv.ParseUint(m[_countIdx], 10, 32)
		if err != nil {
			return err
		}
		for line := uint32(startLine); line <= uint32(endLine); line++ {
			prevCount, ok := lineCounts[line]
			if !ok || uint32(count) > prevCount {
				lineCounts[line] = uint32(count)
			}
		}
	}
	if currentPath != "" {
		if err := emitLcovLines(lcov, currentPath, lineCounts, branches); err != nil {
			return err
		}
	}
	// Emit branch-only records for source files that had branch coverage data
	// but no statement coverage (e.g. in branch coverage mode).
	return emitRemainingBranches(lcov, branches)
}

func emitLcovLines(lcov io.StringWriter, path string, lineCounts map[uint32]uint32, branches map[string]*fileBranches) error {
	srcName, ok := coverdata.SrcPathMapping[path]
	if !ok {
		srcName = path
	}
	_, err := lcov.WriteString(fmt.Sprintf("SF:%s\n", srcName))
	if err != nil {
		return err
	}

	// Emit branch (decision) coverage for this source file, if any, before the
	// line counters, and remove it so it is not emitted again as a branch-only
	// record.
	if fb, ok := branches[srcName]; ok {
		if err := emitLcovBranches(lcov, fb); err != nil {
			return err
		}
		delete(branches, srcName)
	}

	// Emit the coverage counters for the individual source lines.
	sortedLines := make([]uint32, 0, len(lineCounts))
	for line := range lineCounts {
		sortedLines = append(sortedLines, line)
	}
	sort.Slice(sortedLines, func(i, j int) bool { return sortedLines[i] < sortedLines[j] })
	numCovered := 0
	for _, line := range sortedLines {
		count := lineCounts[line]
		if count > 0 {
			numCovered++
		}
		_, err := lcov.WriteString(fmt.Sprintf("DA:%d,%d\n", line, count))
		if err != nil {
			return err
		}
	}
	// Emit a summary containing the number of all/covered lines and end the info for the current source file.
	_, err = lcov.WriteString(fmt.Sprintf("LH:%d\nLF:%d\nend_of_record\n", numCovered, len(sortedLines)))
	if err != nil {
		return err
	}
	return nil
}

// branchCond is the branch (decision) coverage for a single instrumented
// condition: how many times it evaluated true and false.
type branchCond struct {
	line       uint32
	trueCount  uint32
	falseCount uint32
}

// fileBranches holds the branch conditions of a single source file, in the
// order they were registered (which follows source order).
type fileBranches struct {
	conds []branchCond
}

// collectBranchData reads the global branch coverage registry and groups the
// conditions by source file name. The file name is the exec-root-relative path
// recorded in each condition's position by the builder, so it matches the SF:
// records emitted for line coverage.
func collectBranchData() (map[string]*fileBranches, error) {
	result := make(map[string]*fileBranches)
	for _, pkg := range branchcoverdata.Conditions {
		for i, pos := range pkg.Pos {
			file, line, ok := parseBranchPos(pos)
			if !ok {
				return nil, fmt.Errorf("invalid branch coverage position: %q", pos)
			}
			fb := result[file]
			if fb == nil {
				fb = &fileBranches{}
				result[file] = fb
			}
			var trueCount, falseCount uint32
			if 2*i < len(pkg.Counts) {
				trueCount = pkg.Counts[2*i]
			}
			if 2*i+1 < len(pkg.Counts) {
				falseCount = pkg.Counts[2*i+1]
			}
			fb.conds = append(fb.conds, branchCond{line: line, trueCount: trueCount, falseCount: falseCount})
		}
	}
	return result, nil
}

// parseBranchPos splits a "file:line:col" position into its file and line
// components. The last two colons delimit the line and column; Bazel source
// paths do not contain colons, so anything before them is the file name.
func parseBranchPos(pos string) (file string, line uint32, ok bool) {
	lastColon := strings.LastIndex(pos, ":")
	if lastColon < 0 {
		return "", 0, false
	}
	secondColon := strings.LastIndex(pos[:lastColon], ":")
	if secondColon < 0 {
		return "", 0, false
	}
	n, err := strconv.ParseUint(pos[secondColon+1:lastColon], 10, 32)
	if err != nil {
		return "", 0, false
	}
	return pos[:secondColon], uint32(n), true
}

// emitLcovBranches writes the BRDA/BRF/BRH records for a single source file.
// Each condition contributes two branches: branch 0 is the false outcome and
// branch 1 the true outcome. The block number is the condition's index, so
// multiple conditions on the same line remain distinct. A branch whose
// enclosing condition was never evaluated is reported as not taken ("-").
func emitLcovBranches(lcov io.StringWriter, fb *fileBranches) error {
	found := 0
	hit := 0
	for block, c := range fb.conds {
		falseTaken, trueTaken := "-", "-"
		if c.trueCount+c.falseCount > 0 {
			falseTaken = strconv.FormatUint(uint64(c.falseCount), 10)
			trueTaken = strconv.FormatUint(uint64(c.trueCount), 10)
		}
		if _, err := lcov.WriteString(fmt.Sprintf("BRDA:%d,%d,0,%s\n", c.line, block, falseTaken)); err != nil {
			return err
		}
		if _, err := lcov.WriteString(fmt.Sprintf("BRDA:%d,%d,1,%s\n", c.line, block, trueTaken)); err != nil {
			return err
		}
		found += 2
		if c.falseCount > 0 {
			hit++
		}
		if c.trueCount > 0 {
			hit++
		}
	}
	_, err := lcov.WriteString(fmt.Sprintf("BRF:%d\nBRH:%d\n", found, hit))
	return err
}

// emitRemainingBranches writes SF records carrying only branch coverage for
// source files that had no statement coverage data (and were therefore not
// already emitted by emitLcovLines).
func emitRemainingBranches(lcov io.StringWriter, branches map[string]*fileBranches) error {
	if len(branches) == 0 {
		return nil
	}
	names := make([]string, 0, len(branches))
	for name := range branches {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := lcov.WriteString(fmt.Sprintf("SF:%s\n", name)); err != nil {
			return err
		}
		if err := emitLcovBranches(lcov, branches[name]); err != nil {
			return err
		}
		if _, err := lcov.WriteString("end_of_record\n"); err != nil {
			return err
		}
	}
	return nil
}
