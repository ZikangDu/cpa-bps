(() => {
  "use strict";

  const get = id => document.getElementById(id);
  const origin = window.location.origin;
  const marker = "/v0/resource/plugins/cpa-bps/";
  const base = window.location.pathname.split(marker)[0] + "/v8/management";
  const key = get("admin-key");
  const button = get("start");
  const clear = get("reset");
  const authorize = get("authorize");
  const authorizationLink = get("authorization-link");
  const copy = get("copy");
  const callback = get("callback");
  const submit = get("submit");
  const check = get("check");
  const status = get("status");
  const statusLabel = get("authorization-state");
  const providerNote = get("provider-note");
  const ready = document.body.dataset.oauthReady === "true";
  const bpsLogin = document.body.dataset.bpsLogin === "true";
  let session, timer, busy = false, polling = false, parentReady = false, sequence = 0;
  const requests = new Map();

  function notice(label, message, kind = "") {
    statusLabel.textContent = label;
    statusLabel.dataset.kind = kind;
    status.textContent = message;
  }

  function updateControls() {
    button.disabled = busy || !ready;
    clear.disabled = busy || !session;
    copy.disabled = busy || !session;
    callback.disabled = busy || !session;
    submit.disabled = busy || !session;
    check.disabled = busy || polling || !session;
    authorize.setAttribute("aria-disabled", String(!session));
    if (session) {
      authorize.href = session.url;
      authorize.removeAttribute("tabindex");
    } else {
      authorize.removeAttribute("href");
      authorize.tabIndex = -1;
    }
  }

  function clearSession() {
    window.clearTimeout(timer);
    session = undefined;
    authorizationLink.value = "";
    callback.value = "";
  }

  function resetNotice() {
    if (ready) {
      providerNote.textContent = bpsLogin ?
        "使用你准备测试的账号登录，回调提交后由本地 CPA 保存授权。" :
        "授权成功后，CPA 会在凭据管理中保存这个账号。";
      notice("尚未授权", "生成链接后，在授权页完成登录。");
    } else {
      providerNote.textContent = "在 CPA 的插件配置中接入 OAuth 签发方和已注册客户端后，刷新本页。";
      notice("OAuth 未配置", "当前实例还无法生成真实授权链接。", "warning");
    }
  }

  if (window.parent !== window) {
    window.addEventListener("message", event => {
      if (event.source !== window.parent || event.origin !== origin) return;
      if (event.data?.type === "bps-management-ready") {
        parentReady = true;
        key.value = "";
        get("standalone").hidden = true;
      }
      if (event.data?.type !== "bps-management-response") return;
      const pending = requests.get(event.data.id);
      if (!pending) return;
      requests.delete(event.data.id);
      window.clearTimeout(pending.timer);
      if (event.data.ok === true) pending.resolve(event.data.data);
      else pending.reject(new Error("Manager 请求失败，请检查登录状态。"));
    });
    window.parent.postMessage({ type: "bps-management-hello" }, origin);
  }

  async function call(action, params = {}) {
    // CPA accepts OAuth callbacks through its public, state-bound callback route.
    if (parentReady && (action !== "callback" || bpsLogin)) {
      const id = "bps-rebuild-" + (++sequence);
      return new Promise((resolve, reject) => {
        const pending = window.setTimeout(() => {
          requests.delete(id);
          reject(new Error("Manager 请求超时，请查询授权状态。"));
        }, 20000);
        requests.set(id, { resolve, reject, timer: pending });
        window.parent.postMessage({ type: "bps-management-request", id, action, ...params }, origin);
      });
    }
    const admin = key.value.trim();
    if (!admin && action !== "callback") throw new Error("请输入 CPA 管理密钥。");
    const headers = { Accept: "application/json" };
    if (admin && action !== "callback") headers.Authorization = "Bearer " + admin;
    const options = { headers, cache: "no-store", redirect: "error", signal: AbortSignal.timeout(20000) };
    let path = "/oauth/auth-url?provider=cpa-bps";
    if (action === "status") path = "/oauth/status?state=" + encodeURIComponent(params.state);
    if (action === "callback") {
      path = "/oauth/callback";
      options.method = "POST";
      headers["Content-Type"] = "application/json";
      options.body = JSON.stringify({ provider: "cpa-bps", redirect_url: params.callback });
    }
    const response = await fetch(base + path, options);
    if (response.status === 401 || response.status === 403) throw new Error("CPA 管理鉴权未通过。");
    if (!response.ok) throw new Error("CPA 无法完成请求，请检查 OAuth 配置或查询授权状态。");
    return response.json();
  }

  function secureURL(url) {
    const loopback = ["127.0.0.1", "[::1]"].includes(url.hostname);
    return !url.username && !url.password && !url.hash &&
      (url.protocol === "https:" || (loopback && url.protocol === "http:"));
  }

  function queueCheck() {
    window.clearTimeout(timer);
    if (session) timer = window.setTimeout(refresh, 2000);
  }

  async function refresh() {
    if (!session) return;
    if (busy || polling) {
      queueCheck();
      return;
    }
    if (Date.now() >= session.deadline) {
      clearSession();
      updateControls();
      notice("会话已过期", "请重新生成授权链接。", "warning");
      return;
    }
    const current = session;
    polling = true;
    updateControls();
    try {
      const data = await call("status", { state: current.state });
      if (session !== current) return;
      if (data.status === "ok") {
        clearSession();
        key.value = "";
        notice("授权成功", "CPA 已保存账号，可在凭据管理中查看。", "success");
      } else if (data.status === "error") {
        clearSession();
        notice("授权未完成", "请重新生成授权链接。", "error");
      } else {
        queueCheck();
      }
    } catch (error) {
      if (session === current) notice("状态查询失败", error.message, "error");
    } finally {
      polling = false;
      updateControls();
    }
  }

  button.addEventListener("click", async () => {
    if (!ready || busy) return;
    clearSession();
    busy = true;
    notice("正在生成链接", "正在向 CPA 请求本次授权会话。", "active");
    updateControls();
    try {
      const result = await call("start");
      const url = new URL(result.url);
      const redirect = new URL(url.searchParams.get("redirect_uri"));
      if (!secureURL(url) || !secureURL(redirect) || !/^[A-Za-z0-9_-]{32,128}$/.test(result.state) ||
          url.searchParams.getAll("state").length !== 1 || url.searchParams.get("state") !== result.state) {
        throw new Error("CPA 返回了无效的授权会话。");
      }
      session = { state: result.state, url: url.href, redirect, deadline: Date.now() + 10 * 60 * 1000 };
      authorizationLink.value = url.href;
      notice("等待登录", bpsLogin ? "完成登录后，把浏览器中的完整回调地址粘贴到第三步。" : "在授权页完成登录后，返回这里查看结果。", "active");
      queueCheck();
    } catch (error) {
      notice("链接生成失败", error.message, "error");
    } finally {
      busy = false;
      updateControls();
    }
  });

  clear.addEventListener("click", () => {
    clearSession();
    updateControls();
    resetNotice();
  });

  copy.addEventListener("click", async () => {
    if (!session) return;
    try {
      await navigator.clipboard.writeText(session.url);
      notice("链接已复制", "请在浏览器中打开链接并完成登录。", "active");
    } catch {
      authorizationLink.focus();
      authorizationLink.select();
      notice("链接已选中", "使用系统复制快捷键复制授权链接。", "active");
    }
  });

  submit.addEventListener("click", async () => {
    if (!session || busy) return;
    window.clearTimeout(timer);
    busy = true;
    updateControls();
    try {
      const url = new URL(callback.value.trim());
      if (!secureURL(url) || url.origin !== session.redirect.origin || url.pathname !== session.redirect.pathname ||
          url.searchParams.getAll("state").length !== 1 || url.searchParams.get("state") !== session.state ||
          url.searchParams.getAll("code").length !== 1 || !url.searchParams.get("code") || url.searchParams.has("error")) {
        throw new Error("回调地址与这次授权不匹配。");
      }
      const data = await call("callback", { callback: url.href });
      if (data.status !== "ok") throw new Error("CPA 未接受回调，请查询授权状态。");
      callback.value = "";
      notice("正在确认授权", "CPA 已接收回调。", "active");
      queueCheck();
    } catch (error) {
      notice("回调提交失败", error.message, "error");
    } finally {
      busy = false;
      updateControls();
    }
  });
  check.addEventListener("click", () => {
    window.clearTimeout(timer);
    refresh();
  });
  resetNotice();
  updateControls();
})();
