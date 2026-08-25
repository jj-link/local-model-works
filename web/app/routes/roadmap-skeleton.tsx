import { Construction } from "lucide-react";
import type { RoadmapPage } from "~/roadmap-pages";

export function RoadmapSkeleton({ page }: { page: RoadmapPage }) {
  return (
    <div className="grid gap-4">
      <header className="border-b border-hairline px-1 pb-5 pt-2">
        <p className="lmw-label">{page.eyebrow}</p>
        <h1 className="mt-1 font-display text-3xl font-normal">{page.label}</h1>
        <p className="mt-2 max-w-2xl text-sm text-muted">{page.description}</p>
      </header>

      <section className="lmw-panel overflow-hidden">
        <div className="lmw-panel-head flex items-center gap-2">
          <Construction className="h-4 w-4 text-primary" aria-hidden />
          <h2 className="font-display text-lg font-semibold">Section skeleton</h2>
        </div>
        <div className="grid gap-4 p-5 md:grid-cols-[minmax(0,1.4fr)_minmax(16rem,0.6fr)]">
          <div className="grid gap-3" aria-hidden>
            <span className="h-9 rounded border border-hairline bg-raised" />
            <span className="h-28 rounded border border-hairline bg-raised" />
            <div className="grid grid-cols-2 gap-3">
              <span className="h-20 rounded border border-hairline bg-raised" />
              <span className="h-20 rounded border border-hairline bg-raised" />
            </div>
          </div>
          <div className="rounded border border-hairline bg-raised p-4">
            <p className="lmw-label">Not connected</p>
            <p className="mt-2 text-sm leading-6 text-muted">
              This destination is present to preserve the Sample A navigation. It does not claim data or actions until a production API exists.
            </p>
          </div>
        </div>
      </section>
    </div>
  );
}
