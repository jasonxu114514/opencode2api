// Compact proxy management with pinned anonymous free-model checks.
import { api } from "../api.js";
import { t, onChange } from "../i18n.js";
import { $, el, emptyRow, confirmDialog, openDialog } from "../ui.js";

let saved = [];
let drafts = [];
let run = null;

const rows = () => saved.concat(drafts);
const visibleRows = () => rows().filter((row) => {
  const filter = $("#proxies-filter").value;
  const query = $("#proxies-search").value.trim().toLowerCase();
  return (filter === "all" || row.state === filter) &&
    `${row.display} ${row.result?.error || ""} ${row.result?.error_code || ""} ${row.result?.model || ""}`.toLowerCase().includes(query);
});

function applyFilter() {
  const visible = new Set(visibleRows());
  for (const row of rows()) if (row.nodes) row.nodes.tr.hidden = !visible.has(row);
}

function newRow(input, display) {
  return { input, display, selected: false, state: "unchecked", result: null, nodes: null };
}

function draftDisplay(value) {
  const clean = value.replace(/^pool\+/i, "");
  if (clean === "direct") return clean;
  try {
    const url = new URL(clean);
    if (url.username || url.password) {
      url.username = "***";
      url.password = "";
    }
    return url.toString();
  } catch {
    // A malformed draft may still contain credentials; never echo it here.
    return t("无效代理地址", "config.proxy.invalid");
  }
}

function syncDrafts() {
  const previous = new Map(drafts.map((row) => [row.input.value, row]));
  const values = [...new Set($("#proxies-new").value.split(/\r?\n/).map((value) => value.trim()).filter(Boolean))];
  drafts = values.map((value) => previous.get(value) || newRow({ value }, draftDisplay(value)));
}

export function fillProxies(values) {
  stopChecks();
  saved = values.map((item) => newRow({ id: item.id }, item.display));
  syncDrafts();
  render();
}

export function proxyInputs() {
  return rows().map((row) => row.input);
}

export function resetProxies() {
  stopChecks();
  saved = [];
  drafts = [];
  $("#proxies-filter").value = "all";
  $("#proxies-search").value = "";
  $("#proxies-new").value = "";
  render();
}

function refreshControls() {
  const list = rows();
  const selected = list.filter((row) => row.selected).length;
  const visible = visibleRows();
  const available = list.filter((row) => row.state === "available").length;
  const unavailable = list.filter((row) => row.state === "unavailable").length;
  const errors = list.filter((row) => row.state === "error").length;
  const checked = list.filter((row) => ["available", "unavailable", "error"].includes(row.state)).length;
  $("#proxies-summary").textContent = t(
    `共 ${list.length} 条 · 显示 ${visible.length} 条 · 已选 ${selected} 条 · 已检查 ${checked} 条 · 可用 ${available} 条 · 未通过 ${unavailable} 条 · 检查失败 ${errors} 条`,
    "config.proxy.summary", list.length, visible.length, selected, checked, available, unavailable, errors,
  );
  const all = $("#proxies-select-all");
  const selectedVisible = visible.filter((row) => row.selected).length;
  all.checked = visible.length > 0 && selectedVisible === visible.length;
  all.indeterminate = selectedVisible > 0 && selectedVisible < visible.length;
  all.disabled = visible.length === 0;
  $("#proxies-check-all").disabled = !!run || !visible.length;
  $("#proxies-check-selected").disabled = !!run || !selected;
  $("#proxies-delete-selected").disabled = !!run || !selected;
  $("#proxies-select-unavailable").disabled = !list.some((row) => row.state === "unavailable");
  $("#proxies-clear-selection").disabled = !selected;
  $("#proxies-stop").classList.toggle("hidden", !run);
  $("#proxies-new").disabled = !!run;
  $("#proxies-table").setAttribute("aria-busy", run ? "true" : "false");
  applyFilter();
  for (const row of list) {
    if (!row.nodes) continue;
    row.nodes.checkbox.checked = row.selected;
    row.nodes.check.disabled = !!run;
    row.nodes.remove.disabled = !!run;
  }
}

