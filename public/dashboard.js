import {
  cell,
  emptyRow,
  formatDate,
  formatNumber,
  formatPercent,
  numericValue,
  parsedDate,
  responsePayload,
  statusCell,
  stringValue,
} from "./shared.js";

const millisecondsFormat = new Intl.NumberFormat(undefined, {
  maximumFractionDigits: 1,
});

const accountsBody = document.querySelector("#accounts-body");
const eventsBody = document.querySelector("#events-body");
const dashboardError = document.querySelector("#dashboard-error");
const connectionState = document.querySelector("#connection-state");
const connectionLabel = document.querySelector("#connection-label");
const updatedAt = document.querySelector("#updated-at");
const totalLiveAccounts = document.querySelector("#total-live-accounts");
const totalTurns = document.querySelector("#total-turns");
const totalWebSocketTurns = document.querySelector("#total-websocket-turns");
const totalFailovers = document.querySelector("#total-failovers");
const totalRateLimits = document.querySelector("#total-rate-limits");
const totalOpenWebSockets = document.querySelector("#total-open-websockets");
const totalInputTokens = document.querySelector("#total-input-tokens");
const totalCachedInputTokens = document.querySelector(
  "#total-cached-input-tokens",
);
const totalCacheWriteInputTokens = document.querySelector(
  "#total-cache-write-input-tokens",
);
const totalOutputTokens = document.querySelector("#total-output-tokens");
const averageFirstByte = document.querySelector("#average-first-byte");

let socket;
let reconnectTimer;
let reconnectDelay = 1000;
let stopping = false;

function formatMilliseconds(value) {
  const number = numericValue(value);
  return number === null ? "—" : `${millisecondsFormat.format(number)} ms`;
}

function formatRelative(value) {
  const date = parsedDate(value);
  if (!date) return null;
  const seconds = Math.round((date.getTime() - Date.now()) / 1000);
  const absolute = Math.abs(seconds);
  if (absolute < 60)
    return seconds >= 0 ? "in under a minute" : "under a minute ago";
  const units = [
    [86400, "day"],
    [3600, "hour"],
    [60, "minute"],
  ];
  for (const [size, label] of units) {
    if (absolute < size) continue;
    const count = Math.floor(absolute / size);
    const phrase = `${count} ${label}${count === 1 ? "" : "s"}`;
    return seconds >= 0 ? `in ${phrase}` : `${phrase} ago`;
  }
  return null;
}

function resetText(account) {
  const parts = [];
  const banked = numericValue(account?.bankedResets);
  if (banked !== null) parts.push(`${formatNumber(banked)} banked`);
  const relative = formatRelative(account?.resetAt);
  if (relative) parts.push(relative);
  return parts.length ? parts.join(" · ") : "—";
}

function renderAccounts(accounts) {
  if (!Array.isArray(accounts) || accounts.length === 0) {
    accountsBody.replaceChildren(emptyRow(8, "No accounts are available."));
    return;
  }

  const rows = accounts.map((account) => {
    const row = document.createElement("tr");
    row.append(
      cell(stringValue(account?.alias, "Unnamed")),
      cell(stringValue(account?.plan)),
      statusCell(account?.status),
      cell(formatPercent(account?.weeklyRemainingPercent)),
      cell(resetText(account)),
      cell(formatNumber(account?.turns)),
      cell(formatNumber(account?.rateLimits)),
      cell(formatNumber(account?.openWebSockets)),
    );
    return row;
  });
  accountsBody.replaceChildren(...rows);
}

function renderEvents(events) {
  if (!Array.isArray(events) || events.length === 0) {
    eventsBody.replaceChildren(emptyRow(4, "No recent events."));
    return;
  }

  const rows = events.slice(0, 100).map((event) => {
    const row = document.createElement("tr");
    const detail = cell(stringValue(event?.detail), "detail-cell");
    detail.title = stringValue(event?.detail, "");
    row.append(
      cell(formatDate(event?.at), "muted"),
      cell(stringValue(event?.kind)),
      cell(stringValue(event?.accountAlias, "System")),
      detail,
    );
    return row;
  });
  eventsBody.replaceChildren(...rows);
}

