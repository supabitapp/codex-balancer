const notice = document.querySelector("#admin-notice")
let dismissTimer

function clearNotice() {
  clearTimeout(dismissTimer)
  notice.replaceChildren()
}

function scheduleDismiss() {
  clearTimeout(dismissTimer)
  if (notice.hasChildNodes() && !notice.querySelector(".secret")) dismissTimer = setTimeout(clearNotice, 6000)
}

async function copySecret(button) {
  const secret = document.getElementById(button.dataset.copy)
  try {
    await navigator.clipboard.writeText(secret.textContent)
    button.textContent = "Copied"
  } catch {
    getSelection().selectAllChildren(secret)
  }
}

async function checkSession() {
  const response = await fetch("/admin", { method: "HEAD", redirect: "manual", cache: "no-store" })
  if (response.type === "opaqueredirect" || response.status === 503) location.assign("/admin/login")
}

notice.addEventListener("click", event => {
  if (event.target.closest(".notice-close")) clearNotice()
  const copy = event.target.closest("[data-copy]")
  if (copy) copySecret(copy)
})

document.addEventListener("htmx:afterRequest", event => {
  const { elt, xhr, successful } = event.detail
  if (xhr) notice.dataset.state = xhr.status >= 400 ? "error" : "ok"
  if (successful && elt.matches("[data-reset-on-success]")) elt.reset()
})

document.addEventListener("htmx:afterSwap", event => {
  if (event.detail.target === notice) scheduleDismiss()
})

document.addEventListener("htmx:sseError", () => checkSession().catch(() => {}))

scheduleDismiss()
