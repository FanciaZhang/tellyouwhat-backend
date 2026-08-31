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

async function api(path, { method = "GET", body, csrfRequired = false, idempotent = false } = {}) {
  const headers = { Accept: "application/json" };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (csrfRequired) headers["X-Admin-CSRF"] = state.csrf;
  if (idempotent) headers["Idempotency-Key"] = uuid();
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
