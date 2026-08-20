// Copies the built SPA into internal/server/web/dist so the Go binary
// embeds the current UI (//go:embed all:web/dist). `react-router build`
// emits the client bundle at build/client (SPA mode); locate its
// index.html and sync that directory.
import { cpSync, existsSync, mkdirSync, rmSync, readdirSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const dist = resolve(here, "../build");
const target = resolve(here, "../../internal/server/web/dist");

function findIndexHtml(dir, depth) {
  if (depth > 2) return null;
  const direct = join(dir, "index.html");
  if (existsSync(direct)) return direct;
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    if (!entry.isDirectory()) continue;
    const found = findIndexHtml(join(dir, entry.name), depth + 1);
    if (found) return found;
  }
  return null;
}

if (!existsSync(dist)) {
  console.error("sync-embed: build/ missing; run `npm run build` first");
  process.exit(1);
}
const index = findIndexHtml(dist, 0);
if (!index) {
  console.error("sync-embed: no built index.html found under build/");
}
const source = dirname(index);
rmSync(target, { recursive: true, force: true });
mkdirSync(target, { recursive: true });
cpSync(source, target, { recursive: true });
console.log(`sync-embed: ${source} -> ${target}`);
