import { QueryClient, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import * as api from "~/lib/api";
import { rangePolicy, type TelemetryRange } from "~/lib/telemetry";

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 30_000,
      gcTime: 5 * 60_000,
      retry: 1,
      refetchOnWindowFocus: false,
    },
    mutations: { retry: false },
  },
});

/** Live-ish data (nodes, deployments, runs, transfers). */
const LIVE = 5_000;
/** Catalog-ish data (recipes, artifacts, benchmarks, secrets). */
const CATALOG = 30_000;

export const qk = {
  systemInfo: ["system-info"] as const,
  modules: ["modules"] as const,
  nodes: ["nodes"] as const,
  recipeDrafts: ["recipe-drafts"] as const,
  recipeDraft: (id: string) => ["recipe-drafts", id] as const,
  node: (id: string) => ["nodes", id] as const,
  fabrics: ["fabrics"] as const,
  fabric: (id: string) => ["fabrics", id] as const,
  recipes: ["recipes"] as const,
  recipe: (digest: string) => ["recipes", digest] as const,
  recipeRepositories: ["recipes", "repositories"] as const,
  recipeRepository: (id: string) => ["recipes", "repositories", id] as const,
  artifacts: ["artifacts"] as const,
  placements: (id: string) => ["artifacts", id, "placements"] as const,
  transfers: ["transfers"] as const,
  deployments: ["deployments"] as const,
  deployment: (id: string) => ["deployments", id] as const,
  runs: (f: { module?: string; state?: string }) =>
    ["runs", f.module ?? "", f.state ?? ""] as const,
  run: (id: string) => ["runs", id] as const,
  benchmarkRuns: ["benchmarks", "runs"] as const,
  benchmarkResults: ["benchmarks", "results"] as const,
  secrets: ["secrets"] as const,
  moduleSettings: (id: string) => ["modules", id, "settings"] as const,
  enrollmentTokens: ["enrollment-tokens"] as const,
  nodeTelemetryLatest: ["node-telemetry", "latest"] as const,
  nodeTelemetry: (id: string, range: string) => ["node-telemetry", id, range] as const,
  servingTelemetryLatest: ["serving-telemetry", "latest"] as const,
  servingTelemetry: (id: string, range: string) => ["serving-telemetry", id, range] as const,
};

/* ------------------------------------------------------------------ */
/* queries                                                             */
/* ------------------------------------------------------------------ */

export function useSystemInfo() {
  return useQuery({ queryKey: qk.systemInfo, queryFn: ({ signal }) => api.systemInfo({ signal }), staleTime: CATALOG });
}

export function useModules() {
  return useQuery({ queryKey: qk.modules, queryFn: ({ signal }) => api.listModules({ signal }), staleTime: 5 * 60_000 });
}

export function useNodes() {
  return useQuery({ queryKey: qk.nodes, queryFn: ({ signal }) => api.listNodes({ signal }), staleTime: LIVE });
}

export function useNode(id: string | undefined) {
  return useQuery({
    queryKey: qk.node(id ?? ""),
    enabled: !!id,
    queryFn: ({ signal }) => api.getNode(id as string, { signal }),
    staleTime: LIVE,
  });
}

export function useFabrics() {
  return useQuery({ queryKey: qk.fabrics, queryFn: ({ signal }) => api.listFabrics({ signal }), staleTime: LIVE });
}

export function useFabric(id: string | undefined) {
  return useQuery({
    queryKey: qk.fabric(id ?? ""),
    enabled: !!id,
    queryFn: ({ signal }) => api.getFabric(id as string, { signal }),
    staleTime: LIVE,
  });
}

export function useRecipes() {
  return useQuery({ queryKey: qk.recipes, queryFn: ({ signal }) => api.listRecipes({ signal }), staleTime: CATALOG });
}

export function useRecipeRepositories() {
  return useQuery({
    queryKey: qk.recipeRepositories,
    queryFn: ({ signal }) => api.listRecipeRepositories({ signal }),
    staleTime: CATALOG,
  });
}

