// One typed function per OpenAPI operation (operationId-named). Generated
// types come from ~/generated/api (openapi-typescript).
import { API_BASE, http, qs, requestRaw } from "./client";
import type { components } from "~/generated/api";

export type Schemas = components["schemas"];
export type Node = Schemas["Node"];
export type NodeStatus = Schemas["NodeStatus"];
export type Inventory = Schemas["Inventory"];
export type Accelerator = Schemas["Accelerator"];
export type NetworkInterface = Schemas["NetworkInterface"];
export type RdmaDevice = Schemas["RdmaDevice"];
export type CertificateInfo = Schemas["CertificateInfo"];
export type UpdateNodeRequest = Schemas["UpdateNodeRequest"];
export type Fabric = Schemas["Fabric"];
export type CreateFabricRequest = Schemas["CreateFabricRequest"];
export type Recipe = Schemas["Recipe"];
export type RecipeDetail = Schemas["RecipeDetail"];
export type RecipeSource = Schemas["RecipeSource"];
export type RecipeImport = Schemas["RecipeImport"];
export type RecipeTrustRequest = Schemas["RecipeTrustRequest"];
export type RecipeDraft = Schemas["RecipeDraft"];
export type RecipeDraftSource = Schemas["RecipeDraftSource"];
export type Artifact = Schemas["Artifact"];
export type ArtifactKind = Schemas["ArtifactKind"];
export type Placement = Schemas["Placement"];
export type Transfer = Schemas["Transfer"];
export type TransferRequest = Schemas["TransferRequest"];
export type TransferPreview = Schemas["TransferPreview"];
export type Diagnostic = Schemas["Diagnostic"];
export type Deployment = Schemas["Deployment"];
export type DeploymentPlan = Schemas["DeploymentPlan"];
export type DeploymentPlanRequest = Schemas["DeploymentPlanRequest"];
export type DeploymentCreateRequest = Schemas["DeploymentCreateRequest"];
export type ChatRequest = Schemas["ChatCompletionRequest"];
export type ChatResponse = Schemas["ChatCompletionResponse"];
export type NodeTelemetrySample = Schemas["NodeTelemetrySample"];
export type ServingTelemetrySample = Schemas["ServingTelemetrySample"];
export type NodePayload = Schemas["NodePayload"];
export type ServingPayload = Schemas["ServingPayload"];
export type Run = Schemas["Run"];
export type RunState = Schemas["RunState"];
export type RunsPage = Schemas["RunsPage"];
export type BenchmarkCreate = Schemas["BenchmarkCreate"];
export type BenchmarkResult = Schemas["BenchmarkResult"];
export type Module = Schemas["Module"];
export type ModuleSettings = Schemas["ModuleSettings"];
export type Secret = Schemas["Secret"];
export type SecretWrite = Schemas["SecretWrite"];
export type SystemInfo = Schemas["SystemInfo"];
export type EnrollmentToken = Schemas["EnrollmentToken"];
export type MigrationScanRequest = Schemas["MigrationScanRequest"];
export type MigrationImportRequest = Schemas["MigrationImportRequest"];
export type RunAccepted = Schemas["RunAccepted"];
export type AutoResearchProject = Schemas["AutoResearchProject"];
export type AutoResearchProjectCreate = Schemas["AutoResearchProjectCreate"];
export type AutoResearchProjectPatch = Schemas["AutoResearchProjectPatch"];
export type AutoResearchIdea = Schemas["AutoResearchIdea"];
export type AutoResearchIdeaGenerate = Schemas["AutoResearchIdeaGenerate"];
export type AutoResearchIdeaUpdate = Schemas["AutoResearchIdeaUpdate"];
export type AutoResearchSource = Schemas["AutoResearchSource"];
export type AutoResearchSourceCreate = Schemas["AutoResearchSourceCreate"];
export type AutoResearchRunCreate = Schemas["AutoResearchRunCreate"];
export type AutoResearchPaperFile = Schemas["AutoResearchPaperFile"];
export type AutoResearchPaperChatRequest = Schemas["AutoResearchPaperChatRequest"];
export type AutoResearchPaperChatResponse = Schemas["AutoResearchPaperChatResponse"];
export interface AutoResearchPaperContents {
  path: string;
  contents: string;
  etag: string;
}
type Sig = { signal?: AbortSignal };

