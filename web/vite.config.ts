import { fileURLToPath } from "node:url";
import { reactRouter } from "@react-router/dev/vite";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig, loadEnv, type ProxyOptions } from "vite";

// The dev/preview proxy forwards /api to the Go control plane. Override the
// target via VITE_PROXY_TARGET in .env.local or the process environment.
export default defineConfig(async ({ mode }) => {
  const env = loadEnv(mode, process.cwd(), "VITE_");
  const proxyTarget = env.VITE_PROXY_TARGET ?? "http://127.0.0.1:9001";

  const apiProxy: ProxyOptions = {
    target: proxyTarget,
    changeOrigin: true,
    // WSL loopback connects to a dead port can stall; bound the wait so a
    // missing control plane surfaces as a fast 502.
    timeout: 5000,
    // Flush SSE frames through the dev proxy without buffering.
    configure: (proxy) => {
      proxy.on("proxyRes", (proxyRes, _req, res) => {
        const ct = proxyRes.headers["content-type"];
        if (typeof ct === "string" && ct.includes("text/event-stream")) {
          res?.flushHeaders?.();
        }
      });
      // Answer with a structured 502 when the control plane is down instead
      // of dropping the socket, so SSR and the app fail fast.
      proxy.on("error", (_err, _req, res) => {
        const out = res as {
          headersSent?: boolean;
          writeHead?: (code: number, headers: Record<string, string>) => void;
          end?: (body: string) => void;
          socket?: { destroy?: () => void };
        };
        if (!out.headersSent && out.writeHead && out.end) {
          out.writeHead(502, { "content-type": "application/json" });
          out.end(JSON.stringify({ code: "proxy.unavailable", message: "Control plane unreachable." }));
        } else {
          out.socket?.destroy?.();
        }
      });
    },
  };

  return {
    base: "/",
    // SPA mode (see react-router.config.ts): the Go server embeds the built
    // client bundle and serves it statically; the control plane is reached
    // over /api/v1. The document shell comes from the root Layout.
    plugins: [tailwindcss(), reactRouter()],
    resolve: {
      alias: { "~": fileURLToPath(new URL("./app", import.meta.url)) },
    },
    server: {
      port: 5173,
      proxy: { "/api": apiProxy },
    },
    preview: {
      port: 5173,
      proxy: { "/api": apiProxy },
    },
  };
});
