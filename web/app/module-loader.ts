import type { ComponentType } from "react";

export type UIRoute = {
  path: string;
  component: ComponentType;
};

export type UIModule = {
  id: string;
  routes: UIRoute[];
  nav: { label: string; order: number; path: string };
};

type ModuleNamespace = { default: UIModule };

const discovered = import.meta.glob<ModuleNamespace>("../../modules/*/web/index.tsx", { eager: true });

export const uiModules = Object.values(discovered)
  .map((namespace) => namespace.default)
  .sort((left, right) => left.nav.order - right.nav.order);
