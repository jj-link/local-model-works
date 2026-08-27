import type { Deployment, Secret } from "~/lib/api";

export type ProviderRef =
  | { source: "lmw"; deployment_id: string; model: string }
  | { source: "external"; backend: "claude" | "codex" | "claude-ds"; model: string; base_url?: string; secret_name: string };

export interface ProviderOption {
  key: string;
  label: string;
  detail: string;
  ref: ProviderRef;
  available: boolean;
}

export interface RoleGroup {
  id: string;
  label: string;
  roles: ReadonlyArray<{ id: string; label: string }>;
}

export const ROLE_GROUPS: readonly RoleGroup[] = [
  { id: "defaults", label: "Project", roles: [{ id: "default", label: "Project default" }] },
  { id: "idea", label: "Idea factory", roles: [
    { id: "idea-intake-dispatcher", label: "Intake dispatcher" },
    { id: "idea-dispatcher", label: "Idea dispatcher" },
    { id: "idea-creator", label: "Creator" },
    { id: "idea-refiner", label: "Refiner" },
    { id: "idea-reviewer", label: "Reviewer" },
  ] },
  { id: "proposal", label: "Proposal factory", roles: [
    { id: "proposal-dispatcher", label: "Proposal dispatcher" },
    { id: "proposal-refiner", label: "Refiner" },
    { id: "proposal-reviewer", label: "Reviewer" },
  ] },
  { id: "literature", label: "Deep literature", roles: [
    { id: "deep-lit-dispatcher", label: "Dispatcher" },
    { id: "deep-lit-reader", label: "Reader / coordinator" },
  ] },
  { id: "experiment", label: "Experiment factory", roles: [
    { id: "experiment-dispatcher", label: "Experiment dispatcher" },
    { id: "experiment-scientist", label: "Scientist" },
    { id: "experiment-screener", label: "Screener" },
    { id: "experiment-coder", label: "Coder" },
    { id: "experiment-auditor", label: "Auditor" },
    { id: "experiment-reviewer", label: "Reviewer" },
  ] },
  { id: "paper", label: "Paper factory", roles: [
    { id: "paper-dispatcher", label: "Paper dispatcher" },
    { id: "paper-writer", label: "Writer" },
    { id: "paper-drawer", label: "Figure drawer" },
    { id: "paper-auditor-evidence", label: "Evidence auditor" },
    { id: "paper-auditor-citations", label: "Citation auditor" },
    { id: "paper-auditor-reproducibility", label: "Reproducibility auditor" },
    { id: "paper-rhetorician", label: "Rhetorician" },
    { id: "paper-reviewer", label: "Reviewer" },
    { id: "paper-killer-reviewer", label: "Killer reviewer" },
    { id: "paper-area-chair", label: "Area chair" },
    { id: "paper-compiler", label: "Compiler" },
  ] },
] as const;

export const GRAPH_ROLE_BY_NODE: Readonly<Record<string, string>> = {
  "idea.creator": "idea-creator",
  "idea.refiner": "idea-refiner",
  "idea.reviewer": "idea-reviewer",
  "proposal.refiner": "proposal-refiner",
  "proposal.reviewer": "proposal-reviewer",
  "literature.reader": "deep-lit-reader",
  "experiment.scientist": "experiment-scientist",
  "experiment.coder": "experiment-coder",
  "experiment.auditor": "experiment-auditor",
  "experiment.reviewer": "experiment-reviewer",
  "paper.area-chair": "paper-area-chair",
  "paper.rhetorician": "paper-rhetorician",
  "paper.killer": "paper-killer-reviewer",
  "paper.writer": "paper-writer",
  "paper.evidence": "paper-auditor-evidence",
  "paper.citations": "paper-auditor-citations",
  "paper.reproducibility": "paper-auditor-reproducibility",
  "paper.reviewer": "paper-reviewer",
};

interface ExternalProviderSetting {
  name: string;
  backend: "claude" | "codex" | "claude-ds";
  model: string;
  base_url?: string;
  secret_name: string;
}

export function providerKey(ref: ProviderRef): string {
  return ref.source === "lmw"
    ? `lmw|${ref.deployment_id}|${ref.model}`
    : `external|${ref.backend}|${ref.model}|${ref.base_url ?? ""}|${ref.secret_name}`;
}

