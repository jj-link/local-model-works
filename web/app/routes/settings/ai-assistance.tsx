import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { ArrowLeft, KeyRound, Plus, Trash2 } from "lucide-react";
import { Link, useSearchParams } from "react-router";
import { toast } from "sonner";
import { SecretDialog } from "~/components/dialogs/secret-dialog";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { Label } from "~/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "~/components/ui/select";
import * as api from "~/lib/api";
import { useDeployments, useModuleSettings, usePutModuleSettings, useSecrets } from "~/lib/queries";

type Provider =
  | { id: string; label: string; kind: "local"; deployment_id: string }
  | { id: string; label: string; kind: "openai_compatible"; base_url: string; model: string; api_key_secret_id?: string }
  | { id: string; label: string; kind: "codex"; model?: string };

type AssistantSettings = { default_provider_id?: string; providers?: Provider[] };
type SettingsBuffer = { schemaVersion: 2; providers: Provider[]; defaultId: string; kind: Provider["kind"]; label: string; endpoint: string; model: string; deploymentId: string; secretId: string; dirty: boolean; baseVersion: string };
function recoverSettings(): { value?: SettingsBuffer; unavailable?: boolean } {
  try {
    const raw = sessionStorage.getItem("lmw.ai-assistance");
    if (!raw) return {};
    const value = JSON.parse(raw) as SettingsBuffer;
    return Array.isArray(value.providers) && typeof value.baseVersion === "string" ? { value } : { unavailable: true };
  } catch { return { unavailable: true }; }
}
function nonsecretEndpoint(value: string): string {
  try { const url = new URL(value); return url.username || url.password || url.search || url.hash ? "" : value; }
  catch { return ""; }
}

function assistantOf(settings: Record<string, unknown> | undefined): AssistantSettings {
  const value = settings?.assistant;
  return value && typeof value === "object" ? value as AssistantSettings : {};
}

