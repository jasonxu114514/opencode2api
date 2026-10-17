// DOM helpers, formatters, clipboard, toast and in-page dialogs.
import { t, dateLocale } from "./i18n.js";

export const $ = (selector, root = document) => root.querySelector(selector);
export const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];

export function el(tag, props = {}, children = []) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (value == null || value === false) continue;
    if (key === "class") node.className = value;
    else if (key === "text") node.textContent = value;
    else if (key === "dataset") Object.assign(node.dataset, value);
    else if (key.startsWith("on") && typeof value === "function")
      node.addEventListener(key.slice(2), value);
    else if (value === true) node.setAttribute(key, "");
    else node.setAttribute(key, String(value));
  }
  for (const child of Array.isArray(children) ? children : [children]) {
    if (child == null || child === false) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

export const formatNumber = (value) => Number(value || 0).toLocaleString();

export function localTime(value) {
  return value ? new Date(value).toLocaleString(dateLocale(), { hour12: false }) : "—";
}

export function timeOnly(value) {
  return value ? new Date(value).toLocaleTimeString(dateLocale(), { hour12: false }) : "—";
}

let toastTimer = null;

export function toast(message, bad = false) {
  const node = $("#toast");
  node.textContent = message;
  node.classList.toggle("bad-toast", bad);
  node.classList.remove("hidden");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => node.classList.add("hidden"), 3600);
}

export async function copyText(text, okMessage = t("已复制", "toast.copied")) {
  try {
    await navigator.clipboard.writeText(text);
    toast(okMessage);
    return true;
  } catch {
    toast(t("浏览器不允许剪贴板访问，请手动选中复制", "toast.copyDenied"), true);
    return false;
  }
}

export function emptyRow(host, columns, message = t("暂无数据", "common.empty")) {
  host.append(el("tr", {}, [el("td", { class: "empty", colspan: columns, text: message })]));
}

export function renderFacts(host, entries) {
  host.replaceChildren();
  for (const [label, value, tone] of entries) {
    host.append(
      el("div", { class: "fact" }, [
        el("div", { class: "fact-label", text: label }),
        el("div", { class: `fact-value ${tone || ""}`.trim(), text: value ?? "—" }),
      ]),
    );
  }
}

// Generic dialog host; onClose fires on every close path so promise-based
// dialogs settle exactly once.
export function openDialog({ title, body = null, size = "", actions = [], onClose }) {
  const previous = document.activeElement;
  const titleId = `dialog-title-${Math.random().toString(36).slice(2, 8)}`;
  const overlay = el("div", { class: "dialog-overlay" });
  const dialog = el("div", {
    class: `dialog ${size}`.trim(),
    role: "dialog",
    "aria-modal": "true",
    "aria-labelledby": titleId,
  });
  dialog.append(el("h2", { id: titleId, class: "dialog-title", text: title }));
  if (body) dialog.append(body);
  let closed = false;
  const close = () => {
    if (closed) return;
    closed = true;
    overlay.remove();
    document.removeEventListener("keydown", onKey, true);
    if (previous instanceof HTMLElement && previous.isConnected) previous.focus();
    onClose?.();
  };
  const onKey = (event) => {
    if (event.key === "Escape") {
      event.stopPropagation();
      close();
    }
  };
  if (actions.length) {
    const row = el("div", { class: "dialog-actions" });
    for (const action of actions) {
      row.append(
        el("button", {
          type: "button",
          class: `btn btn-sm ${action.kind || "btn-ghost"}`,
          text: action.label,
          onclick: () => (action.onClick ? action.onClick(close) : close()),
        }),
      );
    }
    dialog.append(row);
  }
  overlay.append(dialog);
  overlay.addEventListener("mousedown", (event) => {
    if (event.target === overlay) close();
  });
  document.addEventListener("keydown", onKey, true);
  $("#dialog-host").append(overlay);
  const target = dialog.querySelector("input, select, textarea") || dialog.querySelector(".dialog-actions .btn");
  target?.focus();
  return { close, dialog };
}

export function confirmDialog({
  title,
  message = "",
  confirmLabel = t("确定", "common.confirm"),
  danger = false,
}) {
  return new Promise((resolve) => {
    let settled = false;
    const finish = (value, close) => {
      if (!settled) {
        settled = true;
        resolve(value);
      }
      close?.();
    };
    openDialog({
      title,
      body: message ? el("p", { class: "dialog-text", text: message }) : null,
      onClose: () => finish(false),
      actions: [
        { label: t("取消", "common.cancel"), onClick: (close) => finish(false, close) },
        {
          label: confirmLabel,
          kind: danger ? "btn-danger" : "btn-primary",
          onClick: (close) => finish(true, close),
        },
      ],
    });
  });
}

export function promptDialog({
  title,
  label,
  value = "",
  placeholder = "",
  confirmLabel = t("确定", "common.confirm"),
}) {
  return new Promise((resolve) => {
    let settled = false;
    const input = el("input", { value, placeholder, maxlength: 80 });
    const finish = (result, close) => {
      if (!settled) {
        settled = true;
        resolve(result);
      }
      close?.();
    };
    input.addEventListener("keydown", (event) => {
      if (event.key === "Enter") {
        event.preventDefault();
        finish(input.value, dialog.close);
      }
    });
    const dialog = openDialog({
      title,
      body: el("div", { class: "field" }, [el("label", { text: label }), input]),
      onClose: () => finish(null),
      actions: [
        { label: t("取消", "common.cancel"), onClick: (close) => finish(null, close) },
        { label: confirmLabel, kind: "btn-primary", onClick: (close) => finish(input.value, close) },
      ],
    });
  });
}

// Tab strip wiring: [data-tab] buttons toggle [data-panel] panels and report
// the active id.
export function bindTabs(page, onSwitch) {
  const buttons = $$(".tabs .tab", page);
  const panels = $$("[data-panel]", page);
  const activate = (id) => {
    for (const button of buttons) {
      const active = button.dataset.tab === id;
      button.classList.toggle("active", active);
      button.setAttribute("aria-selected", active ? "true" : "false");
    }
    for (const panel of panels) panel.classList.toggle("hidden", panel.dataset.panel !== id);
    onSwitch?.(id);
  };
  for (const button of buttons) button.addEventListener("click", () => activate(button.dataset.tab));
  return { activate };
}
