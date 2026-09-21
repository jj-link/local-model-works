// One typed function per OpenAPI operation (operationId-named). Generated
// types come from ~/generated/api (openapi-typescript).
import { API_BASE, http, qs } from "./client";
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
export type FabricBinding = Schemas["FabricBinding"];
export type CreateFabricRequest = Schemas["CreateFabricRequest"];
export type Recipe = Schemas["Recipe"];
export type RecipeDetail = Schemas["RecipeDetail"];
export type RecipeUpdateStatus = Schemas["RecipeUpdateStatus"];
export type RecipeRepository = Schemas["RecipeRepository"];
export type RecipeRepositoryDetail = Schemas["RecipeRepositoryDetail"];
export type RecipeRepositoryReplacementPlanRequest = Schemas["RecipeRepositoryReplacementPlanRequest"];
export type RecipeRepositoryReplacementRequest = Schemas["RecipeRepositoryReplacementRequest"];
export type RecipeRepositoryUpdateRequest = Schemas["RecipeRepositoryUpdateRequest"];
export type RecipeRepositoryUpdatePlan = Schemas["RecipeRepositoryUpdatePlan"];
export type RecipeUpdatePlan = Schemas["RecipeUpdatePlan"];
export type DeploymentConfigurationPlanRequest = Schemas["DeploymentConfigurationPlanRequest"];
export type DeploymentConfigurationRequest = Schemas["DeploymentConfigurationRequest"];
export type RecipeUpdateDevice = Schemas["RecipeUpdateDevice"];
export type RecipeUpdateRunningDeployment = Schemas["RecipeUpdateRunningDeployment"];
export type RecipeUpdateAccepted = Schemas["RecipeUpdateAccepted"];
export type RecipeSource = Schemas["RecipeSource"];
export type RecipeImport = Schemas["RecipeImport"];
export type RecipeDraft = Schemas["RecipeDraft"];
export type RecipeDraftSource = Schemas["RecipeDraftSource"];
export type RecipeDraftAssetSelection = Schemas["RecipeDraftAssetSelection"];
export type RecipeDraftContextFile = Schemas["RecipeDraftContextFile"];
export type RecipeChangeContext = Schemas["RecipeChangeContext"];
export type RecipeChangeComparison = Schemas["RecipeChangeComparison"];
export type RecipeGenerationPreviewRequest = Schemas["RecipeGenerationPreviewRequest"];
export type RecipeGenerationRequest = Schemas["RecipeGenerationRequest"];
export type RecipeGenerationPreview = Schemas["RecipeGenerationPreview"];
export type RecipeDownloadPlanRequest = Schemas["RecipeDownloadPlanRequest"];
export type RecipeDownloadCreateRequest = Schemas["RecipeDownloadCreateRequest"];
export type RecipeDownloadResumeRequest = Schemas["RecipeDownloadResumeRequest"];
export type RecipeDownloadPlan = Schemas["RecipeDownloadPlan"];
export type RecipeDownloadRun = Schemas["RecipeDownloadRun"];
export type RecipeAvailabilityRequest = Schemas["RecipeAvailabilityRequest"];
export type RecipeAvailability = Schemas["RecipeAvailability"];
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
export type LaunchProfile = Schemas["LaunchProfile"];
export type LaunchProfileUpsert = Schemas["LaunchProfileUpsert"];
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
export type BenchmarkRunResult = Schemas["BenchmarkRunResult"];
export type BenchmarkCatalogResponse = Schemas["BenchmarkCatalogResponse"];
export type BenchmarkCatalogEntry = Schemas["BenchmarkCatalogEntry"];
export type BenchmarkLanguageResult = Schemas["BenchmarkLanguageResult"];
export type BenchmarkRunnerAvailability = Schemas["BenchmarkRunnerAvailability"];
export type Module = Schemas["Module"];
export type ModuleSettings = Schemas["ModuleSettings"];
export type Secret = Schemas["Secret"];
export type SecretWrite = Schemas["SecretWrite"];
export type SystemInfo = Schemas["SystemInfo"];
export type EnrollmentToken = Schemas["EnrollmentToken"];
export type MigrationScanRequest = Schemas["MigrationScanRequest"];
export type MigrationImportRequest = Schemas["MigrationImportRequest"];
export type RunAccepted = Schemas["RunAccepted"];
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