export function useRecipeRepository(id: string | undefined) {
  return useQuery({
    queryKey: qk.recipeRepository(id ?? ""),
    enabled: !!id,
    queryFn: ({ signal }) => api.getRecipeRepository(id as string, { signal }),
    staleTime: LIVE,
  });
}

export function useRecipe(digest: string | undefined) {
  return useQuery({
    queryKey: qk.recipe(digest ?? ""),
    enabled: !!digest,
    queryFn: ({ signal }) => api.getRecipe(digest as string, { signal }),
    staleTime: CATALOG,
  });
}

export function useArtifacts(params: { kind?: api.ArtifactKind; node?: string } = {}) {
  return useQuery({
    queryKey: [...qk.artifacts, params.kind ?? "", params.node ?? ""],
    queryFn: ({ signal }) => api.listArtifacts({ ...params, signal }),
    staleTime: CATALOG,
  });
}

export function useTransfers() {
  return useQuery({ queryKey: qk.transfers, queryFn: ({ signal }) => api.listTransfers({ signal }), staleTime: LIVE });
}

export function useLatestNodeTelemetry() {
  return useQuery({
    queryKey: qk.nodeTelemetryLatest,
    queryFn: ({ signal }) => api.listNodeTelemetry({ signal }),
    staleTime: LIVE,
    refetchInterval: LIVE,
  });
}

export function useNodeTelemetry(nodeId: string | undefined, range: string) {
  const policy = rangePolicy(range as TelemetryRange);
  return useQuery({
    queryKey: qk.nodeTelemetry(nodeId ?? "", range),
    enabled: !!nodeId,
    queryFn: ({ signal }) => {
      const to = Math.floor(Date.now() / 1000);
      const from = to - Math.ceil(policy.windowMs / 1000);
      return api.getNodeTelemetry(nodeId as string, {
        resolution: policy.resolution,
        from,
        to,
        limit: policy.limit,
        signal,
      });
    },
    staleTime: LIVE,
    refetchInterval: LIVE,
  });
}

export function useLatestServingTelemetry() {
  return useQuery({
    queryKey: qk.servingTelemetryLatest,
    queryFn: ({ signal }) => api.listDeploymentTelemetry({ signal }),
    staleTime: LIVE,
    refetchInterval: LIVE,
  });
}

export function useServingTelemetry(deploymentId: string | undefined, range: string) {
  const policy = rangePolicy(range as TelemetryRange);
  return useQuery({
    queryKey: qk.servingTelemetry(deploymentId ?? "", range),
    enabled: !!deploymentId,
    queryFn: ({ signal }) => {
      const to = Math.floor(Date.now() / 1000);
      const from = to - Math.ceil(policy.windowMs / 1000);
      return api.getDeploymentTelemetry(deploymentId as string, {
        resolution: policy.resolution,
        from,
        to,
        limit: policy.limit,
        signal,
      });
    },
    staleTime: LIVE,
    refetchInterval: LIVE,
  });
}

export function useDeployments() {
  return useQuery({ queryKey: qk.deployments, queryFn: ({ signal }) => api.listDeployments({ signal }), staleTime: LIVE });
}

export function useDeployment(id: string | undefined) {
  return useQuery({
    queryKey: qk.deployment(id ?? ""),
    enabled: !!id,
    queryFn: ({ signal }) => api.getDeployment(id as string, { signal }),
    staleTime: LIVE,
  });
}

export function useRuns(filter: { module?: string; state?: string } = {}) {
  return useQuery({
    queryKey: qk.runs(filter),
    queryFn: ({ signal }) =>
      api.listRuns({ module: filter.module, state: filter.state as api.RunState | undefined, limit: 50, signal }),
    staleTime: LIVE,
  });
}

export function useRun(id: string | undefined) {
  return useQuery({
    queryKey: qk.run(id ?? ""),
    enabled: !!id,
    queryFn: ({ signal }) => api.getRun(id as string, { signal }),
    staleTime: LIVE,
    refetchInterval: (query) => {
      const state = query.state.data?.state;
      return state && ["succeeded", "failed", "cancelled", "interrupted"].includes(state) ? false : 1_000;
    },
  });
}

export function useBenchmarkRuns() {
  return useQuery({ queryKey: qk.benchmarkRuns, queryFn: ({ signal }) => api.listBenchmarks({ signal }), staleTime: CATALOG });
}

