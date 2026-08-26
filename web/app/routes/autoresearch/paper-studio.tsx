import { useEffect, useMemo, useState } from "react";
import CodeMirror from "@uiw/react-codemirror";
import { markdown } from "@codemirror/lang-markdown";
import { latex } from "codemirror-lang-latex";
import { BookOpenCheck, Bot, FileCode2, FileText, Hammer, Play, Send, Upload } from "lucide-react";
import { Button } from "~/components/ui/button";
import { ApiError } from "~/lib/api/client";
import { autoResearchPaperPdfUrl, type AutoResearchPaperFile, type Run } from "~/lib/api";
import {
  useAutoResearchPaperFile,
  useChatEditAutoResearchPaper,
  useCompileAutoResearchPaper,
  useCreateAutoResearchRun,
  useReleaseAutoResearchPaper,
  useSaveAutoResearchPaperFile,
} from "~/lib/queries";
import { cn } from "~/lib/utils";

interface Finding {
  id: string;
  severity: string;
  text: string;
}

function parseFindings(state: string): Finding[] {
  const findings: Finding[] = [];
  for (const line of state.split("\n")) {
    const cells = line.split("|").map((cell) => cell.trim()).filter(Boolean);
    if (cells.length >= 6 && /^[A-Z][A-Z0-9_-]{2,}$/.test(cells[0]) && cells[3].toLowerCase() === "open") {
      findings.push({ id: cells[0], severity: cells[2].toLowerCase(), text: cells[5] });
      continue;
    }
    const match = line.match(/^\s*-\s*([A-Z][A-Z0-9_-]{2,})\s*:\s*(nit|concern|blocker|open)\s*:\s*(.+)$/i);
    if (match) findings.push({ id: match[1], severity: match[2].toLowerCase(), text: match[3].trim() });
  }
  return findings;
}

