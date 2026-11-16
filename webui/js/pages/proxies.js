import { api, hasSession } from "../api.js";
import { t, onChange } from "../i18n.js";
import { $, confirmDialog, el, emptyRow } from "../ui.js";

const pageSize = 50;
const verdicts = ["available", "unavailable", "limited", "rejected", "inconclusive"];
let saved = [];
let drafts = [];
let list = [];
let filtered = [];
let page = 0;
let selected = new Set();
let removed = new Set();
let counts = {};
let visibleNodes = new Map();
let run = null;
let locked = false;
let generation = 0;

function stateLabel(state) {
  const labels = {
    unchecked: ["未检查", "unchecked"],
    checking: ["检查中…", "checking"],
    available: ["本次可连通", "available"],
    unavailable: ["确认代理故障", "unavailable"],
    limited: ["暂时限流", "limited"],
    rejected: ["上游拒绝", "rejected"],
    inconclusive: ["无法判定", "inconclusive"],
  };
  const [fallback, key] = labels[state];
  return t(fallback, `config.proxy.${key}`);
}

function reasonLabel(reason) {
  const reasons = {
    ok: ["上游已响应", "responded"],
    upstream_responded: ["上游已响应", "responded"],
    authentication_required: ["已到达上游，需要认证", "authenticationRequired"],
    proxy_unreachable: ["无法连接代理", "unreachable"],
    proxy_authentication_required: ["代理需要认证", "authentication"],
    rate_limited: ["上游暂时限流，请稍后重试", "rateLimited"],
    upstream_rejected: ["上游拒绝连接检查，不能确认代理故障", "rejected"],
    proxy_tunnel_rejected: ["代理隧道请求被拒绝，不能确认代理故障", "tunnelRejected"],
    check_timeout: ["检查超时，无法确认代理故障", "timeout"],
    transport_error: ["传输失败，无法确认代理故障", "transport"],
    canceled: ["检查已取消", "canceled"],
    response_too_large: ["上游回复过大", "tooLarge"],
    invalid_response: ["检查回复格式无效", "invalidResponse"],
    runtime_unavailable: ["网关运行时暂不可用，请稍后重试", "runtimeUnavailable"],
    invalid_proxy: ["代理地址无效，请检查配置", "invalidProxy"],
    management_error: ["管理请求失败或暂时限流，请重试；这不代表代理故障", "management"],
  };
  const [fallback, key] = Object.hasOwn(reasons, reason) ? reasons[reason] : reasons.invalid_response;
  return t(fallback, `config.proxy.reason.${key}`);
}

