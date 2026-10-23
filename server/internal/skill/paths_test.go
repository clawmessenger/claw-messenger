package skill

import "testing"

// TestIsSafeBundlePath pins the predicate against the review matrix. The
// must-false groups are security cases (Windows resolves each of them to a file
// this predicate must not allow); the must-true group is the counterweight —
// over-tightening here silently breaks real bundles, so every entry in it is a
// regression guard rather than an afterthought.
func TestIsSafeBundlePath(t *testing.T) {
	cases := []struct {
		name string
		p    string
		want bool
	}{
		// --- structural: host-independent shapes -------------------------
		{"plain nested path", "a/b.md", true},
		{"root file", "notes.md", true},
		{"absolute unix path", "/abs", false},
		{"absolute unix file", "/abs/SKILL.md", false},
		{"unc path", `\\server\share`, false},
		{"unc path slash form", "//server/share/SKILL.md", false},
		{"windows drive relative", "C:x", false},
		{"windows drive absolute", "C:/abs/SKILL.md", false},
		{"parent dir", "..", false},
		{"parent traversal", "../x", false},
		{"nested traversal", "a/../x", false},
		{"non canonical double slash", "a//b", false},
		{"non canonical dot prefix", "./a.md", false},
		{"backslash separator", `sub\a.md`, false},
		{"nul byte", "a\x00b.md", false},
		{"empty", "", false},

		// --- must false: Windows trims a component's trailing spaces/dots -
		// "SKILL.md " IS "SKILL.md" on Windows, so allowing it overwrites the
		// primary content while every reserved-name check sees a different
		// string; ".. " IS "..", which is real traversal.
		{"trailing space aliases skill md", "SKILL.md ", false},
		{"trailing space on nested skill md", "a/SKILL.md ", false},
		{"trailing space only", "y ", false},
		{"trailing space on inner segment", "a /b.md", false},
		{"trailing space on leading inner segment", "sub. /x", false},
		{"traversal with trailing space", ".. /x", false},
		{"traversal with trailing dot and space", ".. ./x", false},
		{"trailing dot", "x.", false},
		{"trailing dots", "a...", false},
		{"dots only", "...", false},
		{"trailing dot on inner segment", "a./b.md", false},

		// --- must false: paths that never name a file inside the bundle ---
		{"current dir", ".", false},
		{"traversal collapsed to current dir", "a/..", false},

		// --- must false: NTFS alternate data streams ----------------------
		{"alternate data stream", "ab:c", false},
		{"reserved name with stream", "SKILL.md:foo", false},
		{"nested alternate data stream", "a/b:c", false},

		// --- must false: MS-DOS device names ------------------------------
		// Matched on the stem before the first dot, case-insensitively: on
		// Windows "com1.md" is still COM1. Writing one talks to a device
		// instead of creating a file.
		{"device name", "CON", false},
		{"device name with extension", "con.md", false},
		{"device name nul", "NUL", false},
		{"device name with extension nul", "nul.txt", false},
		{"device name com port", "COM1.txt", false},
		{"device name com port with extension", "COM1.md", false},
		{"device name nested", "a/lpt9", false},
		{"device name with padding", "con .md", false},

		// --- must true: legal names, each one a way to over-tighten -------
		{"dotfile name starting with two dots", "..notes.md", true},
		{"dots inside a segment", "a..b/c", true},
		{"dots inside a file name", "notes..md", true},
		{"dots inside segments", "a.b/c.d", true},
		// "console" starts with "con" but is not the device: a prefix match
		// here would delete a perfectly ordinary file name.
		{"device-like prefix that is not a device", "console.md", true},
		{"device-like prefix nul", "nullable.md", true},
		{"spaces inside segments", "a b/c d.md", true},
		{"dots after a leading pair", "a/..b", true},
		{"wrapper layout", "my-skill/SKILL.md", true},
		{"non-ascii file name", "文件.md", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSafeBundlePath(tc.p); got != tc.want {
				t.Errorf("IsSafeBundlePath(%q) = %v, want %v", tc.p, got, tc.want)
			}
		})
	}
}
