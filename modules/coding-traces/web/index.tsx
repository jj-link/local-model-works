import CodingTracesRoute, { CodingTraceDetailRoute, SweGymReplicationDetailRoute } from "../../../web/app/routes/coding-traces/index";
import type { UIModule } from "../../../web/app/module-loader";

export default {
  id: "coding-traces",
  nav: { label: "Coding Traces", order: 60, path: "/coding-traces" },
  routes: [
    { path: "/coding-traces", component: CodingTracesRoute },
    { path: "/coding-traces/replications/:id", component: SweGymReplicationDetailRoute },
    { path: "/coding-traces/:id", component: CodingTraceDetailRoute },
  ],
} satisfies UIModule;
