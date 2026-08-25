export const ROADMAP_PAGES = {
  profiles: {
    label: "Profiles & Sharing",
    path: "/profiles",
    eyebrow: "Recipes",
    description: "Reusable runtime profiles and operator-controlled recipe sharing will live here.",
  },
  knowledge: {
    label: "Knowledge & RAG",
    path: "/knowledge",
    eyebrow: "Knowledge",
    description: "Document collections, retrieval indexes, and deployment bindings will live here.",
  },
  leaderboard: {
    label: "Community Leaderboard",
    path: "/benchmarks/leaderboard",
    eyebrow: "Benchmarks",
    description: "Comparable published benchmark results will live here.",
  },
  autoresearch: {
    label: "Autoresearch",
    path: "/research/autoresearch",
    eyebrow: "Research",
    description: "Automated research loops and their evidence will live here.",
  },
  experiments: {
    label: "Experiment Builder",
    path: "/research/experiments",
    eyebrow: "Research",
    description: "Reproducible experiment definitions and controlled variables will live here.",
  },
  workflows: {
    label: "Workflow Builder",
    path: "/research/workflows",
    eyebrow: "Research",
    description: "Multi-stage model and data workflows will live here.",
  },
  scheduled: {
    label: "Scheduled Tasks & Automations",
    path: "/scheduled",
    eyebrow: "Automation",
    description: "Operator schedules and recurring control-plane tasks will live here.",
  },
  usage: {
    label: "Usage & Costs",
    path: "/usage",
    eyebrow: "Operations",
    description: "Measured resource use and cost attribution will live here.",
  },
  fineTuning: {
    label: "Integrated Fine-tuning",
    path: "/fine-tuning",
    eyebrow: "Training",
    description: "Fine-tuning jobs, datasets, checkpoints, and evaluations will live here.",
  },
  projects: {
    label: "Projects",
    path: "/projects",
    eyebrow: "Workspace",
    description: "Project-scoped recipes, deployments, experiments, and notes will live here.",
  },
} as const;

export type RoadmapPage = (typeof ROADMAP_PAGES)[keyof typeof ROADMAP_PAGES];

export function roadmapPageForPath(pathname: string): RoadmapPage | undefined {
  return Object.values(ROADMAP_PAGES).find((page) => page.path === pathname);
}
