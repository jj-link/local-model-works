import { useEffect, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router";
import {
  Activity,
  Archive,
  Beaker,
  Check,
  CircleStop,
  Download,
  Pin,
  PinOff,
  RotateCcw,
  Save,
  ShieldCheck,
  Sparkles,
} from "lucide-react";
import { toast } from "sonner";
import { Badge } from "~/components/ui/badge";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { Label } from "~/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "~/components/ui/select";
import { Switch } from "~/components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "~/components/ui/table";
import { EmptyState } from "~/components/empty-state";
import type {
  CodingTrace,
  CodingTraceSettings,
  SweGymConfig,
  SweGymPlan,
  TraceExportCreate,
} from "~/lib/api";
import { codingTraceExportURL } from "~/lib/api";
import {
  useCancelSweGymExperiment,
  useCodingTrace,
  useCodingTraceEvents,
  useCodingTraceExports,
  useCodingTraces,
  useCreateCodingTraceExport,
  useCreateSweGymExperiment,
  useDeployments,
  useModuleSettings,
  usePinCodingTrace,
  usePlanSweGymExperiment,
  usePutModuleSettings,
  useResumeSweGymExperiment,
  useSecrets,
  useSweGymExperiment,
  useSweGymExperiments,
} from "~/lib/queries";
import { number, shortId, wallClock } from "~/lib/format";
import { cn } from "~/lib/utils";

const TABS = [
  { id: "swe-gym", label: "SWE-Gym Replication", icon: Beaker },
  { id: "traces", label: "Trajectories", icon: Activity },
  { id: "datasets", label: "Datasets & Exports", icon: Archive },
  { id: "settings", label: "Settings", icon: ShieldCheck },
] as const;
type TabID = (typeof TABS)[number]["id"];

function StateBadge({ state }: { state?: string | null }) {
  const tone =
    state === "completed" || state === "resolved" || state === "succeeded"
      ? "border-ok/40 bg-ok/10 text-ok"
      : state === "failed" ||
          state === "infrastructure_error" ||
          state === "interrupted"
        ? "border-danger/40 bg-danger/10 text-danger"
        : state === "unresolved" || state === "cancelling"
          ? "border-warn/40 bg-warn/10 text-warn"
          : "border-primary/35 bg-primary/10 text-primary";
  return (
    <Badge
      variant="outline"
      className={cn("font-mono text-[10px] uppercase tracking-wider", tone)}
    >
      {state ?? "unknown"}
    </Badge>
  );
}

function SectionStat({
  label,
  value,
  accent,
}: {
  label: string;
  value: string | number;
  accent?: string;
}) {
  return (
    <div className="min-w-0 border-l border-hairline pl-3">
      <p className="font-mono text-[9px] uppercase tracking-[0.16em] text-faint">
        {label}
      </p>
      <p
        className={cn(
          "mt-1 truncate font-display text-xl font-semibold tnum text-foreground",
          accent,
        )}
      >
        {value}
      </p>
    </div>
  );
}

export default function CodingTracesRoute() {
  const [params, setParams] = useSearchParams();
  const requested = params.get("tab") as TabID | null;
  const tab = TABS.some((item) => item.id === requested)
    ? (requested as TabID)
    : "swe-gym";
  return (
    <div className="grid gap-4">
      <section className="relative overflow-hidden rounded border border-hairline bg-panel px-4 py-4 shadow-[inset_0_1px_0_rgba(255,255,255,0.03)]">
        <div className="pointer-events-none absolute inset-y-0 right-0 w-1/2 bg-[radial-gradient(circle_at_80%_20%,rgba(139,124,246,0.13),transparent_54%)]" />
        <div className="relative flex flex-col gap-4 xl:flex-row xl:items-end xl:justify-between">
          <div>
            <p className="font-mono text-[10px] uppercase tracking-[0.2em] text-violet">
              paper replication / data plane
            </p>
            <h1 className="mt-1 font-display text-2xl font-semibold tracking-tight text-foreground">
              OpenHands trajectory foundry
            </h1>
            <p className="mt-1 max-w-3xl text-sm text-muted">
              Run pinned SWE-Gym rollouts, grade patches in fresh images, and
              shape executable trajectories into policy and verifier training
              files.
            </p>
          </div>
          <div
            className="flex flex-wrap gap-1 rounded border border-hairline bg-background/50 p-1"
            role="tablist"
            aria-label="Coding traces sections"
          >
            {TABS.map((item) => {
              const Icon = item.icon;
              const active = item.id === tab;
              return (
                <button
                  key={item.id}
                  type="button"
                  role="tab"
                  aria-selected={active}
                  onClick={() => setParams({ tab: item.id })}
                  className={cn(
                    "control flex items-center gap-1.5 rounded px-3 py-2 font-mono text-[11px]",
                    active
                      ? "bg-raised text-foreground shadow-sm"
                      : "text-muted hover:text-foreground",
                  )}
                >
                  <Icon className="h-3.5 w-3.5" aria-hidden />
                  {item.label}
                </button>
              );
            })}
          </div>
        </div>
      </section>
      {tab === "swe-gym" ? <SweGymPanel /> : null}
      {tab === "traces" ? <TrajectoriesPanel /> : null}
      {tab === "datasets" ? <ExportsPanel /> : null}
      {tab === "settings" ? <TraceSettingsPanel /> : null}
    </div>
  );
}

function SweGymPanel() {
  const deployments = useDeployments();
  const secrets = useSecrets();
  const experiments = useSweGymExperiments();
  const planMutation = usePlanSweGymExperiment();
  const createMutation = useCreateSweGymExperiment();
  const navigate = useNavigate();
  const [taskText, setTaskText] = useState("getmoto__moto-5752");
  const [config, setConfig] = useState<SweGymConfig>({
    preset: "custom",
    dataset: "lite",
    model_source: "lmw_deployment",
    model: "",
    temperatures: [0],
    rollouts_per_task: 1,
    max_turns: 30,
    context_limit: 32768,
    output_limit: 4096,
    workers: 1,
    per_node_workers: 1,
    runtime: "lmw_local",
    retry_limit: 1,
    timeout_seconds: 7200,
    image_prefix: "docker.io/xingyaoww",
  });
  const [plan, setPlan] = useState<SweGymPlan | null>(null);
  const modelSecrets = (secrets.data ?? []).filter(
    (secret) => secret.purpose === "model-provider",
  );
  const runtimeSecrets = (secrets.data ?? []).filter(
    (secret) => secret.purpose === "runtime-provider",
  );
  const set = <K extends keyof SweGymConfig>(
    key: K,
    value: SweGymConfig[K],
  ) => {
    setConfig((current) => ({ ...current, [key]: value }));
    setPlan(null);
  };
  const buildConfig = (): SweGymConfig => ({
    ...config,
    task_ids: taskText
      .split(/[\s,]+/)
      .map((value) => value.trim())
      .filter(Boolean),
  });
  const preflight = async () => {
    try {
      const next = await planMutation.mutateAsync(buildConfig());
      setPlan(next);
      toast.success("Pinned plan resolved");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Preflight failed");
    }
  };
  const launch = async () => {
    if (!plan) return;
    try {
      const experiment = await createMutation.mutateAsync({ plan });
      toast.success("Replication queued");
      if (experiment.id)
        navigate(`/coding-traces/replications/${experiment.id}`);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Launch failed");
    }
  };
  return (
    <div className="grid gap-4 2xl:grid-cols-[minmax(0,1.2fr)_minmax(420px,0.8fr)]">
      <section className="lmw-panel overflow-hidden">
        <header className="lmw-panel-head">
          <h2 className="lmw-label">replication control</h2>
          <span className="font-mono text-[10px] text-faint">
            pinned sources · hints off · browsing off
          </span>
        </header>
        <div className="grid gap-x-5 gap-y-4 p-4 md:grid-cols-2">
          <Field label="Collection preset">
            <Select
              value={config.preset ?? "custom"}
              onValueChange={(value) => {
                set("preset", value as SweGymConfig["preset"]);
                if (value.startsWith("paper")) {
                  setConfig((current) => ({
                    ...current,
                    max_turns: value === "paper-d2" ? 50 : 30,
                    temperatures: [0],
                    model_source: "external_api",
                  }));
                }
              }}
            >
              <SelectTrigger aria-label="Collection preset">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="custom">Custom matrix</SelectItem>
                <SelectItem value="paper-d0">Paper D0 · Lite t=0</SelectItem>
                <SelectItem value="paper-d1">
                  Paper D1 · Lite temperature sweep
                </SelectItem>
                <SelectItem value="paper-d2">
                  Paper D2 · 50-turn expansion
                </SelectItem>
              </SelectContent>
            </Select>
          </Field>
          <Field label="Dataset">
            <Select
              value={config.dataset}
              onValueChange={(value) =>
                set("dataset", value as "lite" | "full")
              }
            >
              <SelectTrigger aria-label="Dataset">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="lite">SWE-Gym Lite · 230</SelectItem>
                <SelectItem value="full">SWE-Gym full · 2,438</SelectItem>
              </SelectContent>
            </Select>
          </Field>
          <Field
            label="Exact task IDs"
            detail="Whitespace or comma separated; leave empty for the filtered dataset."
          >
            <textarea
              aria-label="Exact task IDs"
              value={taskText}
              onChange={(event) => {
                setTaskText(event.target.value);
                setPlan(null);
              }}
              className="min-h-20 w-full rounded border border-hairline bg-background px-3 py-2 font-mono text-xs text-foreground outline-none focus:border-primary/60"
            />
          </Field>
          <Field label="Model source">
            <Select
              value={config.model_source}
              onValueChange={(value) =>
                set("model_source", value as "lmw_deployment" | "external_api")
              }
            >
              <SelectTrigger aria-label="Model source">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="lmw_deployment">LMW deployment</SelectItem>
                <SelectItem value="external_api">External API</SelectItem>
              </SelectContent>
            </Select>
          </Field>
          {config.model_source === "lmw_deployment" ? (
            <Field label="LMW deployment">
              <Select
                value={config.deployment_id ?? ""}
                onValueChange={(value) => {
                  set("deployment_id", value);
                  const dep = deployments.data?.find(
                    (item) => item.id === value,
                  );
                  if (
                    dep?.model_capabilities &&
                    "model" in dep.model_capabilities
                  )
                    set("model", String(dep.model_capabilities.model));
                }}
              >
                <SelectTrigger aria-label="LMW deployment">
                  <SelectValue placeholder="Select a healthy deployment" />
                </SelectTrigger>
                <SelectContent>
                  {(deployments.data ?? [])
                    .filter((item) => item.desired_state === "running")
                    .map((item) => (
                      <SelectItem key={item.id} value={item.id}>
                        {shortId(item.id)} · {item.profile}
                      </SelectItem>
                    ))}
                </SelectContent>
              </Select>
            </Field>
          ) : (
            <>
              <Field label="Model API endpoint">
                <Input
                  aria-label="Model API endpoint"
                  value={config.endpoint ?? ""}
                  onChange={(event) => set("endpoint", event.target.value)}
                  placeholder="https://api.example/v1"
                />
              </Field>
              <Field label="Model provider secret">
                <Select
                  value={config.secret_reference ?? ""}
                  onValueChange={(value) => set("secret_reference", value)}
                >
                  <SelectTrigger aria-label="Model provider secret">
                    <SelectValue placeholder="model-provider secret" />
                  </SelectTrigger>
                  <SelectContent>
                    {modelSecrets.map((secret) => (
                      <SelectItem key={secret.id} value={secret.name}>
                        {secret.name}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </Field>
            </>
          )}
          <Field label="Model / checkpoint">
            <Input
              aria-label="Model or checkpoint"
              value={config.model}
              onChange={(event) => set("model", event.target.value)}
              placeholder="openai/model or served checkpoint"
            />
          </Field>
          <Field label="Runtime">
            <Select
              value={config.runtime}
              onValueChange={(value) =>
                set("runtime", value as "lmw_local" | "openhands_remote")
              }
            >
              <SelectTrigger aria-label="Runtime">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="lmw_local">LMW local sandbox</SelectItem>
                <SelectItem value="openhands_remote">
                  OpenHands remote runtime
                </SelectItem>
              </SelectContent>
            </Select>
          </Field>
          {config.runtime === "openhands_remote" ? (
            <>
              <Field label="Runtime endpoint">
                <Input
                  aria-label="Runtime endpoint"
                  value={config.runtime_endpoint ?? ""}
                  onChange={(event) =>
                    set("runtime_endpoint", event.target.value)
                  }
                />
              </Field>
              <Field label="Runtime provider secret">
                <Select
                  value={config.runtime_secret_reference ?? ""}
                  onValueChange={(value) =>
                    set("runtime_secret_reference", value)
                  }
                >
                  <SelectTrigger aria-label="Runtime provider secret">
                    <SelectValue placeholder="runtime-provider secret" />
                  </SelectTrigger>
                  <SelectContent>
                    {runtimeSecrets.map((secret) => (
                      <SelectItem key={secret.id} value={secret.name}>
                        {secret.name}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </Field>
            </>
          ) : null}
          <Field label="Temperatures">
            <Input
              aria-label="Temperatures"
              value={(config.temperatures ?? []).join(", ")}
              disabled={config.preset !== "custom"}
              onChange={(event) =>
                set(
                  "temperatures",
                  event.target.value
                    .split(",")
                    .map(Number)
                    .filter(Number.isFinite),
                )
              }
            />
          </Field>
          <Field label="Rollouts per task">
            <Input
              aria-label="Rollouts per task"
              type="number"
              min={1}
              value={config.rollouts_per_task}
              onChange={(event) =>
                set("rollouts_per_task", Number(event.target.value))
              }
            />
          </Field>
          <Field label="Max turns">
            <Input
              aria-label="Max turns"
              type="number"
              min={1}
              value={config.max_turns}
              onChange={(event) => set("max_turns", Number(event.target.value))}
            />
          </Field>
          <Field label="Global / per-node workers">
            <div className="grid grid-cols-2 gap-2">
              <Input
                aria-label="Global workers"
                type="number"
                min={1}
                value={config.workers}
                onChange={(event) => set("workers", Number(event.target.value))}
              />
              <Input
                aria-label="Per-node workers"
                type="number"
                min={1}
                value={config.per_node_workers}
                onChange={(event) =>
                  set("per_node_workers", Number(event.target.value))
                }
              />
            </div>
          </Field>
        </div>
        <div className="flex flex-wrap items-center gap-2 border-t border-hairline bg-background/35 px-4 py-3">
          <Button
            onClick={() => void preflight()}
            disabled={planMutation.isPending}
          >
            <Sparkles />
            {planMutation.isPending ? "resolving images…" : "run preflight"}
          </Button>
          <span className="font-mono text-[10px] text-faint">
            Dataset SHA + registry digests + live node capacity
          </span>
          {plan ? (
            <Button
              className="ml-auto"
              variant="secondary"
              onClick={() => void launch()}
              disabled={createMutation.isPending}
            >
              <Beaker />
              {createMutation.isPending ? "queuing…" : "launch replication"}
            </Button>
          ) : null}
        </div>
      </section>
      <div className="grid content-start gap-4">
        <PlanCard plan={plan} />
        <section className="lmw-panel">
          <header className="lmw-panel-head">
            <h2 className="lmw-label">recent replications</h2>
            <span className="font-mono text-[10px] text-faint">
              {experiments.data?.items.length ?? 0} runs
            </span>
          </header>
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Created</TableHead>
                  <TableHead>State</TableHead>
                  <TableHead className="text-right">Progress</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(experiments.data?.items ?? []).map((item) => (
                  <TableRow key={item.id}>
                    <TableCell>
                      <Link
                        to={`/coding-traces/replications/${item.id}`}
                        className="control font-mono text-xs hover:text-foreground"
                      >
                        {item.created_at
                          ? wallClock(item.created_at)
                          : shortId(item.id ?? "")}
                      </Link>
                    </TableCell>
                    <TableCell>
                      <StateBadge state={item.state} />
                    </TableCell>
                    <TableCell className="text-right font-mono text-xs tnum">
                      {item.completed_items ?? 0}/{item.total_items ?? 0}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
            {!experiments.isPending && !experiments.data?.items.length ? (
              <EmptyState
                className="m-3"
                title="No replications yet"
                hint="Resolve a pinned preflight plan, then launch it here."
              />
            ) : null}
          </div>
        </section>
      </div>
    </div>
  );
}

function Field({
  label,
  detail,
  children,
}: {
  label: string;
  detail?: string;
  children: React.ReactNode;
}) {
  return (
    <div className="grid content-start gap-1.5">
      <Label className="font-mono text-[10px] uppercase tracking-wider text-muted">
        {label}
      </Label>
      {children}
      {detail ? (
        <p className="font-mono text-[9px] leading-relaxed text-faint">
          {detail}
        </p>
      ) : null}
    </div>
  );
}

function PlanCard({ plan }: { plan: SweGymPlan | null }) {
  if (!plan) {
    return (
      <section className="lmw-panel border-dashed">
        <div className="flex min-h-52 flex-col items-center justify-center p-6 text-center">
          <div className="mb-3 rounded-full border border-hairline bg-raised p-3">
            <ShieldCheck className="h-5 w-5 text-violet" />
          </div>
          <p className="font-display text-sm font-medium">
            No immutable plan yet
          </p>
          <p className="mt-1 max-w-sm font-mono text-[10px] leading-relaxed text-faint">
            Preflight resolves pinned dataset rows, image manifest digests, and
            live execution capacity before anything runs.
          </p>
        </div>
      </section>
    );
  }
  const tasks = plan.tasks ?? [];
  const matrix =
    (
      plan as SweGymPlan & {
        sampling_matrix?: Array<{
          name: string;
          temperature: number;
          max_turns: number;
        }>;
      }
    ).sampling_matrix ?? [];
  return (
    <section className="lmw-panel overflow-hidden">
      <header className="lmw-panel-head">
        <Check className="h-3.5 w-3.5 text-ok" />
        <h2 className="lmw-label">plan sealed</h2>
        <span className="ml-auto font-mono text-[10px] text-faint">
          {plan.plan_digest.slice(0, 12)}…
        </span>
      </header>
      <div className="grid grid-cols-3 gap-4 p-4">
        <SectionStat label="tasks" value={tasks.length} />
        <SectionStat label="rollouts" value={plan.total_rollouts} />
        <SectionStat
          label="nodes"
          value={Number(plan.node_capacity?.online_nodes ?? 0)}
        />
      </div>
      <div className="border-t border-hairline px-4 py-3">
        <p className="font-mono text-[9px] uppercase tracking-wider text-faint">
          sampling matrix
        </p>
        <div className="mt-2 flex flex-wrap gap-1.5">
          {matrix.map((item) => (
            <Badge
              key={item.name}
              variant="outline"
              className="font-mono text-[10px]"
            >
              {item.name} · t={item.temperature} · {item.max_turns} turns
            </Badge>
          ))}
        </div>
        {(plan.warnings ?? []).map((warning) => (
          <p
            key={warning}
            className="mt-2 border-l-2 border-warn/50 pl-2 font-mono text-[9px] leading-relaxed text-warn"
          >
            {warning}
          </p>
        ))}
      </div>
    </section>
  );
}

function TrajectoriesPanel() {
  const [state, setState] = useState("all");
  const [task, setTask] = useState("");
  const traces = useCodingTraces({
    state: state === "all" ? undefined : state,
    taskId: task || undefined,
  });
  return (
    <section className="lmw-panel">
      <header className="lmw-panel-head">
        <h2 className="lmw-label">sanitized trajectories</h2>
        <Input
          aria-label="Filter task ID"
          className="ml-auto h-7 w-52 font-mono text-xs"
          value={task}
          onChange={(event) => setTask(event.target.value)}
          placeholder="filter task id"
        />
        <Select value={state} onValueChange={setState}>
          <SelectTrigger aria-label="Filter trajectory state" className="h-7 w-32 font-mono text-xs">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">all states</SelectItem>
            <SelectItem value="completed">completed</SelectItem>
            <SelectItem value="interrupted">interrupted</SelectItem>
          </SelectContent>
        </Select>
      </header>
      {traces.isError ? (
        <EmptyState
          className="m-3"
          title="Cannot load trajectories"
          detail={
            traces.error instanceof Error ? traces.error.message : undefined
          }
        />
      ) : (
        <div className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Task</TableHead>
                <TableHead>Model</TableHead>
                <TableHead>Outcome</TableHead>
                <TableHead className="text-right">Turns</TableHead>
                <TableHead className="text-right">Tokens</TableHead>
                <TableHead className="text-right">Redactions</TableHead>
                <TableHead>Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(traces.data?.items ?? []).map((trace) => (
                <TableRow key={trace.id}>
                  <TableCell>
                    <Link
                      to={`/coding-traces/${trace.id}`}
                      className="control font-mono text-xs text-foreground hover:text-violet"
                    >
                      {trace.task_id}
                    </Link>
                  </TableCell>
                  <TableCell className="max-w-48 truncate font-mono text-[11px] text-muted">
                    {trace.model}
                  </TableCell>
                  <TableCell>
                    {trace.success_label === true ? (
                      <StateBadge state="resolved" />
                    ) : trace.success_label === false ? (
                      <StateBadge state="unresolved" />
                    ) : (
                      <StateBadge state={trace.state} />
                    )}
                  </TableCell>
                  <TableCell className="text-right font-mono text-xs tnum">
                    {trace.turn_count}
                  </TableCell>
                  <TableCell className="text-right font-mono text-xs tnum">
                    {number(trace.token_count)}
                  </TableCell>
                  <TableCell className="text-right font-mono text-xs tnum">
                    {trace.redaction_count}
                  </TableCell>
                  <TableCell className="font-mono text-[10px] text-faint">
                    {wallClock(trace.created_at)}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          {!traces.isPending && !traces.data?.items.length ? (
            <EmptyState
              className="m-3"
              title="No trajectories match"
              hint="Every SWE-Gym rollout appears here once its recorder starts."
            />
          ) : null}
        </div>
      )}
    </section>
  );
}

function ExportsPanel() {
  const exportsQuery = useCodingTraceExports();
  const create = useCreateCodingTraceExport();
  const [form, setForm] = useState<TraceExportCreate>({
    tokenizer: "cl100k_base",
    max_context_tokens: 32768,
    success_cap_per_task: 2,
    seed: 0,
  });
  const submit = async () => {
    try {
      await create.mutateAsync(form);
      toast.success("Dataset export queued");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Export failed");
    }
  };
  return (
    <div className="grid gap-4 xl:grid-cols-[360px_1fr]">
      <section className="lmw-panel">
        <header className="lmw-panel-head">
          <h2 className="lmw-label">new training snapshot</h2>
        </header>
        <div className="grid gap-4 p-4">
          <Field label="Tokenizer">
            <Input
              aria-label="Tokenizer"
              value={form.tokenizer ?? ""}
              onChange={(event) =>
                setForm((current) => ({
                  ...current,
                  tokenizer: event.target.value,
                }))
              }
            />
          </Field>
          <Field
            label="Context ceiling"
            detail="Policy rows at or above this exact tokenizer count are excluded."
          >
            <Input
              aria-label="Context ceiling"
              type="number"
              min={1024}
              value={form.max_context_tokens ?? 32768}
              onChange={(event) =>
                setForm((current) => ({
                  ...current,
                  max_context_tokens: Number(event.target.value),
                }))
              }
            />
          </Field>
          <Field label="Success cap / task">
            <Input
              aria-label="Success cap per task"
              type="number"
              min={1}
              value={form.success_cap_per_task ?? 2}
              onChange={(event) =>
                setForm((current) => ({
                  ...current,
                  success_cap_per_task: Number(event.target.value),
                }))
              }
            />
          </Field>
          <Field label="Balance seed">
            <Input
              aria-label="Balance seed"
              type="number"
              value={form.seed ?? 0}
              onChange={(event) =>
                setForm((current) => ({
                  ...current,
                  seed: Number(event.target.value),
                }))
              }
            />
          </Field>
        </div>
        <div className="border-t border-hairline p-3">
          <Button
            className="w-full"
            onClick={() => void submit()}
            disabled={create.isPending}
          >
            <Archive />
            {create.isPending ? "queuing…" : "build canonical + SFT + verifier"}
          </Button>
        </div>
      </section>
      <section className="lmw-panel">
        <header className="lmw-panel-head">
          <h2 className="lmw-label">dataset exports</h2>
          <span className="font-mono text-[10px] text-faint">
            deterministic tar.gz
          </span>
        </header>
        <div className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Created</TableHead>
                <TableHead>State</TableHead>
                <TableHead className="text-right">Canonical</TableHead>
                <TableHead className="text-right">Policy</TableHead>
                <TableHead className="text-right">Verifier</TableHead>
                <TableHead>Manifest</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {(exportsQuery.data ?? []).map((item) => (
                <TableRow key={item.id}>
                  <TableCell className="font-mono text-[10px] text-faint">
                    {item.created_at ? wallClock(item.created_at) : "—"}
                  </TableCell>
                  <TableCell>
                    <StateBadge state={item.state} />
                  </TableCell>
                  <TableCell className="text-right font-mono text-xs tnum">
                    {item.canonical_count ?? 0}
                  </TableCell>
                  <TableCell className="text-right font-mono text-xs tnum">
                    {item.policy_count ?? 0}
                  </TableCell>
                  <TableCell className="text-right font-mono text-xs tnum">
                    {item.verifier_count ?? 0}
                  </TableCell>
                  <TableCell className="font-mono text-[10px] text-faint">
                    {item.manifest_digest
                      ? `${item.manifest_digest.slice(0, 12)}…`
                      : "—"}
                  </TableCell>
                  <TableCell className="text-right">
                    {item.state === "completed" && item.id ? (
                      <Button size="sm" variant="ghost" asChild>
                        <a href={codingTraceExportURL(item.id)}>
                          <Download />
                          download
                        </a>
                      </Button>
                    ) : null}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          {!exportsQuery.isPending && !exportsQuery.data?.length ? (
            <EmptyState
              className="m-3"
              title="No exports yet"
              hint="Snapshots are content-stable and carry all provenance in manifest.json."
            />
          ) : null}
        </div>
      </section>
    </div>
  );
}

function TraceSettingsPanel() {
  const query = useModuleSettings("coding-traces");
  const put = usePutModuleSettings();
  const [value, setValue] = useState<CodingTraceSettings>({
    capture_reasoning: true,
    retention_days: 0,
    export_max_context_tokens: 32768,
    export_success_cap_per_task: 2,
  });
  useEffect(() => {
    const stored = query.data?.settings as
      | Partial<CodingTraceSettings>
      | undefined;
    if (stored) setValue((current) => ({ ...current, ...stored }));
  }, [query.data]);
  const save = async () => {
    if (!query.data) return;
    try {
      await put.mutateAsync({
        moduleId: "coding-traces",
        body: {
          module: "coding-traces",
          settings: value,
          version: query.data.version,
        },
        ifMatch: query.data.version,
      });
      toast.success("Coding trace settings saved");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Save failed");
    }
  };
  return (
    <section className="lmw-panel max-w-3xl">
      <header className="lmw-panel-head">
        <h2 className="lmw-label">capture & export defaults</h2>
        {query.data ? (
          <span className="ml-auto font-mono text-[9px] text-faint">
            etag {query.data.version.slice(0, 12)}…
          </span>
        ) : null}
      </header>
      <div className="grid gap-5 p-4 sm:grid-cols-2">
        <div className="flex items-center justify-between rounded border border-hairline bg-background/40 p-3 sm:col-span-2">
          <div>
            <p className="font-display text-sm font-medium">
              Capture reasoning
            </p>
            <p className="mt-1 font-mono text-[9px] text-faint">
              Preserve reasoning-bearing model responses after mandatory
              redaction.
            </p>
          </div>
          <Switch
            aria-label="Capture reasoning"
            checked={value.capture_reasoning}
            onCheckedChange={(checked) =>
              setValue((current) => ({
                ...current,
                capture_reasoning: checked,
              }))
            }
          />
        </div>
        <Field
          label="Retention days"
          detail="0 keeps trajectories indefinitely. Pinned traces are never removed."
        >
          <Input
            aria-label="Retention days"
            type="number"
            min={0}
            value={value.retention_days}
            onChange={(event) =>
              setValue((current) => ({
                ...current,
                retention_days: Number(event.target.value),
              }))
            }
          />
        </Field>
        <Field label="Default context tokens">
          <Input
            aria-label="Default context tokens"
            type="number"
            min={1024}
            value={value.export_max_context_tokens}
            onChange={(event) =>
              setValue((current) => ({
                ...current,
                export_max_context_tokens: Number(event.target.value),
              }))
            }
          />
        </Field>
        <Field label="Success cap per task">
          <Input
            aria-label="Success cap per task"
            type="number"
            min={1}
            value={value.export_success_cap_per_task}
            onChange={(event) =>
              setValue((current) => ({
                ...current,
                export_success_cap_per_task: Number(event.target.value),
              }))
            }
          />
        </Field>
      </div>
      <div className="flex justify-end border-t border-hairline p-3">
        <Button
          onClick={() => void save()}
          disabled={!query.data || put.isPending}
        >
          <Save />
          {put.isPending ? "saving…" : "save settings"}
        </Button>
      </div>
    </section>
  );
}

export function CodingTraceDetailRoute() {
  const { id } = useParams();
  const detail = useCodingTrace(id);
  const events = useCodingTraceEvents(id);
  const pin = usePinCodingTrace();
  if (detail.isPending)
    return <p className="font-mono text-xs text-faint">loading trajectory…</p>;
  if (!detail.data) return <EmptyState title="Trajectory not found" />;
  const trace = detail.data as CodingTrace & {
    verification?: Record<string, unknown>;
  };
  return (
    <div className="grid gap-4">
      <section className="lmw-panel">
        <header className="lmw-panel-head">
          <Link
            to="/coding-traces?tab=traces"
            className="control font-mono text-[10px] text-muted hover:text-foreground"
          >
            ← trajectories
          </Link>
          <h1 className="lmw-label">{trace.task_id}</h1>
          <StateBadge
            state={
              trace.success_label === true
                ? "resolved"
                : trace.success_label === false
                  ? "unresolved"
                  : trace.state
            }
          />
          <Button
            size="sm"
            variant="ghost"
            className="ml-auto"
            onClick={() =>
              void pin.mutateAsync({ id: trace.id, pinned: !trace.pinned })
            }
          >
            {trace.pinned ? <PinOff /> : <Pin />}
            {trace.pinned ? "unpin" : "pin"}
          </Button>
        </header>
        <div className="grid gap-4 p-4 sm:grid-cols-2 lg:grid-cols-5">
          <SectionStat label="model" value={trace.model} />
          <SectionStat label="turns" value={trace.turn_count} />
          <SectionStat label="tokens" value={number(trace.token_count)} />
          <SectionStat label="redactions" value={trace.redaction_count} />
          <SectionStat
            label="digest"
            value={trace.digest ? `${trace.digest.slice(0, 10)}…` : "—"}
          />
        </div>
      </section>
      <section className="grid gap-4 xl:grid-cols-[1.2fr_0.8fr]">
        <div className="lmw-panel">
          <header className="lmw-panel-head">
            <h2 className="lmw-label">ordered interaction</h2>
            <span className="font-mono text-[10px] text-faint">
              {events.data?.items.length ?? 0} events
            </span>
          </header>
          <div className="max-h-[720px] overflow-y-auto p-3">
            {(events.data?.items ?? []).map((event) => (
              <article
                key={event.event_id}
                className="grid grid-cols-[42px_1fr] gap-3 border-b border-hairline py-3 last:border-0"
              >
                <span className="font-mono text-[10px] text-faint">
                  #{event.sequence}
                </span>
                <div className="min-w-0">
                  <div className="flex items-center gap-2">
                    <Badge variant="outline" className="font-mono text-[9px]">
                      {event.kind}
                    </Badge>
                    <span className="font-mono text-[9px] text-faint">
                      {event.input_tokens}+{event.output_tokens} tok
                    </span>
                  </div>
                  <pre className="mt-2 overflow-x-auto whitespace-pre-wrap break-words font-mono text-[10px] leading-relaxed text-muted">
                    {JSON.stringify(event.payload, null, 2)}
                  </pre>
                </div>
              </article>
            ))}
          </div>
        </div>
        <div className="grid content-start gap-4">
          <CodePanel
            title="final diff"
            code={trace.final_diff ?? "No patch produced."}
          />
          <CodePanel
            title="verification"
            code={JSON.stringify(trace.verification ?? {}, null, 2)}
          />
        </div>
      </section>
    </div>
  );
}

function CodePanel({ title, code }: { title: string; code: string }) {
  return (
    <section className="lmw-panel overflow-hidden">
      <header className="lmw-panel-head">
        <h2 className="lmw-label">{title}</h2>
      </header>
      <pre className="max-h-96 overflow-auto whitespace-pre p-3 font-mono text-[10px] leading-relaxed text-muted">
        {code}
      </pre>
    </section>
  );
}

export function SweGymReplicationDetailRoute() {
  const { id } = useParams();
  const query = useSweGymExperiment(id);
  const cancel = useCancelSweGymExperiment();
  const resume = useResumeSweGymExperiment();
  if (!query.data)
    return <p className="font-mono text-xs text-faint">loading replication…</p>;
  const experiment = query.data;
  const items =
    (
      experiment as typeof experiment & {
        work_items?: Array<Record<string, unknown>>;
      }
    ).work_items ?? [];
  const terminal = ["completed", "failed", "cancelled"].includes(
    experiment.state ?? "",
  );
  return (
    <div className="grid gap-4">
      <section className="lmw-panel">
        <header className="lmw-panel-head">
          <Link
            to="/coding-traces?tab=swe-gym"
            className="control font-mono text-[10px] text-muted hover:text-foreground"
          >
            ← replications
          </Link>
          <h1 className="lmw-label">
            replication {shortId(experiment.id ?? "")}
          </h1>
          <StateBadge state={experiment.state} />
          <div className="ml-auto flex gap-2">
            {!terminal ? (
              <Button
                size="sm"
                variant="destructive"
                onClick={() => id && void cancel.mutateAsync(id)}
              >
                <CircleStop />
                cancel
              </Button>
            ) : null}
            {["failed", "cancelled"].includes(experiment.state ?? "") ? (
              <Button
                size="sm"
                onClick={() => id && void resume.mutateAsync(id)}
              >
                <RotateCcw />
                resume remaining
              </Button>
            ) : null}
          </div>
        </header>
        <div className="grid gap-4 p-4 sm:grid-cols-2 lg:grid-cols-5">
          <SectionStat label="total" value={experiment.total_items ?? 0} />
          <SectionStat
            label="completed"
            value={experiment.completed_items ?? 0}
          />
          <SectionStat
            label="resolved"
            value={experiment.resolved_items ?? 0}
            accent="text-ok"
          />
          <SectionStat
            label="unresolved"
            value={experiment.unresolved_items ?? 0}
            accent="text-warn"
          />
          <SectionStat
            label="infra"
            value={experiment.infrastructure_errors ?? 0}
            accent="text-danger"
          />
        </div>
      </section>
      <section className="lmw-panel">
        <header className="lmw-panel-head">
          <h2 className="lmw-label">task × rollout ledger</h2>
          <span className="font-mono text-[10px] text-faint">
            retry-safe child runs
          </span>
        </header>
        <div className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Task</TableHead>
                <TableHead className="text-right">Rollout</TableHead>
                <TableHead className="text-right">Attempt</TableHead>
                <TableHead>State</TableHead>
                <TableHead>Trace</TableHead>
                <TableHead>Node</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {items.map((item) => (
                <TableRow key={String(item.id)}>
                  <TableCell className="font-mono text-xs">
                    {String(item.task_id)}
                  </TableCell>
                  <TableCell className="text-right font-mono text-xs tnum">
                    {String(item.rollout_index)}
                  </TableCell>
                  <TableCell className="text-right font-mono text-xs tnum">
                    {String(item.attempt)}
                  </TableCell>
                  <TableCell>
                    <StateBadge state={String(item.state)} />
                  </TableCell>
                  <TableCell>
                    {item.trace_id ? (
                      <Link
                        to={`/coding-traces/${String(item.trace_id)}`}
                        className="control font-mono text-xs hover:text-violet"
                      >
                        {shortId(String(item.trace_id))}
                      </Link>
                    ) : (
                      "—"
                    )}
                  </TableCell>
                  <TableCell className="font-mono text-[10px] text-faint">
                    {item.node_id ? shortId(String(item.node_id)) : "—"}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      </section>
    </div>
  );
}
