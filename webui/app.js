const $ = (selector) => document.querySelector(selector),
  $$ = (selector) => [...document.querySelectorAll(selector)];
let csrf = "",
  config = null,
  monitorData = null,
  debugData = null,
  lastPlayground = null,
  logEvents = [],
  eventSource = null,
  paused = false,
  monitorTimer = null,
  logRenderPending = false,
  toastTimer = null;
const pages = {
  rotation: ["12", "模型轮转", "共享当前模型、故障切换与尝试记录。"],
  overview: ["01", "运行桌面", "最近一小时、进程累计与当前资源状态。"],
  guide: ["02", "首次运行", "用六个检查点完成从配置到首个请求。"],
  access: ["03", "接入手册", "Chat、Responses、Anthropic 与 SDK 示例。"],
  usage: ["04", "Token 用量", "覆盖率、每分钟趋势、模型排行与 Tier 分布。"],
  playground: ["05", "Playground", "三种入口协议共用真实 Gateway 路由与诊断。"],
  diagnostics: ["06", "路由诊断", "Metadata、模型资格与每一次上游尝试。"],
  config: ["07", "配置中心", "验证后原子保存，并热应用可变字段。"],
  logs: ["08", "事件日志", "来自进程内 Ring Buffer 的结构化 SSE。"],
  account: ["09", "账号安全", "更新后撤销全部管理 Session。"],
  "model-quotas": ["10", "模型与额度", "可用模型、共享套餐剩余额度与重置时间。"],
  "api-keys": ["11", "API Key 管理", "给每个调用方独立的访问密钥。"],
};
async function api(path, options = {}) {
  options.headers = { ...(options.headers || {}), Accept: "application/json" };
  if (options.body) options.headers["Content-Type"] = "application/json";
  if (csrf && !["GET", "HEAD"].includes(options.method || "GET"))
    options.headers["X-CSRF-Token"] = csrf;
  const response = await fetch(path, options);
  if (response.status === 401) {
    showLogin();
    throw new Error("登录已失效");
  }
  if (response.status === 204) return null;
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(data.error?.message || `HTTP ${response.status}`);
  return data;
}
function toast(message, bad = false) {
  const node = $("#toast");
  node.textContent = message;
  node.classList.toggle("bad-toast", bad);
  node.classList.remove("hidden");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => node.classList.add("hidden"), 3600);
}
function showLogin() {
  csrf = "";
  $("#app").classList.add("hidden");
  $("#login").classList.remove("hidden");
  if (eventSource) eventSource.close();
  eventSource = null;
  clearInterval(monitorTimer);
  clearInterval(quotaTimer);
  clearCreatedKey();
  $("#api-key-value").value = "";
}
async function enterConsole(session) {
  csrf = session.csrf_token;
  $("#login").classList.add("hidden");
  $("#app").classList.remove("hidden");
  await loadConfig();
  await Promise.all([refreshMonitor(), loadDebugModels()]);
  clearInterval(monitorTimer);
  monitorTimer = setInterval(refreshMonitor, 3000);
  connectLogs();
  openPage(pages[location.hash.slice(1)] ? location.hash.slice(1) : "overview");
}
async function boot() {
  try {
    await enterConsole(await api("/api/auth/session"));
  } catch (_) {
    showLogin();
  }
}
$("#login-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  $("#login-error").textContent = "";
  try {
    const session = await api("/api/auth/login", {
      method: "POST",
      body: JSON.stringify({ username: $("#login-user").value, password: $("#login-pass").value }),
    });
    $("#login-pass").value = "";
    await enterConsole(session);
  } catch (error) {
    $("#login-error").textContent = error.message;
  }
});
$("#logout").onclick = async () => {
  try {
    await api("/api/auth/logout", { method: "POST", body: "{}" });
  } finally {
    showLogin();
  }
};
function openPage(page) {
  if (!pages[page]) return;
  history.replaceState(null, "", `#${page}`);
  clearInterval(quotaTimer);
  if (page !== "api-keys") clearCreatedKey();
  $$(".nav button").forEach((button) =>
    button.classList.toggle("active", button.dataset.page === page),
  );
  $$(".page").forEach((node) => node.classList.toggle("hidden", node.id !== `page-${page}`));
  const meta = pages[page];
  $("#page-index").textContent = `Section ${meta[0]}`;
  $("#page-title").textContent = meta[1];
  $("#page-sub").textContent = meta[2];
  if (page === "access") renderAccessExamples();
  if (page === "usage") renderUsage();
  if (page === "model-quotas") {
    loadModelQuotas();
    quotaTimer = setInterval(() => {
      if (!document.hidden) loadModelQuotas();
    }, 60000);
  }
  if (page === "rotation") loadRotation();
  if (page === "api-keys") loadAPIKeys();
  if (page === "playground" && !debugData) loadDebugModels();
  if (page === "diagnostics") {
    loadDebugModels();
    renderDiagnostics();
  }
}
$$(".nav button").forEach((button) => (button.onclick = () => openPage(button.dataset.page)));
async function loadConfig() {
  config = await api("/api/config");
  fillConfig(config);
  renderAccessExamples();
}
function fillConfig(value) {
  $("#c-listen").value = value.listen;
  $("#c-prefer").value = value.prefer;
  $("#c-web-listen").value = value.webui.listen;
  $("#c-session").value = value.webui.session_ttl_minutes;
  $("#c-web-enabled").checked = value.webui.enabled;
  $("#c-anonymous").checked = !!value.anonymous;
  $("#c-proxyfile").value = value.proxyfile;
  $("#c-up-zen").value = value.upstream.zen;
  $("#c-up-go").value = value.upstream.go;
  $("#c-attempts").value = value.retry.max_attempts;
  $("#c-timeout").value = value.retry.timeout_seconds;
  $("#c-refresh").value = value.models.refresh_seconds;
  $("#c-protocols").value = JSON.stringify(value.models.protocols || {}, null, 2);
  $("#c-idle").value = value.performance.max_idle_conns;
  $("#c-idle-host").value = value.performance.max_idle_conns_per_host;
  $("#c-max-host").value = value.performance.max_conns_per_host;
  $("#c-idle-timeout").value = value.performance.idle_conn_timeout_seconds;
  $("#c-connect").value = value.performance.connect_timeout_seconds;
  $("#c-cooldown").value = value.performance.failure_cooldown_seconds;
  $("#c-attempt-timeout").value = value.performance.attempt_timeout_seconds;
  $("#c-level").value = value.logging.level;
  $("#c-ring").value = value.logging.ring_size;
  $("#c-dump-bodies").checked = !!value.logging.dump_request_bodies;
  ["server_keys", "zen_keys", "go_keys", "proxies"].forEach(renderChips);
  const notice = $("#restart-notice");
  if (value.restart_required_fields?.length) {
    notice.textContent = `已保存但尚未生效：${value.restart_required_fields.join(", ")}。当前 API ${value.effective.listen}、WebUI ${value.effective.webui_listen}。请重启进程。`;
    notice.classList.remove("hidden");
  } else notice.classList.add("hidden");
}
function renderChips(name) {
  const host = $(`#${name}-chips`);
  host.replaceChildren();
  (config[name] || []).forEach((item) => {
    const chip = document.createElement("span"),
      text = document.createElement("span"),
      remove = document.createElement("button");
    chip.className = "chip";
    chip.dataset.id = item.id;
    text.textContent = item.display;
    remove.type = "button";
    remove.title = "移除";
    remove.textContent = "×";
    remove.onclick = () => chip.remove();
    chip.append(text, remove);
    host.appendChild(chip);
  });
}
function secretInputs(name) {
  const kept = [...$(`#${name}-chips`).children].map((node) => ({ id: node.dataset.id }));
  const added = $(`#${name}-new`)
    .value.split(/\r?\n/)
    .map((value) => value.trim())
    .filter(Boolean)
    .map((value) => ({ value }));
  return kept.concat(added);
}
const number = (id) => Number($(id).value);
$("#config-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    const update = {
      listen: $("#c-listen").value,
      server_keys: secretInputs("server_keys"),
      zen_keys: secretInputs("zen_keys"),
      go_keys: secretInputs("go_keys"),
      anonymous: $("#c-anonymous").checked,
      proxies: secretInputs("proxies"),
      proxyfile: $("#c-proxyfile").value,
      prefer: $("#c-prefer").value,
      upstream: { zen: $("#c-up-zen").value, go: $("#c-up-go").value },
      retry: { max_attempts: number("#c-attempts"), timeout_seconds: number("#c-timeout") },
      models: {
        refresh_seconds: number("#c-refresh"),
        protocols: JSON.parse($("#c-protocols").value || "{}"),
      },
      performance: {
        max_idle_conns: number("#c-idle"),
        max_idle_conns_per_host: number("#c-idle-host"),
        max_conns_per_host: number("#c-max-host"),
        idle_conn_timeout_seconds: number("#c-idle-timeout"),
        connect_timeout_seconds: number("#c-connect"),
        failure_cooldown_seconds: number("#c-cooldown"),
        attempt_timeout_seconds: number("#c-attempt-timeout"),
      },
      logging: {
        level: $("#c-level").value,
        ring_size: number("#c-ring"),
        dump_request_bodies: $("#c-dump-bodies").checked,
      },
      webui: {
        enabled: $("#c-web-enabled").checked,
        listen: $("#c-web-listen").value,
        username: config.webui.username,
        session_ttl_minutes: number("#c-session"),
      },
    };
    const result = await api("/api/config", { method: "PUT", body: JSON.stringify(update) });
    config = result.config;
    ["server_keys", "zen_keys", "go_keys", "proxies"].forEach(
      (name) => ($(`#${name}-new`).value = ""),
    );
    fillConfig(config);
    await loadDebugModels();
    toast(
      result.result.restart_required
        ? `已应用；${result.result.restart_fields.join(", ")} 需重启`
        : "配置已保存并立即应用",
    );
  } catch (error) {
    toast(error.message, true);
  }
});
$("#reload-config").onclick = async () => {
  try {
    const result = await api("/api/config/reload", { method: "POST", body: "{}" });
    await loadConfig();
    await loadDebugModels();
    toast(result.restart_required ? "磁盘配置已重载，监听字段需重启" : "磁盘配置已重载");
  } catch (error) {
    toast(error.message, true);
  }
};
$("#reveal").onclick = async () => {
  const password = prompt("请再次输入管理密码");
  if (password === null) return;
  try {
    const values = await api("/api/config/reveal", {
      method: "POST",
      body: JSON.stringify({ password }),
    });
    $("#modal-content").textContent = JSON.stringify(values, null, 2);
    $("#modal").classList.remove("hidden");
  } catch (error) {
    toast(error.message, true);
  }
};
$("#modal-close").onclick = () => {
  $("#modal").classList.add("hidden");
  $("#modal-content").textContent = "";
};
$("#account-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    await api("/api/account", {
      method: "PUT",
      body: JSON.stringify({
        current_password: $("#a-current").value,
        username: $("#a-user").value,
        new_password: $("#a-new").value,
      }),
    });
    toast("账号已更新，请重新登录");
    setTimeout(showLogin, 800);
  } catch (error) {
    toast(error.message, true);
  }
});
function appendCell(row, value, className = "") {
  const cell = document.createElement("td");
  cell.textContent = String(value);
  if (className) cell.className = className;
  row.appendChild(cell);
}
function emptyRow(host, columns, message = "暂无数据") {
  const row = document.createElement("tr"),
    cell = document.createElement("td");
  cell.colSpan = columns;
  cell.className = "empty";
  cell.textContent = message;
  row.appendChild(cell);
  host.appendChild(row);
}
function renderResources(resources) {
  const proxies = $("#proxy-table");
  proxies.replaceChildren();
  (resources.proxies || []).forEach((proxy) => {
    const row = document.createElement("tr");
    appendCell(row, proxy.address);
    appendCell(row, proxy.healthy ? "正常" : "不可用", `status ${proxy.healthy ? "ok" : "bad"}`);
    appendCell(
      row,
      `Z ${proxy.zen_keys} · G ${proxy.go_keys} · A ${proxy.anonymous ? "开" : "关"}`,
    );
    proxies.appendChild(row);
  });
  if (!proxies.children.length) emptyRow(proxies, 3);
  const keys = $("#key-table");
  keys.replaceChildren();
  if (resources.anonymous) {
    const row = document.createElement("tr");
    appendCell(row, "anonymous");
    appendCell(row, "zen");
    appendCell(row, "已启用", "status ok");
    keys.appendChild(row);
  }
  (resources.keys || []).forEach((key) => {
    const row = document.createElement("tr");
    appendCell(row, key.id);
    appendCell(row, key.tier);
    appendCell(
      row,
      key.cooldown_until ? "冷却中" : "可用",
      `status ${key.cooldown_until ? "warn" : "ok"}`,
    );
    keys.appendChild(row);
  });
  if (!keys.children.length) emptyRow(keys, 3);
}
function renderBreakdowns(metrics) {
  const host = $("#breakdowns");
  host.replaceChildren();
  [
    ["端点", metrics.endpoints],
    ["状态码", metrics.statuses],
    ["模型", metrics.models],
    ["Tier", metrics.tiers],
  ].forEach(([title, data]) => {
    const block = document.createElement("div"),
      heading = document.createElement("h3"),
      table = document.createElement("table"),
      body = document.createElement("tbody");
    heading.textContent = title;
    table.className = "table";
    Object.entries(data || {})
      .sort((a, b) => b[1] - a[1])
      .slice(0, 10)
      .forEach(([key, value]) => {
        const row = document.createElement("tr");
        appendCell(row, key);
        appendCell(row, value);
        body.appendChild(row);
      });
    if (!body.children.length) emptyRow(body, 2, "暂无请求");
    table.appendChild(body);
    block.append(heading, table);
    host.appendChild(block);
  });
}
function renderGuide() {
  if (!config || !monitorData) return;
  const resources = monitorData.resources,
    models = resources.models || {},
    modelDescription = models.stale
      ? "目录来自" + (models.cache_source || "磁盘") + "快取，等待背景刷新。"
      : "上游目录已完成首次刷新且有可暴露模型。";
  const checks = [
    ["登录管理端", "当前 Session 已通过服务端验证。", true],
    ["配置本地 Key", "客户端使用 Server Key 访问 Gateway。", (config.server_keys || []).length > 0],
    [
      "选择上游通道",
      "至少有 Zen / Go Key，或启用匿名通道。",
      (config.zen_keys || []).length + (config.go_keys || []).length > 0 || config.anonymous,
    ],
    [
      "检查 Proxy",
      "至少一个代理节点健康。",
      (resources.proxies || []).some((proxy) => proxy.healthy),
    ],
    ["等待模型目录", modelDescription, models.exposed > 0 && !!models.updated_at],
    ["复制接入示例", "进入接入手册，替换模型与 YOUR_API_KEY。", true],
  ];
  const host = $("#guide-steps");
  host.replaceChildren();
  checks.forEach(([title, description, done]) => {
    const node = document.createElement("article"),
      heading = document.createElement("h3"),
      copy = document.createElement("p"),
      state = document.createElement("div");
    node.className = "guide-step";
    heading.textContent = title;
    copy.textContent = description;
    state.className = `step-state ${done ? "ok" : "warn"}`;
    state.textContent = done ? "✓ 已就绪" : "○ 待处理";
    node.append(heading, copy, state);
    host.appendChild(node);
  });
}
const formatTokens = (value) => Number(value || 0).toLocaleString();
function renderUsage() {
  if (!monitorData) return;
  const usage = monitorData.usage?.last_hour || {},
    tokens = usage.tokens || {};
  $("#u-coverage").textContent = `${((usage.coverage || 0) * 100).toFixed(1)}%`;
  $("#u-total").textContent = formatTokens(tokens.total_tokens);
  $("#u-cached").textContent = formatTokens(tokens.cached_tokens);
  $("#u-reasoning").textContent = formatTokens(tokens.reasoning_tokens);
  const trend = $("#usage-trend"),
    series = monitorData.metrics?.series || [],
    maximum = Math.max(1, ...series.map((point) => point.total_tokens || 0));
  trend.replaceChildren();
  series.forEach((point) => {
    const bar = document.createElement("div");
    bar.className = "trend-bar";
    bar.style.height = `${Math.max(2, Math.round(((point.total_tokens || 0) / maximum) * 100))}%`;
    bar.title = `${new Date(point.minute).toLocaleTimeString()} · input ${formatTokens(point.input_tokens)} · output ${formatTokens(point.output_tokens)} · total ${formatTokens(point.total_tokens)}`;
    trend.appendChild(bar);
  });
  const models = $("#usage-models");
  models.replaceChildren();
  Object.entries(usage.models || {})
    .sort((a, b) => (b[1].total_tokens || 0) - (a[1].total_tokens || 0))
    .slice(0, 15)
    .forEach(([model, value]) => {
      const row = document.createElement("tr");
      appendCell(row, model);
      appendCell(row, formatTokens(value.input_tokens));
      appendCell(row, formatTokens(value.output_tokens));
      appendCell(row, formatTokens(value.total_tokens));
      models.appendChild(row);
    });
  if (!models.children.length) emptyRow(models, 4, "尚无 Token 用量");
  const tiers = $("#usage-tiers"),
    tierEntries = Object.entries(usage.tiers || {}).sort(
      (a, b) => (b[1].total_tokens || 0) - (a[1].total_tokens || 0),
    ),
    tierTotal = Math.max(1, ...tierEntries.map(([, value]) => value.total_tokens || 0));
  tiers.replaceChildren();
  tierEntries.forEach(([tier, value]) => {
    const row = document.createElement("div"),
      name = document.createElement("strong"),
      track = document.createElement("div"),
      fill = document.createElement("div"),
      amount = document.createElement("span");
    row.className = "tier-row";
    name.textContent = tier;
    track.className = "tier-track";
    fill.className = "tier-fill";
    fill.style.width = `${Math.round(((value.total_tokens || 0) / tierTotal) * 100)}%`;
    amount.className = "mono";
    amount.textContent = formatTokens(value.total_tokens);
    track.appendChild(fill);
    row.append(name, track, amount);
    tiers.appendChild(row);
  });
  if (!tierEntries.length) {
    const empty = document.createElement("div");
    empty.className = "empty";
    empty.textContent = "尚无 Tier 用量";
    tiers.appendChild(empty);
  }
}
function renderFacts(selector, entries) {
  const host = $(selector);
  host.replaceChildren();
  entries.forEach(([label, value, tone]) => {
    const fact = document.createElement("dl"),
      term = document.createElement("dt"),
      detail = document.createElement("dd");
    fact.className = "fact";
    term.textContent = label;
    detail.textContent = value ?? "—";
    if (tone) detail.className = tone;
    fact.append(term, detail);
    host.appendChild(fact);
  });
}
function renderPlayKeys() {
  const select = $("#play-key");
  if (!select) return;
  const selected = select.value;
  select.replaceChildren();
  const auto = document.createElement("option");
  auto.value = "auto";
  auto.textContent = "自动路由（匿名 / Tier 回退 / Key 轮询）";
  select.appendChild(auto);
  const keys = debugData?.keys || {};
  [
    ["zen", "Zen Keys"],
    ["go", "Go Keys"],
  ].forEach(([tier, label]) => {
    const items = keys[tier] || [];
    if (!items.length) return;
    const group = document.createElement("optgroup");
    group.label = label;
    items.forEach((item) => {
      const option = document.createElement("option");
      option.value = `${tier}:${item.id}`;
      const cooling = item.cooldown_until
        ? ` · 冷却至 ${new Date(item.cooldown_until).toLocaleTimeString()}`
        : "";
      option.textContent = `${tier.toUpperCase()} · ${item.display} · ${item.id}${cooling}`;
      group.appendChild(option);
    });
    select.appendChild(group);
  });
  if (selected && [...select.options].some((option) => option.value === selected))
    select.value = selected;
  else select.value = "auto";
}
async function loadDebugModels() {
  try {
    debugData = await api("/api/debug/models");
    if (debugData.last_inference) lastPlayground = debugData.last_inference;
    const select = $("#play-model"),
      selected = select.value;
    select.replaceChildren();
    (debugData.models || []).forEach((item) => {
      const option = document.createElement("option");
      option.value = item.model;
      option.textContent = item.model;
      select.appendChild(option);
    });
    if (selected && [...select.options].some((option) => option.value === selected))
      select.value = selected;
    renderPlayKeys();
    if (!$("#play-request").value.trim()) resetPlayTemplate();
    renderDiagnostics();
  } catch (error) {
    toast(error.message, true);
  }
}
function playgroundTemplate() {
  const protocol = $("#play-protocol").value,
    model = $("#play-model").value || "YOUR_MODEL";
  if (protocol === "responses") return { model, input: "用一句话说明这个模型的能力。" };
  if (protocol === "anthropic")
    return {
      model,
      max_tokens: 512,
      messages: [{ role: "user", content: "用一句话说明这个模型的能力。" }],
    };
  return { model, messages: [{ role: "user", content: "用一句话说明这个模型的能力。" }] };
}
function resetPlayTemplate() {
  $("#play-request").value = JSON.stringify(playgroundTemplate(), null, 2);
}
$("#play-protocol").addEventListener("change", resetPlayTemplate);
$("#play-model").addEventListener("change", () => {
  try {
    const request = JSON.parse($("#play-request").value);
    request.model = $("#play-model").value;
    $("#play-request").value = JSON.stringify(request, null, 2);
  } catch (_) {
    resetPlayTemplate();
  }
});
$("#play-reset").onclick = resetPlayTemplate;
function displayPlayground(result) {
  lastPlayground = result;
  const keyTest = result.key_test
    ? {
        usable: "Key 可用",
        rejected: "Key 被拒绝",
        rate_limited: "Key 已被接受但受到限流",
        upstream_error: "上游异常，无法判定 Key",
        transport_error: "网络或代理异常，无法判定 Key",
        unavailable: "Key 或运行时不可用",
        request_error: "请求未成功",
      }[result.key_test] || result.key_test
    : "";
  $("#play-state").textContent = result.ok
    ? `请求成功 · HTTP ${result.http_status}`
    : `请求失败 · HTTP ${result.http_status}`;
  $("#play-state").className = `section-caption ${result.ok ? "ok" : "bad"}`;
  const route = result.route || {},
    eligibility = route.anonymous_eligibility || {},
    selected = result.selected_key,
    routeLabel = route.tier
      ? `${route.tier} / ${route.anonymous ? "anonymous" : route.key_id || "key"}`
      : "未路由";
  renderFacts("#play-facts", [
    ["HTTP 状态", result.http_status, result.ok ? "ok" : "bad"],
    ["耗时", `${result.duration_ms} ms`],
    ["Request ID", result.request_id || "—"],
    ["测试模式", selected ? `指定 ${selected.tier.toUpperCase()} Key` : "自动路由"],
    ["测试 Key", selected ? `${selected.display} · ${selected.id}` : "—"],
    [
      "Key 判断",
      keyTest || "自动路由未单独判定",
      result.key_test === "usable" ? "ok" : result.key_test ? "bad" : "",
    ],
    ["实际路由", routeLabel],
    ["原生协议", route.native_protocol || "—"],
    [
      "匿名判断",
      `${eligibility.allowed ? "允许" : "拒绝"} · ${eligibility.source || "—"}`,
      eligibility.allowed ? "ok" : "warn",
    ],
  ]);
  $("#play-output").textContent = JSON.stringify(result.response, null, 2);
  renderDiagnostics();
}
$("#play-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  let request;
  try {
    request = JSON.parse($("#play-request").value);
  } catch (error) {
    toast(`请求 JSON 无效：${error.message}`, true);
    return;
  }
  const keyValue = $("#play-key").value,
    key =
      keyValue === "auto"
        ? { mode: "auto" }
        : {
            mode: "selected",
            tier: keyValue.split(":")[0],
            id: keyValue.split(":").slice(1).join(":"),
          };
  $("#play-run").disabled = true;
  $("#play-state").textContent =
    key.mode === "selected" ? "指定 Key 测试进行中…" : "Gateway 请求进行中…";
  try {
    const result = await api("/api/debug/inference", {
      method: "POST",
      body: JSON.stringify({ protocol: $("#play-protocol").value, key, request }),
    });
    displayPlayground(result);
  } catch (error) {
    toast(error.message, true);
    $("#play-state").textContent = "诊断入口请求失败";
  } finally {
    $("#play-run").disabled = false;
  }
});
$("#play-copy").onclick = async () => {
  try {
    await navigator.clipboard.writeText($("#play-output").textContent);
    toast("原始响应已复制");
  } catch (_) {
    toast("浏览器不允许剪贴板访问", true);
  }
};
function renderDiagnostics() {
  if (!monitorData && !debugData) return;
  const metadata = debugData?.metadata || monitorData?.resources?.metadata || {},
    last = lastPlayground || debugData?.last_inference;
  renderFacts("#metadata-facts", [
    ["就绪", metadata.ready ? "是" : "否", metadata.ready ? "ok" : "warn"],
    ["模型数", formatTokens(metadata.models)],
    ["更新时间", metadata.updated_at ? new Date(metadata.updated_at).toLocaleString() : "尚未更新"],
    ["是否过期", metadata.stale ? "是" : "否", metadata.stale ? "warn" : "ok"],
    ["下次刷新", metadata.next_refresh ? new Date(metadata.next_refresh).toLocaleString() : "—"],
    ["最近错误", metadata.last_error || "无", metadata.last_error ? "bad" : "ok"],
  ]);
  renderFacts(
    "#last-playground",
    last
      ? [
          ["结果", last.ok ? "成功" : "失败", last.ok ? "ok" : "bad"],
          ["HTTP", last.http_status],
          ["耗时", `${last.duration_ms} ms`],
          ["Request ID", last.request_id || "—"],
          ["模型", last.route?.model || "—"],
          [
            "路由",
            last.route?.tier
              ? `${last.route.tier} / ${last.route.anonymous ? "anonymous" : last.route.key_id || "key"}`
              : "未路由",
          ],
        ]
      : [["状态", "尚无 Playground 追踪"]],
  );
  renderDiagnosticModels();
  const requests = $("#upstream-requests");
  requests.replaceChildren();
  (monitorData?.upstream?.requests || [])
    .slice(-100)
    .reverse()
    .forEach((request) => {
      const row = document.createElement("tr");
      appendCell(row, new Date(request.time).toLocaleTimeString());
      appendCell(row, request.request_id);
      appendCell(row, request.model);
      appendCell(row, request.tier || "—");
      appendCell(row, request.channel);
      appendCell(row, request.key_id || "—");
      appendCell(row, request.attempts);
      const terminalResults = {
        stream_error: "流式响应失败",
        client_canceled: "客户端已取消",
      };
      const result = terminalResults[request.outcome]
        ? `HTTP ${request.status} · ${terminalResults[request.outcome]}`
        : request.status || request.outcome;
      appendCell(row, result, request.success ? "ok" : "bad");
      appendCell(row, `${request.duration_ms} ms`);
      requests.appendChild(row);
    });
  if (!requests.children.length) emptyRow(requests, 9, "尚无请求路由");
  const attempts = $("#upstream-attempts");
  attempts.replaceChildren();
  (monitorData?.upstream?.recent || [])
    .slice(-100)
    .reverse()
    .forEach((attempt) => {
      const row = document.createElement("tr");
      appendCell(row, new Date(attempt.time).toLocaleTimeString());
      appendCell(row, attempt.request_id);
      appendCell(row, attempt.model);
      appendCell(row, `${attempt.tier} / ${attempt.channel}`);
      appendCell(row, attempt.key_id);
      appendCell(row, attempt.proxy_node);
      appendCell(row, attempt.status || attempt.outcome, attempt.success ? "ok" : "bad");
      appendCell(row, `${attempt.duration_ms} ms`);
      attempts.appendChild(row);
    });
  if (!attempts.children.length) emptyRow(attempts, 8, "尚无上游尝试");
}
function renderDiagnosticModels() {
  const host = $("#diagnostic-models");
  if (!host || !debugData) return;
  const query = $("#diagnostic-model-search").value.trim().toLowerCase();
  host.replaceChildren();
  (debugData.models || [])
    .filter((item) => !query || item.model.toLowerCase().includes(query))
    .forEach((item) => {
      const eligibility = item.anonymous_eligibility || {},
        row = document.createElement("tr"),
        input = eligibility.input_cost,
        output = eligibility.output_cost,
        plan = [...(item.anonymous ? ["anonymous"] : []), ...(item.key_tiers || [])];
      appendCell(row, item.model);
      appendCell(row, `${item.native_protocol} · ${item.protocol_source}`);
      appendCell(
        row,
        item.route_error || (plan.length ? plan.join(" → ") : "—"),
        item.route_error ? "bad" : "",
      );
      appendCell(row, eligibility.allowed ? "允许" : "拒绝", eligibility.allowed ? "ok" : "warn");
      appendCell(row, eligibility.source || "—");
      appendCell(row, `${input == null ? "?" : input} / ${output == null ? "?" : output}`, "cost");
      host.appendChild(row);
    });
  if (!host.children.length) emptyRow(host, 6, "没有匹配模型");
}
$("#diagnostic-model-search").addEventListener("input", renderDiagnosticModels);
async function refreshMonitor() {
  try {
    monitorData = await api("/api/monitor");
    const metrics = monitorData.metrics;
    $("#version").textContent = `version ${monitorData.version}`;
    $("#m-total").textContent = Number(metrics.last_hour.total || 0).toLocaleString();
    $("#m-rate").textContent = `${((metrics.last_hour.success_rate || 0) * 100).toFixed(1)}%`;
    $("#m-p95").textContent = `${metrics.last_hour.p95_ms || 0} ms`;
    $("#m-active").textContent = `${metrics.active_requests || 0} / ${metrics.active_streams || 0}`;
    renderResources(monitorData.resources);
    renderBreakdowns(metrics);
    renderGuide();
    renderUsage();
    renderDiagnostics();
  } catch (_) {}
}
$("#refresh-now").onclick = async () => {
  await Promise.all([refreshMonitor(), loadConfig(), loadDebugModels()]);
  if (location.hash === "#model-quotas") await loadModelQuotas();
  if (location.hash === "#api-keys") await loadAPIKeys();
  toast("状态已刷新");
};
function derivedAPIBase() {
  if (!config) return `${location.protocol}//${location.hostname}:8080`;
  const listen = config.effective?.listen || config.listen || ":8080";
  const bracket = listen.match(/^\[.*\]:(\d+)$/),
    plain = listen.match(/:(\d+)$/),
    port = (bracket || plain)?.[1] || "8080";
  return `${location.protocol}//${location.hostname}:${port}`;
}
function codeCard(label, text) {
  const card = document.createElement("div"),
    head = document.createElement("div"),
    title = document.createElement("span"),
    copy = document.createElement("button"),
    pre = document.createElement("pre"),
    code = document.createElement("code");
  card.className = "code-card";
  head.className = "code-head";
  title.textContent = label;
  copy.type = "button";
  copy.className = "copy";
  copy.textContent = "复制";
  copy.onclick = async () => {
    try {
      await navigator.clipboard.writeText(text);
      toast("示例已复制");
    } catch (_) {
      toast("浏览器不允许剪贴板访问", true);
    }
  };
  head.append(title, copy);
  code.textContent = text;
  pre.appendChild(code);
  card.append(head, pre);
  return card;
}
function renderAccessExamples() {
  if (!config) return;
  const base = derivedAPIBase(),
    model = $("#access-model")?.value.trim() || "YOUR_MODEL";
  $("#access-base-value").value = base;
  const protocols = $("#access-protocol-examples"),
    sdks = $("#access-sdk-examples");
  protocols.replaceChildren();
  sdks.replaceChildren();
  const chat = `curl ${base}/v1/chat/completions \\\n  -H "Authorization: Bearer YOUR_API_KEY" \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"${model}","messages":[{"role":"user","content":"Hello"}]}'`;
  const responses = `curl ${base}/v1/responses \\\n  -H "Authorization: Bearer YOUR_API_KEY" \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"${model}","input":"Hello"}'`;
  const anthropic = `curl ${base}/v1/messages \\\n  -H "x-api-key: YOUR_API_KEY" \\\n  -H "anthropic-version: 2023-06-01" \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"${model}","max_tokens":512,"messages":[{"role":"user","content":"Hello"}]}'`;
  protocols.append(
    codeCard("Chat Completions", chat),
    codeCard("Responses", responses),
    codeCard("Anthropic Messages", anthropic),
  );
  const python = `from openai import OpenAI\n\nclient = OpenAI(base_url="${base}/v1", api_key="YOUR_API_KEY")\nresponse = client.chat.completions.create(\n    model="${model}",\n    messages=[{"role": "user", "content": "Hello"}],\n)\nprint(response.choices[0].message.content)`;
  const javascript = `import OpenAI from "openai";\n\nconst client = new OpenAI({\n  baseURL: "${base}/v1",\n  apiKey: "YOUR_API_KEY",\n});\nconst response = await client.responses.create({\n  model: "${model}",\n  input: "Hello",\n});\nconsole.log(response.output_text);`;
  sdks.append(
    codeCard("Python · OpenAI SDK", python),
    codeCard("JavaScript · OpenAI SDK", javascript),
  );
}
$("#access-model").addEventListener("input", renderAccessExamples);
$$("[data-scroll]").forEach(
  (button) =>
    (button.onclick = () =>
      document.getElementById(button.dataset.scroll).scrollIntoView({ behavior: "smooth" })),
);
function connectLogs() {
  if (eventSource) eventSource.close();
  eventSource = new EventSource("/api/logs/stream");
  eventSource.addEventListener("log", (event) => {
    const item = JSON.parse(event.data);
    logEvents.push(item);
    if (logEvents.length > 2000) logEvents.shift();
    if (!paused && !logRenderPending) {
      logRenderPending = true;
      requestAnimationFrame(() => {
        logRenderPending = false;
        renderLogs();
      });
    }
  });
  eventSource.addEventListener("gap", () => toast("较早日志已从内存清除", true));
}
function renderLogs() {
  const level = $("#log-level").value,
    component = $("#log-component").value.toLowerCase(),
    query = $("#log-search").value.toLowerCase(),
    rows = logEvents
      .filter(
        (item) =>
          (!level || item.level === level) &&
          (!component || (item.component || "").toLowerCase().includes(component)) &&
          (!query || JSON.stringify(item).toLowerCase().includes(query)),
      )
      .slice(-500),
    host = $("#log-list");
  host.replaceChildren();
  rows.forEach((item) => {
    const row = document.createElement("div");
    row.className = "log";
    const timeNode = document.createElement("span"),
      levelNode = document.createElement("span"),
      componentNode = document.createElement("span"),
      messageNode = document.createElement("span"),
      fieldsNode = document.createElement("span");
    timeNode.className = "log-time";
    timeNode.textContent = new Date(item.time).toLocaleTimeString();
    levelNode.className =
      item.level === "error"
        ? "bad"
        : item.level === "warn"
          ? "warn"
          : item.level === "info"
            ? "ok"
            : "";
    levelNode.textContent = item.level;
    componentNode.className = "log-component";
    componentNode.textContent = item.component || "core";
    messageNode.textContent = item.message;
    fieldsNode.className = "log-fields";
    fieldsNode.textContent = JSON.stringify(item.fields || {});
    row.append(timeNode, levelNode, componentNode, messageNode, fieldsNode);
    host.appendChild(row);
  });
  if (!rows.length) {
    const empty = document.createElement("div");
    empty.className = "empty";
    empty.textContent = "等待日志事件…";
    host.appendChild(empty);
  }
  host.scrollTop = host.scrollHeight;
}
["#log-level", "#log-component", "#log-search"].forEach((id) =>
  $(id).addEventListener("input", renderLogs),
);
$("#log-pause").onclick = () => {
  paused = !paused;
  $("#log-pause").textContent = paused ? "继续" : "暂停";
  if (!paused) renderLogs();
};
let quotaData = null,
  quotaTimer = null,
  quotaLoading = false,
  createdKey = "",
  apiKeysLoading = false;
