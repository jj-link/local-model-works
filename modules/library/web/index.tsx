import Library from "../../../web/app/routes/library/index";
import Recipes from "../../../web/app/routes/library/recipes/index";
import Artifacts from "../../../web/app/routes/library/artifacts/index";
import Transfers from "../../../web/app/routes/library/transfers/index";
import RecipePage from "../../../web/app/routes/library/recipes/detail";
import type { UIModule } from "../../../web/app/module-loader";

export default {
  id: "library",
  nav: { label: "Library", order: 20, path: "/library" },
  routes: [
    { path: "/library", component: Library },
    { path: "/library/recipes", component: Recipes },
    { path: "/library/artifacts", component: Artifacts },
    { path: "/library/transfers", component: Transfers },
    { path: "/library/recipes/new", component: RecipePage },
    { path: "/library/recipes/repositories/:id", component: RecipePage },
    { path: "/library/recipes/packages/:digest", component: RecipePage },
  ],
} satisfies UIModule;
