import * as os from "node:os";
import { buildProgram } from "./main.js";
import { Keystore } from "./keystore.js";
import { XiachatApi } from "./api.js";
import { defaultKeystorePath } from "./config.js";
import { ensureInstalled, isSeaExecutable, installPaths, type InstallResult } from "./install.js";
import { applyStagedUpdate, cleanupStaleBackups } from "./self-update.js";

// A staged update — downloaded and checksum-verified by `clawmessenger update`
// on an earlier run — is swapped in here, before anything else starts. The
// current process keeps executing the image it already mapped; every process
// started afterwards (including the agents the supervisor spawns) runs the new
// version. Renaming the live exe is what makes this possible on Windows, where
// an in-place overwrite is refused with EBUSY.
let stagedVersion: string | null = null;
try {
  if (isSeaExecutable(process.argv[1])) {
    const target = installPaths(process.platform, os.homedir()).target;
    const applied = applyStagedUpdate(target, process.platform);
    if (applied.applied) stagedVersion = applied.version ?? null;
    cleanupStaleBackups(target);
  }
} catch {
  // Update bookkeeping must never block normal operation.
}

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

if (stagedVersion) {
  process.stdout.write(
    `\nclawmessenger: update ${stagedVersion} is now on disk; ` +
    `it takes effect the next time the supervisor starts.\n`,
  );
}
