const tooltip = document.querySelector("#banked-tooltip")
const dashboardHost = document.querySelector("[ws-connect]")
let active

function bankedInfo(element) {
  return element instanceof Element ? element.closest(".banked-info") : null
}

function hideBankedInfo() {
  active = undefined
  tooltip.hidden = true
}

function placeBankedInfo() {
  if (!active?.isConnected) {
    hideBankedInfo()
    return
  }
  const anchor = active.getBoundingClientRect()
  const popup = tooltip.getBoundingClientRect()
  const gap = 8
  const preferredLeft = anchor.left + anchor.width / 2 - popup.width / 2
  const left = Math.max(gap, Math.min(preferredLeft, window.innerWidth - popup.width - gap))
  const preferredTop = anchor.top - popup.height - gap
  const top = preferredTop >= gap ? preferredTop : anchor.bottom + gap
  tooltip.style.left = `${left}px`
  tooltip.style.top = `${top}px`
}

function showBankedInfo(element) {
  active = element
  if (tooltip.textContent !== element.dataset.info) tooltip.textContent = element.dataset.info
  tooltip.hidden = false
  placeBankedInfo()
}

function showBankedInfoFor(event) {
  const element = bankedInfo(event.target)
  if (element) showBankedInfo(element)
}

function hideBankedInfoFor(event) {
  const element = bankedInfo(event.target)
  if (element && element !== bankedInfo(event.relatedTarget)) hideBankedInfo()
}

document.addEventListener("pointerover", showBankedInfoFor)
document.addEventListener("pointerout", hideBankedInfoFor)
document.addEventListener("focusin", showBankedInfoFor)
document.addEventListener("focusout", hideBankedInfoFor)

document.addEventListener("keydown", event => {
  if (event.key === "Escape") hideBankedInfo()
})

document.addEventListener("scroll", placeBankedInfo, true)
window.addEventListener("resize", placeBankedInfo)

new MutationObserver(() => {
  const hovered = document.querySelector(".banked-info:hover")
  if (hovered) showBankedInfo(hovered)
  else if (active && !active.isConnected) hideBankedInfo()
}).observe(dashboardHost, {childList: true, subtree: true})