function showError(message) {
  dashboardError.textContent = message;
  dashboardError.hidden = false;
}

function hideError() {
  dashboardError.hidden = true;
  dashboardError.textContent = "";
}

function setConnection(state, label) {
  connectionState.dataset.state = state;
  connectionLabel.textContent = label;
}

function renderDashboard(payload) {
  if (!payload || typeof payload !== "object")
    throw new Error("Dashboard data is invalid.");
  const totals =
    payload.totals && typeof payload.totals === "object" ? payload.totals : {};
  const accounts = Array.isArray(payload.accounts) ? payload.accounts : [];
  const liveAccounts = accounts.filter(
    (account) => account?.status === "live",
  ).length;
  const openWebSockets = accounts.reduce(
    (total, account) => total + (numericValue(account?.openWebSockets) ?? 0),
    0,
  );
  totalLiveAccounts.textContent = `${formatNumber(liveAccounts)} / ${formatNumber(accounts.length)}`;
  totalTurns.textContent = formatNumber(totals.turns);
  totalWebSocketTurns.textContent = formatNumber(totals.websocketTurns);
  totalFailovers.textContent = formatNumber(totals.failovers);
  totalRateLimits.textContent = formatNumber(totals.rateLimits);
  totalOpenWebSockets.textContent = formatNumber(openWebSockets);
  totalInputTokens.textContent = formatNumber(totals.inputTokens);
  totalCachedInputTokens.textContent = formatNumber(totals.cachedInputTokens);
  totalCacheWriteInputTokens.textContent = formatNumber(
    totals.cacheWriteInputTokens,
  );
  totalOutputTokens.textContent = formatNumber(totals.outputTokens);
  averageFirstByte.textContent = formatMilliseconds(
    totals.averageFirstByteMilliseconds,
  );
  renderAccounts(accounts);
  renderEvents(payload.events);

  const updateDate = parsedDate(payload.updatedAt);
  updatedAt.textContent = updateDate
    ? `Updated ${formatDate(updateDate.toISOString())}`
    : "Updated now";
  updatedAt.dateTime = updateDate ? updateDate.toISOString() : "";
  hideError();
}

async function loadSnapshot() {
  try {
    const response = await fetch("/stats", {
      headers: { Accept: "application/json" },
      cache: "no-store",
    });
    renderDashboard(await responsePayload(response));
  } catch (error) {
    showError(
      error instanceof Error ? error.message : "Could not load dashboard data.",
    );
  }
}

function scheduleReconnect() {
  if (stopping || reconnectTimer) return;
  setConnection("offline", "Reconnecting");
  reconnectTimer = window.setTimeout(() => {
    reconnectTimer = undefined;
    connect();
  }, reconnectDelay);
  reconnectDelay = Math.min(reconnectDelay * 2, 30000);
}

function connect() {
  if (stopping) return;
  setConnection("connecting", "Connecting");
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  socket = new WebSocket(`${protocol}//${window.location.host}/dashboard/ws`);

  socket.addEventListener("open", () => {
    reconnectDelay = 1000;
    setConnection("live", "Live");
  });

  socket.addEventListener("message", (event) => {
    if (typeof event.data !== "string") return;
    try {
      renderDashboard(JSON.parse(event.data));
      setConnection("live", "Live");
    } catch (error) {
      showError(
        error instanceof Error ? error.message : "A live update was invalid.",
      );
    }
  });

  socket.addEventListener("error", () => {
    socket?.close();
  });

  socket.addEventListener("close", () => {
    socket = undefined;
    scheduleReconnect();
  });
}

window.addEventListener("pagehide", () => {
  stopping = true;
  if (reconnectTimer) window.clearTimeout(reconnectTimer);
  socket?.close();
});

void loadSnapshot();
connect();
