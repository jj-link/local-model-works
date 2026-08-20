import { useState } from "react";
import { Navigate, useLocation, useNavigate } from "react-router";
import { useQueryClient } from "@tanstack/react-query";
import { Loader2 } from "lucide-react";
import { ApiError, setOnUnauthorized } from "~/lib/api/client";
import { login, useSession } from "~/lib/api/session";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { Label } from "~/components/ui/label";

/**
 * Single-operator login. On success: fetch the session (sets csrf) and
 * continue to the intended location.
 */
export default function LoginRoute() {
  const navigate = useNavigate();
  const location = useLocation();
  const queryClient = useQueryClient();
  const session = useSession();

  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const from = (location.state as { from?: string } | null)?.from;
  const target = from && from.startsWith("/") ? from : "/";

  // Already signed in? Straight to the console.
  if (session) {
    return <Navigate to={target} replace />;
  }
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!username || !password) return;
    setBusy(true);
    setError(null);
    try {
      await login(username, password);
      setOnUnauthorized(() => {
        queryClient.clear();
        navigate("/login", { replace: true });
      });
      navigate(target, { replace: true });
    } catch (err) {
      if (err instanceof ApiError) {
        setError(err.message || `login failed (${err.code})`);
      } else {
        setError(err instanceof Error ? err.message : "login failed");
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="lmw-bg flex min-h-screen items-center justify-center p-4">
      <main className="lmw-panel w-full max-w-sm rounded p-6">
        <header className="mb-6">
          <p className="lmw-label">local model works</p>
          <h1 className="mt-1 font-display text-2xl font-semibold tracking-wide text-foreground">
            Console
          </h1>
          <p className="mt-1 font-mono text-xs text-muted">
            single-operator · session cookie + csrf
          </p>
        </header>

        <form onSubmit={(e) => void submit(e)} className="grid gap-4" noValidate>
          <div className="grid gap-2">
            <Label htmlFor="login-user">Username</Label>
            <Input
              id="login-user"
              name="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              autoComplete="username"
              autoFocus
              spellCheck={false}
            />
          </div>
          <div className="grid gap-2">
            <Input
              id="login-pass"
              name="password"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="current-password"
            />
          </div>

          {error ? (
            <p
              role="alert"
              className="rounded border border-fault/50 bg-fault/10 px-3 py-2 font-mono text-xs text-fault"
            >
              {error}
            </p>
          ) : null}

          <Button type="submit" disabled={busy || !username || !password} className="w-full">
            {busy ? <Loader2 className="h-4 w-4 animate-spin" aria-hidden /> : "Sign in"}
          </Button>
        </form>
      </main>
    </div>
  );
}
