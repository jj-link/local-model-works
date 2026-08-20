import Benchmarks from "../../../web/app/routes/benchmarks/index";
import type { UIModule } from "../../../web/app/module-loader";

export default {
  id: "benchmarks",
  nav: { label: "Benchmarks", order: 40, path: "/benchmarks" },
  routes: [{ path: "/benchmarks", component: Benchmarks }],
} satisfies UIModule;
