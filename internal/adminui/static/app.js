const $ = selector => document.querySelector(selector);
const $$ = selector => [...document.querySelectorAll(selector)];
const state = {
  csrf: "", user: null, apps: [], currentApp: "", preview: null,
  busy: false, offerCreationAllowed: false, bootstrapToken: "", enrollmentToken: ""
};

const durationLabels = {
  THREE_DAYS: "3 天", ONE_WEEK: "1 周", TWO_WEEKS: "2 周", ONE_MONTH: "1 个月",
  TWO_MONTHS: "2 个月", THREE_MONTHS: "3 个月", SIX_MONTHS: "6 个月", ONE_YEAR: "1 年"
};
const eligibilityLabels = { NEW: "新用户", EXISTING: "当前订阅用户", EXPIRED: "已过期用户" };
const roleLabels = { admin: "管理员", operator: "运营" };
const outcomeLabels = { denied: "已拒绝", failed: "失败" };
const actionLabels = {
  "admin.bootstrap_link.create": "签发首次设置链接", "admin.bootstrap": "创建首位管理员",
  "admin.login": "登录", "admin.logout": "退出",
  "admin.reauthenticate": "再次验证", "admin.user.enroll": "接受后台邀请",
  "admin.passkey.add": "添加通行密钥", "admin.passkey.remove": "移除通行密钥",
  "admin.passkey.recover": "完成通行密钥恢复", "admin.passkey.recovery_invite": "签发恢复邀请",
  "admin.invitation.create": "创建人员邀请", "admin.invitation.revoke": "撤销邀请",
  "admin.user.update": "更新后台人员", "offer.create": "创建 Offer",
  "ai.draft.create": "保存 AI 配置草稿", "ai.config.publish": "发布 AI 配置",
  "offer.deactivate": "停用 Offer", "offer_codes.custom_create": "创建自定义码池",
  "offer_codes.batch_create": "创建一次性码池", "offer_codes.download": "下载一次性码"
};

function notice(message, error = false) {
  const element = $("#notice");
  element.textContent = message;
  element.classList.remove("hidden", "error");
  if (error) element.classList.add("error");
}

function clearNotice() { $("#notice").classList.add("hidden"); }
function uuid() { return crypto.randomUUID?.() || `${Date.now()}-${crypto.getRandomValues(new Uint32Array(2)).join("-")}`; }
function escapeHTML(value) {
  return String(value ?? "").replace(/[&<>"']/g, character => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"
  })[character]);
}
function formatTime(value) { return value ? new Date(value).toLocaleString("zh-CN", { dateStyle: "medium", timeStyle: "short" }) : "—"; }

async function api(path, { method = "GET", body, csrfRequired = false, idempotent = false, idempotencyKey } = {}) {
  const headers = { Accept: "application/json" };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (csrfRequired) headers["X-Admin-CSRF"] = state.csrf;
  if (idempotencyKey || idempotent) headers["Idempotency-Key"] = idempotencyKey || uuid();
  const response = await fetch(path, {
    method, headers, body: body === undefined ? undefined : JSON.stringify(body), credentials: "same-origin"
  });
  let data = {};
  if (response.status !== 204) data = await response.json().catch(() => ({}));
  if (!response.ok) {
    const error = new Error(data.error?.message || "请求失败，请稍后重试");
    error.code = data.error?.code;
    error.status = response.status;
    throw error;
  }
  return data;
}

const b64 = {
  decode(value) {
    value = value.replace(/-/g, "+").replace(/_/g, "/");
    const raw = atob(value.padEnd(Math.ceil(value.length / 4) * 4, "="));
    return Uint8Array.from(raw, character => character.charCodeAt(0)).buffer;
  },
  encode(value) {
    return btoa(String.fromCharCode(...new Uint8Array(value)))
      .replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
  }
};

function createOptions(options) {
  const result = structuredClone(options);
  result.challenge = b64.decode(result.challenge);
  result.user.id = b64.decode(result.user.id);
  result.excludeCredentials = (result.excludeCredentials || []).map(value => ({ ...value, id: b64.decode(value.id) }));
  return result;
}

function getOptions(options) {
  const result = structuredClone(options);
  result.challenge = b64.decode(result.challenge);
  result.allowCredentials = (result.allowCredentials || []).map(value => ({ ...value, id: b64.decode(value.id) }));
  return result;
}

function credentialJSON(credential) {
  const response = { clientDataJSON: b64.encode(credential.response.clientDataJSON) };
  if (credential.response.attestationObject) response.attestationObject = b64.encode(credential.response.attestationObject);
  if (credential.response.authenticatorData) response.authenticatorData = b64.encode(credential.response.authenticatorData);
  if (credential.response.signature) response.signature = b64.encode(credential.response.signature);
  if (credential.response.userHandle) response.userHandle = b64.encode(credential.response.userHandle);
  if (credential.response.getTransports) response.transports = credential.response.getTransports();
  return {
    id: credential.id, rawId: b64.encode(credential.rawId), type: credential.type, response,
    clientExtensionResults: credential.getClientExtensionResults(),
    authenticatorAttachment: credential.authenticatorAttachment
  };
}

async function ceremony(beginPath, finishPath, beginBody = {}) {
  if (!window.PublicKeyCredential || !navigator.credentials) throw new Error("当前浏览器不支持通行密钥");
  const begin = await api(beginPath, { method: "POST", body: beginBody, csrfRequired: !!state.csrf });
  const creation = !!begin.publicKey.user;
  const credential = creation
    ? await navigator.credentials.create({ publicKey: createOptions(begin.publicKey) })
    : await navigator.credentials.get({ publicKey: getOptions(begin.publicKey) });
  if (!credential) throw new Error("没有完成通行密钥验证");
  const headers = { "Content-Type": "application/json", "X-Admin-Ceremony-ID": begin.ceremonyID };
  if (state.csrf) headers["X-Admin-CSRF"] = state.csrf;
  const response = await fetch(finishPath, {
    method: "POST", headers, credentials: "same-origin", body: JSON.stringify(credentialJSON(credential))
  });
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(data.error?.message || "通行密钥验证失败");
  return data;
}

