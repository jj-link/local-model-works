import { type RouteConfig, index, route } from "@react-router/dev/routes";

export default [
  route("login", "routes/login.tsx"),
  index("routes/dashboard.tsx"),
  route("*", "routes/module-host.tsx"),
] satisfies RouteConfig;
