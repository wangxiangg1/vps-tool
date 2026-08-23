(function () {
  "use strict";

  const state = {
    csrfToken: "",
    user: null,
    nodes: [],
    actions: [],
    tasks: [],
    selected: new Set(),
    busy: new Set(),
    activityTab: "actions",
    confirmAction: null,
  };

  const $ = (id) => document.getElementById(id);
  const text = (value, fallback = "—") => value === null || value === undefined || value === "" ? fallback : String(value);
  const escapeHtml = (value) => text(value, "").replace(/[&<>"']/g, (char) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  })[char]);
  const actionLabels = {
    get_status: "刷新状态",
    get_ip: "获取 IP",
    warp_on: "开启 WARP",
    warp_off: "关闭 WARP",
    change_ip: "换 IP",
    restart_xui: "重启 3x-ui",
  };
  const statusLabels = {
    online: "在线", offline: "离线", unknown: "未知", on: "开启", off: "关闭",
    degraded: "降级", running: "运行中", stopped: "已停止", failed: "失败",
    not_found: "未找到", queued: "排队中", dispatched: "已下发", accepted: "已接收",
    executing: "执行中", running_action: "执行中", succeeded: "成功", timed_out: "超时",
    expired: "已过期", skipped_offline: "跳过（离线）", canceled: "已取消",
  };

  async function api(path, options) {
    const request = Object.assign({ credentials: "same-origin" }, options || {});
    request.headers = Object.assign({}, request.headers || {});
    if (request.body && typeof request.body !== "string") {
      request.headers["Content-Type"] = "application/json";
      request.body = JSON.stringify(request.body);
    }
    if (request.method && request.method !== "GET" && state.csrfToken) {
      request.headers["X-CSRF-Token"] = state.csrfToken;
    }
    const response = await fetch(path, request);
    const raw = await response.text();
    let body = {};
    try { body = raw ? JSON.parse(raw) : {}; } catch (_) { body = { message: raw }; }
    if (!response.ok) {
      const detail = body.detail && typeof body.detail === "object" ? body.detail : body.detail;
      const error = new Error(text(detail && detail.message, text(body.message, "请求失败")));
      error.code = detail && detail.code ? detail.code : body.code;
      error.status = response.status;
      throw error;
    }
    return body;
  }

  function showLogin(message) {
    $("loginView").hidden = false;
    $("dashboardView").hidden = true;
    if (message) $("loginMessage").textContent = message;
  }

  function showDashboard() {
    $("loginView").hidden = true;
    $("dashboardView").hidden = false;
    $("operatorName").textContent = state.user && state.user.username ? state.user.username : "管理员";
  }

  function setHealth(ok, detail) {
    $("healthDot").className = `status-dot ${ok ? "status-dot-online" : "status-dot-danger"}`;
    $("healthLabel").textContent = ok ? "主控在线" : "主控异常";
    $("healthDetail").textContent = detail || "等待检测";
    $("loginHealthText").textContent = ok ? "主控可连接" : "主控不可用";
  }

  function notify(message, kind) {
    const item = document.createElement("div");
    item.className = `toast ${kind === "error" ? "is-error" : kind === "warning" ? "is-warning" : ""}`;
    item.innerHTML = `<div class="toast-message">${escapeHtml(message)}</div>`;
    $("toastRegion").appendChild(item);
    window.setTimeout(() => item.remove(), 4800);
  }

  function formatTime(value) {
    if (!value) return "—";
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return text(value);
    return date.toLocaleString("zh-CN", { hour12: false, month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" });
  }

  function formatBytes(value) {
    const number = Number(value);
    if (!Number.isFinite(number) || number < 0) return "—";
    if (number < 1024 * 1024) return `${Math.round(number / 1024)} KB`;
    return `${(number / 1024 / 1024).toFixed(1)} MB`;
  }

  function formatUptime(value) {
    const seconds = Number(value);
    if (!Number.isFinite(seconds) || seconds < 0) return "—";
    const days = Math.floor(seconds / 86400);
    const hours = Math.floor((seconds % 86400) / 3600);
    const minutes = Math.floor((seconds % 3600) / 60);
    return days ? `${days}d ${hours}h` : `${hours}h ${minutes}m`;
  }

  function nodeStatus(node) {
    return node.online ? "online" : (node.last_seen_at ? "offline" : "unknown");
  }

  function statusValue(node, key) {
    return node[key] || (node.status && node.status[key]) || "unknown";
  }

  function badge(value) {
    const key = text(value, "unknown");
    return `<span class="status-badge status-${escapeHtml(key)}">${escapeHtml(statusLabels[key] || key)}</span>`;
  }

  function renderSummary(nodes) {
    const online = nodes.filter((node) => node.online).length;
    const warp = nodes.filter((node) => statusValue(node, "warp_status") === "on").length;
    const xui = nodes.filter((node) => statusValue(node, "xui_status") === "running").length;
    const unknown = nodes.filter((node) => nodeStatus(node) === "unknown" || statusValue(node, "warp_status") === "unknown").length;
    $("onlineCount").textContent = String(online);
    $("railOnlineCount").textContent = String(online);
    $("onlineSummary").textContent = `${nodes.length} 个节点已登记`;
    $("warpOnCount").textContent = String(warp);
    $("xuiRunningCount").textContent = String(xui);
    $("unknownCount").textContent = String(unknown);
    $("navNodeCount").textContent = String(nodes.length);
  }

  function filteredNodes() {
    const query = $("nodeSearch").value.trim().toLowerCase();
    const status = $("statusFilter").value;
    const warp = $("warpFilter").value;
    const xui = $("xuiFilter").value;
    return state.nodes.filter((node) => {
      const searchable = `${node.name || ""} ${node.id || ""} ${node.region || ""}`.toLowerCase();
      return (!query || searchable.includes(query))
        && (status === "all" || nodeStatus(node) === status)
        && (warp === "all" || statusValue(node, "warp_status") === warp)
        && (xui === "all" || statusValue(node, "xui_status") === xui);
    });
  }

  function renderNodes() {
    const nodes = filteredNodes();
    renderSummary(state.nodes);
    $("resultCount").textContent = `${nodes.length} / ${state.nodes.length}`;
    $("navActivityCount").textContent = String(state.actions.length);
    $("selectionCount").textContent = `已选 ${state.selected.size} 个`;
    $("selectionTools").hidden = state.selected.size === 0;
    $("batchButton").disabled = state.selected.size === 0;
    if (!state.nodes.length) {
      $("nodeState").hidden = false;
      $("nodeState").className = "state-message is-empty";
      $("nodeState").innerHTML = "暂无节点。请先在主控 API 创建节点并完成 Agent 注册。";
      $("nodeTableWrap").hidden = true;
      return;
    }
    if (!nodes.length) {
      $("nodeState").hidden = false;
      $("nodeState").className = "state-message is-empty";
      $("nodeState").innerHTML = "没有符合当前筛选条件的节点。";
      $("nodeTableWrap").hidden = true;
      return;
    }
    $("nodeState").hidden = true;
    $("nodeTableWrap").hidden = false;
    $("nodesBody").innerHTML = nodes.map((node) => {
      const memoryUsed = Number(node.memory_used_bytes || 0);
      const memoryTotal = Number(node.memory_total_bytes || 0);
      const memoryPercent = memoryTotal ? Math.min(100, Math.round(memoryUsed * 100 / memoryTotal)) : 0;
      const cpu = Math.max(0, Math.min(100, Number(node.cpu_percent || 0)));
      const barClass = (value) => value >= 90 ? "is-danger" : value >= 75 ? "is-warn" : "";
      const selected = state.selected.has(node.id) ? " checked" : "";
      const disabled = node.online ? "" : " disabled";
      const tags = (node.tags || []).slice(0, 2).map((tag) => `<span class="node-tag">${escapeHtml(tag)}</span>`).join("");
      const ip = node.public_ipv4 || node.egress_ipv4 || (node.status && node.status.public_ipv4);
      return `<tr data-node-id="${escapeHtml(node.id)}">
        <td><label class="checkbox-label" title="选择在线节点"><input class="node-select" type="checkbox" data-node-id="${escapeHtml(node.id)}"${selected}${disabled} aria-label="选择 ${escapeHtml(node.name)}"><span class="custom-checkbox" aria-hidden="true"></span></label></td>
        <td><div class="node-cell"><div class="node-name-line"><span class="node-name">${escapeHtml(node.name)}</span>${tags}</div><span class="node-meta">${escapeHtml(node.region || "未设置地区")} · ${escapeHtml(node.id.slice(0, 12))}</span></div></td>
        <td><span class="ip-value ${ip ? "" : "is-unknown"}">${escapeHtml(ip || "未采集")}</span></td>
        <td>${badge(statusValue(node, "warp_status"))}</td>
        <td>${badge(statusValue(node, "xui_status"))}</td>
        <td><div class="resource-cell"><div class="resource-line"><span>CPU ${cpu.toFixed(1)}%</span><span>${formatBytes(memoryUsed)} / ${formatBytes(memoryTotal)}</span></div><div class="resource-bar"><span class="${barClass(cpu)}" style="width:${cpu}%"></span></div><div class="resource-line"><span>内存 ${memoryPercent}%</span><span>${escapeHtml(node.agent_version || "Agent —")}</span></div><div class="resource-bar"><span class="${barClass(memoryPercent)}" style="width:${memoryPercent}%"></span></div></div></td>
        <td><span class="uptime-value ${node.uptime_seconds ? "" : "is-unknown"}">${escapeHtml(formatUptime(node.uptime_seconds))}</span></td>
        <td><span class="table-meta">${escapeHtml(formatTime(node.last_seen_at))}</span><br>${badge(nodeStatus(node))}</td>
        <td class="action-column"><button class="row-detail-button" data-open-node="${escapeHtml(node.id)}" type="button" title="查看节点详情" aria-label="查看 ${escapeHtml(node.name)} 的详情">→</button></td>
      </tr>`;
    }).join("");
    const visibleIds = new Set(nodes.map((node) => node.id));
    state.selected.forEach((id) => { if (!visibleIds.has(id)) state.selected.delete(id); });
    $("selectAllNodes").checked = nodes.length > 0 && nodes.every((node) => state.selected.has(node.id));
    $("selectAllNodes").indeterminate = nodes.some((node) => state.selected.has(node.id)) && !$("selectAllNodes").checked;
  }

  function renderActivities() {
    $("actionCount").textContent = String(state.actions.length);
    $("taskCount").textContent = String(state.tasks.length);
    const content = $("activityContent");
    if (state.activityTab === "tasks") {
      content.innerHTML = state.tasks.length ? `<div class="activity-list">${state.tasks.map((task) => `<div class="activity-row"><div><strong>${escapeHtml(task.name)}</strong><span class="table-meta">${escapeHtml(task.timezone)} · ${escapeHtml(task.schedule_value)}</span></div><div>${badge(task.action)}</div><div>${badge(task.enabled ? "online" : "stopped")}</div><div class="table-meta">下次：${escapeHtml(formatTime(task.next_run_at))}</div></div>`).join("")}</div>` : `<div class="activity-empty">暂无定时任务。</div>`;
    } else {
      content.innerHTML = state.actions.length ? `<div class="activity-list">${state.actions.slice(0, 30).map((item) => `<div class="activity-row"><div><strong>${escapeHtml(actionLabels[item.action] || item.action)}</strong><span class="table-meta">${escapeHtml(item.node_id || "未知节点")} · ${escapeHtml(item.id || "")}</span></div><div>${badge(item.status)}</div><div class="table-meta">${escapeHtml(formatTime(item.created_at))}</div><div class="table-meta">${escapeHtml(item.error_message || (item.result ? "已返回结果" : "等待 Agent 回执"))}</div></div>`).join("")}</div>` : `<div class="activity-empty">暂无操作记录。</div>`;
    }
    document.querySelectorAll("[data-activity-tab]").forEach((tab) => {
      const active = tab.dataset.activityTab === state.activityTab;
      tab.classList.toggle("is-active", active);
      tab.setAttribute("aria-selected", active ? "true" : "false");
    });
  }

  function openDrawer(nodeId) {
    const node = state.nodes.find((item) => item.id === nodeId);
    if (!node) return;
    $("detailTitle").textContent = node.name;
    const memoryUsed = Number(node.memory_used_bytes || 0);
    const memoryTotal = Number(node.memory_total_bytes || 0);
    const memoryPercent = memoryTotal ? Math.round(memoryUsed * 100 / memoryTotal) : 0;
    const cpu = Math.max(0, Math.min(100, Number(node.cpu_percent || 0)));
    const recent = state.actions.filter((item) => item.node_id === nodeId).slice(0, 6);
    $("detailContent").innerHTML = `<div class="detail-lead"><div><p class="detail-id">${escapeHtml(node.id)}</p><div class="detail-tags">${(node.tags || []).map((tag) => `<span class="node-tag">${escapeHtml(tag)}</span>`).join("")}</div></div>${badge(nodeStatus(node))}</div>
      <div class="detail-grid"><div class="detail-fact"><span class="field-label">当前出口 IPv4</span><strong>${escapeHtml(node.public_ipv4 || "未采集")}</strong></div><div class="detail-fact"><span class="field-label">Agent 版本</span><strong>${escapeHtml(node.agent_version || "—")}</strong></div><div class="detail-fact"><span class="field-label">WARP</span><strong>${escapeHtml(statusLabels[statusValue(node, "warp_status")] || statusValue(node, "warp_status"))}</strong></div><div class="detail-fact"><span class="field-label">3x-ui Unit</span><strong>${escapeHtml(node.xui_service || "—")}</strong></div><div class="detail-fact"><span class="field-label">系统 / 架构</span><strong>${escapeHtml(`${node.os_name || "—"} / ${node.architecture || "—"}`)}</strong></div><div class="detail-fact"><span class="field-label">最后心跳</span><strong>${escapeHtml(formatTime(node.last_seen_at))}</strong></div></div>
      <div class="detail-metrics"><div class="metric-row"><span class="metric-label">CPU</span><span class="metric-track"><span style="width:${cpu}%"></span></span><span class="metric-value">${cpu.toFixed(1)}%</span></div><div class="metric-row"><span class="metric-label">内存</span><span class="metric-track"><span style="width:${memoryPercent}%"></span></span><span class="metric-value">${memoryPercent}%</span></div></div>
      <section class="detail-section"><div class="detail-section-heading"><h3>固定操作</h3><span class="table-meta">${node.online ? "Agent 在线" : "节点离线"}</span></div><div class="action-grid">${["get_status", "get_ip", "warp_on", "warp_off", "change_ip", "restart_xui"].map((action) => `<button type="button" class="button ${["warp_off", "change_ip", "restart_xui"].includes(action) ? "button-danger" : "button-quiet"}" data-action="${action}" data-node-id="${escapeHtml(node.id)}"${!node.online || state.busy.has(node.id) ? " disabled" : ""}>${escapeHtml(actionLabels[action])}</button>`).join("")}</div></section>
      <section class="detail-section"><div class="detail-section-heading"><h3>最近操作</h3></div><div class="detail-log-list">${recent.length ? recent.map((item) => `<div class="detail-log-row"><div class="detail-log-main"><strong>${escapeHtml(actionLabels[item.action] || item.action)}</strong><span>${escapeHtml(formatTime(item.created_at))}</span></div>${badge(item.status)}</div>`).join("") : `<div class="detail-empty">暂无记录。</div>`}</div></section>`;
    $("drawerScrim").hidden = false;
    $("detailDrawer").hidden = false;
    requestAnimationFrame(() => $("detailDrawer").classList.add("is-open"));
  }

  function closeDrawer() {
    $("detailDrawer").classList.remove("is-open");
    window.setTimeout(() => { $("detailDrawer").hidden = true; $("drawerScrim").hidden = true; }, 160);
  }

  function confirm(message, details, callback, options) {
    state.confirmAction = callback;
    $("confirmTitle").textContent = options && options.title ? options.title : "确认操作";
    $("confirmMessage").textContent = message;
    $("confirmDetails").innerHTML = details || "";
    $("confirmProceed").textContent = options && options.proceed ? options.proceed : "确认执行";
    const dialog = $("confirmDialog");
    if (typeof dialog.showModal === "function") dialog.showModal();
    else dialog.hidden = false;
  }

  async function executeAction(nodeId, action) {
    const node = state.nodes.find((item) => item.id === nodeId);
    if (!node || state.busy.has(nodeId)) return;
    const dangerous = ["warp_on", "warp_off", "change_ip", "restart_xui"].includes(action);
    const run = async () => {
      state.busy.add(nodeId);
      openDrawer(nodeId);
      try {
        const response = await api(`/api/nodes/${encodeURIComponent(nodeId)}/actions`, { method: "POST", body: { action, parameters: {}, queue_if_offline: false } });
        notify(`${actionLabels[action]}：请求已提交`, "warning");
        await pollRequest(response.request && response.request.id);
      } catch (error) {
        notify(`${actionLabels[action]}：${error.message}`, "error");
      } finally {
        state.busy.delete(nodeId);
        await refreshData(false);
        openDrawer(nodeId);
      }
    };
    if (dangerous) {
      confirm(`${node.name} 将执行“${actionLabels[action]}”。`, `<ul><li>同一节点的状态变更会串行执行。</li><li>结果会写入操作记录。</li></ul>`, run, { title: actionLabels[action], proceed: "确认执行" });
    } else {
      await run();
    }
  }

  async function pollRequest(requestId) {
    if (!requestId) return;
    for (let attempt = 0; attempt < 15; attempt += 1) {
      await new Promise((resolve) => window.setTimeout(resolve, 700));
      try {
        const response = await api(`/api/action-requests/${encodeURIComponent(requestId)}`);
        const request = response.request;
        if (["succeeded", "failed", "timed_out", "expired", "skipped_offline", "canceled"].includes(request.status)) {
          notify(request.status === "succeeded" ? "操作已成功" : (request.error_message || `操作状态：${request.status}`), request.status === "succeeded" ? "ok" : "error");
          return;
        }
      } catch (_) { return; }
    }
  }

  async function refreshData(showLoading) {
    if (showLoading) {
      $("nodeState").hidden = false;
      $("nodeState").className = "state-message";
      $("nodeState").innerHTML = `<span class="state-spinner" aria-hidden="true"></span>正在读取节点数据……`;
    }
    try {
      const [nodeResponse, actionResponse, taskResponse, health] = await Promise.all([
        api("/api/nodes"), api("/api/actions"), api("/api/tasks"), api("/api/health"),
      ]);
      state.nodes = nodeResponse.nodes || nodeResponse.items || [];
      state.actions = actionResponse.requests || actionResponse.items || [];
      state.tasks = taskResponse.tasks || taskResponse.items || [];
      setHealth(Boolean(health.ok), `${health.online_agents || 0} 个 Agent 在线`);
      $("lastUpdated").textContent = `数据更新时间：${formatTime(new Date().toISOString())}`;
      $("viewStatusLine").textContent = `最近同步 ${formatTime(new Date().toISOString())} · ${state.nodes.length} 个节点`;
      renderNodes();
      renderActivities();
    } catch (error) {
      if (error.status === 401) { showLogin("登录已失效，请重新登录。"); return; }
      $("nodeState").hidden = false;
      $("nodeState").className = "state-message is-error";
      $("nodeState").innerHTML = `<strong>节点数据读取失败</strong><span class="state-detail">${escapeHtml(error.message)}</span><button class="button button-quiet state-action" type="button" id="retryDataButton">重新读取</button>`;
      $("retryDataButton").addEventListener("click", () => refreshData(true), { once: true });
      setHealth(false, error.message);
    }
  }

  async function boot() {
    try {
      const health = await api("/api/health");
      setHealth(Boolean(health.ok), `${health.online_agents || 0} 个 Agent 在线`);
    } catch (_) { setHealth(false, "主控不可达"); }
    try {
      const me = await api("/api/auth/me");
      state.user = { username: me.username };
      state.csrfToken = me.csrf_token || "";
      showDashboard();
      await refreshData(true);
    } catch (_) { showLogin(); }
  }

  $("loginForm").addEventListener("submit", async (event) => {
    event.preventDefault();
    const button = $("loginButton");
    button.disabled = true;
    $("loginMessage").textContent = "正在验证……";
    try {
      const response = await api("/api/auth/login", { method: "POST", body: { username: $("username").value.trim(), password: $("password").value } });
      state.user = response.user;
      state.csrfToken = response.csrf_token || "";
      $("password").value = "";
      $("loginMessage").textContent = "";
      showDashboard();
      await refreshData(true);
    } catch (error) {
      $("loginMessage").textContent = error.message;
    } finally { button.disabled = false; }
  });

  $("logoutButton").addEventListener("click", async () => {
    try { await api("/api/auth/logout", { method: "POST" }); } catch (_) { /* session may already be gone */ }
    state.csrfToken = "";
    state.user = null;
    showLogin("已退出登录。");
  });
  $("refreshButton").addEventListener("click", () => refreshData(true));
  ["nodeSearch", "statusFilter", "warpFilter", "xuiFilter"].forEach((id) => $(id).addEventListener("input", renderNodes));
  $("nodesBody").addEventListener("click", (event) => {
    const detail = event.target.closest("[data-open-node]");
    if (detail) openDrawer(detail.dataset.openNode);
    const action = event.target.closest("[data-action]");
    if (action) executeAction(action.dataset.nodeId, action.dataset.action);
  });
  $("nodesBody").addEventListener("change", (event) => {
    const checkbox = event.target.closest(".node-select");
    if (!checkbox) return;
    if (checkbox.checked) state.selected.add(checkbox.dataset.nodeId); else state.selected.delete(checkbox.dataset.nodeId);
    renderNodes();
  });
  $("selectAllNodes").addEventListener("change", () => {
    filteredNodes().forEach((node) => { if (node.online && $("selectAllNodes").checked) state.selected.add(node.id); else state.selected.delete(node.id); });
    renderNodes();
  });
  $("batchButton").addEventListener("click", () => {
    const nodes = state.nodes.filter((node) => state.selected.has(node.id) && node.online);
    if (!nodes.length) return;
    const options = ["change_ip", "warp_on", "warp_off", "restart_xui"].map((value) => `<option value="${value}">${actionLabels[value]}</option>`).join("");
    confirm(`将对 ${nodes.length} 个在线节点执行批量操作。`, `<label class="field-label" for="batchActionSelect">固定 Action</label><select id="batchActionSelect">${options}</select>`, async () => {
      const action = $("batchActionSelect").value;
      for (const node of nodes) await executeAction(node.id, action);
      state.selected.clear();
      renderNodes();
    }, { title: "批量操作", proceed: "开始批量执行" });
  });
  document.querySelectorAll("[data-scroll-target]").forEach((button) => button.addEventListener("click", () => $(button.dataset.scrollTarget).scrollIntoView({ behavior: "smooth" })));
  $("activityToggle").addEventListener("click", () => $("activitySection").scrollIntoView({ behavior: "smooth" }));
  $("activityCollapse").addEventListener("click", () => { const body = $("activityBody"); body.hidden = !body.hidden; $("activityCollapse").setAttribute("aria-expanded", body.hidden ? "false" : "true"); });
  document.querySelectorAll("[data-activity-tab]").forEach((tab) => tab.addEventListener("click", () => { state.activityTab = tab.dataset.activityTab; renderActivities(); }));
  $("closeDrawerButton").addEventListener("click", closeDrawer);
  $("drawerScrim").addEventListener("click", closeDrawer);
  $("confirmForm").addEventListener("submit", async (event) => {
    event.preventDefault();
    const proceed = event.submitter && event.submitter.value === "confirm";
    const callback = state.confirmAction;
    state.confirmAction = null;
    if ($("confirmDialog").open) $("confirmDialog").close(); else $("confirmDialog").hidden = true;
    if (proceed && callback) await callback();
  });
  window.addEventListener("keydown", (event) => { if (event.key === "Escape" && !$('detailDrawer').hidden) closeDrawer(); });
  boot();
}());
