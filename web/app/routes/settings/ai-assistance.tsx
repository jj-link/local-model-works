import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, KeyRound, Plus, Trash2 } from "lucide-react";
import { Link, useSearchParams } from "react-router";
import { toast } from "sonner";
import { SecretDialog } from "~/components/dialogs/secret-dialog";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { Label } from "~/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "~/components/ui/select";
import * as api from "~/lib/api";
import { useModuleSettings, usePutModuleSettings, useSecrets } from "~/lib/queries";

type RemoteProvider = { id: string; label: string; kind: "openai_compatible"; base_url: string; model: string; api_key_secret_id?: string };
type SettingsBuffer = { schemaVersion: 3; providers: RemoteProvider[]; label: string; endpoint: string; model: string; secretId: string; dirty: boolean; baseVersion: string };
function recoverSettings(): { value?: SettingsBuffer; unavailable?: boolean } {
  try {
    const raw = sessionStorage.getItem("lmw.ai-assistance");
    if (!raw) return {};
    const value = JSON.parse(raw) as SettingsBuffer;
    return Array.isArray(value.providers) && typeof value.baseVersion === "string"
      ? { value: { ...value, schemaVersion: 3, providers: value.providers.filter((provider) => provider.kind === "openai_compatible") } }
      : { unavailable: true };
  } catch { return { unavailable: true }; }
}
function nonsecretEndpoint(value: string): string {
  try { const url = new URL(value); return url.username || url.password || url.search || url.hash ? "" : value; }
  catch { return ""; }
}
function remoteProviders(settings: Record<string, unknown>): RemoteProvider[] {
  const assistant = settings.assistant as { providers?: RemoteProvider[] } | undefined;
  return (assistant?.providers ?? []).filter((provider) => provider.kind === "openai_compatible");
}

