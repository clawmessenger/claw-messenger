package skill

import (
	"path"
	"strings"
)

// IsSafeBundlePath reports whether p is a relative path inside a skill bundle:
// safe to write under a bundle directory and safe to hand to a daemon.
//
// The predicate is deliberately host-OS independent. The server may run on
// Linux while the daemon runs on Windows (or the reverse), so neither
// filepath.IsAbs nor filepath.Clean can be used: they interpret the path
// according to the OS the server happens to run on. On Windows,
// filepath.IsAbs("/abs/x") and filepath.IsAbs(`\abs\x`) are both false and
// filepath.IsAbs(`\\server\share\x`) is false as well, which is exactly how
// rooted and UNC archive entries slipped through validation. Everything below
// is judged with path (slash semantics only), so the answer is identical on
// every host.
//
// Callers that receive native paths (a Windows client's JSON body, a
// PowerShell-made archive) must normalize backslashes to slashes before
// calling; this function does not guess what a backslash means.
//
// The checks are per component rather than whole-string because Windows
// resolves several spellings of a component to the same file: "SKILL.md "
// (trailing space) IS "SKILL.md", ".. " IS "..", and "ab:c" addresses an NTFS
// alternate data stream of "ab". Accepting any of those would let a bundle
// write over the primary content while every reserved-name check saw a
// different string, so the rules below apply to each segment.
func IsSafeBundlePath(p string) bool {
	// An empty string names nothing.
	if p == "" {
		return false
	}
	// Only canonical spellings are accepted. "./x", "a//b" and "a/./b" resolve
	// to a different string than they read as, so a bundle could store one path
	// and have another one written; a path that does not survive Clean is a way
	// to ship one file and land another.
	if p != path.Clean(p) {
		return false
	}
	// Backslash is a separator on Windows only. After the caller's
	// normalization it is a literal character that cannot exist in a bundle
	// path, and treating it as text here would let `\\server\share\x` through on
	// a Unix host. NUL truncates the path at the syscall boundary, so it is
	// rejected for the same reason.
	if strings.ContainsRune(p, '\\') || strings.ContainsRune(p, '\x00') {
		return false
	}
	// Rooted paths. This covers "/abs/x" and also UNC "//server/share/x":
	// path.Clean collapses the doubled leading slash to "/server/share/x",
	// which path.IsAbs still reports as absolute. path.IsAbs (not
	// filepath.IsAbs) is what makes this hold regardless of the host OS.
	if path.IsAbs(p) {
		return false
	}
	// Traversal out of the bundle root. Compared on whole segments: a literal
	// prefix match would also reject the perfectly legal file name
	// "..notes.md".
	if p == ".." || strings.HasPrefix(p, "../") {
		return false
	}
	// Per-component checks. Windows applies the rules below to each component
	// of a path, and two different strings can name the same component — so
	// judging only the whole string leaves aliases open.
	for _, seg := range strings.Split(p, "/") {
		// path.Clean already collapsed doubled and trailing separators; an
		// empty segment here means the input bypassed our canonical-form
		// requirement.
		if seg == "" {
			return false
		}
		// "." and ".." are the two directory references that mean "not a file
		// inside the bundle" (the ".." prefix case is caught above; this also
		// covers a nested "a/./b" that Clean would have folded away).
		if seg == "." || seg == ".." {
			return false
		}
		// Windows silently strips trailing spaces and dots from a component,
		// so "SKILL.md " opens SKILL.md and ".. " becomes "..". This is not a
		// cosmetic rule: allowing the first lets an imported bundle overwrite
		// the primary content — silently through os.WriteFile in the qwenpaw
		// workspace, or as a prep-killing errPathPreExists in the sandbox —
		// because every IsReservedContentPath check compares against a string
		// that ends in a space; allowing the second is real traversal out of
		// the bundle root. Only the tail is trimmed, so the legal file name
		// "..notes.md" and the legal segment "a.b" are unaffected.
		if last := seg[len(seg)-1]; last == ' ' || last == '.' {
			return false
		}
		// A colon inside a segment is NTFS alternate-data-stream syntax
		// ("ab:c" names a stream of file "ab") and, in the leading position,
		// a volume ("C:x" is drive-relative, "C:/x" absolute). path.IsAbs has
		// no volume concept, so this is the only place a drive is caught.
		if strings.ContainsRune(seg, ':') {
			return false
		}
		// MS-DOS device names are resolved in every directory, not just the
		// root, and Windows matches them before the extension ("con.md" is
		// still the console). Writing one does not create a file, it talks to
		// a device, so the result is unpredictable — refuse instead.
		if isWindowsDeviceName(seg) {
			return false
		}
	}
	return true
}

// isWindowsDeviceName reports whether seg names a Windows device. The match is
// case-insensitive and ignores an extension, because that is how Windows
// resolves it.
func isWindowsDeviceName(seg string) bool {
	name := seg
	// Windows ignores the extension when resolving a device: "con.md" is the
	// console. Trailing dots/spaces are trimmed here as well so "con .md"
	// cannot slip past (the segment rule above would not see a trailing space
	// in the middle of a segment).
	if i := strings.IndexByte(name, '.'); i >= 0 {
		name = name[:i]
	}
	name = strings.ToUpper(strings.TrimRight(name, " ."))
	switch name {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	// COM1-COM9 and LPT1-LPT9 (COM0/LPT0 are not standard devices).
	if len(name) == 4 && name[3] >= '1' && name[3] <= '9' &&
		(strings.HasPrefix(name, "COM") || strings.HasPrefix(name, "LPT")) {
		return true
	}
	return false
}
