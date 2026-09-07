import AIAssistanceSettings from "../../../web/app/routes/settings/ai-assistance";
import type { UIModule } from "../../../web/app/module-loader";

export default {
  id: "settings",
  nav: { label: "Settings", order: 90, path: "/settings/ai-assistance" },
  routes: [{ path: "/settings/ai-assistance", component: AIAssistanceSettings }],
} satisfies UIModule;
