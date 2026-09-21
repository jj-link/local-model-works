import { useEffect, useState } from "react";
import { toast } from "sonner";
import { Plus } from "lucide-react";

import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { Label } from "~/components/ui/label";
import { useCreateRecipeDraft } from "~/lib/queries";

const sourceRecoveryKey = "lmw.recipe-source";

function recoveredSource(): { remote: string; revision: string; path: string } {
  try {
    const value = sessionStorage.getItem(sourceRecoveryKey);
    return value ? JSON.parse(value) : { remote: "", revision: "", path: "" };
  } catch {
    return { remote: "", revision: "", path: "" };
  }
}
export function SourceForm({ onCreated }: { onCreated: (id: string) => void }) {
  const createDraft = useCreateRecipeDraft();
  const [initial] = useState(recoveredSource);
  const [remote, setRemote] = useState(initial.remote);
  const [revision, setRevision] = useState(initial.revision);
  const [path, setPath] = useState(initial.path);
  const [advanced, setAdvanced] = useState(Boolean(initial.path));
  const [recoveryUnavailable, setRecoveryUnavailable] = useState(false);

  useEffect(() => {
    try {
      sessionStorage.setItem(sourceRecoveryKey, JSON.stringify({ schemaVersion: 2, remote, revision, path }));
      setRecoveryUnavailable(false);
    } catch {
      setRecoveryUnavailable(true);
    }
  }, [remote, revision, path]);

  const submit = async () => {
    try {
      const result = await createDraft.mutateAsync({ remote, revision, ...(path ? { path } : {}) });
      toast.success("Source inspection queued");
      try { sessionStorage.removeItem(sourceRecoveryKey); } catch { /* in-memory values remain safe */ }
      onCreated(result.draft_id);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Draft analysis failed");
    }
  };

  return (
    <section className="space-y-4">
      <header><h2 className="font-display text-2xl font-semibold">Add from GitHub</h2><p className="mt-2 text-sm text-muted">Start with a repository URL. Investigate its documented launch procedures, review hardware requirements and any adaptations, then save each procedure as a recipe. Models are fixed by upstream or configured later through documented deployment parameters.</p></header>
      <section className="grid gap-px border border-hairline bg-hairline lg:grid-cols-[1.2fr_.8fr]">
        <div className="bg-panel p-4">
          <div className="mb-4 flex items-center gap-2">
            <Plus className="size-4 text-primary" aria-hidden />
            <h3 className="lmw-label">Repository source</h3>
          </div>
          <div className="grid gap-3 md:grid-cols-2">
            <div className="flex flex-col gap-1.5 md:col-span-2">
              <Label htmlFor="recipe-remote">GitHub URL</Label>
              <Input id="recipe-remote" value={remote} onChange={(event) => setRemote(event.target.value)} placeholder="https://github.com/owner/repository" />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="builder-revision">Branch, tag, or commit <span className="text-muted">(optional)</span></Label>
              <Input id="builder-revision" value={revision} onChange={(event) => setRevision(event.target.value)} placeholder="Default branch when empty" />
            </div>
            <div className="flex items-end">
              <Button type="button" variant="ghost" size="sm" onClick={() => setAdvanced((value) => !value)}>
                {advanced ? "Hide advanced" : "Advanced"}
              </Button>
            </div>
            {advanced ? <div className="flex flex-col gap-1.5 md:col-span-2">
              <Label htmlFor="builder-path">Repository subpath <span className="text-muted">(optional)</span></Label>
              <Input id="builder-path" value={path} onChange={(event) => setPath(event.target.value)} placeholder="packages/model" />
            </div> : null}
          </div>
          {recoveryUnavailable ? <p className="mt-3 text-xs text-warning">Unsaved source recovery is unavailable; this tab still retains the current values.</p> : null}
          {createDraft.isError ? <div className="mt-3 border border-fault/40 bg-fault/5 p-3 text-xs text-fault" role="alert">
            <p>Inspection could not be queued. Your source fields were retained.</p>
            <p className="mt-1 font-mono">{createDraft.error instanceof Error ? createDraft.error.message : "Request failed"}</p>
          </div> : null}
          <Button className="mt-4" disabled={!remote.trim() || createDraft.isPending} onClick={submit}>
            {createDraft.isPending ? "Queuing inspection…" : "Inspect repository"}
          </Button>
        </div>
        <aside className="bg-raised p-4 text-xs text-muted">
          <p className="lmw-label mb-3">Import boundary</p>
          <ul className="space-y-2">
            <li>Pin the repository before investigation.</li>
            <li>Approve the repository content scope and AI destination before investigation.</li>
            <li>Preserve upstream launch commands, Dockerfiles and scripts.</li>
            <li>No upstream code execution, model downloads or deployments.</li>
          </ul>
        </aside>
      </section>

    </section>
  );
}
