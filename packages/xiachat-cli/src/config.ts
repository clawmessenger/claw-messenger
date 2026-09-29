import { homedir } from "node:os";
import { join } from "node:path";

export function defaultKeystorePath(): string {
  return join(homedir(), ".xiachat", "credentials.json");
}

export const DEFAULT_SERVER_URL = "http://localhost:8080";
