package main

import (
	"os"
	"path/filepath"
	"testing"
)

// mkwork builds a temp workdir populated with the given relative files
// (contents are irrelevant) and returns its path.
func mkwork(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("# x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestHasTestSuite(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  bool
	}{
		{"readme only repo has no suite", []string{"README.md"}, false},
		{"test_ prefix file", []string{"test_foo.py"}, true},
		{"_test suffix file", []string{"foo_test.py"}, true},
		{"nested test file", []string{"src/bar/test_baz.py"}, true},
		{"test file inside .git is ignored", []string{".git/test_foo.py"}, false},
		{"test file inside __pycache__ is ignored", []string{"__pycache__/test_foo.py"}, false},
		{"pytest.ini implies suite", []string{"pytest.ini"}, true},
		{"pyproject with pytest config", []string{"pyproject.toml"}, true},
		{"tests directory implies suite", []string{"tests/"}, true},
		{"package.json with test script", []string{"package.json"}, true},
		{"factory.json test command is authoritative", []string{"factory.json"}, true},
	}
	// Write meaningful contents for the config-file cases.
	write := map[string]string{
		"pyproject.toml": "[tool.pytest.ini_options]\n",
		"package.json":   `{"scripts": {"test": "jest"}}`,
		"factory.json":   `{"test": "make check"}`,
		"tests/":         "", // directory marker, no file content needed
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tc.files {
				p := filepath.Join(dir, f)
				if f[len(f)-1] == '/' {
					if err := os.MkdirAll(p, 0o755); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				content := "# x\n"
				if c, ok := write[f]; ok {
					content = c
				}
				if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := hasTestSuite(dir); got != tc.want {
				t.Errorf("hasTestSuite() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHasTestSuiteNegativeConfigs(t *testing.T) {
	t.Run("pyproject without pytest config is not a suite", func(t *testing.T) {
		dir := mkwork(t)
		if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"),
			[]byte("[build-system]\nrequires = [\"setuptools\"]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if hasTestSuite(dir) {
			t.Error("hasTestSuite() = true, want false")
		}
	})
	t.Run("package.json without test script is not a suite", func(t *testing.T) {
		dir := mkwork(t)
		if err := os.WriteFile(filepath.Join(dir, "package.json"),
			[]byte(`{"scripts": {"build": "tsc"}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if hasTestSuite(dir) {
			t.Error("hasTestSuite() = true, want false")
		}
	})
	t.Run("setup.cfg with pytest section is a suite", func(t *testing.T) {
		dir := mkwork(t)
		if err := os.WriteFile(filepath.Join(dir, "setup.cfg"),
			[]byte("[tool:pytest]\ntestpaths = tests\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !hasTestSuite(dir) {
			t.Error("hasTestSuite() = false, want true")
		}
	})
}
