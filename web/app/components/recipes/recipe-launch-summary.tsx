import type { RecipeDetail } from "~/lib/api";
import { bytes } from "~/lib/format";
import { record, records } from "./workflow";

const settingLabels: Record<string, { label: string; explanation: string; primary?: boolean }> = {
  MODEL_PATH: { label: "Model location", explanation: "Where the launcher reads the model files." },
  SERVED: { label: "Served model name", explanation: "The model name exposed to API clients." },
  API_HOST: { label: "Listen address", explanation: "Network interfaces on which the API listens." },
  API_PORT: { label: "API port", explanation: "Port used by clients to reach the model.", primary: true },
  MAX_MODEL_LEN: { label: "Context limit", explanation: "Token budget for input and generated output.", primary: true },
  GPU_MEMORY_UTILIZATION: { label: "GPU memory fraction", explanation: "Requested share of GPU memory for the engine." },
  HOST_RESERVE_GIB: { label: "Host memory reserve (GiB)", explanation: "Reserve target supplied to the launcher." },
  MAX_NUM_SEQS: { label: "Concurrent sequences", explanation: "Maximum sequences scheduled together.", primary: true },
  MAX_NUM_BATCHED_TOKENS: { label: "Tokens per batch", explanation: "Token budget per scheduler step.", primary: true },
  MTP_NUM_SPECULATIVE_TOKENS: { label: "Speculative tokens", explanation: "Tokens proposed ahead of verification.", primary: true },
  KV_CACHE_DTYPE: { label: "KV cache format", explanation: "Storage format for attention history.", primary: true },
  MAMBA_SSM_CACHE_DTYPE: { label: "SSM cache format", explanation: "Storage format for recurrent model state." },
  TENSOR_PARALLEL_SIZE: { label: "Tensor parallel size", explanation: "Accelerators sharing the model's tensors." },
};
function text(value: unknown): string {
  if (value === undefined || value === null) return "Not recorded";
  if (Array.isArray(value)) return value.map(text).join(", ");
  return typeof value === "object" ? JSON.stringify(value) : String(value);
}
function hardware(value: unknown): string {
  const requirement = record(value); const accelerator = record(requirement.accelerator);
  return [requirement.nodeCount ? `${requirement.nodeCount} node(s)` : "", accelerator.count ? `${accelerator.count} accelerator(s) per node` : "",
    accelerator.vendor, text(accelerator.architectures || []), accelerator.minMemoryBytes ? `${bytes(Number(accelerator.minMemoryBytes))} minimum accelerator memory` : "",
    accelerator.features ? `features: ${text(accelerator.features)}` : ""].filter(Boolean).join(" · ");
}
function ArtifactSource({ source }: { source: Record<string, unknown> }) {
  return <div className="space-y-1 text-sm"><p className="break-all font-mono">{text(source.identity)}</p><p className="break-all text-xs text-muted">{text(source.type)} · Revision: {text(source.revision)}{source.digest ? ` · Digest: ${text(source.digest)}` : ""}</p></div>;
}
function Environment({ entries, compact = false }: { entries: [string, unknown][]; compact?: boolean }) {
  return <dl className={compact ? "grid gap-4 text-sm sm:grid-cols-2 xl:grid-cols-3" : "divide-y divide-rule text-sm"}>{entries.map(([name, value]) => <div key={name} className={compact ? "space-y-1" : "grid gap-1 py-2 sm:grid-cols-2"}><dt title={name} className="font-medium">{settingLabels[name]?.label || name}</dt><dd><p className="break-all font-mono text-sm">{text(value)}</p>{settingLabels[name] ? <p className="mt-1 text-xs leading-relaxed text-muted">{settingLabels[name].explanation}</p> : null}</dd></div>)}</dl>;
}

