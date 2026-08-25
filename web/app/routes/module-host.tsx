import { matchPath, useLocation } from "react-router";

import { useModules } from "~/lib/queries";
import { uiModules } from "~/module-loader";
import { roadmapPageForPath } from "~/roadmap-pages";
import { RoadmapSkeleton } from "~/routes/roadmap-skeleton";

export default function ModuleHost() {
  const location = useLocation();
  const modules = useModules();
  const roadmapPage = roadmapPageForPath(location.pathname);
  if (roadmapPage) return <RoadmapSkeleton page={roadmapPage} />;
  const enabled = new Set((modules.data ?? []).map((descriptor) => descriptor.id));
  for (const module of uiModules) {
    if (!enabled.has(module.id)) continue;
    for (const route of module.routes) {
      if (matchPath({ path: route.path, end: true }, location.pathname)) {
        const Component = route.component;
        return <Component />;
      }
    }
  }
  return (
    <main className="p-6">
      <p className="lmw-label text-fault">route unavailable</p>
      <h1 className="mt-2 font-display text-2xl">No enabled module owns {location.pathname}</h1>
    </main>
  );
}
