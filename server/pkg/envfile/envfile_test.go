package envfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// clearEnv unsets the keys a test touches, so a value inherited from the
// developer's shell cannot make an assertion pass or fail by accident.
func clearEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		t.Setenv(k, "") // registers restoration
		os.Unsetenv(k)  // then actually remove it
	}
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	res, err := Load(Options{Paths: []string{filepath.Join(t.TempDir(), "absent.env")}, Override: true})
	if err != nil {
		t.Fatalf("expected no error for a missing file, got %v", err)
	}
	if res.Path != "" || len(res.Applied) != 0 {
		t.Fatalf("expected empty result, got %+v", res)
	}
}

func TestLoadParsesTheSyntaxTheRepoActuallyUses(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, ".env", strings.Join([]string{
		"# a comment",
		"",
		"PLAIN=value",
		"export EXPORTED=yes",
		"SPACED = padded ",
		"QUOTED=\"a b\tc\"",
		"LITERAL='${NOT_EXPANDED}'",
		"BARE_HASH=abc#123",
		"INLINE=5101  # api port",
		"EMPTY=",
		"CRLF=windows\r",
	}, "\n"))

	keys := []string{"PLAIN", "EXPORTED", "SPACED", "QUOTED", "LITERAL", "BARE_HASH", "INLINE", "EMPTY", "CRLF"}
	clearEnv(t, keys...)

	res, err := Load(Options{Paths: []string{path}, Override: true})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if res.Path != path || len(res.Applied) != len(keys) {
		t.Fatalf("expected %d keys from %s, got %+v", len(keys), path, res)
	}

	want := map[string]string{
		"PLAIN":     "value",
		"EXPORTED":  "yes",
		"SPACED":    "padded",
		"QUOTED":    "a b\tc",
		"LITERAL":   "${NOT_EXPANDED}", // single quotes are literal
		"BARE_HASH": "abc#123",         // '#' without leading space is data
		"INLINE":    "5101",            // '#' after whitespace is a comment
		"EMPTY":     "",
		"CRLF":      "windows",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestLoadExpandsReferencesAcrossFileAndEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, ".env", strings.Join([]string{
		"WEB_PORT=3000",
		"PUBLIC_URL=http://localhost:${WEB_PORT}",
		"BRACELESS=http://localhost:$WEB_PORT/x",
		"DEFAULTED=${MISSING_KEY:-fallback}",
		"EMPTY_DEFAULTED=${EMPTY_BASE:-fallback}",
		"EMPTY_BASE=",
		"NESTED_A=alpha",
		"NESTED_B=${NESTED_A}-beta",
		"NESTED_C=${NESTED_B}-gamma",
		"FROM_OS=${ENVFILE_TEST_OS}",
		"CHAINED=${PUBLIC_URL}/app",
	}, "\n"))

	keys := []string{"WEB_PORT", "PUBLIC_URL", "BRACELESS", "DEFAULTED", "EMPTY_DEFAULTED",
		"EMPTY_BASE", "NESTED_A", "NESTED_B", "NESTED_C", "FROM_OS", "CHAINED"}
	clearEnv(t, keys...)
	t.Setenv("ENVFILE_TEST_OS", "from-process")

	if _, err := Load(Options{Paths: []string{path}, Override: true}); err != nil {
		t.Fatalf("load: %v", err)
	}

	want := map[string]string{
		"PUBLIC_URL":      "http://localhost:3000",
		"BRACELESS":       "http://localhost:3000/x",
		"DEFAULTED":       "fallback",
		"EMPTY_DEFAULTED": "fallback", // :- treats empty as unset
		"NESTED_C":        "alpha-beta-gamma",
		"FROM_OS":         "from-process",
		"CHAINED":         "http://localhost:3000/app",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

// TestOverridePrecedence pins the two modes. This is the behaviour operators
// depend on: the file wins by default so an edit takes effect on restart, and
// MULTICA_ENV_NO_OVERRIDE=1 flips it for orchestrator-managed deployments.
func TestOverridePrecedence(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, ".env", "CONFLICT=from-file\nONLY_IN_FILE=file\n")

	t.Run("file wins", func(t *testing.T) {
		t.Setenv("CONFLICT", "from-process")
		clearEnv(t, "ONLY_IN_FILE")

		res, err := Load(Options{Paths: []string{path}, Override: true})
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if got := os.Getenv("CONFLICT"); got != "from-file" {
			t.Fatalf("CONFLICT = %q, want from-file", got)
		}
		if len(res.Changed) != 1 || res.Changed[0] != "CONFLICT" {
			t.Fatalf("Changed = %v, want [CONFLICT]", res.Changed)
		}
		if len(res.Skipped) != 0 {
			t.Fatalf("Skipped = %v, want none", res.Skipped)
		}
	})

	t.Run("process wins when override is off", func(t *testing.T) {
		t.Setenv("CONFLICT", "from-process")
		clearEnv(t, "ONLY_IN_FILE")

		res, err := Load(Options{Paths: []string{path}, Override: false})
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if got := os.Getenv("CONFLICT"); got != "from-process" {
			t.Fatalf("CONFLICT = %q, want from-process", got)
		}
		if got := os.Getenv("ONLY_IN_FILE"); got != "file" {
			t.Fatalf("unset key should still be filled from file, got %q", got)
		}
		if len(res.Skipped) != 1 || res.Skipped[0] != "CONFLICT" {
			t.Fatalf("Skipped = %v, want [CONFLICT]", res.Skipped)
		}
	})
}