async function run(task) {
  if (state.busy) return;
  state.busy = true;
  clearNotice();
  const previousButtonState = new Map($$('button').map(button => [button, button.disabled]));
  previousButtonState.forEach((_, button) => button.disabled = true);
  try { await task(); }
  catch (error) { notice(error.message || "操作失败", true); }
  finally {
    state.busy = false;
    previousButtonState.forEach((disabled, button) => {
      if (button.isConnected) button.disabled = disabled;
    });
    syncPersistentControls();
  }
}

function syncPersistentControls() {
  const newOffer = $("#new-offer");
  if (newOffer) newOffer.disabled = state.busy || !state.offerCreationAllowed;
}

function appPath(path) { return `/api/v1/apps/${encodeURIComponent(state.currentApp)}${path}`; }

async function loadSession() {
  try {
    const session = await api("/api/v1/session");
    state.csrf = session.csrfToken;
    state.user = session.user;
    $("#auth").classList.add("hidden");
    $("#workspace").classList.remove("hidden");
    $("#account-actions").classList.remove("hidden");
    $("#account-name").textContent = state.user.displayName;
    $("#account-role").textContent = roleLabels[state.user.role] || state.user.role;
    $("#people-tab").classList.toggle("hidden", state.user.role !== "admin");
    $("#ai-tab").classList.toggle("hidden", state.user.role !== "admin");
    await loadApps();
    await loadOffers();
    return true;
  } catch (error) {
    state.csrf = "";
    state.user = null;
    $("#auth").classList.remove("hidden");
    $("#workspace").classList.add("hidden");
    $("#account-actions").classList.add("hidden");
    if (error.status >= 500) notice(error.message || "管理服务暂时不可用", true);
    return false;
  }
}

async function loadApps() {
  const data = await api("/api/v1/apps");
  state.apps = Array.isArray(data.apps) ? data.apps : [];
  const picker = $("#app-picker");
  const stored = localStorage.getItem("admin-app");
  state.currentApp = state.apps.some(app => app.id === stored) ? stored : state.apps[0]?.id || "";
  picker.innerHTML = state.apps.map(app => `<option value="${escapeHTML(app.id)}">${escapeHTML(app.displayName)}</option>`).join("");
  picker.value = state.currentApp;
  picker.onchange = async () => {
    state.currentApp = picker.value;
    localStorage.setItem("admin-app", state.currentApp);
    await run(loadOffers);
  };
  renderInviteApps();
}

async function loadOffers() {
  if (!state.currentApp) {
    state.offerCreationAllowed = false;
    syncPersistentControls();
    $("#offers").innerHTML = '<article class="card empty">当前账号没有可管理的 App。</article>';
    return;
  }
  const [data, metricData] = await Promise.all([
    api(appPath("/offers")), api(appPath("/metrics/offers")).catch(() => ({ metrics: [] }))
  ]);
  const offers = Array.isArray(data.offers) ? data.offers : [];
  const production = (Array.isArray(metricData.metrics) ? metricData.metrics : [])
    .filter(metric => metric.environment === "production");
  const metrics = new Map(production.map(metric => [metric.offerIdentifier, metric]));
  $("#redemption-count").textContent = production.reduce((sum, metric) => sum + metric.redemptions, 0);
  $("#active-count").textContent = data.activeCount;
  $("#active-limit").textContent = data.activeLimit;
  $("#synced-at").textContent = formatTime(data.syncedAt);
  state.offerCreationAllowed = !!data.writesEnabled && data.activeCount < data.activeLimit;
  syncPersistentControls();
  $("#offers").innerHTML = offers.length
    ? offers.map(offer => offerCard(offer, metrics.get(offer.id), data.writesEnabled)).join("")
    : '<article class="card empty">还没有 Offer。创建后再为它生成邀请码池。</article>';
  $$('[data-codes]').forEach(button => button.onclick = () => openCodes(button.dataset.codes));
  $$('[data-deactivate]').forEach(button => button.onclick = () => deactivate(button.dataset.deactivate, button.dataset.name));
}

function offerCard(offer, metric, writesEnabled) {
  const chips = [durationLabels[offer.duration] || offer.duration, offer.autoRenewEnabled ? "到期自动续订" : "到期自动结束",
    ...(offer.customerEligibilities || []).map(value => eligibilityLabels[value] || value), offer.active ? "启用中" : "已停用"];
  const actions = offer.active && writesEnabled
    ? `<button class="secondary" data-codes="${escapeHTML(offer.id)}">创建码池</button><button class="quiet" data-deactivate="${escapeHTML(offer.id)}" data-name="${escapeHTML(offer.name)}">停用</button>` : "";
  return `<article class="card offer"><div><h3>${escapeHTML(offer.name)}</h3><div class="chips">${chips.map((value, index) => `<span class="chip ${!offer.active && index === chips.length - 1 ? "inactive" : ""}">${escapeHTML(value)}</span>`).join("")}</div><p class="muted"><small>正式码 ${offer.productionCodeCount || 0} · 沙盒码 ${offer.sandboxCodeCount || 0} · 已兑换 ${metric?.redemptions || 0} 次</small></p></div><div class="offer-actions">${actions}</div></article>`;
}

