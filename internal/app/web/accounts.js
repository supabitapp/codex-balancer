const code = document.querySelector("[data-copy-code]")

code.addEventListener("click", async () => {
  await navigator.clipboard.writeText(code.dataset.copyCode)
  code.querySelector("small").textContent = "Copied"
})

const wait = () => new Promise(resolve => setTimeout(resolve, 1000))

const redirectAfterSignIn = async () => {
  while (true) {
    const response = await fetch("/accounts/status")
    if (response.status === 200) {
      window.location.assign("/dashboard")
      return
    }
    if (response.status !== 204) return
    await wait()
  }
}

redirectAfterSignIn()
