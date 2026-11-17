// fetch wrapper: JSON handling, CSRF for writes, 401 routed to the login view.
import * as store from "./store.js";
import { t } from "./i18n.js";

let csrf = "";
let sessionVersion = 0;
let onUnauthorized = null;

export function setCsrf(value) {
  csrf = value || "";
  sessionVersion++;
}

export function clearCsrf() {
  csrf = "";
  sessionVersion++;
}

export function hasSession() {
  return csrf !== "";
}

export function setUnauthorizedHandler(handler) {
  onUnauthorized = handler;
}

export async function api(path, options = {}) {
  const requestVersion = sessionVersion;
  const headers = { ...(options.headers || {}), Accept: "application/json" };
  if (options.body) headers["Content-Type"] = "application/json";
  if (csrf && !["GET", "HEAD"].includes((options.method || "GET").toUpperCase()))
    headers["X-CSRF-Token"] = csrf;
  const response = await fetch(path, { ...options, headers });
  const data = response.status === 204 ? null : await response.json().catch(() => ({}));
  // A response from a previous login must not reveal data or invalidate the
  // new session after the operator logs out and signs in again.
  if (requestVersion !== sessionVersion)
    throw new DOMException("The management session changed", "AbortError");
  if (response.status === 204) return null;
  if (!response.ok) {
    // The login endpoint answers 401 for bad credentials: surface its message
    // instead of treating the attempt as an expired session.
    if (response.status === 401 && path !== "/api/auth/login") {
      if (onUnauthorized) onUnauthorized();
      throw new Error(t("登录已失效", "error.session"));
    }
    throw new Error(data.error?.message || `HTTP ${response.status}`);
  }
  return data;
}

// Client-facing API base derived from the configured listen address.
export function derivedAPIBase() {
  const config = store.get("config");
  const listen = config?.effective?.listen || config?.listen || ":8080";
  const match = listen.match(/^\[.*\]:(\d+)$/) || listen.match(/:(\d+)$/);
  const port = match?.[1] || "8080";
  return `${location.protocol}//${location.hostname}:${port}`;
}