async function reauthenticate() {
  await ceremony("/api/v1/auth/reauth/options", "/api/v1/auth/reauth/finish");
}

async function deactivate(id, name) {
  if (!confirm(`停用“${name}”？\n\n停用后不能重新启用，未使用的邀请码将无法再兑换。`)) return;
  await run(async () => {
    await reauthenticate();
    await api(appPath(`/offers/${encodeURIComponent(id)}/deactivate`), { method: "POST", body: {}, csrfRequired: true, idempotent: true });
    notice("Offer 已停用");
    await loadOffers();
  });
}

async function openCodes(id) {
  const form = $("#codes-form");
  form.reset();
  form.offerID.value = id;
  form.expirationDate.value = new Date(Date.now() + 86400000 * 30).toISOString().slice(0, 10);
  toggleCodeKind();
  $("#codes-dialog").showModal();
  const target = $("#code-pools");
  target.textContent = "正在读取现有码池…";
  try {
    const data = await api(appPath(`/offers/${encodeURIComponent(id)}/code-pools`));
    const pools = Array.isArray(data.codePools) ? data.codePools : [];
    target.innerHTML = pools.length ? pools.map(poolRow).join("") : "还没有码池";
    $$('[data-download-batch]').forEach(button => button.onclick = () => run(() => downloadBatch(button.dataset.downloadBatch)));
  } catch (error) { target.textContent = error.message; }
}

function poolRow(pool) {
  const title = pool.kind === "custom" ? pool.code : `Apple 一次性码 · ${pool.environment === "SANDBOX" ? "沙盒" : "正式"}`;
  return `<div class="pool"><span><strong>${escapeHTML(title)}</strong><br>${pool.numberOfCodes} 次 · 到期 ${escapeHTML(pool.expirationDate || "未设置")}</span>${pool.kind === "oneTime" ? `<button type="button" class="secondary" data-download-batch="${escapeHTML(pool.id)}">下载 CSV</button>` : ""}</div>`;
}

async function downloadBatch(id, shouldReauthenticate = true) {
  if (shouldReauthenticate) await reauthenticate();
  const response = await fetch(appPath(`/one-time-code-batches/${encodeURIComponent(id)}/download`), {
    method: "POST", headers: { "X-Admin-CSRF": state.csrf }, credentials: "same-origin"
  });
  if (!response.ok) {
    const data = await response.json().catch(() => ({}));
    throw new Error(data.error?.message || "无法下载一次性码");
  }
  const blob = await response.blob();
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = `offer-codes-${id}.csv`;
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
  notice("一次性码 CSV 已下载，请妥善保管");
}

function toggleCodeKind() {
  const oneTime = $("#codes-form").kind.value === "oneTime";
  $("#custom-code-label").classList.toggle("hidden", oneTime);
  $("#environment-label").classList.toggle("hidden", !oneTime);
  $("#codes-form").code.required = !oneTime;
}

async function loadPasskeys() {
  const data = await api("/api/v1/security/passkeys");
  const passkeys = Array.isArray(data.passkeys) ? data.passkeys : [];
  $("#passkeys").innerHTML = passkeys.length ? passkeys.map(passkey => `
    <article class="card passkey"><div><h3>${escapeHTML(passkey.displayName)}</h3><p class="muted">添加于 ${formatTime(passkey.createdAt)}${passkey.lastUsedAt ? ` · 最近使用 ${formatTime(passkey.lastUsedAt)}` : ""}</p></div><button class="quiet" data-remove-passkey="${escapeHTML(passkey.id)}" ${passkeys.length === 1 ? 'disabled title="至少保留一枚通行密钥"' : ""}>移除</button></article>`).join("")
    : '<article class="card empty">没有可用的通行密钥。</article>';
  $$('[data-remove-passkey]').forEach(button => button.onclick = () => removePasskey(button.dataset.removePasskey));
}

async function removePasskey(id) {
  if (!confirm("移除这枚通行密钥？\n\n安全信息变化后，当前设备也需要重新登录。")) return;
  await run(async () => {
    await reauthenticate();
    await api(`/api/v1/security/passkeys/${encodeURIComponent(id)}`, { method: "DELETE", csrfRequired: true });
    location.reload();
  });
}

function renderInviteApps() {
  $("#invite-apps").innerHTML = `<legend>可管理的 App</legend>${state.apps.map(app => `<label class="check"><input type="checkbox" name="appIDs" value="${escapeHTML(app.id)}"> ${escapeHTML(app.displayName)}</label>`).join("")}`;
}

async function loadPeople() {
  if (state.user.role !== "admin") return;
  const data = await api("/api/v1/admin/users");
  const users = Array.isArray(data.users) ? data.users : [];
  const invitations = Array.isArray(data.invitations) ? data.invitations : [];
  $("#users").innerHTML = users.length ? users.map(userCard).join("") : '<article class="card empty">还没有后台人员。</article>';
  $("#invitations").innerHTML = invitations.length ? invitations.map(invitationCard).join("") : '<article class="card empty">没有待处理邀请。</article>';
  $$('[data-save-user]').forEach(button => button.onclick = () => saveUser(button.dataset.saveUser));
  $$('[data-recover-user]').forEach(button => button.onclick = () => recoverUser(button.dataset.recoverUser));
  $$('[data-revoke-invitation]').forEach(button => {
    button.onclick = () => revokeInvitation(button.dataset.revokeInvitation, button.dataset.invitationKind);
  });
  $$('[data-user-role]').forEach(select => select.onchange = () => updateUserAppControls(select.dataset.userRole));
  $$('[data-user-role]').forEach(select => updateUserAppControls(select.dataset.userRole));
}

