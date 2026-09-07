import { useEffect } from "react";
import {
  isRouteErrorResponse,
  Link,
  Links,
  Meta,
  Outlet,
  redirect,
  Scripts,
  ScrollRestoration,
  useLocation,
  useNavigate,
  useRouteError,
  useRouteLoaderData,
} from "react-router";
import { QueryClientProvider } from "@tanstack/react-query";
import { queryClient } from "~/lib/queries";
import { fetchSession, type Session } from "~/lib/api/session";
import { setOnReauth, setOnUnauthorized } from "~/lib/api/client";
import { eventStream } from "~/lib/events";
import { AppShell } from "~/components/app-shell";
import { ErrorState } from "~/components/empty-state";
import "./app.css";
/**
 * Document wrapper rendered around every route (and the error element) on
 * the server; the SPA fallback is prerendered from it at build time.
 */
export function Layout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <head>
        <meta charSet="utf-8" />
        <meta name="viewport" content="width=device-width, initial-scale=1" />
        <meta name="color-scheme" content="light" />
        <Meta />
        <Links />
      </head>
      <body>
        {children}
        <ScrollRestoration />
        <Scripts />
      </body>
    </html>
  );
}

export function meta() {
  return [
    { title: "Local Model Works" },
    {
      name: "description",
      content: "Control console for a self-hosted local-AI workshop.",
    },
  ];
}

interface RootData {
  session: Session | null;
  /** Present when a mutation was rejected because the one-time CSRF token
   *  is gone and the operator must re-verify before returning. */
  reauth?: string;
}

// SPA builds invoke the server loader once to prerender index.html. Runtime
// authentication belongs in clientLoader; otherwise React Router requests a
// server data stream that the static Go asset server does not implement.
export async function loader(): Promise<RootData> {
  return { session: null };
}

export async function clientLoader({ request }: { request: Request }): Promise<RootData> {
  const { pathname, searchParams } = new URL(request.url);
  // A reauth intent is only honoured when it names an in-app destination; a
  // missing/absolute value falls back to the console default.
  const requested = searchParams.get("next") ?? "";
  const reauth =
    searchParams.get("reauth") === "1" && requested.startsWith("/") && !requested.startsWith("//")
      ? requested
      : undefined;
  const session = await fetchSession();
  if (!session) {
    if (pathname !== "/login") throw redirect("/login");
    return { session, reauth };
  }
  // A signed-in operator who is here to re-verify stays on the login panel;
  // every other visit to /login goes to the console.
  if (pathname === "/login" && reauth === undefined) throw redirect("/");
  return { session, reauth };
}

clientLoader.hydrate = true as const;

// Re-check the session on every navigation: auth state (login, logout,
// expiry) changes outside the router's data flow, and the login transition
// must swap from the centered panel to the console shell.
export function shouldRevalidate() {
  return true;
}

export default function App() {
  const data = useRouteLoaderData<RootData>("root") ?? { session: null };
  const authenticated = !!data.session;
  const location = useLocation();
  return (
    <QueryClientProvider client={queryClient}>
      <AuthEffects authenticated={authenticated}>
        {authenticated && location.pathname !== "/login" ? (
          <AppShell session={data.session} />
        ) : authenticated ? (
          <ReauthSurface next={data.reauth ?? ""} routerOutlet={<Outlet />} />
        ) : (
          // The loader redirects every non-login path when signed out, so
          // only the centered login panel can render here.
          <main id="main-content" className="lmw-bg min-h-screen">
            <Outlet />
          </main>
        )}
      </AuthEffects>
    </QueryClientProvider>
  );
}

// The one-time CSRF token only comes back from a fresh sign-in, so recovery
// keeps the operator on the login panel and returns them to the page they
// were on. The rejected mutation is never retried automatically.
function ReauthSurface({ next, routerOutlet }: { next: string; routerOutlet: React.ReactNode }) {
  return (
    <main id="main-content" className="lmw-bg min-h-screen">
      <div
        role="alert"
        className="mx-auto mt-4 max-w-sm rounded border border-warn/40 bg-warn/10 px-3 py-2 text-sm text-warn"
      >
        Your session's CSRF token expired, so the last action was not applied.
        Sign in again to return to {next || "your work"}.
      </div>
      {routerOutlet}
    </main>
  );
}

function AuthEffects({ authenticated, children }: { authenticated: boolean; children: React.ReactNode }) {
  const navigate = useNavigate();
  const location = useLocation();
  useEffect(() => {
    setOnUnauthorized(() => {
      window.location.assign("/login");
    });
    // A rejected mutation because of a missing CSRF token must surface an
    // explicit re-verification that returns to this page, not the cookie
    // bounce that silently drops the operator at the Overview.
    setOnReauth(({ next }) => {
      queryClient.clear();
      const current = next || location.pathname + location.search;
      const params = new URLSearchParams({ reauth: "1" });
      if (current && current !== "/login") params.set("next", current);
      navigate(`/login?${params.toString()}`, { replace: true });
    });
    eventStream.bindQueryClient(queryClient);
    if (authenticated) eventStream.start();
    return () => {
      if (!authenticated) eventStream.stop();
    };
  }, [authenticated, location.pathname, location.search, navigate]);
  return <>{children}</>;
}
function RouteErrorPanel() {
  const error = useRouteError();
  if (isRouteErrorResponse(error)) {
    if (error.status === 404) {
      return (
        <div className="lmw-bg flex min-h-screen items-center justify-center p-4">
          <div className="lmw-panel flex w-full max-w-md flex-col items-center gap-3 px-8 py-10 text-center">
            <p className="font-display text-5xl font-semibold text-primary tnum">404</p>
            <p className="text-sm text-foreground">No panel at this address.</p>
            <Link to="/" className="mt-2 font-mono text-xs text-accent underline-offset-2 hover:underline">
              back to the workshop
            </Link>
          </div>
        </div>
      );
    }
  }
  return (
    <div className="lmw-bg flex min-h-screen items-center justify-center p-4">
      <div className="w-full max-w-md">
        <ErrorState error={error} />
      </div>
    </div>
  );
}

export function errorElement() {
  return <RouteErrorPanel />;
}