export const listRecipeRepositories = ({ signal }: Sig = {}) =>
  http.get<RecipeRepository[]>("/recipe-repositories", { signal });

export const getRecipeRepository = (id: string, { signal }: Sig = {}) =>
  http.get<RecipeRepositoryDetail>(`/recipe-repositories/${encodeURIComponent(id)}`, { signal });

export const planRecipeRepositoryUpdate = (id: string, { signal }: Sig = {}) =>
  http.post<RecipeRepositoryUpdatePlan>(`/recipe-repositories/${encodeURIComponent(id)}/updates/plan`, undefined, { signal });

export const startRecipeRepositoryUpdate = (id: string, body: RecipeRepositoryUpdateRequest) =>
  http.post<RecipeUpdateAccepted>(`/recipe-repositories/${encodeURIComponent(id)}/updates`, body);

export const planRecipeRepositoryReplacement = (
  id: string,
  body: RecipeRepositoryReplacementPlanRequest,
) => http.post<RecipeUpdatePlan>(`/recipe-repositories/${encodeURIComponent(id)}/replacements/plan`, body);

export const startRecipeRepositoryReplacement = (
  id: string,
  body: RecipeRepositoryReplacementRequest,
) => http.post<RecipeUpdateAccepted>(`/recipe-repositories/${encodeURIComponent(id)}/replacements`, body);

export const planDeploymentConfiguration = (id: string, body: DeploymentConfigurationPlanRequest) =>
  http.post<RecipeUpdatePlan>(`/deployments/${encodeURIComponent(id)}/configuration/plan`, body);

export const applyDeploymentConfiguration = (id: string, body: DeploymentConfigurationRequest) =>
  http.post<RecipeUpdateAccepted>(`/deployments/${encodeURIComponent(id)}/configuration`, body);

export const checkRecipeRepositoryUpdates = (id: string) =>
  http.post<RecipeUpdateStatus>(`/recipe-repositories/${encodeURIComponent(id)}/check-updates`);

export const listRecipes = ({ signal }: Sig = {}) => http.get<Recipe[]>("/recipes", { signal });

export const getRecipe = (digest: string, { signal }: Sig = {}) =>
  http.get<RecipeDetail>(`/recipes/${digest}`, { signal });

export const checkRecipeUpdates = () =>
  http.post<RecipeUpdateStatus[]>("/recipes/check-updates");

/** If-Match must carry the recipe digest. */
export const deleteRecipe = (digest: string) =>
  http.del<void>(`/recipes/${digest}`, { headers: { "if-match": digest } });


export const importRecipe = (body: RecipeImport) => http.post<Recipe>("/recipes/import", body);

export type RecipeDraftOperationAccepted = { draft_id: string; run_id: string };
export const createRecipeDraft = (body: RecipeDraftSource) =>
  http.post<RecipeDraftOperationAccepted>("/recipe-drafts", body);
export const listRecipeDrafts = ({ signal, repository_id, package_digest }: Sig & { repository_id?: string; package_digest?: string } = {}) =>
  http.get<RecipeDraft[]>(`/recipe-drafts${qs({ repository_id, package_digest })}`, { signal });
export const getRecipeDraft = (id: string, { signal }: Sig = {}) =>
  http.get<RecipeDraft>(`/recipe-drafts/${id}`, { signal });
export const updateRecipeDraft = (
  id: string,
  version: number,
  body: { manifest: Record<string, unknown>; selected_assets: RecipeDraftAssetSelection[]; answers?: { question_id: string; answer: string }[]; acknowledged_warnings?: string[] },
) => http.put<RecipeDraft>(`/recipe-drafts/${id}`, body, { headers: { "if-match": String(version) } });
export const updateRecipeDraftContext = (id: string, version: number, files: RecipeDraftContextFile[]) =>
  http.put<RecipeDraft>(`/recipe-drafts/${id}/context`, { files }, { headers: { "if-match": String(version) } });
export const getRecipeDraftFile = (id: string, selection: { path: string; sha256: string; source_commit?: string }, { signal }: Sig = {}) =>
  http.get<{ path: string; sha256: string; origin: string; content: string }>(`/recipe-drafts/${id}/files${qs(selection)}`, { signal });
