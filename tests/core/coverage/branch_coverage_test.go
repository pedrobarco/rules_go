// Copyright 2024 The Bazel Authors. All rights reserved.
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

package branch_coverage_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/tools/bazel_testing"
)

func TestMain(m *testing.M) {
	bazel_testing.TestMain(m, bazel_testing.Args{
		Main: `
-- src/BUILD.bazel --
load("@io_bazel_rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "lib",
    srcs = ["lib.go"],
    importpath = "example.com/lib",
)

go_test(
    name = "lib_test",
    srcs = ["lib_test.go"],
    embed = [":lib"],
)
-- src/lib.go --
package lib

func Classify(n int) string {
	if n > 0 {
		return "positive"
	}
	return "non-positive"
}

func Unused(n int) string {
	if n > 0 {
		return "x"
	}
	return "y"
}
-- src/lib_test.go --
package lib

import "testing"

func TestClassify(t *testing.T) {
	if got := Classify(1); got != "positive" {
		t.Errorf("Classify(1) = %q", got)
	}
	if got := Classify(-1); got != "non-positive" {
		t.Errorf("Classify(-1) = %q", got)
	}
}
`,
	})
}

// TestBranchCoverage verifies that, with the experimental_branch_coverage flag,
// the per-test LCOV report carries branch (BRDA/BRF/BRH) records instead of
// line (DA) records: the decision exercised both ways is hit, and the decision
// in the never-called function reports both outcomes as not taken ("-").
func TestBranchCoverage(t *testing.T) {
	if err := bazel_testing.RunBazel(
		"coverage",
		"--@io_bazel_rules_go//go/config:experimental_branch_coverage",
		"//src:lib_test",
	); err != nil {
		t.Fatal(err)
	}

	path := filepath.FromSlash("bazel-testlogs/src/lib_test/coverage.dat")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)

	for _, want := range []string{
		"SF:src/lib.go\n",
		// Classify's "if n > 0" (line 4): both outcomes exercised by the test.
		"BRDA:4,0,0,1\n",
		"BRDA:4,0,1,1\n",
		// Unused's "if n > 0" (line 11): never evaluated.
		"BRDA:11,1,0,-\n",
		"BRDA:11,1,1,-\n",
		"BRF:4\n",
		"BRH:2\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("coverage.dat does not contain %q\nactual content:\n\n%s", want, got)
		}
	}

	// Branch-only mode replaces "go tool cover", so there are no line records.
	if strings.Contains(got, "\nDA:") {
		t.Errorf("coverage.dat unexpectedly contains line (DA:) records:\n\n%s", got)
	}
}