function failureLabel(code) {
  const reasons = {
    free_tier_denied: ["免费层拒绝", "denied"], rate_limited: ["请求限流", "rateLimited"],
    proxy_auth: ["代理认证失败", "proxyAuth"], unauthorized: ["上游认证失败", "unauthorized"],
    timeout: ["请求超时", "timeout"], connection_failed: ["代理连接失败", "connection"],
    model_unavailable: ["模型不可用", "modelUnavailable"], invalid_response: ["回复无效或不完整", "invalidResponse"],
    invalid_request: ["探测请求失败", "invalidRequest"], upstream_error: ["上游错误", "upstreamError"],
  };
  const reason = reasons[code];
  return reason ? t(reason[0], `config.proxy.reason.${reason[1]}`) : t("检查失败", "config.proxy.checkFailed");
}

function showResult(row) {
  const result = row.result;
  if (!result) return;
  const body = el("div", { class: "proxy-result-detail" });
  const fact = (label, value) => body.append(el("p", {}, [el("strong", { text: `${label}: ` }), el("span", { text: value || "—" })]));
  fact(t("代理", "config.proxy.node"), row.display);
  fact(t("测试模型", "config.proxy.model"), result.model);
  if (result.duration_ms != null) fact(t("耗时", "config.proxy.duration"), `${result.duration_ms} ms`);
  if (result.output) fact(t("模型回复", "config.proxy.reply"), result.output);
  if (result.error) fact(t("失败原因", "config.proxy.failureReason"), result.error);
  for (const attempt of result.attempts || []) {
    fact(attempt.upstream, `${attempt.http_status ? `HTTP ${attempt.http_status}` : "—"} · ${attempt.error || result.output || "OK"}`);
  }
  openDialog({ title: t("检查详情", "config.proxy.details"), body,
    actions: [{ label: t("关闭", "common.close") }] });
}

function renderResult(row) {
  const { status, model, duration } = row.nodes;
  const states = {
    unchecked: [t("未检查", "config.proxy.unchecked"), ""],
    checking: [t("检查中…", "config.proxy.checking"), "warn"],
    available: [t("可用", "config.proxy.available"), "ok"],
    unavailable: [t("未通过", "config.proxy.unavailable"), "bad"],
    error: [t("检查失败", "config.proxy.checkFailed"), "warn"],
  };
  const [label, tone] = states[row.state];
  status.replaceChildren(el("span", { class: `status ${tone}`, text: label }));
  if (row.result) {
    const detail = row.result.ok ? row.result.output || "OK" : failureLabel(row.result.error_code);
    status.append(el("button", { type: "button", class: "proxy-check-detail",
      text: `${row.result.http_status && !row.result.ok ? `${row.result.http_status} · ` : ""}${detail}`,
      title: `${row.result.error || row.result.output || detail} — ${t("点击查看详情", "config.proxy.viewDetails")}`,
      onclick: () => showResult(row),
    }));
  }
  model.textContent = row.result?.model || "—";
  model.title = row.result?.model || "";
  duration.textContent = row.result?.duration_ms != null ? `${(row.result.duration_ms / 1000).toFixed(1)} s` : "—";
}

