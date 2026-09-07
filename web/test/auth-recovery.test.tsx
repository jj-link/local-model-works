/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, cleanup } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { createMemoryRouter, MemoryRouter, Route, Routes, RouterProvider } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { getSessionState, request, setOnReauth, setOnUnauthorized, setSession } from "~/lib/api/client";
import App, { clientLoader } from "~/root";
import LoginRoute from "~/routes/login";
import { server } from "../msw/server";

// A valid session cookie whose one-time CSRF token has been lost (cleared
// sessionStorage / opened in a fresh tab). Every mutation is then rejected with
// 403 auth.csrf. The bug: that rejection bounced through /login, which — seeing
// the still-valid cookie — immediately redirected to the Overview, silently
// discarding the action (recipe import, launch) the operator was taking.

const validSession = { username: "admin", csrfToken: "", expiresAt: "2099-01-01T00:00:00Z" };
// The server never re-issues the CSRF token on GET /session (login only), so
// every /session stub here omits csrf_token — the real lost-token condition.
const sessionView = { username: "admin", expires_at: "2099-01-01T00:00:00Z" };
const loginView = { username: "admin", csrf_token: "reissued-csrf", expires_at: "2099-01-01T00:00:00Z" };

function sessionHandlers() {
  server.use(
    http.get("*/api/v1/session", () => HttpResponse.json(sessionView)),
    http.post("*/api/v1/login", () => HttpResponse.json(loginView)),
    http.get("*/api/v1/modules", () => HttpResponse.json([])),
    http.get("*/api/v1/system/info", () => HttpResponse.json({})),
    http.get("*/api/v1/runs", () => HttpResponse.json({ items: [] })),
    http.get("*/api/v1/deployments", () => HttpResponse.json([])),
    http.get("*/api/v1/events", () => new HttpResponse("", { status: 200 })),
  );
}

describe("CSRF-rejected mutation contract", () => {
  beforeEach(() => {
    setSession(validSession);
    window.history.pushState({}, "", "/library/recipes");
  });

  afterEach(() => {
    setSession(null);
  });

  it("routes the 403 to reauth with the current page and drops the session", async () => {
    server.use(
      http.post("*/api/v1/deployments/plan", () =>
        HttpResponse.json(
          { code: "auth.csrf", message: "missing or invalid X-CSRF-Token" },
          { status: 403 },
        ),
      ),
    );
    const onReauth = vi.fn();
    const onUnauthorized = vi.fn();
    setOnReauth(onReauth);
    setOnUnauthorized(onUnauthorized);

    await expect(
      request("/deployments/plan", { method: "POST", json: { recipe_digest: "x" } }),
    ).rejects.toMatchObject({ status: 403, code: "auth.csrf" });

    expect(onReauth).toHaveBeenCalledWith({ next: "/library/recipes" });
    expect(onUnauthorized).not.toHaveBeenCalled();
    expect(getSessionState()).toBeNull();
  });
});

describe("Login panel redirect guard", () => {
  function renderLogin(entry: string) {
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
    });
    return render(
      <QueryClientProvider client={client}>
        <MemoryRouter initialEntries={[entry]}>
          <Routes>
            <Route path="/login" element={<LoginRoute />} />
            <Route path="/" element={<div>OVERVIEW</div>} />
            <Route path="/library/recipes" element={<div>RECIPES</div>} />
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>,
    );
  }

  afterEach(() => {
    setSession(null);
  });

  it("keeps a still-valid cookie on the sign-in form when reauthing", () => {
    setSession(validSession);
    renderLogin("/login?reauth=1&next=/library/recipes");

    expect(screen.getByRole("button", { name: /sign in/i })).toBeInTheDocument();
    expect(screen.queryByText("OVERVIEW")).not.toBeInTheDocument();
  });

  it("still redirects a signed-in operator with no reauth intent", () => {
    setSession(validSession);
    renderLogin("/login");

    expect(screen.getByText("OVERVIEW")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /sign in/i })).not.toBeInTheDocument();
  });
});