export default function AIAssistanceSettings() {
  const [searchParams] = useSearchParams();
  const requestedReturn = searchParams.get("return") || "";
  const returnPath = /^\/library\/recipes(?:\/|$)/.test(requestedReturn) && !requestedReturn.includes("\\") ? requestedReturn : "/library/recipes";
  const client = useQueryClient();
  const [dirty, setDirty] = useState(false);
  const [baseVersion, setBaseVersion] = useState("");
  const [recovery, setRecovery] = useState(recoverSettings);
  const [storageUnavailable, setStorageUnavailable] = useState(Boolean(recovery.unavailable));
  const [loginNotice, setLoginNotice] = useState("");
  const settingsQuery = useModuleSettings("library");
  const putSettings = usePutModuleSettings();
  const secrets = useSecrets();
  const [providers, setProviders] = useState<RemoteProvider[]>([]);
  const [label, setLabel] = useState("");
  const [endpoint, setEndpoint] = useState("http://127.0.0.1:8000/v1");
  const [model, setModel] = useState("");
  const [secretId, setSecretId] = useState("");
  const [secretOpen, setSecretOpen] = useState(false);
  const [login, setLogin] = useState<{ login_id: string; verification_url: string; user_code: string }>();
  const providerCatalog = useQuery({ queryKey: ["recipe-assistant", "providers"], queryFn: ({ signal }) => api.listRecipeAssistantProviders({ signal }) });

  useEffect(() => {
    if (!settingsQuery.data || dirty || recovery.value) return;
    setProviders(remoteProviders(settingsQuery.data.settings));
    setBaseVersion(settingsQuery.data.version);
  }, [settingsQuery.data, dirty, recovery.value]);

  useEffect(() => {
    if (recovery.value || !baseVersion) return;
    try {
      if (dirty || label || model || secretId || endpoint !== "http://127.0.0.1:8000/v1") {
        sessionStorage.setItem("lmw.ai-assistance", JSON.stringify({ schemaVersion: 3,
          providers: providers.map((provider) => ({ ...provider, base_url: nonsecretEndpoint(provider.base_url) })),
          label, endpoint: nonsecretEndpoint(endpoint), model, secretId, dirty, baseVersion } satisfies SettingsBuffer));
      } else sessionStorage.removeItem("lmw.ai-assistance");
      setStorageUnavailable(false);
    } catch { setStorageUnavailable(true); }
  }, [recovery.value, providers, label, endpoint, model, secretId, dirty, baseVersion]);

  const codexStatus = useQuery({ queryKey: ["recipe-assistant", "codex", "status"], queryFn: ({ signal }) => api.getRecipeAssistantCodexStatus({ signal }), refetchInterval: login ? 2000 : false });
  const loginStatus = useQuery({ queryKey: ["recipe-assistant", "codex", "login", login?.login_id], queryFn: ({ signal }) => api.getRecipeAssistantCodexLogin(login!.login_id, { signal }), enabled: Boolean(login), refetchInterval: (query) => login && !["complete", "failed", "cancelled", "expired"].includes(query.state.data?.state ?? "") ? 1500 : false });
  const refetchCodex = codexStatus.refetch;
  useEffect(() => {
    if (loginStatus.data?.state === "complete") {
      toast.success("Codex account connected");
      setLogin(undefined);
      void refetchCodex();
      void client.invalidateQueries({ queryKey: ["recipe-assistant", "providers"] });
    }
    if (loginStatus.data && ["failed", "cancelled", "expired"].includes(loginStatus.data.state)) {
      setLoginNotice(`Login ${loginStatus.data.state}${loginStatus.data.error ? `: ${loginStatus.data.error}` : ""}`);
      setLogin(undefined);
    }
  }, [loginStatus.data, refetchCodex, client]);

  const testProvider = useMutation({ mutationFn: api.testRecipeAssistantProvider, onSuccess: (result) => result.ok ? toast.success("Provider connection succeeded", { description: result.model }) : toast.error(result.message) });
  const startLogin = useMutation({ mutationFn: api.startRecipeAssistantCodexLogin, onSuccess: (value) => { setLoginNotice(""); setLogin(value); }, onError: (error) => toast.error(error instanceof Error ? error.message : "Login could not start") });
  const logout = useMutation({ mutationFn: api.logoutRecipeAssistantCodex, onSuccess: () => { setLogin(undefined); void codexStatus.refetch(); void client.invalidateQueries({ queryKey: ["recipe-assistant", "providers"] }); } });
  const cancelLogin = useMutation({ mutationFn: () => api.cancelRecipeAssistantCodexLogin(login!.login_id), onSuccess: () => setLogin(undefined) });

  const addProvider = () => {
    if (!endpoint.trim() || !model.trim()) return toast.error("Endpoint and model are required");
    const provider: RemoteProvider = { id: `provider-${crypto.randomUUID()}`, label: label || "OpenAI-compatible endpoint", kind: "openai_compatible", base_url: endpoint.trim(), model: model.trim(), ...(secretId ? { api_key_secret_id: secretId } : {}) };
    setProviders((current) => [...current, provider]);
    setDirty(true);
    setLabel("");
  };
  const save = async () => {
    const current = settingsQuery.data;
    if (!current) return;
    try {
      await putSettings.mutateAsync({ moduleId: "library", body: { ...current, settings: { ...current.settings, assistant: { providers } } }, ifMatch: baseVersion });
      setDirty(false);
      void client.invalidateQueries({ queryKey: ["recipe-assistant", "providers"] });
      toast.success("API providers saved");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Settings changed elsewhere; reload and retry");
    }
  };

  return <div className="mx-auto max-w-5xl space-y-5 p-4 md:p-6">
    <header className="space-y-2 border-b border-rule pb-4">
      <Link className="inline-flex items-center gap-1 text-xs text-muted hover:text-ink" to={returnPath}><ArrowLeft className="size-3" /> Return to recipe catalog</Link>
      <h1 className="font-display text-2xl font-semibold">AI providers</h1>
      <p className="text-sm text-muted">Running models and models from your connected Codex account are available directly in recipe investigation. You do not need to register them again.</p>
      <p className="text-xs text-warning">Remote requests use your provider quota or subscription allowance. Nothing is sent without a separate recipe-content review.</p>
    </header>
    {storageUnavailable ? <p role="alert" className="text-sm text-warning">Browser recovery storage is unavailable. Keep this tab open until API provider settings are saved.</p> : null}
    {loginNotice ? <p role="status" className="text-sm text-warning">{loginNotice}</p> : null}
    {recovery.value ? <section className="space-y-2 border border-warning/40 p-3"><p>Unsaved API provider settings are available. OAuth codes and credentials were not stored.</p><div className="flex gap-2"><Button variant="outline" onClick={() => { const value = recovery.value!; setProviders(value.providers); setLabel(value.label); setEndpoint(value.endpoint); setModel(value.model); setSecretId(value.secretId); setDirty(value.dirty); setBaseVersion(value.baseVersion); setRecovery({}); }}>Restore provider edits</Button><Button variant="ghost" onClick={() => { if (window.confirm("Discard recovered provider edits?")) setRecovery({}); }}>Discard recovered edits</Button></div></section> : null}
    {settingsQuery.isError ? <div className="border border-fault/40 bg-fault/5 p-3 text-sm text-fault" role="alert">Settings could not be loaded. <Button size="sm" variant="ghost" onClick={() => settingsQuery.refetch()}>Retry</Button></div> : null}
    {dirty && settingsQuery.data?.version !== baseVersion ? <div className="border border-warning/40 p-3 text-sm" role="alert">Provider settings changed elsewhere. Your edits are retained. <Button variant="outline" onClick={() => { if (window.confirm("Discard local provider changes and load current settings?")) setDirty(false); }}>Reload settings</Button></div> : null}
    {([["Available providers", providerCatalog], ["Secrets", secrets], ["Codex status", codexStatus], ["Codex login", loginStatus]] as const).map(([name, query]) => query.isError ? <div key={name} role="alert" className="text-sm text-fault">{name}: {query.error instanceof Error ? query.error.message : "Request failed"} <Button size="sm" variant="outline" onClick={() => void query.refetch()}>Retry</Button></div> : null)}
    {codexStatus.data?.error ? <p role="alert" className="text-sm text-fault">{codexStatus.data.error}</p> : null}
    {[testProvider, startLogin, logout, cancelLogin].map((mutation, index) => mutation.isError ? <p role="alert" className="text-sm text-fault" key={index}>{mutation.error instanceof Error ? mutation.error.message : "Request failed"}</p> : null)}

    <section className="control space-y-3 p-4">
      <h2 className="font-display text-lg font-semibold">Available providers</h2>
      {providerCatalog.isPending ? <p className="text-sm text-muted">Loading providers…</p> : null}
      {providerCatalog.isSuccess && !providerCatalog.data.providers.length ? <p className="text-sm text-muted">No AI providers are available. Start a model in <Link className="underline" to="/serving">Serving</Link>, connect Codex below, or configure an API provider.</p> : null}
      {(providerCatalog.data?.providers ?? []).map((provider) => <div key={provider.id} className="flex items-center justify-between gap-3 border-b border-rule py-2"><p className="text-sm">{provider.label}</p><Button size="sm" variant="outline" disabled={testProvider.isPending} onClick={() => testProvider.mutate(provider.id)}>Test</Button></div>)}
    </section>
    <section className="control space-y-3 p-4">
      <h2 className="font-display text-lg font-semibold">Codex account</h2>
      <p className="text-sm text-muted">Connecting your account makes its models available immediately. No separate provider entry or settings save is required.</p>
      <div className="flex flex-wrap items-center gap-2"><span className="text-sm">{codexStatus.isPending ? "Checking availability…" : codexStatus.data?.available === false ? "Codex is unavailable on this server" : codexStatus.data?.connected ? `Connected${codexStatus.data.email ? ` · ${codexStatus.data.email}` : ""}` : "Not connected"}</span>{codexStatus.data?.connected ? <Button size="sm" variant="outline" disabled={logout.isPending} onClick={() => logout.mutate()}>Disconnect</Button> : <Button size="sm" variant="outline" disabled={startLogin.isPending || Boolean(login) || codexStatus.data?.available !== true} onClick={() => startLogin.mutate()}>Connect Codex account</Button>}</div>
      {login ? <div className="border border-accent/40 bg-accent/5 p-3 text-sm"><p>Open <a className="underline" href={login.verification_url} target="_blank" rel="noreferrer">{login.verification_url}</a> and enter:</p><p className="mt-2 font-mono text-xl font-semibold tracking-widest">{login.user_code}</p><div className="mt-2 flex gap-2"><Button size="sm" variant="outline" onClick={() => void navigator.clipboard.writeText(login.user_code).catch(() => toast.error("Copy failed; select the code manually"))}>Copy code</Button><Button size="sm" variant="outline" disabled={cancelLogin.isPending} onClick={() => cancelLogin.mutate()}>Cancel login</Button></div><p className="mt-2 text-xs text-muted">{loginStatus.data?.state || "Waiting for authorization"}{loginStatus.data?.error ? ` · ${loginStatus.data.error}` : ""}</p></div> : null}
    </section>
    <section className="control space-y-4 p-4">
      <div className="flex items-center justify-between gap-3"><div><h2 className="font-display text-lg font-semibold">API providers</h2><p className="text-xs text-muted">For other OpenAI-compatible services, configure the endpoint, model and optional API key here. Credentials remain in the encrypted secret store.</p></div><Button disabled={!dirty || !settingsQuery.data || !baseVersion || putSettings.isPending || Boolean(recovery.value)} onClick={() => void save()}>{putSettings.isPending ? "Saving…" : "Save API providers"}</Button></div>
      {providers.map((provider) => <article key={provider.id} className="flex items-center justify-between gap-3 border border-rule p-3"><div><p className="font-medium">{provider.label}</p><p className="break-all font-mono text-xs text-muted">{provider.base_url} · {provider.model}</p></div><Button size="icon" variant="ghost" aria-label={`Remove ${provider.label}`} onClick={() => { setDirty(true); setProviders((current) => current.filter((item) => item.id !== provider.id)); }}><Trash2 className="size-4" /></Button></article>)}
      <div className="grid gap-3 md:grid-cols-2">
        <div className="space-y-1.5"><Label htmlFor="provider-label">Label</Label><Input id="provider-label" value={label} onChange={(event) => setLabel(event.target.value)} placeholder="API provider name" /></div>
        <div className="space-y-1.5"><Label htmlFor="provider-url">Base URL</Label><Input id="provider-url" value={endpoint} onChange={(event) => setEndpoint(event.target.value)} /></div>
        <div className="space-y-1.5"><Label htmlFor="provider-model">Model</Label><Input id="provider-model" value={model} onChange={(event) => setModel(event.target.value)} /></div>
        <div className="space-y-1.5"><Label>API key secret</Label><div className="flex gap-2"><Select value={secretId} onValueChange={setSecretId}><SelectTrigger aria-label="API key secret"><SelectValue placeholder="No authentication" /></SelectTrigger><SelectContent>{(secrets.data ?? []).filter((item) => item.purpose === "recipe-assistant").map((item) => <SelectItem key={item.id} value={item.id}>{item.name}</SelectItem>)}</SelectContent></Select><Button size="icon" variant="outline" aria-label="Create API key secret" onClick={() => setSecretOpen(true)}><KeyRound className="size-4" /></Button></div></div>
      </div>
      <Button onClick={addProvider}><Plus className="size-4" /> Add API provider</Button>
    </section>
    <SecretDialog open={secretOpen} onOpenChange={setSecretOpen} />
  </div>;
}
