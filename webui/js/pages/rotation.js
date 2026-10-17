// Page: 模型轮转 — sota/sweet alias groups, ordering, manual switch and attempts.
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

// The persisted rotation state serializes nil maps and slices as JSON null;
// normalize them before anything renders against them.
function normalize(value) {
  for (const group of value.groups || []) {
    group.order = group.order || [];
    group.available = group.available || {};
    group.paused = group.paused || {};
    group.switches = group.switches || [];
  }
  value.attempts = value.attempts || [];
  return value;
}

async function load() {
  try {
    const value = await api("/api/rotation");
    if (!hasSession()) return;
    data = normalize(value);
    dirty = false;
    render();
  } catch (error) {
    toast(error.message, true);
  }
}

function renderGroup(group) {
  const reasonMap = reasons();
  const aliasInput = el("input", {
    id: `rotation-alias-${group.id}`,
    value: group.alias,
    required: true,
    maxlength: 64,
  });
  aliasInput.addEventListener("input", () => {
    group.alias = aliasInput.value;
    dirty = true;
  });
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
    const until = group.paused[model];
    const paused = until && new Date(until) > new Date();
    row.append(
      el("td", {
        text: !group.available[model]
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
    const select = el("button", {
      type: "button",
      class: "btn btn-sm btn-ghost",
      text: t("设为当前", "rotation.setCurrent"),
      "aria-label": t(`使用 ${model}`, "rotation.useModel", model),
      disabled: !group.available[model],
    });
    select.addEventListener("click", async () => {
      select.disabled = true;
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
          Object.assign(item, { current: live.current, paused: live.paused, switches: live.switches });
        }
        data.persistence_error = result.persistence_error;
        render();
        toast(t("当前模型已切换", "toast.rotation.switched"));
      } catch (error) {
        select.disabled = false;
        toast(error.message, true);
      }
    });
    actions.append(select);
    row.append(el("td", {}, [actions]));
    body.append(row);
  });
  if (!group.order.length) emptyRow(body, 4, t("等待 Go 模型目录", "rotation.waitingCatalog"));
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
  return el("section", { class: "section rotation-group" }, [
    el("h2", { class: "section-title", text: group.id === "sota" ? "SOTA" : t("甜点", "rotation.sweet") }),
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
    el("div", { class: "table-wrap" }, [table]),
    recent,
  ]);
}

function render() {
  if (!data) return;
  const reasonMap = reasons();
  $("#rotation-status").textContent =
    data.persistence_error ||
    t("状态已更新 · 两组独立轮转，所有调用方共享当前位置。", "rotation.status");
  $("#rotation-timeout").value = data.first_output_seconds;
  const host = $("#rotation-groups");
  host.replaceChildren();
  for (const group of data.groups) host.append(renderGroup(group));
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
    for (const group of data.groups) body[group.id] = { alias: group.alias.trim(), order: group.order };
    data = normalize(await api("/api/rotation", { method: "PUT", body: JSON.stringify(body) }));
    dirty = false;
    render();
    toast(t("模型轮转配置已保存并生效", "toast.rotation.saved"));
  } catch (error) {
    toast(error.message, true);
  } finally {
    button.disabled = false;
  }
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
