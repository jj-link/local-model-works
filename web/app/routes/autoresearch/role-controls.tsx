import { useEffect, useMemo, useState } from "react";
import { Plus, Save, Shield, X } from "lucide-react";
import { Button } from "~/components/ui/button";
import type { AutoResearchProject } from "~/lib/api";
import {
  effectiveFallbacks,
  effectiveProvider,
  providerKey,
  providerLabel,
  readFallbackMap,
  readProviderMap,
  ROLE_GROUPS,
  withConfiguredProviders,
  withRoutingConfig,
  type ProviderOption,
  type ProviderRef,
} from "./routing";

const ADVISOR_ROLES = [
  "idea-creator", "idea-refiner", "idea-reviewer", "proposal-refiner", "proposal-reviewer",
  "deep-lit-reader", "experiment-scientist", "experiment-coder", "experiment-auditor", "experiment-reviewer",
  "paper-writer", "paper-auditor-evidence", "paper-auditor-citations", "paper-auditor-reproducibility",
  "paper-rhetorician", "paper-reviewer", "paper-killer-reviewer", "paper-area-chair",
] as const;

type AdvisorConfig = { enabled: boolean; backlog: "off" | 1 | 3 | 5; provider?: ProviderRef };
type SettingsSection = "routing" | "advisors" | "paper";

interface RoleControlsProps {
  project: AutoResearchProject;
  providers: ProviderOption[];
  defaults: Record<string, ProviderRef>;
  saving: boolean;
  error: string | null;
  onSaveConfig: (config: AutoResearchProject["config"]) => void;
}

function ProviderOptions({ options, selected }: { options: ProviderOption[]; selected?: string }) {
  return <>{options.map((option) => <option key={option.key} value={option.key} disabled={!option.available && option.key !== selected}>{option.label}{option.available ? "" : " (unavailable)"}</option>)}</>;
}

function FallbackEditor({
  role,
  keys,
  inherited,
  options,
  onChange,
  onInheritedChange,
}: {
  role: string;
  keys: string[];
  inherited: boolean;
  options: ProviderOption[];
  onChange: (keys: string[]) => void;
  onInheritedChange: (inherited: boolean) => void;
}) {
  return <div className="arf-role-fallbacks">
    {role !== "default" ? <label><input type="checkbox" checked={inherited} onChange={(event) => onInheritedChange(event.target.checked)} /> inherit project fallbacks</label> : null}
    {!inherited ? <>
      {keys.map((key, index) => <div key={`${index}-${key}`}>
        <select aria-label={`${role} fallback ${index + 1}`} value={key} onChange={(event) => onChange(keys.map((item, itemIndex) => itemIndex === index ? event.target.value : item))}>
          <option value="">Select provider</option>
          <ProviderOptions options={options} selected={key} />
        </select>
        <button type="button" aria-label={`Remove ${role} fallback ${index + 1}`} onClick={() => onChange(keys.filter((_, itemIndex) => itemIndex !== index))}><X aria-hidden /></button>
      </div>)}
      <button type="button" className="arf-routing-add" onClick={() => onChange([...keys, ""])}><Plus aria-hidden /> Add fallback</button>
    </> : null}
  </div>;
}

