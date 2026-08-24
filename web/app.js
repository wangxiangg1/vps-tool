(function () {
  "use strict";

  const state = {
    csrfToken: "", user: null, nodes: [], actions: [], tasks: [], ipHistory: {},
    selected: new Set(), busy: new Set(), activeView: "nodes",
    activeNodeId: "", confirmAction: null, nodeFormMode: "create", editingNodeId: "",
  };
  const $ = (id) => document.getElementById(id);
  const text = (value, fallback = "—") => value === null || value === undefined || value === "" ? fallback : String(value);
  const escapeHtml = (value) => text(value, "").replace(/[&<>"']/g, (char) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  })[char]);
  const shellQuote = (value) => `'${String(value ?? "").replace(/'/g, "'\\''")}'`;
  const agentWssUrl = () => `${window.location.protocol === "https:" ? "wss:" : "ws:"}//${window.location.host}/agent`;
  const buildAgentInstallCommand = (node, registrationToken) => {
    const installerUrl = "https://github.com/wangxiangg1/vps-tool/releases/latest/download/install-agent.sh";
    const args = [
      "--node-id", shellQuote(node.id),
      "--registration-token", shellQuote(registrationToken),
      "--wss-url", shellQuote(agentWssUrl()),
      "--xui-unit", shellQuote(node.xui_service || "x-ui"),
      "--warp-adapter", shellQuote(node.warp_adapter || "generic"),
    ].join(" ");
    return `(command -v curl >/dev/null 2>&1 && curl -fsSL ${installerUrl} || wget -qO- ${installerUrl}) | sh -s -- ${args}`;
  };
  const actionLabels = {
    get_status: "刷新状态", get_ip: "获取出口 IP", warp_on: "开启 WARP",
    warp_off: "关闭 WARP", change_ip: "切换出口 IP", restart_xui: "重启 3x-ui",
    upgrade_agent: "升级 Agent",
  };
  const actionIcons = {
    get_status: "sync", get_ip: "public", warp_on: "shield", warp_off: "shield_lock",
    change_ip: "swap_horiz", restart_xui: "restart_alt", upgrade_agent: "system_update_alt",
  };
  const statusLabels = {
    online: "在线", offline: "离线", unknown: "未知", on: "开启", off: "关闭",
    degraded: "降级", running: "运行中", stopped: "已停止", failed: "失败",
    not_found: "未找到", queued: "排队中", dispatched: "已下发", accepted: "已接收",
    executing: "执行中", running_action: "执行中", succeeded: "成功", timed_out: "超时",
    expired: "已过期", skipped_offline: "跳过（离线）", canceled: "已取消",
  };

  const iconPaths = {
    account_circle: '<circle cx="12" cy="8" r="4"/><path d="M4 21a8 8 0 0 1 16 0"/>',
    lock: '<rect width="18" height="11" x="3" y="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/>',
    visibility_off: '<path d="m2 2 20 20"/><path d="M10.6 10.7a2 2 0 0 0 2.7 2.7"/><path d="M9.9 4.2A10.5 10.5 0 0 1 12 4c7 0 10 8 10 8a18 18 0 0 1-2.1 3.2"/><path d="M6.6 6.6C3.8 8.4 2 12 2 12s3 8 10 8a9.7 9.7 0 0 0 5.4-1.6"/>',
    visibility: '<path d="M2 12s3-8 10-8 10 8 10 8-3 8-10 8S2 12 2 12Z"/><circle cx="12" cy="12" r="3"/>',
    login: '<path d="M15 3h4a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2h-4"/><path d="m10 17 5-5-5-5"/><path d="M15 12H3"/>',
    verified_user: '<path d="M20 13c0 5-3.5 7.5-8 9-4.5-1.5-8-4-8-9V5l8-3 8 3Z"/><path d="m9 12 2 2 4-4"/>',
    search: '<circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/>',
    sync: '<path d="M20 7h-5V2"/><path d="M4 17h5v5"/><path d="M5.1 9A8 8 0 0 1 18.4 5.6L20 7"/><path d="M18.9 15A8 8 0 0 1 5.6 18.4L4 17"/>',
    person: '<path d="M19 21v-2a4 4 0 0 0-4-4H9a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/>',
    dns: '<rect width="18" height="8" x="3" y="3" rx="2"/><rect width="18" height="8" x="3" y="13" rx="2"/><path d="M7 7h.01M7 17h.01"/>',
    assignment: '<rect width="16" height="18" x="4" y="3" rx="2"/><path d="M9 3V1h6v2M8 8h8M8 12h8M8 16h5"/>',
    policy: '<path d="M20 13c0 5-3.5 7.5-8 9-4.5-1.5-8-4-8-9V5l8-3 8 3Z"/><circle cx="12" cy="11" r="2"/><path d="m13.5 12.5 2 2"/>',
    hub: '<circle cx="12" cy="5" r="3"/><circle cx="5" cy="19" r="3"/><circle cx="19" cy="19" r="3"/><path d="M10.5 7.7 6.5 16M13.5 7.7l4 8.3M8 19h8"/>',
    database: '<ellipse cx="12" cy="5" rx="9" ry="3"/><path d="M3 5v7c0 1.7 4 3 9 3s9-1.3 9-3V5"/><path d="M3 12v7c0 1.7 4 3 9 3s9-1.3 9-3v-7"/>',
    add: '<path d="M12 5v14M5 12h14"/>',
    shield: '<path d="M20 13c0 5-3.5 7.5-8 9-4.5-1.5-8-4-8-9V5l8-3 8 3Z"/>',
    router: '<rect width="20" height="8" x="2" y="13" rx="2"/><path d="M6 17h.01M10 17h.01M15 13V9M11.5 5.5a5 5 0 0 1 7 0M14 8a1.5 1.5 0 0 1 2 0"/>',
    warning: '<path d="m21.7 18-8-14a2 2 0 0 0-3.4 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.7-3Z"/><path d="M12 9v4M12 17h.01"/>',
    playlist_add_check: '<path d="M11 6H3M11 12H3M8 18H3M15 6l2 2 4-4M15 15l2 2 4-4"/>',
    filter_list: '<path d="M4 6h16M7 12h10M10 18h4"/>',
    download: '<path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4M7 10l5 5 5-5M12 15V3"/>',
    schedule: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>',
    arrow_back: '<path d="m15 18-6-6 6-6M9 12h12"/>',
    close: '<path d="M18 6 6 18M6 6l12 12"/>',
    content_copy: '<rect width="14" height="14" x="8" y="8" rx="2"/><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"/>',
    edit: '<path d="M12 20h9"/><path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L8 18l-4 1 1-4Z"/>',
    memory: '<rect width="16" height="16" x="4" y="4" rx="2"/><rect width="6" height="6" x="9" y="9"/><path d="M9 1v3M15 1v3M9 20v3M15 20v3M20 9h3M20 14h3M1 9h3M1 14h3"/>',
    developer_board: '<rect width="14" height="18" x="5" y="3" rx="2"/><path d="M9 7h6M9 11h6M9 15h4M3 7h2M3 11h2M3 15h2M19 7h2M19 11h2M19 15h2"/>',
    hard_drive: '<rect width="20" height="8" x="2" y="3" rx="2"/><rect width="20" height="8" x="2" y="13" rx="2"/><path d="M6 7h.01M6 17h.01"/>',
    public: '<circle cx="12" cy="12" r="10"/><path d="M2 12h20M12 2a15 15 0 0 1 0 20M12 2a15 15 0 0 0 0 20"/>',
    cloud_off: '<path d="m2 2 20 20M5.8 5.8A7 7 0 0 1 19 9a5 5 0 0 1 1.6 9.7M7 19H6a4 4 0 0 1-1.5-7.7"/>',
    more_vert: '<circle cx="12" cy="5" r="1" fill="currentColor" stroke="none"/><circle cx="12" cy="12" r="1" fill="currentColor" stroke="none"/><circle cx="12" cy="19" r="1" fill="currentColor" stroke="none"/>',
    manage_search: '<circle cx="10" cy="10" r="7"/><path d="m15 15 6 6M3 20h6"/>',
    calendar_add_on: '<rect width="18" height="18" x="3" y="4" rx="2"/><path d="M16 2v4M8 2v4M3 10h18M12 14v5M9.5 16.5h5"/>',
    pause: '<path d="M8 5v14M16 5v14"/>',
    delete: '<path d="M3 6h18M8 6V4h8v2M19 6l-1 15H6L5 6M10 11v6M14 11v6"/>',
    error: '<circle cx="12" cy="12" r="10"/><path d="M12 8v4M12 16h.01"/>',
    check_circle: '<circle cx="12" cy="12" r="10"/><path d="m8 12 3 3 5-6"/>',
    shield_lock: '<path d="M20 13c0 5-3.5 7.5-8 9-4.5-1.5-8-4-8-9V5l8-3 8 3Z"/><rect width="7" height="5" x="8.5" y="10" rx="1"/><path d="M10 10V8.5a2 2 0 0 1 4 0V10"/>',
    swap_horiz: '<path d="m16 3 4 4-4 4M20 7H4M8 21l-4-4 4-4M4 17h16"/>',
    restart_alt: '<path d="M3 12a9 9 0 1 0 3-6.7L3 8"/><path d="M3 3v5h5"/>',
    system_update_alt: '<path d="M12 3v12M7 10l5 5 5-5"/><path d="M5 21h14"/>',
    arrow_forward: '<path d="M5 12h14M13 6l6 6-6 6"/>',
    history: '<path d="M3 12a9 9 0 1 0 3-6.7L3 8"/><path d="M3 3v5h5"/><path d="M12 7v5l3 2"/>',
    filter_alt_off: '<path d="M3 3h18l-7 8v6l-4 2v-8Z"/><path d="m2 2 20 20"/>',
    terminal: '<path d="m4 17 6-6-6-6M12 19h8"/>',
  };

  function iconSvg(name, className) {
    const paths = iconPaths[name] || iconPaths.warning;
    return `<svg class="ui-icon${className ? ` ${escapeHtml(className)}` : ""}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${paths}</svg>`;
  }

  function hydrateIcons(root) {
    const icons = [];
    if (root.matches && root.matches(".material-symbols-outlined")) icons.push(root);
    if (root.querySelectorAll) icons.push(...root.querySelectorAll(".material-symbols-outlined"));
    icons.forEach((element) => {
      const name = element.textContent.trim();
      const className = Array.from(element.classList).filter((value) => value !== "material-symbols-outlined").join(" ");
      const template = document.createElement("template");
      template.innerHTML = iconSvg(name, className);
      element.replaceWith(template.content.firstElementChild);
    });
  }

  async function api(path, options) {
    const request = Object.assign({ credentials: "same-origin" }, options || {});
    request.headers = Object.assign({}, request.headers || {});
    if (request.body && typeof request.body !== "string") {
      request.headers["Content-Type"] = "application/json";
      request.body = JSON.stringify(request.body);
    }
    if (request.method && request.method !== "GET" && state.csrfToken) request.headers["X-CSRF-Token"] = state.csrfToken;
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
    navigate(state.activeView);
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
    item.innerHTML = `<span class="material-symbols-outlined" aria-hidden="true">${kind === "error" ? "error" : kind === "warning" ? "schedule" : "check_circle"}</span><div class="toast-message">${escapeHtml(message)}</div>`;
    $("toastRegion").appendChild(item);
    window.setTimeout(() => item.remove(), 4800);
  }

  function formatTime(value, includeYear) {
    if (!value) return "—";
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return text(value);
    const options = { hour12: false, month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit" };
    if (includeYear) options.year = "numeric";
    return date.toLocaleString("zh-CN", options).replace(/\//g, "-");
  }

  function relativeTime(value) {
    if (!value) return "—";
    const delta = Math.max(0, Date.now() - new Date(value).getTime());
    if (!Number.isFinite(delta)) return formatTime(value);
    if (delta < 10000) return "刚刚";
    if (delta < 60000) return `${Math.floor(delta / 1000)} 秒前`;
    if (delta < 3600000) return `${Math.floor(delta / 60000)} 分钟前`;
    if (delta < 86400000) return `${Math.floor(delta / 3600000)} 小时前`;
    return `${Math.floor(delta / 86400000)} 天前`;
  }

  function formatBytes(value) {
    const number = Number(value);
    if (!Number.isFinite(number) || number < 0) return "—";
    if (number < 1024 * 1024) return `${Math.round(number / 1024)} KB`;
    if (number < 1024 * 1024 * 1024) return `${(number / 1024 / 1024).toFixed(1)} MB`;
    return `${(number / 1024 / 1024 / 1024).toFixed(1)} GB`;
  }

  function formatUptime(value) {
    const seconds = Number(value);
    if (!Number.isFinite(seconds) || seconds < 0) return "—";
    const days = Math.floor(seconds / 86400);
    const hours = Math.floor((seconds % 86400) / 3600);
    const minutes = Math.floor((seconds % 3600) / 60);
    return days ? `${days}d ${hours}h` : `${hours}h ${minutes}m`;
  }

  function nodeStatus(node) { return node.online ? "online" : (node.last_seen_at ? "offline" : "unknown"); }
  function statusValue(node, key) { return node[key] || (node.status && node.status[key]) || "unknown"; }
  function badge(value) {
    const key = text(value, "unknown");
    return `<span class="status-badge status-${escapeHtml(key)}">${escapeHtml(statusLabels[key] || key)}</span>`;
  }

  function navigate(view, nodeId) {
    if (view === "detail") {
      state.activeNodeId = nodeId || state.activeNodeId;
      if (!state.nodes.some((node) => node.id === state.activeNodeId)) view = "nodes";
    }
    state.activeView = view;
    const viewMap = { nodes: "nodesView", detail: "nodeDetailView", audit: "auditView", tasks: "tasksView" };
    Object.entries(viewMap).forEach(([key, id]) => { $(id).hidden = key !== view; });
    document.querySelectorAll("[data-view]").forEach((button) => {
      const active = button.dataset.view === view || (view === "detail" && button.dataset.view === "nodes");
      if (button.classList.contains("nav-button")) {
        button.classList.toggle("is-active", active);
        if (active) button.setAttribute("aria-current", "page"); else button.removeAttribute("aria-current");
      }
      if (button.classList.contains("tab-button")) {
        button.classList.toggle("is-active", button.dataset.view === view);
        button.setAttribute("aria-selected", button.dataset.view === view ? "true" : "false");
      }
    });
    if (view === "detail") renderNodeDetail();
    if (view === "audit") renderAudit();
    if (view === "tasks") renderTasks();
    window.scrollTo({ top: 0, behavior: "smooth" });
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
    $("topAgentCount").textContent = nodes.length.toLocaleString("en-US");
    const values = [online, warp, xui];
    document.querySelectorAll(".signal-cell").forEach((cell, index) => {
      const bar = cell.querySelector(".signal-progress i");
      if (bar) bar.style.width = `${nodes.length ? Math.round(values[index] * 100 / nodes.length) : 0}%`;
    });
  }

  function filteredNodes() {
    const query = $("nodeSearch").value.trim().toLowerCase();
    const status = $("statusFilter").value;
    const warp = $("warpFilter").value;
    const xui = $("xuiFilter").value;
    return state.nodes.filter((node) => {
      const ip = node.public_ipv4 || node.egress_ipv4 || (node.status && node.status.public_ipv4) || "";
      const searchable = `${node.name || ""} ${node.id || ""} ${node.region || ""} ${ip}`.toLowerCase();
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
    $("navActivityCount").textContent = String(state.tasks.length);
    $("navAuditCount").textContent = String(state.actions.length);
    $("selectionCount").textContent = `已选 ${state.selected.size} 个`;
    $("selectionTools").hidden = state.selected.size === 0;
    $("batchButton").disabled = state.selected.size === 0;
    if (!state.nodes.length) {
      $("nodeState").hidden = false;
      $("nodeState").className = "state-message is-empty";
      $("nodeState").innerHTML = `<span class="material-symbols-outlined" aria-hidden="true">dns</span><strong>还没有节点</strong><span>添加节点并完成 Agent 注册后，状态会显示在这里。</span>`;
      $("nodeTableWrap").hidden = true;
      return;
    }
    if (!nodes.length) {
      $("nodeState").hidden = false;
      $("nodeState").className = "state-message is-empty";
      $("nodeState").innerHTML = `<span class="material-symbols-outlined" aria-hidden="true">filter_alt_off</span><strong>没有匹配的节点</strong><span>调整搜索词或筛选条件后重试。</span>`;
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
      const selected = state.selected.has(node.id) ? " checked" : "";
      const disabled = node.online ? "" : " disabled";
      const tags = (node.tags || []).slice(0, 2).map((tag) => `<span class="node-tag">${escapeHtml(tag)}</span>`).join("");
      const ip = node.public_ipv4 || node.egress_ipv4 || (node.status && node.status.public_ipv4);
      return `<tr data-node-id="${escapeHtml(node.id)}">
        <td><label class="checkbox-label" title="选择在线节点"><input class="node-select" type="checkbox" data-node-id="${escapeHtml(node.id)}"${selected}${disabled} aria-label="选择 ${escapeHtml(node.name)}"><span class="custom-checkbox" aria-hidden="true"></span></label></td>
        <td><button class="node-cell node-link" data-open-node="${escapeHtml(node.id)}" type="button"><span class="node-avatar${node.online ? "" : " is-offline"}" aria-hidden="true"><span class="material-symbols-outlined">${node.online ? "public" : "cloud_off"}</span></span><span class="node-cell-copy"><span class="node-name-line"><span class="node-name">${escapeHtml(node.name)}</span>${tags}</span><span class="node-meta">${escapeHtml(node.region || "未设置地区")} · ${escapeHtml(node.id.slice(0, 12))}</span></span></button></td>
        <td><span class="ip-value ${ip ? "" : "is-unknown"}">${escapeHtml(ip || "未采集")}</span></td>
        <td class="service-cell"><div class="service-badges">${badge(statusValue(node, "warp_status"))}${badge(statusValue(node, "xui_status"))}</div></td>
        <td><div class="resource-cell"><div class="resource-line"><span>CPU ${cpu.toFixed(1)}%</span><span>MEM ${memoryPercent}%</span></div><div class="resource-bars"><span><i style="width:${cpu}%"></i></span><span><i class="memory" style="width:${memoryPercent}%"></i></span></div><div class="resource-line resource-caption"><span>${formatBytes(memoryUsed)} / ${formatBytes(memoryTotal)}</span><span>${escapeHtml(node.agent_version || "Agent —")}</span></div></div></td>
        <td><span class="uptime-value ${node.uptime_seconds ? "" : "is-unknown"}">${escapeHtml(formatUptime(node.uptime_seconds))}</span></td>
        <td><span class="heartbeat-value ${node.online ? "" : "is-offline"}">${escapeHtml(relativeTime(node.last_seen_at))}</span><br>${badge(nodeStatus(node))}</td>
        <td class="action-column"><button class="row-detail-button" data-open-node="${escapeHtml(node.id)}" type="button" title="查看节点详情" aria-label="查看 ${escapeHtml(node.name)} 的详情"><span class="material-symbols-outlined" aria-hidden="true">more_vert</span></button></td>
      </tr>`;
    }).join("");
    const visibleIds = new Set(nodes.map((node) => node.id));
    state.selected.forEach((id) => { if (!visibleIds.has(id)) state.selected.delete(id); });
    const selectable = nodes.filter((node) => node.online);
    $("selectAllNodes").checked = selectable.length > 0 && selectable.every((node) => state.selected.has(node.id));
    $("selectAllNodes").indeterminate = selectable.some((node) => state.selected.has(node.id)) && !$("selectAllNodes").checked;
  }

  function renderNodeDetail() {
    const node = state.nodes.find((item) => item.id === state.activeNodeId);
    if (!node) { navigate("nodes"); return; }
    $("detailTitle").textContent = node.name;
    const memoryUsed = Number(node.memory_used_bytes || 0);
    const memoryTotal = Number(node.memory_total_bytes || 0);
    const memoryPercent = memoryTotal ? Math.round(memoryUsed * 100 / memoryTotal) : 0;
    const cpu = Math.max(0, Math.min(100, Number(node.cpu_percent || 0)));
    const diskUsed = Number(node.root_used_bytes || 0);
    const diskTotal = Number(node.root_total_bytes || 0);
    const diskPercent = diskTotal ? Math.round(diskUsed * 100 / diskTotal) : 0;
    const recent = state.actions.filter((item) => item.node_id === node.id).slice(0, 8);
    const installed = Boolean(node.agent_version || node.last_seen_at);
    const upgradeSupported = supportsPanelUpgrade(node.agent_version);
    const agentButton = installed
      ? `<button class="button button-quiet button-small" type="button" data-action="upgrade_agent" data-node-id="${escapeHtml(node.id)}"${!node.online || state.busy.has(node.id) || !upgradeSupported ? " disabled" : ""} title="${upgradeSupported ? "从 GitHub Release 校验并升级 Agent" : "Agent 0.3.8 起支持面板直升"}"><span class="button-icon material-symbols-outlined" aria-hidden="true">system_update_alt</span>升级 Agent</button>`
      : `<button class="button button-quiet button-small" type="button" data-enroll-node="${escapeHtml(node.id)}"><span class="button-icon material-symbols-outlined" aria-hidden="true">terminal</span>生成安装命令</button>`;
    const warpAction = statusValue(node, "warp_status") === "on" ? "warp_off" : "warp_on";
    const actions = ["change_ip", warpAction, "restart_xui", "get_status", "get_ip"];
    $("detailContent").innerHTML = `
      <section class="detail-hero"><div class="detail-identity"><span class="detail-node-icon"><span class="material-symbols-outlined" aria-hidden="true">dns</span><i class="${node.online ? "is-online" : ""}"></i></span><div><div class="detail-title-line"><h2>${escapeHtml(node.name)}</h2>${badge(nodeStatus(node))}</div><div class="detail-id">ID: ${escapeHtml(node.id)}</div></div></div><div class="detail-hero-actions"><button class="button button-quiet button-small" data-edit-node="${escapeHtml(node.id)}" type="button"><span class="button-icon material-symbols-outlined" aria-hidden="true">edit</span>编辑节点</button><button class="button button-quiet" data-action="get_status" data-node-id="${escapeHtml(node.id)}" type="button"${!node.online || state.busy.has(node.id) ? " disabled" : ""}><span class="button-icon material-symbols-outlined" aria-hidden="true">sync</span>同步状态</button></div></section>
      <div class="detail-layout"><div class="detail-primary">
        <section class="detail-metric-grid"><div class="metric-card"><div class="metric-card-heading"><span><span class="material-symbols-outlined" aria-hidden="true">memory</span>CPU 使用率</span><strong>${cpu.toFixed(1)}%</strong></div><span class="metric-track"><i style="width:${cpu}%"></i></span></div><div class="metric-card"><div class="metric-card-heading"><span><span class="material-symbols-outlined" aria-hidden="true">developer_board</span>内存使用</span><strong>${formatBytes(memoryUsed)} / ${formatBytes(memoryTotal)}</strong></div><span class="metric-track is-blue"><i style="width:${memoryPercent}%"></i></span></div><div class="metric-card"><div class="metric-card-heading"><span><span class="material-symbols-outlined" aria-hidden="true">hard_drive</span>根分区</span><strong>${diskPercent}%</strong></div><span class="metric-track is-tertiary"><i style="width:${diskPercent}%"></i></span></div></section>
        <section class="detail-panel"><div class="detail-section-heading"><h3>快捷操作</h3><span class="table-meta">仅固定 Action 白名单</span></div><div class="quick-action-grid">${actions.map((action) => `<button type="button" class="quick-action${["warp_off", "change_ip", "restart_xui"].includes(action) ? " is-state-changing" : ""}" data-action="${action}" data-node-id="${escapeHtml(node.id)}"${!node.online || state.busy.has(node.id) ? " disabled" : ""}><span class="material-symbols-outlined" aria-hidden="true">${actionIcons[action]}</span><strong>${escapeHtml(actionLabels[action])}</strong><small>${action === "get_status" || action === "get_ip" ? "读取状态" : "记录到审计"}</small></button>`).join("")}</div></section>
        <section class="detail-panel node-information"><div class="detail-section-heading"><h3>节点信息</h3><div class="detail-section-actions">${agentButton}<span class="table-meta">最后更新：${escapeHtml(relativeTime(node.last_seen_at))}</span></div></div><dl><div><dt>公共 IPv4</dt><dd>${escapeHtml(node.public_ipv4 || "未采集")}</dd></div><div><dt>公共 IPv6</dt><dd>${escapeHtml(node.public_ipv6 || "未采集")}</dd></div><div><dt>Agent 版本</dt><dd>${escapeHtml(node.agent_version || "—")}</dd></div><div><dt>系统 / 架构</dt><dd>${escapeHtml(`${node.os_name || "—"} ${node.os_version || ""} / ${node.architecture || "—"}`)}</dd></div><div><dt>WARP 接口</dt><dd>${badge(statusValue(node, "warp_status"))}</dd></div><div><dt>3x-ui 服务</dt><dd>${badge(statusValue(node, "xui_status"))} ${escapeHtml(node.xui_service || "x-ui")}</dd></div><div><dt>运行时间</dt><dd>${escapeHtml(formatUptime(node.uptime_seconds))}</dd></div><div><dt>地区</dt><dd>${escapeHtml(node.region || "未设置")}</dd></div></dl></section>
        <section class="detail-panel ip-history-launch-panel"><div class="detail-section-heading"><div><h3>出口 IP 历史</h3><span class="table-meta">最多保留 100 条</span></div><button class="button button-quiet button-small" type="button" data-open-ip-history="${escapeHtml(node.id)}"><span class="button-icon material-symbols-outlined" aria-hidden="true">history</span>查看历史</button></div></section>
      </div><aside class="detail-secondary"><section class="detail-panel recent-operations"><div class="detail-section-heading"><h3>最近操作</h3><button class="text-button" type="button" data-view="audit">查看全部</button></div><div class="timeline">${recent.length ? recent.map((item) => `<div class="timeline-item status-${escapeHtml(item.status)}"><i aria-hidden="true"></i><div><div><strong>${escapeHtml(actionLabels[item.action] || item.action)}</strong><time>${escapeHtml(relativeTime(item.created_at))}</time></div><p>${escapeHtml(item.error_message || (item.result ? "Agent 已返回结果。" : "等待 Agent 回执。"))}</p>${badge(item.status)}</div></div>`).join("") : `<div class="detail-empty">暂无操作记录。</div>`}</div></section><section class="danger-zone"><div><span class="material-symbols-outlined" aria-hidden="true">warning</span><h3>危险操作</h3></div><p>删除节点会吊销凭据、停止关联任务，并从控制平面移除节点。</p><button class="button button-danger button-wide" type="button" data-delete-node="${escapeHtml(node.id)}">删除节点</button></section></aside></div>`;
  }

  function supportsPanelUpgrade(version) {
    const parts = String(version || "").split(".").map((value) => Number.parseInt(value, 10));
    if (parts.length < 3 || parts.some((value) => !Number.isInteger(value))) return false;
    return parts[0] > 0 || parts[1] > 3 || (parts[1] === 3 && parts[2] >= 8);
  }

  function renderIpHistoryRows(history, errorMessage) {
    if (errorMessage) return `<div class="history-empty is-error"><span class="material-symbols-outlined" aria-hidden="true">error</span><div><strong>历史记录读取失败</strong><span>${escapeHtml(errorMessage)}</span></div></div>`;
    if (history === undefined) return `<div class="history-empty is-loading"><span class="state-spinner" aria-hidden="true"></span><div><strong>正在读取历史记录</strong><span>请稍候。</span></div></div>`;
    if (!history.length) return `<div class="history-empty"><span class="material-symbols-outlined" aria-hidden="true">history</span><div><strong>暂无 IP 变更记录</strong><span>执行切换出口 IP 后，记录会显示在这里。</span></div></div>`;
    return `<div class="ip-history-list">${history.map((item) => `<article class="ip-history-item ${item.success ? "is-success" : "is-failed"}"><span class="ip-history-marker" aria-hidden="true"></span><div class="ip-history-addresses"><code>${escapeHtml(item.old_ip || "未知")}</code><span class="material-symbols-outlined" aria-hidden="true">arrow_forward</span><code>${escapeHtml(item.new_ip || "未切换")}</code></div><div class="ip-history-meta"><span>${item.success ? "切换成功" : escapeHtml(item.error_message || item.message || "切换失败")}</span><time>${escapeHtml(formatTime(item.created_at, true))}</time>${item.attempts ? `<small>${escapeHtml(item.attempts)} 次尝试${item.duration_ms ? ` · ${escapeHtml(Math.max(1, Math.round(item.duration_ms / 1000)))} 秒` : ""}</small>` : ""}</div></article>`).join("")}</div>`;
  }

  function renderIpHistoryDialog(nodeId, errorMessage) {
    const node = state.nodes.find((item) => item.id === nodeId);
    if (!node) return;
    const history = state.ipHistory[nodeId];
    $("ipHistoryDialogMessage").textContent = node.name;
    $("ipHistoryDialogContent").classList.toggle("has-history", Boolean(history && history.length));
    $("ipHistoryDialogContent").innerHTML = renderIpHistoryRows(history, errorMessage);
    hydrateIcons($("ipHistoryDialogContent"));
  }

  async function openIpHistory(nodeId) {
    if (!state.nodes.some((node) => node.id === nodeId)) return;
    renderIpHistoryDialog(nodeId);
    openDialog("ipHistoryDialog");
    await loadIpHistory(nodeId);
  }

  async function loadIpHistory(nodeId) {
    if (!nodeId) return;
    try {
      const response = await api(`/api/nodes/${encodeURIComponent(nodeId)}/ip-history`);
      state.ipHistory[nodeId] = response.history || [];
      if (state.activeView === "detail" && state.activeNodeId === nodeId) renderNodeDetail();
      if ($("ipHistoryDialog").open) renderIpHistoryDialog(nodeId);
    } catch (error) {
      if ($("ipHistoryDialog").open) renderIpHistoryDialog(nodeId, error.message);
      notify(`IP 历史读取失败：${error.message}`, "error");
    }
  }

  function filteredActions() {
    const query = $("auditSearch").value.trim().toLowerCase();
    if (!query) return state.actions;
    return state.actions.filter((item) => `${item.action || ""} ${item.node_id || ""} ${item.id || ""} ${item.status || ""} ${item.error_message || ""}`.toLowerCase().includes(query));
  }

  function renderAudit() {
    $("actionCount").textContent = String(state.actions.length);
    const actions = filteredActions();
    const failures = state.actions.filter((item) => ["failed", "timed_out", "expired", "skipped_offline"].includes(item.status)).length;
    const active = state.actions.filter((item) => ["queued", "dispatched", "accepted", "executing", "running_action"].includes(item.status)).length;
    const succeeded = state.actions.filter((item) => item.status === "succeeded").length;
    const completed = succeeded + failures;
    const successRate = completed ? Math.round(succeeded * 100 / completed) : 100;
    $("auditSummary").innerHTML = `<div><span>全部操作</span><strong>${state.actions.length.toLocaleString("zh-CN")}</strong><small>当前保留记录</small></div><div><span>失败执行</span><strong class="is-error">${failures}</strong><small>需要复核</small></div><div><span>正在执行</span><strong class="is-blue">${active}</strong><small>排队或处理中</small></div><div><span>成功率</span><strong class="is-primary">${successRate}%</strong><small>已完成请求</small></div>`;
    $("activityContent").innerHTML = actions.length ? `<div class="audit-table-wrap"><table class="audit-table"><thead><tr><th>操作</th><th>节点 ID</th><th>状态</th><th>时间</th><th>消息</th></tr></thead><tbody>${actions.map((item) => `<tr class="${item.status === "failed" ? "is-failed" : ""}"><td><div class="audit-action"><span class="material-symbols-outlined" aria-hidden="true">${actionIcons[item.action] || "terminal"}</span><strong>${escapeHtml(actionLabels[item.action] || item.action)}</strong></div></td><td><code>${escapeHtml((item.node_id || "—").slice(0, 18))}</code></td><td>${badge(item.status)}</td><td><time>${escapeHtml(formatTime(item.created_at, true))}</time></td><td><span class="audit-message">${escapeHtml(item.error_message || (item.result ? "Agent 已返回结果" : "等待 Agent 回执"))}</span></td></tr>`).join("")}</tbody></table></div>` : `<div class="activity-empty"><span class="material-symbols-outlined" aria-hidden="true">manage_search</span><strong>没有匹配的操作记录</strong><span>调整筛选词后重试。</span></div>`;
  }

  function renderTasks() {
    $("taskCount").textContent = String(state.tasks.length);
    $("topTaskCount").textContent = state.tasks.filter((task) => task.enabled).length.toLocaleString("en-US");
    $("tasksContent").innerHTML = state.tasks.length ? `<div class="task-list">${state.tasks.map((task) => `<article class="task-row"><div class="task-status-icon ${task.enabled ? "is-enabled" : ""}"><span class="material-symbols-outlined" aria-hidden="true">${task.enabled ? "schedule" : "pause"}</span></div><div class="task-copy"><strong>${escapeHtml(task.name)}</strong><span>${escapeHtml(actionLabels[task.action] || task.action)} · ${task.node_ids ? task.node_ids.length : 0} 个节点</span></div><div class="task-schedule"><span>${escapeHtml(task.schedule_type)}</span><strong>${escapeHtml(task.schedule_value)}</strong><small>${escapeHtml(task.timezone)}</small></div><div class="task-next"><span>下次执行</span><strong>${escapeHtml(formatTime(task.next_run_at, true))}</strong></div><label class="switch-control" title="${task.enabled ? "停用任务" : "启用任务"}"><input type="checkbox" data-task-toggle="${escapeHtml(task.id)}"${task.enabled ? " checked" : ""}><span></span></label><button class="icon-button" type="button" data-task-delete="${escapeHtml(task.id)}" title="删除任务" aria-label="删除 ${escapeHtml(task.name)}"><span class="material-symbols-outlined" aria-hidden="true">delete</span></button></article>`).join("")}</div>` : `<div class="activity-empty"><span class="material-symbols-outlined" aria-hidden="true">calendar_add_on</span><strong>还没有定时任务</strong><span>新建任务后，调度器会按指定时区执行固定 Action。</span><button class="button button-primary" type="button" data-open-task-form>新建任务</button></div>`;
  }

  function confirmAction(message, details, callback, options) {
    state.confirmAction = callback;
    $("confirmTitle").textContent = options && options.title ? options.title : "确认操作";
    $("confirmMessage").textContent = message;
    $("confirmDetails").innerHTML = details || "";
    $("confirmProceed").textContent = options && options.proceed ? options.proceed : "确认执行";
    openDialog("confirmDialog");
  }

  async function submitAction(nodeId, action) {
    const previousVersion = (state.nodes.find((node) => node.id === nodeId) || {}).agent_version;
    state.busy.add(nodeId);
    if (state.activeView === "detail") renderNodeDetail();
    try {
      const response = await api(`/api/nodes/${encodeURIComponent(nodeId)}/actions`, { method: "POST", body: { action, parameters: {}, queue_if_offline: false } });
      notify(`${actionLabels[action]}：请求已提交`, "warning");
      await pollRequest(response.request && response.request.id, nodeId, action, previousVersion);
    } catch (error) { notify(`${actionLabels[action]}：${error.message}`, "error"); }
    finally { state.busy.delete(nodeId); await refreshData(false); }
  }

  async function executeAction(nodeId, action, skipConfirmation) {
    const node = state.nodes.find((item) => item.id === nodeId);
    if (!node || state.busy.has(nodeId)) return;
    const dangerous = ["warp_on", "warp_off", "change_ip", "restart_xui", "upgrade_agent"].includes(action);
    const run = () => submitAction(nodeId, action);
    if (dangerous && !skipConfirmation) {
      const upgradeDetail = action === "upgrade_agent" ? "<li>升级包仅从项目 GitHub Release 下载并校验，完成后 Agent 会自动重启。</li>" : "";
      confirmAction(`${node.name} 将执行“${actionLabels[action]}”。`, `<ul><li>状态变更会在该节点串行执行。</li><li>请求和结果会写入操作记录。</li>${upgradeDetail}</ul>`, run, { title: actionLabels[action], proceed: "确认执行" });
    } else await run();
  }

  async function pollRequest(requestId, nodeId, action, previousVersion) {
    if (!requestId) return;
    for (let attempt = 0; attempt < 260; attempt += 1) {
      await new Promise((resolve) => window.setTimeout(resolve, 700));
      try {
        const response = await api(`/api/action-requests/${encodeURIComponent(requestId)}`);
        const request = response.request;
        if (["succeeded", "failed", "timed_out", "expired", "skipped_offline", "canceled"].includes(request.status)) {
          notify(request.status === "succeeded" ? "操作已成功" : (request.error_message || `操作状态：${request.status}`), request.status === "succeeded" ? "ok" : "error");
          await refreshData(false);
          if (action === "change_ip") await loadIpHistory(nodeId);
          if (action === "upgrade_agent" && request.status === "succeeded" && request.result && request.result.changed) {
            await waitForAgentUpgrade(nodeId, previousVersion);
          }
          return;
        }
      } catch (_) {
        if (attempt >= 259) return;
      }
    }
  }

  async function waitForAgentUpgrade(nodeId, previousVersion) {
    for (let attempt = 0; attempt < 30; attempt += 1) {
      await new Promise((resolve) => window.setTimeout(resolve, 1000));
      try {
        const response = await api(`/api/nodes/${encodeURIComponent(nodeId)}`);
        const node = response.node;
        const index = state.nodes.findIndex((item) => item.id === nodeId);
        if (index >= 0) state.nodes[index] = node;
        if (state.activeView === "detail" && state.activeNodeId === nodeId) renderNodeDetail();
        if (node.agent_version && node.agent_version !== previousVersion) return;
      } catch (_) {
        // The Agent normally disconnects briefly while its service restarts.
      }
    }
  }

  function deleteNode(nodeId) {
    const node = state.nodes.find((item) => item.id === nodeId);
    if (!node) return;
    confirmAction(`确定删除节点“${node.name}”？`, `<ul><li>Agent 凭据会被立即吊销。</li><li>关联任务会停止，节点从清单移除。</li><li>此操作无法撤销。</li></ul>`, async () => {
      try {
        await api(`/api/nodes/${encodeURIComponent(nodeId)}`, { method: "DELETE" });
        notify("节点已删除"); state.activeNodeId = ""; await refreshData(false); navigate("nodes");
      } catch (error) { notify(`删除失败：${error.message}`, "error"); }
    }, { title: "删除节点", proceed: "永久删除" });
  }

  async function refreshData(showLoading) {
    if (showLoading) {
      $("nodeState").hidden = false;
      $("nodeState").className = "state-message";
      $("nodeState").innerHTML = `<span class="state-spinner" aria-hidden="true"></span>正在读取节点数据……`;
    }
    try {
      const [nodeResponse, actionResponse, taskResponse, health] = await Promise.all([api("/api/nodes"), api("/api/actions"), api("/api/tasks"), api("/api/health")]);
      state.nodes = nodeResponse.nodes || nodeResponse.items || [];
      state.actions = actionResponse.requests || actionResponse.items || [];
      state.tasks = taskResponse.tasks || taskResponse.items || [];
      setHealth(Boolean(health.ok), `${health.online_agents || 0} 个 Agent 在线`);
      const now = new Date().toISOString();
      $("lastUpdated").textContent = `数据更新时间：${formatTime(now)}`;
      $("topLastSync").textContent = "刚刚";
      $("viewStatusLine").textContent = `最近同步 ${formatTime(now)} · ${state.nodes.length} 个节点`;
      renderNodes(); renderTasks();
      if (state.activeView === "detail") renderNodeDetail();
      if (state.activeView === "audit") renderAudit();
    } catch (error) {
      if (error.status === 401) { showLogin("登录已失效，请重新登录。"); return; }
      $("nodeState").hidden = false;
      $("nodeState").className = "state-message is-error";
      $("nodeState").innerHTML = `<strong>节点数据读取失败</strong><span class="state-detail">${escapeHtml(error.message)}</span><button class="button button-quiet state-action" type="button" id="retryDataButton">重新读取</button>`;
      $("retryDataButton").addEventListener("click", () => refreshData(true), { once: true });
      setHealth(false, error.message);
    }
  }

  function openDialog(id) {
    const dialog = $(id);
    if (typeof dialog.showModal === "function") dialog.showModal(); else dialog.hidden = false;
  }
  function closeDialog(id) {
    const dialog = $(id);
    if (dialog.open) dialog.close(); else dialog.hidden = true;
  }
  function openTaskForm() {
    if (!state.nodes.length) { notify("请先添加节点。", "warning"); return; }
    $("taskNodes").innerHTML = state.nodes.map((node) => `<option value="${escapeHtml(node.id)}">${escapeHtml(node.name)}</option>`).join("");
    $("taskForm").reset();
    $("taskTimezone").value = "Asia/Shanghai";
    $("taskScheduleValue").value = "03:00";
    $("taskFormMessage").textContent = "";
    openDialog("taskDialog");
  }
  function exportAudit() {
    const rows = [["action", "node_id", "status", "created_at", "message"], ...filteredActions().map((item) => [actionLabels[item.action] || item.action, item.node_id, statusLabels[item.status] || item.status, item.created_at, item.error_message || ""])];
    const csv = rows.map((row) => row.map((value) => `"${text(value, "").replace(/"/g, '""')}"`).join(",")).join("\r\n");
    const link = document.createElement("a");
    link.href = URL.createObjectURL(new Blob(["\ufeff", csv], { type: "text/csv;charset=utf-8" }));
    link.download = `vps-tool-audit-${new Date().toISOString().slice(0, 10)}.csv`;
    link.click();
    window.setTimeout(() => URL.revokeObjectURL(link.href), 1000);
  }

  hydrateIcons(document);
  const iconObserver = new MutationObserver((mutations) => {
    mutations.forEach((mutation) => mutation.addedNodes.forEach((node) => {
      if (node.nodeType === Node.ELEMENT_NODE) hydrateIcons(node);
    }));
  });
  iconObserver.observe(document.body, { childList: true, subtree: true });

  async function boot() {
    try { const health = await api("/api/health"); setHealth(Boolean(health.ok), `${health.online_agents || 0} 个 Agent 在线`); }
    catch (_) { setHealth(false, "主控不可达"); }
    try {
      const me = await api("/api/auth/me");
      state.user = { username: me.username }; state.csrfToken = me.csrf_token || "";
      showDashboard(); await refreshData(true);
    } catch (_) { showLogin(); }
  }

  $("loginForm").addEventListener("submit", async (event) => {
    event.preventDefault();
    const button = $("loginButton"); button.disabled = true; $("loginMessage").textContent = "正在验证……";
    try {
      const response = await api("/api/auth/login", { method: "POST", body: { username: $("username").value.trim(), password: $("password").value } });
      state.user = response.user; state.csrfToken = response.csrf_token || ""; $("password").value = ""; $("loginMessage").textContent = "";
      showDashboard(); await refreshData(true);
    } catch (error) { $("loginMessage").textContent = error.message; } finally { button.disabled = false; }
  });
  $("passwordToggle").addEventListener("click", () => {
    const input = $("password"); const visible = input.type === "password";
    input.type = visible ? "text" : "password";
    $("passwordToggle").innerHTML = iconSvg(visible ? "visibility" : "visibility_off");
  });
  $("logoutButton").addEventListener("click", async () => {
    try { await api("/api/auth/logout", { method: "POST" }); } catch (_) { /* Session may already be gone. */ }
    state.csrfToken = ""; state.user = null; state.activeView = "nodes"; showLogin("已退出登录。");
  });
  $("refreshButton").addEventListener("click", () => refreshData(true));
  $("globalSearch").addEventListener("input", () => { $("nodeSearch").value = $("globalSearch").value; navigate("nodes"); renderNodes(); });
  $("nodeSearch").addEventListener("input", () => { $("globalSearch").value = $("nodeSearch").value; renderNodes(); });
  ["statusFilter", "warpFilter", "xuiFilter"].forEach((id) => $(id).addEventListener("input", renderNodes));
  document.querySelectorAll("[data-view]").forEach((button) => button.addEventListener("click", () => navigate(button.dataset.view)));

  $("nodesBody").addEventListener("click", (event) => {
    const detail = event.target.closest("[data-open-node]");
    if (detail) navigate("detail", detail.dataset.openNode);
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
    confirmAction(`将对 ${nodes.length} 个在线节点执行批量操作。`, `<label class="field-label" for="batchActionSelect">固定 Action</label><select id="batchActionSelect">${options}</select>`, async () => {
      const action = $("batchActionSelect").value;
      for (const node of nodes) await executeAction(node.id, action, true);
      state.selected.clear(); renderNodes();
    }, { title: "批量操作", proceed: "开始批量执行" });
  });
  $("detailContent").addEventListener("click", (event) => {
    const historyButton = event.target.closest("[data-open-ip-history]");
    if (historyButton) {
      void openIpHistory(historyButton.dataset.openIpHistory);
      return;
    }
    const edit = event.target.closest("[data-edit-node]");
    if (edit) {
      const node = state.nodes.find((item) => item.id === edit.dataset.editNode);
      if (node) openNodeEditor(node);
      return;
    }
    const enroll = event.target.closest("[data-enroll-node]");
    if (enroll) {
      const node = state.nodes.find((item) => item.id === enroll.dataset.enrollNode);
      if (!node) return;
      enroll.disabled = true;
      api(`/api/nodes/${encodeURIComponent(node.id)}/enrollment-token`, { method: "POST" })
        .then((response) => {
          $("registrationToken").textContent = response.registration_token;
          $("tokenExpiry").textContent = `有效期至：${formatTime(response.registration_token_expires_at, true)}`;
          $("agentInstallCommand").textContent = buildAgentInstallCommand(node, response.registration_token);
          openDialog("tokenDialog");
        })
        .catch((error) => notify(`生成安装命令失败：${error.message}`, "error"))
        .finally(() => { enroll.disabled = false; });
      return;
    }
    const action = event.target.closest("[data-action]");
    if (action) executeAction(action.dataset.nodeId, action.dataset.action);
    const deleteButton = event.target.closest("[data-delete-node]");
    if (deleteButton) deleteNode(deleteButton.dataset.deleteNode);
    const viewButton = event.target.closest("[data-view]");
    if (viewButton) navigate(viewButton.dataset.view);
  });
  $("closeDrawerButton").addEventListener("click", () => navigate("nodes"));
  $("auditSearch").addEventListener("input", renderAudit);
  $("exportAuditButton").addEventListener("click", exportAudit);

  $("tasksContent").addEventListener("change", async (event) => {
    const toggle = event.target.closest("[data-task-toggle]");
    if (!toggle) return;
    toggle.disabled = true;
    try { await api(`/api/tasks/${encodeURIComponent(toggle.dataset.taskToggle)}`, { method: "PATCH", body: { enabled: toggle.checked } }); notify(toggle.checked ? "任务已启用" : "任务已停用"); await refreshData(false); }
    catch (error) { toggle.checked = !toggle.checked; notify(`任务更新失败：${error.message}`, "error"); }
    finally { toggle.disabled = false; }
  });
  $("tasksContent").addEventListener("click", (event) => {
    if (event.target.closest("[data-open-task-form]")) openTaskForm();
    const button = event.target.closest("[data-task-delete]");
    if (!button) return;
    const task = state.tasks.find((item) => item.id === button.dataset.taskDelete);
    confirmAction(`确定删除任务“${task ? task.name : ""}”？`, "<p>任务将停止调度，既有审计记录不会删除。</p>", async () => {
      try { await api(`/api/tasks/${encodeURIComponent(button.dataset.taskDelete)}`, { method: "DELETE" }); notify("任务已删除"); await refreshData(false); }
      catch (error) { notify(`删除失败：${error.message}`, "error"); }
    }, { title: "删除任务", proceed: "删除任务" });
  });

  $("addNodeButton").addEventListener("click", () => {
    state.nodeFormMode = "create"; state.editingNodeId = "";
    $("nodeForm").reset(); $("nodeXuiService").value = "x-ui";
    $("nodeDialogTitle").textContent = "添加节点"; $("nodeDialogKicker").textContent = "FLEET / ENROLLMENT";
    $("nodeDialogMessage").textContent = "创建后会生成一个短时有效、仅显示一次的注册 Token。";
    $("nodeFormSubmit").textContent = "创建并生成 Token"; $("nodeFormMessage").textContent = "";
    openDialog("nodeDialog");
  });
  function openNodeEditor(node) {
    state.nodeFormMode = "edit"; state.editingNodeId = node.id;
    $("nodeName").value = node.name || ""; $("nodeRegion").value = node.region || "";
    $("nodeTags").value = (node.tags || []).join(", "); $("nodeWarpAdapter").value = node.warp_adapter || "generic";
    $("nodeXuiService").value = node.xui_service || "x-ui"; $("nodeNotes").value = node.notes || "";
    $("nodeDialogTitle").textContent = "编辑节点"; $("nodeDialogKicker").textContent = "NODE / SETTINGS";
    $("nodeDialogMessage").textContent = "修改节点信息不会更换 Agent 凭据或注册 Token。";
    $("nodeFormSubmit").textContent = "保存修改"; $("nodeFormMessage").textContent = "";
    openDialog("nodeDialog");
  }
  $("nodeForm").addEventListener("submit", async (event) => {
    event.preventDefault(); $("nodeFormSubmit").disabled = true; $("nodeFormMessage").textContent = state.nodeFormMode === "edit" ? "正在保存修改……" : "正在创建节点……";
    try {
      const body = { name: $("nodeName").value.trim(), region: $("nodeRegion").value.trim(), tags: $("nodeTags").value.split(",").map((tag) => tag.trim()).filter(Boolean), warp_adapter: $("nodeWarpAdapter").value, xui_service: $("nodeXuiService").value.trim(), notes: $("nodeNotes").value.trim() };
      if (state.nodeFormMode === "edit") {
        await api(`/api/nodes/${encodeURIComponent(state.editingNodeId)}`, { method: "PATCH", body });
        closeDialog("nodeDialog"); notify("节点信息已更新"); await refreshData(false);
      } else {
        const response = await api("/api/nodes", { method: "POST", body });
        closeDialog("nodeDialog"); $("registrationToken").textContent = response.registration_token; $("tokenExpiry").textContent = `有效期至：${formatTime(response.registration_token_expires_at, true)}`; $("agentInstallCommand").textContent = buildAgentInstallCommand(response.node, response.registration_token); openDialog("tokenDialog"); await refreshData(false);
      }
    } catch (error) { $("nodeFormMessage").textContent = error.message; } finally { $("nodeFormSubmit").disabled = false; }
  });
  $("copyTokenButton").addEventListener("click", async () => {
    try { await navigator.clipboard.writeText($("registrationToken").textContent); notify("注册 Token 已复制"); }
    catch (_) { notify("复制失败，请手动选择 Token。", "error"); }
  });
  $("copyInstallCommandButton").addEventListener("click", async () => {
    try { await navigator.clipboard.writeText($("agentInstallCommand").textContent); notify("Agent 安装命令已复制"); }
    catch (_) { notify("复制失败，请手动选择安装命令。", "error"); }
  });

  $("newTaskButton").addEventListener("click", openTaskForm);
  $("taskForm").addEventListener("submit", async (event) => {
    event.preventDefault();
    const nodeIds = Array.from($("taskNodes").selectedOptions).map((option) => option.value);
    if (!nodeIds.length) { $("taskFormMessage").textContent = "至少选择一个节点。"; return; }
    $("createTaskSubmit").disabled = true;
    try {
      await api("/api/tasks", { method: "POST", body: { name: $("taskName").value.trim(), node_ids: nodeIds, action: $("taskAction").value, parameters: {}, schedule_type: $("taskScheduleType").value, schedule_value: $("taskScheduleValue").value.trim(), timezone: $("taskTimezone").value.trim(), enabled: $("taskEnabled").checked, max_retries: Number($("taskRetries").value), retry_intervals_seconds: [30, 90] } });
      closeDialog("taskDialog"); notify("定时任务已创建"); await refreshData(false); navigate("tasks");
    } catch (error) { $("taskFormMessage").textContent = error.message; } finally { $("createTaskSubmit").disabled = false; }
  });

  document.querySelectorAll("[data-close-dialog]").forEach((button) => button.addEventListener("click", () => closeDialog(button.dataset.closeDialog)));
  $("confirmForm").addEventListener("submit", async (event) => {
    event.preventDefault();
    const proceed = event.submitter && event.submitter.value === "confirm";
    const callback = state.confirmAction; state.confirmAction = null; closeDialog("confirmDialog");
    if (proceed && callback) await callback();
  });
  window.addEventListener("keydown", (event) => { if (event.key === "Escape" && state.activeView === "detail") navigate("nodes"); });
  boot();
}());
