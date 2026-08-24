import { http, HttpResponse } from "msw";

const now = "2026-08-23T12:00:00Z";
const config = {
  candidate_count: 1,
  paper_max_rounds: 5,
  human_gates: { idea_selection: true, paper_post_edit: true, experiment_handback: false },
  roles: {},
  advisors: {},
  fallbacks: {},
  auditor_prompts: {},
};

export const autoResearchProjects = [
  { id: "00000000-0000-4000-8000-000000000001", name: "Awaiting candidate selection", status: "awaiting_idea_selection", idea_prompt: "Test grounded decoding", config, version: 2, created_at: now, updated_at: now },
  { id: "00000000-0000-4000-8000-000000000002", name: "Active experiment", status: "running", idea_prompt: "Measure sparse routing", config, version: 5, created_at: now, updated_at: now },
  { id: "00000000-0000-4000-8000-000000000003", name: "Paused run", status: "running", idea_prompt: "Pause fixture", config, version: 3, created_at: now, updated_at: now },
  { id: "00000000-0000-4000-8000-000000000004", name: "Provider unavailable", status: "failed", idea_prompt: "Provider fixture", config, version: 3, created_at: now, updated_at: now },
  { id: "00000000-0000-4000-8000-000000000005", name: "Paper post-edit", status: "paper_editing", idea_prompt: "Paper fixture", config, version: 8, created_at: now, updated_at: now },
  { id: "00000000-0000-4000-8000-000000000006", name: "Released paper", status: "completed", idea_prompt: "Completed fixture", config, version: 10, created_at: now, updated_at: now },
] as const;

const runBase = { module: "autoresearch", resources: { nodes: [], accelerators: [], fabrics: [] }, input: {}, created_at: now };
export const autoResearchRunsByProject: Record<string, unknown[]> = {
  [autoResearchProjects[0].id]: [],
  [autoResearchProjects[1].id]: [{ ...runBase, id: "10000000-0000-4000-8000-000000000002", kind: "autoresearch-factory", state: "running" }],
  [autoResearchProjects[2].id]: [{ ...runBase, id: "10000000-0000-4000-8000-000000000003", kind: "autoresearch-factory", state: "paused" }],
  [autoResearchProjects[3].id]: [{ ...runBase, id: "10000000-0000-4000-8000-000000000004", kind: "autoresearch-factory", state: "failed", error_code: "autoresearch.provider_unavailable", error_message: "No configured provider passed preflight." }],
  [autoResearchProjects[4].id]: [{ ...runBase, id: "10000000-0000-4000-8000-000000000005", kind: "autoresearch-paper-compile", state: "succeeded", finished_at: now, output: { changed_paths: ["build/manuscript.pdf"] } }],
  [autoResearchProjects[5].id]: [{ ...runBase, id: "10000000-0000-4000-8000-000000000006", kind: "autoresearch-factory", state: "succeeded", finished_at: now }],
};

export const autoResearchEvents = [
  { version: 1, event_id: "e1", run_id: "10000000-0000-4000-8000-000000000002", invocation_id: "i1", node_id: "experiment.coder", timestamp: now, type: "agent.started", payload: { role: "experiment-coder", backend: "codex", model: "qwen-coder" } },
  { version: 1, event_id: "e2", run_id: "10000000-0000-4000-8000-000000000002", invocation_id: "a1", parent_invocation_id: "i1", node_id: "aux:advisor", timestamp: now, type: "advisor.started", payload: { role: "experiment-coder", backend: "claude", model: "haiku" } },
  { version: 1, event_id: "e3", run_id: "10000000-0000-4000-8000-000000000002", invocation_id: "i1", node_id: "experiment.coder", timestamp: now, type: "agent.text.delta", payload: { delta: "Running the audited ablation matrix." } },
] as const;

export const autoResearchHandlers = [
  http.get("*/api/v1/modules", () => HttpResponse.json([{ id: "autoresearch", title: "AutoResearch", enabled: true, route: "/autoresearch", nav: { label: "AutoResearch", order: 45, icon: "autoresearch" } }])),
  http.get("*/api/v1/autoresearch/projects", () => HttpResponse.json(autoResearchProjects)),
  http.get("*/api/v1/autoresearch/projects/:projectId", ({ params }) => HttpResponse.json(autoResearchProjects.find((project) => project.id === params.projectId) ?? null)),
  http.get("*/api/v1/autoresearch/projects/:projectId/ideas", ({ params }) => HttpResponse.json(params.projectId === autoResearchProjects[0].id ? [{ id: "20000000-0000-4000-8000-000000000001", project_id: params.projectId, ordinal: 1, source: "generated", title: "Candidate one", body: "## Research question\nDoes the fixture work?", selected: false, version: 1, created_at: now, updated_at: now }] : [])),
  http.get("*/api/v1/autoresearch/projects/:projectId/sources", ({ params }) => HttpResponse.json(params.projectId === autoResearchProjects[0].id ? [{ id: "30000000-0000-4000-8000-000000000001", project_id: params.projectId, kind: "url", locator: "https://example.test/paper", title: "Restricted fixture", metadata: {}, status: "blocked", error: "license acceptance required", created_at: now }] : [])),
  http.get("*/api/v1/autoresearch/projects/:projectId/runs", ({ params }) => HttpResponse.json(autoResearchRunsByProject[String(params.projectId)] ?? [])),
  http.get("*/api/v1/autoresearch/projects/:projectId/paper/files", ({ params }) => HttpResponse.json(params.projectId === autoResearchProjects[4].id ? [{ path: "PAPER_STATE.md", sha256: "state", size: 256 }, { path: "main.tex", sha256: "main", size: 128 }, { path: "sections/introduction.tex", sha256: "intro", size: 64 }] : [])),
  http.get("*/api/v1/autoresearch/projects/:projectId/paper/files/:path", ({ params }) => {
    const path = decodeURIComponent(String(params.path));
    const contents = path === "PAPER_STATE.md" ? "---\nphase: awaiting_human_edit\n---\n## Claims–Evidence Matrix\n| C-001 | E-001 |\n## Open findings\n| Finding ID | Role | Severity | Status | References | Finding | Resolution |\n|---|---|---|---|---|---|---|\n| REV-001 | paper-reviewer | concern | open | sections/introduction.tex | Tighten the limitation. | |\n## Revision history\n- round 005 compiled\n" : path === "main.tex" ? "\\documentclass{article}\n\\begin{document}\nFixture\\end{document}\n" : "\\section{Introduction}\nFixture\n";
    return new HttpResponse(contents, { headers: { "content-type": "text/plain", etag: `\"${path}\"` } });
  }),
  http.get("*/api/v1/runs/:runId/logs", () => new HttpResponse("event: end\ndata: {}\n\n", { headers: { "content-type": "text/event-stream" } })),
];
