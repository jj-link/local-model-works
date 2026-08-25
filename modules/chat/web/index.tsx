import Chat from "../../../web/app/routes/chat/index";
import type { UIModule } from "../../../web/app/module-loader";

export default {
  id: "chat",
  nav: { label: "Chat", order: 55, path: "/chat" },
  routes: [{ path: "/chat", component: Chat }],
} satisfies UIModule;
