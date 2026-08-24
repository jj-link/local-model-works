import AutoResearch from "../../../web/app/routes/autoresearch/index";
import type { UIModule } from "../../../web/app/module-loader";

export default {
  id: "autoresearch",
  nav: { label: "AutoResearch", order: 45, path: "/autoresearch" },
  routes: [{ path: "/autoresearch", component: AutoResearch }],
} satisfies UIModule;
