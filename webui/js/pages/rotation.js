// Page: 模型轮转 — built-in sota/sweet plus operator-defined groups, per-group
// ordering, manual selection, recent switches and attempts.
import { api, hasSession } from "../api.js";
import { t, onChange } from "../i18n.js";
import { $, el, emptyRow, localTime, toast } from "../ui.js";

const reasons = () => ({
  success: t("成功", "rotation.reason.success"),
  client_canceled: t("调用方取消", "rotation.reason.clientCanceled"),
  quota_or_rate_limit: t("额度不足 / 限流", "rotation.reason.quota"),
  transport_error: t("连接错误", "rotation.reason.transport"),
  first_output_timeout: t("首个输出超时", "rotation.reason.firstOutputTimeout"),
  stream_error: t("输出中断", "rotation.reason.streamError"),
  response_read_error: t("读取响应失败", "rotation.reason.readError"),
  response_conversion_error: t("响应解析或转换失败", "rotation.reason.conversionError"),
  request_conversion_error: t("请求转换失败", "rotation.reason.requestConversion"),
  upstream_error: t("上游错误", "rotation.reason.upstreamError"),
});

let data = null;
let dirty = false;
let selected = "";

// The persisted rotation state serializes nil maps and slices as JSON null;
// normalize them before anything renders against them.
function normalize(value) {
  for (const group of value.groups || []) {
    group.order = group.order || [];
    group.available = group.available || {};
    group.paused = group.paused || {};
    group.switches = group.switches || [];
  }
  value.available_models = value.available_models || [];
  value.attempts = value.attempts || [];
  return value;
}

function kind(group) {
  if (group.id === "sota") return "sota";
  if (group.id === "sweet") return "sweet";
  return "custom";
}

function label(group) {
  if (group.id === "sota") return "SOTA";
  if (group.id === "sweet") return t("甜点", "rotation.sweet");
  return group.alias || t("自定义分组", "rotation.customDefault");
}

function caption(group) {
  switch (kind(group)) {
    case "sota":
      return t("内置分组，固定包含五个 SOTA 模型，可调整顺序。", "rotation.detail.sota");
    case "sweet":
      return t("内置兜底分组，自动纳入其余 Go / Zen 模型，可调整顺序。", "rotation.detail.sweet");
    default:
      return t("自定义分组，仅轮转下方列出的模型（可来自 Go 或 Zen）。", "rotation.detail.custom");
  }
}

async function load() {
  try {
    const value = await api("/api/rotation");
    if (!hasSession()) return;
    data = normalize(value);
    dirty = false;
    selected = data.groups[0]?.id || "";
    render();
  } catch (error) {
    toast(error.message, true);
  }
}

function select(id) {
  selected = id;
  render();
}

function render() {
  if (!data) return;
  $("#rotation-status").textContent =
    data.persistence_error ||
    t("状态已更新 · 自定义与内置分组独立轮转，所有调用方共享当前位置。", "rotation.status");
  $("#rotation-timeout").value = data.first_output_seconds;
  renderList();
  renderDetail();
  renderAttempts();
}

function renderList() {
  const host = $("#rotation-list");
  host.replaceChildren();
  for (const group of data.groups) {
    const paused = group.order.filter(
      (model) => group.paused?.[model] && new Date(group.paused[model]) > new Date(),
    ).length;
    const meta = [
      group.current || t("等待可用模型", "rotation.waitingModel"),
      t(`${group.order.length} 个模型`, "rotation.modelCount", group.order.length),
    ];
    if (paused) meta.push(t(`${paused} 暂停`, "rotation.pausedCount", paused));
    host.append(
      el(
        "button",
        {
          type: "button",
          class: `rotation-item${group.id === selected ? " active" : ""}`,
          onclick: () => select(group.id),
        },
        [
          el("span", { class: "rotation-item-name", text: label(group) }),
          el("span", {
            class: "rotation-item-alias mono",
            text: t(`别名 ${group.alias}`, "rotation.itemAlias", group.alias),
          }),
          el("span", { class: "rotation-item-meta", text: meta.join(" · ") }),
        ],
      ),
    );
  }
  if (!data.groups.length) host.textContent = t("暂无分组", "rotation.noGroups");
}

