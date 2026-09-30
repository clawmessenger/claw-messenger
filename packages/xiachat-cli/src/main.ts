import { Command } from "commander";
import { createHash } from "node:crypto";
import * as os from "node:os";
import { Keystore, type StoredCredentials } from "./keystore.js";
import { XiachatApi } from "./api.js";
import { discoverAgents } from "./agents.js";
import { DEFAULT_SERVER_URL, machineId } from "./config.js";

// Deterministic per (ticket, machine): a retried claim after a lost
// response reuses the same key, so the server deduplicates instead of
// answering a misleading 409 "another device". Different machines derive
// different keys.
export function pairIdempotencyKey(ticket: string, mid: string): string {
  return `idem-${createHash("sha256").update(`${ticket}:${mid}`).digest("hex").slice(0, 24)}`;
}

export interface BuildProgramOpts {
  keystore: Keystore;
  apiFactory: (serverUrl: string) => XiachatApi;
  stdout: NodeJS.WriteStream;
  // Injectable for tests; defaults to the machine-id minted next to the
  // keystore (see config.ts machineId).
  machineId?: () => string;
}

export function buildProgram(opts: BuildProgramOpts): Command {
  const program = new Command();
  program
    .name("xiachat")
    .description("User-device agent CLI over RongCloud IM")
    .version("0.1.0")
    // Tests parse this program in-process; commander would otherwise
    // process.exit on missing options and kill the vitest runner.
    .exitOverride();

  program
    .command("register")
    .description("Register this device as a RongCloud AI node")
    .requiredOption("--name <name>", "node display name")
    .requiredOption("--ai-type <type>", "agent platform (claude, codex, opencode, ...)")
    .requiredOption("--server <url>", "Quukk server base URL")
    .option("--pairing-ticket <ticket>", "attribute the node to a workspace via pairing ticket")
    .action(async (cmdOpts: { name: string; aiType: string; server: string; pairingTicket?: string }) => {
      const api = opts.apiFactory(cmdOpts.server);
      try {
        const result = await api.register({
          name: cmdOpts.name,
          aiType: cmdOpts.aiType,
          nodeType: "ai",
          // Stable per-machine id: keeps rc_user_id (and thus node_id)
          // consistent across re-registers on one machine.
          macAddress: (opts.machineId ?? machineId)(),
          pairingTicket: cmdOpts.pairingTicket,
        });
        const creds: StoredCredentials = {
          nodeId: result.nodeId,
          token: result.token,
          credentialId: result.deviceCredentialTicket,
          serverUrl: cmdOpts.server,
        };
        opts.keystore.save(creds);
        opts.stdout.write(`registered ${result.nodeId}\n`);
      } catch (err) {
        const message = err instanceof Error ? err.message : String(err);
        if (message.includes("attribution")) {
          throw new Error(`${message}\nhint: provide --pairing-ticket (get one from your workspace admin)`);
        }
        throw err;
      }
    });

  program
    .command("pair")
    .description("Register this device and claim a pairing session with a ticket")
    .requiredOption("--ticket <ticket>", "pairing ticket (pt_...)")
    .option("--server <url>", "Quukk server base URL", DEFAULT_SERVER_URL)
    .option("--name <name>", "node display name")
    .option("--ai-type <type>", "agent platform (claude, codex, opencode, ...)", "opencode")
    .action(async (cmdOpts: { ticket: string; server: string; name?: string; aiType: string }) => {
      const api = opts.apiFactory(cmdOpts.server);
      // Register with the ticket first: the server attributes the node to
      // the ticket's workspace and backfills the session's candidate node
      // list so the subsequent claim can issue device credentials.
      try {
        const reg = await api.register({
          name: cmdOpts.name ?? `${os.hostname()} ${cmdOpts.aiType}`,
          aiType: cmdOpts.aiType,
          nodeType: "ai",
          macAddress: (opts.machineId ?? machineId)(),
          pairingTicket: cmdOpts.ticket,
        });
        opts.keystore.save({
          nodeId: reg.nodeId,
          token: reg.token,
          credentialId: reg.deviceCredentialTicket,
          serverUrl: cmdOpts.server,
        });
        opts.stdout.write(`registered ${reg.nodeId}\n`);
      } catch (err) {
        const message = err instanceof Error ? err.message : String(err);
        if (message.includes("attribution")) {
          throw new Error(`${message}\nhint: provide --pairing-ticket (get one from your workspace admin)`);
        }
        throw err;
      }
      const idemKey = pairIdempotencyKey(cmdOpts.ticket, (opts.machineId ?? machineId)());
      const result = await api.claimPairing(cmdOpts.ticket, "", idemKey);
      if (result.session?.status === "claimed" && !result.deviceSecret) {
        throw new Error("ticket already claimed by another device");
      }
      opts.keystore.save({
        nodeId: result.nodeId,
        token: "",
        credentialId: result.deviceCredentialId,
        deviceSecret: result.deviceSecret,
        serverUrl: cmdOpts.server,
      });
      opts.stdout.write(`paired as ${result.nodeId}\n`);
    });

  program
    .command("login")
    .description("Refresh the IM token for the stored node")
    .action(async () => {
      const creds = opts.keystore.load();
      if (!creds) throw new Error("no credentials; run xiachat register or pair first");
      const api = opts.apiFactory(creds.serverUrl);
      const { token } = await api.refreshToken(creds.nodeId);
      opts.keystore.save({ ...creds, token });
      opts.stdout.write("token refreshed\n");
    });

  program
    .command("agents")
    .description("List agent CLIs discoverable on this machine's PATH")
    .action(async () => {
      const found = await discoverAgents({});
      if (found.length === 0) {
        opts.stdout.write("no agent CLIs found on PATH\n");
        return;
      }
      for (const a of found) opts.stdout.write(`${a.name}\t${a.path}\n`);
    });

  program
    .command("run")
    .description("Connect to RongCloud IM and dispatch agent turns")
    .requiredOption("--agent <name>", "agent CLI name; must be discoverable on PATH (see xiachat agents)")
    .option("--model <model>", "model override passed to the agent CLI")
    .action(async (cmdOpts: { agent: string; model?: string }) => {
      const creds = opts.keystore.load();
      if (!creds) throw new Error("no credentials; run xiachat register or pair first");
      const api = opts.apiFactory(creds.serverUrl);
      const found = await discoverAgents({});
      const agent = found.find((a) => a.name === cmdOpts.agent);
      if (!agent) throw new Error(`agent CLI ${cmdOpts.agent} not found on PATH; run xiachat agents`);
      const { startRunLoop, createImlibTransport } = await import("./run.js");
      const transport = await createImlibTransport();
      await startRunLoop({
        creds,
        api,
        transport,
        agentExecPath: agent.path,
        agentName: agent.name,
        model: cmdOpts.model,
        stdout: opts.stdout,
      });
    });

  program
    .command("status")
    .description("Show stored identity")
    .action(async () => {
      const creds = opts.keystore.load();
      if (!creds) {
        opts.stdout.write("not registered\n");
        return;
      }
      opts.stdout.write(`node: ${creds.nodeId}\nserver: ${creds.serverUrl}\ntoken: ${creds.token ? "present" : "missing"}\n`);
    });

  return program;
}