/* ------------------------------------------------------------------ */
/* system                                                              */
/* ------------------------------------------------------------------ */

export const systemInfo = ({ signal }: Sig = {}) =>
  http.get<SystemInfo>("/system/info", { signal });

export const listModules = ({ signal }: Sig = {}) =>
  http.get<Module[]>("/modules", { signal });

/* ------------------------------------------------------------------ */
/* enrollment                                                          */
/* ------------------------------------------------------------------ */

export const createEnrollmentToken = (body?: { description?: string }) =>
  http.post<EnrollmentToken>("/enrollment-tokens", body);

export const listEnrollmentTokens = ({ signal }: Sig = {}) =>
  http.get<EnrollmentToken[]>("/enrollment-tokens", { signal });

export const deleteEnrollmentToken = (id: string) =>
  http.del<void>(`/enrollment-tokens/${id}`);

/* ------------------------------------------------------------------ */
/* nodes                                                               */
/* ------------------------------------------------------------------ */

export const listNodes = ({ signal }: Sig = {}) => http.get<Node[]>("/nodes", { signal });

export const getNode = (id: string, { signal }: Sig = {}) =>
  http.get<Node>(`/nodes/${id}`, { signal });

export const updateNode = (id: string, body: UpdateNodeRequest) =>
  http.put<Node>(`/nodes/${id}`, body);

export const approveNode = (id: string) => http.post<Node>(`/nodes/${id}/approve`);

export const rotateNodeCertificate = (id: string) =>
  http.post<CertificateInfo>(`/nodes/${id}/rotate-certificate`);

export const listNodeTelemetry = ({ signal }: Sig = {}) =>
  http.get<NodeTelemetrySample[]>("/nodes/telemetry", { signal });

export interface NodeTelemetryQuery {
  resolution?: "5s" | "1m";
  from?: number;
  to?: number;
  limit?: number;
  signal?: AbortSignal;
}

export const getNodeTelemetry = (id: string, q: NodeTelemetryQuery) =>
  http.get<NodeTelemetrySample[]>(
    `/nodes/${id}/telemetry${qs({ resolution: q.resolution, from: q.from, to: q.to, limit: q.limit })}`,
    { signal: q.signal },
  );

/* ------------------------------------------------------------------ */
/* fabrics                                                             */
/* ------------------------------------------------------------------ */

export const listFabrics = ({ signal }: Sig = {}) => http.get<Fabric[]>("/fabrics", { signal });

export const createFabric = (body: CreateFabricRequest) =>
  http.post<Fabric>("/fabrics", body);

export const getFabric = (id: string, { signal }: Sig = {}) =>
  http.get<Fabric>(`/fabrics/${id}`, { signal });

export const updateFabric = (id: string, body: CreateFabricRequest, ifMatch: string) =>
  http.put<Fabric>(`/fabrics/${id}`, body, { headers: { "if-match": ifMatch } });

export const deleteFabric = (id: string, ifMatch: string) =>
  http.del<void>(`/fabrics/${id}`, { headers: { "if-match": ifMatch } });

/* ------------------------------------------------------------------ */
/* recipes                                                             */
/* ------------------------------------------------------------------ */

export const listRecipes = ({ signal }: Sig = {}) => http.get<Recipe[]>("/recipes", { signal });

export const getRecipe = (digest: string, { signal }: Sig = {}) =>
  http.get<RecipeDetail>(`/recipes/${digest}`, { signal });

/** If-Match must carry the recipe digest. */
export const deleteRecipe = (digest: string) =>
  http.del<void>(`/recipes/${digest}`, { headers: { "if-match": digest } });

export const setRecipeTrust = (digest: string, body: RecipeTrustRequest) =>
  http.post<Recipe>(`/recipes/${digest}/trust`, body);

export const importRecipe = (body: RecipeImport) => http.post<Recipe>("/recipes/import", body);

