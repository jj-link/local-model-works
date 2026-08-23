(function () {
  var shell = document.querySelector(".lmw-shell");
  if (!shell) return;

  var pages = {
    workshop: {
      eyebrow: "Operator overview / live state",
      title: "Workshop",
      deck: "A single operational readout for what is running, what is ready, and where attention is needed across the local model fleet.",
      stats: [["3", "active runs"], ["4", "online nodes"], ["2", "deployments"], ["96%", "fleet ready"]],
      panel: "Recent activity",
      columns: ["Work", "Target", "State", "Updated"],
      rows: [
        ["Qwen3 32B evaluation", "spark2 + spark3", "running", "12 sec"],
        ["Llama 3.3 deployment", "RTX workstation", "healthy", "4 min"],
        ["Recipe compatibility scan", "spark1", "complete", "18 min"]
      ],
      cards: [["Next action", "Review the Qwen3 benchmark once the distributed run completes."], ["Fleet note", "All four registered devices are reachable; the Spark fabric reports RDMA ready."]]
    },
    chat: {
      eyebrow: "Conversation / operator assistant",
      title: "Chat",
      deck: "A conversational workspace for the local model stack: ask questions, request runs, and coordinate recipes, experiments, and agents in natural language.",
      stats: [["3", "active threads"], ["12", "messages / session"], ["2", "agents online"], ["<1s", "median reply"]],
      panel: "Recent conversations",
      columns: ["Thread", "Topic", "Last activity", "State"],
      rows: [
        ["qwen3-32b-debug", "Debug AWQ serving on spark-cluster", "2 min", "open"],
        ["bench-behavior", "Compare TTFT across engines", "yesterday", "open"],
        ["retention-policy", "Propose a checkpoint retention policy", "3 days", "archived"]
      ],
      cards: [["Context", "Chat can reference recipes, projects, and running research agents."], ["Guardrails", "Actions still require approval; chat never places work on nodes by itself."]]
    },
    fleet: {
      eyebrow: "Infrastructure / registered hardware",
      title: "Fleet",
      deck: "Individual devices stay visible even when they participate in a fabric. Capacity, health, and connectivity remain scannable at one level.",
      stats: [["4", "registered devices"], ["4", "online"], ["384 GB", "aggregate memory"], ["2", "fabrics"]],
      panel: "Nodes",
      columns: ["Device", "Hardware", "Status", "Fabric"],
      rows: [
        ["RTX workstation", "RTX 5090 · 32 GB", "online", "local"],
        ["spark1", "GB10 · 128 GB", "online", "standalone"],
        ["spark2", "GB10 · 128 GB", "online", "spark-cluster"],
        ["spark3", "GB10 · 128 GB", "online", "spark-cluster"]
      ],
      cards: [["Fabric", "spark2 and spark3 are linked as an RDMA-capable two-node fabric."], ["Enrollment", "No pending enrollment requests."]]
    },
    serving: {
      eyebrow: "Runtime / model endpoints",
      title: "Serving",
      deck: "Deployment health, engine, placement, and endpoint state are presented as an operational register rather than a setup wizard.",
      stats: [["2", "deployments"], ["2", "healthy"], ["0", "degraded"], ["44.8", "tokens / sec"]],
      panel: "Deployments",
      columns: ["Deployment", "Placement", "Engine", "State"],
      rows: [
        ["llama-3.3-70b", "spark2 + spark3", "vLLM", "healthy"],
        ["qwen3-8b-dev", "RTX workstation", "SGLang", "healthy"]
      ],
      cards: [["Endpoint", "OpenAI-compatible APIs are available on ports 8888 and 8000."], ["Capacity", "The workstation has room for one additional small-model deployment."]]
    },
    benchmarks: {
      eyebrow: "Measurement / comparable evidence",
      title: "Benchmarks",
      deck: "Benchmark results keep recipe, device, engine, and run conditions together so throughput numbers remain attributable.",
      stats: [["18", "completed runs"], ["3", "suites"], ["51.2", "best tok / sec"], ["0", "queued"]],
      panel: "Latest results",
      columns: ["Suite", "Target", "Result", "Recorded"],
      rows: [
        ["Serving throughput", "Qwen3 32B · 2 nodes", "51.2 tok/s", "today"],
        ["Time to first token", "Llama 3.3 · 2 nodes", "286 ms", "yesterday"],
        ["Prompt processing", "Qwen3 8B · RTX", "1,104 tok/s", "Mar 18"]
      ],
      cards: [["Comparable", "Results are grouped only when model, quantization, engine, and concurrency match."], ["Export", "Benchmark evidence can be downloaded with its full run manifest."]]
    },
    autoresearch: {
      eyebrow: "Research / autonomous literature + data sweeps",
      title: "Autoresearch",
      deck: "An autonomous loop that proposes, runs, and evaluates candidate experiments, capturing evidence and tracing every claim back to source and artifact.",
      stats: [["5", "active agents"], ["3", "queued questions"], ["214", "sources indexed"], ["12", "findings flagged"]],
      panel: "Active research agents",
      columns: ["Agent", "Question", "Progress", "State"],
      rows: [
        ["qwen-distill-review", "Does AWQ hurt reasoning fidelity on 32B?", "68%", "running"],
        ["fabric-sched-search", "What batch size saturates the RDMA fabric?", "100%", "complete"],
        ["ckpt-policy-scan", "Which checkpoints are safe to prune?", "41%", "running"]
      ],
      cards: [["Attribution", "Every finding carries a source link, excerpt, and the run that validated it."], ["Guardrails", "Agents request approval before placing anything on active hardware."]]
    },
    "recipe-builder": {
      eyebrow: "Authoring / reusable workloads",
      title: "Recipe Builder",
      deck: "A structured editor for turning a model, runtime, and harness into a reusable recipe with explicit compatibility and trust requirements.",
      stats: [["2", "drafts"], ["6", "published"], ["0", "errors"], ["schema 3", "manifest format"]],
      panel: "Recipe drafts",
      columns: ["Recipe", "Base model", "Validation", "Updated"],
      rows: [
        ["qwen3-32b-sglang", "Qwen3 32B", "valid", "8 min"],
        ["llama-vision-worker", "Llama 3.2 Vision", "incomplete", "yesterday"],
        ["mistral-24b-awq", "Mistral 24B", "valid", "3 days"]
      ],
      cards: [["Reuse", "Published recipes are versioned and can be imported by any project."], ["Trust", "Compatibility and provenance checks run before a recipe can be published."]]
    },
    "experiment-builder": {
      eyebrow: "Authoring / structured runs",
      title: "Experiment Builder",
      deck: "Compose experiments from recipes, parameters, and hardware into reproducible runs with declared success criteria.",
      stats: [["4", "drafts"], ["18", "executed"], ["2", "running"], ["1.0", "pass rate"]],
      panel: "Experiment drafts",
      columns: ["Experiment", "Recipe", "Hardware", "State"],
      rows: [
        ["qwen3-awq-ablation", "qwen3-32b-sglang", "spark2 + spark3", "ready"],
        ["ttft-vs-batch", "llama-3.3-70b", "workstation", "running"],
        ["fp8-vs-awq-8b", "qwen3-8b-dev", "spark1", "ready"]
      ],
      cards: [["Reproducibility", "Pin model, recipe, hyperparameters, seed, and device set before launch."], ["Criteria", "Every experiment declares the metric and threshold that mark success."]]
    },
    "cron-jobs": {
      eyebrow: "Automation / scheduled orchestration",
      title: "Cron Jobs",
      deck: "Scheduled tasks that run recipes, benchmarks, transfers, and research sweeps on a cadence, with failure handling baked in.",
      stats: [["7", "schedules"], ["5", "active"], ["19", "runs / week"], ["0", "overdue"]],
      panel: "Schedules",
      columns: ["Job", "Cadence", "Last run", "State"],
      rows: [
        ["nightly-index-refresh", "every 6 hours", "2h ago", "active"],
        ["weekly-benchmark-suite", "every Monday 03:00", "2 days", "active"],
        ["artifact-retention-sweep", "every Sunday 04:00", "5 days", "paused"]
      ],
      cards: [["Failure policy", "Missed runs retry with backoff and notify the owning project."], ["Idempotency", "Each schedule records its last run so nothing executes twice."]]
    },
    "projects": {
      eyebrow: "Organization / grouped work",
      title: "Projects",
      deck: "Projects group recipes, experiments, research, and cron schedules around a shared goal, keeping ownership and scope explicit.",
      stats: [["3", "projects"], ["12", "experiments"], ["9", "schedules"], ["25 GB", "shared store"]],
      panel: "Projects",
      columns: ["Project", "Scope", "Experiments", "State"],
      rows: [
        ["model-works-ops", "serving + benchmarks", "6", "active"],
        ["autoresearch-eval", "automated distills", "4", "active"],
        ["legacy-migration", "ckpt compatibility", "2", "archived"],
      ],
      cards: [["Isolation", "Projects keep their hardware placement and storage separate."], ["Sharing", "A project can be handed to an Autoresearch agent as its working boundary."]]
    },
    "chat-a": {
      eyebrow: "Conversation / operator assistant",
      title: "Chat",
      deck: "A conversational workspace for the local model stack: ask questions, request runs, and coordinate recipes, experiments, and agents in natural language.",
      stats: [["3", "active threads"], ["12", "messages / session"], ["2", "agents online"], ["<1s", "median reply"]],
      panel: "Recent conversations",
      columns: ["Thread", "Topic", "Last activity", "State"],
      rows: [
        ["qwen3-32b-debug", "Debug AWQ serving on spark-cluster", "2 min", "open"],
        ["bench-behavior", "Compare TTFT across engines", "yesterday", "open"],
        ["retention-policy", "Propose a checkpoint retention policy", "3 days", "archived"],
      ],
      cards: [["Context", "Chat can reference recipes, projects, and running research agents."], ["Collaboration · planned", "Shared threads, live responses, annotations, branching, and team handoff will live inside Chat."]]
    },
    "profiles-sharing": {
      eyebrow: "Recipes / identity + exchange",
      title: "Profiles & Sharing",
      deck: "Publish model configurations, prompts, skills, and complete recipe setups from a trusted profile, then follow authors or import their work.",
      stats: [["8", "published setups"], ["14", "followers"], ["31", "imports"], ["100%", "attributed"]],
      panel: "Shared setups",
      columns: ["Setup", "Author", "Trust", "Updated"],
      rows: [
        ["qwen3-reasoning-local", "Joseph", "verified", "today"],
        ["vision-review-agent", "Mira Chen", "community", "yesterday"],
        ["spark-cluster-serve", "Local Ops", "verified", "Mar 19"],
      ],
      cards: [["Attribution", "Imported setups retain their author, version, license, and source recipe."], ["Trust", "Profiles separate verified local operators from unverified community publishers."]]
    },
    "knowledge-rag": {
      eyebrow: "Recipes / retrieval components",
      title: "Knowledge & RAG",
      deck: "Configure sources, chunking, embeddings, retrieval, and reranking as reusable recipe components; Workflow Builder consumes these pipelines as typed knowledge nodes.",
      stats: [["4", "knowledge bases"], ["12", "sources"], ["3", "pipelines"], ["98.4%", "index ready"]],
      panel: "Retrieval pipelines",
      columns: ["Pipeline", "Embedding", "Retriever", "State"],
      rows: [
        ["ops-runbooks", "nomic-embed-text", "hybrid + rerank", "ready"],
        ["research-library", "bge-m3", "semantic", "indexing"],
        ["model-docs", "nomic-embed-text", "hybrid", "ready"],
      ],
      cards: [["Modular by design", "Each source, transform, index, retriever, and reranker remains replaceable and versioned."], ["Workflow bridge", "Published retrieval pipelines appear as typed nodes in Workflow Builder."]]
    },
    "community-leaderboard": {
      eyebrow: "Benchmarks / community evidence",
      title: "Community Leaderboard",
      deck: "Compare models by task using opt-in real-world usage, structured feedback, and attributable benchmark evidence rather than a single synthetic score.",
      stats: [["24", "ranked models"], ["6", "task groups"], ["412", "ratings"], ["87", "verified runs"]],
      panel: "Top community results",
      columns: ["Model", "Task", "Rating", "Evidence"],
      rows: [
        ["Qwen3 32B", "reasoning", "4.8 / 5", "28 runs"],
        ["Llama 3.3 70B", "tool use", "4.7 / 5", "21 runs"],
        ["Mistral Small 3.1", "coding", "4.6 / 5", "17 runs"],
      ],
      cards: [["Methodology", "Rankings expose sample size, hardware, recipe version, and confidence beside every score."], ["Privacy", "Only explicitly shared, anonymized usage and feedback contribute to community rankings."]]
    },
    "workflow-builder": {
      eyebrow: "Automation / visual orchestration",
      title: "Workflow Builder",
      deck: "Compose models, tools, knowledge, rules, approvals, and outputs into inspectable multi-step pipelines.",
      stats: [["5", "workflows"], ["18", "nodes"], ["3", "published"], ["0", "invalid"]],
      panel: "Workflow drafts",
      columns: ["Workflow", "Trigger", "Nodes", "State"],
      rows: [
        ["research-digest", "schedule", "8", "ready"],
        ["incident-triage", "Chat", "6", "draft"],
        ["benchmark-review", "run complete", "4", "ready"],
      ],
      cards: [["Knowledge nodes", "Knowledge & RAG pipelines plug into workflows as typed retrieval nodes."], ["Execution", "Run workflows manually, from Chat, through a webhook, or on a schedule."]]
    },
    "scheduled-automations": {
      eyebrow: "Automation / scheduled orchestration",
      title: "Scheduled Tasks & Automations",
      deck: "Schedule reports, data pulls, monitoring, recipes, and complete workflows on a cadence with explicit ownership and failure handling.",
      stats: [["7", "schedules"], ["5", "active"], ["19", "runs / week"], ["0", "overdue"]],
      panel: "Automations",
      columns: ["Task", "Cadence", "Last run", "State"],
      rows: [
        ["nightly-index-refresh", "every 6 hours", "2h ago", "active"],
        ["weekly-benchmark-suite", "Monday 03:00", "2 days", "active"],
        ["research-digest", "weekday 08:00", "today", "active"],
      ],
      cards: [["Failure policy", "Missed runs retry with backoff and notify the owning project."], ["Workflow aware", "Any published workflow can be attached to a schedule without duplicating its steps."]]
    },
    "usage-costs": {
      eyebrow: "Governance / consumption + budgets",
      title: "Usage & Costs",
      deck: "Track token usage by user and model, compare local operating cost, and enforce project budgets before workloads consume scarce capacity.",
      stats: [["8.4M", "tokens / month"], ["$42", "estimated cost"], ["63%", "budget used"], ["3", "active users"]],
      panel: "Usage by model",
      columns: ["Model", "Tokens", "Estimated cost", "Budget"],
      rows: [
        ["Qwen3 32B", "4.8M", "$24.10", "within"],
        ["Llama 3.3 70B", "2.6M", "$14.80", "within"],
        ["Qwen3 8B", "1.0M", "$3.10", "within"],
      ],
      cards: [["Budgets", "Set monthly limits by user, project, or model and warn before scheduled work exceeds them."], ["Local economics", "Cost estimates combine measured power, run duration, and optional hosted-model pricing."]]
    },
    "fine-tuning": {
      eyebrow: "Models / feedback-driven training",
      title: "Integrated Fine-tuning",
      deck: "Turn opted-in conversations and ratings into an inspectable dataset, then package it for local fine-tuning without hiding the training artifacts.",
      stats: [["1,284", "accepted examples"], ["3", "datasets"], ["2", "training recipes"], ["0", "policy flags"]],
      panel: "Training datasets",
      columns: ["Dataset", "Source", "Examples", "State"],
      rows: [
        ["support-preferences-v3", "Chat ratings", "812", "ready"],
        ["tool-correction-set", "edited responses", "391", "review"],
        ["reasoning-style", "curated imports", "81", "ready"],
      ],
      cards: [["Consent", "Only conversations explicitly opted into training can enter a dataset."], ["Export first", "Datasets, adapters, hyperparameters, and evaluations remain downloadable and reproducible."]]
    },
  };
  function escapeHtml(value) {
    return String(value).replace(/[&<>\"]/g, function (char) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", "\"": "&quot;" }[char];
    });
  }

  function renderChatA(target) {
    var MODELS = {
      "Qwen3 32B": "spark2 + spark3 · balanced",
      "Qwen3 8B": "RTX workstation · fast",
      "Llama 3.3 70B": "spark-cluster · high value"
    };
    target.innerHTML =
      '<section class="lmw-chat" aria-label="Chat workspace">' +
        '<aside class="lmw-chat-history" aria-label="Conversation history">' +
          '<button type="button" class="lmw-chat-new" data-chat-new><span aria-hidden="true">＋</span><strong>New chat</strong><kbd>Ctrl K</kbd></button>' +
          '<button type="button" class="lmw-chat-search"><span aria-hidden="true">⌕</span><span>Search chats</span></button>' +
          '<div class="lmw-chat-history-scroll">' +
            '<p class="lmw-chat-history-label">Today</p>' +
            '<button type="button" class="lmw-chat-history-item is-current"><strong>Spark model for research digests</strong><small>Qwen3 32B · 4 min</small></button>' +
            '<button type="button" class="lmw-chat-history-item"><strong>AWQ serving failure</strong><small>Qwen3 32B · 2 hours</small></button>' +
            '<p class="lmw-chat-history-label">Previous 7 days</p>' +
            '<button type="button" class="lmw-chat-history-item"><strong>Benchmark TTFT differences</strong><small>Llama 3.3 · yesterday</small></button>' +
            '<button type="button" class="lmw-chat-history-item"><strong>Checkpoint retention policy</strong><small>Qwen3 8B · Monday</small></button>' +
            '<button type="button" class="lmw-chat-history-item"><strong>Build an ops RAG pipeline</strong><small>Qwen3 32B · Friday</small></button>' +
          '</div>' +
          '<div class="lmw-chat-history-foot"><span class="lmw-chat-avatar">J</span><div><strong>Private workspace</strong><small>History stays on this machine</small></div></div>' +
        '</aside>' +
        '<div class="lmw-chat-stage">' +
          '<header class="lmw-chat-bar">' +
            '<div class="lmw-chat-model-wrap">' +
              '<button type="button" class="lmw-chat-model" data-chat-model aria-expanded="false"><span class="lmw-chat-model-dot"></span><span><strong data-chat-model-name>Qwen3 32B</strong><small data-chat-model-detail>spark2 + spark3 · balanced</small></span><span class="lmw-chat-chevron" aria-hidden="true">⌄</span></button>' +
              '<div class="lmw-chat-model-menu" data-chat-model-menu hidden>' +
                '<button type="button" data-model-name="Qwen3 32B"><strong>Qwen3 32B</strong><small>spark2 + spark3 · 51.2 tok/s</small></button>' +
                '<button type="button" data-model-name="Qwen3 8B"><strong>Qwen3 8B</strong><small>RTX workstation · 44.8 tok/s</small></button>' +
                '<button type="button" data-model-name="Llama 3.3 70B"><strong>Llama 3.3 70B</strong><small>spark-cluster · 37.4 tok/s</small></button>' +
              '</div>' +
            '</div>' +
            '<div class="lmw-chat-bar-actions"><div class="lmw-chat-presence" title="Joseph, Mira, and two collaborators"><span>J</span><span>M</span><span>+2</span></div><button type="button" class="lmw-chat-share" data-chat-share>Share</button><button type="button" class="lmw-chat-icon-button" data-chat-new aria-label="Start a new chat">＋</button></div>' +
          '</header>' +
          '<div class="lmw-chat-scroll" data-chat-scroll>' +
            '<div class="lmw-chat-thread" data-chat-thread>' +
              '<p class="lmw-chat-date">Today, 10:42</p>' +
              '<article class="lmw-chat-message is-user"><div class="lmw-chat-message-body"><p>Compare Qwen3 32B on the Spark pair with the local 8B model. Which should handle our daily research digests?</p></div></article>' +
              '<article class="lmw-chat-message is-assistant">' +
                '<div class="lmw-chat-assistant-mark" aria-hidden="true">LM</div>' +
                '<div class="lmw-chat-message-body">' +
                  '<div class="lmw-chat-message-meta"><strong>Qwen3 32B</strong><span>spark2 + spark3</span></div>' +
                  '<p>Use <strong>Qwen3 32B on the Spark pair</strong> for the final digest. Keep the local 8B model for extraction, tagging, and short summaries.</p>' +
                  '<div class="lmw-chat-comparison">' +
                    '<div><span>Qwen3 32B</span><strong>Best final synthesis</strong><small>Longer context, stronger source reconciliation</small></div>' +
                    '<div><span>Qwen3 8B</span><strong>Best utility worker</strong><small>Lower latency, keeps the cluster free</small></div>' +
                  '</div>' +
                  '<p>A practical workflow: run source extraction locally, route only the resulting evidence bundle through <button type="button" class="lmw-chat-inline-link">research-digest</button>, and let the 32B model produce the cited brief.</p>' +
                  '<div class="lmw-chat-citations"><button type="button">▤ Serving benchmark · run 018</button><button type="button">⌁ ops-runbooks knowledge</button></div>' +
                  '<div class="lmw-chat-response-actions"><button type="button" aria-label="Copy response">□</button><button type="button" aria-label="Good response">↑</button><button type="button" aria-label="Bad response">↓</button><button type="button" aria-label="Regenerate response">↻</button><span>322 tokens · 51.2 tok/s</span></div>' +
                '</div>' +
              '</article>' +
            '</div>' +
          '</div>' +
          '<footer class="lmw-chat-dock">' +
            '<div class="lmw-chat-collab"><div class="lmw-chat-presence"><span>J</span><span>M</span><span>+2</span></div><p><strong>Shared thread · planned</strong><span>Live responses, annotations, branches, and handoff will live here.</span></p></div>' +
            '<form class="lmw-chat-composer" data-chat-form>' +
              '<label class="sr-only" for="lmw-chat-input">Message Qwen3 32B</label>' +
              '<textarea id="lmw-chat-input" data-chat-input rows="1" placeholder="Message Qwen3 32B"></textarea>' +
              '<div class="lmw-chat-composer-row">' +
                '<div><button type="button" class="lmw-chat-tool" aria-label="Attach a file">＋</button><button type="button" class="lmw-chat-pill">⌁ ops-runbooks</button><button type="button" class="lmw-chat-pill">Tools</button></div>' +
                '<button type="submit" class="lmw-chat-send" data-chat-send aria-label="Send message" disabled>↑</button>' +
              '</div>' +
            '</form>' +
            '<p class="lmw-chat-disclaimer">Local models can make mistakes. Verify important outputs and linked evidence.</p>' +
          '</footer>' +
        '</div>' +
      '</section>';

    var input = target.querySelector("[data-chat-input]");
    var form = target.querySelector("[data-chat-form]");
    var send = target.querySelector("[data-chat-send]");
    var thread = target.querySelector("[data-chat-thread]");
    var scroll = target.querySelector("[data-chat-scroll]");
    var modelButton = target.querySelector("[data-chat-model]");
    var modelMenu = target.querySelector("[data-chat-model-menu]");

    input.addEventListener("input", function () {
      send.disabled = !input.value.trim();
      input.style.height = "auto";
      input.style.height = Math.min(input.scrollHeight, 140) + "px";
    });

    form.addEventListener("submit", function (event) {
      event.preventDefault();
      var message = input.value.trim();
      if (!message) return;
      thread.insertAdjacentHTML("beforeend",
        '<article class="lmw-chat-message is-user"><div class="lmw-chat-message-body"><p>' + escapeHtml(message) + "</p></div></article>" +
        '<article class="lmw-chat-message is-assistant"><div class="lmw-chat-assistant-mark" aria-hidden="true">LM</div><div class="lmw-chat-message-body"><div class="lmw-chat-message-meta"><strong>' +
        escapeHtml(target.querySelector("[data-chat-model-name]").textContent) +
        '</strong><span>local response</span></div><p>I can help with that using the active model, linked recipes, and approved knowledge sources. This prototype keeps execution behind an explicit operator action.</p><div class="lmw-chat-response-actions"><button type="button" aria-label="Copy response">□</button><button type="button" aria-label="Good response">↑</button><button type="button" aria-label="Bad response">↓</button><span>ready</span></div></div></article>');
      input.value = "";
      input.style.height = "auto";
      send.disabled = true;
      scroll.scrollTop = scroll.scrollHeight;
    });

    modelButton.addEventListener("click", function () {
      var open = modelMenu.hidden;
      modelMenu.hidden = !open;
      modelButton.setAttribute("aria-expanded", open ? "true" : "false");
    });

    modelMenu.querySelectorAll("[data-model-name]").forEach(function (option) {
      option.addEventListener("click", function () {
        var name = option.dataset.modelName;
        target.querySelector("[data-chat-model-name]").textContent = name;
        target.querySelector("[data-chat-model-detail]").textContent = MODELS[name] || "";
        input.placeholder = "Message " + name;
        modelMenu.hidden = true;
        modelButton.setAttribute("aria-expanded", "false");
      });
    });

    target.querySelectorAll("[data-chat-new]").forEach(function (button) {
      button.addEventListener("click", function () {
        thread.innerHTML = '<div class="lmw-chat-empty"><span class="lmw-chat-empty-mark">LM</span><h1>How can I help?</h1><p>Ask about your models, recipes, knowledge bases, or running workflows.</p><div><button type="button">Compare two models</button><button type="button">Build a RAG pipeline</button><button type="button">Review fleet health</button></div></div>';
        input.focus();
      });
    });

    var shareButton = target.querySelector("[data-chat-share]");
    var chatUrl = location.href.split("#")[0];
    shareButton.addEventListener("click", function () {
      function copied() { shareButton.textContent = "Link copied"; }
      function fallback() {
        try {
          var ta = document.createElement("textarea");
          ta.value = chatUrl;
          ta.style.position = "fixed";
          ta.style.opacity = "0";
          document.body.appendChild(ta);
          ta.select();
          document.execCommand("copy");
          document.body.removeChild(ta);
        } catch (e) {}
        copied();
      }
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(chatUrl).then(copied, fallback);
      } else {
        fallback();
      }
    });
  }

  var isCatalog = document.body && document.body.classList.contains("catalog");
  var sampleAOverrides = isCatalog ? {
    fleet: {
      deck: "Individual devices stay visible even when they participate in a fabric. Each row maps a device to the recipe it is running and the model it serves.",
      stats: [["4", "registered devices"], ["3", "recipes assigned"], ["384 GB", "aggregate memory"], ["2", "fabrics"]],
      panel: "Device workloads",
      columns: ["Device", "Model", "Recipe", "Engine", "Workload"],
      rows: [
        ["RTX workstation", "—", "lmw-demo-serve", "SGLang", "serving"],
        ["spark1", "—", "lmw-demo-profiles", "—", "idle"],
        ["spark2", "DeepSeek V4 Flash", "deepseek-v4-flash", "vLLM", "serving"],
        ["spark3", "DeepSeek V4 Flash", "deepseek-v4-flash", "vLLM", "serving"]
      ],
      cards: [["Fabric", "spark2 and spark3 are linked as an RDMA-capable two-node fabric."], ["Enrollment", "No pending enrollment requests."], ["Illustrative", "Recipes record compatibility, not a running engine or device placement; the engine and workload state here are example assignments."]]
    },
    serving: {
      rows: [
        ["deepseek-v4-flash", "spark2 + spark3", "vLLM", "healthy"],
        ["lmw-demo-serve", "RTX workstation", "SGLang", "healthy"]
      ]
    }
  } : {};

  function renderPreview(key, target) {
    var page = pages[key];
    if (!page || !target) return;
    if (sampleAOverrides[key]) {
      var base = page, merged = {};
      for (var mk in base) merged[mk] = base[mk];
      for (var mk2 in sampleAOverrides[key]) merged[mk2] = sampleAOverrides[key][mk2];
      page = merged;
    }
    if (key === "chat-a") {
      renderChatA(target);
      return;
    }
    var stats = page.stats.map(function (item) {
      return '<div class="lmw-stat"><b>' + escapeHtml(item[0]) + '</b><span>' + escapeHtml(item[1]) + '</span></div>';
    }).join("");
    var head = page.columns.map(function (column) { return "<span>" + escapeHtml(column) + "</span>"; }).join("");
    var rows = page.rows.map(function (row) {
      return '<div class="lmw-data-row">' + row.map(function (cell, index) {
        var status = /online|healthy|complete|enabled|active|verified|valid|enforced/.test(String(cell).toLowerCase()) ? " is-ok" :
          /failed|blocked|error|degraded/.test(String(cell).toLowerCase()) ? " is-warn" : "";
        return '<span class="' + status + '">' + (index === 0 ? "<strong>" : "") + escapeHtml(cell) + (index === 0 ? "</strong>" : "") + "</span>";
      }).join("") + "</div>";
    }).join("");
    var cards = page.cards.map(function (card) {
      return '<article class="lmw-mini-card"><span>Operational note</span><h3>' + escapeHtml(card[0]) + '</h3><p>' + escapeHtml(card[1]) + "</p></article>";
    }).join("");
    var wide = page.columns.length > 4 ? " lmw-grid-wide" : "";
    target.innerHTML =
      '<div class="lmw-preview">' +
        '<header class="lmw-preview-head"><div><p class="lmw-preview-eyebrow">' + escapeHtml(page.eyebrow) + '</p><h1>' + escapeHtml(page.title) + '</h1></div><p class="lmw-preview-deck">' + escapeHtml(page.deck) + "</p></header>" +
        '<div class="lmw-stat-strip">' + stats + "</div>" +
        '<div class="lmw-preview-grid"><section class="lmw-preview-panel"><header class="lmw-preview-panel-head"><h2>' + escapeHtml(page.panel) + '</h2><span>live prototype</span></header><div class="lmw-data-wrap' + wide + '"><div class="lmw-data-head">' + head + "</div>" + rows + "</div></section>" +
        '<aside class="lmw-side-stack">' + cards + "</aside></div>" +
        '<p class="lmw-prototype-note">Static representative data · navigation is included to demonstrate this visual system across Local Model Works.</p>' +
      "</div>";
  }

  var catalogPage = shell.querySelector('[data-shell-page="catalog"]');
  var genericPage = shell.querySelector('[data-shell-page="generic"]');
  var genericTarget = shell.querySelector('[data-shell-preview="generic"]');
  var sectionTitle = shell.querySelector("[data-shell-title]");
  var crumb = shell.querySelector("[data-shell-crumb]");

  function closeNav() { shell.classList.remove("is-nav-open"); }
  function setTitle(text) { sectionTitle.textContent = text; }
  function setCrumb(text) { crumb.textContent = text; }

  shell.querySelectorAll("[data-nav-group]").forEach(function (toggle) {
    toggle.addEventListener("click", function () {
      var g = toggle.dataset.navGroup;
      var children = shell.querySelector('[data-nav-children="' + g + '"]');
      var open = toggle.classList.toggle("is-open");
      toggle.setAttribute("aria-expanded", open ? "true" : "false");
      if (children) children.classList.toggle("is-closed", !open);
    });
  });

  shell.querySelectorAll("[data-shell-tab]").forEach(function (button) {
    button.addEventListener("click", function () {
      var key = button.dataset.shellTab;
      if (!pages[key] && key !== "catalog") return;
      shell.querySelectorAll("[data-shell-tab]").forEach(function (item) {
        item.classList.toggle("is-active", item.dataset.shellTab === key);
        if (item.dataset.shellTab === key) item.setAttribute("aria-current", "page"); else item.removeAttribute("aria-current");
      });
      if (key === "catalog") {
        catalogPage.hidden = false;
        genericPage.hidden = true;
        setTitle("Recipes");
        setCrumb("/ Catalog");
      } else {
        catalogPage.hidden = true;
        genericPage.hidden = false;
        setTitle(pages[key].title);
        setCrumb("");
        renderPreview(key, genericTarget);
      }
      closeNav();
      window.scrollTo({ top: 0, behavior: "instant" });
    });
  });

  var menu = shell.querySelector("[data-mobile-menu]");
  if (menu) menu.addEventListener("click", function () { shell.classList.toggle("is-nav-open"); });
  shell.addEventListener("click", function (event) {
    if (shell.classList.contains("is-nav-open") && !event.target.closest(".lmw-rail") && !event.target.closest("[data-mobile-menu]")) closeNav();
  });
})();