const localTime = (value) =>
  value ? new Date(value).toLocaleString("zh-CN", { hour12: false }) : "—";

function clearCreatedKey() {
  createdKey = "";
  $("#api-key-env").textContent = "";
  $("#api-key-created").classList.add("hidden");
}

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast("已复制");
  } catch {
    toast("浏览器未允许复制，请手动选中复制", true);
  }
}

async function loadModelQuotas() {
  if (quotaLoading) return;
  quotaLoading = true;
  $("#quota-refresh").disabled = true;
  $("#quota-refresh").textContent = "读取中…";
  try {
    const value = await api("/api/model-quotas");
    if (!csrf) return;
    quotaData = value;
    const select = $("#quota-account"),
      selected = select.value;
    select.replaceChildren();
    for (const account of value.accounts || []) {
      const option = document.createElement("option");
      option.value = account.id;
      option.textContent = `Go Key ${account.display}`;
      select.appendChild(option);
    }
    if ([...select.options].some((option) => option.value === selected)) select.value = selected;
    if (!select.options.length) select.appendChild(new Option("尚未配置 Go Key", ""));
    renderModelQuotas();
  } catch (error) {
    $("#quota-status").textContent = `查询失败：${error.message}。已有显示仅为上次查询结果。`;
  } finally {
    quotaLoading = false;
    $("#quota-refresh").disabled = false;
    $("#quota-refresh").textContent = "刷新额度";
  }
}