// A partial file must not blank out unrelated configuration: the environment
// is additive here, never a replacement.
func TestLoadDoesNotRemoveUnrelatedKeys(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, ".env", "ONE=1\n")
	t.Setenv("UNRELATED", "keep-me")

	if _, err := Load(Options{Paths: []string{path}, Override: true}); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := os.Getenv("UNRELATED"); got != "keep-me" {
		t.Fatalf("UNRELATED = %q, want keep-me", got)
	}
}

func TestParseRejectsMalformedInput(t *testing.T) {
	cases := map[string]string{
		"no equals sign":      "JUSTAWORD\n",
		"invalid identifier":  "1BAD=x\n",
		"unterminated double": `Q="unclosed` + "\n",
		"unterminated single": "Q='unclosed\n",
		"duplicate with diff": "DUP=a\nDUP=b\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeFile(t, dir, ".env", body)
			if _, err := Load(Options{Paths: []string{path}, Override: true}); err == nil {
				t.Fatalf("expected an error for %q", body)
			}
		})
	}
}

func TestParseAllowsIdenticalDuplicate(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, ".env", "SAME=v\nSAME=v\n")
	t.Setenv("SAME", "")
	if _, err := Load(Options{Paths: []string{path}, Override: true}); err != nil {
		t.Fatalf("identical duplicate should be tolerated, got %v", err)
	}
	if got := os.Getenv("SAME"); got != "v" {
		t.Fatalf("SAME = %q, want v", got)
	}
}

func TestLoadFromEnvHonoursExplicitPath(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "custom.env", "FROM_CUSTOM=yes\n")
	clearEnv(t, "FROM_CUSTOM")
	t.Setenv("MULTICA_ENV_FILE", path)

	res, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv: %v", err)
	}
	if res.Path != path {
		t.Fatalf("Path = %q, want %q", res.Path, path)
	}
	if got := os.Getenv("FROM_CUSTOM"); got != "yes" {
		t.Fatalf("FROM_CUSTOM = %q, want yes", got)
	}
}

func TestLoadFromEnvDefaultIsOverride(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".env", "WHICH=file\n")
	clearEnv(t, "WHICH", "MULTICA_ENV_NO_OVERRIDE")
	t.Chdir(dir)

	if _, err := LoadFromEnv(); err != nil {
		t.Fatalf("LoadFromEnv: %v", err)
	}
	if got := os.Getenv("WHICH"); got != "file" {
		t.Fatalf("WHICH = %q, want file (override is the default)", got)
	}
}

func TestLoadFromEnvNoOverrideSwitch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".env", "WHICH=file\n")
	t.Setenv("MULTICA_ENV_NO_OVERRIDE", "1")
	t.Setenv("WHICH", "process")
	t.Chdir(dir)

	if _, err := LoadFromEnv(); err != nil {
		t.Fatalf("LoadFromEnv: %v", err)
	}
	if got := os.Getenv("WHICH"); got != "process" {
		t.Fatalf("WHICH = %q, want process", got)
	}
}

func TestLoadFromEnvSkipSwitch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".env", "SHOULD_STAY=file\n")
	clearEnv(t, "SHOULD_STAY", "MULTICA_ENV_NO_OVERRIDE")
	t.Setenv("MULTICA_ENV_SKIP", "1")
	t.Chdir(dir)

	res, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv: %v", err)
	}
	if res.Path != "" || len(res.Applied) != 0 {
		t.Fatalf("skip switch should load nothing, got %+v", res)
	}
	if got := os.Getenv("SHOULD_STAY"); got != "" {
		t.Fatalf("SHOULD_STAY = %q, want empty", got)
	}
}

func TestCircularReferenceFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, ".env", "A=${B}\nB=${A}\n")
	clearEnv(t, "A", "B")
	if _, err := Load(Options{Paths: []string{path}, Override: true}); err == nil {
		t.Fatal("expected a circular reference to be rejected")
	}
}
