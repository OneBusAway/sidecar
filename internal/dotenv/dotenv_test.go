package dotenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeEnvFile writes contents to a .env file in a fresh temp directory and
// returns its path.
func writeEnvFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// clearEnv unsets key now and restores its original state when the test
// ends. Tests in this package mutate real process environment (Load's whole
// job is os.Setenv), so none of them run in parallel, and every key a test
// touches goes through here first so a leaked value can't couple two tests.
func clearEnv(t *testing.T, key string) {
	t.Helper()
	orig, wasSet := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unsetting %s: %v", key, err)
	}
	t.Cleanup(func() {
		if wasSet {
			_ = os.Setenv(key, orig)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func TestLoad_SetsVariableFromFile(t *testing.T) {
	clearEnv(t, "DOTENV_TEST_BASIC")
	path := writeEnvFile(t, "DOTENV_TEST_BASIC=hello\n")

	if err := Load(path); err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	if got := os.Getenv("DOTENV_TEST_BASIC"); got != "hello" {
		t.Errorf("DOTENV_TEST_BASIC = %q, want %q", got, "hello")
	}
}

// TestLoad_RealEnvironmentWins pins the precedence rule the whole design
// hangs on: a variable already present in the process environment -- even
// one set to the empty string -- is never overwritten by the file. This is
// what keeps a production deployment (env from the platform) unaffected by a
// stray .env on disk.
func TestLoad_RealEnvironmentWins(t *testing.T) {
	t.Run("set variable is not overwritten", func(t *testing.T) {
		clearEnv(t, "DOTENV_TEST_PRESET")
		if err := os.Setenv("DOTENV_TEST_PRESET", "from-env"); err != nil {
			t.Fatal(err)
		}
		path := writeEnvFile(t, "DOTENV_TEST_PRESET=from-file\n")

		if err := Load(path); err != nil {
			t.Fatalf("Load() = %v, want nil", err)
		}
		if got := os.Getenv("DOTENV_TEST_PRESET"); got != "from-env" {
			t.Errorf("DOTENV_TEST_PRESET = %q, want %q (real environment must win)", got, "from-env")
		}
	})

	t.Run("empty-but-set variable still wins", func(t *testing.T) {
		clearEnv(t, "DOTENV_TEST_EMPTYSET")
		if err := os.Setenv("DOTENV_TEST_EMPTYSET", ""); err != nil {
			t.Fatal(err)
		}
		path := writeEnvFile(t, "DOTENV_TEST_EMPTYSET=from-file\n")

		if err := Load(path); err != nil {
			t.Fatalf("Load() = %v, want nil", err)
		}
		if got := os.Getenv("DOTENV_TEST_EMPTYSET"); got != "" {
			t.Errorf("DOTENV_TEST_EMPTYSET = %q, want empty (set-but-empty counts as set)", got)
		}
	})
}

func TestLoad_MissingFileIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := Load(path); err != nil {
		t.Errorf("Load(missing file) = %v, want nil", err)
	}
}

func TestLoad_SkipsBlankLinesAndComments(t *testing.T) {
	clearEnv(t, "DOTENV_TEST_AFTER_NOISE")
	path := writeEnvFile(t, "\n   \n# a comment\n  # indented comment\nDOTENV_TEST_AFTER_NOISE=ok\n")

	if err := Load(path); err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	if got := os.Getenv("DOTENV_TEST_AFTER_NOISE"); got != "ok" {
		t.Errorf("DOTENV_TEST_AFTER_NOISE = %q, want %q", got, "ok")
	}
}

func TestLoad_ValueHandling(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{"double quotes stripped", `DOTENV_TEST_VAL="quoted value"`, "quoted value"},
		{"single quotes stripped", `DOTENV_TEST_VAL='quoted value'`, "quoted value"},
		{"whitespace around key and value trimmed", "  DOTENV_TEST_VAL  =  spaced  ", "spaced"},
		{"equals sign inside value kept", "DOTENV_TEST_VAL=a=b=c", "a=b=c"},
		{"hash inside quoted value kept", `DOTENV_TEST_VAL="key#with#hashes"`, "key#with#hashes"},
		{"empty value sets empty string", "DOTENV_TEST_VAL=", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t, "DOTENV_TEST_VAL")
			path := writeEnvFile(t, tt.line+"\n")

			if err := Load(path); err != nil {
				t.Fatalf("Load() = %v, want nil", err)
			}
			got, ok := os.LookupEnv("DOTENV_TEST_VAL")
			if !ok {
				t.Fatal("DOTENV_TEST_VAL not set, want it set")
			}
			if got != tt.want {
				t.Errorf("DOTENV_TEST_VAL = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestLoad_MalformedFileErrors pins the "error loudly" half of the design:
// anything outside the deliberately minimal supported syntax is a startup
// error, not a silently skipped line, and the error names the line number
// but never the line's content -- a .env line is exactly where a secret
// lives, and Load's errors flow straight into the boot log.
func TestLoad_MalformedFileErrors(t *testing.T) {
	// s3cr3t-* values are canaries: distinctive tokens that could never
	// occur in an error's own wording, so a match proves file content
	// leaked into the error.
	tests := []struct {
		name     string
		contents string
		wantLine string // substring identifying the offending line number
	}{
		{"line without equals", "DOTENV_TEST_SECRET_A=s3cr3t-a\nnot a pair\n", "line 2"},
		{"empty key", "=s3cr3t-b\n", "line 1"},
		{"key with internal space", "BAD KEY=s3cr3t-c\n", "line 1"},
		{"unterminated double quote", `DOTENV_TEST_B="s3cr3t-d` + "\n", "line 1"},
		{"unterminated single quote", "DOTENV_TEST_B='s3cr3t-e\n", "line 1"},
		{"duplicate key", "DOTENV_TEST_DUP=s3cr3t-f\nDOTENV_TEST_DUP=s3cr3t-g\n", "line 2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t, "DOTENV_TEST_SECRET_A")
			path := writeEnvFile(t, tt.contents)

			err := Load(path)
			if err == nil {
				t.Fatal("Load() = nil, want an error for a malformed file")
			}
			if !strings.Contains(err.Error(), tt.wantLine) {
				t.Errorf("error %q does not name the offending line (%s)", err, tt.wantLine)
			}
			if strings.Contains(err.Error(), "s3cr3t") {
				t.Errorf("error %q leaks file content; errors must carry line numbers only", err)
			}
		})
	}
}

// TestLoad_MalformedFileAppliesNothing pins validate-then-apply: a file with
// an error on line 2 must not have already exported line 1 by the time Load
// returns. Half-applied configuration is worse than none -- the process
// would run with a mix of file and default values that no one configured.
func TestLoad_MalformedFileAppliesNothing(t *testing.T) {
	clearEnv(t, "DOTENV_TEST_PARTIAL")
	path := writeEnvFile(t, "DOTENV_TEST_PARTIAL=applied\nnot a pair\n")

	if err := Load(path); err == nil {
		t.Fatal("Load() = nil, want an error")
	}
	if _, ok := os.LookupEnv("DOTENV_TEST_PARTIAL"); ok {
		t.Error("DOTENV_TEST_PARTIAL was set despite a later parse error; Load must validate the whole file before applying any of it")
	}
}
