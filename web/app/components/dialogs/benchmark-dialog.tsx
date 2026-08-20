import { useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router";
import { toast } from "sonner";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "~/components/ui/dialog";
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
import { useCreateBenchmark, useDeployments } from "~/lib/queries";

/** The six grader languages (closed set, benchmarks module). */
export const BENCHMARK_LANGUAGES = ["python", "javascript", "go", "rust", "cpp", "java"] as const;

const DEFAULT_PROMPTS = 8;
const DEFAULT_MAX_TOKENS = 512;

/**
 * Launch a benchmark run against a deployment. Languages multi-select
 * (six), prompts/language and max_tokens with the module defaults shown.
 */
export function BenchmarkDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
}) {
  const navigate = useNavigate();
  const { data: deployments } = useDeployments();
  const create = useCreateBenchmark();

  const [deploymentId, setDeploymentId] = useState("");
  const [languages, setLanguages] = useState<string[]>([]);
  const [prompts, setPrompts] = useState(DEFAULT_PROMPTS);
  const [maxTokens, setMaxTokens] = useState(DEFAULT_MAX_TOKENS);
  const [reason, setReason] = useState("");

  const healthy = useMemo(
    () => (deployments ?? []).filter((d) => d.observed_state === "healthy"),
    [deployments],
  );

  useEffect(() => {
    if (open) {
      setDeploymentId("");
      setLanguages([]);
      setPrompts(DEFAULT_PROMPTS);
      setMaxTokens(DEFAULT_MAX_TOKENS);
      setReason("");
      create.reset();
    }
  }, [open, create]);

  const toggleLanguage = (lang: string) => {
    setLanguages((prev) => (prev.includes(lang) ? prev.filter((l) => l !== lang) : [...prev, lang]));
  };

  const onSubmit = async () => {
    if (!deploymentId || languages.length === 0) return;
    try {
      const run = await create.mutateAsync({
        deployment_id: deploymentId,
        languages,
        prompts_per_language: prompts,
        max_tokens: maxTokens,
        temperature: 0,
        ...(reason ? { reason } : {}),
      });
      toast.success("Benchmark run launched", { description: run.kind });
      onOpenChange(false);
      navigate(`/runs/${run.id}`);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "benchmark launch failed");
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg max-sm:max-h-[94dvh] max-sm:overflow-auto max-sm:rounded-none max-sm:inset-0 max-sm:m-0 max-sm:h-full max-sm:max-w-none">
        <DialogHeader>
          <DialogTitle className="font-display text-lg font-semibold tracking-wide">
            Run benchmark
          </DialogTitle>
          <DialogDescription>
            One-shot request dispatch against a healthy deployment. Runs one digest-pinned grader
            per language; results land in Benchmarks and Runs.
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label>Deployment</Label>
            <Select value={deploymentId} onValueChange={setDeploymentId}>
              <SelectTrigger className="w-full" aria-label="Deployment">
                <SelectValue placeholder="select healthy deployment" />
              </SelectTrigger>
              <SelectContent>
                {healthy.map((d) => (
                  <SelectItem key={d.id} value={d.id}>
                    <span className="flex items-baseline gap-2">
                      <span>{d.recipe_name}@{d.profile}</span>
                      {d.endpoint ? (
                        <span className="font-mono text-[11px] text-muted-foreground">
                          {d.endpoint.host}:{d.endpoint.port}
                        </span>
                      ) : null}
                    </span>
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {healthy.length === 0 ? (
              <p className="font-mono text-[11px] text-muted">no healthy deployments available</p>
            ) : null}
          </div>

          <div className="grid gap-2">
            <Label>Languages</Label>
            <div className="flex flex-wrap gap-1.5" role="group" aria-label="Benchmark languages">
              {BENCHMARK_LANGUAGES.map((lang) => {
                const on = languages.includes(lang);
                return (
                  <button
                    key={lang}
                    type="button"
                    aria-pressed={on}
                    onClick={() => toggleLanguage(lang)}
                    className={
                      on
                        ? "control rounded border border-violet/60 bg-violet/15 px-2.5 py-1 font-mono text-xs text-violet"
                        : "control rounded border border-hairline bg-raised px-2.5 py-1 font-mono text-xs text-muted hover:text-foreground"
                    }
                  >
                    {on ? "✓ " : ""}
                    {lang}
                  </button>
                );
              })}
            </div>
          </div>

          <div className="grid grid-cols-2 gap-3">
            <div className="grid gap-2">
              <Label htmlFor="bm-prompts">
                prompts/language <span className="text-muted-foreground">(default {DEFAULT_PROMPTS})</span>
              </Label>
              <Input
                id="bm-prompts"
                type="number"
                min={1}
                max={256}
                value={prompts}
                onChange={(e) => setPrompts(Number(e.target.value) || 1)}
                className="font-mono"
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="bm-max">
                max_tokens <span className="text-muted-foreground">(default {DEFAULT_MAX_TOKENS})</span>
              </Label>
              <Input
                id="bm-max"
                type="number"
                min={16}
                max={16384}
                value={maxTokens}
                onChange={(e) => setMaxTokens(Number(e.target.value) || 16)}
                className="font-mono"
              />
            </div>
          </div>

          <div className="grid gap-2">
            <Label htmlFor="bm-reason">Reason (optional)</Label>
            <Input
              id="bm-reason"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              placeholder="e.g. after abliteration regression check"
            />
          </div>
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            onClick={() => void onSubmit()}
            disabled={!deploymentId || languages.length === 0 || create.isPending}
          >
            {create.isPending ? "launching…" : `Run ${languages.length || "…"} language${languages.length === 1 ? "" : "s"}`}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
