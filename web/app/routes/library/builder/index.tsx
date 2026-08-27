import { useState } from "react";
import { Link } from "react-router";
import { toast } from "sonner";
import { Hammer, Plus, RefreshCcw } from "lucide-react";

import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { Label } from "~/components/ui/label";
import { StatusDot } from "~/components/status-dot";
import { useCreateRecipeDraft, useRecipeDrafts } from "~/lib/queries";
import { shortId, wallClock } from "~/lib/format";

export default function RecipeBuilderIndex() {
  const drafts = useRecipeDrafts();
  const createDraft = useCreateRecipeDraft();
  const [remote, setRemote] = useState("");
  const [revision, setRevision] = useState("");
  const [path, setPath] = useState("");

  const submit = async () => {
    try {
      const result = await createDraft.mutateAsync({ remote, revision, ...(path ? { path } : {}) });
      toast.success("Source inspection queued", { description: `Run ${shortId(result.run_id)}` });
      setRemote("");
      setRevision("");
      setPath("");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Draft analysis failed");
    }
  };

  return (
    <main className="flex flex-col gap-4 p-4 lg:p-6">
      <header className="flex flex-wrap items-end gap-3 border-b border-hairline pb-4">
        <div>
          <p className="lmw-label text-primary">Library / Fabrication bay</p>
          <h1 className="font-display text-3xl font-semibold tracking-wide">Recipe builder</h1>
          <p className="max-w-2xl text-sm text-muted">
            Inspect immutable Git sources, then author the declarative package. Repository scripts are evidence only—never execution inputs.
          </p>
        </div>
        <Button variant="outline" size="sm" className="ml-auto" onClick={() => drafts.refetch()}>
          <RefreshCcw aria-hidden /> refresh
        </Button>
      </header>

      <section className="grid gap-px border border-hairline bg-hairline lg:grid-cols-[1.2fr_.8fr]">
        <div className="bg-panel p-4">
          <div className="mb-4 flex items-center gap-2">
            <Plus className="size-4 text-primary" aria-hidden />
            <h2 className="lmw-label">Inspect pinned source</h2>
          </div>
          <div className="grid gap-3 md:grid-cols-2">
            <div className="flex flex-col gap-1.5 md:col-span-2">
              <Label htmlFor="builder-remote">Git remote</Label>
              <Input id="builder-remote" value={remote} onChange={(event) => setRemote(event.target.value)} placeholder="https://github.com/owner/repository" />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="builder-revision">Branch, tag, or commit</Label>
              <Input id="builder-revision" value={revision} onChange={(event) => setRevision(event.target.value)} placeholder="main" />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="builder-path">Subpath</Label>
              <Input id="builder-path" value={path} onChange={(event) => setPath(event.target.value)} placeholder="optional/package/path" />
            </div>
          </div>
          <Button className="mt-4" disabled={!remote || !revision || createDraft.isPending} onClick={submit}>
            <Hammer aria-hidden /> {createDraft.isPending ? "queuing…" : "Analyze source"}
          </Button>
        </div>
        <aside className="bg-raised p-4 text-xs text-muted">
          <p className="lmw-label mb-3">Inspection boundary</p>
          <ul className="space-y-2 font-mono">
            <li>01 / resolves one full commit and tree</li>
            <li>02 / hashes bounded regular files</li>
            <li>03 / flags symlinks, binaries, Docker lifecycle</li>
            <li>04 / deletes the executable checkout</li>
          </ul>
        </aside>
      </section>

      <section className="border border-hairline bg-panel">
        <div className="flex items-center border-b border-hairline px-4 py-2">
          <h2 className="lmw-label">Draft ledger</h2>
          <span className="ml-auto font-mono text-[11px] text-faint">{drafts.data?.length ?? 0} drafts</span>
        </div>
        <div className="divide-y divide-hairline">
          {(drafts.data ?? []).map((draft) => (
            <Link key={draft.id} to={`/library/builder/${draft.id}`} className="control grid gap-2 px-4 py-3 hover:bg-raised sm:grid-cols-[auto_1fr_auto] sm:items-center">
              <StatusDot state={draft.state} pulse={draft.state === "analyzing"} />
              <div className="min-w-0">
                <p className="truncate font-mono text-xs">{draft.resolved_commit || draft.id}</p>
                <p className="truncate text-xs text-muted">{String(draft.source.remote ?? "source pending")}</p>
              </div>
              <div className="text-right font-mono text-[10px] text-faint">
                <p>{draft.state}</p>
                <p>{wallClock(draft.updated_at)}</p>
              </div>
            </Link>
          ))}
          {!drafts.isPending && !drafts.data?.length ? (
            <div className="p-8 text-center text-sm text-muted">No inspected sources yet.</div>
          ) : null}
        </div>
      </section>
    </main>
  );
}
