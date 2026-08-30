import { useEffect, useState } from "react";
import { Link } from "react-router";
import { toast } from "sonner";
import { ArrowLeft, Box, Check, PackageCheck, Save } from "lucide-react";

import { Button } from "~/components/ui/button";
import { Badge } from "~/components/ui/badge";
import { StatusDot } from "~/components/status-dot";
import { installRecipeDraft, packageRecipeDraft, updateRecipeDraft } from "~/lib/api";
import { useRecipeDraft } from "~/lib/queries";
import { useTailPathParam } from "~/lib/path-param";

const steps = ["Source", "Manifest", "Artifacts", "Workload", "Assets", "Review"] as const;
type Candidate = { path: string; size: number; sha256: string; binary?: boolean };
type Finding = { code?: string; severity?: string; path?: string; message?: string };

export default function RecipeDraftRoute() {
  const id = useTailPathParam();
  const draftQuery = useRecipeDraft(id);
  const draft = draftQuery.data;
  const [step, setStep] = useState<(typeof steps)[number]>("Source");
  const [manifestText, setManifestText] = useState("{}");
  const [selected, setSelected] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (!draft) return;
    setManifestText(JSON.stringify(draft.manifest, null, 2));
    setSelected(draft.selected_assets ?? []);
  }, [draft]);

  const candidates = (draft?.candidates ?? []) as unknown as Candidate[];
  const findings = (draft?.diagnostics ?? []) as unknown as Finding[];
  const selectedCandidates = candidates.filter((candidate) => selected.includes(candidate.sha256));

  const save = async () => {
    if (!draft) return;
    setBusy(true);
    try {
      const manifest = JSON.parse(manifestText) as Record<string, unknown>;
      await updateRecipeDraft(draft.id, draft.version, { manifest, selected_assets: selected });
      await draftQuery.refetch();
      toast.success("Draft validated");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Draft update failed");
    } finally {
      setBusy(false);
    }
  };

  const packageDraft = async () => {
    if (!draft) return;
    setBusy(true);
    try {
      await packageRecipeDraft(draft.id);
      await draftQuery.refetch();
      toast.success("Immutable OCI package created");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Packaging failed");
    } finally {
      setBusy(false);
    }
  };

  const installDraft = async () => {
    if (!draft) return;
    setBusy(true);
    try {
      const installed = await installRecipeDraft(draft.id);
      await draftQuery.refetch();
      toast.success(`Installed ${installed.name}@${installed.version}`);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Install failed");
    } finally {
      setBusy(false);
    }
  };

  if (!draft) return <main className="p-6 font-mono text-sm text-muted">loading fabrication record…</main>;

  return (
    <main className="flex min-h-full flex-col bg-background">
      <header className="flex flex-wrap items-center gap-3 border-b border-hairline bg-panel px-4 py-3 lg:px-6">
        <Link to="/library/builder" className="control inline-flex items-center gap-1 text-xs text-muted hover:text-foreground">
          <ArrowLeft className="size-3.5" aria-hidden /> builder
        </Link>
        <div className="h-5 w-px bg-hairline" aria-hidden />
        <StatusDot state={draft.state} pulse={draft.state === "analyzing"} />
        <div>
          <p className="font-mono text-xs">{draft.resolved_commit || draft.id}</p>
          <p className="text-[11px] text-muted">version {draft.version} · {draft.state}</p>
        </div>
        {draft.package_digest ? <Badge className="ml-auto font-mono">{draft.package_digest.slice(0, 19)}…</Badge> : null}
      </header>

      <nav aria-label="Builder steps" className="grid grid-cols-3 border-b border-hairline bg-raised md:grid-cols-6">
        {steps.map((item, index) => (
          <button
            key={item}
            type="button"
            onClick={() => setStep(item)}
            className={`control border-r border-hairline px-3 py-2 text-left font-display text-xs uppercase tracking-widest ${step === item ? "bg-primary/10 text-primary" : "text-muted hover:text-foreground"}`}
          >
            <span className="mr-2 font-mono text-[9px] text-faint">0{index + 1}</span>{item}
          </button>
        ))}
      </nav>

      <div className="grid flex-1 lg:grid-cols-[minmax(0,1fr)_340px]">
        <section className="min-w-0 p-4 lg:p-6">
          {step === "Source" ? (
            <div className="space-y-4">
              <h1 className="font-display text-2xl font-semibold">Resolved source</h1>
              <dl className="grid gap-px border border-hairline bg-hairline sm:grid-cols-2">
                {Object.entries(draft.source).map(([key, value]) => (
                  <div key={key} className="bg-panel p-3"><dt className="lmw-label">{key}</dt><dd className="mt-1 break-all font-mono text-xs">{String(value)}</dd></div>
                ))}
                <div className="bg-panel p-3"><dt className="lmw-label">tree</dt><dd className="mt-1 break-all font-mono text-xs">{draft.resolved_tree}</dd></div>
              </dl>
            </div>
          ) : null}

          {step === "Manifest" || step === "Artifacts" || step === "Workload" ? (
            <div className="space-y-3">
              <div className="flex items-end gap-3"><div><p className="lmw-label">Declarative control surface</p><h1 className="font-display text-2xl font-semibold">Manifest editor</h1></div><Button className="ml-auto" size="sm" disabled={busy} onClick={save}><Save aria-hidden /> save + validate</Button></div>
              <textarea
                aria-label="Recipe manifest JSON"
                value={manifestText}
                onChange={(event) => setManifestText(event.target.value)}
                spellCheck={false}
                className="min-h-[58vh] w-full resize-y rounded-md border border-hairline bg-card p-4 font-mono text-xs leading-relaxed text-foreground outline-none focus:border-primary focus:ring-2 focus:ring-primary/20"
              />
            </div>
          ) : null}

          {step === "Assets" ? (
            <div className="space-y-3">
              <p className="lmw-label">Hashed candidate store</p><h1 className="font-display text-2xl font-semibold">Select package assets</h1>
              <div className="divide-y divide-hairline border border-hairline bg-panel">
                {candidates.map((candidate) => (
                  <label key={`${candidate.path}-${candidate.sha256}`} className="control grid cursor-pointer grid-cols-[auto_1fr_auto] items-center gap-3 px-3 py-2 hover:bg-raised">
                    <input type="checkbox" checked={selected.includes(candidate.sha256)} onChange={(event) => setSelected((current) => event.target.checked ? [...current, candidate.sha256] : current.filter((hash) => hash !== candidate.sha256))} />
                    <span className="min-w-0 truncate font-mono text-xs">{candidate.path}</span>
                    <span className="font-mono text-[10px] text-faint">{candidate.size} B{candidate.binary ? " · binary" : ""}</span>
                  </label>
                ))}
              </div>
              <Button disabled={busy} onClick={save}><Check aria-hidden /> confirm selection</Button>
            </div>
          ) : null}

          {step === "Review" ? (
            <div className="space-y-5">
              <div><p className="lmw-label">Consequence review</p><h1 className="font-display text-2xl font-semibold">Package and install</h1></div>
              <div className="grid gap-px border border-hairline bg-hairline sm:grid-cols-3">
                <div className="bg-panel p-4"><p className="lmw-label">diagnostics</p><p className="mt-2 font-mono text-2xl">{findings.length}</p></div>
                <div className="bg-panel p-4"><p className="lmw-label">selected assets</p><p className="mt-2 font-mono text-2xl">{selectedCandidates.length}</p></div>
                <div className="bg-panel p-4"><p className="lmw-label">state</p><p className="mt-2 font-mono text-sm text-primary">{draft.state}</p></div>
              </div>
              <div className="flex flex-wrap gap-2"><Button disabled={busy || draft.state !== "valid"} onClick={packageDraft}><Box aria-hidden /> package OCI</Button><Button disabled={busy || draft.state !== "packaged"} variant="outline" onClick={installDraft}><PackageCheck aria-hidden /> accept permissions + install</Button></div>
            </div>
          ) : null}
        </section>

        <aside className="border-t border-hairline bg-panel p-4 lg:border-l lg:border-t-0">
          <p className="lmw-label mb-3">Diagnostics / evidence</p>
          <div className="space-y-2">
            {findings.map((finding, index) => (
              <article key={`${finding.code}-${index}`} className="border-l-2 border-warning bg-warning/5 p-2">
                <p className="font-mono text-[10px] text-warning">{finding.code ?? "diagnostic"} {finding.path}</p>
                <p className="mt-1 text-xs text-muted">{finding.message}</p>
              </article>
            ))}
            {!findings.length ? <p className="text-xs text-success">No validation findings.</p> : null}
          </div>
        </aside>
      </div>
    </main>
  );
}