function userCard(user) {
  const userAppIDs = Array.isArray(user.appIDs) ? user.appIDs : [];
  const appChecks = state.apps.map(app => `<label><input type="checkbox" data-user-app="${escapeHTML(user.id)}" value="${escapeHTML(app.id)}" ${userAppIDs.includes(app.id) ? "checked" : ""}>${escapeHTML(app.displayName)}</label>`).join("");
  const self = user.id === state.user.id;
  return `<article class="card person" data-user-card="${escapeHTML(user.id)}" data-user-name="${escapeHTML(user.displayName)}"><div><h3>${escapeHTML(user.displayName)}${self ? "（我）" : ""}</h3><div class="chips"><span class="chip">${roleLabels[user.role]}</span><span class="chip ${user.status === "disabled" ? "inactive" : ""}">${user.status === "active" ? "已启用" : "已停用"}</span><span class="chip">${user.credentialCount} 枚通行密钥</span></div><div class="person-controls"><label>角色<select data-user-role="${escapeHTML(user.id)}"><option value="operator" ${user.role === "operator" ? "selected" : ""}>运营</option><option value="admin" ${user.role === "admin" ? "selected" : ""}>管理员</option></select></label><label>状态<select data-user-status="${escapeHTML(user.id)}"><option value="active" ${user.status === "active" ? "selected" : ""}>启用</option><option value="disabled" ${user.status === "disabled" ? "selected" : ""}>停用</option></select></label></div><div class="app-checks" data-user-apps="${escapeHTML(user.id)}">${appChecks}</div></div><div class="row-actions">${!self && user.status === "active" ? `<button class="quiet" data-recover-user="${escapeHTML(user.id)}">恢复登录</button>` : ""}<button class="secondary" data-save-user="${escapeHTML(user.id)}">保存</button></div></article>`;
}

function updateUserAppControls(userID) {
  const role = document.querySelector(`[data-user-role="${CSS.escape(userID)}"]`)?.value;
  const container = document.querySelector(`[data-user-apps="${CSS.escape(userID)}"]`);
  if (container) container.classList.toggle("hidden", role === "admin");
}

function invitationCard(invitation) {
  const kind = invitation.kind === "recovery" ? "恢复邀请" : `${roleLabels[invitation.role]}邀请`;
  const appIDs = Array.isArray(invitation.appIDs) ? invitation.appIDs : [];
  const appNames = appIDs.map(appName).map(escapeHTML).join("、");
  return `<article class="card invitation"><div><strong>${escapeHTML(invitation.displayName)}</strong><p class="muted">${escapeHTML(kind)}${appNames ? ` · ${appNames}` : ""}<br><time>有效至 ${formatTime(invitation.expiresAt)}</time></p></div><button class="quiet" data-invitation-kind="${escapeHTML(invitation.kind)}" data-revoke-invitation="${escapeHTML(invitation.id)}">撤销</button></article>`;
}

function appName(appID) { return state.apps.find(app => app.id === appID)?.displayName || appID; }

async function saveUser(userID) {
  const source = document.querySelector(`[data-user-card="${CSS.escape(userID)}"]`);
  const originalName = source.dataset.userName;
  const role = source.querySelector(`[data-user-role]`).value;
  const status = source.querySelector(`[data-user-status]`).value;
  const appIDs = [...source.querySelectorAll(`[data-user-app]:checked`)].map(input => input.value);
  if (role === "operator" && appIDs.length === 0) { notice("运营人员至少需要分配一个 App", true); return; }
  if (!confirm(`保存“${originalName}”的角色和权限？\n\n如果角色、状态或 App 范围发生变化，该人员的现有后台会话会立即失效。`)) return;
  await run(async () => {
    await reauthenticate();
    const data = await api(`/api/v1/admin/users/${encodeURIComponent(userID)}`, {
      method: "PATCH", body: { displayName: originalName, role, status, appIDs }, csrfRequired: true
    });
    if (data.sessionInvalidated) location.reload();
    else { notice("人员权限已更新"); await loadPeople(); }
  });
}

async function recoverUser(userID) {
  if (!confirm("为这个人签发恢复邀请？\n\n签发后，其现有通行密钥和所有后台会话会立即失效。")) return;
  await run(async () => {
    await reauthenticate();
    const data = await api(`/api/v1/admin/users/${encodeURIComponent(userID)}/recovery-invitations`, {
      method: "POST", body: {}, csrfRequired: true
    });
    showEnrollmentLink(data.enrollmentURL);
    await loadPeople();
  });
}

async function revokeInvitation(invitationID, kind) {
  const warning = kind === "recovery"
    ? "撤销这个恢复邀请？\n\n旧通行密钥不会因此恢复；需要登录时必须再签发新的恢复邀请。"
    : "撤销这个邀请？";
  if (!confirm(warning)) return;
  await run(async () => {
    await reauthenticate();
    await api(`/api/v1/admin/invitations/${encodeURIComponent(invitationID)}`, { method: "DELETE", csrfRequired: true });
    notice("邀请已撤销");
    await loadPeople();
  });
}

function showEnrollmentLink(url) {
  $("#enrollment-link").value = url;
  $("#link-dialog").showModal();
}

async function loadAudit() {
  const data = await api("/api/v1/admin/audit");
  $("#audit-scope").textContent = data.scope === "all" ? "显示全部后台人员的安全和业务操作。" : "运营人员只能看到自己的操作。";
  const events = Array.isArray(data.events) ? data.events : [];
  $("#audit-events").innerHTML = events.length ? events.map(event => `
    <article class="card audit-event"><div><strong>${escapeHTML(actionLabels[event.action] || event.action)}</strong><p class="muted">${escapeHTML(event.displayName || "系统")}${event.appID ? ` · ${escapeHTML(appName(event.appID))}` : ""}${outcomeLabels[event.outcome] ? ` · ${outcomeLabels[event.outcome]}` : ""}</p></div><time>${formatTime(event.createdAt)}</time></article>`).join("")
    : '<article class="card empty">还没有操作记录。</article>';
}