function draftDisplay(value) {
  const pooled = /^pool\+/i.test(value) || /#pool$/i.test(value);
  const clean = value.replace(/^pool\+/i, "").replace(/#pool$/i, "");
  if (clean === "direct") return `direct${pooled ? "#pool" : ""}`;
  try {
    const address = new URL(clean);
    if (!["http:", "https:", "socks5:", "socks5h:"].includes(address.protocol) || !address.host)
      return t("无效代理地址", "config.proxy.invalid");
    const masked = address.username || address.password ? "***@" : "";
    return `${address.protocol}//${masked}${address.host}${pooled ? "#pool" : ""}`;
  } catch {
    return t("无效代理地址", "config.proxy.invalid");
  }
}

function newRow(input, display) {
  return { input, display, state: "unchecked", result: null };
}

function applySearch() {
  const query = $("#proxies-search").value.trim().toLowerCase();
  filtered = query ? list.filter((row) => row.display.toLowerCase().includes(query)) : list;
  page = Math.max(0, Math.min(page, Math.ceil(filtered.length / pageSize) - 1));
}

function rebuild() {
  list = saved.concat(drafts);
  const current = new Set(list);
  selected = new Set([...selected].filter((row) => current.has(row)));
  counts = {};
  for (const row of list) counts[row.state] = (counts[row.state] || 0) + 1;
  applySearch();
  render();
}

function syncDrafts() {
  const previous = new Map(drafts.map((row) => [row.input.value, row]));
  const values = [...new Set($("#proxies-new").value.split(/\r?\n/).map((value) => value.trim()).filter(Boolean))];
  drafts = values.map((value) => previous.get(value) || newRow({ value }, draftDisplay(value)));
}

export function fillProxies(values, replace = false) {
  stopChecks();
  const previous = new Map(saved.map((row) => [row.input.id, row]));
  if (replace) {
    removed.clear();
    selected.clear();
    drafts = [];
    $("#proxies-new").value = "";
  }
  saved = values.filter((item) => !removed.has(item.id)).map((item) => {
    const row = previous.get(item.id) || newRow({ id: item.id }, item.display);
    row.display = item.display;
    return row;
  });
  rebuild();
}

export function proxyInputs() {
  return list.map((row) => ({ ...row.input }));
}

export function lockProxies(value) {
  locked = value;
  if (locked) stopChecks();
  refreshControls();
}

export function resetProxies() {
  generation++;
  stopChecks();
  locked = false;
  saved = [];
  drafts = [];
  removed.clear();
  selected.clear();
  page = 0;
  $("#proxies-search").value = "";
  $("#proxies-new").value = "";
  rebuild();
}

function refreshControls() {
  const busy = locked || !!run;
  const checked = verdicts.reduce((total, state) => total + (counts[state] || 0), 0);
  $("#proxies-summary").textContent = t(
    "共 {0} 条 · 筛选 {1} 条 · 已选 {2} 条 · 已检查 {3} 条 · 确认故障 {4} 条",
    "config.proxy.summary", list.length, filtered.length, selected.size, checked, counts.unavailable || 0,
  );
  $("#proxies-progress").textContent = run
    ? t("本次检查：{0} / {1}；最多并发 4 条", "config.proxy.progress", run.completed, run.targets.length)
    : "";
  $("#proxies-check-filtered").disabled = busy || !filtered.length;
  $("#proxies-check-selected").disabled = busy || !selected.size;
  $("#proxies-select-filtered").disabled = busy || !filtered.length;
  $("#proxies-select-unavailable").disabled = busy || !counts.unavailable;
  $("#proxies-clear-selection").disabled = busy || !selected.size;
  $("#proxies-delete-selected").disabled = busy || !selected.size;
  $("#proxies-stop").classList.toggle("hidden", !run);
  $("#proxies-new").disabled = busy;
  $("#c-proxyfile").disabled = locked;
  $("#proxies-search").disabled = locked;
  $("#proxies-table").setAttribute("aria-busy", run ? "true" : "false");
  const pages = Math.max(1, Math.ceil(filtered.length / pageSize));
  $("#proxies-page").textContent = t("第 {0} / {1} 页 · 每页最多 50 条", "config.proxy.page", page + 1, pages);
  $("#proxies-prev").disabled = locked || page === 0;
  $("#proxies-next").disabled = locked || page + 1 >= pages;
  const checkedVisible = [...visibleNodes.keys()].filter((row) => selected.has(row)).length;
  const checkbox = $("#proxies-select-page");
  checkbox.checked = visibleNodes.size > 0 && checkedVisible === visibleNodes.size;
  checkbox.indeterminate = checkedVisible > 0 && checkedVisible < visibleNodes.size;
  checkbox.disabled = busy || !visibleNodes.size;
  for (const [row, nodes] of visibleNodes) {
    nodes.checkbox.checked = selected.has(row);
    nodes.checkbox.disabled = busy;
    nodes.check.disabled = busy;
    nodes.remove.disabled = busy;
  }
}

function renderResult(row) {
  const nodes = visibleNodes.get(row);
  if (!nodes) return;
  const tone = row.state === "available" ? "ok" : row.state === "unavailable" ? "bad" : "warn";
  nodes.status.replaceChildren(el("span", { class: row.state === "unchecked" ? "" : tone, text: stateLabel(row.state) }));
  if (row.result) {
    nodes.status.append(el("div", { class: "proxy-reason", text: reasonLabel(row.result.reason) }));
  }
  nodes.response.textContent = row.result?.status ? `HTTP ${row.result.status}` : "—";
  nodes.latency.textContent = row.result?.latency_ms != null ? `${row.result.latency_ms} ms` : "—";
}

function render() {
  const host = $("#proxies-rows");
  host.replaceChildren();
  visibleNodes = new Map();
  const entries = filtered.slice(page * pageSize, (page + 1) * pageSize);
  if (!entries.length)
    emptyRow(host, 6, list.length
      ? t("没有匹配的代理", "config.proxy.noMatch")
      : t("暂无内联代理；可在下方新增。proxyfile 中的条目请在文件中管理。", "config.proxy.empty"));
  for (const row of entries) {
    const checkbox = el("input", { type: "checkbox", "aria-label": t("选择代理", "config.proxy.select") });
    checkbox.addEventListener("change", () => {
      if (checkbox.checked) selected.add(row);
      else selected.delete(row);
      refreshControls();
    });
    const status = el("td");
    const response = el("td", { class: "mono" });
    const latency = el("td", { class: "num" });
    const check = el("button", {
      type: "button", class: "btn btn-sm btn-ghost", text: t("检查", "config.proxy.check"),
      onclick: () => checkRows([row]),
    });
    const remove = el("button", {
      type: "button", class: "btn btn-sm btn-ghost", text: t("移除", "common.remove"),
      onclick: () => removeRows([row]),
    });
    visibleNodes.set(row, { checkbox, status, response, latency, check, remove });
    host.append(el("tr", {}, [
      el("td", {}, [checkbox]), el("td", { class: "mono proxy-address", text: row.display, title: row.display }),
      status, response, latency, el("td", {}, [el("div", { class: "proxy-row-actions" }, [check, remove])]),
    ]));
    renderResult(row);
  }
  refreshControls();
}

async function removeRows(targets) {
  if (locked || run || !targets.length) return;
  const operation = generation;
  const accepted = await confirmDialog({
    title: t("移除代理", "config.proxy.removeTitle"),
    message: t("移除所选 {0} 条代理？保存后生效；存在绑定时可能被配置验证拒绝，不会自动删除绑定。", "config.proxy.removeConfirm", targets.length),
    confirmLabel: t("移除", "common.remove"), danger: true,
  });
  if (!accepted || operation !== generation || !hasSession() || locked || run) return;
  const deleting = new Set(targets);
  for (const row of targets) if (row.input.id) removed.add(row.input.id);
  saved = saved.filter((row) => !deleting.has(row));
  drafts = drafts.filter((row) => !deleting.has(row));
  $("#proxies-new").value = drafts.map((row) => row.input.value).join("\n");
  rebuild();
}

function setResult(row, state, result) {
  counts[row.state] = (counts[row.state] || 0) - 1;
  counts[state] = (counts[state] || 0) + 1;
  row.state = state;
  row.result = result;
  renderResult(row);
}

export function stopChecks() {
  if (!run) return;
  const previous = run;
  run = null;
  previous.controller.abort();
  for (const row of previous.inFlight) setResult(row, "unchecked", null);
  refreshControls();
}

async function checkRows(targets) {
  if (locked || run || !targets.length || !hasSession()) return;
  const active = {
    controller: new AbortController(), targets: targets.slice(), next: 0, completed: 0,
    inFlight: new Set(), generation,
  };
  run = active;
  refreshControls();
  const accepted = await confirmDialog({
    title: t("确认代理连通性检查", "config.proxy.checkTitle"),
    message: t("将检查 {0} 条指定代理到 Zen 的连通性，不发送模型推理、不使用上游 Key，也不验证额度。上游拒绝或限流不能证明代理永久损坏。是否继续？", "config.proxy.checkConfirm", targets.length),
    confirmLabel: t("开始检查", "config.proxy.start"),
  });
  if (run !== active || active.generation !== generation || !hasSession()) return;
  if (!accepted) {
    stopChecks();
    return;
  }
  const worker = async () => {
    while (run === active && active.next < active.targets.length) {
      const row = active.targets[active.next++];
      active.inFlight.add(row);
      setResult(row, "checking", null);
      try {
        const result = await api("/api/proxies/check", {
          method: "POST", body: JSON.stringify(row.input), signal: active.controller.signal,
        });
        if (run !== active || active.generation !== generation || !hasSession()) return;
        if (!result || !verdicts.includes(result.state) || result.ok !== (result.state === "available")) {
          setResult(row, "inconclusive", { reason: "invalid_response" });
        } else {
          setResult(row, result.state, {
            reason: result.reason,
            latency_ms: Number.isFinite(result.latency_ms) && result.latency_ms >= 0 ? Math.round(result.latency_ms) : null,
            status: Number.isInteger(result.status) && result.status >= 100 && result.status <= 599 ? result.status : null,
          });
        }
      } catch {
        if (run !== active || active.generation !== generation || !hasSession()) return;
        setResult(row, "inconclusive", { reason: "management_error" });
      }
      active.inFlight.delete(row);
      active.completed++;
      refreshControls();
    }
  };
  await Promise.all(Array.from({ length: Math.min(4, active.targets.length) }, worker));
  if (run === active) {
    run = null;
    refreshControls();
  }
}

export function initProxies() {
  $("#proxies-new").addEventListener("input", () => {
    if (locked || run) return;
    syncDrafts();
    rebuild();
  });
  $("#proxies-search").addEventListener("input", () => { page = 0; applySearch(); render(); });
  $("#proxies-search").addEventListener("keydown", (event) => { if (event.key === "Enter") event.preventDefault(); });
  $("#proxies-prev").addEventListener("click", () => { page--; render(); });
  $("#proxies-next").addEventListener("click", () => { page++; render(); });
  $("#proxies-select-page").addEventListener("change", (event) => {
    for (const row of visibleNodes.keys()) {
      if (event.target.checked) selected.add(row);
      else selected.delete(row);
    }
    refreshControls();
  });
  $("#proxies-select-filtered").addEventListener("click", () => {
    for (const row of filtered) selected.add(row);
    refreshControls();
  });
  $("#proxies-select-unavailable").addEventListener("click", () => {
    selected = new Set(list.filter((row) => row.state === "unavailable"));
    refreshControls();
  });
  $("#proxies-clear-selection").addEventListener("click", () => { selected.clear(); refreshControls(); });
  $("#proxies-delete-selected").addEventListener("click", () => removeRows([...selected]));
  $("#proxies-check-filtered").addEventListener("click", () => checkRows(filtered));
  $("#proxies-check-selected").addEventListener("click", () => checkRows([...selected]));
  $("#proxies-stop").addEventListener("click", stopChecks);
  onChange(() => {
    for (const row of drafts) row.display = draftDisplay(row.input.value);
    applySearch();
    render();
  });
  rebuild();
}
