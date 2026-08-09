const numberFormat = new Intl.NumberFormat(undefined, {
  maximumFractionDigits: 0,
});
const percentFormat = new Intl.NumberFormat(undefined, {
  maximumFractionDigits: 1,
});
const dateTimeFormat = new Intl.DateTimeFormat(undefined, {
  dateStyle: "medium",
  timeStyle: "short",
});

export function stringValue(value, fallback = "—") {
  return typeof value === "string" && value.trim() ? value.trim() : fallback;
}

export function numericValue(value) {
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

export function formatNumber(value) {
  const number = numericValue(value);
  return number === null ? "—" : numberFormat.format(number);
}

export function formatPercent(value) {
  const number = numericValue(value);
  return number === null ? "—" : `${percentFormat.format(number)}%`;
}

export function parsedDate(value) {
  if (typeof value !== "string" || !value) return null;
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? null : date;
}

export function formatDate(value) {
  const date = parsedDate(value);
  return date ? dateTimeFormat.format(date) : "—";
}

export function cell(text, className) {
  const element = document.createElement("td");
  element.textContent = text;
  if (className) element.className = className;
  return element;
}

export function emptyRow(columns, text) {
  const row = document.createElement("tr");
  const content = cell(text, "empty-cell");
  content.colSpan = columns;
  row.append(content);
  return row;
}

export function statusCell(status) {
  const container = document.createElement("td");
  const badge = document.createElement("span");
  badge.className = "status-badge";
  badge.dataset.status =
    typeof status === "string" && /^[a-z_]+$/u.test(status)
      ? status
      : "unknown";
  badge.textContent = stringValue(status, "unknown").replaceAll("_", " ");
  container.append(badge);
  return container;
}

export async function responsePayload(response) {
  const text = await response.text();
  let payload;
  try {
    payload = text ? JSON.parse(text) : null;
  } catch {
    payload = null;
  }
  if (!response.ok) {
    const message =
      typeof payload?.error === "string"
        ? payload.error
        : typeof payload?.message === "string"
          ? payload.message
          : `Request failed with status ${response.status}.`;
    throw new Error(message);
  }
  return payload;
}