async function showSection(name) {
  $$('.workspace-section').forEach(section => section.classList.add("hidden"));
  $$('.tab').forEach(tab => {
    const active = tab.dataset.section === name;
    tab.classList.toggle("active", active);
    tab.setAttribute("aria-selected", String(active));
    tab.tabIndex = active ? 0 : -1;
  });
  $(`#${name}-section`).classList.remove("hidden");
  if (name === "ai") await loadAI();
  if (name === "security") await loadPasskeys();
  if (name === "people") await loadPeople();
  if (name === "audit") await loadAudit();
}

$$('[data-close]').forEach(button => button.onclick = () => button.closest("dialog").close());
$$('.tab').forEach(button => {
  button.onclick = () => run(() => showSection(button.dataset.section));
  button.onkeydown = event => {
    const tabs = $$('.tab').filter(tab => !tab.classList.contains("hidden"));
    const current = tabs.indexOf(button);
    let next = current;
    if (event.key === "ArrowRight") next = (current + 1) % tabs.length;
    else if (event.key === "ArrowLeft") next = (current - 1 + tabs.length) % tabs.length;
    else if (event.key === "Home") next = 0;
    else if (event.key === "End") next = tabs.length - 1;
    else return;
    event.preventDefault();
    tabs[next].focus();
    tabs[next].click();
  };
});

$("#login").onclick = () => run(async () => {
  const data = await ceremony("/api/v1/auth/login/options", "/api/v1/auth/login/finish");
  state.csrf = data.csrfToken;
  await loadSession();
});

$("#logout").onclick = () => run(async () => {
  await api("/api/v1/auth/logout", { method: "POST", csrfRequired: true });
  state.csrf = "";
  location.reload();
});

$("#setup").onclick = () => run(async () => {
  const data = await ceremony("/api/v1/auth/setup/options", "/api/v1/auth/setup/finish", {
    bootstrapToken: state.bootstrapToken, displayName: $("#display-name").value
  });
  state.bootstrapToken = "";
  history.replaceState(null, "", "/");
  state.csrf = data.csrfToken;
  if (await loadSession()) notice("首位管理员已创建。请在通行密钥页面添加备用凭证。");
});

$("#enroll").onclick = () => run(async () => {
  const data = await ceremony("/api/v1/auth/enroll/options", "/api/v1/auth/enroll/finish", {
    token: state.enrollmentToken
  });
  state.enrollmentToken = "";
  history.replaceState(null, "", "/");
  state.csrf = data.csrfToken;
  if (await loadSession()) notice("通行密钥已登记。");
});

$("#add-passkey").onclick = () => {
  const displayName = prompt("给这枚通行密钥起一个名称", "备用通行密钥")?.trim();
  if (!displayName) return;
  run(async () => {
    await reauthenticate();
    await ceremony("/api/v1/security/passkeys/options", "/api/v1/security/passkeys/finish", { displayName });
    notice("新的通行密钥已登记");
    await loadPasskeys();
  });
};

$("#invite-user").onclick = () => {
  $("#invite-form").reset();
  renderInviteApps();
  toggleInviteRole();
  $("#invite-dialog").showModal();
};

function toggleInviteRole() {
  $("#invite-apps").classList.toggle("hidden", $("#invite-form").role.value === "admin");
}

$("#invite-form").role.onchange = toggleInviteRole;
$("#invite-form").onsubmit = event => {
  event.preventDefault();
  run(async () => {
    const form = new FormData(event.currentTarget);
    const role = form.get("role");
    const appIDs = form.getAll("appIDs");
    if (role === "operator" && appIDs.length === 0) throw new Error("运营人员至少需要分配一个 App");
    await reauthenticate();
    const data = await api("/api/v1/admin/invitations", {
      method: "POST", body: { displayName: form.get("displayName").trim(), role, appIDs }, csrfRequired: true
    });
    $("#invite-dialog").close();
    showEnrollmentLink(data.enrollmentURL);
    await loadPeople();
  });
};

$("#copy-link").onclick = () => navigator.clipboard.writeText($("#enrollment-link").value)
  .then(() => notice("邀请链接已复制"))
  .catch(() => notice("无法自动复制，请手动复制链接", true));
$("#refresh-audit").onclick = () => run(loadAudit);
$("#new-offer").onclick = () => $("#offer-dialog").showModal();

$("#offer-form").onsubmit = event => {
  event.preventDefault();
  run(async () => {
    const form = new FormData(event.currentTarget);
    const draft = {
      name: form.get("name").trim(), duration: form.get("duration"),
      customerEligibilities: form.getAll("eligibility"), autoRenewEnabled: form.has("autoRenewEnabled")
    };
    state.preview = await api(appPath("/offers/preview"), { method: "POST", body: draft, csrfRequired: true });
    $("#preview").innerHTML = `<p><strong>${escapeHTML(draft.name)}</strong></p><p>免费 ${durationLabels[draft.duration]} · ${draft.autoRenewEnabled ? "之后自动续订" : "之后自动结束"}</p><p>${draft.customerEligibilities.map(value => eligibilityLabels[value]).join("、")}</p>`;
    $("#offer-dialog").close();
    $("#confirm-dialog").showModal();
  });
};

