const tooltip = document.querySelector("#dashboard-tooltip")
const dashboardHost = document.querySelector("[ws-connect]")
let active

function tooltipAnchor(element) {
  return element instanceof Element ? element.closest(".has-tooltip") : null
}

function hideTooltip() {
  active = undefined
  tooltip.hidden = true
}

function placeTooltip() {
  if (!active?.isConnected) {
    hideTooltip()
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

function showTooltip(element) {
  active = element
  const strongText = element.dataset.tooltipStrong
  if (strongText) {
    const strong = document.createElement("strong")
    strong.textContent = strongText
    tooltip.replaceChildren(strong, element.dataset.tooltip)
  } else if (tooltip.textContent !== element.dataset.tooltip || tooltip.children.length) {
    tooltip.textContent = element.dataset.tooltip
  }
  tooltip.hidden = false
  placeTooltip()
}

function showTooltipFor(event) {
  const element = tooltipAnchor(event.target)
  if (element) showTooltip(element)
}

function hideTooltipFor(event) {
  const element = tooltipAnchor(event.target)
  if (element && element !== tooltipAnchor(event.relatedTarget)) hideTooltip()
}

document.addEventListener("pointerover", showTooltipFor)
document.addEventListener("pointerout", hideTooltipFor)
document.addEventListener("focusin", showTooltipFor)
document.addEventListener("focusout", hideTooltipFor)

document.addEventListener("keydown", event => {
  if (event.key === "Escape") hideTooltip()
})

document.addEventListener("scroll", placeTooltip, true)
window.addEventListener("resize", placeTooltip)

new MutationObserver(() => {
  const hovered = document.querySelector(".has-tooltip:hover")
  if (hovered) showTooltip(hovered)
  else if (active && !active.isConnected) hideTooltip()
}).observe(dashboardHost, {childList: true, subtree: true})