function extractSection(state: string, heading: string): string[] {
  const lines = state.split("\n");
  const start = lines.findIndex((line) => line.trim().toLowerCase().includes(heading.toLowerCase()));
  if (start < 0) return [];
  const result: string[] = [];
  for (let index = start + 1; index < lines.length; index += 1) {
    if (/^#{1,3}\s/.test(lines[index])) break;
    if (lines[index].trim() !== "") result.push(lines[index]);
  }
  return result.slice(0, 18);
}

function fileLanguage(path: string) {
  if (path.endsWith(".md")) return [markdown()];
  if (path.endsWith(".tex") || path.endsWith(".bib") || path.endsWith(".sty") || path.endsWith(".cls")) return [latex({ fileName: path })];
  return [];
}

export function PaperStudio({ projectId, files, runs }: { projectId: string; files: AutoResearchPaperFile[]; runs: Run[] }) {
  const [activePath, setActivePath] = useState("");
  const [draft, setDraft] = useState("");
  const [chat, setChat] = useState("");
  const [tab, setTab] = useState<"findings" | "claims" | "history">("findings");
  const current = useAutoResearchPaperFile(projectId, activePath || undefined);
  const state = useAutoResearchPaperFile(projectId, files.some((file) => file.path === "PAPER_STATE.md") ? "PAPER_STATE.md" : undefined);
  const save = useSaveAutoResearchPaperFile(projectId);
  const compile = useCompileAutoResearchPaper(projectId);
  const writer = useChatEditAutoResearchPaper(projectId);
  const release = useReleaseAutoResearchPaper(projectId);
  const continueRounds = useCreateAutoResearchRun(projectId);

  useEffect(() => {
    if (!activePath && files.length > 0) {
      const preferred = files.find((file) => file.path === "main.tex") ?? files[0];
      setActivePath(preferred.path);
    }
  }, [activePath, files]);

  useEffect(() => {
    if (current.data) setDraft(current.data.contents);
  }, [current.data]);

  const stateText = state.data?.contents ?? "";
  const findings = useMemo(() => parseFindings(stateText), [stateText]);
  const claims = useMemo(() => extractSection(stateText, "Claims–Evidence Matrix"), [stateText]);
  const history = useMemo(() => extractSection(stateText, "Revision history"), [stateText]);
  const latestPaperRun = runs.find((run) => run.kind === "autoresearch-paper-compile" || run.kind === "autoresearch-factory");
  const pdfKey = `${latestPaperRun?.id ?? "none"}-${latestPaperRun?.finished_at ?? "pending"}`;
  const pending = save.isPending || compile.isPending || writer.isPending || release.isPending || continueRounds.isPending;
  const conflict = save.error instanceof ApiError && save.error.code === "paper.edit_conflict";
  const grouped = findings.reduce<Record<string, Finding[]>>((result, finding) => {
    (result[finding.severity] ??= []).push(finding);
    return result;
  }, {});

  return (
    <section className="grid gap-3" aria-label="Paper editing studio">
      <div className="grid gap-3 xl:grid-cols-[minmax(0,1.15fr)_minmax(420px,0.85fr)]">
        <div className="lmw-panel min-w-0 overflow-hidden">
          <header className="lmw-panel-head">
            <h2 className="lmw-label">paper source</h2>
            <span className="ml-auto max-w-[50%] truncate font-mono text-[10px] text-faint">{activePath || "no file"}</span>
            <Button size="sm" variant="outline" disabled={pending || !current.data || draft === current.data.contents} onClick={() => current.data && save.mutate({ path: activePath, contents: draft, etag: current.data.etag })}><Upload aria-hidden /> save</Button>
          </header>
          <div className="grid min-h-[620px] grid-cols-[180px_minmax(0,1fr)] max-sm:grid-cols-1">
            <nav className="overflow-auto border-r border-hairline bg-raised/25 p-2 max-sm:max-h-36 max-sm:border-b max-sm:border-r-0" aria-label="Paper files">
              {files.map((file) => (
                <button key={file.path} type="button" className={cn("control flex w-full items-center gap-1.5 rounded px-2 py-1.5 text-left font-mono text-[10px]", activePath === file.path ? "bg-primary/12 text-primary" : "text-muted hover:bg-raised hover:text-foreground")} onClick={() => setActivePath(file.path)}>
                  {file.path.endsWith(".tex") ? <FileCode2 className="h-3 w-3" aria-hidden /> : <FileText className="h-3 w-3" aria-hidden />}<span className="truncate">{file.path}</span>
                </button>
              ))}
            </nav>
            <div className="min-w-0 bg-panel">
              {current.isPending ? <p className="p-4 font-mono text-xs text-muted">loading source…</p> : current.isError ? <p className="p-4 font-mono text-xs text-fault">{current.error.message}</p> : (
                <CodeMirror value={draft} height="620px" theme="light" extensions={fileLanguage(activePath)} onChange={setDraft} basicSetup={{ foldGutter: true, lineNumbers: true, highlightActiveLine: true }} aria-label={`Editor for ${activePath}`} />
              )}
            </div>
          </div>
          {conflict ? <div className="flex items-center gap-2 border-t border-warn/40 bg-warn/10 px-3 py-2 font-mono text-[10px] text-warn"><span>Source changed after you opened it. Reload before saving.</span><Button size="sm" variant="outline" onClick={() => void current.refetch()}>reload</Button></div> : null}
        </div>

        <div className="grid min-w-0 gap-3 grid-rows-[minmax(360px,1fr)_auto]">
          <div className="lmw-panel overflow-hidden">
            <header className="lmw-panel-head"><h2 className="lmw-label">compiled manuscript</h2><span className="ml-auto font-mono text-[10px] text-faint">PDF</span></header>
            <iframe title="Compiled paper preview" src={autoResearchPaperPdfUrl(projectId, pdfKey)} className="h-[560px] w-full bg-white" />
          </div>
          <div className="lmw-panel">
            <header className="lmw-panel-head"><h2 className="lmw-label">release controls</h2></header>
            <div className="flex flex-wrap gap-2 p-3">
              <Button size="sm" variant="outline" disabled={pending} onClick={() => compile.mutate()}><Hammer aria-hidden /> compile</Button>
              <Button size="sm" variant="outline" disabled={pending} onClick={() => continueRounds.mutate({ factory: "paper" })}><Play aria-hidden /> continue rounds</Button>
              <Button size="sm" disabled={pending} onClick={() => release.mutate()}><BookOpenCheck aria-hidden /> release</Button>
            </div>
          </div>
        </div>
      </div>

      <div className="grid gap-3 lg:grid-cols-[minmax(0,1fr)_420px]">
        <div className="lmw-panel overflow-hidden">
          <header className="lmw-panel-head gap-1">
            {(["findings", "claims", "history"] as const).map((value) => <button key={value} type="button" className={cn("control rounded px-2 py-1 font-mono text-[10px]", tab === value ? "bg-raised text-foreground" : "text-faint hover:text-foreground")} onClick={() => setTab(value)}>{value}</button>)}
          </header>
          <div className="min-h-40 p-3">
            {tab === "findings" ? (
              findings.length === 0 ? <p className="font-mono text-xs text-faint">No open findings parsed from PAPER_STATE.md.</p> : ["blocker", "concern", "nit", "open"].map((severity) => grouped[severity]?.length ? <div key={severity} className="mb-3"><p className="lmw-label mb-1">{severity}</p>{grouped[severity]?.map((finding) => <article key={finding.id} className="mb-1 rounded border border-hairline bg-raised/35 px-2 py-1.5"><span className="mr-2 font-mono text-[10px] text-primary">{finding.id}</span><span className="text-xs text-muted">{finding.text}</span></article>)}</div> : null)
            ) : tab === "claims" ? <pre className="whitespace-pre-wrap font-mono text-[10px] leading-5 text-muted">{claims.join("\n") || "Claims–Evidence Matrix unavailable."}</pre> : <pre className="whitespace-pre-wrap font-mono text-[10px] leading-5 text-muted">{history.join("\n") || "No revision history yet."}</pre>}
          </div>
        </div>

        <div className="lmw-panel overflow-hidden">
          <header className="lmw-panel-head"><h2 className="lmw-label inline-flex items-center gap-1"><Bot className="h-3.5 w-3.5" aria-hidden /> writer chat</h2></header>
          <div className="p-3">
            <textarea value={chat} onChange={(event) => setChat(event.target.value)} className="min-h-28 w-full resize-y rounded border border-hairline bg-background px-3 py-2 font-mono text-xs" placeholder="Ask the paper writer for a scoped edit to the open source…" />
            <Button size="sm" className="mt-2 w-full" disabled={pending || chat.trim() === "" || !current.data} onClick={() => current.data && writer.mutate({ message: chat, base_etags: { [activePath]: current.data.etag } }, { onSuccess: () => setChat("") })}><Send aria-hidden /> apply writer edit</Button>
            {writer.data ? <p className="mt-2 font-mono text-[10px] text-ok">changed {writer.data.changed_paths.length} file{writer.data.changed_paths.length === 1 ? "" : "s"}</p> : null}
            {(writer.error || compile.error || release.error || continueRounds.error) ? <p className="mt-2 font-mono text-[10px] text-fault">{(writer.error ?? compile.error ?? release.error ?? continueRounds.error)?.message}</p> : null}
          </div>
        </div>
      </div>
    </section>
  );
}
