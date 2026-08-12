const colorScheme = matchMedia("(prefers-color-scheme: dark)")

function applyTheme() {
  document.documentElement.dataset.webtuiTheme = colorScheme.matches ? "dark" : "light"
}

applyTheme()
colorScheme.addEventListener("change", applyTheme)
