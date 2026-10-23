// Package envfile loads a dotenv-style file into the process environment.
//
// # Why this exists
//
// The server used to take its whole configuration from the process
// environment and nothing else: no dotenv, no config file. That is a clean
// rule, and it stays true for *where the values end up* — everything is still
// read back through os.Getenv. What changes is that the process is now allowed
// to seed those variables from a file before it starts reading them, because
// on an operator-managed box the alternative is a hand-maintained `export`
// block in someone's shell: it is not in version control, it is lost when the
// session ends, and a restart from a different shell silently drops it.
//
// # Precedence
//
// Precedent matters more than taste here, so state it once:
//
//   - By default the file WINS over values already present in the process
//     environment. This is what makes "edit .env, restart, done" work.
//     Without it, a restart script that re-exported the previous process's
//     environment would keep re-applying the old value and the edit would
//     look like it did nothing.
//   - Set MULTICA_ENV_NO_OVERRIDE=1 to invert that: the file then only fills
//     in keys that are still unset, and explicit process environment always
//     wins. Use that when the values come from systemd's EnvironmentFile= or
//     a container orchestrator, and the file is only a local convenience.
//
// Nothing is deleted from the environment: keys present in the process but
// absent from the file survive untouched, so a partial file cannot blank out
// unrelated configuration.
package envfile

import (
	"fmt"
	"os"
	"strings"
)

// maxExpansionDepth bounds recursive ${A} -> ${B} -> ... resolution. Real files
// nest one or two levels; a cycle would otherwise hang the process at startup.
const maxExpansionDepth = 16

// Options describes one load. The zero value is not useful; prefer Load or
// LoadFromEnv.
type Options struct {
	// Paths are tried in order; the first one that exists wins. No path is
	// merged with another — one file, one source of truth.
	Paths []string
	// Override decides whether a file value replaces an already-set process
	// environment variable.
	Override bool
}

// Result reports what happened, so callers can log it instead of guessing.
type Result struct {
	// Path is the file that was loaded, or "" when none of the candidates
	// existed.
	Path string
	// Applied are the keys written into the environment, in file order.
	Applied []string
	// Skipped are keys the file defines but that were left alone because they
	// were already set and Override was false.
	Skipped []string
	// Changed are the keys that were already set to a *different* value and
	// were replaced. Names only: values are never recorded or logged, because
	// this file holds secrets.
	Changed []string
}

// Load reads the first existing candidate file and applies it.
//
// A missing file is not an error: that is the normal case for tests, for the
// container images that configure everything through the orchestrator, and for
// every box that has not adopted the file yet. Result.Path is empty then.
func Load(opts Options) (Result, error) {
	path, err := firstExisting(opts.Paths)
	if err != nil {
		return Result{}, err
	}
	if path == "" {
		return Result{}, nil
	}

	raw, err := parseFile(path)
	if err != nil {
		return Result{}, err
	}

	res := Result{Path: path}
	for _, entry := range raw {
		value, err := resolve(entry.key, raw, map[string]bool{}, 0)
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", path, err)
		}
		previous, existed := os.LookupEnv(entry.key)
		if existed && !opts.Override {
			res.Skipped = append(res.Skipped, entry.key)
			continue
		}
		if err := os.Setenv(entry.key, value); err != nil {
			return Result{}, fmt.Errorf("%s: set %s: %w", path, entry.key, err)
		}
		res.Applied = append(res.Applied, entry.key)
		if existed && previous != value {
			res.Changed = append(res.Changed, entry.key)
		}
	}
	return res, nil
}

