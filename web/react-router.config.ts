import type { Config } from "@react-router/dev/config";

export default {
  // The production deployment embeds the built client bundle in the Go
  // server and serves it statically; the control plane is reached over
  // /api/v1. SPA mode prerenders the document shell (root Layout) as the
  // build's index.html and drops the server bundle.
  ssr: false,
} satisfies Config;
