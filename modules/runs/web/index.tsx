import Runs from "../../../web/app/routes/runs/index";
import RunDetail from "../../../web/app/routes/runs/$id";
import type { UIModule } from "../../../web/app/module-loader";

export default {
  id: "runs",
  nav: { label: "Runs", order: 50, path: "/runs" },
  routes: [
    { path: "/runs", component: Runs },
    { path: "/runs/:id", component: RunDetail },
  ],
} satisfies UIModule;
