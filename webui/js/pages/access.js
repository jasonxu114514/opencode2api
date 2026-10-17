// Page: 接入手册 — derived API base, protocol and SDK examples with copy buttons.
import { derivedAPIBase } from "../api.js";
import * as store from "../store.js";
import { $, el, copyText } from "../ui.js";
import { t, onChange } from "../i18n.js";

function codeCard(label, text) {
  const copy = el("button", { type: "button", class: "btn btn-sm btn-ghost", text: t("复制", "common.copy") });
  copy.addEventListener("click", () => copyText(text, t("示例已复制", "toast.example.copied")));
  return el("div", { class: "code-card" }, [
    el("div", { class: "code-head" }, [el("span", { text: label }), copy]),
    el("pre", {}, [el("code", { text })]),
  ]);
}

function render() {
  const config = store.get("config");
  if (!config) return;
  const base = derivedAPIBase();
  const model = $("#access-model")?.value.trim() || "YOUR_MODEL";
  $("#access-base-value").value = base;
  const protocols = $("#access-protocol-examples");
  const sdks = $("#access-sdk-examples");
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
  sdks.append(codeCard("Python · OpenAI SDK", python), codeCard("JavaScript · OpenAI SDK", javascript));
}

export default {
  id: "access",
  title: "接入手册",
  sub: "Chat、Responses、Anthropic 与 SDK 示例。",
  init() {
    store.on("config", render);
    $("#access-model").addEventListener("input", render);
    onChange(render);
  },
  show: render,
};