export const createRecipeDraft = (body: RecipeDraftSource) =>
  http.post<{ run_id: string }>("/recipe-drafts", body);
export const listRecipeDrafts = ({ signal }: Sig = {}) =>
  http.get<RecipeDraft[]>("/recipe-drafts", { signal });
export const getRecipeDraft = (id: string, { signal }: Sig = {}) =>
  http.get<RecipeDraft>(`/recipe-drafts/${id}`, { signal });
export const updateRecipeDraft = (
  id: string,
  version: number,
  body: { manifest: Record<string, unknown>; selected_assets: string[] },
) => http.put<RecipeDraft>(`/recipe-drafts/${id}`, body, { headers: { "if-match": String(version) } });
export const packageRecipeDraft = (id: string) =>
  http.post<RecipeDraft>(`/recipe-drafts/${id}/package`);
export const installRecipeDraft = (id: string, permissionDiffAccepted: boolean) =>
  http.post<Recipe>(`/recipe-drafts/${id}/install`, { permission_diff_accepted: permissionDiffAccepted });
export const deleteRecipeDraft = (id: string) => http.del<void>(`/recipe-drafts/${id}`);

/* ------------------------------------------------------------------ */
/* artifacts + transfers                                               */
/* ------------------------------------------------------------------ */

export const listArtifacts = (
  params: { kind?: ArtifactKind; node?: string; signal?: AbortSignal } = {},
) =>
  http.get<Artifact[]>(
    `/artifacts${qs({ kind: params.kind, node: params.node })}`,
    { signal: params.signal },
  );

export const listArtifactPlacements = (id: string, { signal }: Sig = {}) =>
  http.get<Placement[]>(`/artifacts/${id}/placements`, { signal });

export const listTransfers = ({ signal }: Sig = {}) =>
  http.get<Transfer[]>("/transfers", { signal });

export const createTransfer = (body: TransferRequest) =>
  http.post<Transfer>("/transfers", body);

export const getTransfer = (id: string, { signal }: Sig = {}) =>
  http.get<Transfer>(`/transfers/${id}`, { signal });

export const cancelTransfer = (id: string) => http.del<Transfer>(`/transfers/${id}`);

/* ------------------------------------------------------------------ */
/* deployments                                                         */
/* ------------------------------------------------------------------ */

export const listDeployments = ({ signal }: Sig = {}) =>
  http.get<Deployment[]>("/deployments", { signal });

export const getDeployment = (id: string, { signal }: Sig = {}) =>
  http.get<Deployment>(`/deployments/${id}`, { signal });

export const planDeployment = (body: DeploymentPlanRequest) =>
  http.post<DeploymentPlan>("/deployments/plan", body);

export const createDeployment = (body: DeploymentCreateRequest) =>
  http.post<Deployment>("/deployments", body);

export const verifyDeployment = (id: string) =>
  http.post<Deployment>(`/deployments/${id}/verify`);

export const stopDeployment = (id: string) =>
  http.post<Deployment>(`/deployments/${id}/stop`);

export const startDeployment = (id: string) =>
  http.post<Deployment>(`/deployments/${id}/start`);

export const deleteDeployment = (id: string) =>
  http.del<void>(`/deployments/${id}`);

export const chatCompletions = (body: ChatRequest, { signal }: Sig = {}) =>
  http.post<ChatResponse>("/chat/completions", body, { signal });
export const listDeploymentTelemetry = ({ signal }: Sig = {}) =>
  http.get<ServingTelemetrySample[]>("/deployments/telemetry", { signal });

export interface ServingTelemetryQuery {
  resolution?: "5s" | "1m";
  from?: number;
  to?: number;
  limit?: number;
  signal?: AbortSignal;
}

export const getDeploymentTelemetry = (id: string, q: ServingTelemetryQuery) =>
  http.get<ServingTelemetrySample[]>(
    `/deployments/${id}/telemetry${qs({ resolution: q.resolution, from: q.from, to: q.to, limit: q.limit })}`,
    { signal: q.signal },
  );