export function UpstreamLaunchContract({ source, upstream, environment, configuration = [], rank }: {
  source: Record<string, unknown>; upstream: Record<string, unknown>; environment: Record<string, unknown>; configuration?: Record<string, unknown>[]; rank?: number;
}) {
  const install = Array.isArray(upstream.install) ? upstream.install : [];
  const perRankInstall = Object.entries(record(upstream.installByRank));
  const observer = typeof upstream.coordinatorRank === "number" && rank !== undefined && rank !== upstream.coordinatorRank;
  const commands = [
    ...install.map((argv, index) => ({ label: `Install · step ${index + 1}`, argv })),
    ...perRankInstall.flatMap(([nodeRank, commands]) => Array.isArray(commands) ? commands.map((argv, index) => ({ label: `Install override · rank ${nodeRank} · step ${index + 1}`, argv })) : []),
    ...(observer ? [] : [{ label: "Start", argv: upstream.start }, { label: "Stop", argv: upstream.stop }]),
  ];
  return <section className="space-y-3">
    <h4 className="font-medium">Upstream launch</h4>
    <dl className="space-y-1 text-sm">
      <div><dt className="font-medium">Repository</dt><dd className="break-all font-mono text-xs">{text(source.url)}</dd></div>
      <div><dt className="font-medium">Exact revision</dt><dd className="break-all font-mono text-xs">{text(source.revision)}</dd></div>
      <div><dt className="font-medium">Source directory · command working directory</dt><dd className="break-all font-mono text-xs">{text(source.path || ".")}</dd></div>
    </dl>
    {typeof upstream.coordinatorRank === "number" ? <p className="text-sm font-medium">{observer ? `Observer rank ${rank}: per-node setup and observation only; rank ${upstream.coordinatorRank} executes start/stop.` : `Coordinator rank ${upstream.coordinatorRank} executes the original start/stop procedure; other ranks independently observe the service.`}</p> : null}
    <p className="text-xs text-muted">The upstream repository supplies installation, downloads, builds and startup. The app applies the runtime overrides listed below before launching. Each quoted command item is one argument, in execution order.</p>
    {!install.length && !perRankInstall.length ? <p className="text-sm text-muted">No separate install command is declared.</p> : null}
    {perRankInstall.filter(([, commands]) => Array.isArray(commands) && commands.length === 0).map(([nodeRank]) => <p key={nodeRank} className="text-sm text-muted">Rank {nodeRank} explicitly skips the default install commands.</p>)}
    {commands.map(({ label, argv }) => <div key={label} className="space-y-1"><p className="text-sm font-medium">{label}</p><pre className="overflow-x-auto whitespace-pre-wrap break-all bg-raised p-2 text-xs">{Array.isArray(argv) && argv.length ? argv.map((argument) => JSON.stringify(String(argument))).join(" ") : "No command declared"}</pre></div>)}
    <div className="space-y-1 text-sm"><p className="font-medium">Supported upstream configuration</p>
      <p className="break-all text-xs">{upstream.envFile ? <>Destination relative to source directory: <code>{text(upstream.envFile)}</code>. Reviewed keys are written here and also supplied to the command environment; other upstream configuration entries are retained.</> : "No environment file is declared; reviewed values are supplied to the command environment."}</p>
      {upstream.envTemplate ? <p className="break-all text-xs">Upstream template: <code>{text(upstream.envTemplate)}</code> · Copied only when the destination file does not exist.</p> : null}
      {upstream.envFile ? <p className="text-xs">Authored configuration parser: {text(upstream.envFormat || "shell")}.</p> : null}
      {Object.keys(environment).length ? <Environment entries={Object.entries(environment)} /> : <p className="text-xs text-muted">No configuration overrides are declared.</p>}
    </div>
    {configuration.length ? <section className="space-y-2"><h4 className="text-sm font-medium">Runtime argument overrides</h4>{configuration.map((file, index) => <div className="space-y-1 text-xs" key={index}><p className="font-mono">{text(file.path)}</p><p className="break-all text-muted">Original source SHA-256: {text(file.sha256)}</p><pre className="overflow-x-auto whitespace-pre-wrap break-all bg-raised p-2">{records(file.edits).map((edit) => {
      const binding = records(records(upstream.configuration).find((sourceFile) => sourceFile.path === file.path)?.edits).find((candidate) => candidate.start === edit.start && candidate.end === edit.end);
      const source = binding?.variable ? `Preserve upstream ${binding.indirect ? "indirect " : ""}variable ${text(binding.variable)} (${text(binding.format)})` : binding?.parameter ? `Parameter ${text(binding.parameter)}` : binding?.template ? "Resolved template" : "Resolved source edit";
      return `${text(edit.start)}–${text(edit.end)} · ${source}: ${text(edit.replacement)}`;
    }).join("\n")}</pre></div>)}</section> : null}
    <div className="space-y-1 text-sm"><p className="font-medium">Observation after startup</p><p className="break-all text-xs">Upstream-owned container names: {Array.isArray(upstream.containers) && upstream.containers.length ? upstream.containers.map(text).join(", ") : "None declared"}. The app observes these names; it does not rename the containers.</p>
      {Object.entries(record(upstream.containersByRank)).map(([nodeRank, names]) => <p key={nodeRank} className="break-all text-xs">Rank {nodeRank} container override: {text(names)}</p>)}
      {Array.isArray(upstream.auxiliaryContainers) && upstream.auxiliaryContainers.length ? <p className="break-all text-xs">Additional authored containers in the ownership/cleanup scope: {upstream.auxiliaryContainers.map(text).join(", ")}</p> : null}
      <p className="break-all text-xs">{upstream.logFile ? <>Upstream log file relative to source directory: <code>{text(upstream.logFile)}</code></> : "No upstream log file is declared."}</p>
      <p className="text-xs text-muted">Declared readiness and verification checks observe the result; they do not replace the upstream startup commands.</p>
    </div>
  </section>;
}