$("#confirm-create").onclick = () => run(async () => {
  await reauthenticate();
  await api(appPath("/offers"), {
    method: "POST", body: { draft: state.preview.draft, previewToken: state.preview.previewToken },
    csrfRequired: true, idempotent: true
  });
  $("#confirm-dialog").close();
  notice("Offer 已创建，现在可以为它创建邀请码池");
  await loadOffers();
});

$("#codes-form").kind.onchange = toggleCodeKind;
$("#codes-form").onsubmit = event => {
  event.preventDefault();
  run(async () => {
    const form = new FormData(event.currentTarget);
    const id = form.get("offerID");
    const kind = form.get("kind");
    await reauthenticate();
    const body = { numberOfCodes: Number(form.get("numberOfCodes")), expirationDate: form.get("expirationDate") };
    if (kind === "custom") body.code = form.get("code").trim();
    else body.environment = form.get("environment");
    const path = kind === "custom" ? "custom-codes" : "one-time-code-batches";
    const data = await api(appPath(`/offers/${encodeURIComponent(id)}/${path}`), {
      method: "POST", body, csrfRequired: true, idempotent: true
    });
    $("#codes-dialog").close();
    if (kind === "oneTime") await downloadBatch(data.codePool.id, false);
    else notice(`自定义码 ${data.codePool.code} 已创建`);
    await loadOffers();
  });
};

(async () => {
  const token = new URLSearchParams(location.hash.slice(1)).get("token") || "";
  const setup = location.pathname === "/setup" && !!token;
  const enroll = location.pathname === "/enroll" && !!token;
  if (setup) state.bootstrapToken = token;
  if (enroll) state.enrollmentToken = token;
  if (setup || enroll) history.replaceState(null, "", location.pathname);
  $("#setup-panel").classList.toggle("hidden", !setup);
  $("#enroll-panel").classList.toggle("hidden", !enroll);
  $("#login-panel").classList.toggle("hidden", setup || enroll);
  if (!setup && !enroll) await loadSession();
})();