/* ------------------------------------------------------------------ */
/* runs                                                                */
/* ------------------------------------------------------------------ */

export interface RunsQuery {
  module?: string;
  state?: RunState;
  limit?: number;
  cursor?: string;
  signal?: AbortSignal;
}

export const listRuns = ({ module, state, limit, cursor, signal }: RunsQuery = {}) =>
  http.get<RunsPage>(`/runs${qs({ module, state, limit, cursor })}`, { signal });

export const getRun = (id: string, { signal }: Sig = {}) =>
  http.get<Run>(`/runs/${id}`, { signal });

export const cancelRun = (id: string) => http.post<Run>(`/runs/${id}/cancel`);

/* ------------------------------------------------------------------ */
/* benchmarks                                                          */
/* ------------------------------------------------------------------ */

export const listBenchmarks = ({ signal }: Sig = {}) =>
  http.get<Run[]>("/benchmarks", { signal });

export const createBenchmark = (body: BenchmarkCreate) =>
  http.post<Run>("/benchmarks", body);

export const listBenchmarkResults = ({ signal }: Sig = {}) =>
  http.get<BenchmarkResult[]>("/benchmarks/results", { signal });

/* ------------------------------------------------------------------ */
/* module settings + secrets                                           */
/* ------------------------------------------------------------------ */

export const getModuleSettings = (moduleId: string, { signal }: Sig = {}) =>
  http.get<ModuleSettings>(`/modules/${moduleId}/settings`, { signal });

export const putModuleSettings = (moduleId: string, body: ModuleSettings, ifMatch: string) =>
  http.put<ModuleSettings>(`/modules/${moduleId}/settings`, body, {
    headers: { "if-match": ifMatch },
  });

export const listSecrets = ({ signal }: Sig = {}) => http.get<Secret[]>("/secrets", { signal });

export const putSecret = (body: SecretWrite) => http.post<Secret>("/secrets", body);

export const deleteSecret = (id: string) => http.del<void>(`/secrets/${id}`);

/* ------------------------------------------------------------------ */
/* migration                                                           */
/* ------------------------------------------------------------------ */

export const migrationScan = (body: MigrationScanRequest) =>
  http.post<RunAccepted>("/migration/scan", body);

export const migrationImport = (body: MigrationImportRequest) =>
  http.post<RunAccepted>("/migration/import", body);

/* ------------------------------------------------------------------ */
/* autoresearch                                                        */
/* ------------------------------------------------------------------ */

export const listAutoResearchProjects = ({ signal }: Sig = {}) =>
  http.get<AutoResearchProject[]>("/autoresearch/projects", { signal });

export const createAutoResearchProject = (body: AutoResearchProjectCreate) =>
  http.post<AutoResearchProject>("/autoresearch/projects", body);

export const getAutoResearchProject = (id: string, { signal }: Sig = {}) =>
  http.get<AutoResearchProject>(`/autoresearch/projects/${id}`, { signal });

export const updateAutoResearchProject = (id: string, version: number, body: AutoResearchProjectPatch) =>
  http.put<AutoResearchProject>(`/autoresearch/projects/${id}`, body, {
    headers: { "if-match": String(version) },
  });

export const listAutoResearchIdeas = (projectId: string, { signal }: Sig = {}) =>
  http.get<AutoResearchIdea[]>(`/autoresearch/projects/${projectId}/ideas`, { signal });

export const updateAutoResearchIdea = (
  projectId: string,
  ideaId: string,
  version: number,
  body: AutoResearchIdeaUpdate,
) =>
  http.put<AutoResearchIdea>(`/autoresearch/projects/${projectId}/ideas/${ideaId}`, body, {
    headers: { "if-match": String(version) },
  });

export const selectAutoResearchIdea = (projectId: string, ideaId: string) =>
  http.post<AutoResearchIdea>(`/autoresearch/projects/${projectId}/ideas/${ideaId}/select`);

export const generateAutoResearchIdeas = (projectId: string, body: AutoResearchIdeaGenerate) =>
  http.post<Run>(`/autoresearch/projects/${projectId}/ideas/generate`, body);

