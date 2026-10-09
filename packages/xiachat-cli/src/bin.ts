import { buildProgram } from "./main.js";
import { Keystore } from "./keystore.js";
import { XiachatApi } from "./api.js";
import { defaultKeystorePath } from "./config.js";
import { ensureInstalled, type InstallResult } from "./install.js";

// Single-file exe: best-effort per-user install (copy + PATH) on first run.
// Quiet no-op when already installed or running under `node` in dev.
let installNote: InstallResult | null = null;
try {
  installNote = ensureInstalled();
} catch {
  installNote = null;
}

const program = buildProgram({
  keystore: new Keystore(defaultKeystorePath()),
  apiFactory: (serverUrl) => new XiachatApi(serverUrl),
  stdout: process.stdout,
});

program.parseAsync(process.argv).catch((err: unknown) => {
  console.error(err instanceof Error ? err.message : err);
  process.exitCode = 1;
});

// After the command finishes (or when commander prints help and the parse
// promise resolves), tell the user where the binary now lives.
if (installNote && (installNote.copied || installNote.pathAdded)) {
  const what = [installNote.copied ? "copied itself to " + installNote.target : null,
    installNote.pathAdded ? "added it to your user PATH" : null].filter(Boolean).join(" and ");
  process.stdout.write(
    `\nclawmessenger: ${what}.\n` +
    `Open a NEW terminal window and run \`clawmessenger\` from anywhere.\n`,
  );
}