describe("Reauth round trip through the app root", () => {
  // Catalog page stub: fires the same kind of mutation the import/launch
  // dialogs fire, so we can count whether it is ever replayed.
  let importCalls = 0;
  function CatalogStub() {
    return (
      <div>
        CATALOG PAGE
        <button
          type="button"
          onClick={() => {
            importCalls += 1;
            void request("/recipes/import", {
              method: "POST",
              json: { source: { type: "git", remote: "https://github.com/MiaAI-Lab/x" } },
            }).catch(() => undefined);
          }}
        >
          Import
        </button>
      </div>
    );
  }

  beforeEach(() => {
    importCalls = 0;
    sessionStorage.clear();
    setSession(validSession);
    sessionHandlers();
  });

  afterEach(() => {
    cleanup();
    setSession(null);
  });

  it("reauths after a rejected import and returns to the page without replaying it", async () => {
    let importResponses = 0;
    server.use(
      http.post("*/api/v1/recipes/import", () => {
        importResponses += 1;
        return HttpResponse.json(
          { code: "auth.csrf", message: "missing or invalid X-CSRF-Token" },
          { status: 403 },
        );
      }),
    );

    const router = createMemoryRouter(
      [
        {
          id: "root",
          element: <App />,
          loader: ({ request: req }) => clientLoader({ request: req }),
          children: [
            { path: "login", element: <LoginRoute /> },
            { path: "library/recipes", element: <CatalogStub /> },
            { path: "*", element: <div>OTHER</div> },
          ],
        },
      ],
      { initialEntries: ["/library/recipes"] },
    );
    render(<RouterProvider router={router} />);

    // Signed-in catalog page with a lost CSRF token.
    expect(await screen.findByText("CATALOG PAGE")).toBeInTheDocument();

    // The import attempt is rejected with auth.csrf — the real AuthEffects
    // handler (not a mock) must route to explicit re-verification.
    await userEvent.click(screen.getByRole("button", { name: "Import" }));

    await waitFor(() =>
      expect(router.state.location.pathname).toBe("/login"),
    );
    expect(router.state.location.search).toBe("?reauth=1&next=%2Flibrary%2Frecipes");

    // Recovery surface + sign-in form render; no Overview bounce; the import
    // was rejected exactly once and nothing has been replayed.
    expect(await screen.findByRole("alert")).toHaveTextContent(/not applied/i);
    expect(screen.getByRole("button", { name: /sign in/i })).toBeInTheDocument();
    expect(screen.queryByText("CATALOG PAGE")).not.toBeInTheDocument();
    expect(importCalls).toBe(1);
    expect(importResponses).toBe(1);

    // Re-verify. LoginRoute's own guard must not have bounced the still-valid
    // cookie away — the form must be here and functional.
    await userEvent.type(screen.getByLabelText("Username"), "admin");
    await userEvent.type(
      document.querySelector('input[name="password"]') as HTMLInputElement,
      "correct horse battery staple",
    );
    await userEvent.click(screen.getByRole("button", { name: /sign in/i }));

    // Returned to the catalog page with a reissued token; the import was
    // never automatically retried.
    expect(await screen.findByText("CATALOG PAGE")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/library/recipes");
    expect(importCalls).toBe(1);
    expect(importResponses).toBe(1);
    expect(getSessionState()?.csrfToken).toBe("reissued-csrf");
  });

  it("the loader keeps a valid session on /login only for reauth, else redirects", async () => {
    await expect(
      clientLoader({ request: new Request("https://lmw.test/login?reauth=1&next=/library/recipes") }),
    ).resolves.toEqual({ session: sessionView, reauth: "/library/recipes" });

    const redirect = await clientLoader({
      request: new Request("https://lmw.test/login"),
    }).then(
      () => null,
      (thrown: unknown) => thrown,
    );
    expect(redirect).not.toBeNull();
  });
});
