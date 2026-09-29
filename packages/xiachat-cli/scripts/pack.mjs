import { build } from "esbuild";
import { chmodSync } from "node:fs";

// Bundles dist/bin.js into a standalone executable-ish entry. True single-
// file binaries (bun compile / pkg) are CI concerns; locally esbuild output
// plus a shell wrapper is enough for the smoke test.
await build({
  entryPoints: ["dist/bin.js"],
  bundle: true,
  platform: "node",
  target: "node20",
  format: "esm",
  outfile: "dist/xiachat.bundle.js",
  external: ["@rongcloud/imlib-next"],
  // CJS deps (commander) reach node builtins through require(); ESM output
  // needs a real require shim or esbuild throws "Dynamic require of ...".
  banner: {
    js: 'import { createRequire as __createRequire } from "node:module";\nconst require = __createRequire(import.meta.url);',
  },
});
chmodSync("dist/xiachat.bundle.js", 0o755);
console.log("bundled dist/xiachat.bundle.js — run with: node dist/xiachat.bundle.js <cmd>");