function renderModelQuotas() {
  if (!quotaData) return;
  const account = quotaData.accounts.find((item) => item.id === $("#quota-account").value);
  const stale = account?.status === "stale";
  const cards = $("#quota-cards");
  cards.replaceChildren();
  $("#quota-status").textContent = !account
    ? "请先在配置中心添加 OpenCode Go Key。"
    : account.error
      ? `${account.error}${stale ? "；下方显示上次成功数据，当前额度未知。" : "；当前额度未知。"}`
      : "已连接 OpenCode · 共享套餐额度。多个 Key 的额度不相加。";
  $("#quota-updated").textContent = account?.fetched_at
    ? `${stale ? "上次成功查询" : "额度更新时间"}：${localTime(account.fetched_at)} · 缓存 60 秒 · 来源：OpenCode Go /v1/usage`
    : "";
  const windows = [
    ["rolling", "5 小时窗口"],
    ["weekly", "每周窗口"],
    ["monthly", "每月窗口"],
  ];
  for (const [id, label] of windows) {
    const item = account?.windows?.[id];
    const card = document.createElement("div"),
      heading = document.createElement("h3"),
      value = document.createElement("div"),
      progress = document.createElement("progress"),
      reset = document.createElement("div");
    card.className = `quota-card${item?.remaining_percent < 20 ? " quota-low" : ""}`;
    heading.textContent = label;
    value.className = "quota-value";
    value.textContent = item ? `≈ ${item.remaining_percent}%` : "未知";
    progress.max = 100;
    if (item) progress.value = item.remaining_percent;
    else progress.value = 0;
    progress.setAttribute("aria-label", `${label}剩余额度`);
    reset.className = "quota-reset";
    reset.textContent = item
      ? `${stale ? "历史剩余" : "剩余"} · ${item.status === "rate-limited" ? "已限额 · " : ""}重置于 ${localTime(item.resets_at)}`
      : "等待上游额度数据";
    card.append(heading, value, progress, reset);
    cards.appendChild(card);
  }
  const query = $("#quota-model-search").value.trim().toLowerCase();
  const models = quotaData.models.filter((model) => model.id.toLowerCase().includes(query));
  $("#quota-model-count").textContent = `${models.length} / ${quotaData.models.length}`;
  const catalog = quotaData.catalog;
  $("#quota-catalog-status").textContent =
    `${catalog?.stale ? "模型目录暂未更新，显示缓存 · " : ""}目录更新时间：${localTime(catalog?.updated_at)}`;
  const host = $("#quota-models");
  host.replaceChildren();
  for (const model of models) {
    const row = document.createElement("tr");
    appendCell(row, model.id, "mono");
    appendCell(
      row,
      { chat: "Chat Completions", responses: "Responses", anthropic: "Messages" }[model.protocol] ||
        model.protocol,
    );
    for (const [id] of windows) {
      const item = account?.windows?.[id];
      appendCell(row, item ? `≈ ${item.remaining_percent}%${stale ? "（历史）" : ""}` : "未知");
    }
    const cell = document.createElement("td"),
      button = document.createElement("button");
    button.className = "ghost";
    button.textContent = "复制名称";
    button.onclick = () => copyText(model.id);
    cell.appendChild(button);
    row.appendChild(cell);
    host.appendChild(row);
  }
  if (!models.length)
    emptyRow(host, 6, quotaData.models.length ? "没有匹配的模型" : "模型目录尚未就绪，请稍后刷新");
}
$("#quota-refresh").onclick = loadModelQuotas;
$("#quota-account").onchange = renderModelQuotas;
$("#quota-model-search").oninput = renderModelQuotas;

