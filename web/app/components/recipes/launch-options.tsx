import { useState } from "react";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import type { Node, RecipeDownloadPlan, DeploymentPlan } from "~/lib/api";
import { bytes } from "~/lib/format";
import { record, records } from "./workflow";
import { UpstreamLaunchContract } from "./recipe-launch-summary";
import { parameterLabel } from "./presentation";

export type RuntimeSelection = { workload_index?: number; variants: Record<string, string> };
export function runtimeDefaults(manifest: Record<string, unknown>) {
  return {
    parameters: Object.fromEntries(records(manifest.parameters).filter((parameter) => parameter.optional !== true && parameter.default !== undefined).map((parameter) => [String(parameter.name), parameter.default])),
    selection: { variants: Object.fromEntries(records(manifest.artifacts).filter((artifact) => artifact.defaultVariant).map((artifact) => [String(artifact.name), String(artifact.defaultVariant)])) } as RuntimeSelection,
  };
}

function argumentList(value: unknown): string[] | undefined {
  if (typeof value !== "string") return undefined;
  try { const parsed: unknown = JSON.parse(value); return Array.isArray(parsed) && parsed.every((item) => typeof item === "string") ? parsed : undefined; }
  catch { return undefined; }
}

function ParameterInput({ parameter, parameters, resolvedParameters, onChange }: {
  parameter: Record<string, unknown>; parameters: Record<string, unknown>; resolvedParameters?: Record<string, unknown>; onChange: (parameters: Record<string, unknown>) => void;
}) {
  const name = String(parameter.name);
  const label = String(parameter.label || parameterLabel(name));
  const optional = parameter.optional === true;
  const overridden = Object.hasOwn(parameters, name);
  const chosen = overridden ? parameters[name] : optional ? undefined : resolvedParameters?.[name] ?? parameter.default;
  const type = String(parameter.type);
  const numeric = ["int", "integer", "number", "float"].includes(type);
  const boolean = ["boolean", "bool"].includes(type);
  const sensitive = parameter.sensitive === true;
  const argv = parameter.format === "argv";
  const args = argv ? argumentList(chosen) : undefined;
  const enumValues = Array.isArray(parameter.enum) ? parameter.enum : undefined;
  const enumIndex = enumValues?.findIndex((option) => Object.is(option, chosen)) ?? -1;
  const missing = chosen === undefined || chosen === null;
  const unsupported = !missing && (argv ? !args : enumValues ? enumIndex === -1 : boolean ? typeof chosen !== "boolean" : numeric ? typeof chosen !== "number" && chosen !== "" : typeof chosen !== "string");
  const required = !optional && parameter.default == null && resolvedParameters?.[name] == null;
  const update = (next: unknown) => onChange({ ...parameters, [name]: next });
  const unset = () => { const next = { ...parameters }; delete next[name]; onChange(next); };
  return <div className="grid gap-2 border-b border-rule py-3 sm:grid-cols-[minmax(0,1fr)_minmax(12rem,1fr)]">
    <div className="space-y-1">
      <p className="text-sm font-medium">{label}{required ? <span className="ml-2 text-xs font-normal text-muted">Required</span> : null}</p>
      <p className="break-all font-mono text-xs text-muted">{name}</p>
      {parameter.description ? <p className="text-xs text-muted">{String(parameter.description)}</p> : null}
      {optional ? <label className="flex items-center gap-2 text-xs"><input type="checkbox" checked={overridden} onChange={(event) => event.target.checked ? update(parameter.default ?? (argv ? "[]" : boolean ? false : numeric ? "" : "")) : unset()} />Override upstream default</label> : null}
    </div>
    <div className="min-w-0 space-y-2">
      {optional && !overridden ? <p className="py-2 text-sm text-muted">Unset · keep upstream default or computed value</p> : argv ? <div className="space-y-2" role="group" aria-label={label}>
        <p className="text-xs text-muted">One argument per row. Spaces stay within an argument; no shell expressions are evaluated.</p>
        {(args || []).map((argument, index) => <div className="flex gap-2" key={index}><Input aria-label={`${label} argument ${index + 1}`} type={sensitive ? "password" : "text"} autoComplete={sensitive ? "off" : undefined} value={argument} onChange={(event) => update(JSON.stringify(args!.map((item, position) => position === index ? event.target.value : item)))} /><Button type="button" size="sm" variant="ghost" aria-label={`Remove ${label} argument ${index + 1}`} onClick={() => update(JSON.stringify(args!.filter((_, position) => position !== index)))}>Remove</Button></div>)}
        <Button type="button" size="sm" variant="outline" disabled={unsupported} onClick={() => update(JSON.stringify([...(args || []), ""]))}>Add argument</Button>
        {unsupported ? <Button type="button" size="sm" variant="outline" onClick={() => update("[]")}>Replace invalid saved list with empty list</Button> : null}
      </div> : enumValues ? <select aria-label={label} aria-invalid={unsupported || undefined} required={required} className="control w-full bg-panel p-2 text-sm" value={enumIndex < 0 ? "" : String(enumIndex)} onChange={(event) => update(enumValues[Number(event.target.value)])}>
        <option value="" disabled>{unsupported ? "Unsupported saved value" : "Choose a value"}</option>
        {enumValues.map((option, index) => <option key={index} value={index}>{sensitive ? `Option ${index + 1}` : String(option)}</option>)}
      </select> : boolean ? <select aria-label={label} aria-invalid={unsupported || undefined} required={required} className="control w-full bg-panel p-2 text-sm" value={typeof chosen === "boolean" ? String(chosen) : ""} onChange={(event) => update(event.target.value === "true")}>
        <option value="" disabled>{unsupported ? "Unsupported saved value" : "Choose a value"}</option><option value="true">Yes</option><option value="false">No</option>
      </select> : <Input aria-label={label} aria-invalid={unsupported || undefined} required={required} type={sensitive ? "password" : numeric ? "number" : "text"} autoComplete={sensitive ? "off" : undefined} inputMode={numeric ? "decimal" : undefined} step={numeric ? (["int", "integer"].includes(type) ? 1 : "any") : undefined} min={typeof parameter.min === "number" ? parameter.min : undefined} max={typeof parameter.max === "number" ? parameter.max : undefined} minLength={typeof parameter.minLength === "number" ? parameter.minLength : undefined} maxLength={typeof parameter.maxLength === "number" ? parameter.maxLength : undefined} value={missing ? "" : String(chosen)} onChange={(event) => update(numeric && event.target.value !== "" ? Number(event.target.value) : event.target.value)} />}
      {unsupported ? <p className="text-xs text-fault" role="alert">Saved value is unsupported. Correct it before running; it has not been discarded.</p> : null}
    </div>
  </div>;
}