// LoadFromEnv is the entry point the binaries use. It reads the candidate list
// from MULTICA_ENV_FILE (paths separated by ':' or ',' , as in PATH), falling
// back to .env then ../.env — the server is run both from the repo root and
// from server/, and asking operators to remember which one is a needless trap.
//
// Override defaults to true and is inverted by MULTICA_ENV_NO_OVERRIDE=1, which
// is the setting to use when the process environment is managed elsewhere.
//
// MULTICA_ENV_SKIP=1 disables loading entirely. It exists for one scenario: a
// malformed file must not be able to keep the API down while someone finds a
// shell to fix it. Loading is the default, but it is never the only way in.
func LoadFromEnv() (Result, error) {
	if truthy(os.Getenv("MULTICA_ENV_SKIP")) {
		return Result{}, nil
	}
	paths := []string{".env", "../.env"}
	if custom := strings.TrimSpace(os.Getenv("MULTICA_ENV_FILE")); custom != "" {
		paths = nil
		// os.PathListSeparator, not a hardcoded ':'. On Windows a colon is the
		// drive separator, so splitting on it would tear "C:\app\.env" in two
		// and silently load nothing.
		for _, p := range strings.FieldsFunc(custom, func(r rune) bool {
			return r == ',' || r == rune(os.PathListSeparator)
		}) {
			if p = strings.TrimSpace(p); p != "" {
				paths = append(paths, p)
			}
		}
	}
	return Load(Options{Paths: paths, Override: !truthy(os.Getenv("MULTICA_ENV_NO_OVERRIDE"))})
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func firstExisting(paths []string) (string, error) {
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		info, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", fmt.Errorf("stat %s: %w", p, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("%s is a directory, not an env file", p)
		}
		return p, nil
	}
	return "", nil
}

type entry struct {
	key   string
	value string // already unquoted, not yet expanded
	line  int
	// literal marks a single-quoted value: ${...} inside it is data, not a
	// reference. Double quotes expand, matching shell behaviour.
	literal bool
}

// parseFile turns the file into ordered entries. Syntax errors are returned
// rather than ignored: a typo that silently drops a database URL costs far more
// than a startup failure that names the line.
func parseFile(path string) ([]entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var entries []entry
	seen := map[string]int{}
	for i, rawLine := range strings.Split(string(data), "\n") {
		lineNo := i + 1
		line := strings.TrimSuffix(rawLine, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// A leading `export ` is common in files that are also sourced by a
		// shell; accepting it costs nothing and avoids duplicating the file.
		if rest, ok := strings.CutPrefix(trimmed, "export "); ok {
			trimmed = strings.TrimSpace(rest)
		}

		eq := strings.IndexByte(trimmed, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE, got %q", lineNo, trimmed)
		}
		key := strings.TrimSpace(trimmed[:eq])
		if !validKey(key) {
			return nil, fmt.Errorf("line %d: invalid variable name %q", lineNo, key)
		}
		value, literal, err := parseValue(strings.TrimLeft(trimmed[eq+1:], " \t"))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		if prev, dup := seen[key]; dup {
			// Last assignment wins, as in every shell. Say so in the error
			// only when the two differ, which is almost always a mistake.
			if entries[prev].value != value {
				return nil, fmt.Errorf("line %d: %s already assigned on line %d", lineNo, key, entries[prev].line)
			}
			continue
		}
		seen[key] = len(entries)
		entries = append(entries, entry{key: key, value: value, line: lineNo, literal: literal})
	}
	return entries, nil
}

func validKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// parseValue handles the three forms a value can take: bare, single-quoted
// (literal) and double-quoted (escapes). Only bare values get inline-comment
// stripping, because `PASSWORD=p#ss` must survive while `PORT=5101  # api`
// should not keep the comment.
func parseValue(v string) (string, bool, error) {
	if v == "" {
		return "", false, nil
	}
	switch v[0] {
	case '\'':
		end := strings.IndexByte(v[1:], '\'')
		if end < 0 {
			return "", false, fmt.Errorf("unterminated single-quoted value")
		}
		return v[1 : 1+end], true, nil
	case '"':
		var b strings.Builder
		escaped := false
		for i := 1; i < len(v); i++ {
			c := v[i]
			if escaped {
				switch c {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				case 'r':
					b.WriteByte('\r')
				case '"', '\\', '$':
					b.WriteByte(c)
				default:
					b.WriteByte('\\')
					b.WriteByte(c)
				}
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
				return b.String(), false, nil
			default:
				b.WriteByte(c)
			}
		}
		return "", false, fmt.Errorf("unterminated double-quoted value")
	}

	if idx := inlineCommentIndex(v); idx >= 0 {
		v = v[:idx]
	}
	return strings.TrimRight(v, " \t"), false, nil
}

// inlineCommentIndex finds a `#` that starts a comment: it must be preceded by
// whitespace, so values like `abc#123` are left intact.
func inlineCommentIndex(v string) int {
	for i := 0; i < len(v); i++ {
		if v[i] != '#' {
			continue
		}
		if i == 0 {
			return 0
		}
		if prev := v[i-1]; prev == ' ' || prev == '\t' {
			return i
		}
	}
	return -1
}

// resolve expands ${VAR}, ${VAR:-default}, ${VAR-default} and $VAR references.
// The lookup order is: keys defined in this file, then the process environment.
// os.Expand is not used because it does not understand shell-style defaults,
// and a literal `${FRONTEND_ORIGIN}` leaking into a URL is a bug that only
// shows up at request time.
func resolve(key string, raw []entry, visiting map[string]bool, depth int) (string, error) {
	if depth > maxExpansionDepth {
		return "", fmt.Errorf("%s: variable references are nested too deeply (cycle?)", key)
	}
	value := ""
	literal := false
	for _, e := range raw {
		if e.key == key {
			value = e.value
			literal = e.literal
			break
		}
	}
	if literal {
		return value, nil
	}

	visiting[key] = true
	defer delete(visiting, key)

	var b strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c != '$' || i+1 >= len(value) {
			b.WriteByte(c)
			continue
		}
		next := value[i+1]
		if next == '$' { // escaped dollar
			b.WriteByte('$')
			i++
			continue
		}
		if next == '{' {
			end := strings.IndexByte(value[i+2:], '}')
			if end < 0 {
				b.WriteByte(c)
				continue
			}
			expr := value[i+2 : i+2+end]
			i += 2 + end
			resolved, err := resolveExpr(expr, raw, visiting, depth)
			if err != nil {
				return "", err
			}
			b.WriteString(resolved)
			continue
		}
		if isIdentStart(next) {
			j := i + 1
			for j < len(value) && isIdentPart(value[j]) {
				j++
			}
			name := value[i+1 : j]
			i = j - 1
			resolved, err := resolveExpr(name, raw, visiting, depth)
			if err != nil {
				return "", err
			}
			b.WriteString(resolved)
			continue
		}
		b.WriteByte(c)
	}
	return b.String(), nil
}