function modelTable(group) {
  const isCustom = kind(group) === "custom";
  const table = el("table", { class: "table" }, [
    el("thead", {}, [
      el(
        "tr",
        {},
        [
          t("顺序", "rotation.col.order"),
          t("模型", "common.model"),
          t("状态 / 恢复时间", "rotation.col.state"),
          t("操作", "common.action"),
        ].map((text) => el("th", { text })),
      ),
    ]),
  ]);
  const body = el("tbody");
  table.append(body);
  group.order.forEach((model, index) => {
    const row = el("tr", {}, [
      el("td", { class: "num", text: String(index + 1) }),
      el("td", { class: "mono", text: model }),
    ]);
    const until = group.paused?.[model];
    const paused = until && new Date(until) > new Date();
    row.append(
      el("td", {
        text: !group.available?.[model]
          ? t("已下架 / 暂不可用", "rotation.unavailable")
          : paused
            ? t(`暂停至 ${localTime(until)}`, "rotation.pausedUntil", localTime(until))
            : model === group.current
              ? t("当前使用", "rotation.inUse")
              : t("可用", "state.available"),
      }),
    );
    const actions = el("div", { class: "rotation-actions" });
    for (const [text, delta] of [
      [t("上移", "rotation.moveUp"), -1],
      [t("下移", "rotation.moveDown"), 1],
    ]) {
      const button = el("button", {
        type: "button",
        class: "btn btn-sm btn-ghost",
        text,
        "aria-label": `${model} ${text}`,
        disabled: index + delta < 0 || index + delta >= group.order.length,
      });
      button.addEventListener("click", () => {
        [group.order[index], group.order[index + delta]] = [
          group.order[index + delta],
          group.order[index],
        ];
        data.first_output_seconds = Number($("#rotation-timeout").value);
        dirty = true;
        render();
      });
      actions.append(button);
    }
    if (isCustom) {
      const remove = el("button", {
        type: "button",
        class: "btn btn-sm btn-ghost",
        text: t("移除", "rotation.remove"),
        "aria-label": t(`移除 ${model}`, "rotation.removeModel", model),
        disabled: group.order.length <= 1,
      });
      remove.addEventListener("click", () => {
        group.order.splice(index, 1);
        dirty = true;
        render();
      });
      actions.append(remove);
    }
    const pick = el("button", {
      type: "button",
      class: "btn btn-sm btn-ghost",
      text: t("设为当前", "rotation.setCurrent"),
      "aria-label": t(`使用 ${model}`, "rotation.useModel", model),
      disabled: !group.available?.[model],
    });
    pick.addEventListener("click", async () => {
      pick.disabled = true;
      try {
        const result = normalize(
          await api("/api/rotation/current", {
            method: "POST",
            body: JSON.stringify({ group: group.id, model }),
          }),
        );
        // Keep unsaved alias/order edits while refreshing operational state.
        for (const item of data.groups) {
          const live = result.groups.find((entry) => entry.id === item.id);
          if (!live) continue;
          Object.assign(item, { current: live.current, paused: live.paused, switches: live.switches });
        }
        data.persistence_error = result.persistence_error;
        render();
        toast(t("当前模型已切换", "toast.rotation.switched"));
      } catch (error) {
        pick.disabled = false;
        toast(error.message, true);
      }
    });
    actions.append(pick);
    row.append(el("td", {}, [actions]));
    body.append(row);
  });
  if (!group.order.length) emptyRow(body, 4, t("等待添加模型", "rotation.waitingAdd"));
  return el("div", { class: "table-wrap" }, [table]);
}

function addModel(group) {
  const wrap = el("div", { class: "actions rotation-add" });
  const used = new Set(group.order);
  const options = data.available_models.filter((model) => !used.has(model));
  const picker = el("select", { id: `rotation-add-${group.id}` });
  for (const model of options) picker.append(el("option", { value: model, text: model }));
  const button = el("button", {
    type: "button",
    class: "btn btn-sm btn-ghost",
    text: t("添加模型", "rotation.addModel"),
    disabled: !options.length,
  });
  button.addEventListener("click", () => {
    if (!picker.value) return;
    group.order.push(picker.value);
    dirty = true;
    render();
  });
  wrap.append(picker, button);
  return wrap;
}

