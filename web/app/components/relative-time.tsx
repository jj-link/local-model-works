import { useEffect, useState } from "react";
import { relativeTime } from "~/lib/format";

/** Relative timestamp that refreshes itself (30s tick). */
export function RelativeTime({ iso, className }: { iso: string | null | undefined; className?: string }) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 30_000);
    return () => clearInterval(t);
  }, []);
  return (
    <time dateTime={iso ?? undefined} title={iso ?? undefined} className={className}>
      {relativeTime(iso, now)}
    </time>
  );
}
