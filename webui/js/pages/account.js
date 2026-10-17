// Page: 账号安全 — update admin credentials; all sessions are revoked afterwards.
import { api } from "../api.js";
import { t } from "../i18n.js";
import { $, toast } from "../ui.js";

export default {
  id: "account",
  title: "账号安全",
  sub: "更新账号后撤销全部管理会话。",
  init() {
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
        toast(t("账号已更新，请重新登录", "toast.account.updated"));
        setTimeout(() => location.reload(), 800);
      } catch (error) {
        toast(error.message, true);
      }
    });
  },
};
