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

const refreshState = document.querySelector("#refresh-state");
const adminError = document.querySelector("#admin-error");
const adminNotice = document.querySelector("#admin-notice");
const inviteForm = document.querySelector("#invite-form");
const inviteExpiry = document.querySelector("#invite-expiry");
const createInvite = document.querySelector("#create-invite");
const inviteResult = document.querySelector("#invite-result");
const inviteURL = document.querySelector("#invite-url");
const inviteResultExpiry = document.querySelector("#invite-result-expiry");
const copyInvite = document.querySelector("#copy-invite");
const accountsBody = document.querySelector("#admin-accounts-body");
const invitesBody = document.querySelector("#invites-body");

let currentInviteURL = "";
let loading = false;

function resetText(account) {
  const parts = [];
  const banked = numericValue(account?.bankedResets);
  if (banked !== null) parts.push(`${formatNumber(banked)} banked`);
  const resetAt = parsedDate(account?.resetAt);
  if (resetAt) parts.push(formatDate(resetAt.toISOString()));
  return parts.length ? parts.join(" · ") : "—";
}

function button(label, className, action, account) {
  const element = document.createElement("button");
  element.type = "button";
  element.className = `button button-small ${className}`;
  element.textContent = label;
  element.dataset.action = action;
  element.dataset.accountId = stringValue(account?.id, "");
  element.dataset.accountEmail = stringValue(account?.email, "account");
  element.dataset.paused = account?.paused === true ? "true" : "false";
  return element;
}

function renderAccounts(accounts) {
  if (!Array.isArray(accounts) || accounts.length === 0) {
    accountsBody.replaceChildren(emptyRow(6, "No accounts are available."));
    return;
  }

  const rows = accounts.map((account) => {
    const row = document.createElement("tr");
    const identity = document.createElement("td");
    const identityContent = document.createElement("div");
    identityContent.className = "account-identity";
    const email = document.createElement("strong");
    email.textContent = stringValue(account?.email, "Unknown account");
    identityContent.append(email);
    identity.append(identityContent);

    const actions = document.createElement("td");
    const actionGroup = document.createElement("div");
    actionGroup.className = "row-actions";
    actionGroup.append(
      button(
        account?.paused === true ? "Resume" : "Pause",
        "button-secondary",
        "toggle",
        account,
      ),
      button("Remove", "button-danger", "delete", account),
    );
    actions.append(actionGroup);

    row.append(
      identity,
      cell(stringValue(account?.plan)),
      statusCell(account?.status),
      cell(formatPercent(account?.weeklyRemainingPercent)),
      cell(resetText(account)),
      actions,
    );
    return row;
  });
  accountsBody.replaceChildren(...rows);
}

function inviteStatus(invite) {
  if (parsedDate(invite?.usedAt)) return "used";
  const expiry = parsedDate(invite?.expiresAt);
  if (expiry && expiry.getTime() <= Date.now()) return "expired";
  return "ready";
}

function renderInvites(invites) {
  if (!Array.isArray(invites) || invites.length === 0) {
    invitesBody.replaceChildren(emptyRow(4, "No invites have been created."));
    return;
  }

  const rows = invites.map((invite) => {
    const row = document.createElement("tr");
    const identifier = stringValue(invite?.id, "unknown");
    row.append(
      cell(identifier.length > 16 ? `${identifier.slice(0, 16)}…` : identifier),
      statusCell(inviteStatus(invite)),
      cell(formatDate(invite?.expiresAt), "muted"),
      cell(formatDate(invite?.usedAt), "muted"),
    );
    return row;
  });
  invitesBody.replaceChildren(...rows);
}

function showError(message) {
  adminError.textContent = message;
  adminError.hidden = false;
}

function hideError() {
  adminError.textContent = "";
  adminError.hidden = true;
}

function showNotice(message) {
  adminNotice.textContent = message;
  adminNotice.hidden = false;
}

function hideNotice() {
  adminNotice.textContent = "";
  adminNotice.hidden = true;
}

