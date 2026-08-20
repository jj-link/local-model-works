import Settings from "../../../web/app/routes/settings/index";
import Modules from "../../../web/app/routes/modules/index";
import type { UIModule } from "../../../web/app/module-loader";

export default {
  id: "settings",
  nav: { label: "Settings", order: 60, path: "/settings" },
  routes: [
    { path: "/settings", component: Settings },
    { path: "/modules", component: Modules },
  ],
} satisfies UIModule;
