import { existsSync, readFileSync, writeFileSync, chmodSync, mkdirSync } from "node:fs";
import { dirname } from "node:path";

// Credentials are stored as JSON with 0600 permissions. On Windows the mode
// bit is advisory only (ACLs apply); the file still lives under the user
// profile. OS keychain integration is a future adapter.
export interface StoredCredentials {
  nodeId: string;
  token: string;
  credentialId?: string;
  deviceSecret?: string;
  appKey?: string;
  serverUrl: string;
}

export class Keystore {
  constructor(private readonly path: string) {}

  load(): StoredCredentials | null {
    if (!existsSync(this.path)) return null;
    try {
      return JSON.parse(readFileSync(this.path, "utf8")) as StoredCredentials;
    } catch {
      return null;
    }
  }

  save(creds: StoredCredentials): void {
    mkdirSync(dirname(this.path), { recursive: true });
    writeFileSync(this.path, JSON.stringify(creds, null, 2), { mode: 0o600 });
    try {
      chmodSync(this.path, 0o600);
    } catch {
      // Windows: chmod is a no-op; ACLs govern access.
    }
  }
}