export const updateRecipeDraftFile = (id: string, version: number, body: { path: string; content: string }) =>
  http.put<RecipeDraft>(`/recipe-drafts/${id}/files`, body, { headers: { "if-match": String(version) } });
export const generateRecipeDraft = (id: string, version: number, body: RecipeGenerationRequest) =>
  http.post<RecipeDraftOperationAccepted>(`/recipe-drafts/${id}/generate`, body, { headers: { "if-match": String(version) } });
export const acceptRecipeDraftProposal = (id: string, version: number, proposalId: string) =>
  http.post<RecipeDraft>(`/recipe-drafts/${id}/proposal/accept`, { proposal_id: proposalId }, { headers: { "if-match": String(version) } });
export const discardRecipeDraftProposal = (id: string, version: number, proposalId: string) =>
  http.delJson<RecipeDraft>(`/recipe-drafts/${id}/proposal`, { proposal_id: proposalId }, { headers: { "if-match": String(version) } });
export const packageRecipeDraft = (id: string, version: number) =>
  http.post<RecipeDraftOperationAccepted>(`/recipe-drafts/${id}/package`, undefined, { headers: { "if-match": String(version) } });
export const resolveRecipeDraftReferences = (id: string, version: number, body: { credentials?: { path: string; host: string; secret_id: string }[]; file_checksums?: { path: string; url: string; sha256: string; evidence_note: string }[] } = {}) =>
  http.post<RecipeDraftOperationAccepted>(`/recipe-drafts/${id}/resolve`, body, { headers: { "if-match": String(version) } });
export const dismissRecipeDraftDiagnostics = (id: string, version: number, diagnosticIds: string[]) =>
  http.post<RecipeDraft>(`/recipe-drafts/${id}/diagnostics/dismiss`, { diagnostic_ids: diagnosticIds }, { headers: { "if-match": String(version) } });
export const getRecipeDraftRunExcerpt = (id: string, { signal }: Sig = {}) =>
  http.get<{ run_id: string; stdout: string; stderr: string; truncated: boolean }>(`/recipe-drafts/${id}/run-excerpt`, { signal });
export const installRecipeDraft = (id: string, version: number, body: { package_digest: string; acknowledged_warnings: string[] }) =>
  http.post<RecipeDraftOperationAccepted>(`/recipe-drafts/${id}/install`, body, { headers: { "if-match": String(version) } });
export const deleteRecipeDraft = (id: string, version: number) =>
  http.del<void>(`/recipe-drafts/${id}`, { headers: { "if-match": String(version) } });
export const inspectRecipeDraft = (id: string, version: number) =>
  http.post<RecipeDraftOperationAccepted>(`/recipe-drafts/${id}/inspect`, undefined, { headers: { "if-match": String(version) } });
export const createRecipeChange = (digest: string, body: { kind: "repair" | "update"; expected_current_digest: string; expected_head_commit?: string; deployment_id?: string }) =>
  http.post<RecipeDraftOperationAccepted>(`/recipes/${encodeURIComponent(digest)}/changes`, body);
export const compareRecipeDraft = (id: string, { signal }: Sig = {}) =>
  http.get<RecipeChangeComparison>(`/recipe-drafts/${id}/comparison`, { signal });
export const previewRecipeGeneration = (id: string, version: number, body: RecipeGenerationPreviewRequest) =>
  http.post<RecipeGenerationPreview>(`/recipe-drafts/${id}/generation-preview`, body, { headers: { "if-match": String(version) } });
export const planRecipeDownload = (digest: string, body: RecipeDownloadPlanRequest) =>
  http.post<RecipeDownloadPlan>(`/recipes/${encodeURIComponent(digest)}/downloads/plan`, body);
export const startRecipeDownload = (digest: string, body: RecipeDownloadCreateRequest) =>
  http.post<{ run_id: string }>(`/recipes/${encodeURIComponent(digest)}/downloads`, body);
export const listRecipeDownloads = (digest: string, { signal }: Sig = {}) =>
  http.get<RecipeDownloadRun[]>(`/recipes/${encodeURIComponent(digest)}/downloads`, { signal });