export function RoleControls({ project, providers, defaults, saving, error, onSaveConfig }: RoleControlsProps) {
  const [section, setSection] = useState<SettingsSection>("routing");
  const [roles, setRoles] = useState<Record<string, ProviderRef>>({});
  const [fallbacks, setFallbacks] = useState<Record<string, ProviderRef[]>>({});
  const [advisors, setAdvisors] = useState<Record<string, AdvisorConfig>>({});
  const [maxRounds, setMaxRounds] = useState(project.config.paper_max_rounds);
  const [selectedRoles, setSelectedRoles] = useState<Set<string>>(new Set());
  const [bulkProvider, setBulkProvider] = useState("inherit");
  const [expandedFallbacks, setExpandedFallbacks] = useState<Set<string>>(new Set());

  useEffect(() => {
    setRoles(readProviderMap(project.config.roles));
    setFallbacks(readFallbackMap(project.config.fallbacks));
    const next: Record<string, AdvisorConfig> = {};
    for (const role of ADVISOR_ROLES) {
      const configured = project.config.advisors?.[role] as AdvisorConfig | undefined;
      next[role] = { enabled: configured?.enabled ?? false, backlog: configured?.backlog ?? 1, ...(configured?.provider ? { provider: configured.provider } : {}) };
    }
    setAdvisors(next);
    setMaxRounds(project.config.paper_max_rounds);
    setSelectedRoles(new Set());
    setExpandedFallbacks(new Set());
  }, [project]);

  const configuredRefs = useMemo(() => [
    ...Object.values(roles),
    ...Object.values(fallbacks).flat(),
    ...Object.values(advisors).map((advisor) => advisor.provider),
    ...Object.values(defaults),
  ], [advisors, defaults, fallbacks, roles]);
  const options = useMemo(() => withConfiguredProviders(providers, configuredRefs), [configuredRefs, providers]);
  const byKey = useMemo(() => new Map(options.map((option) => [option.key, option.ref])), [options]);
  const allRoleIds = useMemo(() => ROLE_GROUPS.flatMap((group) => group.roles.map((role) => role.id)), []);
  const routingValidationError = useMemo(() => {
    for (const [role, refs] of Object.entries(fallbacks)) {
      const keys = refs.map(providerKey);
      if (new Set(keys).size !== keys.length) return `${role}: fallback providers must be unique.`;
      if (roles[role] && keys.includes(providerKey(roles[role]))) return `${role}: primary provider cannot also be a fallback.`;
    }
    return null;
  }, [fallbacks, roles]);

  const setRoleProvider = (role: string, key: string) => setRoles((current) => {
    const next = { ...current };
    if (key === "inherit") delete next[role];
    else if (byKey.get(key)) next[role] = byKey.get(key) as ProviderRef;
    return next;
  });

  const saveRouting = () => onSaveConfig(withRoutingConfig(project.config, roles, fallbacks));
  const saveAdvisors = () => onSaveConfig({ ...project.config, advisors } as unknown as AutoResearchProject["config"]);
  const savePaper = () => onSaveConfig({ ...project.config, paper_max_rounds: maxRounds });

  return (
    <section className="lmw-panel overflow-hidden arf-role-settings">
      <header className="lmw-panel-head">
        <h2 className="lmw-label inline-flex items-center gap-1"><Shield className="h-3.5 w-3.5" aria-hidden /> role supervision</h2>
        <span className="ml-auto font-mono text-[10px] text-faint">changes apply to the next invocation</span>
      </header>
      <nav className="arf-settings-tabs" aria-label="Role settings sections">
        {(["routing", "advisors", "paper"] as SettingsSection[]).map((value) => <button key={value} type="button" aria-current={section === value ? "page" : undefined} onClick={() => setSection(value)}>{value === "routing" ? "Model routing" : value === "advisors" ? "Advisors" : "Paper policy"}</button>)}
      </nav>

      {section === "routing" ? <div className="arf-routing-settings">
        <div className="arf-routing-bulk">
          <div><strong>Bulk assignment</strong><span>{selectedRoles.size} role{selectedRoles.size === 1 ? "" : "s"} selected</span></div>
          <select aria-label="Bulk provider assignment" value={bulkProvider} onChange={(event) => setBulkProvider(event.target.value)}>
            <option value="inherit">Use inherited default</option>
            <ProviderOptions options={options} selected={bulkProvider} />
          </select>
          <button type="button" disabled={!selectedRoles.size} onClick={() => {
            for (const role of selectedRoles) setRoleProvider(role, bulkProvider);
          }}>Apply to selected</button>
          <button type="button" onClick={() => setSelectedRoles((current) => current.size === allRoleIds.length ? new Set() : new Set(allRoleIds))}>{selectedRoles.size === allRoleIds.length ? "Clear all" : "Select all"}</button>
        </div>
        {ROLE_GROUPS.map((group) => <section key={group.id} className="arf-role-group">
          <header><h3>{group.label}</h3><span>{group.roles.length} assignment{group.roles.length === 1 ? "" : "s"}</span></header>
          {group.roles.map((role) => {
            const effective = effectiveProvider(role.id, roles, defaults);
            const effectiveFallback = effectiveFallbacks(role.id, fallbacks);
            const primaryKey = roles[role.id] ? providerKey(roles[role.id]) : "inherit";
            const fallbackKeys = (fallbacks[role.id] ?? effectiveFallback.refs).map(providerKey);
            const fallbackOptions = withConfiguredProviders(options, effectiveFallback.refs);
            const duplicateFallback = !effectiveFallback.inherited && new Set(fallbackKeys.filter(Boolean)).size !== fallbackKeys.filter(Boolean).length;
            const primaryConflict = !effectiveFallback.inherited && primaryKey !== "inherit" && fallbackKeys.includes(primaryKey);
            return <div key={role.id} className="arf-role-row">
              <label className="arf-role-select"><input type="checkbox" aria-label={`Select ${role.id}`} checked={selectedRoles.has(role.id)} onChange={(event) => setSelectedRoles((current) => {
                const next = new Set(current);
                if (event.target.checked) next.add(role.id); else next.delete(role.id);
                return next;
              })} /><span><strong>{role.label}</strong><code>{role.id}</code></span></label>
              <div className="arf-role-primary">
                <select aria-label={`${role.id} primary provider`} value={primaryKey} onChange={(event) => setRoleProvider(role.id, event.target.value)}>
                  <option value="inherit">Inherit default</option>
                  <ProviderOptions options={options} selected={primaryKey} />
                </select>
                <span>Effective: {effective.ref ? providerLabel(effective.ref) : "unassigned"} · {effective.source.replaceAll("-", " ")}</span>
              </div>
              <button type="button" className="arf-fallback-toggle" aria-expanded={expandedFallbacks.has(role.id)} onClick={() => setExpandedFallbacks((current) => {
                const next = new Set(current);
                if (next.has(role.id)) next.delete(role.id); else next.add(role.id);
                return next;
              })}>Fallbacks · {effectiveFallback.refs.length || "none"}</button>
              {expandedFallbacks.has(role.id) ? <div className="arf-role-fallback-panel">
                <FallbackEditor
                  role={role.id}
                  keys={fallbackKeys}
                  inherited={effectiveFallback.inherited}
                  options={fallbackOptions}
                  onInheritedChange={(inherited) => setFallbacks((current) => {
                    const next = { ...current };
                    if (inherited) delete next[role.id];
                    else next[role.id] = effectiveFallback.refs;
                    return next;
                  })}
                  onChange={(keys) => setFallbacks((current) => ({ ...current, [role.id]: keys.map((key) => byKey.get(key)).filter((ref): ref is ProviderRef => Boolean(ref)) }))}
                />
                {duplicateFallback ? <p className="arf-inline-error" role="alert">Fallback providers must be unique.</p> : null}
                {primaryConflict ? <p className="arf-inline-error" role="alert">Primary provider cannot also be a fallback.</p> : null}
              </div> : null}
            </div>;
          })}
        </section>)}
        {routingValidationError ? <p className="arf-inline-error arf-settings-error" role="alert">{routingValidationError}</p> : null}
        {error ? <p className="arf-inline-error arf-settings-error" role="alert">{error}</p> : null}
        <footer><span>Project assignments override module defaults and Agon settings.</span><Button size="sm" disabled={saving || Boolean(routingValidationError)} onClick={saveRouting}><Save aria-hidden /> {saving ? "saving…" : "save model routing"}</Button></footer>
      </div> : null}

      {section === "advisors" ? <div>
        <div className="arf-advisor-grid">
          {ADVISOR_ROLES.map((role) => {
            const config = advisors[role] ?? { enabled: false, backlog: 1 as const };
            const providerSelection = config.provider ? providerKey(config.provider) : "inherit";
            return <div key={role} className="arf-advisor-row">
              <label><input type="checkbox" checked={config.enabled} onChange={(event) => setAdvisors((previous) => ({ ...previous, [role]: { ...config, enabled: event.target.checked } }))} /><span>{role}</span></label>
              <select aria-label={`${role} advisor provider`} disabled={!config.enabled} value={providerSelection} onChange={(event) => setAdvisors((previous) => {
                const next = { ...config };
                if (event.target.value === "inherit") delete next.provider;
                else if (byKey.get(event.target.value)) next.provider = byKey.get(event.target.value);
                return { ...previous, [role]: next };
              })}><option value="inherit">inherit role provider</option><ProviderOptions options={options} selected={providerSelection} /></select>
              <select aria-label={`${role} advisor backlog`} disabled={!config.enabled} value={config.backlog} onChange={(event) => setAdvisors((previous) => ({ ...previous, [role]: { ...config, backlog: event.target.value === "off" ? "off" : Number(event.target.value) as 1 | 3 | 5 } }))}>
                <option value="off">off</option><option value={1}>1</option><option value={3}>3</option><option value={5}>5</option>
              </select>
            </div>;
          })}
        </div>
        {error ? <p className="arf-inline-error arf-settings-error" role="alert">{error}</p> : null}
        <footer className="arf-settings-footer"><span>Advisors remain non-vetoing and cannot mutate artifacts.</span><Button size="sm" disabled={saving} onClick={saveAdvisors}><Save aria-hidden /> save advisor settings</Button></footer>
      </div> : null}

      {section === "paper" ? <div className="arf-paper-policy">
        <div><label htmlFor="arf-paper-round-cap">Paper round cap</label><input id="arf-paper-round-cap" type="number" min={1} max={20} value={maxRounds} onChange={(event) => setMaxRounds(Math.min(20, Math.max(1, Number(event.target.value))))} /><p>Maximum automated writing, audit, review, and revision rounds before human post-editing.</p></div>
        {error ? <p className="arf-inline-error" role="alert">{error}</p> : null}
        <footer className="arf-settings-footer"><span>Current project policy</span><Button size="sm" disabled={saving} onClick={savePaper}><Save aria-hidden /> save paper policy</Button></footer>
      </div> : null}
    </section>
  );
}