function renderDetail() {
  const host = $("#rotation-detail");
  host.replaceChildren();
  const group = data.groups.find((item) => item.id === selected);
  if (!group) {
    host.append(
      el("section", { class: "section" }, [
        el("p", {
          class: "section-caption",
          text: t("请选择一个分组查看详情。", "rotation.detail.none"),
        }),
      ]),
    );
    return;
  }
  const aliasInput = el("input", {
    id: `rotation-alias-${group.id}`,
    value: group.alias,
    required: true,
    maxlength: 64,
  });
  aliasInput.addEventListener("input", () => {
    group.alias = aliasInput.value;
    dirty = true;
    renderList();
  });
  const head = el("div", { class: "section-head" }, [
    el("div", {}, [
      el("h2", { class: "section-title", text: label(group) }),
      el("p", { class: "section-caption", text: caption(group) }),
    ]),
  ]);
  if (kind(group) === "custom") {
    const remove = el("button", {
      type: "button",
      class: "btn btn-sm btn-danger",
      text: t("删除分组", "rotation.removeGroup"),
    });
    remove.addEventListener("click", () => {
      data.groups = data.groups.filter((item) => item.id !== group.id);
      dirty = true;
      select(data.groups[0]?.id || "");
    });
    head.append(remove);
  }
  const reasonMap = reasons();
  const recent = el("div", { class: "rotation-switches" }, [
    el("h3", { text: t("最近切换", "rotation.recentSwitches") }),
  ]);
  for (const item of (group.switches || []).slice(-5).reverse()) {
    recent.append(
      el("p", {
        text: `${localTime(item.time)} · ${item.from} → ${item.to} · ${
          reasonMap[item.reason] || item.reason
        }`,
      }),
    );
  }
  if (!group.switches?.length)
    recent.append(el("p", { text: t("尚无切换记录", "rotation.noSwitches") }));

  const section = el("section", { class: "section" }, [
    head,
    el("div", { class: "field" }, [
      el("label", { for: `rotation-alias-${group.id}`, text: t("调用别名", "rotation.aliasLabel") }),
      aliasInput,
    ]),
    el("p", {
      class: "rotation-current",
      text: t(
        `当前模型：${group.current || t("等待可用模型", "rotation.waitingModel")}`,
        "rotation.current",
        group.current || t("等待可用模型", "rotation.waitingModel"),
      ),
    }),
  ]);
  if (kind(group) === "custom") section.append(addModel(group));
  section.append(modelTable(group), recent);
  host.append(section);
}

function renderAttempts() {
  const reasonMap = reasons();
  const attempts = $("#rotation-attempts");
  attempts.replaceChildren();
  for (const item of (data.attempts || []).slice().reverse()) {
    attempts.append(
      el("tr", {}, [
        el("td", { class: "mono", text: localTime(item.time) }),
        el("td", { class: "mono", text: item.request }),
        el("td", { text: item.alias }),
        el("td", { class: "mono", text: item.model }),
        el("td", { class: "num", text: String(item.number) }),
        el("td", { text: reasonMap[item.outcome] || item.outcome }),
      ]),
    );
  }
  if (!data.attempts?.length)
    emptyRow(attempts, 6, t("调用分组别名后显示最近 100 次尝试", "rotation.attemptsEmpty"));
}

async function save(event) {
  event.preventDefault();
  if (!data) return;
  const button = $("#rotation-save");
  button.disabled = true;
  try {
    const body = { first_output_seconds: Number($("#rotation-timeout").value) };
    for (const group of data.groups) {
      const entry = { alias: group.alias.trim(), order: group.order.slice() };
      if (group.id === "sota") body.sota = entry;
      else if (group.id === "sweet") body.sweet = entry;
      else (body.custom ||= []).push(entry);
    }
    data = normalize(await api("/api/rotation", { method: "PUT", body: JSON.stringify(body) }));
    dirty = false;
    if (!data.groups.some((group) => group.id === selected)) selected = data.groups[0]?.id || "";
    render();
    toast(t("模型轮转配置已保存并生效", "toast.rotation.saved"));
  } catch (error) {
    toast(error.message, true);
  } finally {
    button.disabled = false;
  }
}

function addGroup() {
  if (!data) return;
  const aliases = new Set(data.groups.map((group) => group.alias));
  let name = "group-1";
  for (let i = 2; aliases.has(name); i++) name = `group-${i}`;
  const used = new Set(data.groups.flatMap((group) => group.order));
  const first = data.available_models.find((model) => !used.has(model)) || "";
  const group = {
    id: `custom:${name}`,
    alias: name,
    order: first ? [first] : [],
    current: "",
    generation: 0,
    paused: {},
    available: first ? { [first]: true } : {},
    switches: [],
  };
  data.groups.push(group);
  dirty = true;
  if (!first) toast(t("没有可用的空闲模型，请先为该分组添加模型", "toast.rotation.groupEmpty"), true);
  select(group.id);
}

export default {
  id: "rotation",
  title: "模型轮转",
  sub: "共享当前模型、故障切换与尝试记录。",
  init() {
    $("#rotation-refresh").addEventListener("click", () => {
      if (dirty) {
        toast(t("有未保存的修改，请先保存后刷新", "toast.rotation.dirty"), true);
        return;
      }
      load();
    });
    $("#rotation-timeout").addEventListener("input", () => {
      dirty = true;
    });
    $("#rotation-add").addEventListener("click", addGroup);
    $("#rotation-form").addEventListener("submit", save);
    onChange(() => {
      if (data) {
        data.first_output_seconds = Number($("#rotation-timeout").value);
        render();
      }
    });
  },
  show: load,
};