export const resumeRecipeDownload = (digest: string, runID: string, body: RecipeDownloadResumeRequest) =>
  http.post<{ run_id: string }>(`/recipes/${encodeURIComponent(digest)}/downloads/${encodeURIComponent(runID)}/resume`, body);
export const getRecipeAvailability = (digest: string, body: RecipeAvailabilityRequest, { signal }: Sig = {}) =>
  http.post<RecipeAvailability>(`/recipes/${encodeURIComponent(digest)}/availability`, body, { signal });
export type RecipeAssistantProvider = Schemas["RecipeAssistantProvider"];
export const listRecipeAssistantProviders = ({ signal }: Sig = {}) =>
  http.get<Schemas["RecipeAssistantProviderCatalog"]>("/recipe-assistant/providers", { signal });
export const testRecipeAssistantProvider = (id: string) =>
  http.post<{ ok: boolean; model: string; message: string }>(`/recipe-assistant/providers/${encodeURIComponent(id)}/test`);
export const getRecipeAssistantCodexStatus = ({ signal }: Sig = {}) =>
  http.get<{ available: boolean; connected: boolean; email?: string; plan?: string; error?: string }>("/recipe-assistant/codex/status", { signal });
export const startRecipeAssistantCodexLogin = () =>
  http.post<{ login_id: string; verification_url: string; user_code: string }>("/recipe-assistant/codex/login");
export const getRecipeAssistantCodexLogin = (id: string, { signal }: Sig = {}) =>
  http.get<{ login_id: string; state: string; error?: string }>(`/recipe-assistant/codex/login/${encodeURIComponent(id)}`, { signal });
export const cancelRecipeAssistantCodexLogin = (id: string) =>
  http.del<void>(`/recipe-assistant/codex/login/${encodeURIComponent(id)}`);
export const logoutRecipeAssistantCodex = () => http.post<void>("/recipe-assistant/codex/logout");
export const listRecipeAssistantCodexModels = ({ signal }: Sig = {}) =>
  http.get<{ id: string; display_name: string; description?: string; default: boolean }[]>("/recipe-assistant/codex/models", { signal });

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

export const listLaunchProfiles = (recipeDigest: string, { signal }: Sig = {}) =>
  http.get<LaunchProfile[]>(
    `/recipes/${encodeURIComponent(recipeDigest)}/launch-profiles`,
    { signal },
  );

export const createLaunchProfile = (recipeDigest: string, body: LaunchProfileUpsert) =>
  http.post<LaunchProfile>(
    `/recipes/${encodeURIComponent(recipeDigest)}/launch-profiles`,
    body,
  );

export const updateLaunchProfile = (id: string, body: LaunchProfileUpsert) =>
  http.put<LaunchProfile>(`/launch-profiles/${encodeURIComponent(id)}`, body);

export const deleteLaunchProfile = (id: string) =>
  http.del<void>(`/launch-profiles/${encodeURIComponent(id)}`);

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
  http.get<Schemas["BenchmarkRunSummary"][]>("/benchmarks/results", { signal });

export const getBenchmark = (runId: string, { signal }: Sig = {}) =>
  http.get<Schemas["BenchmarkRunSummary"]>(`/benchmarks/${runId}`, { signal });

export const listBenchmarkTrials = (runId: string, { signal }: Sig = {}) =>
  http.get<Schemas["BenchmarkTrialResult"][]>(`/benchmarks/${runId}/trials`, { signal });

export const cancelBenchmark = (runId: string) =>
  http.post<Run>(`/benchmarks/${runId}/cancel`);

export const benchmarkBundleUrl = (runId: string) => `${API_BASE}/benchmarks/${runId}/bundle`;

export const getBenchmarkCatalog = ({ signal }: Sig = {}) =>
  http.get<BenchmarkCatalogResponse>("/benchmarks/catalog", { signal });

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
/* SSE stream URLs (consumed by the streamEvents helper)               */
/* ------------------------------------------------------------------ */

export const eventsUrl = (types?: string) => `${API_BASE}/events${qs({ types })}`;

export const runLogsUrl = (id: string) => `${API_BASE}/runs/${id}/logs`;

export const deploymentLogsUrl = (id: string, rank?: number) =>
  `${API_BASE}/deployments/${id}/logs${qs({ rank })}`;