export const listAutoResearchSources = (projectId: string, { signal }: Sig = {}) =>
  http.get<AutoResearchSource[]>(`/autoresearch/projects/${projectId}/sources`, { signal });

export const createAutoResearchSource = (projectId: string, body: AutoResearchSourceCreate) =>
  http.post<AutoResearchSource>(`/autoresearch/projects/${projectId}/sources`, body);

export async function uploadAutoResearchSource(projectId: string, file: File): Promise<AutoResearchSource> {
  const body = new FormData();
  body.set("file", file);
  const response = await requestRaw(`/autoresearch/projects/${projectId}/source-files`, {
    method: "POST",
    body,
  });
  return (await response.json()) as AutoResearchSource;
}

export const listAutoResearchRuns = (projectId: string, { signal }: Sig = {}) =>
  http.get<Run[]>(`/autoresearch/projects/${projectId}/runs`, { signal });

export const createAutoResearchRun = (projectId: string, body: AutoResearchRunCreate) =>
  http.post<Run>(`/autoresearch/projects/${projectId}/runs`, body);

export const pauseAutoResearchRun = (runId: string) =>
  http.post<Run>(`/autoresearch/runs/${runId}/pause`);

export const resumeAutoResearchRun = (runId: string) =>
  http.post<Run>(`/autoresearch/runs/${runId}/resume`);

export const stopAutoResearchRun = (runId: string) =>
  http.post<Run>(`/autoresearch/runs/${runId}/stop`);

// The Go router decodes one URL layer before matching its single path
// segment; preserve nested paper paths for securePaperPath's second decode.
function paperPathSegment(path: string): string {
  return encodeURIComponent(encodeURIComponent(path));
}

export const listAutoResearchPaperFiles = (projectId: string, { signal }: Sig = {}) =>
  http.get<AutoResearchPaperFile[]>(`/autoresearch/projects/${projectId}/paper/files`, { signal });

export async function getAutoResearchPaperFile(
  projectId: string,
  path: string,
  { signal }: Sig = {},
): Promise<AutoResearchPaperContents> {
  const response = await requestRaw(
    `/autoresearch/projects/${projectId}/paper/files/${paperPathSegment(path)}`,
    { signal },
  );
  return {
    path,
    contents: await response.text(),
    etag: response.headers.get("etag") ?? "",
  };
}

export async function updateAutoResearchPaperFile(
  projectId: string,
  path: string,
  contents: string,
  etag: string,
): Promise<AutoResearchPaperFile> {
  const response = await requestRaw(
    `/autoresearch/projects/${projectId}/paper/files/${paperPathSegment(path)}`,
    {
      method: "PUT",
      body: contents,
      headers: { "content-type": "text/plain; charset=utf-8", "if-match": etag },
    },
  );
  return (await response.json()) as AutoResearchPaperFile;
}

export const compileAutoResearchPaper = (projectId: string) =>
  http.post<Run>(`/autoresearch/projects/${projectId}/paper/compile`);

export const chatEditAutoResearchPaper = (projectId: string, body: AutoResearchPaperChatRequest) =>
  http.post<AutoResearchPaperChatResponse>(`/autoresearch/projects/${projectId}/paper/chat`, body);

export const releaseAutoResearchPaper = (projectId: string) =>
  http.post<Run>(`/autoresearch/projects/${projectId}/paper/release`);

export const autoResearchPaperPdfUrl = (projectId: string, cacheKey = "") =>
  `${API_BASE}/autoresearch/projects/${projectId}/paper/pdf${qs({ v: cacheKey })}`;

/* ------------------------------------------------------------------ */
/* SSE stream URLs (consumed by the streamEvents helper)               */
/* ------------------------------------------------------------------ */

export const eventsUrl = (types?: string) => `${API_BASE}/events${qs({ types })}`;

export const runLogsUrl = (id: string) => `${API_BASE}/runs/${id}/logs`;

export const deploymentLogsUrl = (id: string, rank?: number) =>
  `${API_BASE}/deployments/${id}/logs${qs({ rank })}`;
