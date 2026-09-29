import { buildProgram } from "./main.js";
import { Keystore } from "./keystore.js";
import { XiachatApi } from "./api.js";
import { defaultKeystorePath } from "./config.js";

const program = buildProgram({
  keystore: new Keystore(defaultKeystorePath()),
  apiFactory: (serverUrl) => new XiachatApi(serverUrl),
  stdout: process.stdout,
});

program.parseAsync(process.argv).catch((err: unknown) => {
  console.error(err instanceof Error ? err.message : err);
  process.exitCode = 1;
});