export default function AIAssistanceSettings() {
  const [searchParams] = useSearchParams();
  const requestedReturn = searchParams.get("return") || "";
  const returnPath = /^\/library\/recipes(?:\/|$)/.test(requestedReturn) && !requestedReturn.includes("\\") ? requestedReturn : "/library/recipes";
  const [dirty, setDirty] = useState(false);
  const [baseVersion, setBaseVersion] = useState("");
  const [recovery, setRecovery] = useState(recoverSettings);
  const [storageUnavailable, setStorageUnavailable] = useState(Boolean(recovery.unavailable));
  const [loginNotice, setLoginNotice] = useState("");
  const settingsQuery = useModuleSettings("library");
  const putSettings = usePutModuleSettings();
  const deployments = useDeployments();
  const secrets = useSecrets();
  const [providers, setProviders] = useState<Provider[]>([]);
  const [defaultId, setDefaultId] = useState("");
  const [kind, setKind] = useState<Provider["kind"]>("local");
  const [label, setLabel] = useState("");
  const [endpoint, setEndpoint] = useState("http://127.0.0.1:8000/v1");
  const [model, setModel] = useState("");
  const [deploymentId, setDeploymentId] = useState("");
  const [secretId, setSecretId] = useState("");
  const [secretOpen, setSecretOpen] = useState(false);
  const [login, setLogin] = useState<{ login_id: string; verification_url: string; user_code: string }>();

  useEffect(() => {
    if (!settingsQuery.data || dirty || recovery.value) return;
    const assistant = assistantOf(settingsQuery.data.settings);
    setProviders(assistant.providers ?? []);
    setDefaultId(assistant.default_provider_id ?? "");
    setBaseVersion(settingsQuery.data.version);
  }, [settingsQuery.data, dirty, recovery.value]);

  useEffect(() => {
    if (recovery.value || !baseVersion) return;
    try {
      if (dirty || label || model || deploymentId || secretId || endpoint !== "http://127.0.0.1:8000/v1") {
        sessionStorage.setItem("lmw.ai-assistance", JSON.stringify({ schemaVersion: 2,
          providers: providers.map((provider) => provider.kind === "openai_compatible" ? { ...provider, base_url: nonsecretEndpoint(provider.base_url) } : provider),
          defaultId, kind, label, endpoint: nonsecretEndpoint(endpoint), model, deploymentId, secretId, dirty, baseVersion } satisfies SettingsBuffer));
      } else sessionStorage.removeItem("lmw.ai-assistance");
      setStorageUnavailable(false);
    } catch { setStorageUnavailable(true); }
  }, [recovery.value, providers, defaultId, kind, label, endpoint, model, deploymentId, secretId, dirty, baseVersion]);

  const codexStatus = useQuery({ queryKey: ["recipe-assistant", "codex", "status"], queryFn: ({ signal }) => api.getRecipeAssistantCodexStatus({ signal }), refetchInterval: login ? 2000 : false });
  const codexModels = useQuery({ queryKey: ["recipe-assistant", "codex", "models"], queryFn: ({ signal }) => api.listRecipeAssistantCodexModels({ signal }), enabled: codexStatus.data?.connected === true });
  const loginStatus = useQuery({ queryKey: ["recipe-assistant", "codex", "login", login?.login_id], queryFn: ({ signal }) => api.getRecipeAssistantCodexLogin(login!.login_id, { signal }), enabled: Boolean(login), refetchInterval: (query) => login && !["complete", "failed", "cancelled", "expired"].includes(query.state.data?.state ?? "") ? 1500 : false });
  const refetchCodex = codexStatus.refetch;
  useEffect(() => {
    if (loginStatus.data?.state === "complete") {
      toast.success("Codex account connected");
      setLogin(undefined);
      void refetchCodex();
    }
    if (loginStatus.data && ["failed", "cancelled", "expired"].includes(loginStatus.data.state)) {
      setLoginNotice(`Login ${loginStatus.data.state}${loginStatus.data.error ? `: ${loginStatus.data.error}` : ""}`);
      setLogin(undefined);
    }
  }, [loginStatus.data, refetchCodex]);

  const testProvider = useMutation({ mutationFn: api.testRecipeAssistantProvider, onSuccess: (result) => result.ok ? toast.success("Provider connection succeeded", { description: result.model }) : toast.error(result.message) });
  const startLogin = useMutation({ mutationFn: api.startRecipeAssistantCodexLogin, onSuccess: (value) => { setLoginNotice(""); setLogin(value); }, onError: (error) => toast.error(error instanceof Error ? error.message : "Login could not start") });
  const logout = useMutation({ mutationFn: api.logoutRecipeAssistantCodex, onSuccess: () => { setLogin(undefined); void codexStatus.refetch(); } });
  const cancelLogin = useMutation({ mutationFn: () => api.cancelRecipeAssistantCodexLogin(login!.login_id), onSuccess: () => setLogin(undefined) });

  const healthyLocalDeployments = useMemo(() => (deployments.data ?? []).filter((item) => item.observed_state === "healthy"), [deployments.data]);

  const addProvider = () => {
    const id = `provider-${crypto.randomUUID()}`;
    let provider: Provider;
    if (kind === "local") {
      if (!deploymentId) return toast.error("Choose a healthy local deployment");
      provider = { id, label: label || "Local deployment", kind, deployment_id: deploymentId };
    } else if (kind === "openai_compatible") {
      if (!endpoint.trim() || !model.trim()) return toast.error("Endpoint and model are required");
      provider = { id, label: label || "OpenAI-compatible endpoint", kind, base_url: endpoint.trim(), model: model.trim(), ...(secretId ? { api_key_secret_id: secretId } : {}) };
    } else {
      provider = { id, label: label || "Codex", kind, ...(model ? { model } : {}) };
    }
    setProviders((current) => [...current, provider]);
    setDirty(true);
    setDefaultId((current) => current || id);
    setLabel("");
  };

  const save = async () => {
    const current = settingsQuery.data;
    if (!current) return;
    const body = { ...current, settings: { ...current.settings, assistant: { providers, ...(defaultId ? { default_provider_id: defaultId } : {}) } } };
    try {
      await putSettings.mutateAsync({ moduleId: "library", body, ifMatch: baseVersion });
      setDirty(false);
      toast.success("Assistant providers saved");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Settings changed elsewhere; reload and retry");
    }
  };

  return <div className="mx-auto max-w-5xl space-y-5 p-4 md:p-6">
    <header className="flex flex-wrap items-start justify-between gap-3 border-b border-rule pb-4">
      <div>
        <Link className="inline-flex items-center gap-1 text-xs text-muted hover:text-ink" to={returnPath}><ArrowLeft className="size-3" /> Return to recipe catalog</Link>
        <h1 className="mt-2 font-display text-2xl font-semibold">AI assistance</h1>
        <p className="mt-1 text-sm text-muted">Connections are explicit. Credentials remain in the encrypted secret store and are never included in draft prompts.</p>
        <p className="mt-2 text-xs text-warning">Remote requests use your provider quota or subscription allowance. Nothing is sent without a separate recipe-content review.</p>
      </div>
      <Button disabled={!settingsQuery.data || !baseVersion || putSettings.isPending || Boolean(recovery.value)} onClick={() => void save()}>{putSettings.isPending ? "Saving…" : "Save settings"}</Button>
    </header>
    {storageUnavailable ? <p role="alert" className="text-sm text-warning">Browser recovery storage is unavailable. Keep this tab open until nonsecret provider settings are saved.</p> : null}
    {loginNotice ? <p role="status" className="text-sm text-warning">{loginNotice}</p> : null}
    {recovery.value ? <section className="space-y-2 border border-warning/40 p-3"><p>Unsaved nonsecret provider settings are available. OAuth codes and credentials were not stored.</p><div className="flex gap-2"><Button variant="outline" onClick={() => { const value = recovery.value!; setProviders(value.providers); setDefaultId(value.defaultId); setKind(value.kind); setLabel(value.label); setEndpoint(value.endpoint); setModel(value.model); setDeploymentId(value.deploymentId); setSecretId(value.secretId); setDirty(value.dirty); setBaseVersion(value.baseVersion); setRecovery({}); }}>Restore provider edits</Button><Button variant="ghost" onClick={() => { if (window.confirm("Discard recovered provider edits?")) setRecovery({}); }}>Discard recovered edits</Button></div></section> : null}

    {settingsQuery.isError ? <div className="border border-fault/40 bg-fault/5 p-3 text-sm text-fault" role="alert">Settings could not be loaded. <Button size="sm" variant="ghost" onClick={() => settingsQuery.refetch()}>Retry</Button></div> : null}
    {dirty && settingsQuery.data?.version !== baseVersion ? <div className="border border-warning/40 p-3 text-sm" role="alert">Saved settings changed elsewhere. Your edits are retained. <Button variant="outline" onClick={() => { if (window.confirm("Discard local provider changes and load saved settings?")) setDirty(false); }}>Reload saved settings</Button></div> : null}
    {([["Local deployments", deployments], ["Secrets", secrets], ["Codex status", codexStatus], ["Codex models", codexModels], ["Codex login", loginStatus]] as const).map(([name, query]) => query.isError ? <div key={name} role="alert" className="text-sm text-fault">{name}: {query.error instanceof Error ? query.error.message : "Request failed"} <Button size="sm" variant="outline" onClick={() => void query.refetch()}>Retry</Button></div> : null)}
    {codexStatus.data?.error ? <p role="alert" className="text-sm text-fault">{codexStatus.data.error}</p> : null}
    {[testProvider, startLogin, logout, cancelLogin].map((mutation, index) => mutation.isError ? <p role="alert" className="text-sm text-fault" key={index}>{mutation.error instanceof Error ? mutation.error.message : "Request failed"}</p> : null)}

    <section className="space-y-3">
      <div className="flex items-end justify-between gap-3"><div><h2 className="font-display text-lg font-semibold">Configured providers</h2><p className="text-xs text-muted">Generation requires a provider and binds consent to the saved settings version.</p></div></div>
      {providers.length === 0 ? <div className="border border-dashed border-rule p-5 text-sm text-muted">No assistant provider configured. Manual editing remains available.</div> : null}
      {providers.map((provider) => <article key={provider.id} className="control flex flex-wrap items-center justify-between gap-3 p-4">
        <div><p className="font-medium">{provider.label}</p><p className="mt-1 font-mono text-xs text-muted">{provider.kind} · {provider.id}</p></div>
        <div className="flex gap-2">
          <Button size="sm" variant="outline" disabled={dirty || !baseVersion || testProvider.isPending} title={dirty ? "Save settings before testing" : undefined} onClick={() => testProvider.mutate(provider.id)}>Test</Button>
          <Button size="icon" variant="ghost" aria-label={`Remove ${provider.label}`} onClick={() => { setDirty(true); setProviders((current) => current.filter((item) => item.id !== provider.id)); if (defaultId === provider.id) setDefaultId(""); }}><Trash2 className="size-4" /></Button>
        </div>
      </article>)}
      {providers.length ? <div className="max-w-sm space-y-1.5"><Label>Default provider</Label><Select value={defaultId} onValueChange={(value) => { setDefaultId(value); setDirty(true); }}><SelectTrigger aria-label="Default provider"><SelectValue placeholder="Choose provider" /></SelectTrigger><SelectContent>{providers.map((provider) => <SelectItem key={provider.id} value={provider.id}>{provider.label}</SelectItem>)}</SelectContent></Select></div> : null}
    </section>

    <section className="control space-y-4 p-4">
      <div><h2 className="font-display text-lg font-semibold">Add provider</h2><p className="text-xs text-muted">Only healthy local deployments can be selected. Remote API keys are referenced by secret ID.</p></div>
      <div className="grid gap-3 md:grid-cols-2">
        <div className="space-y-1.5"><Label>Provider type</Label><Select value={kind} onValueChange={(value) => { setKind(value as Provider["kind"]); setModel(""); }}><SelectTrigger aria-label="Provider type"><SelectValue /></SelectTrigger><SelectContent><SelectItem value="local">Local deployment</SelectItem><SelectItem value="openai_compatible">OpenAI-compatible</SelectItem><SelectItem value="codex">Codex subscription</SelectItem></SelectContent></Select></div>
        <div className="space-y-1.5"><Label htmlFor="provider-label">Label</Label><Input id="provider-label" value={label} onChange={(event) => setLabel(event.target.value)} placeholder="My assistant" /></div>
        {kind === "local" ? <div className="space-y-1.5 md:col-span-2"><Label>Healthy deployment</Label><Select value={deploymentId} onValueChange={setDeploymentId}><SelectTrigger aria-label="Healthy deployment"><SelectValue placeholder="Select deployment" /></SelectTrigger><SelectContent>{healthyLocalDeployments.map((item) => <SelectItem key={item.id} value={item.id}>{item.recipe_name || item.id}</SelectItem>)}</SelectContent></Select>{!healthyLocalDeployments.length ? <p className="text-xs text-warning">No healthy local deployments are available.</p> : null}</div> : null}
        {kind === "openai_compatible" ? <><div className="space-y-1.5"><Label htmlFor="provider-url">Base URL</Label><Input id="provider-url" value={endpoint} onChange={(event) => setEndpoint(event.target.value)} /></div><div className="space-y-1.5"><Label htmlFor="provider-model">Model</Label><Input id="provider-model" value={model} onChange={(event) => setModel(event.target.value)} /></div><div className="space-y-1.5 md:col-span-2"><Label>API key secret</Label><div className="flex gap-2"><Select value={secretId} onValueChange={setSecretId}><SelectTrigger aria-label="API key secret"><SelectValue placeholder="No authentication" /></SelectTrigger><SelectContent>{(secrets.data ?? []).filter((item) => item.purpose === "recipe-assistant").map((item) => <SelectItem key={item.id} value={item.id}>{item.name}</SelectItem>)}</SelectContent></Select><Button variant="outline" onClick={() => setSecretOpen(true)}><KeyRound className="size-4" /> New secret</Button></div></div></> : null}
        {kind === "codex" ? <div className="space-y-3 md:col-span-2">
          <div className="flex flex-wrap items-center gap-2"><span className="text-sm">{codexStatus.isPending ? "Checking availability…" : codexStatus.data?.available === false ? "Codex is unavailable on this server" : codexStatus.data?.connected ? `Connected${codexStatus.data.email ? ` · ${codexStatus.data.email}` : ""}` : "Not connected"}</span>{codexStatus.data?.connected ? <Button size="sm" variant="outline" disabled={logout.isPending} onClick={() => logout.mutate()}>Disconnect</Button> : <Button size="sm" variant="outline" disabled={startLogin.isPending || Boolean(login) || codexStatus.data?.available !== true} onClick={() => startLogin.mutate()}>Connect Codex account</Button>}</div>
          {login ? <div className="border border-accent/40 bg-accent/5 p-3 text-sm"><p>Open <a className="underline" href={login.verification_url} target="_blank" rel="noreferrer">{login.verification_url}</a> and enter:</p><p className="mt-2 font-mono text-xl font-semibold tracking-widest">{login.user_code}</p><div className="mt-2 flex gap-2"><Button size="sm" variant="outline" onClick={() => void navigator.clipboard.writeText(login.user_code).catch(() => toast.error("Copy failed; select the code manually"))}>Copy code</Button><Button size="sm" variant="outline" disabled={cancelLogin.isPending} onClick={() => cancelLogin.mutate()}>Cancel login</Button></div><p className="mt-2 text-xs text-muted">{loginStatus.data?.state || "Waiting for authorization"}{loginStatus.data?.error ? `: ${loginStatus.data.error}` : ""}</p></div> : null}
          <div className="space-y-1.5"><Label>Model override <span className="text-muted">(optional)</span></Label><Select value={model} onValueChange={setModel}><SelectTrigger aria-label="Model override"><SelectValue placeholder="Account default" /></SelectTrigger><SelectContent>{(codexModels.data ?? []).map((item) => <SelectItem key={item.id} value={item.id}>{item.display_name}</SelectItem>)}</SelectContent></Select></div>
        </div> : null}
      </div>
      <Button onClick={addProvider}><Plus className="size-4" /> Add provider</Button>
    </section>
    <SecretDialog open={secretOpen} onOpenChange={setSecretOpen} />
  </div>;
}
