const notice = document.querySelector("#admin-notice")
let dismissTimer

Idiomorph.defaults.callbacks.beforeAttributeUpdated = (name, node) => !(name === "style" && node.matches("[popover]"))

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

function placePopover(popover) {
  const trigger = document.querySelector(`[popovertarget="${popover.id}"]`)
  if (!trigger) return
  const anchor = trigger.getBoundingClientRect()
  const below = anchor.bottom < innerHeight * 0.6
  const leading = anchor.left < innerWidth / 2
  popover.style.left = leading ? `${anchor.left}px` : "auto"
  popover.style.right = leading ? "auto" : `${innerWidth - anchor.right}px`
  popover.style.top = below ? `${anchor.bottom + 4}px` : "auto"
  popover.style.bottom = below ? "auto" : `${innerHeight - anchor.top + 4}px`
}

function placeOpenPopovers() {
  for (const popover of document.querySelectorAll("[popover]:popover-open")) placePopover(popover)
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

document.addEventListener("beforetoggle", event => {
  if (event.newState === "open" && event.target.matches("[popover]")) placePopover(event.target)
}, true)

addEventListener("scroll", placeOpenPopovers, true)
addEventListener("resize", placeOpenPopovers)

document.addEventListener("htmx:beforeRequest", event => {
  event.detail.elt.closest?.(".menu")?.hidePopover()
})

document.addEventListener("htmx:afterRequest", event => {
  const { elt, xhr, successful } = event.detail
  if (xhr) notice.dataset.state = xhr.status >= 400 ? "error" : "ok"
  if (successful && elt.matches("[data-reset-on-success]")) {
    elt.reset()
    if (elt.matches(":popover-open")) elt.hidePopover()
  }
})

document.addEventListener("htmx:afterSwap", event => {
  if (event.detail.target === notice) scheduleDismiss()
})

document.addEventListener("htmx:sseError", () => checkSession().catch(() => {}))

scheduleDismiss()
