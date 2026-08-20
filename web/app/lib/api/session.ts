import { useSyncExternalStore } from "react";
import type { components } from "~/generated/api";
import { API_BASE, getSessionState, request, setSession, storedCsrfToken, subscribeSession, type SessionState } from "./client";

export type Session = components["schemas"]["Session"];

/**
 * GET /session. A 401 or network failure resolves to null (treated as
 * signed out) so the boot path can redirect to /login.
 *
 * When `req` is given (SSR root loader) the fetch runs against the request
 * origin with its session cookie forwarded — Node's global fetch rejects
 * relative URLs. In the browser without `req` it is a same-origin fetch.
 */
export async function fetchSession(req?: Request): Promise<Session | null> {
  try {
    const url = req
      ? new URL("/api/v1/session", req.url).toString()
      : `${API_BASE}/session`;
    const headers: Record<string, string> = { accept: "application/json" };
    if (req) {
      const cookie = req.headers.get("cookie");
      if (cookie) headers.cookie = cookie;
    }
    const res = await fetch(url, { headers, credentials: "include" });
    if (!res.ok) return null;
    const s = (await res.json().catch(() => null)) as Session | null;
    if (!s || typeof s.username !== "string") return null;
    if (typeof window !== "undefined") {
      setSession({
        username: s.username,
        // /session does not re-issue the token; fall back to the one
        // captured at login and persisted in sessionStorage.
        csrfToken: s.csrf_token || storedCsrfToken(),
        expiresAt: s.expires_at,
      });
    }
    return s;
  } catch {
    return null;
  }
}

/**
 * POST /login. The public contract declares 204; the server returns the
 * session view (username + csrf_token) with 200, which is what the client
 * stores. Falls back to GET /session if the body is absent.
 */
export async function login(username: string, password: string): Promise<Session> {
  const res = await request<Session | undefined>("/login", {
    method: "POST",
    json: { username, password },
    requireAuth: false,
  });
  if (res && typeof res === "object" && "csrf_token" in res) {
    const s = res as Session;
    setSession({
      username: s.username,
      csrfToken: s.csrf_token,
      expiresAt: s.expires_at,
    });
    return s;
  }
  const s = await fetchSession();
  if (!s) throw new Error("login did not establish a session");
  return s;
}

export async function logout(): Promise<void> {
  try {
    await request<void>("/logout", { method: "POST", requireAuth: false });
  } finally {
    setSession(null);
  }
}

/**
 * Reactive session state for components. Returns the current
 * SessionState or null when signed out.
 */
export function useSession(): SessionState | null {
  return useSyncExternalStore(subscribeSession, getSessionState, getSessionState);
}
