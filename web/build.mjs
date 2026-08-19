// Placeholder build: the Vite app lands in the frontend phase. For now the
// embedded dist/ is the source of truth, so "build" only verifies it exists.
import { statSync } from "node:fs";

statSync(new URL("./dist/index.html", import.meta.url));
console.log("web: placeholder dist present");