async function request(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    headers: {
      Accept: "application/json",
      ...(options.body ? { "Content-Type": "application/json" } : {}),
      ...options.headers,
    },
    cache: "no-store",
  });
  return responsePayload(response);
}

function setLoading(value) {
  loading = value;
  refreshState.disabled = value;
}

async function loadState() {
  if (loading) return;
  setLoading(true);
  hideError();
  try {
    const state = await request("/admin/state");
    if (!state || typeof state !== "object")
      throw new Error("Admin state is invalid.");
    renderAccounts(state.accounts);
    renderInvites(state.invites);
  } catch (error) {
    showError(
      error instanceof Error ? error.message : "Could not load admin state.",
    );
  } finally {
    setLoading(false);
  }
}

async function createNewInvite(event) {
  event.preventDefault();
  if (createInvite.disabled) return;
  createInvite.disabled = true;
  hideError();
  hideNotice();
  inviteResult.hidden = true;

  const expiresInSeconds = Number.parseInt(inviteExpiry.value, 10);
  const body = Number.isFinite(expiresInSeconds) ? { expiresInSeconds } : {};
  try {
    const invite = await request("/admin/invites", {
      method: "POST",
      body: JSON.stringify(body),
    });
    if (typeof invite?.url !== "string" || !invite.url) {
      throw new Error("The invite response did not include a URL.");
    }
    currentInviteURL = invite.url;
    inviteURL.textContent = currentInviteURL;
    inviteResultExpiry.textContent = `Expires ${formatDate(invite.expiresAt)}`;
    inviteResult.hidden = false;
    showNotice("Invite created.");
    await loadState();
  } catch (error) {
    showError(
      error instanceof Error ? error.message : "Could not create an invite.",
    );
  } finally {
    createInvite.disabled = false;
  }
}

async function copyCurrentInvite() {
  if (!currentInviteURL) return;
  try {
    await navigator.clipboard.writeText(currentInviteURL);
    copyInvite.textContent = "Copied";
    window.setTimeout(() => {
      copyInvite.textContent = "Copy URL";
    }, 1800);
  } catch {
    showError("Could not copy the invite URL. Select and copy it manually.");
  }
}

async function updateAccount(buttonElement) {
  const accountId = buttonElement.dataset.accountId;
  if (!accountId) return;
  const paused = buttonElement.dataset.paused !== "true";
  buttonElement.disabled = true;
  hideError();
  hideNotice();
  try {
    await request(`/admin/accounts/${encodeURIComponent(accountId)}`, {
      method: "PATCH",
      body: JSON.stringify({ paused }),
    });
    showNotice(paused ? "Account paused." : "Account resumed.");
    await loadState();
  } catch (error) {
    showError(
      error instanceof Error ? error.message : "Could not update the account.",
    );
  } finally {
    buttonElement.disabled = false;
  }
}

async function deleteAccount(buttonElement) {
  const accountId = buttonElement.dataset.accountId;
  const accountEmail = buttonElement.dataset.accountEmail || "this account";
  if (
    !accountId ||
    !window.confirm(`Remove ${accountEmail} from the routing pool?`)
  )
    return;
  buttonElement.disabled = true;
  hideError();
  hideNotice();
  try {
    await request(`/admin/accounts/${encodeURIComponent(accountId)}`, {
      method: "DELETE",
    });
    showNotice("Account removed.");
    await loadState();
  } catch (error) {
    showError(
      error instanceof Error ? error.message : "Could not remove the account.",
    );
  } finally {
    buttonElement.disabled = false;
  }
}

accountsBody.addEventListener("click", (event) => {
  const buttonElement =
    event.target instanceof Element ? event.target.closest("button") : null;
  if (!(buttonElement instanceof HTMLButtonElement)) return;
  if (buttonElement.dataset.action === "toggle")
    void updateAccount(buttonElement);
  if (buttonElement.dataset.action === "delete")
    void deleteAccount(buttonElement);
});

refreshState.addEventListener("click", () => {
  hideNotice();
  void loadState();
});

inviteForm.addEventListener("submit", (event) => {
  void createNewInvite(event);
});

copyInvite.addEventListener("click", () => {
  void copyCurrentInvite();
});

void loadState();
