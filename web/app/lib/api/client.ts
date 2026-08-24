import type { components } from "~/generated/api";

export type ApiErrorBody = components["schemas"]["Error"];

/** Typed API error: stable machine code plus operator message. */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly details?: Record<string, unknown>;

  constructor(status: number, body: ApiErrorBody | null, fallbackMessage?: string) {
    super(body?.message ?? fallbackMessage ?? `HTTP ${status}`);
    this.name = "ApiError";
    this.status = status;
    this.code = body?.code ?? `http.${status}`;
    this.details = body?.details;
  }
}

export const API_BASE = "/api/v1";

export interface SessionState {
  username: string;
  csrfToken: string;
  expiresAt: string;
}
// The per-session CSRF token is issued once, in the login response; the
// /session view only carries its hash. Persist it across page reloads.
const CSRF_STORAGE_KEY = "lmw.csrf";

export function storedCsrfToken(): string {
  if (typeof sessionStorage === "undefined") return "";
  return sessionStorage.getItem(CSRF_STORAGE_KEY) ?? "";
}

let session: SessionState | null = null;
const sessionListeners = new Set<() => void>();

export function setSession(s: SessionState | null): void {
  session = s;
  if (typeof sessionStorage !== "undefined") {
    if (s && s.csrfToken) sessionStorage.setItem(CSRF_STORAGE_KEY, s.csrfToken);
    else sessionStorage.removeItem(CSRF_STORAGE_KEY);
  }
  for (const l of sessionListeners) l();
}

export function subscribeSession(l: () => void): () => void {
  sessionListeners.add(l);
  return () => sessionListeners.delete(l);
}

export function getSessionState(): SessionState | null {
  return session;
}

let onUnauthorized: () => void = () => {
  if (typeof window !== "undefined" && !window.location.pathname.startsWith("/login")) {
    window.location.assign("/login");
  }
};

export function setOnUnauthorized(fn: () => void): void {
  onUnauthorized = fn;
}

/** Called by the client (and the SSE helper) on a 401. */
export function notifyUnauthorized(): void {
  onUnauthorized();
}

export interface RequestInit {
  method?: string;
  json?: unknown;
  body?: BodyInit;
  headers?: Record<string, string>;
  signal?: AbortSignal;
  /** When false, a 401 does not trigger the global login redirect. */
  requireAuth?: boolean;
}

/**
 * Authenticated fetch primitive for text, binary, multipart, and
 * response-header-sensitive operations.
 */
export async function requestRaw(path: string, init: RequestInit = {}): Promise<Response> {
  const method = (init.method ?? "GET").toUpperCase();
  const headers = new Headers(init.headers);
  let body = init.body;
  if (init.json !== undefined) {
    headers.set("content-type", "application/json");
    body = JSON.stringify(init.json);
  }
  if (method !== "GET" && method !== "HEAD" && session) {
    headers.set("x-csrf-token", session.csrfToken || storedCsrfToken());
  }
  let res: Response;
  try {
    res = await fetch(`${API_BASE}${path}`, {
      method,
      headers,
      body,
      signal: init.signal,
      credentials: "same-origin",
    });
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") throw err;
    throw new ApiError(0, { code: "network.unreachable", message: "Control plane unreachable." });
  }
  if (res.status === 401 && init.requireAuth !== false) {
    onUnauthorized();
    throw new ApiError(401, {
      code: "auth.unauthorized",
      message: "Session expired; sign in again.",
    });
  }
  if (!res.ok) {
    const parsed = await res.clone().json().catch(() => null);
    const errBody = (parsed && typeof parsed === "object" ? parsed : {}) as Partial<ApiErrorBody>;
    const code = errBody.code ?? `http.${res.status}`;
    if (res.status === 403 && code === "auth.csrf") {
      setSession(null);
      onUnauthorized();
    }
    throw new ApiError(res.status, {
      code,
      message: errBody.message ?? res.statusText,
      details: errBody.details,
    });
  }
  return res;
}

/**
 * Thin typed JSON wrapper over requestRaw.
 */
export async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await requestRaw(path, init);
  if (res.status === 204) return undefined as T;
  const ct = res.headers.get("content-type") ?? "";
  if (!ct.includes("application/json")) return null as T;
  return (await res.json().catch(() => null)) as T;
}

/** Query-string builder: omits null/undefined/empty values. */
export function qs(params: Record<string, string | number | undefined | null>): string {
  const sp = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === null || v === "") continue;
    sp.set(k, String(v));
  }
  const s = sp.toString();
  return s ? `?${s}` : "";
}

export const http = {
  get: <T>(path: string, init?: Omit<RequestInit, "method" | "json">) =>
    request<T>(path, { ...init, method: "GET" }),
  post: <T>(path: string, json?: unknown, init?: Omit<RequestInit, "method" | "json">) =>
    request<T>(path, { ...init, method: "POST", json }),
  put: <T>(path: string, json?: unknown, init?: Omit<RequestInit, "method" | "json">) =>
    request<T>(path, { ...init, method: "PUT", json }),
  del: <T>(path: string, init?: Omit<RequestInit, "method" | "json">) =>
    request<T>(path, { ...init, method: "DELETE" }),
};
