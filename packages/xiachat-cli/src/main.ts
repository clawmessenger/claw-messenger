import { Command } from "commander";
import { spawn, type ChildProcess } from "node:child_process";
import { createHash } from "node:crypto";
import * as os from "node:os";
import { Keystore, type StoredCredentials } from "./keystore.js";
import { XiachatApi } from "./api.js";
import { discoverAgents } from "./agents.js";
import { DEFAULT_SERVER_URL, machineId } from "./config.js";
import { ensureOpencodeInstalled, OPS_AGENT_NAME, OPS_TURN_TIMEOUT_MS, setupOpsWorkdir } from "./ops.js";

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
    .name("clawmessenger")
    .description("User-device agent CLI over RongCloud IM")
    .version("0.1.1")
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
      // list so the subsequent claim can issue device credentials. The
      // machine's discoverable agent CLIs are reported alongside so the web
      // dialog can offer them for checkbox binding.
      const foundAgents = await discoverAgents({});
      try {
        const reg = await api.register({
          name: cmdOpts.name ?? `${os.hostname()} ${cmdOpts.aiType}`,
          aiType: cmdOpts.aiType,
          nodeType: "ai",
          macAddress: (opts.machineId ?? machineId)(),
          pairingTicket: cmdOpts.ticket,
          agents: foundAgents.map((a) => a.name),
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
      if (!creds) throw new Error("no credentials; run clawmessenger register or pair first");
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
    .description(
      "Connect to RongCloud IM and dispatch agent turns. Without --agent, run every agent bound to this device (supervisor mode).",
    )
    .option("--agent <name>", "agent CLI name; must be discoverable on PATH (see clawmessenger agents)")
    .option("--model <model>", "model override passed to the agent CLI")
    .action(async (cmdOpts: { agent?: string; model?: string }) => {
      // Supervisor-spawned children carry their agent-specific credentials
      // in the environment; a plain `clawmessenger run` never sees them otherwise
      // because each child overwrites nothing in the shared keystore.
      const envCreds = process.env.CLAWMESSENGER_AGENT_CREDS;
      const parsedEnvCreds = envCreds ? (JSON.parse(envCreds) as StoredCredentials) : undefined;
      const creds = parsedEnvCreds ?? opts.keystore.load();
      if (!creds) throw new Error("no credentials; run clawmessenger register or pair first");
      const api = opts.apiFactory(creds.serverUrl);
      if (!cmdOpts.agent) {
        await runSupervisor({ opts, creds, api, model: cmdOpts.model });
        return;
      }
      // The built-in ops agent runs on the local opencode CLI (auto-installed
      // when missing); it is never skipped by PATH discovery and runs with a
      // longer turn timeout and its own ops workdir (AGENTS.md system prompt).
      let agentExecPath: string;
      let agentName: string;
      let turnTimeoutMs: number | undefined;
      let cwd: string | undefined;
      if (cmdOpts.agent === OPS_AGENT_NAME) {
        const opencode = await ensureOpencodeInstalled();
        const workdir = await setupOpsWorkdir();
        agentExecPath = opencode.path;
        agentName = "opencode";
        turnTimeoutMs = OPS_TURN_TIMEOUT_MS;
        cwd = workdir;
      } else {
        const found = await discoverAgents({});
        const agent = found.find((a) => a.name === cmdOpts.agent);
        if (!agent) throw new Error(`agent CLI ${cmdOpts.agent} not found on PATH; run clawmessenger agents`);
        agentExecPath = agent.path;
        agentName = agent.name;
      }
      const { startRunLoop, createImlibTransport } = await import("./run.js");
      const transport = await createImlibTransport();
      const hb = setInterval(() => {
        void api
          .heartbeat(creds.nodeId ?? "", creds.credentialId ?? "", creds.deviceSecret ?? "")
          .catch(() => {});
      }, 30_000);
      try {
        await startRunLoop({
          creds,
          api,
          transport,
          agentExecPath,
          agentName,
          model: cmdOpts.model,
          stdout: opts.stdout,
          turnTimeoutMs,
          cwd,
        });
      } finally {
        clearInterval(hb);
      }
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

// Supervisor mode: `clawmessenger run` with no --agent. Exchanges the machine
// node's device credential for every bound agent's IM credentials, then
// spawns one child `clawmessenger run --agent <name>` per agent. Each child gets
// its agent-specific credentials via CLAWMESSENGER_AGENT_CREDS (imlib-next is a
// module-level singleton, so one OS process per IM user is required).
interface RunSupervisorOpts {
  opts: BuildProgramOpts;
  creds: StoredCredentials;
  api: XiachatApi;
  model?: string;
}

async function runSupervisor(supervisor: RunSupervisorOpts): Promise<void> {
  const { opts, creds, api } = supervisor;
  if (!creds.credentialId || !creds.deviceSecret) {
    throw new Error("no machine device credential; run clawmessenger pair on this device first");
  }
  // Best-effort auto-bind of the built-in ops agent so every device gets ops
  // capability; server-side binding is idempotent, so this is safe every start.
  try {
    await api.bindDeviceAgents(creds.nodeId, creds.credentialId, creds.deviceSecret, [OPS_AGENT_NAME]);
  } catch (err) {
    console.warn("ops auto-bind failed:", err instanceof Error ? err.message : String(err));
  }
  const bound = await api.fetchDeviceNodes(creds.nodeId, creds.credentialId, creds.deviceSecret);
  if (bound.length === 0) {
    throw new Error("no agents bound to this device; select and bind agents in the web binding dialog first");
  }
  const found = await discoverAgents({});
  // SEA single-file exe: re-invoke the exe itself. Node script mode: re-run
  // the bundle entry so argv keeps the [execPath, script, ...] shape.
  const script = process.argv[1];
  const isSea = !script || !script.endsWith(".js");
  const children: ChildProcess[] = [];
  for (const agent of bound) {
    // The built-in ops agent has no PATH-installed CLI of its own; the child
    // (`clawmessenger run --agent ops`) installs opencode itself before connecting.
    if (agent.agent !== OPS_AGENT_NAME) {
      const local = found.find((f) => f.name === agent.agent);
      if (!local) {
        console.error(`agent CLI ${agent.agent} not found on PATH; skipping`);
        continue;
      }
    }
    const childCreds: StoredCredentials = {
      nodeId: agent.nodeId,
      token: agent.token,
      credentialId: agent.credentialId,
      deviceSecret: agent.deviceSecret,
      serverUrl: creds.serverUrl,
    };
    const runArgs = ["run", "--agent", agent.agent];
    if (supervisor.model) runArgs.push("--model", supervisor.model);
    const child = spawn(process.execPath, isSea ? runArgs : [script as string, ...runArgs], {
      env: { ...process.env, CLAWMESSENGER_AGENT_CREDS: JSON.stringify(childCreds) },
    });
    child.stdout?.on("data", (chunk: Buffer) => opts.stdout.write(`[${agent.agent}] ${chunk}`));
    child.stderr?.on("data", (chunk: Buffer) => process.stderr.write(`[${agent.agent}] ${chunk}`));
    child.on("exit", (code) => {
      console.error(`agent ${agent.agent} exited with code ${code}`);
    });
    children.push(child);
  }
  if (children.length === 0) {
    throw new Error("none of the bound agent CLIs is available on this machine's PATH");
  }
  opts.stdout.write(`clawmessenger supervisor: ${children.length} agent(s) online\n`);
  const shutdown = (): void => {
    for (const child of children) child.kill("SIGTERM");
  };
  process.on("SIGINT", shutdown);
  process.on("SIGTERM", shutdown);
  await new Promise<void>(() => {}); // supervise until process exit
}