export function useBenchmarkResults() {
  return useQuery({ queryKey: qk.benchmarkResults, queryFn: ({ signal }) => api.listBenchmarkResults({ signal }), staleTime: CATALOG });
}

export function useSecrets() {
  return useQuery({ queryKey: qk.secrets, queryFn: ({ signal }) => api.listSecrets({ signal }), staleTime: CATALOG });
}

export function useModuleSettings(moduleId: string | undefined) {
  return useQuery({
    queryKey: qk.moduleSettings(moduleId ?? ""),
    enabled: !!moduleId,
    queryFn: ({ signal }) => api.getModuleSettings(moduleId as string, { signal }),
    staleTime: CATALOG,
  });
}

export function useEnrollmentTokens(enabled = true) {
  return useQuery({
    queryKey: qk.enrollmentTokens,
    enabled,
    queryFn: ({ signal }) => api.listEnrollmentTokens({ signal }),
    staleTime: CATALOG,
  });
}

export function useRecipeDrafts() {
  return useQuery({ queryKey: qk.recipeDrafts, queryFn: ({ signal }) => api.listRecipeDrafts({ signal }) });
}

export function useRecipeDraft(id: string) {
  return useQuery({ queryKey: qk.recipeDraft(id), queryFn: ({ signal }) => api.getRecipeDraft(id, { signal }), enabled: Boolean(id) });
}
/* ------------------------------------------------------------------ */
/* mutations                                                           */
/* ------------------------------------------------------------------ */

function invalidates(...keys: ReadonlyArray<readonly unknown[]>) {
  return (qc: QueryClient) => {
    for (const k of keys) void qc.invalidateQueries({ queryKey: k });
  };
}

export function useChatCompletions() {
  return useMutation({
    mutationFn: ({
      body,
      signal,
    }: {
      body: api.ChatRequest;
      signal: AbortSignal;
      threadId: string;
    }) => api.chatCompletions(body, { signal }),
  });
}

export function useApproveNode() {
  const qc = useQueryClient();
  return useMutation({

    mutationFn: (id: string) => api.approveNode(id),
    onSuccess: () => invalidates(qk.nodes, qk.systemInfo)(qc),
  });
}

export function useUpdateNode() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ...body }: { id: string } & api.UpdateNodeRequest) =>
      api.updateNode(id, body),
    onSuccess: (n) => {
      invalidates(qk.nodes)(qc);
      qc.setQueryData<api.Node>(qk.node(n.id), n);
    },
  });
}

export function useRotateCertificate() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.rotateNodeCertificate(id),
    onSuccess: () => invalidates(qk.nodes)(qc),
  });
}

export function useCreateFabric() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: api.CreateFabricRequest) => api.createFabric(body),
    onSuccess: () => invalidates(qk.fabrics)(qc),
  });
}

export function useUpdateFabric() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ...body }: { id: string } & api.CreateFabricRequest & { ifMatch: string }) =>
      api.updateFabric(id, body, body.ifMatch),
    onSuccess: (f) => {
      invalidates(qk.fabrics)(qc);
      qc.setQueryData<api.Fabric>(qk.fabric(f.id), f);
    },
  });
}

export function useDeleteFabric() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ifMatch }: { id: string; ifMatch: string }) =>
      api.deleteFabric(id, ifMatch),
    onSuccess: () => invalidates(qk.fabrics)(qc),
  });
}

export function useCreateRecipeDraft() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: api.createRecipeDraft,
    onSuccess: () => invalidates(qk.recipeDrafts)(qc),
  });
}

export function useCheckRecipeUpdates() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: api.checkRecipeUpdates,
    onSuccess: () => invalidates(qk.recipes)(qc),
  });
}

export function usePlanRecipeRepositoryUpdate() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, expected_head_commit }: { id: string; expected_head_commit: string }) =>
      api.planRecipeRepositoryUpdate(id, { expected_head_commit }),
    onSuccess: (_plan, request) => {
      invalidates(qk.recipeRepositories)(qc);
      invalidates(qk.recipeRepository(request.id))(qc);
    },
  });
}

