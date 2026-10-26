import { existsSync, mkdirSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";

// 「按用户」的模型偏好。
//
// 用户在私聊里发 `/use-model <provider>/<model>` 时落盘，之后**这个用户**在本
// 节点发起的每一轮都用这个模型。
//
// 为什么键是 fromUserId 而不是「节点全局」：一台节点经常同时挂在多个人、多个
// 讨论组角色的会话里。A 把模型换成 glm-5.3 不该顺手把 B 手里的 claude 也换掉。
// 融云每条消息都带 fromUserId，天然就是这把钥匙。
//
// 为什么落盘而不是只放内存：supervisor 会因为升级 / 崩溃自愈 / `clawmessenger
// restart` 反复拉起子进程。内存版一重启就退回节点默认，用户会以为「我设的模型
// 自己变了」——这种「改了但没保住」比「不让改」更伤。
//
// 为什么文件名带 nodeId：supervisor 一节点一子进程，但它们共用同一个 home。
// 不分文件就有多进程同时读改写、互相把对方的表覆盖掉的风险（同
// processed-uids 的处理方式）。
export function defaultUserModelsPath(nodeId?: string): string {
  const suffix = nodeId ? `-${nodeId.replace(/[^A-Za-z0-9._-]/g, "_")}` : "";
  return join(homedir(), ".clawmessenger", `user-models${suffix}.json`);
}

export interface UserModelStore {
  /** 该用户显式选过的模型 route；undefined = 跟随节点默认。 */
  get(userId: string): string | undefined;
  /** route 传 null 表示清掉偏好、恢复节点默认。 */
  set(userId: string, route: string | null): void;
  snapshot(): Record<string, string>;
}

/** 内存实现：测试与「没有可写 home」的降级路径用。 */
export class MemoryUserModelStore implements UserModelStore {
  private readonly table: Record<string, string>;

  constructor(initial: Record<string, string> = {}) {
    this.table = { ...initial };
  }

  get(userId: string): string | undefined {
    return this.table[userId];
  }

  set(userId: string, route: string | null): void {
    if (route === null || !route.trim()) delete this.table[userId];
    else this.table[userId] = route.trim();
  }

  snapshot(): Record<string, string> {
    return { ...this.table };
  }
}

export class FileUserModelStore implements UserModelStore {
  // 懒加载 + 缓存：私聊是热路径，每条消息都 stat 一次磁盘不值得。写操作同步
  // 落盘（低频、且用户就在等回执），所以缓存永远与磁盘一致。
  private cache: Record<string, string> | null = null;

  constructor(private readonly path: string) {}

  private load(): Record<string, string> {
    if (this.cache) return this.cache;
    const table: Record<string, string> = {};
    let parsed: unknown = null;
    try {
      if (existsSync(this.path)) parsed = JSON.parse(readFileSync(this.path, "utf8"));
    } catch {
      // 半截 JSON / 手工改坏：当空表处理，下一次写会覆盖成合法内容。
      // 这里绝不能抛 —— 抛了就等于「文件坏过一次，这台节点的切模型永久报错」。
      parsed = null;
    }
    if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
      for (const [key, value] of Object.entries(parsed as Record<string, unknown>)) {
        if (typeof value === "string" && value.trim()) table[key] = value.trim();
      }
    }
    this.cache = table;
    return table;
  }

  get(userId: string): string | undefined {
    return this.load()[userId];
  }

  set(userId: string, route: string | null): void {
    const table = this.load();
    if (route === null || !route.trim()) delete table[userId];
    else table[userId] = route.trim();
    this.persist(table);
  }

  snapshot(): Record<string, string> {
    return { ...this.load() };
  }

  private persist(table: Record<string, string>): void {
    try {
      mkdirSync(dirname(this.path), { recursive: true });
      // 先写临时文件再 rename：直接覆盖时若进程正好被杀，会留下半截 JSON，
      // 下次启动把整张表读丢 —— 一个人的偏好丢失会连累所有人的偏好一起丢。
      // rename 在同目录内是原子的。
      const tmp = `${this.path}.tmp`;
      writeFileSync(tmp, JSON.stringify(table, null, 2), { mode: 0o600 });
      renameSync(tmp, this.path);
    } catch {
      // 只读 home / 磁盘满：内存里已经生效（本轮及之后都用新模型），只是重启
      // 会回退到节点默认。不能因为落盘失败就打断用户的消息。
    }
  }
}
