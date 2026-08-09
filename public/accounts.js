import { formatDate, parsedDate, responsePayload } from "./shared.js";

const accountIntro = document.querySelector("#account-intro");
const flowStatus = document.querySelector("#flow-status");
const flowTitle = document.querySelector("#flow-title");
const flowDetail = document.querySelector("#flow-detail");
const deviceCode = document.querySelector("#device-code");
const userCode = document.querySelector("#user-code");
const startDevice = document.querySelector("#start-device");
const openVerification = document.querySelector("#open-verification");
const loginExpiry = document.querySelector("#login-expiry");
const accountError = document.querySelector("#account-error");

let pollTimer;
let requestPending = false;
let finished = false;

function errorMessage(error, fallback) {
  return error instanceof Error && error.message ? error.message : fallback;
}

function showError(message) {
  accountError.textContent = message;
  accountError.hidden = false;
}

function hideError() {
  accountError.textContent = "";
  accountError.hidden = true;
}

function setVerificationURL(value) {
  openVerification.hidden = true;
  openVerification.removeAttribute("href");
  if (typeof value !== "string" || !value) return;
  try {
    const url = new URL(value, window.location.origin);
    if (url.protocol !== "https:") return;
    openVerification.href = url.href;
    openVerification.hidden = false;
  } catch {
    return;
  }
}

function setExpiry(value) {
  const expiry = parsedDate(value);
  loginExpiry.hidden = !expiry;
  loginExpiry.textContent = expiry
    ? `This sign-in expires ${formatDate(expiry.toISOString())}.`
    : "";
}

function resetActions() {
  startDevice.hidden = true;
  startDevice.disabled = true;
  openVerification.hidden = true;
  openVerification.removeAttribute("href");
  deviceCode.hidden = true;
  userCode.textContent = "";
  loginExpiry.hidden = true;
  loginExpiry.textContent = "";
}

function renderStatus(payload) {
  if (
    !payload ||
    typeof payload !== "object" ||
    typeof payload.status !== "string"
  ) {
    throw new Error("Account status is invalid.");
  }

  resetActions();
  hideError();
  flowStatus.dataset.state = payload.status;

  switch (payload.status) {
    case "ready":
      accountIntro.textContent = "This invite is ready to add one account.";
      flowTitle.textContent = "Ready to sign in";
      flowDetail.textContent =
        "Sign-in starts only when you choose the button below.";
      startDevice.textContent = "Start sign-in";
      startDevice.hidden = false;
      startDevice.disabled = requestPending;
      break;
    case "pending":
      accountIntro.textContent = "Finish sign-in in the new browser tab.";
      flowTitle.textContent = "Waiting for sign-in";
      flowDetail.textContent = "Enter the one-time code on the sign-in page.";
      if (typeof payload.userCode === "string" && payload.userCode) {
        userCode.textContent = payload.userCode;
        deviceCode.hidden = false;
      }
      setVerificationURL(payload.verificationUrl);
      setExpiry(payload.expiresAt);
      break;
    case "complete":
      accountIntro.textContent = "The account is now in the routing pool.";
      flowTitle.textContent = "Account added";
      flowDetail.textContent =
        "You can close this page or view the public dashboard.";
      finished = true;
      break;
    case "failed":
      accountIntro.textContent = "The last sign-in did not finish.";
      flowTitle.textContent = "Sign-in failed";
      flowDetail.textContent = "You can start a new sign-in attempt.";
      startDevice.textContent = "Try again";
      startDevice.hidden = false;
      startDevice.disabled = requestPending;
      if (typeof payload.error === "string" && payload.error)
        showError(payload.error);
      break;
    case "expired":
      accountIntro.textContent = "This invite or sign-in session has expired.";
      flowTitle.textContent = "Access expired";
      flowDetail.textContent = "Ask an administrator for a new invite.";
      finished = true;
      break;
    default:
      throw new Error("Account status is unknown.");
  }
}

function schedulePoll(delay) {
  if (finished || pollTimer) return;
  pollTimer = window.setTimeout(() => {
    pollTimer = undefined;
    void loadStatus(true);
  }, delay);
}

async function loadStatus(silent = false) {
  if (requestPending || finished) return;
  try {
    const response = await fetch("/accounts/status", {
      headers: { Accept: "application/json" },
      cache: "no-store",
    });
    const payload = await responsePayload(response);
    renderStatus(payload);
    schedulePoll(payload.status === "pending" ? 2000 : 10000);
  } catch (error) {
    if (!silent) {
      flowStatus.dataset.state = "failed";
      flowTitle.textContent = "Could not check access";
      flowDetail.textContent = "The status service did not respond.";
    }
    showError(errorMessage(error, "Could not check account status."));
    schedulePoll(5000);
  }
}

async function startSignIn() {
  if (requestPending || finished) return;
  requestPending = true;
  startDevice.disabled = true;
  hideError();
  flowStatus.dataset.state = "loading";
  flowTitle.textContent = "Starting sign-in";
  flowDetail.textContent = "Requesting a one-time code.";

  try {
    const response = await fetch("/accounts/device", {
      method: "POST",
      headers: { Accept: "application/json" },
    });
    const payload = await responsePayload(response);
    renderStatus(payload);
    schedulePoll(payload.status === "pending" ? 2000 : 10000);
  } catch (error) {
    flowStatus.dataset.state = "failed";
    flowTitle.textContent = "Could not start sign-in";
    flowDetail.textContent = "Try again when the service is available.";
    showError(errorMessage(error, "Could not start account sign-in."));
    startDevice.textContent = "Try again";
    startDevice.hidden = false;
  } finally {
    requestPending = false;
    startDevice.disabled = false;
  }
}

startDevice.addEventListener("click", () => {
  void startSignIn();
});

window.addEventListener("pagehide", () => {
  finished = true;
  if (pollTimer) window.clearTimeout(pollTimer);
});

void loadStatus();