export function RuntimeSelections({ manifest, value, onChange, parameters, resolvedParameters, onParametersChange, disabled = false, compact = false }: {
  manifest: Record<string, unknown>; value: RuntimeSelection; onChange: (value: RuntimeSelection) => void;
  parameters?: Record<string, unknown>; resolvedParameters?: Record<string, unknown>; onParametersChange?: (value: Record<string, unknown>) => void; disabled?: boolean; compact?: boolean;
}) {
  const [search, setSearch] = useState("");
  const [group, setGroup] = useState("");
  const artifacts = records(manifest.artifacts).filter((artifact) => Array.isArray(artifact.variants) && artifact.variants.length);
  const workloads = records(manifest.workloads);
  const parameterFields = records(manifest.parameters);
  const extraVariants = Object.keys(value.variants).filter((name) => !artifacts.some((artifact) => artifact.name === name));
  const extraParameters = Object.keys(parameters || {}).filter((name) => !parameterFields.some((parameter) => parameter.name === name));
  const unsupportedRuntime = value.workload_index !== undefined && (!Number.isInteger(value.workload_index) || value.workload_index < 0 || value.workload_index >= workloads.length);
  const groups = [...new Set(parameterFields.map((parameter) => String(parameter.group || "General")))].sort();
  const query = search.trim().toLowerCase();
  const visible = parameterFields.filter((parameter) => (!group || String(parameter.group || "General") === group) && [parameter.name, parameter.label, parameter.description, parameter.group, parameterLabel(String(parameter.name))].join(" ").toLowerCase().includes(query));
  return <fieldset disabled={disabled} className="space-y-3">
    {workloads.length > 1 ? <label className="grid gap-1.5 text-sm"><span>Runtime</span><select aria-label="Runtime" className="control bg-panel p-2" value={value.workload_index ?? ""} onChange={(event) => onChange({ ...value, workload_index: event.target.value === "" ? undefined : Number(event.target.value) })}><option value="">Choose automatically for the device</option>{unsupportedRuntime ? <option value={value.workload_index} disabled>Unsupported saved runtime</option> : null}{workloads.map((workload, index) => <option key={index} value={index}>{workload.name ? parameterLabel(String(workload.name)) : `Runtime ${index + 1}`}</option>)}</select></label> : null}
    {unsupportedRuntime ? <div className="border border-fault/40 p-2 text-sm" role="alert">The saved runtime is not supported by this version. <Button type="button" size="sm" variant="outline" onClick={() => onChange({ ...value, workload_index: undefined })}>Use automatic runtime</Button></div> : null}
    {artifacts.map((artifact) => {
      const name = String(artifact.name);
      const label = `${parameterLabel(name)} variant`;
      const variants = records(artifact.variants);
      const chosen = value.variants[name] ?? String(artifact.defaultVariant || "");
      const unsupported = chosen !== "" && !variants.some((variant) => variant.name === chosen);
      return <div className="space-y-1.5" key={name}>
        <label className="grid gap-1.5 text-sm"><span>{label}</span><select aria-label={label} aria-invalid={unsupported || undefined} className="control bg-panel p-2" value={chosen} onChange={(event) => onChange({ ...value, variants: { ...value.variants, [name]: event.target.value } })}>
          <option value="">Choose a supported variant</option>
          {unsupported ? <option value={chosen} disabled>Unsupported saved variant</option> : null}
          {variants.map((variant) => <option key={String(variant.name)} value={String(variant.name)}>{String(variant.label || variant.name)}{!compact && variant.description ? ` · ${String(variant.description)}` : ""}</option>)}
        </select></label>
        {unsupported ? <p className="text-xs text-fault" role="alert">The saved choice for {parameterLabel(name)} is not supported by this version. Choose a supported variant.</p> : null}
      </div>;
    })}
    {parameters && onParametersChange && parameterFields.length ? <section className="space-y-2" aria-label="Runtime parameters">
      <div className="flex flex-wrap items-center gap-2">
        <Input className="min-w-40 flex-1" type="search" aria-label="Search all settings" placeholder={`Search ${parameterFields.length} settings…`} value={search} onChange={(event) => setSearch(event.target.value)} />
        <select aria-label="Settings group" className="control bg-panel p-2 text-sm" value={group} onChange={(event) => setGroup(event.target.value)}><option value="">All groups</option>{groups.map((name) => <option key={name} value={name}>{name}</option>)}</select>
        {search || group ? <Button type="button" variant="ghost" size="sm" onClick={() => { setSearch(""); setGroup(""); }}>Clear filters</Button> : null}
      </div>
      <p className="text-xs text-muted" role="status">{visible.length} of {parameterFields.length} settings · filtering never changes values</p>
      <div className="max-h-[32rem] overflow-y-auto pr-2">{visible.map((parameter) => <ParameterInput key={String(parameter.name)} parameter={parameter} parameters={parameters} resolvedParameters={resolvedParameters} onChange={onParametersChange} />)}</div>
      {!visible.length ? <p className="text-sm text-muted">No matching settings. Clear the search or choose another group.</p> : null}
    </section> : null}
    {extraVariants.map((name) => <div className="border border-fault/40 p-2 text-sm" role="alert" key={name}>Saved variant {parameterLabel(name)} is not supported by this version. <Button type="button" size="sm" variant="outline" onClick={() => { const variants = { ...value.variants }; delete variants[name]; onChange({ ...value, variants }); }}>Remove unsupported variant</Button></div>)}
    {extraParameters.map((name) => <div className="border border-fault/40 p-2 text-sm" role="alert" key={name}>Saved parameter {parameterLabel(name)} is not supported by this version. <Button type="button" size="sm" variant="outline" onClick={() => { const next = { ...parameters }; delete next[name]; onParametersChange?.(next); }}>Remove unsupported parameter</Button></div>)}
  </fieldset>;
}
export function DeviceSelections({ nodes, selected, onChange, rankCount, reason, disabled = false }: {
  nodes: Node[]; selected: string[]; onChange: (selected: string[]) => void; rankCount?: number;
  reason?: (node: Node, rank: number) => string; disabled?: boolean;
}) {
  return <fieldset disabled={disabled} className="space-y-2">
    <legend className="mb-2 text-sm font-medium">{rankCount ? rankCount === 1 ? "Device" : "Devices" : "Download devices"}</legend>
    {rankCount ? Array.from({ length: rankCount }, (_, rank) => {
      const label = rankCount === 1 ? "Device" : rank === 0 ? "Head device" : `Worker ${rank}`;
      return <label key={rank} className="grid gap-1 text-sm">
        {rankCount > 1 ? <span>{label}</span> : null}
        <select aria-label={label} className="control bg-panel p-2" value={selected[rank] || ""} onChange={(event) => { const next = [...selected]; next[rank] = event.target.value; onChange(next); }}>
          <option value="">Choose a device</option>
          {nodes.map((node) => { const blocker = reason?.(node, rank) || (node.status !== "online" ? node.status : ""); return <option key={node.id} value={node.id} disabled={Boolean(blocker)}>{node.display_name}{blocker ? ` · ${blocker}` : ""}</option>; })}
        </select>
      </label>;
    }) : nodes.map((node) => <label key={node.id} className="flex gap-2 border border-rule p-2 text-sm"><input type="checkbox" checked={selected.includes(node.id)} disabled={node.status !== "online"} onChange={() => onChange(selected.includes(node.id) ? selected.filter((id) => id !== node.id) : [...selected, node.id])} /><span>{node.display_name} · {node.status}<span className="block text-xs text-muted">Storage eligibility is checked separately from GPU occupancy and runtime suitability.</span></span></label>)}
  </fieldset>;
}
export function AcquisitionPlanDetails({ plan }: { plan: RecipeDownloadPlan }) {
  return <section className="space-y-3"><h3 className="font-medium">Required files and destinations</h3><div className="space-y-2">{plan.resources.map((resource) => <article key={resource.key} className="space-y-1 border border-rule p-3 text-xs"><p className="font-medium">{resource.kind} · {resource.action} · {resource.verification.state}{resource.verification.stale ? " (stale observation)" : ""}</p><p className="break-all font-mono">{resource.identity}</p><p className="break-all">Device {resource.node_id} · {resource.destination}{resource.platform ? ` · ${resource.platform}` : ""}</p><p>Remaining: {resource.bytes_remaining == null ? "Unknown" : bytes(resource.bytes_remaining)} · Total: {resource.bytes_total == null ? "Unknown" : bytes(resource.bytes_total)}</p>{resource.source_node ? <p>Copy from {resource.source_node}: {resource.source_path}</p> : null}{resource.credential_id ? <p>Selected credential: {resource.credential_id}</p> : null}{resource.verification.verified_at ? <p>Last checked: {resource.verification.verified_at}</p> : null}</article>)}</div>
    <h3 className="font-medium">Storage and staging</h3>{plan.storage.map((storage, index) => <article className="border border-rule p-3 text-xs" key={index}><p className="break-all">{storage.node_id} · {storage.destination}</p><p className="break-all">Filesystem: {storage.filesystem}</p><p>Available: {storage.available_bytes == null ? "Unknown" : bytes(storage.available_bytes)} · Required: {storage.required_bytes == null ? "Unknown" : bytes(storage.required_bytes)} · Staging: {storage.staging_bytes == null ? "Unknown" : bytes(storage.staging_bytes)} · Reserve: {bytes(storage.reserve_bytes)}</p><p>{storage.sufficient == null ? "Safe capacity is unknown; submission is blocked" : storage.sufficient ? "Storage capacity verified" : "Insufficient storage"}</p></article>)}
    {plan.diagnostics.map((diagnostic, index) => <p key={index} className={diagnostic.severity === "error" ? "text-sm text-fault" : "text-sm text-warning"}>{diagnostic.code}: {diagnostic.message}{diagnostic.resource ? ` (${diagnostic.resource})` : ""}</p>)}
  </section>;
}
export function UpstreamExecutionAcknowledgment({ checked, onChange, disabled = false }: {
  checked: boolean; onChange: (checked: boolean) => void; disabled?: boolean;
}) {
  return <label className="flex gap-2 border border-warning/40 p-3 text-sm"><input type="checkbox" checked={checked} disabled={disabled} onChange={(event) => onChange(event.target.checked)} /><span>I authorize original upstream code to execute on the selected nodes as the agent account (host.upstream-exec). It can use Docker and host resources and perform its own downloads and builds. It is not confined by the existing managed-container safeguards. Existing-file policy does not prevent upstream script network activity, and host or dependency changes are not guaranteed to be rolled back.</span></label>;
}
export function LaunchPlanDetails({ plan }: { plan: DeploymentPlan }) {
  const upstreamExecution = Boolean(plan.risks?.includes("host.upstream-exec"));
  return <section className="space-y-3"><h3 className="font-medium">Run consequences</h3><p className="text-sm text-muted">{upstreamExecution ? "The listed upstream procedures run as the agent account, with access to Docker and host resources. Where a coordinator is listed, it runs start/stop; observers run only their listed per-node setup and independently observe the service. Upstream can perform its own downloads and builds outside managed-container safeguards. Stopping or replacing containers does not guarantee rollback of host or dependency changes." : "Starting can create containers, run reviewed preparation and verification helpers, and apply declared host changes. Download completion alone does not prove hardware readiness or offline setup."}</p>
    <section className="space-y-2" aria-label="Effective launch selections"><h4 className="text-sm font-medium">Effective launch selections</h4><p className="break-all text-xs">Recipe: {plan.recipe_digest} · Runtime index: {plan.workload_index}</p><pre className="max-h-64 overflow-auto whitespace-pre-wrap break-all bg-raised p-2 text-xs">{JSON.stringify({ parameters: plan.parameters, variants: plan.variants, placements: plan.placements, fabric: plan.fabric }, null, 2)}</pre></section>
    {(plan.upstream || []).map((preview) => <article key={`${preview.node_id}:${preview.rank}`} className="space-y-3 border border-warning/40 p-3"><h4 className="break-all font-medium">Upstream execution on {preview.node_name || preview.node_id} · rank {preview.rank}</h4>{preview.node_name ? <p className="break-all font-mono text-xs">{preview.node_id}</p> : null}<UpstreamLaunchContract source={record(preview.source)} upstream={record(preview.execution)} environment={record(preview.environment)} configuration={records(preview.configuration)} rank={preview.rank} /></article>)}
    {upstreamExecution ? <p className="text-sm text-warning">The file and storage review below covers app-managed resources only. It is not an inventory or capacity guarantee for downloads and builds performed by upstream scripts. “Use verified existing files only” does not restrict their network activity.</p> : null}
    {plan.acquisition ? <AcquisitionPlanDetails plan={plan.acquisition} /> : null}
    {(plan.diagnostics || []).map((diagnostic, index) => <p key={index} className={diagnostic.severity === "error" ? "text-sm text-fault" : "text-sm text-warning"}>{diagnostic.code}: {diagnostic.message}{diagnostic.resource ? ` (${diagnostic.resource})` : ""}</p>)}
    {plan.risks?.length ? <section className="space-y-1"><h4 className="text-sm font-medium">Requested permissions and risks</h4><ul className="list-disc space-y-1 pl-5 text-sm">{plan.risks.map((risk) => <li key={risk}>{risk === "host.upstream-exec" ? "host.upstream-exec · Execute original upstream code as the agent account outside managed-container safeguards; explicit approval is required." : risk}</li>)}</ul></section> : null}
    <details><summary className="cursor-pointer text-sm">All ranks, conflicts, permissions, images and host consequences</summary><pre className="mt-2 max-h-96 overflow-auto text-xs">{JSON.stringify(plan, null, 2)}</pre></details>
  </section>;
}