export function RecipeLaunchSummary({ recipe }: { recipe: RecipeDetail }) {
  const manifest = record(recipe.manifest); const metadata = record(manifest.metadata);
  const source = record(metadata.source || recipe.source); const compatibility = record(manifest.compatibility || recipe.compatibility);
  const artifacts = records(manifest.artifacts); const workloads = records(manifest.workloads); const parameters = records(manifest.parameters);
  const hasUpstream = workloads.some((workload) => Boolean(workload.upstream));
  const description = text(metadata.description || recipe.description || "No launch notes or upstream differences were recorded.");
  const fabric = record(compatibility.fabric);
  const hardwareSummary = hardware(compatibility);
  return <section className="control space-y-6 p-4">
    <div className="space-y-2"><h2 className="font-display text-xl font-semibold">How this recipe launches</h2><p className="text-sm text-muted">Saved launch instructions · Version {recipe.version}. These are not live device settings. {hasUpstream ? "Upstream scripts can acquire files while installing or starting; an app-managed download is not a prerequisite inventory of those files." : "Downloading files and launching a model are separate actions."}</p>
      <p className="text-sm"><span className="font-medium">Serving engine:</span> {text(metadata.engine || recipe.engine || (hasUpstream ? "Not recorded; inspect the upstream commands below" : "Not recorded; inspect the image and launcher below"))}</p>
      {hardwareSummary ? <p className="text-sm"><span className="font-medium">Requires:</span> {hardwareSummary}{fabric.transport ? ` · ${text(fabric.transport)} fabric` : ""}{fabric.minBandwidthGbps ? ` · ${text(fabric.minBandwidthGbps)} Gbps minimum fabric bandwidth` : ""}</p> : null}
    </div>
    <section className="space-y-3"><h3 className="font-display text-lg font-semibold">Model files and checkpoints</h3>
      {artifacts.length ? artifacts.map((artifact, index) => <article key={index} className="space-y-2 border-l-2 border-rule pl-3"><h4 className="font-medium">{text(artifact.name)} <span className="text-xs text-muted">{text(artifact.kind)}</span></h4>
        {artifact.source ? <ArtifactSource source={record(artifact.source)} /> : records(artifact.variants).map((variant, variantIndex) => <div key={variantIndex} className="space-y-1"><p className="text-sm font-medium">{text(variant.label || variant.name)}{variant.name === artifact.defaultVariant ? " · Saved default" : " · Available variant"}</p>{variant.description ? <p className="text-sm text-muted">{text(variant.description)}</p> : null}<ArtifactSource source={record(variant.source)} /></div>)}
        <p className="break-all text-xs text-muted">Mounted at {text(artifact.mount)}{artifact.sizeBytes ? ` · Recorded size: ${bytes(Number(artifact.sizeBytes))}` : ""}{artifact.validation ? ` · Validation: ${text(artifact.validation)}` : ""}</p>
      </article>) : <p className="text-sm text-muted">{hasUpstream ? "Model files and serving images are acquired by the original upstream scripts." : "No model artifacts are declared in the saved manifest. A model identity cannot be established from the recipe name alone."}</p>}
      {hasUpstream ? <p className="text-sm text-muted">Downloads performed by upstream scripts are not represented by the app-managed artifact list or cache inventory.</p> : null}
    </section>
    <section className="space-y-3"><h3 className="font-display text-lg font-semibold">Launch settings</h3>
      {parameters.length ? <><p className="text-sm text-muted">Supported launch parameters. Defaults below belong to this recipe; device-specific overrides are not shown.</p><dl className="divide-y divide-rule text-sm">{parameters.map((parameter, index) => <div key={index} className="space-y-1 py-2"><dt className="font-medium">{text(parameter.name)} <span className="text-xs text-muted">{text(parameter.type)}{parameter.sensitive ? " · sensitive" : ""}</span></dt><dd>{parameter.description ? <p>{text(parameter.description)}</p> : null}<p className="break-all text-xs text-muted">Default: {parameter.default === undefined ? "No default declared" : parameter.sensitive ? "Sensitive value (see manifest)" : text(parameter.default)}{parameter.enum ? ` · Choices: ${text(parameter.enum)}` : ""}{parameter.min !== undefined ? ` · Minimum: ${text(parameter.min)}` : ""}{parameter.max !== undefined ? ` · Maximum: ${text(parameter.max)}` : ""}{parameter.minLength !== undefined ? ` · Minimum length: ${text(parameter.minLength)}` : ""}{parameter.maxLength !== undefined ? ` · Maximum length: ${text(parameter.maxLength)}` : ""}</p></dd></div>)}</dl></> : <p className="text-sm text-muted">No launch-time parameters are declared. To change the fixed settings below, use Edit settings to review a new recipe version.</p>}
      {workloads.length > 1 ? <p className="text-sm text-muted">Multiple launch variants are saved. The first hardware match is selected at launch; this page does not select one for a device.</p> : null}
      {workloads.map((workload, index) => {
        const image = record(workload.image); const environment = Object.entries(record(workload.env));
        const upstream = workload.upstream ? record(workload.upstream) : undefined;
        const primaryEnvironment = environment.filter(([name]) => settingLabels[name]?.primary); const otherEnvironment = environment.filter(([name]) => !settingLabels[name]?.primary);
        const resources = record(workload.resources); const host = record(workload.hostPreparation);
        const command = [...(Array.isArray(workload.command) ? workload.command : []), ...(Array.isArray(workload.args) ? workload.args : [])];
        return <article key={index} className="space-y-3 border-t border-rule pt-3">
          {workloads.length > 1 ? <h4 className="font-medium">Launch variant {index + 1} · {hardware(workload.match) || "Any remaining hardware"}</h4> : null}
          {upstream ? <UpstreamLaunchContract source={source} upstream={upstream} environment={record(workload.env)} /> : <><details><summary className="cursor-pointer text-sm">Serving image and launch command</summary><div className="mt-2 space-y-3">
          <div className="space-y-1"><p className="text-sm font-medium">Serving image</p><p className="break-all font-mono text-xs">{text(image.reference)}</p>{image.digest && !text(image.reference).includes(text(image.digest)) ? <p className="break-all font-mono text-xs text-muted">Pinned digest: {text(image.digest)}</p> : null}</div>
          <div className="space-y-1"><p className="text-sm font-medium">Launcher and arguments</p><p className="text-xs text-muted">Arguments are shown in order, one per line. Artifact, parameter and node placeholders are resolved only when preparing a deployment.</p><pre className="overflow-x-auto whitespace-pre-wrap break-all bg-raised p-2 text-xs">{command.map(text).join("\n") || "No launcher declared"}</pre></div>
          </div></details>
          {primaryEnvironment.length ? <Environment entries={primaryEnvironment} compact /> : null}
          {otherEnvironment.length ? <details><summary className="cursor-pointer text-sm">{primaryEnvironment.length ? "Other runtime environment" : "Runtime environment"} ({otherEnvironment.length})</summary><Environment entries={otherEnvironment} /></details> : null}
          {environment.length ? <p className="text-xs text-muted">Environment values are saved inputs to the launcher, not a measurement of effective runtime behavior. Helper code may interpret or override them.</p> : null}
          </>}
          <div className="space-y-1 text-xs text-muted">{!upstream ? <><p>Network: {text(workload.networkMode || "Not explicitly set")}{records(workload.ports).map((port) => ` · ${text(port.protocol || "tcp")} ${text(port.host || "no host mapping")} → container ${text(port.container)}`).join("")}</p>
            {Object.keys(resources).length ? <p>{[resources.shmBytes ? `Shared memory: ${bytes(Number(resources.shmBytes))}` : "", resources.tmpfsBytes ? `Temporary memory filesystem: ${bytes(Number(resources.tmpfsBytes))}` : "", resources.pids ? `Process limit: ${text(resources.pids)}` : ""].filter(Boolean).join(" · ")}</p> : null}</> : <p className="text-warning">Original code executes as the agent account on the selected node and can use Docker and host resources. Existing managed-container safeguards do not confine this execution.</p>}
            {workload.ranks ? <p>Distributed ranks: {text(workload.ranks)}{workload.startOrder ? ` · Start order: ${text(workload.startOrder)}` : ""}</p> : null}
            {Object.keys(host).length ? <p>Declared host preparation: {Object.entries(host).map(([name, value]) => `${name}: ${text(value)}`).join(" · ")}</p> : null}
            {workload.permissions ? <p className="break-all">Requested permissions: {text(workload.permissions)}</p> : null}
          </div>
        </article>;
      })}
      {!workloads.length ? <p className="text-sm text-warning">No workload details are available in this response. Inspect the saved manifest before changing the launch instructions.</p> : null}
    </section>
    <section className="space-y-3"><h3 className="font-display text-lg font-semibold">Recorded launch notes and upstream differences</h3><ul className="list-disc space-y-2 pl-5 text-sm leading-relaxed">{description.split(/(?<=[.!?])\s+(?=[A-Z])/u).map((note, index) => <li key={index}>{note}</li>)}</ul>
      <p className="text-xs text-muted">These are the recipe's recorded notes, not an independently verified comparison. The saved package API does not provide a complete structured adaptation record. Differences not documented here remain unknown; an empty record does not mean this matches upstream.</p>
      <details><summary className="cursor-pointer text-sm">Pinned upstream source</summary><dl className="mt-2 space-y-1 text-xs"><div><dt className="inline font-medium">Upstream source: </dt><dd className="inline break-all">{text(source.url)}</dd></div><div><dt className="inline font-medium">Pinned revision: </dt><dd className="inline break-all font-mono">{text(source.revision)}</dd></div>{source.path ? <div><dt className="inline font-medium">Source path: </dt><dd className="inline break-all">{text(source.path)}</dd></div> : null}{source.procedure ? <div><dt className="inline font-medium">Documented procedure: </dt><dd className="inline">{text(source.procedure)}</dd></div> : null}</dl></details>
    </section>
    {Array.isArray(manifest.assets) && manifest.assets.length ? <details><summary className="cursor-pointer text-sm">Packaged helper files ({manifest.assets.length})</summary><p className="mt-2 text-xs text-muted">Available under /lmw/assets. A filename alone does not establish upstream origin or unchanged content.</p><ul className="mt-2 space-y-1 font-mono text-xs">{manifest.assets.map((asset, index) => <li key={index} className="break-all">{text(asset)}</li>)}</ul></details> : null}
    <details><summary className="cursor-pointer text-sm">Full saved manifest</summary><pre className="mt-3 max-h-[36rem] overflow-auto text-xs">{JSON.stringify(recipe.manifest, null, 2)}</pre></details>
    <details><summary className="cursor-pointer text-sm">Package identity, source and permissions</summary><pre className="mt-2 overflow-auto text-xs">{JSON.stringify({ digest: recipe.digest, source: recipe.source, permissions: recipe.permissions, high_risk: recipe.high_risk }, null, 2)}</pre></details>
  </section>;
}