func resolveExpr(expr string, raw []entry, visiting map[string]bool, depth int) (string, error) {
	name := expr
	fallback := ""
	hasFallback := false
	fallbackOnEmpty := false
	if idx := strings.Index(expr, ":-"); idx >= 0 {
		name, fallback, hasFallback, fallbackOnEmpty = expr[:idx], expr[idx+2:], true, true
	} else if idx := strings.IndexByte(expr, '-'); idx > 0 {
		name, fallback, hasFallback = expr[:idx], expr[idx+1:], true
	}

	value, ok, err := lookup(name, raw, visiting, depth+1)
	if err != nil {
		return "", err
	}
	if ok && (value != "" || !fallbackOnEmpty) {
		return value, nil
	}
	if hasFallback {
		return resolveValue(fallback, raw, visiting, depth+1)
	}
	return "", nil
}

func lookup(name string, raw []entry, visiting map[string]bool, depth int) (string, bool, error) {
	if visiting[name] {
		return "", false, fmt.Errorf("%s: circular variable reference", name)
	}
	for _, e := range raw {
		if e.key == name {
			v, err := resolve(name, raw, visiting, depth)
			return v, true, err
		}
	}
	if v, ok := os.LookupEnv(name); ok {
		return v, true, nil
	}
	return "", false, nil
}

func resolveValue(text string, raw []entry, visiting map[string]bool, depth int) (string, error) {
	tmp := append(append([]entry{}, raw...), entry{key: "\x00literal", value: text})
	return resolve("\x00literal", tmp, visiting, depth)
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}