export function useStartRecipeRepositoryUpdate() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ...body }: { id: string } & api.RecipeRepositoryUpdateRequest) =>
      api.startRecipeRepositoryUpdate(id, body),
    onSuccess: (accepted) => {
      invalidates(qk.recipeRepositories)(qc);
      invalidates(qk.deployments)(qc);
      invalidates(qk.run(accepted.run_id))(qc);
    },
  });
}

export function useImportRecipe() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: api.RecipeImport) => api.importRecipe(body),
    onSuccess: () => invalidates(qk.recipes)(qc),
  });
}

export function useSetRecipeTrust() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ digest, ...body }: { digest: string } & api.RecipeTrustRequest) =>
      api.setRecipeTrust(digest, body),
    onSuccess: (r) => {
      invalidates(qk.recipes)(qc);
      qc.setQueryData<api.RecipeDetail>(qk.recipe(r.digest), (old) =>
        old ? { ...old, ...r } : (r as api.RecipeDetail),
      );
    },
  });
}

export function useDeleteRecipe() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (digest: string) => api.deleteRecipe(digest),
    onSuccess: () => invalidates(qk.recipes)(qc),
  });
}

export function useCreateTransfer() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: api.TransferRequest) => api.createTransfer(body),
    onSuccess: () => invalidates(qk.transfers)(qc),
  });
}

export function useCancelTransfer() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.cancelTransfer(id),
    onSuccess: () => invalidates(qk.transfers)(qc),
  });
}

export function usePlanDeployment() {
  return useMutation({
    mutationFn: (body: api.DeploymentPlanRequest) => api.planDeployment(body),
  });
}

export function useCreateDeployment() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: api.DeploymentCreateRequest) => api.createDeployment(body),
    onSuccess: () => invalidates(qk.deployments, qk.runs({}))(qc),
  });
}

export function useVerifyDeployment() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.verifyDeployment(id),
    onSuccess: (d) => {
      invalidates(qk.deployments)(qc);
      qc.setQueryData<api.Deployment>(qk.deployment(d.id), d);
    },
  });
}

export function useStopDeployment() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.stopDeployment(id),
    onSuccess: (d) => {
      invalidates(qk.deployments)(qc);
      qc.setQueryData<api.Deployment>(qk.deployment(d.id), d);
    },
  });
}

export function useStartDeployment() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.startDeployment(id),
    onSuccess: (d) => {
      invalidates(qk.deployments)(qc);
      qc.setQueryData<api.Deployment>(qk.deployment(d.id), d);
    },
  });
}

export function useDeleteDeployment() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.deleteDeployment(id),
    onSuccess: () => invalidates(qk.deployments)(qc),
  });
}

export function useCancelRun() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.cancelRun(id),
    onSuccess: (r) => {
      invalidates(qk.runs({}))(qc);
      qc.setQueryData<api.Run>(qk.run(r.id), r);
    },
  });
}

export function useCreateBenchmark() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: api.BenchmarkCreate) => api.createBenchmark(body),
    onSuccess: () =>
      invalidates(qk.benchmarkRuns, qk.benchmarkResults, qk.runs({}))(qc),
  });
}

export function usePutSecret() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: api.SecretWrite) => api.putSecret(body),
    onSuccess: () => invalidates(qk.secrets)(qc),
  });
}

export function useDeleteSecret() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.deleteSecret(id),
    onSuccess: () => invalidates(qk.secrets)(qc),
  });
}

export function usePutModuleSettings() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      moduleId,
      body,
      ifMatch,
    }: {
      moduleId: string;
      body: api.ModuleSettings;
      ifMatch: string;
    }) => api.putModuleSettings(moduleId, body, ifMatch),
    onSuccess: (s) => qc.setQueryData(qk.moduleSettings(s.module), s),
  });
}

export function useCreateEnrollmentToken() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body?: { description?: string }) => api.createEnrollmentToken(body),
    onSuccess: () => invalidates(qk.enrollmentTokens)(qc),
  });
}

export function useDeleteEnrollmentToken() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.deleteEnrollmentToken(id),
    onSuccess: () => invalidates(qk.enrollmentTokens)(qc),
  });
}