export function providerLabel(ref: ProviderRef): string {
  return ref.source === "lmw" ? ref.model : `${ref.backend} / ${ref.model}`;
}

export function readProviderMap(value: unknown): Record<string, ProviderRef> {
  if (!value || typeof value !== "object" || Array.isArray(value)) return {};
  return value as Record<string, ProviderRef>;
}

export function readFallbackMap(value: unknown): Record<string, ProviderRef[]> {
  if (!value || typeof value !== "object" || Array.isArray(value)) return {};
  return value as Record<string, ProviderRef[]>;
}

export function readDefaultRoleAssignments(settings: Record<string, unknown> | undefined): Record<string, ProviderRef> {
  return readProviderMap(settings?.default_role_assignments);
}

export function withRoutingConfig<T extends object>(
  config: T,
  roles: Record<string, ProviderRef>,
  fallbacks: Record<string, ProviderRef[]>,
): T {
  return { ...config, roles, fallbacks } as T;
}

export function effectiveProvider(
  role: string,
  roles: Record<string, ProviderRef>,
  defaults: Record<string, ProviderRef>,
): { ref: ProviderRef | undefined; source: "override" | "project-default" | "module-default" | "unassigned" } {
  if (roles[role]) return { ref: roles[role], source: "override" };
  if (role !== "default" && roles.default) return { ref: roles.default, source: "project-default" };
  if (defaults[role]) return { ref: defaults[role], source: "module-default" };
  if (role !== "default" && defaults.default) return { ref: defaults.default, source: "module-default" };
  return { ref: undefined, source: "unassigned" };
}

export function effectiveFallbacks(
  role: string,
  fallbacks: Record<string, ProviderRef[]>,
): { refs: ProviderRef[]; inherited: boolean } {
  if (fallbacks[role]?.length) return { refs: fallbacks[role], inherited: false };
  if (role !== "default" && fallbacks.default?.length) return { refs: fallbacks.default, inherited: true };
  return { refs: [], inherited: role !== "default" };
}

export function buildProviderCatalog(
  deployments: Deployment[],
  settings: Record<string, unknown> | undefined,
  secrets: Secret[],
): ProviderOption[] {
  const options: ProviderOption[] = [];
  for (const deployment of deployments) {
    const model = deployment.endpoint?.model;
    if (!model) continue;
    const ref: ProviderRef = { source: "lmw", deployment_id: deployment.id, model };
    options.push({
      key: providerKey(ref),
      label: `${model} · ${deployment.recipe_name ?? deployment.id.slice(0, 8)}`,
      detail: `LMW deployment · ${deployment.observed_state}`,
      ref,
      available: deployment.observed_state === "healthy",
    });
  }
  const secretNames = new Set(secrets.map((secret) => secret.name));
  const configured = Array.isArray(settings?.external_providers)
    ? settings.external_providers as ExternalProviderSetting[]
    : [];
  for (const provider of configured) {
    if (!provider?.name || !provider.backend || !provider.model || !provider.secret_name) continue;
    const ref: ProviderRef = {
      source: "external",
      backend: provider.backend,
      model: provider.model,
      ...(provider.base_url ? { base_url: provider.base_url } : {}),
      secret_name: provider.secret_name,
    };
    options.push({
      key: providerKey(ref),
      label: `${provider.name} · ${provider.model}`,
      detail: `${provider.backend} · secret ${provider.secret_name}${secretNames.has(provider.secret_name) ? "" : " missing"}`,
      ref,
      available: secretNames.has(provider.secret_name),
    });
  }
  return options.sort((left, right) => left.label.localeCompare(right.label));
}

export function withConfiguredProviders(catalog: ProviderOption[], refs: Array<ProviderRef | undefined>): ProviderOption[] {
  const byKey = new Map(catalog.map((option) => [option.key, option]));
  for (const ref of refs) {
    if (!ref) continue;
    const key = providerKey(ref);
    if (!byKey.has(key)) {
      byKey.set(key, {
        key,
        label: `${providerLabel(ref)} · unavailable`,
        detail: "Configured provider is not currently available",
        ref,
        available: false,
      });
    }
  }
  return [...byKey.values()];
}