async function loadAPIKeys() {
  if (apiKeysLoading) return;
  apiKeysLoading = true;
  $("#api-key-refresh").disabled = true;
  try {
    const data = await api("/api/api-keys");
    if (csrf) renderAPIKeys(data.keys);
  } catch (error) {
    $("#api-key-status").textContent = `读取失败：${error.message}`;
  } finally {
    apiKeysLoading = false;
    $("#api-key-refresh").disabled = false;
  }
}

async function mutateAPIKey(id, method, body, button) {
  button.disabled = true;
  try {
    const data = await api(`/api/api-keys/${encodeURIComponent(id)}`, {
      method,
      ...(body ? { body: JSON.stringify(body) } : {}),
    });
    if (!csrf) return;
    renderAPIKeys(data.keys);
    await loadConfig();
    toast("已保存并立即生效");
  } catch (error) {
    toast(error.message, true);
  } finally {
    button.disabled = false;
  }
}

function renderAPIKeys(keys) {
  $("#api-key-count").textContent = `${keys.length} 个`;
  $("#api-key-status").textContent = `${keys.filter((key) => key.enabled).length} 个已启用`;
  const host = $("#api-key-list");
  host.replaceChildren();
  for (const key of keys) {
    const row = document.createElement("tr");
    appendCell(row, key.name);
    appendCell(row, key.display, "mono");
    appendCell(row, key.enabled ? "已启用" : "已停用", key.enabled ? "status ok" : "status warn");
    appendCell(row, key.created_at ? localTime(key.created_at) : "已有配置");
    const cell = document.createElement("td"),
      actions = document.createElement("div");
    actions.className = "key-actions";
    for (const [label, action] of [
      [
        "重命名",
        (button) => {
          const name = prompt("密钥名称", key.name);
          if (name !== null) mutateAPIKey(key.id, "PATCH", { name }, button);
        },
      ],
      [
        key.enabled ? "停用" : "启用",
        (button) => mutateAPIKey(key.id, "PATCH", { enabled: !key.enabled }, button),
      ],
      [
        "删除",
        (button) => {
          if (confirm(`删除“${key.name}”后，使用它的应用将无法继续访问。确定删除？`))
            mutateAPIKey(key.id, "DELETE", null, button);
        },
      ],
    ]) {
      const button = document.createElement("button");
      button.className = "ghost";
      button.textContent = label;
      button.onclick = () => action(button);
      actions.appendChild(button);
    }
    cell.appendChild(actions);
    row.appendChild(cell);
    host.appendChild(row);
  }
  if (!keys.length) emptyRow(host, 5, "暂无 API Key");
}
$("#api-key-form").onsubmit = async (event) => {
  event.preventDefault();
  const button = $("#api-key-create");
  button.disabled = true;
  try {
    const data = await api("/api/api-keys", {
      method: "POST",
      body: JSON.stringify({ name: $("#api-key-name").value, value: $("#api-key-value").value }),
    });
    if (!csrf) return;
    createdKey = data.key;
    const goModels = debugData?.models?.filter((item) => item.available_go) || [];
    const model =
      goModels.find((item) => item.model === "glm-5.3-flash")?.model ||
      goModels[0]?.model ||
      "YOUR_MODEL";
    $("#api-key-env").textContent =
      `OPENAI_API_KEY=${data.key}\nOPENAI_BASE_URL=${derivedAPIBase()}/v1\nOPENAI_MODEL=${model}`;
    $("#api-key-created").classList.remove("hidden");
    $("#api-key-form").reset();
    renderAPIKeys(data.keys);
    await loadConfig();
    toast("API Key 已创建");
  } catch (error) {
    toast(error.message, true);
  } finally {
    button.disabled = false;
  }
};
$("#api-key-refresh").onclick = loadAPIKeys;
$("#api-key-dismiss").onclick = clearCreatedKey;
$("#api-key-copy").onclick = () => copyText(createdKey);
$("#api-key-copy-env").onclick = () => copyText($("#api-key-env").textContent);
let rotationData = null,
  rotationDirty = false;
