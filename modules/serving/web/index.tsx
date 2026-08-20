import Serving from "../../../web/app/routes/serving/index";
import Deployments from "../../../web/app/routes/serving/deployments/index";
import DeploymentDetail from "../../../web/app/routes/serving/deployments/$id";
import type { UIModule } from "../../../web/app/module-loader";

export default {
  id: "serving",
  nav: { label: "Serving", order: 30, path: "/serving" },
  routes: [
    { path: "/serving", component: Serving },
    { path: "/serving/deployments", component: Deployments },
    { path: "/serving/deployments/:id", component: DeploymentDetail },
  ],
} satisfies UIModule;
