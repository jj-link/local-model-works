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
  useRouteError,
  useRouteLoaderData,
} from "react-router";
import { QueryClientProvider } from "@tanstack/react-query";
import { queryClient } from "~/lib/queries";
import { fetchSession, type Session } from "~/lib/api/session";
import { setOnUnauthorized } from "~/lib/api/client";
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
        <meta name="color-scheme" content="dark" />
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
}

export async function loader({ request }: { request: Request }): Promise<RootData> {
  // SSR-safe: on the server the check runs against the same origin (the dev
  // proxy or the control plane itself) with the request's session cookie;
  // on the client it is the usual same-origin fetch.
  const { pathname } = new URL(request.url);
  // Build-time prerender of the SPA fallback document (the framework flags
  // the request with this header and renders down to the root): emit a
  // plain shell — no session check, no redirects — so the static
  // index.html hydrates and the client router takes over.
  if (request.headers.get("X-React-Router-SPA-Mode") === "yes") {
    return { session: null };
  }
  const session = await fetchSession(request);
  if (!session) {
    if (pathname !== "/login") throw redirect("/login");
    return { session };
  }
  if (pathname === "/login") throw redirect("/");
  return { session };
}

// Re-check the session on every navigation: auth state (login, logout,
// expiry) changes outside the router's data flow, and the login transition
// must swap from the centered panel to the console shell.
export function shouldRevalidate() {
  return true;
}

export default function App() {
  const data = useRouteLoaderData<RootData>("root") ?? { session: null };
  const authenticated = !!data.session;
  return (
    <QueryClientProvider client={queryClient}>
      <AuthEffects authenticated={authenticated}>
        {authenticated ? (
          <AppShell session={data.session} />
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

function AuthEffects({ authenticated, children }: { authenticated: boolean; children: React.ReactNode }) {
  useEffect(() => {
    setOnUnauthorized(() => {
      window.location.assign("/login");
    });
    eventStream.bindQueryClient(queryClient);
    if (authenticated) eventStream.start();
    return () => {
      if (!authenticated) eventStream.stop();
    };
  }, [authenticated]);
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
