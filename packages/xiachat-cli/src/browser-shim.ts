// Minimal browser globals for @rongcloud/imlib-next (browser-built) under
// Node. Covers exactly what the engine touches at init/connect/send time:
// `window` (Electron context-bridge probe, online/offline listeners),
// localStorage (KV cache), location.protocol, and an XMLHttpRequest backed
// by fetch. Not a general-purpose polyfill; anything beyond these code
// paths will throw loudly instead of silently misbehaving.

export function installBrowserShim(): void {
  const g = globalThis as typeof globalThis & {
    window?: unknown;
    localStorage?: unknown;
    navigator?: unknown;
    location?: { protocol: string; host: string };
    XMLHttpRequest?: unknown;
  };
  if (typeof g.window !== "undefined") return;

  const noop = () => {};

  const store = new Map<string, string>();
  const localStorageShim = {
    getItem: (k: string) => (store.has(k) ? store.get(k)! : null),
    setItem: (k: string, v: string) => void store.set(k, String(v)),
    removeItem: (k: string) => void store.delete(k),
    clear: () => store.clear(),
    key: (i: number) => [...store.keys()][i] ?? null,
    get length() {
      return store.size;
    },
  };

  const windowShim = Object.assign(Object.create(null), {
    addEventListener: noop,
    removeEventListener: noop,
    localStorage: localStorageShim,
    navigator: { userAgent: "node", onLine: true },
    location: { protocol: "https:", host: "localhost" },
    XMLHttpRequest: FetchXHR,
  });

  g.window = windowShim;
  if (typeof g.localStorage === "undefined") g.localStorage = localStorageShim;
  if (typeof g.navigator === "undefined") g.navigator = windowShim.navigator;
  if (typeof g.location === "undefined") g.location = windowShim.location;
  if (typeof g.XMLHttpRequest === "undefined") g.XMLHttpRequest = FetchXHR as unknown as typeof XMLHttpRequest;
}

// fetch-backed XMLHttpRequest subset: open/setRequestHeader/send/abort,
// onreadystatechange with readyState 2/4, status, responseText,
// getResponseHeader, HEADERS_RECEIVED/DONE constants.
class FetchXHR {
  static readonly UNSENT = 0;
  static readonly OPENED = 1;
  static readonly HEADERS_RECEIVED = 2;
  static readonly LOADING = 3;
  static readonly DONE = 4;

  readonly UNSENT = 0;
  readonly OPENED = 1;
  readonly HEADERS_RECEIVED = 2;
  readonly LOADING = 3;
  readonly DONE = 4;

  readyState = 0;  status = 0;
  responseText = "";
  onreadystatechange: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onloadend: (() => void) | null = null;

  private method = "GET";
  private url = "";
  private headers: Record<string, string> = {};
  private aborted = false;
  private responseHeaders = new Map<string, string>();

  open(method: string, url: string): void {
    this.method = method;
    this.url = url;
    this.readyState = 1;
  }

  setRequestHeader(name: string, value: string): void {
    this.headers[name] = value;
  }

  getResponseHeader(name: string): string | null {
    return this.responseHeaders.get(name.toLowerCase()) ?? null;
  }

  abort(): void {
    this.aborted = true;
  }

  async send(body?: string): Promise<void> {
    try {
      const res = await fetch(this.url, {
        method: this.method,
        headers: this.headers,
        body: body !== undefined ? body : undefined,
      });
      if (this.aborted) return;
      for (const [k, v] of res.headers) this.responseHeaders.set(k.toLowerCase(), v);
      this.status = res.status;
      this.readyState = 2;
      this.onreadystatechange?.();
      if (this.aborted) return;
      this.responseText = await res.text();
      this.readyState = 4;
      this.onreadystatechange?.();
      this.onloadend?.();
    } catch {
      if (this.aborted) return;
      this.onerror?.();
      this.onloadend?.();
    }
  }
}
