import { type RouteConfig, index, route } from "@react-router/dev/routes";

export default [
  route("login", "routes/login.tsx"),
  index("routes/dashboard.tsx"),
  route("fleet", "routes/fleet/index.tsx"),
  route("fleet/nodes", "routes/fleet/nodes/index.tsx"),
  route("fleet/nodes/:id", "routes/fleet/nodes/$id.tsx"),
  route("fleet/fabrics", "routes/fleet/fabrics/index.tsx"),
  route("fleet/fabrics/:id", "routes/fleet/fabrics/$id.tsx"),
  route("library", "routes/library/index.tsx"),
  route("library/recipes", "routes/library/recipes/index.tsx"),
  route("library/recipes/:id", "routes/library/recipes/$id.tsx"),
  route("library/artifacts", "routes/library/artifacts/index.tsx"),
  route("library/transfers", "routes/library/transfers/index.tsx"),
  route("serving", "routes/serving/index.tsx"),
  route("serving/deployments", "routes/serving/deployments/index.tsx"),
  route("serving/deployments/:id", "routes/serving/deployments/$id.tsx"),
  route("benchmarks", "routes/benchmarks/index.tsx"),
  route("runs", "routes/runs/index.tsx"),
  route("runs/:id", "routes/runs/$id.tsx"),
  route("settings", "routes/settings/index.tsx"),
  route("modules", "routes/modules/index.tsx"),
] satisfies RouteConfig;
