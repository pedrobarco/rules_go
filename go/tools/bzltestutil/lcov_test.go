package bzltestutil

import (
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/tools/branchcoverdata"
)

func TestConvertCoverToLcov(t *testing.T) {
	var tests = []struct {
		name         string
		goCover      string
		expectedLcov string
	}{
		{
			"empty",
			"",
			"",
		},
		{
			"mode only",
			"mode: atomic\n",
			"",
		},
		{
			"single file",
			`mode: count
file.go:0.4,2.10 0 0
`,
			`SF:file.go
DA:0,0
DA:1,0
DA:2,0
LH:0
LF:3
end_of_record
`,
		},
		{
			"narrow ranges",
			`mode: atomic
path/to/pkg/file.go:0.1,0.2 5 1
path/to/pkg/file2.go:1.2,1.2 4 2
`,
			`SF:path/to/pkg/file.go
DA:0,1
LH:1
LF:1
end_of_record
SF:path/to/pkg/file2.go
DA:1,2
LH:1
LF:1
end_of_record
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := strings.NewReader(tt.goCover)
			var out strings.Builder
			err := convertCoverToLcov(in, &out)
			if err != nil {
				t.Errorf("convertCoverToLcov returned unexpected error: %+v", err)
			}
			actualLcov := out.String()
			if actualLcov != tt.expectedLcov {
				t.Errorf("covertCoverToLcov returned:\n%q\n, expected:\n%q\n", actualLcov, tt.expectedLcov)
			}
		})
	}
}

func TestConvertCoverToLcovWithBranches(t *testing.T) {
	old := branchcoverdata.Conditions
	defer func() { branchcoverdata.Conditions = old }()
	branchcoverdata.Conditions = map[string]*branchcoverdata.PkgConds{
		"example.com/pkg": {
			Pos:  []string{"src/lib.go:10:5", "src/lib.go:20:8"},
			Code: []string{`name == ""`, "n > 0"},
			// cond 0: true=3, false=0; cond 1: never evaluated.
			Counts: []uint32{3, 0, 0, 0},
		},
	}

	const expected = `SF:src/lib.go
BRDA:10,0,0,0
BRDA:10,0,1,3
BRDA:20,1,0,-
BRDA:20,1,1,-
BRF:4
BRH:1
end_of_record
`

	var out strings.Builder
	if err := convertCoverToLcov(strings.NewReader(""), &out); err != nil {
		t.Fatalf("convertCoverToLcov returned unexpected error: %+v", err)
	}
	if got := out.String(); got != expected {
		t.Errorf("convertCoverToLcov returned:\n%q\n, expected:\n%q\n", got, expected)
	}
}
