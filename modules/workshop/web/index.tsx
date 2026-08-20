import Workshop from "../../../web/app/routes/workshop/index";
import type { UIModule } from "../../../web/app/module-loader";

export default {
  id: "workshop",
  nav: { label: "Workshop", order: 45, path: "/workshop" },
  routes: [{ path: "/workshop", component: Workshop }],
} satisfies UIModule;
