const tooltip = document.querySelector("#banked-tooltip")
const dashboardHost = document.querySelector("[ws-connect]")
const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)")
const summarySelector = "#summary > span:not(.dot)"
const totalsSelector = "#totals > div"
const accountRowsSelector = "#accounts tbody tr"
const accountCellsSelector = "#accounts tbody td:not(:nth-child(1), :nth-child(2), :nth-child(6))"
const threadRowsSelector = "#threads tbody tr"
const threadTurnsSelector = "#threads tbody td:nth-child(6)"
const eventsSelector = "#events tbody tr"
let active
let beforeUpdate

function elementText(element) {
  return element.textContent.trim()
}

function snapshot(selector, key) {
  return new Map(Array.from(document.querySelectorAll(selector), element => [key(element), elementText(element)]))
}

function rowKey(row) {
  return elementText(row.cells[0])
}

function cellKey(cell) {
  return `${rowKey(cell.parentElement)}\u0000${cell.cellIndex}`
}

function summaryKey(element) {
  return elementText(element).replace(/^\d+\s+/, "")
}

function totalKey(element) {
  return elementText(element.querySelector("dt"))
}

function dashboardState() {
  return {
    summary: snapshot(summarySelector, summaryKey),
    totals: snapshot(totalsSelector, totalKey),
    accountRows: snapshot(accountRowsSelector, rowKey),
    accountCells: snapshot(accountCellsSelector, cellKey),
    threadRows: snapshot(threadRowsSelector, rowKey),
    threadTurns: snapshot(threadTurnsSelector, cellKey),
    events: snapshot(eventsSelector, elementText),
  }
}

function animateChanged(selector, key, previous, skippedKey) {
  document.querySelectorAll(selector).forEach(element => {
    const currentKey = key(element)
    const old = previous.get(currentKey)
    if (currentKey !== skippedKey && old !== undefined && old !== elementText(element)) element.classList.add("motion-change")
  })
}

function animateNew(selector, key, previous) {
  document.querySelectorAll(selector).forEach(element => {
    if (!previous.has(key(element))) element.classList.add("motion-enter")
  })
}

function animateDashboard(previous) {
  animateChanged(summarySelector, summaryKey, previous.summary)
  animateChanged(totalsSelector, totalKey, previous.totals, "uptime")
  animateNew(accountRowsSelector, rowKey, previous.accountRows)
  animateChanged(accountCellsSelector, cellKey, previous.accountCells)
  animateNew(threadRowsSelector, rowKey, previous.threadRows)
  animateChanged(threadTurnsSelector, cellKey, previous.threadTurns)
  animateNew(eventsSelector, elementText, previous.events)
}

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

dashboardHost.addEventListener("htmx:wsBeforeMessage", () => {
  beforeUpdate = dashboardState()
})

dashboardHost.addEventListener("htmx:wsAfterMessage", () => {
  if (beforeUpdate && !reducedMotion.matches) animateDashboard(beforeUpdate)
})

setTimeout(() => document.body.classList.add("dashboard-live"), 240)

new MutationObserver(() => {
  const hovered = document.querySelector(".banked-info:hover")
  if (hovered) showBankedInfo(hovered)
  else if (active && !active.isConnected) hideBankedInfo()
}).observe(dashboardHost, {childList: true, subtree: true})
