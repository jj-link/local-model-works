import { useEffect, useMemo, useState } from "react";
import { toast } from "sonner";
import { KeyRound, Plus } from "lucide-react";
import { Button } from "~/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "~/components/ui/dialog";
import { Input } from "~/components/ui/input";
import { Label } from "~/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "~/components/ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "~/components/ui/tabs";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "~/components/ui/table";
import { useModules, useModuleSettings, usePutModuleSettings, useSecrets, usePutSecret, useDeleteSecret } from "~/lib/queries";
import { ApiError } from "~/lib/api/client";
import type { SecretWrite } from "~/lib/api";
import { EmptyState } from "~/components/empty-state";
import { ConfirmDialog } from "~/components/dialogs/confirm-dialog";
import { wallClock } from "~/lib/format";

/* ---------------------------- module settings --------------------------- */

function ModuleSettingsPanel() {
  const { data: modules } = useModules();
  const [moduleId, setModuleId] = useState<string>("");
  const activeModule = (modules ?? []).find((m) => m.id === moduleId) ?? modules?.[0];
  const { data: settings, isPending, isError, error, refetch } = useModuleSettings(activeModule?.id);
  const put = usePutModuleSettings();
  const [text, setText] = useState("{}");
  const [invalidJson, setInvalidJson] = useState(false);
  const [conflict, setConflict] = useState(false);

  useEffect(() => {
    if (settings) {
      setText(JSON.stringify(settings.settings, null, 2));
      setConflict(false);
    }
  }, [settings]);

  const defaults = activeModule?.settingsSchema
    ? JSON.stringify(activeModule.settingsSchema, null, 2)
    : null;

  const save = async () => {
    if (!activeModule || !settings) return;
    let parsed: Record<string, unknown>;
    try {
      parsed = JSON.parse(text);
      setInvalidJson(false);
    } catch {
      setInvalidJson(true);
      return;
    }
    try {
      await put.mutateAsync({
        moduleId: activeModule.id,
        body: { module: activeModule.id, settings: parsed, version: settings.version },
        ifMatch: settings.version,
      });
      setConflict(false);
      toast.success("Module settings saved");
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        setConflict(true);
        toast.error("Settings changed elsewhere (409 conflict)");
      } else {
        toast.error(e instanceof Error ? e.message : "save failed");
      }
    }
  };

  return (
    <div className="grid gap-3">
      <div className="flex flex-wrap items-center gap-2">
        <Select
          value={activeModule?.id ?? ""}
          onValueChange={setModuleId}
        >
          <SelectTrigger className="h-8 w-44 font-mono text-xs" aria-label="Module">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {(modules ?? []).map((m) => (
              <SelectItem key={m.id} value={m.id}>
                {m.id} — {m.title}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {settings ? (
          <span className="font-mono text-[10px] text-faint" title="ETag for optimistic concurrency">
            etag {settings.version}
          </span>
        ) : null}
        <div className="ml-auto flex items-center gap-2">
          {conflict ? (
            <Button size="sm" variant="outline" onClick={() => void refetch()}>
              reload
            </Button>
          ) : null}
          <Button size="sm" onClick={() => void save()} disabled={put.isPending || !settings}>
            {put.isPending ? "saving…" : "save"}
          </Button>
        </div>
      </div>

      {conflict ? (
        <p className="font-mono text-xs text-warn">
          409 conflict — the settings were modified on another surface. Reload to see the current
          value; your unsaved edits are kept in the editor.
        </p>
      ) : null}
      {invalidJson ? (
        <p className="font-mono text-xs text-fault">invalid JSON — fix the editor before saving</p>
      ) : null}

      <div className="grid gap-3 lg:grid-cols-2">
        <div>
          <p className="lmw-label mb-1.5">current value</p>
          {isPending ? (
            <p className="py-8 text-center font-mono text-xs text-faint">loading…</p>
          ) : isError ? (
            <EmptyState
              title="Cannot load settings"
              detail={error instanceof Error ? error.message : undefined}
              onRetry={() => void refetch()}
            />
          ) : (
            <textarea
              value={text}
              onChange={(e) => setText(e.target.value)}
              spellCheck={false}
              rows={16}
              aria-label="Module settings JSON"
              className="w-full resize-y rounded border border-hairline bg-background/60 p-2.5 font-mono text-xs leading-relaxed outline-none focus-visible:ring-2 focus-visible:ring-primary/50"
            />
          )}
        </div>
        <div>
          <p className="lmw-label mb-1.5">schema defaults</p>
          {defaults ? (
            <pre className="max-h-96 overflow-auto rounded border border-hairline bg-background/60 p-2.5 font-mono text-xs leading-relaxed text-muted">
              {defaults}
            </pre>
          ) : (
            <p className="rounded border border-hairline bg-background/40 p-3 font-mono text-xs text-faint">
              no schema declared by this module
            </p>
          )}
        </div>
      </div>
    </div>
  );
}

/* --------------------------------- secrets ------------------------------ */

const PURPOSES: SecretWrite["purpose"][] = ["huggingface", "github", "registry"];

function SecretsPanel() {
  const { data, isPending, isError, error, refetch } = useSecrets();
  const put = usePutSecret();
  const remove = useDeleteSecret();
  const [addOpen, setAddOpen] = useState(false);
  const [name, setName] = useState("");
  const [purpose, setPurpose] = useState<SecretWrite["purpose"]>("huggingface");
  const [value, setValue] = useState("");

  useEffect(() => {
    if (addOpen) {
      setName("");
      setPurpose("huggingface");
      setValue("");
    }
  }, [addOpen]);

  const submit = async () => {
    if (!name || !value) return;
    try {
      await put.mutateAsync({ name, purpose, value });
      setAddOpen(false);
      toast.success("Secret stored", { description: name });
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "secret save failed");
    }
  };

  return (
    <div className="grid gap-3">
      <div className="flex items-center justify-between">
        <p className="font-mono text-[11px] text-faint">
          values are AES-256-GCM encrypted at rest; the API returns metadata only
        </p>
        <Button size="sm" onClick={() => setAddOpen(true)}>
          <Plus aria-hidden /> add secret
        </Button>
      </div>

      {isPending ? (
        <p className="py-8 text-center font-mono text-xs text-faint">loading secrets…</p>
      ) : isError ? (
        <EmptyState
          title="Cannot load secrets"
          detail={error instanceof Error ? error.message : undefined}
          onRetry={() => void refetch()}
        />
      ) : (data ?? []).length === 0 ? (
        <EmptyState
          title="No secrets"
          icon={<KeyRound aria-hidden />}
          hint="Store Hugging Face, GitHub, and registry tokens for authenticated imports."
        />
      ) : (
        <div className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Purpose</TableHead>
                <TableHead>Created</TableHead>
                <TableHead>Updated</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {(data ?? []).map((s) => (
                <TableRow key={s.id}>
                  <TableCell className="font-mono text-xs">{s.name}</TableCell>
                  <TableCell className="font-mono text-xs text-muted">{s.purpose}</TableCell>
                  <TableCell className="font-mono text-[11px] text-faint">
                    {s.created_at ? wallClock(s.created_at) : "—"}
                  </TableCell>
                  <TableCell className="font-mono text-[11px] text-faint">
                    {s.updated_at ? wallClock(s.updated_at) : "—"}
                  </TableCell>
                  <TableCell>
                    <ConfirmDialog
                      title="Delete secret"
                      description={`Remove ${s.name}? Imports that reference it will fail until re-added.`}
                      confirmLabel="delete"
                      tone="destructive"
                      onConfirm={async () => {
                        try {
                          await remove.mutateAsync(s.id);
                          toast.success("Secret deleted");
                        } catch (e) {
                          toast.error(e instanceof Error ? e.message : "delete failed");
                          throw e;
                        }
                      }}
                    >
                      delete
                    </ConfirmDialog>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      <Dialog open={addOpen} onOpenChange={setAddOpen}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle className="font-display text-base font-semibold tracking-wide">
              Add secret
            </DialogTitle>
            <DialogDescription>
              The value is written once; it is never returned by the API.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-3">
            <div className="grid gap-1.5">
              <Label htmlFor="secret-name">Name</Label>
              <Input
                id="secret-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="hf-token"
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="secret-purpose">Purpose</Label>
              <Select value={purpose} onValueChange={(v) => setPurpose(v as SecretWrite["purpose"])}>
                <SelectTrigger id="secret-purpose" className="font-mono text-xs">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {PURPOSES.map((p) => (
                    <SelectItem key={p} value={p}>
                      {p}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="secret-value">Value</Label>
              <Input
                id="secret-value"
                type="password"
                value={value}
                onChange={(e) => setValue(e.target.value)}
                placeholder="••••••••••••••••"
              />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setAddOpen(false)}>
              Cancel
            </Button>
            <Button onClick={() => void submit()} disabled={put.isPending || !name || !value}>
              {put.isPending ? "storing…" : "store"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

/* --------------------------------- route -------------------------------- */

export default function SettingsRoute() {
  const moduleIds = useModules().data;
  const key = useMemo(() => moduleIds?.map((m) => m.id).join(",") ?? "", [moduleIds]);
  void key;

  return (
    <div className="grid gap-4">
      <div className="lmw-panel">
        <header className="lmw-panel-head">
          <h1 className="lmw-label">settings</h1>
        </header>
        <Tabs defaultValue="modules" className="p-3">
          <TabsList>
            <TabsTrigger value="modules">module settings</TabsTrigger>
            <TabsTrigger value="secrets">secrets</TabsTrigger>
          </TabsList>
          <TabsContent value="modules" className="mt-3">
            <ModuleSettingsPanel />
          </TabsContent>
          <TabsContent value="secrets" className="mt-3">
            <SecretsPanel />
          </TabsContent>
        </Tabs>
      </div>
    </div>
  );
}
