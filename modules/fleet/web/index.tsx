import Fleet from "../../../web/app/routes/fleet/index";
import Nodes from "../../../web/app/routes/fleet/nodes/index";
import NodeDetail from "../../../web/app/routes/fleet/nodes/$id";
import Fabrics from "../../../web/app/routes/fleet/fabrics/index";
import FabricDetail from "../../../web/app/routes/fleet/fabrics/$id";
import type { UIModule } from "../../../web/app/module-loader";

export default {
  id: "fleet",
  nav: { label: "Fleet", order: 10, path: "/fleet" },
  routes: [
    { path: "/fleet", component: Fleet },
    { path: "/fleet/nodes", component: Nodes },
    { path: "/fleet/nodes/:id", component: NodeDetail },
    { path: "/fleet/fabrics", component: Fabrics },
    { path: "/fleet/fabrics/:id", component: FabricDetail },
  ],
} satisfies UIModule;
