import Library from "../../../web/app/routes/library/index";
import Recipes from "../../../web/app/routes/library/recipes/index";
import RecipeDetail from "../../../web/app/routes/library/recipes/$id";
import Artifacts from "../../../web/app/routes/library/artifacts/index";
import Transfers from "../../../web/app/routes/library/transfers/index";
import Builder from "../../../web/app/routes/library/builder/index";
import Draft from "../../../web/app/routes/library/builder/$id";
import type { UIModule } from "../../../web/app/module-loader";

export default {
  id: "library",
  nav: { label: "Library", order: 20, path: "/library" },
  routes: [
    { path: "/library", component: Library },
    { path: "/library/recipes", component: Recipes },
    { path: "/library/recipes/:id", component: RecipeDetail },
    { path: "/library/artifacts", component: Artifacts },
    { path: "/library/transfers", component: Transfers },
    { path: "/library/builder", component: Builder },
    { path: "/library/builder/:id", component: Draft },
  ],
} satisfies UIModule;