const aiLabels = { voice_transcription:"语音转写", meal_photo_capture:"拍照记饮食", hydration_cup_estimate:"水杯估算", meal_text_capture:"文字记饮食", meal_decision:"饮食决策", diet_analysis:"饮食分析", health_nutrition_analysis:"健康营养分析", health_behavior_analysis:"健康行为分析" };
let aiRows=[], aiData={};
async function aiCommand(action, body) {
  const slot=`ai-command:${state.user.id}:${action}:${body.operation}`;
  const identity=JSON.stringify({...body,previewToken:undefined});
  let previous; try {previous=JSON.parse(sessionStorage.getItem(slot));} catch {}
  const command=previous?.identity===identity ? previous : {identity,key:uuid()};
  sessionStorage.setItem(slot,JSON.stringify(command));
  const result=await api(`/api/v1/ai/health/${action}`,{method:"POST",csrfRequired:true,idempotencyKey:command.key,body});
  sessionStorage.removeItem(slot);return result;
}
function aiPolicyLabel(p) { return p ? `${p.endpoint} / ${p.reasoningEffort} / 联网${p.webSearchEnabled?"开":"关"}` : "沿用 App 参数"; }
async function loadAI() {
  aiData=await api("/api/v1/ai/health"); aiRows=aiData.operations;
  $("#ai-status").textContent=`${aiData.writesEnabled?"配置发布已启用":"配置发布未启用"}。${aiData.rollingWritesEnabled?"原生灰度操作已启用":"原生灰度操作未启用"}。`;
  renderAI();
}
function renderAI() {
  const endpoints=[...new Map(aiRows.filter(r=>r.endpoint?.Id).map(r=>[r.endpoint.Id,r.endpoint])).values()];
  $("#ai-operations").innerHTML=aiRows.map((row,index)=>{
    const p=row.current?.policy || {endpoint:row.endpoint.Id,reasoningEffort:"minimal",webSearchEnabled:false};
    const model=row.endpoint.ModelReference?.FoundationModel;
    return `<article class="card"><h3>${escapeHTML(aiLabels[row.operation]||row.operation)}</h3>
      <p class="muted">${escapeHTML(model ? `${model.Name} · ${model.ModelVersion}` : "尚未同步模型")}<br>${escapeHTML(row.syncError||"")} · 最近成功同步：${formatTime(row.syncedAt)}<br>${row.current?`当前版本：${escapeHTML(row.current.id)}`:"尚未接管：当前仍按 App 参数执行；下方是待保存的配置。"}</p>
      <form data-ai-index="${index}"><label>接入点<select name="endpoint">${endpoints.map(ep=>`<option value="${escapeHTML(ep.Id)}" ${ep.Id===p.endpoint?"selected":""}>${escapeHTML(ep.Name||ep.Id)} · ${escapeHTML(ep.ModelReference?.FoundationModel?.Name)} (${escapeHTML(ep.Id)})</option>`).join("")}</select></label>
      <label>思考深度<select name="reasoningEffort">${[["minimal","关闭思考"],["low","轻度"],["medium","中度"],["high","深度"]].map(([value,label])=>`<option value="${value}" ${value===p.reasoningEffort?"selected":""}>${label}</option>`).join("")}</select></label>
      <label class="check"><input name="webSearchEnabled" type="checkbox" ${p.webSearchEnabled?"checked":""} ${row.operation!=="meal_decision"?"disabled":""}>允许联网搜索</label>
      ${row.operation!=="meal_decision"?'<p class="muted">此功能的数据边界不允许联网。</p>':""}
      <button class="primary" ${!aiData.writesEnabled||row.syncError?"disabled":""}>验证并保存草稿</button></form>
      <button class="quiet" data-ai-endpoint="${index}">查看接入点与灰度详情</button><div data-ai-endpoint-result="${index}"></div><details><summary>草稿与历史版本</summary>${(row.history||[]).map(r=>`<p>${escapeHTML(r.id)} · ${formatTime(r.createdAt)} · ${r.publishedAt?"已发布":"草稿"}<br>${escapeHTML(aiPolicyLabel(r.policy))} <button class="quiet" data-ai-restore="${index}" data-revision="${escapeHTML(r.id)}">载入配置</button> ${!r.publishedAt&&aiData.writesEnabled?`<button class="primary" data-ai-publish="${index}" data-revision="${escapeHTML(r.id)}">验证并发布</button>`:""}</p>`).join("")}${row.nextCursor?`<button data-ai-more="${index}">更早的版本</button>`:""}</details></article>`;
  }).join("");
  $$('[data-ai-index]').forEach(form=>form.onsubmit=event=>{event.preventDefault();run(async()=>{
    const row=aiRows[Number(form.dataset.aiIndex)];const values=new FormData(form);
    const body={operation:row.operation,baseVersion:row.current?.id||"",policy:{version:"",endpoint:values.get("endpoint"),reasoningEffort:values.get("reasoningEffort"),webSearchEnabled:values.has("webSearchEnabled"),timeoutSeconds:aiData.timeoutSeconds||90}};
    await reauthenticate();await aiCommand("drafts",body);
    await loadAI();$(`[data-ai-index="${form.dataset.aiIndex}"]`).closest("article").querySelector("details").open=true;notice("草稿已保存，线上配置尚未改变");
  });});
  $$('[data-ai-restore]').forEach(button=>button.onclick=()=>{
    const index=Number(button.dataset.aiRestore);const revision=aiRows[index].history.find(r=>r.id===button.dataset.revision);const form=$(`[data-ai-index="${index}"]`);
    form.elements.endpoint.value=revision.policy.endpoint;form.elements.reasoningEffort.value=revision.policy.reasoningEffort;form.elements.webSearchEnabled.checked=revision.policy.webSearchEnabled;
    notice("已载入历史配置，请保存为新草稿后发布");
  });
  $$('[data-ai-endpoint]').forEach(button=>button.onclick=()=>run(async()=>{
    const index=Number(button.dataset.aiEndpoint),row=aiRows[index];
    const data=await api(`/api/v1/ai/endpoints/${encodeURIComponent(row.endpointID||row.endpoint.Id)}`);
    const ep=data.endpoint,r=data.rolling,model=ep.ModelReference?.FoundationModel;
    const panel=$(`[data-ai-endpoint-result="${index}"]`);
    const states={queued:"等待执行",dispatching:"正在提交",watching:"等待云端完成",uncertain:"结果待核对",succeeded:"已完成",cancelled:"已撤销",failed:"失败",conflict:"状态已变化"};
    const pending=(data.commands||[]).some(c=>["queued","dispatching","uncertain"].includes(c.state));
    const isActive=r && !(r.Status==="Reverted"&&r.RollingGray===0) && r.RollingGray<100;
    panel.innerHTML=`<p>${escapeHTML(ep.Name||ep.Id)} · ${escapeHTML(ep.Status)} · ${escapeHTML(model?.Name)} / ${escapeHTML(model?.ModelVersion)}<br>${r?`新模型：${escapeHTML(r.RollingIn?.Name)} / ${escapeHTML(r.RollingIn?.ModelVersion)}，流量 ${Number(r.RollingGray)}%；状态：${escapeHTML(r.Status)}`:"没有关联的原生灰度任务"}<br>同步时间：${formatTime(data.syncedAt)}</p>
      ${data.writesEnabled?`<label>目标模型与版本<select data-rolling-target>${data.targets.filter(t=>t.audio&&(t.model.Name!==model?.Name||t.model.ModelVersion!==model?.ModelVersion)).map(t=>`<option value="${escapeHTML(JSON.stringify(t.model))}">${escapeHTML(t.model.Name)} / ${escapeHTML(t.model.ModelVersion)}</option>`).join("")}</select></label>
      ${pending&&r&&(data.commands||[]).some(c=>c.state==="uncertain"&&c.input.action==="start"&&!c.rollingID)?'<button data-rolling-action="reconcile">核对并关联云端任务</button>':""}<div class="actions"><button data-rolling-action="start" ${pending||isActive?"disabled":""}>预览模型切换</button><button data-rolling-action="step_back" ${pending||r?.Status!=="Running"||!r?.RollingGray||r?.RollingGray>=100?"disabled":""}>回退一个阶段</button><button data-rolling-action="cancel" ${pending||!["Running","Reverting"].includes(r?.Status)||r?.RollingGray>=100?"disabled":""}>撤销灰度</button></div><p class="muted">火山自动推进灰度。回退一个阶段与撤销整个灰度的作用不同。目录中的其他模型需完成兼容性验证后才能使用。</p>`:""}
      ${(data.commands||[]).length?`<details open><summary>最近的操作记录</summary>${data.commands.map(c=>`<p>${escapeHTML(states[c.state]||c.state)} · ${formatTime(c.createdAt)}<br>${escapeHTML(c.detail||c.input.action)}<br><small>${escapeHTML(c.id)}${c.rollingID?` / ${escapeHTML(c.rollingID)}`:""}</small></p>`).join("")}</details>`:""}
      ${data.attempts?.length?`<details><summary>最近请求实际使用的模型</summary>${data.attempts.map(a=>`<p>${escapeHTML(a.actualModel||"火山未返回模型名")} · ${formatTime(a.createdAt)} · ${escapeHTML(aiLabels[a.operation]||a.operation)}</p>`).join("")}</details>`:""}
      ${pending?'<p class="muted">已有操作等待执行或核对，暂不能再次提交。可刷新详情查看进展。</p>':""}`;
    panel.querySelectorAll('[data-rolling-action]').forEach(action=>action.onclick=()=>run(async()=>{
      const input={endpoint:ep.Id,action:action.dataset.rollingAction,target:action.dataset.rollingAction==="start"?JSON.parse(panel.querySelector('[data-rolling-target]').value):{Name:"",ModelVersion:""}};
      await reauthenticate();
      const preview=await api("/api/v1/ai/rolling/preview",{method:"POST",csrfRequired:true,body:input});
      const price=preview.snapshot.price;
      const label={start:"开始原生灰度",step_back:"回退一个阶段",cancel:"撤销灰度",reconcile:"核对并关联云端任务"}[input.action];
      if(!confirm(`${label}\n接入点：${ep.Name||ep.Id}\n旧模型：${model.Name} / ${model.ModelVersion}${input.action==="start"?`\n新模型：${input.target.Name} / ${input.target.ModelVersion}`:""}\n受影响功能：${preview.affectedOperations.map(op=>aiLabels[op]||op).join("、")||"暂无当前绑定"}\n费用预留：输入 ${price.InputNanosPerMillionTokens/1e9} 元 / 百万 tokens（含音频最高档），输出 ${price.OutputNanosPerMillionTokens/1e9} 元 / 百万 tokens。\n${preview.notice}`))return;
      const slot=`ai-rolling:${state.user.id}:${ep.Id}`;const identity=JSON.stringify(input);
      let previous;try{previous=JSON.parse(sessionStorage.getItem(slot));}catch{}
      const command=previous?.identity===identity?previous:{identity,key:uuid()};sessionStorage.setItem(slot,JSON.stringify(command));
      await api("/api/v1/ai/rolling/commands",{method:"POST",csrfRequired:true,idempotencyKey:command.key,body:{input,previewToken:preview.previewToken}});
      sessionStorage.removeItem(slot);notice("操作已保存，后台将提交至火山；可刷新详情查看进展");setTimeout(()=>button.click(),0);
    }));

  }));
  $$('[data-ai-more]').forEach(button=>button.onclick=()=>run(async()=>{
    const index=Number(button.dataset.aiMore),row=aiRows[index];
    const page=await api(`/api/v1/ai/health/${row.operation}/history?cursor=${encodeURIComponent(row.nextCursor)}`);
    row.history.push(...page.revisions);row.nextCursor=page.nextCursor;renderAI();
    $(`[data-ai-index="${index}"]`).closest("article").querySelector("details").open=true;
  }));
  $$('[data-ai-publish]').forEach(button=>button.onclick=()=>run(async()=>{
    const row=aiRows[Number(button.dataset.aiPublish)];
    const preview=await api(`/api/v1/ai/health/${row.operation}/revisions/${encodeURIComponent(button.dataset.revision)}`);
    if(!preview.canPublish) throw new Error("此草稿已经发布或基线已变化，请载入配置并保存新草稿");
    if(!confirm(`${aiLabels[row.operation]}\n当前：${aiPolicyLabel(preview.before)}\n发布：${aiPolicyLabel(preview.after)}\n仅影响此功能的新请求。`))return;
    await reauthenticate();await aiCommand("publish",{operation:row.operation,revision:preview.revision.id,baseVersion:preview.revision.baseVersion,previewToken:preview.previewToken});
    await loadAI();notice("配置已发布");
  }));
}
$("#refresh-ai").onclick=()=>run(loadAI);
$("#load-ai-models").onclick=()=>run(async()=>{
  const data=await api("/api/v1/ai/models");
  $("#ai-models").innerHTML=`<p class="muted">目录同步：${formatTime(data.syncedAt)} · 开通状态同步：${formatTime(data.activations?.syncedAt)}${data.stale||data.activations?.stale?" · 同步失败，当前显示缓存数据":""}</p><label>模型<select id="ai-model-picker">${data.models.map(m=>{const a=data.activations?.value?.find(a=>a.FoundationModelName===m.Name);return `<option value="${escapeHTML(m.Name)}">${escapeHTML(m.DisplayName||m.Name)} · ${a?.State==="Available"?"账号已开通":escapeHTML(a?.State||"开通状态未知")}</option>`;}).join("")}</select></label><button class="quiet" id="ai-model-versions">读取版本与价格</button><div id="ai-model-detail"></div>`;
  $("#ai-model-versions").onclick=()=>run(async()=>{
    const name=$("#ai-model-picker").value;
    const versions=await api(`/api/v1/ai/models/${encodeURIComponent(name)}/versions`);
    const activation=data.activations?.value?.find(a=>a.FoundationModelName===name);
    const groups=activation?.MultiChargeItems?.length?activation.MultiChargeItems:[{Name:"基础价格",ChargeItems:activation?.ChargeItems||[]}];
    $("#ai-model-detail").innerHTML=`<p>版本：${versions.versions.map(v=>`${escapeHTML(v.ModelVersion)} (${escapeHTML(v.Status||"状态未知")})`).join("、")}</p><p class="muted">以下为火山返回的账号价格；版本存在、账号已开通、可升级至该版本需要分别验证。</p>${groups.map(g=>`<p>${escapeHTML(g.Description||g.Name||"价格分档")}<br>${(g.ChargeItems||[]).filter(c=>["InferencePrompt","InferenceCompletion","AudioPrompt"].includes(c.Type)).map(c=>`${escapeHTML(({InferencePrompt:"文本/图片输入",InferenceCompletion:"输出",AudioPrompt:"音频输入"})[c.Type])}：${escapeHTML(c.Price)} 元 / ${escapeHTML(c.UnitCode)}`).join("<br>")}</p>`).join("")}`;
  });
});