function render() {
  const host = $("#proxies-rows");
  host.replaceChildren();
  const list = rows();
  if (!list.length) emptyRow(host, 6, t("暂无代理，可在下方每行添加一个", "config.proxy.empty"));
  for (const row of list) {
    const checkbox = el("input", {
      type: "checkbox", "aria-label": t("选择代理", "config.proxy.select"),
      onchange: () => { row.selected = checkbox.checked; refreshControls(); },
    });
    const status = el("td");
    const model = el("td", { class: "mono" });
    const duration = el("td");
    const check = el("button", {
      type: "button", class: "btn btn-sm btn-ghost", text: t("检查", "config.proxy.check"),
      onclick: () => checkRows([row]),
    });
    const remove = el("button", {
      type: "button", class: "btn btn-sm btn-ghost", text: t("移除", "common.remove"),
      onclick: () => removeRows([row]),
    });
    const tr = el("tr", {}, [
      el("td", {}, [checkbox]), el("td", { class: "mono", text: row.display, title: row.display }), status, model, duration,
      el("td", {}, [el("div", { class: "actions" }, [check, remove])]),
    ]);
    row.nodes = { tr, checkbox, status, model, duration, check, remove };
    host.append(tr);
    renderResult(row);
  }
  refreshControls();
}

function removeRows(removed) {
  if (run) return;
  const targets = new Set(removed);
  saved = saved.filter((row) => !targets.has(row));
  drafts = drafts.filter((row) => !targets.has(row));
  $("#proxies-new").value = drafts.map((row) => row.input.value).join("\n");
  render();
}

function stopChecks() {
  if (!run) return;
  run.controller.abort();
  run = null;
  for (const row of rows()) {
    if (row.state === "checking") {
      row.state = "unchecked";
      row.result = null;
      if (row.nodes) renderResult(row);
    }
  }
  refreshControls();
}

async function checkRows(targets) {
  if (run || !targets.length) return;
  const active = { controller: new AbortController(), next: 0 };
  run = active;
  refreshControls();
  const worker = async () => {
    while (run === active && active.next < targets.length) {
      const row = targets[active.next++];
      row.state = "checking";
      row.result = null;
      renderResult(row);
      try {
        const result = await api("/api/proxies/check", {
          method: "POST", body: JSON.stringify(row.input), signal: active.controller.signal,
        });
        if (run !== active) return;
        row.result = result;
        row.state = result.ok ? "available" : "unavailable";
      } catch (error) {
        if (run !== active) return;
        // An expired login, stale ID or API failure is not evidence that a
        // proxy is unavailable and must not enter "select unavailable".
        row.state = "error";
        row.result = { error: error.message };
      }
      renderResult(row);
      refreshControls();
    }
  };
  await Promise.all(Array.from({ length: Math.min(4, targets.length) }, worker));
  if (run === active) {
    run = null;
    refreshControls();
  }
}

export function initProxies() {
  $("#proxies-filter").addEventListener("change", refreshControls);
  $("#proxies-search").addEventListener("input", refreshControls);
  $("#proxies-new").addEventListener("input", () => { syncDrafts(); render(); });
  $("#proxies-select-all").addEventListener("change", (event) => {
    visibleRows().forEach((row) => { row.selected = event.target.checked; });
    refreshControls();
  });
  $("#proxies-select-unavailable").addEventListener("click", () => {
    rows().forEach((row) => { row.selected = row.state === "unavailable"; });
    refreshControls();
  });
  $("#proxies-clear-selection").addEventListener("click", () => {
    rows().forEach((row) => { row.selected = false; });
    refreshControls();
  });
  $("#proxies-check-all").addEventListener("click", () => checkRows(visibleRows()));
  $("#proxies-check-selected").addEventListener("click", () => checkRows(rows().filter((row) => row.selected)));
  $("#proxies-stop").addEventListener("click", stopChecks);
  $("#proxies-delete-selected").addEventListener("click", async () => {
    const selected = rows().filter((row) => row.selected);
    if (!selected.length || run) return;
    if (await confirmDialog({
      title: t("删除所选代理", "config.proxy.deleteSelected"),
      message: t(`移除所选的 ${selected.length} 条代理？保存配置后生效。`, "config.proxy.confirmDelete", selected.length),
      confirmLabel: t("删除", "common.delete"), danger: true,
    })) removeRows(selected);
  });
  onChange(() => {
    drafts.forEach((row) => { row.display = draftDisplay(row.input.value); });
    render();
  });
}
