import { useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router";
import { MessageSquare, Plus, RotateCcw, Send, Server, Square } from "lucide-react";
import { PlanDeploymentDialog } from "~/components/dialogs/plan-deployment-dialog";
import { EmptyState, ErrorState, LoadingPanel } from "~/components/empty-state";
import { Button } from "~/components/ui/button";
import { useChatCompletions, useDeployments } from "~/lib/queries";
import type { ChatResponse, Deployment } from "~/lib/api";
import { cn } from "~/lib/utils";

type MessageState = "complete" | "waiting" | "error";

type ThreadMessage = {
  id: string;
  role: "system" | "user" | "assistant";
  content: string;
  reasoning?: string;
  usage?: ChatResponse["usage"];
  state: MessageState;
};

type ChatThread = {
  id: string;
  title: string;
  deploymentId: string;
  messages: ThreadMessage[];
};

type ActiveRequest = {
  controller: AbortController;
  threadId: string;
  waitingId: string;
};

const initialThread: ChatThread = {
  id: "thread-1",
  title: "New chat",
  deploymentId: "",
  messages: [],
};

export default function Chat() {
  const [searchParams] = useSearchParams();
  const requestedDeployment = searchParams.get("deployment") ?? "";
  const deploymentsQuery = useDeployments();
  const completion = useChatCompletions();
  const [threads, setThreads] = useState<ChatThread[]>([initialThread]);
  const [activeThreadId, setActiveThreadId] = useState(initialThread.id);
  const activeThreadIdRef = useRef(activeThreadId);
  const [composer, setComposer] = useState("");
  const [planOpen, setPlanOpen] = useState(false);
  const requestRef = useRef<ActiveRequest | null>(null);
  const composerRef = useRef<HTMLTextAreaElement | null>(null);
  const nextThreadNumber = useRef(2);

  const usableDeployments = useMemo(
    () =>
      (deploymentsQuery.data ?? []).filter(
        (deployment) =>
          deployment.desired_state === "running" &&
          deployment.observed_state === "healthy" &&
          Boolean(deployment.endpoint?.model?.trim()),
      ),
    [deploymentsQuery.data],
  );
  const usableDeploymentIds = useMemo(
    () => new Set(usableDeployments.map((deployment) => deployment.id)),
    [usableDeployments],
  );
  const activeThread = threads.find((thread) => thread.id === activeThreadId) ?? threads[0];
  const activeDeployment = usableDeployments.find(
    (deployment) => deployment.id === activeThread?.deploymentId,
  );

  useEffect(() => {
    const fallback = usableDeploymentIds.has(requestedDeployment)
      ? requestedDeployment
      : usableDeployments[0]?.id ?? "";
    setThreads((current) =>
      current.map((thread) =>
        usableDeploymentIds.has(thread.deploymentId)
          ? thread
          : { ...thread, deploymentId: fallback },
      ),
    );
  }, [requestedDeployment, usableDeploymentIds, usableDeployments]);

  useEffect(() => {
    activeThreadIdRef.current = activeThreadId;
  }, [activeThreadId]);

  useEffect(
    () => () => {
      requestRef.current?.controller.abort();
    },
    [],
  );

  const updateThreadDeployment = (deploymentId: string) => {
    setThreads((current) =>
      current.map((thread) =>
        thread.id === activeThreadId ? { ...thread, deploymentId } : thread,
      ),
    );
  };

  const createThread = () => {
    const id = `thread-${nextThreadNumber.current}`;
    nextThreadNumber.current += 1;
    setThreads((current) => [
      ...current,
      {
        id,
        title: "New chat",
        deploymentId: usableDeployments[0]?.id ?? "",
        messages: [],
      },
    ]);
    setActiveThreadId(id);
    setComposer("");
    requestAnimationFrame(() => composerRef.current?.focus());
  };

  const submit = async (threadId: string, newContent?: string) => {
    if (requestRef.current) return;
    const capturedThread = threads.find((thread) => thread.id === threadId);
    if (!capturedThread || !usableDeploymentIds.has(capturedThread.deploymentId)) return;

    const trimmed = newContent?.trim();
    if (newContent !== undefined && !trimmed) return;
    const userMessage: ThreadMessage | null = trimmed
      ? {
          id: crypto.randomUUID(),
          role: "user",
          content: trimmed,
          state: "complete",
        }
      : null;
    const previousMessages = capturedThread.messages.filter(
      (message) => message.state === "complete",
    );
    const requestMessages = userMessage
      ? [...previousMessages, userMessage]
      : previousMessages;
    if (requestMessages.length === 0 || requestMessages.length > 64) return;

    const waitingId = crypto.randomUUID();
    const waitingMessage: ThreadMessage = {
      id: waitingId,
      role: "assistant",
      content: "",
      state: "waiting",
    };
    const controller = new AbortController();
    requestRef.current = { controller, threadId, waitingId };
    setThreads((current) =>
      current.map((thread) =>
        thread.id === threadId
          ? {
              ...thread,
              title:
                thread.title === "New chat" && userMessage
                  ? userMessage.content.slice(0, 48)
                  : thread.title,
              messages: [...requestMessages, waitingMessage],
            }
          : thread,
      ),
    );
    if (newContent !== undefined) setComposer("");

    try {
      const response = await completion.mutateAsync({
        body: {
          deployment_id: capturedThread.deploymentId,
          messages: requestMessages.map((message) => ({
            role: message.role,
            content: message.content,
          })),
        },
        signal: controller.signal,
        threadId,
      });
      setThreads((current) =>
        current.map((thread) =>
          thread.id === threadId
            ? {
                ...thread,
                messages: thread.messages.map((message) =>
                  message.id === waitingId
                    ? {
                        id: waitingId,
                        role: "assistant",
                        content: response.message.content,
                        reasoning: response.message.reasoning_content,
                        usage: response.usage,
                        state: "complete",
                      }
                    : message,
                ),
              }
            : thread,
        ),
      );
      if (activeThreadIdRef.current === threadId) {
        requestAnimationFrame(() => composerRef.current?.focus());
      }
    } catch (error) {
      const aborted = controller.signal.aborted;
      setThreads((current) =>
        current.map((thread) =>
          thread.id === threadId
            ? {
                ...thread,
                messages: aborted
                  ? thread.messages.filter((message) => message.id !== waitingId)
                  : thread.messages.map((message) =>
                      message.id === waitingId
                        ? {
                            id: waitingId,
                            role: "assistant",
                            content:
                              error instanceof Error
                                ? error.message
                                : "The completion request failed.",
                            state: "error",
                          }
                        : message,
                    ),
              }
            : thread,
        ),
      );
    } finally {
      if (requestRef.current?.waitingId === waitingId) requestRef.current = null;
    }
  };

  const cancelRequest = () => {
    requestRef.current?.controller.abort();
  };

  if (deploymentsQuery.isPending) {
    return <LoadingPanel label="loading deployments" />;
  }
  if (deploymentsQuery.isError) {
    return (
      <div className="space-y-3">
        <ErrorState error={deploymentsQuery.error} />
        <Button variant="outline" onClick={() => void deploymentsQuery.refetch()}>
          Retry
        </Button>
      </div>
    );
  }
  if (usableDeployments.length === 0) {
    return (
      <>
        <EmptyState
          title="No chat-ready deployment"
          hint="Chat requires a deployment that is running, healthy, and reports its served model."
          icon={<Server className="h-6 w-6" />}
          action={<Button onClick={() => setPlanOpen(true)}>Launch deployment</Button>}
        />
        <PlanDeploymentDialog open={planOpen} onOpenChange={setPlanOpen} />
      </>
    );
  }

  return (
    <div className="grid min-h-[calc(100vh-8rem)] gap-3 lg:grid-cols-[220px_minmax(0,1fr)]">
      <aside className="lmw-panel flex min-h-0 flex-col" aria-label="Chat history for this session">
        <div className="lmw-panel-head flex items-center justify-between">
          <div>
            <p className="lmw-label">This session</p>
            <p className="font-display text-base font-semibold">Chats</p>
          </div>
          <Button size="icon" variant="outline" onClick={createThread} aria-label="New chat">
            <Plus className="h-4 w-4" />
          </Button>
        </div>
        <div className="flex gap-1 overflow-x-auto p-2 lg:flex-col lg:overflow-y-auto">
          {threads.map((thread) => (
            <button
              type="button"
              key={thread.id}
              onClick={() => setActiveThreadId(thread.id)}
              className={cn(
                "min-w-40 rounded-md border px-3 py-2 text-left text-sm transition-colors lg:min-w-0",
                thread.id === activeThreadId
                  ? "border-primary/50 bg-primary/10 text-foreground"
                  : "border-transparent text-muted hover:border-hairline hover:bg-raised hover:text-foreground",
              )}
              aria-current={thread.id === activeThreadId ? "page" : undefined}
            >
              <span className="block truncate font-medium">{thread.title}</span>
              <span className="mt-0.5 block font-mono text-[10px] text-faint">
                {thread.messages.filter((message) => message.role === "user").length} messages
              </span>
            </button>
          ))}
        </div>
      </aside>

      <section className="lmw-panel flex min-h-0 min-w-0 flex-col" aria-label="Chat conversation">
        <header className="lmw-panel-head flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <p className="lmw-label">Active deployment</p>
            <h1 className="font-display text-xl font-semibold">{activeDeployment?.endpoint?.model}</h1>
          </div>
          <label className="grid gap-1 text-xs text-muted">
            <span className="sr-only">Deployment</span>
            <select
              className="control min-w-64 rounded-md border border-hairline bg-panel px-3 py-2 text-sm text-foreground"
              value={activeThread?.deploymentId ?? ""}
              onChange={(event) => updateThreadDeployment(event.target.value)}
              disabled={Boolean(requestRef.current)}
            >
              {usableDeployments.map((deployment) => (
                <option key={deployment.id} value={deployment.id}>
                  {deploymentLabel(deployment)}
                </option>
              ))}
            </select>
          </label>
        </header>

        <div className="flex min-h-80 flex-1 flex-col gap-4 overflow-y-auto px-4 py-5 sm:px-8" aria-live="polite">
          {activeThread?.messages.length ? (
            activeThread.messages.map((message) => (
              <article
                key={message.id}
                className={cn(
                  "max-w-3xl rounded-lg border px-4 py-3",
                  message.role === "user"
                    ? "ml-auto border-primary/30 bg-primary/5"
                    : "mr-auto border-hairline bg-panel-raised",
                  message.state === "error" && "border-fault/40 bg-fault/5",
                )}
              >
                <p className="lmw-label mb-2">{message.role === "user" ? "You" : "Assistant"}</p>
                {message.state === "waiting" ? (
                  <p className="text-sm text-muted">Waiting for the deployment…</p>
                ) : message.state === "error" ? (
                  <div className="space-y-2" role="alert">
                    <p className="text-sm text-fault">{message.content}</p>
                    <Button size="sm" variant="outline" onClick={() => void submit(activeThread.id)}>
                      <RotateCcw className="h-3.5 w-3.5" />
                      Retry
                    </Button>
                  </div>
                ) : (
                  <div className="space-y-3">
                    <p className="whitespace-pre-wrap text-sm leading-6">
                      {message.content || "The deployment returned an empty response."}
                    </p>
                    {message.reasoning ? (
                      <details className="rounded border border-hairline bg-raised px-3 py-2 text-xs text-muted">
                        <summary className="cursor-pointer font-medium text-foreground">Reasoning</summary>
                        <p className="mt-2 whitespace-pre-wrap leading-5">{message.reasoning}</p>
                      </details>
                    ) : null}
                    {message.usage ? (
                      <p className="font-mono text-[10px] text-faint">
                        {message.usage.prompt_tokens} prompt · {message.usage.completion_tokens} completion · {message.usage.total_tokens} total tokens
                      </p>
                    ) : null}
                  </div>
                )}
              </article>
            ))
          ) : (
            <div className="m-auto max-w-lg text-center">
              <MessageSquare className="mx-auto mb-3 h-7 w-7 text-muted" aria-hidden />
              <h2 className="font-display text-xl font-semibold">Start a conversation</h2>
              <p className="mt-2 text-sm text-muted">
                Messages are sent to the selected deployment. Chat history stays in this browser session only.
              </p>
            </div>
          )}
        </div>

        <div className="border-t border-hairline bg-panel px-3 py-3 sm:px-6">
          <div className="mx-auto flex max-w-4xl items-end gap-2 rounded-lg border border-hairline bg-background p-2 focus-within:border-primary focus-within:ring-2 focus-within:ring-primary/20">
            <textarea
              ref={composerRef}
              value={composer}
              onChange={(event) => setComposer(event.target.value)}
              onKeyDown={(event) => {
                if (event.key === "Enter" && !event.shiftKey) {
                  event.preventDefault();
                  void submit(activeThread.id, composer);
                }
              }}
              rows={2}
              maxLength={65536}
              disabled={Boolean(requestRef.current)}
              placeholder={`Message ${activeDeployment?.endpoint?.model ?? "deployment"}`}
              aria-label="Chat message"
              className="min-h-12 flex-1 resize-none bg-transparent px-2 py-1.5 text-sm outline-none placeholder:text-faint disabled:cursor-not-allowed"
            />
            {requestRef.current ? (
              <Button type="button" variant="destructive" size="sm" onClick={cancelRequest}>
                <Square className="h-3.5 w-3.5" />
                Cancel
              </Button>
            ) : (
              <Button
                type="button"
                size="sm"
                onClick={() => void submit(activeThread.id, composer)}
                disabled={!composer.trim()}
              >
                <Send className="h-3.5 w-3.5" />
                Send
              </Button>
            )}
          </div>
          <p className="mx-auto mt-1.5 max-w-4xl font-mono text-[10px] text-faint">
            Enter to send · Shift+Enter for a new line · responses are not persisted
          </p>
        </div>
      </section>
    </div>
  );
}

function deploymentLabel(deployment: Deployment): string {
  const model = deployment.endpoint?.model ?? "Not reported";
  const recipe = deployment.recipe_name ?? deployment.recipe_digest;
  const engine =
    deployment.engine === "vllm"
      ? "vLLM"
      : deployment.engine === "sglang"
        ? "SGLang"
        : deployment.engine || "Not reported";
  const placements = (deployment.placements ?? [])
    .map((placement) => placement.node_name ?? placement.node_id)
    .join(", ");
  return [model, recipe, engine, placements].filter(Boolean).join(" · ");
}