const rotationReasons = {
  success: "成功",
  client_canceled: "调用方取消",
  quota_or_rate_limit: "额度不足 / 限流",
  transport_error: "连接错误",
  first_output_timeout: "首个输出超时",
  stream_error: "输出中断",
  response_read_error: "读取响应失败",
  response_conversion_error: "响应解析或转换失败",
  request_conversion_error: "请求转换失败",
  upstream_error: "上游错误",
};
async function loadRotation() {
  try {
    const value = await api("/api/rotation");
    if (!csrf) return;
    rotationData = value;
    rotationDirty = false;
    renderRotation();
  } catch (error) {
    toast(error.message, true);
  }
}
function renderRotation() {
  const state = rotationData;
  $("#rotation-status").textContent =
    state.persistence_error || "状态已更新 · 两组独立轮转，所有调用方共享当前位置。";
  $("#rotation-timeout").value = state.first_output_seconds;
  const host = $("#rotation-groups");
  host.replaceChildren();
  for (const group of state.groups) {
    const section = document.createElement("div");
    section.className = "section";
    const title = document.createElement("h2");
    title.textContent = group.id === "sota" ? "SOTA" : "甜点";
    const label = document.createElement("label");
    label.textContent = "调用别名";
    label.htmlFor = `rotation-alias-${group.id}`;
    const input = document.createElement("input");
    input.id = label.htmlFor;
    input.value = group.alias;
    input.required = true;
    input.maxLength = 64;
    input.oninput = () => {
      group.alias = input.value;
      rotationDirty = true;
    };
    const field = document.createElement("div");
    field.className = "field";
    field.append(label, input);
    const current = document.createElement("p");
    current.className = "mono";
    current.textContent = `当前模型：${group.current || "等待可用模型"}`;
    const table = document.createElement("table");
    table.className = "table";
    const header = document.createElement("tr");
    for (const text of ["顺序", "模型", "状态 / 恢复时间", "操作"]) {
      const th = document.createElement("th");
      th.textContent = text;
      header.append(th);
    }
    const thead = document.createElement("thead");
    thead.append(header);
    table.append(thead);
    const tbody = document.createElement("tbody");
    table.append(tbody);
    group.order.forEach((model, index) => {
      const row = document.createElement("tr");
      appendCell(row, String(index + 1));
      appendCell(row, model, "mono");
      const until = group.paused[model],
        paused = until && new Date(until) > new Date();
      appendCell(
        row,
        !group.available[model]
          ? "已下架 / 暂不可用"
          : paused
            ? `暂停至 ${localTime(until)}`
            : model === group.current
              ? "当前使用"
              : "可用",
      );
      const actions = document.createElement("td");
      actions.className = "rotation-actions";
      for (const [text, delta] of [
        ["上移", -1],
        ["下移", 1],
      ]) {
        const button = document.createElement("button");
        button.type = "button";
        button.className = "ghost";
        button.textContent = text;
        button.setAttribute("aria-label", `${model} ${text}`);
        button.disabled = index + delta < 0 || index + delta >= group.order.length;
        button.onclick = () => {
          [group.order[index], group.order[index + delta]] = [
            group.order[index + delta],
            group.order[index],
          ];
          state.first_output_seconds = Number($("#rotation-timeout").value);
          rotationDirty = true;
          renderRotation();
        };
        actions.append(button);
      }
      const select = document.createElement("button");
      select.type = "button";
      select.className = "ghost";
      select.textContent = "设为当前";
      select.disabled = !group.available[model];
      select.setAttribute("aria-label", `使用 ${model}`);
      select.onclick = async () => {
        select.disabled = true;
        try {
          const result = await api("/api/rotation/current", {
            method: "POST",
            body: JSON.stringify({ group: group.id, model }),
          });
          // Keep unsaved alias/order edits while refreshing operational state.
          for (const item of state.groups) {
            const live = result.groups.find((x) => x.id === item.id);
            Object.assign(item, {
              current: live.current,
              paused: live.paused,
              switches: live.switches,
            });
          }
          state.persistence_error = result.persistence_error;
          renderRotation();
          toast("当前模型已切换");
        } catch (error) {
          select.disabled = false;
          toast(error.message, true);
        }
      };
      actions.append(select);
      row.append(actions);
      tbody.append(row);
    });
    if (!group.order.length) emptyRow(tbody, 4, "等待 Go 模型目录");
    const wrap = document.createElement("div");
    wrap.className = "table-wrap";
    wrap.append(table);
    const recent = document.createElement("div");
    recent.className = "rotation-switches";
    const caption = document.createElement("h3");
    caption.textContent = "最近切换";
    recent.append(caption);
    for (const item of (group.switches || []).slice(-5).reverse()) {
      const line = document.createElement("p");
      line.textContent = `${localTime(item.time)} · ${item.from} → ${item.to} · ${rotationReasons[item.reason] || item.reason}`;
      recent.append(line);
    }
    if (!group.switches?.length) {
      const line = document.createElement("p");
      line.textContent = "尚无切换记录";
      recent.append(line);
    }
    section.append(title, field, current, wrap, recent);
    host.append(section);
  }
  const attempts = $("#rotation-attempts");
  attempts.replaceChildren();
  for (const item of (state.attempts || []).slice().reverse()) {
    const row = document.createElement("tr");
    for (const value of [
      localTime(item.time),
      item.request,
      item.alias,
      item.model,
      String(item.number),
      rotationReasons[item.outcome] || item.outcome,
    ])
      appendCell(row, value);
    attempts.append(row);
  }
  if (!state.attempts?.length) emptyRow(attempts, 6, "调用分组别名后显示最近 100 次尝试");
}
$("#rotation-refresh").onclick = () => {
  if (rotationDirty) {
    toast("有未保存的修改，请先保存后刷新", true);
    return;
  }
  loadRotation();
};
$("#rotation-timeout").oninput = () => {
  rotationDirty = true;
};
$("#rotation-form").onsubmit = async (event) => {
  event.preventDefault();
  if (!rotationData) return;
  const button = $("#rotation-save");
  button.disabled = true;
  try {
    const body = { first_output_seconds: Number($("#rotation-timeout").value) };
    for (const group of rotationData.groups)
      body[group.id] = { alias: group.alias.trim(), order: group.order };
    rotationData = await api("/api/rotation", { method: "PUT", body: JSON.stringify(body) });
    rotationDirty = false;
    renderRotation();
    toast("模型轮转配置已保存并生效");
  } catch (error) {
    toast(error.message, true);
  } finally {
    button.disabled = false;
  }
};
boot();
